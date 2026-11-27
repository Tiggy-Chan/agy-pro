package runner

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
	"github.com/Tiggy-Chan/agy-pro/internal/quota"
	"github.com/Tiggy-Chan/agy-pro/internal/sessions"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

// TerminalResetSeq resets modern terminal protocols enabled by agy on startup:
// - CSI < u            : pop Kitty keyboard protocol (agy sends CSI > 5 u)
// - CSI > 4 ; 0 m      : disable xterm modifyOtherKeys (agy sends CSI > 4 ; 2 m)
// - CSI ? 2004 l       : disable bracketed paste
// - CSI ? 1000..1006 l : disable mouse tracking
// - CSI ? 25 h         : show cursor
// - CSI 0 m            : reset SGR attributes
var TerminalResetSeq = []byte(
	"\x1b[<u" +
		"\x1b[>4;0m" +
		"\x1b[?2004l" +
		"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l" +
		"\x1b[?25h" +
		"\x1b[0m",
)

func ResumeContinuePrompt() string {
	return i18n.T("runner.resume_prompt")
}

const uuidPat = `([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`

var (
	ansiRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)
	// Match ONLY foreground CLI session manager lines — NEVER server.go update streams,
	// GetConversationDetail, Created conversation, or cascade_id="..." (which fire for ephemeral
	// /btw side questions, subagents, swarm workers, and cancelled background cascades!).
	convLogRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)common\.go:\d+\]\s+Resuming conversation\s+` + uuidPat),
		regexp.MustCompile(`(?i)conversation_manager\.go:\d+\]\s+(?:Streaming|Forwarding user message to)\s+conversation\s+` + uuidPat),
		regexp.MustCompile(`(?i)send_user_message_manager\.go:\d+\]\s+Forwarding mid-turn user message immediately to conversation\s+` + uuidPat),
		regexp.MustCompile(`(?i)manager\.go:\d+\]\s+Full redraw completed \(RerenderAll\) for conversation\s+` + uuidPat),
	}
	uuidPrefixRe = regexp.MustCompile(`^` + uuidPat)
	authEmailRe  = regexp.MustCompile(`applyAuthResult:\s+email=([^\s,]+)`)

	// BackendLogQuotaRe matches ONLY genuine agy Go backend RPC quota errors in --log-file.
	// Covers both 5-hour AND 7-day exhaustion (including G1 AI credits balance fallback errors).
	BackendLogQuotaRe = regexp.MustCompile(
		`(?i)errorreport\.go:\d+\].*(?:ResourceExhausted,\s*HTTP\s*429|RESOURCE_EXHAUSTED\s*\(code\s*429\)|Individual quota reached|AI credits balance is too low|insufficient.*credits)`,
	)

	// FatalTurnQuotaRe identifies errors logged AFTER agy exhausted all internal retries and
	// aborted the turn (used as fallback when live quota probing itself times out).
	FatalTurnQuotaRe = regexp.MustCompile(
		`(?i)errorreport\.go:\d+\].*agent executor error:.*(?:ResourceExhausted,\s*HTTP\s*429|RESOURCE_EXHAUSTED\s*\(code\s*429\)|AI credits balance is too low|Individual quota reached)`,
	)

	// PrintModeQuotaRe is used ONLY in non-interactive --print mode (fresh single turn, no historical redraw).
	PrintModeQuotaRe = regexp.MustCompile(
		`(?i)(?:Individual quota reached|RESOURCE_EXHAUSTED\s*\(code\s*429\))`,
	)

	resetsInRe = regexp.MustCompile(`(?i)Resets in\s+(?:(\d+)d)?\s*(?:(\d+)h)?\s*(?:(\d+)m)?\s*(?:(\d+)s)?`)
)

type RunResult struct {
	ReturnCode     int
	Stdout         string
	Stderr         string
	QuotaHit       bool
	ExhaustionKind string // "fast", "7d", "5h", "claude-7d", "claude-5h", or "suspected"
	QuotaText      string
	VerifiedQuota  *vault.Quota
	ConvID         string
	Title          string
	AuthEmail      string
}

type InteractiveOptions struct {
	InitialConvID     string
	CustomTitle       string
	NotifyEnabled     bool
	ReleaseLaunchLock func()
	OnConvChange      func(convID string)
}

func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

func ParseResetsIn(text string) int {
	m := resetsInRe.FindStringSubmatch(text)
	if m == nil {
		return 3600
	}
	d, _ := strconv.Atoi(m[1])
	h, _ := strconv.Atoi(m[2])
	mins, _ := strconv.Atoi(m[3])
	s, _ := strconv.Atoi(m[4])
	total := d*86400 + h*3600 + mins*60 + s
	if total <= 0 {
		return 3600
	}
	if total < 60 {
		return 60
	}
	return total
}

func ExtractCLIConversationID(args []string) string {
	for i, a := range args {
		for _, pfx := range []string{"--conversation=", "--resume=", "-r="} {
			if strings.HasPrefix(a, pfx) {
				return strings.TrimSpace(strings.TrimPrefix(a, pfx))
			}
		}
		if (a == "--conversation" || a == "--resume" || a == "-r") && i+1 < len(args) {
			return strings.TrimSpace(args[i+1])
		}
	}
	return ""
}

func StripResumeAndPromptArgs(args []string) []string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == "--conversation" || a == "--resume" || a == "-r" || a == "--prompt-interactive" {
			skip = true
			continue
		}
		if strings.HasPrefix(a, "--conversation=") || strings.HasPrefix(a, "--resume=") ||
			strings.HasPrefix(a, "-r=") || strings.HasPrefix(a, "--prompt-interactive=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// StripLogFileArg removes only --log-file so agy-pro can attach its own session log.
// All user model, effort, settings, mode, and permission flags pass through verbatim to native agy.
func StripLogFileArg(args []string) []string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == "--log-file" {
			skip = true
			continue
		}
		if strings.HasPrefix(a, "--log-file=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// ScanLog incrementally parses agy's backend --log-file for conversation UUID,
// authenticated email, and genuine backend HTTP 429 ResourceExhausted errors.
func ScanLog(logPath string, offset int64) (int64, string, string, string) {
	f, err := os.Open(logPath)
	if err != nil {
		return offset, "", "", ""
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, "", "", ""
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return offset, "", "", ""
	}
	newOff := offset + int64(len(data))
	var convID, authEmail, quotaLine string
	for _, line := range strings.Split(string(data), "\n") {
		for _, rx := range convLogRes {
			if m := rx.FindStringSubmatch(line); m != nil {
				convID = m[1]
			}
		}
		if m := authEmailRe.FindStringSubmatch(line); m != nil {
			authEmail = strings.ToLower(strings.TrimSpace(m[1]))
		}
		if BackendLogQuotaRe.MatchString(line) {
			quotaLine = line
		}
	}
	return newOff, convID, authEmail, quotaLine
}

// ConversationExists checks whether convID has an actual persisted local conversation store
// (.db or legacy .pb) under ~/.gemini/antigravity-cli/conversations/.
// Ephemeral /btw side questions never create a persisted store and return false.
func ConversationExists(convID string) bool {
	id := strings.TrimSpace(convID)
	if id == "" || !uuidPrefixRe.MatchString(id) || len(id) != 36 {
		return false
	}
	dir := vault.ConversationsDir()
	for _, ext := range []string{".db", ".pb"} {
		if info, err := os.Stat(filepath.Join(dir, id+ext)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func FindNewestConversationSince(start time.Time) string {
	entries, err := os.ReadDir(vault.ConversationsDir())
	if err != nil {
		return ""
	}
	cutoff := start.Add(-time.Second)
	var bestTime time.Time
	var bestID string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".db") && !strings.HasSuffix(name, ".pb") {
			continue
		}
		m := uuidPrefixRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		// Skip subagent conversations (ResolveRootID returns a different parent UUID)
		candID := m[1]
		if root := sessions.ResolveRootID("", candID); root != "" && root != candID {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if !mt.Before(cutoff) && mt.After(bestTime) {
			bestTime = mt
			bestID = candID
		}
	}
	return bestID
}

// BuildAgyArgs constructs official `agy` CLI flags with zero intrusion into model/settings:
// user flags (including --model, --effort, --settings, /model in TUI) are respected natively.
func BuildAgyArgs(baseArgs []string, logPath, resumeID, promptInteractive string) []string {
	args := []string{
		"--log-file", logPath,
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if promptInteractive != "" {
		args = append(args, "--prompt-interactive", promptInteractive)
	}
	return append(args, baseArgs...)
}

// ExhaustionKind inspects a live quota probe result and returns the exhausted pool:
//   - "7d" / "5h" for Gemini pool (checked 7d first because 7d blocks even when 5h > 0%!)
//   - "claude-7d" / "claude-5h" for Claude & GPT pool
//   - "" if all known pools still have > 0.5% remaining, or if probe errored/timed out
func ExhaustionKind(q *vault.Quota) string {
	if q == nil || q.Error != "" {
		return ""
	}
	if q.D7Rem != nil && *q.D7Rem <= 0.5 {
		return "7d"
	}
	if q.H5Rem != nil && *q.H5Rem <= 0.5 {
		return "5h"
	}
	if q.C7Rem != nil && *q.C7Rem <= 0.5 {
		return "claude-7d"
	}
	if q.C5Rem != nil && *q.C5Rem <= 0.5 {
		return "claude-5h"
	}
	return ""
}

// IsQuotaTrulyExhausted returns true when Google confirms either 7d <= 0.5% or 5h <= 0.5%.
func IsQuotaTrulyExhausted(q *vault.Quota) bool {
	return ExhaustionKind(q) != ""
}

// ShouldFailoverAfterProbe decides whether to switch accounts after a backend 429:
//   - If live probe succeeded and confirmed 7d or 5h <= 0.5% -> failover with exact kind ("7d" / "5h")
//   - If live probe succeeded and BOTH 5h & 7d > 0.5% -> transient 429, DO NOT failover!
//   - If live probe itself timed out/errored AND the backend line was a fatal turn-ending
//     `agent executor error: ... ResourceExhausted` -> fallback failover ("suspected") with short cooldown
func ShouldFailoverAfterProbe(q *vault.Quota, backendLine string) (bool, string) {
	if kind := ExhaustionKind(q); kind != "" {
		return true, kind
	}
	if q != nil && q.Error == "" && q.H5Rem != nil && q.D7Rem != nil {
		return false, ""
	}
	if FatalTurnQuotaRe.MatchString(backendLine) {
		return true, "suspected"
	}
	return false, ""
}

// TerminalGuard saves baseline termios before starting child agy and guarantees
// termios + modern keyboard/mouse protocols are cleanly restored if child is killed.
type TerminalGuard struct {
	stdinFd  int
	stdout   *os.File
	isTTY    bool
	mu       sync.Mutex
	saved    *term.State
	restored bool
}

func NewTerminalGuard(stdinFd int, stdout *os.File) *TerminalGuard {
	outFd := int(stdout.Fd())
	return &TerminalGuard{
		stdinFd: stdinFd,
		stdout:  stdout,
		isTTY:   term.IsTerminal(stdinFd) && term.IsTerminal(outFd),
	}
}

func (g *TerminalGuard) IsTTY() bool { return g.isTTY }

// Save snapshots baseline termios WITHOUT switching the terminal into raw mode
// (letting child agy own raw mode directly with zero middleman interference).
func (g *TerminalGuard) Save() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.isTTY {
		return nil
	}
	st, err := term.GetState(g.stdinFd)
	if err != nil {
		return err
	}
	g.saved = st
	g.restored = false
	return nil
}

// EnterRaw is kept for testing raw-mode restoration.
func (g *TerminalGuard) EnterRaw() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.isTTY {
		return nil
	}
	st, err := term.MakeRaw(g.stdinFd)
	if err != nil {
		return err
	}
	g.saved = st
	g.restored = false
	return nil
}

func (g *TerminalGuard) Restore() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.isTTY || g.restored {
		return
	}
	g.restored = true
	if g.saved != nil {
		_ = term.Restore(g.stdinFd, g.saved)
		g.saved = nil
	}
	// Use os.File.Write (handles EINTR & short writes properly, unlike raw unix.Write)
	_, _ = g.stdout.Write(TerminalResetSeq)
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// RunInteractiveOnce launches `agy` directly on the user's real TTY (os.Stdin/Stdout/Stderr)
// with ZERO PTY proxy layer!
//
// Why direct TTY inheritance:
//   - Zero double-PTY buffering or EINTR/short-write frame drops (eliminates TUI ghosting/duplication).
//   - Zero SIGWINCH forwarding lag (kernel delivers window resizes directly to agy).
//   - 100% byte-identical rendering to launching official `agy` directly.
//   - agy-pro passively tails --log-file in the background for verified 5h/7d exhaustion,
//     and TerminalGuard guarantees clean termios + Kitty/modifyOtherKeys reset on forced failover/SIGTERM.
func RunInteractiveOnce(
	agyBin string,
	agyArgs []string,
	logPath string,
	opts InteractiveOptions,
	acct *vault.Account,
	autoSwitch bool,
	env []string,
) *RunResult {
	var releaseOnce sync.Once
	releaseLaunch := func() {
		releaseOnce.Do(func() {
			if opts.ReleaseLaunchLock != nil {
				opts.ReleaseLaunchLock()
			}
		})
	}
	defer releaseLaunch()

	startTime := time.Now()
	stdinFd := int(os.Stdin.Fd())

	guard := NewTerminalGuard(stdinFd, os.Stdout)
	_ = guard.Save()
	defer guard.Restore()

	cwd, _ := os.Getwd()
	metaReader := sessions.OpenMetaReader("")
	defer metaReader.Close()

	notifier := NewNotifier(opts.NotifyEnabled, DefaultNotifyMinDuration)
	titleSync := NewTabTitleSync(os.Stdout, guard.IsTTY(), opts.CustomTitle, filepath.Base(cwd))
	if opts.InitialConvID != "" {
		if meta, ok := metaReader.Lookup(opts.InitialConvID); ok {
			titleSync.Update(meta.Title, meta.Preview, meta.Running)
			notifier.Observe(titleSync.CurrentTitle(), meta.Running, time.Now())
		}
	}
	titleSync.Start()
	defer titleSync.Restore()

	cmd := exec.Command(agyBin, agyArgs...)
	if len(env) > 0 {
		cmd.Env = env
	} else {
		cmd.Env = vault.CleanNativeEnv("")
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		releaseLaunch()
		return &RunResult{ReturnCode: 1, Stderr: err.Error()}
	}

	// Let foreground child `agy` handle Ctrl+C (SIGINT) directly without killing agy-pro.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)

	stopSigCh := make(chan os.Signal, 2)
	notifyTerminationSignals(stopSigCh)
	defer signal.Stop(stopSigCh)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	type verifyResult struct {
		q    *vault.Quota
		line string
	}
	verifyCh := make(chan verifyResult, 1)

	var (
		convID          = opts.InitialConvID
		authEmail       = ""
		launchHeld      = opts.ReleaseLaunchLock != nil
		quotaHit        = false
		exhaustionKind  = ""
		quotaText       = ""
		verifiedQuota   *vault.Quota
		logOffset       int64
		waitErr         error
		terminatedBySig = false
		verifying       = false
		lastVerifyAt    time.Time
		fatalDetectedAt time.Time
		fatalLine       = ""
	)

	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()

	spinTicker := time.NewTicker(120 * time.Millisecond)
	defer spinTicker.Stop()

	// Fast 50ms startup handshake poll so launch.lock is released immediately after agy loads token (~45ms).
	var handshakeTicker *time.Ticker
	var handshakeC <-chan time.Time
	if launchHeld {
		handshakeTicker = time.NewTicker(50 * time.Millisecond)
		handshakeC = handshakeTicker.C
		defer handshakeTicker.Stop()
	}

loop:
	for {
		select {
		case waitErr = <-waitCh:
			releaseLaunch()
			break loop
		case <-stopSigCh:
			releaseLaunch()
			terminatedBySig = true
			waitErr = stopChildGraceful(cmd.Process, waitCh, 1500*time.Millisecond)
			break loop
		case <-handshakeC:
			newOff, cID, aEm, _ := ScanLog(logPath, logOffset)
			logOffset = newOff
			if cID != "" && cID != convID {
				if convID != "" {
					titleSync.OnConversationSwitch()
				}
				convID = cID
				if opts.OnConvChange != nil {
					opts.OnConvChange(convID)
				}
			}
			if aEm != "" {
				authEmail = aEm
			}
			if authEmail != "" || time.Since(startTime) >= 1500*time.Millisecond {
				releaseLaunch()
				launchHeld = false
				handshakeTicker.Stop()
				handshakeC = nil
			}
		case <-spinTicker.C:
			titleSync.TickSpinner()
		case vr := <-verifyCh:
			verifying = false
			if shouldSwitch, kind := ShouldFailoverAfterProbe(vr.q, vr.line); shouldSwitch {
				quotaHit = true
				exhaustionKind = kind
				quotaText = vr.line
				if vr.q != nil && vr.q.Error == "" {
					verifiedQuota = vr.q
				}
				titleSync.SetSwitching()
				waitErr = stopChildGraceful(cmd.Process, waitCh, 1500*time.Millisecond)
				break loop
			}
		case <-ticker.C:
			newOff, cID, aEm, qLine := ScanLog(logPath, logOffset)
			logOffset = newOff
			if cID != "" && cID != convID {
				if convID != "" {
					titleSync.OnConversationSwitch()
				}
				convID = cID
				if opts.OnConvChange != nil {
					opts.OnConvChange(convID)
				}
			}
			if aEm != "" {
				authEmail = aEm
				if launchHeld {
					releaseLaunch()
					launchHeld = false
					if handshakeTicker != nil {
						handshakeTicker.Stop()
						handshakeC = nil
					}
				}
			}
			if convID != "" {
				if meta, ok := metaReader.Lookup(convID); ok {
					titleSync.Update(meta.Title, meta.Preview, meta.Running)
					notifier.Observe(titleSync.CurrentTitle(), meta.Running, time.Now())
				}
			}
			// Fast path: agy logged a fatal turn-ending `agent executor error: ... (ResourceExhausted, HTTP 429)`
			// AFTER exhausting all internal retries -> wait 1.0s for trajectory flush, then switch immediately
			// (~1.5s total) while a detached background verifier fetches exact Google reset times!
			if autoSwitch && qLine != "" && fatalDetectedAt.IsZero() && FatalTurnQuotaRe.MatchString(qLine) {
				fatalDetectedAt = time.Now()
				fatalLine = qLine
			}
			if !fatalDetectedAt.IsZero() && time.Since(fatalDetectedAt) >= 1000*time.Millisecond {
				quotaHit = true
				exhaustionKind = "fast"
				quotaText = fatalLine
				titleSync.SetSwitching()
				waitErr = stopChildGraceful(cmd.Process, waitCh, 1500*time.Millisecond)
				break loop
			}
			// Non-fatal 429 path: verify live quota quietly without interrupting agy unless confirmed
			if autoSwitch && acct != nil && qLine != "" && fatalDetectedAt.IsZero() && !verifying && time.Since(lastVerifyAt) >= 25*time.Second {
				verifying = true
				lastVerifyAt = time.Now()
				capturedLine := qLine
				go func() {
					q := quota.ProbeAccount(acct, agyBin, 35*time.Second)
					select {
					case verifyCh <- verifyResult{q: q, line: capturedLine}:
					default:
					}
				}()
			}
		}
	}

	releaseLaunch()
	guard.Restore()

	_, cID, aEm, _ := ScanLog(logPath, logOffset)
	if cID != "" {
		convID = cID
	}
	if aEm != "" {
		authEmail = aEm
	}
	if convID == "" {
		convID = FindNewestConversationSince(startTime)
	}

	return &RunResult{
		ReturnCode:     exitCodeOf(waitErr),
		QuotaHit:       quotaHit && !terminatedBySig,
		ExhaustionKind: exhaustionKind,
		QuotaText:      quotaText,
		VerifiedQuota:  verifiedQuota,
		ConvID:         convID,
		Title:          titleSync.CurrentTitle(),
		AuthEmail:      authEmail,
	}
}

// RunPrintOnce executes non-interactive `agy --print ...` completely detached from /dev/tty.
func RunPrintOnce(agyBin string, agyArgs []string, logPath string, acct *vault.Account, autoSwitch bool, env []string) *RunResult {
	startTime := time.Now()
	cmd := exec.Command(agyBin, agyArgs...)
	if len(env) > 0 {
		cmd.Env = env
	} else {
		cmd.Env = vault.CleanNativeEnv("")
	}
	cmd.Stdin = nil
	vault.DetachCmd(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	combined := stdout.String() + "\n" + stderr.String()
	_, convID, authEmail, qLine := ScanLog(logPath, 0)
	if convID == "" {
		convID = FindNewestConversationSince(startTime)
	}

	suspected := qLine != "" || PrintModeQuotaRe.MatchString(combined)
	var verifiedQuota *vault.Quota
	confirmed := false
	exhaustionKind := ""
	if autoSwitch && suspected && acct != nil {
		q := quota.ProbeAccount(acct, agyBin, 35*time.Second)
		if ok, kind := ShouldFailoverAfterProbe(q, qLine); ok {
			confirmed = true
			exhaustionKind = kind
			if q != nil && q.Error == "" {
				verifiedQuota = q
			}
		}
	}

	qText := qLine
	if qText == "" {
		qText = combined
	}
	return &RunResult{
		ReturnCode:     exitCodeOf(err),
		Stdout:         stdout.String(),
		Stderr:         stderr.String(),
		QuotaHit:       confirmed,
		ExhaustionKind: exhaustionKind,
		QuotaText:      qText,
		VerifiedQuota:  verifiedQuota,
		ConvID:         convID,
		AuthEmail:      authEmail,
	}
}
