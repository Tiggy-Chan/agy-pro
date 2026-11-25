package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/Tiggy-Chan/agy-pro/internal/quota"
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

const ResumeContinuePrompt = "刚刚因上一个账号额度耗尽（5小时或7天池触顶），agy-pro 已自动切换到新账号并恢复当前会话。请先核对刚才中断前的进展与本地文件状态，然后直接继续完成未完成的任务。"

const uuidPat = `([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`

var (
	ansiRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)
	convLogRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:Resuming|Streaming|Starting conversation update stream for|GetConversationDetail: found|Created(?: new)?)\s+conversation\s+` + uuidPat),
		regexp.MustCompile(`(?i)cascade_id="` + uuidPat + `"`),
	}
	uuidPrefixRe = regexp.MustCompile(`^` + uuidPat)
	authEmailRe  = regexp.MustCompile(`applyAuthResult:\s+email=([^\s,]+)`)

	// BackendLogQuotaRe matches ONLY genuine agy Go backend RPC quota errors in --log-file.
	// It NEVER matches user messages, assistant text, tool output, or RerenderAll scrollback!
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
	ExhaustionKind string // "7d", "5h", or "suspected"
	QuotaText      string
	VerifiedQuota  *vault.Quota
	ConvID         string
	AuthEmail      string
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

func StripLockedArgs(args []string, allowModelOverride bool) []string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == "--log-file" || (!allowModelOverride && a == "--model") {
			skip = true
			continue
		}
		if strings.HasPrefix(a, "--log-file=") || (!allowModelOverride && strings.HasPrefix(a, "--model=")) {
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

func FindNewestConversationSince(start time.Time) string {
	entries, err := os.ReadDir(vault.ConversationsDir())
	if err != nil {
		return ""
	}
	cutoff := start.Add(-time.Second)
	var bestTime time.Time
	var bestID string
	for _, e := range entries {
		m := uuidPrefixRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if !mt.Before(cutoff) && mt.After(bestTime) {
			bestTime = mt
			bestID = m[1]
		}
	}
	return bestID
}

func BuildAgyArgs(baseArgs []string, logPath, model, resumeID, promptInteractive string) []string {
	settingsBytes, _ := json.Marshal(map[string]string{
		"modelProvider": "",
		"model":         model,
	})
	args := []string{
		"--settings", string(settingsBytes),
		"--model", model,
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

// ExhaustionKind inspects a live quota probe result and returns:
//   - "7d" if weekly remaining <= 0.5% (checked FIRST because 7d exhaustion blocks even when 5h > 0%!)
//   - "5h" if 5-hour remaining <= 0.5%
//   - ""   if account still has usable quota OR if probe errored/timed out
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
	return ""
}

// IsQuotaTrulyExhausted checks a live quota probe result: returns true when
// Google confirms either 7d remaining <= 0.5% OR 5h remaining <= 0.5%.
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
		// Live probe succeeded and proved BOTH 5h and 7d still have remaining quota -> transient blip
		return false, ""
	}
	if FatalTurnQuotaRe.MatchString(backendLine) {
		return true, "suspected"
	}
	return false, ""
}

// TerminalGuard saves and unconditionally restores raw termios AND modern terminal modes.
type TerminalGuard struct {
	stdinFd  int
	stdoutFd int
	isTTY    bool
	mu       sync.Mutex
	saved    *term.State
	restored bool
}

func NewTerminalGuard(stdinFd, stdoutFd int) *TerminalGuard {
	return &TerminalGuard{
		stdinFd:  stdinFd,
		stdoutFd: stdoutFd,
		isTTY:    term.IsTerminal(stdinFd) && term.IsTerminal(stdoutFd),
	}
}

func (g *TerminalGuard) IsTTY() bool { return g.isTTY }

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
	_, _ = unix.Write(g.stdoutFd, TerminalResetSeq)
}

func killProcessGroupGraceful(pid int, waitCh <-chan error, grace time.Duration) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case err := <-waitCh:
		return err
	case <-time.After(grace):
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return <-waitCh
	}
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

// RunInteractiveOnce executes `agy` inside a full-fidelity PTY.
//
// Zero-false-positive quota detection:
//   1. NEVER inspects PTY stdout for quota strings (immune to historical conversation
//      redraws, user-pasted error text, or tool output).
//   2. Watches ONLY structured Go backend HTTP 429 ResourceExhausted lines in --log-file.
//   3. Before killing agy or blacklisting the account, verifies live quota via an
//      isolated `quota.ProbeAccount` call; transient 429s when quota remains > 0.5%
//      NEVER kill the session or cool down healthy accounts.
func RunInteractiveOnce(
	agyBin string,
	agyArgs []string,
	logPath string,
	initialConvID string,
	acct *vault.Account,
	autoSwitch bool,
) *RunResult {
	startTime := time.Now()
	stdinFd := int(os.Stdin.Fd())
	stdoutFd := int(os.Stdout.Fd())

	guard := NewTerminalGuard(stdinFd, stdoutFd)
	_ = guard.EnterRaw()
	defer guard.Restore()

	cmd := exec.Command(agyBin, agyArgs...)
	cmd.Env = vault.CleanNativeEnv("")

	var winSize *pty.Winsize
	if guard.IsTTY() {
		if ws, err := pty.GetsizeFull(os.Stdin); err == nil {
			winSize = ws
		}
	}

	ptmx, err := pty.StartWithSize(cmd, winSize)
	if err != nil {
		return &RunResult{ReturnCode: 1, Stderr: err.Error()}
	}
	defer ptmx.Close()

	winchCh := make(chan os.Signal, 2)
	if guard.IsTTY() {
		signal.Notify(winchCh, syscall.SIGWINCH)
		defer signal.Stop(winchCh)
		go func() {
			for range winchCh {
				_ = pty.InheritSize(os.Stdin, ptmx)
				if cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGWINCH)
				}
			}
		}()
	}

	stopSigCh := make(chan os.Signal, 2)
	signal.Notify(stopSigCh, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stopSigCh)

	stopIO := make(chan struct{})
	var ioWG sync.WaitGroup

	// Stdin -> PTY (cancellable via unix.Poll)
	if guard.IsTTY() {
		ioWG.Add(1)
		go func() {
			defer ioWG.Done()
			buf := make([]byte, 4096)
			pollFds := []unix.PollFd{{Fd: int32(stdinFd), Events: unix.POLLIN}}
			for {
				select {
				case <-stopIO:
					return
				default:
				}
				nReady, err := unix.Poll(pollFds, 80)
				if err != nil {
					if errors.Is(err, syscall.EINTR) {
						continue
					}
					return
				}
				if nReady <= 0 {
					continue
				}
				if pollFds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
					return
				}
				if pollFds[0].Revents&unix.POLLIN != 0 {
					n, rerr := unix.Read(stdinFd, buf)
					if n > 0 {
						if _, werr := ptmx.Write(buf[:n]); werr != nil {
							return
						}
					}
					if rerr != nil {
						if errors.Is(rerr, syscall.EINTR) {
							continue
						}
						return
					}
					if n == 0 {
						return
					}
				}
			}
		}()
	}

	// PTY -> Stdout transparent passthrough (ZERO regex scanning on terminal text!)
	ptyDone := make(chan struct{})
	go func() {
		defer close(ptyDone)
		buf := make([]byte, 8192)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				_, _ = unix.Write(stdoutFd, buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	type verifyResult struct {
		q    *vault.Quota
		line string
	}
	verifyCh := make(chan verifyResult, 1)

	var (
		convID          = initialConvID
		authEmail       = ""
		quotaHit        = false
		exhaustionKind  = ""
		quotaText       = ""
		verifiedQuota   *vault.Quota
		logOffset       int64
		waitErr         error
		terminatedBySig = false
		verifying       = false
		lastVerifyAt    time.Time
	)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

loop:
	for {
		select {
		case waitErr = <-waitCh:
			break loop
		case <-stopSigCh:
			terminatedBySig = true
			if cmd.Process != nil {
				waitErr = killProcessGroupGraceful(cmd.Process.Pid, waitCh, 1500*time.Millisecond)
			}
			break loop
		case vr := <-verifyCh:
			verifying = false
			if shouldSwitch, kind := ShouldFailoverAfterProbe(vr.q, vr.line); shouldSwitch {
				quotaHit = true
				exhaustionKind = kind
				quotaText = vr.line
				if vr.q != nil && vr.q.Error == "" {
					verifiedQuota = vr.q
				}
				if cmd.Process != nil {
					waitErr = killProcessGroupGraceful(cmd.Process.Pid, waitCh, 1500*time.Millisecond)
				}
				break loop
			}
			// Transient 429 while account still has both 5h & 7d quota -> DO NOT kill agy!
		case <-ticker.C:
			newOff, cID, aEm, qLine := ScanLog(logPath, logOffset)
			logOffset = newOff
			if cID != "" {
				convID = cID
			}
			if aEm != "" {
				authEmail = aEm
			}
			// Only trigger live quota verification on genuine backend HTTP 429 ResourceExhausted
			// (debounced to at most once every 25s)
			if autoSwitch && acct != nil && qLine != "" && !verifying && time.Since(lastVerifyAt) >= 25*time.Second {
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

	close(stopIO)
	ioWG.Wait()
	select {
	case <-ptyDone:
	case <-time.After(150 * time.Millisecond):
	}
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
		AuthEmail:      authEmail,
	}
}

// RunPrintOnce executes non-interactive `agy --print ...` completely detached from /dev/tty.
func RunPrintOnce(agyBin string, agyArgs []string, logPath string, acct *vault.Account, autoSwitch bool) *RunResult {
	startTime := time.Now()
	cmd := exec.Command(agyBin, agyArgs...)
	cmd.Env = vault.CleanNativeEnv("")
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

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
