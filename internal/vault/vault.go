package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
)

// Version is the build version string, overridable via -ldflags "-X .../internal/vault.Version=...".
var Version = "0.1.0"

const (
	DefaultAutoCompact       = "90%"
	AutoCompactEnvKey        = "AGY_CLI_AUTO_COMPACT_OVERRIDE"
	ForceFileTokenEnvKey     = "AGY_CLI_FORCE_FILE_TOKEN_STORAGE"
	ForceOSC8EnvKey          = "AGY_CLI_FORCE_OSC8"
	SeamlessActiveProxyEnv   = "AGY_PRO_SEAMLESS_ACTIVE_PROXY"
	SeamlessActiveCAEnv      = "AGY_PRO_SEAMLESS_ACTIVE_CA"
	SeamlessOrigProxyEnv     = "AGY_PRO_SEAMLESS_ORIG_HTTPS_PROXY"
	SeamlessOrigSSLCertEnv   = "AGY_PRO_SEAMLESS_ORIG_SSL_CERT_FILE"
	CtrlURLEnvKey            = "AGY_PRO_CTRL_URL"
	CtrlTokenEnvKey          = "AGY_PRO_CTRL_TOKEN"
)

var StripEnvKeys = []string{
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"GOOGLE_GEMINI_BASE_URL",
	"AGY_ADC_AUTH",
	"GOOGLE_GENAI_USE_VERTEXAI",
}

type Account struct {
	Email     string
	SlotID    string
	HomeDir   string
	TokenFile string
}

type Cooldown struct {
	Until  float64 `json:"until"`
	Reason string  `json:"reason"`
	At     float64 `json:"at"`
}

type Quota struct {
	H5Used  *float64 `json:"h5_used"`
	H5Rem   *float64 `json:"h5_rem"`
	H5Reset float64  `json:"h5_reset"`
	D7Used  *float64 `json:"d7_used"`
	D7Rem   *float64 `json:"d7_rem"`
	D7Reset float64  `json:"d7_reset"`
	C5Used  *float64 `json:"c5_used,omitempty"`
	C5Rem   *float64 `json:"c5_rem,omitempty"`
	C5Reset float64  `json:"c5_reset,omitempty"`
	C7Used  *float64 `json:"c7_used,omitempty"`
	C7Rem   *float64 `json:"c7_rem,omitempty"`
	C7Reset float64  `json:"c7_reset,omitempty"`
	At      float64  `json:"at"`
	Error   string   `json:"error"`
}

type State struct {
	Active             string               `json:"active"`
	Order              []string             `json:"order"`
	ReservePct         float64              `json:"reserve_pct,omitempty"`
	AutoCompact        string               `json:"auto_compact,omitempty"`
	NotifyDisabled     bool                 `json:"notify_disabled,omitempty"`
	SeamlessDisabled   bool                 `json:"seamless_disabled,omitempty"`
	Language           string               `json:"language,omitempty"`
	FirstRunConfigured bool                 `json:"first_run_configured,omitempty"`
	Cooldowns          map[string]*Cooldown `json:"cooldowns"`
	Quotas             map[string]*Quota    `json:"quotas"`
	LastConv           map[string]string    `json:"last_conv"`
}

type TokenMeta struct {
	Email       string
	AccessToken string
	Expiry      float64
	HasRefresh  bool
	AuthMethod  string
}

type Vault struct {
	StateDir       string
	AccountsDir    string
	LogsDir        string
	LiveDir        string
	StateFile      string
	LockFile       string
	LaunchLockFile string
}

type LiveSession struct {
	PID       int     `json:"pid"`
	ConvID    string  `json:"conv_id"`
	Email     string  `json:"email"`
	TTY       string  `json:"tty,omitempty"`
	CWD       string  `json:"cwd,omitempty"`
	StartedAt float64 `json:"started_at"`
}

func HomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		if runtime.GOOS == "windows" {
			if up := os.Getenv("USERPROFILE"); up != "" {
				return up
			}
		}
		return os.Getenv("HOME")
	}
	return h
}

func AgyDir() string {
	return filepath.Join(HomeDir(), ".gemini", "antigravity-cli")
}

func ActiveTokenFile() string {
	return filepath.Join(AgyDir(), "antigravity-oauth-token")
}

func ConversationsDir() string {
	return filepath.Join(AgyDir(), "conversations")
}

func DefaultStateDir() string {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "agy-pro")
		}
		return filepath.Join(HomeDir(), "AppData", "Local", "agy-pro")
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "agy-pro")
	}
	return filepath.Join(HomeDir(), ".local", "state", "agy-pro")
}

// SQLiteReadOnlyDSN builds a portable SQLite read-only URI DSN across Unix and Windows paths
// (handling spaces, #, ?, backslashes, Windows drive letters, and UNC shares cleanly).
func SQLiteReadOnlyDSN(dbPath string, busyTimeoutMs int) string {
	if busyTimeoutMs <= 0 {
		busyTimeoutMs = 1000
	}
	p := filepath.ToSlash(dbPath)
	query := fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(1)", busyTimeoutMs)
	if strings.HasPrefix(p, "//") {
		// UNC share path: //server/share/path -> file://server/share/path?...
		rest := strings.TrimPrefix(p, "//")
		host, sharePath, _ := strings.Cut(rest, "/")
		u := url.URL{
			Scheme:   "file",
			Host:     host,
			Path:     "/" + sharePath,
			RawQuery: query,
		}
		return u.String()
	}
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) {
		p = "/" + p
	}
	u := url.URL{
		Scheme:   "file",
		Path:     p,
		RawQuery: query,
	}
	return u.String()
}

func (v *Vault) IsDefaultStateDir() bool {
	return filepath.Clean(v.StateDir) == filepath.Clean(DefaultStateDir())
}

func New(stateDir string) *Vault {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return &Vault{
		StateDir:       stateDir,
		AccountsDir:    filepath.Join(stateDir, "accounts"),
		LogsDir:        filepath.Join(stateDir, "logs"),
		LiveDir:        filepath.Join(stateDir, "live"),
		StateFile:      filepath.Join(stateDir, "state.json"),
		LockFile:       filepath.Join(stateDir, "switch.lock"),
		LaunchLockFile: filepath.Join(stateDir, "launch.lock"),
	}
}

func FindAgyBinary() string {
	if ov := os.Getenv("AGY_PRO_BIN"); ov != "" {
		if info, err := os.Stat(ov); err == nil && IsExecutableFile(ov, info) {
			return ov
		}
		if runtime.GOOS == "windows" && filepath.Ext(ov) == "" {
			withExe := ov + ".exe"
			if info, err := os.Stat(withExe); err == nil && IsExecutableFile(withExe, info) {
				return withExe
			}
		}
	}
	var candidates []string
	var fallback string
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			p := filepath.Join(local, "agy", "bin", "agy.exe")
			candidates = append(candidates, p)
			fallback = p
		}
		homeLocal := filepath.Join(HomeDir(), "AppData", "Local", "agy", "bin", "agy.exe")
		candidates = append(candidates,
			homeLocal,
			filepath.Join(HomeDir(), ".local", "bin", "agy.exe"),
		)
		if fallback == "" {
			fallback = homeLocal
		}
	} else {
		fallback = filepath.Join(HomeDir(), ".local", "bin", "agy")
		candidates = []string{
			fallback,
			"/usr/local/bin/agy",
			"/usr/bin/agy",
		}
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && IsExecutableFile(c, info) {
			return c
		}
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p
	}
	return fallback
}

func ParseISOTime(raw string) float64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Year() <= 1 {
				return 0
			}
			return float64(t.UnixNano()) / 1e9
		}
	}
	return 0
}

func FmtDuration(seconds float64) string {
	sec := int(seconds)
	if sec < 0 {
		sec = 0
	}
	d := sec / 86400
	rem := sec % 86400
	h := rem / 3600
	rem %= 3600
	m := rem / 60
	s := rem % 60
	var b strings.Builder
	if d > 0 {
		fmt.Fprintf(&b, "%dd", d)
	}
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

func FmtLocalTime(epoch float64) string {
	if epoch <= 0 {
		return "-"
	}
	return time.Unix(int64(epoch), 0).Local().Format("01-02 15:04")
}

func FmtAge(epoch float64, now float64) string {
	if epoch <= 0 {
		return i18n.T("vault.no_cache")
	}
	diff := int(now - epoch)
	if diff < 0 {
		diff = 0
	}
	switch {
	case diff < 60:
		return i18n.T("vault.age_s", diff)
	case diff < 3600:
		return i18n.T("vault.age_m", diff/60)
	case diff < 86400:
		return i18n.T("vault.age_h", diff/3600)
	default:
		return i18n.T("vault.age_d", diff/86400)
	}
}

func SlotIDForEmail(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])[:16]
}

func ReadTokenMeta(path string) (*TokenMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		IDTokenEmail string `json:"id_token_email"`
		Email        string `json:"email"`
		AuthMethod   string `json:"auth_method"`
		Token        *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Expiry       string `json:"expiry"`
		} `json:"token"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.Token == nil {
		return nil, errors.New("missing token object")
	}
	hasRefresh := raw.Token.RefreshToken != ""
	hasAccess := raw.Token.AccessToken != ""
	if !hasRefresh && !hasAccess {
		return nil, errors.New("empty token")
	}
	email := strings.ToLower(strings.TrimSpace(raw.IDTokenEmail))
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(raw.Email))
	}
	method := raw.AuthMethod
	if method == "" {
		method = "consumer"
	}
	return &TokenMeta{
		Email:       email,
		AccessToken: raw.Token.AccessToken,
		Expiry:      ParseISOTime(raw.Token.Expiry),
		HasRefresh:  hasRefresh,
		AuthMethod:  method,
	}, nil
}

func AtomicWriteFile(dst string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp_*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := renameFileAtomic(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(dst, perm)
}

func AtomicCopySecret(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return AtomicWriteFile(dst, data, 0o600)
}

func CleanNativeEnv(overrideHome string) []string {
	return CleanNativeEnvWithCompact(overrideHome, "", false)
}

// CleanNativeEnvWithCompact prepares a clean environment for native `agy`.
// When forceCompact is true, AGY_CLI_AUTO_COMPACT_OVERRIDE is explicitly set to
// autoCompact (or stripped when autoCompact == ""). When forceCompact is false,
// an existing AGY_CLI_AUTO_COMPACT_OVERRIDE in the caller's environment is preserved
// or defaulted to DefaultAutoCompact ("80%").
func normEnvKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

func CleanNativeEnvWithCompact(overrideHome, autoCompact string, forceCompact bool) []string {
	stripSet := make(map[string]struct{}, len(StripEnvKeys)+8)
	for _, k := range StripEnvKeys {
		stripSet[normEnvKey(k)] = struct{}{}
	}
	stripSet[normEnvKey(ForceFileTokenEnvKey)] = struct{}{}
	if overrideHome != "" {
		stripSet[normEnvKey("HOME")] = struct{}{}
		if runtime.GOOS == "windows" {
			stripSet["USERPROFILE"] = struct{}{}
			stripSet["HOMEDRIVE"] = struct{}{}
			stripSet["HOMEPATH"] = struct{}{}
		}
	}
	if forceCompact {
		stripSet[normEnvKey(AutoCompactEnvKey)] = struct{}{}
	}

	orig := os.Environ()
	activeSeamlessProxy := strings.TrimSpace(os.Getenv(SeamlessActiveProxyEnv))
	activeSeamlessCA := strings.TrimSpace(os.Getenv(SeamlessActiveCAEnv))
	origHTTPSProxy := strings.TrimSpace(os.Getenv(SeamlessOrigProxyEnv))
	origSSLCert := strings.TrimSpace(os.Getenv(SeamlessOrigSSLCertEnv))
	stripSeamlessEnv := activeSeamlessProxy != "" || activeSeamlessCA != ""

	out := make([]string, 0, len(orig)+8)
	hasCompactEnv := false
	hasOSC8Env := false
	compactKeyNorm := normEnvKey(AutoCompactEnvKey)
	osc8KeyNorm := normEnvKey(ForceOSC8EnvKey)
	for _, kv := range orig {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		nk := normEnvKey(k)
		if _, drop := stripSet[nk]; drop {
			continue
		}
		// Never leak a parent agy-pro --seamless loopback proxy into detached probes, logins, or nested sessions.
		if stripSeamlessEnv {
			switch {
			case strings.EqualFold(k, SeamlessActiveProxyEnv),
				strings.EqualFold(k, SeamlessActiveCAEnv),
				strings.EqualFold(k, SeamlessOrigProxyEnv),
				strings.EqualFold(k, SeamlessOrigSSLCertEnv),
				strings.EqualFold(k, CtrlURLEnvKey),
				strings.EqualFold(k, CtrlTokenEnvKey):
				continue
			case (strings.EqualFold(k, "HTTPS_PROXY")) && activeSeamlessProxy != "" && strings.TrimSpace(val) == activeSeamlessProxy:
				continue
			case (strings.EqualFold(k, "SSL_CERT_FILE") || strings.EqualFold(k, "NODE_EXTRA_CA_CERTS")) && activeSeamlessCA != "" && strings.TrimSpace(val) == activeSeamlessCA:
				continue
			}
		}
		if nk == compactKeyNorm && strings.TrimSpace(val) != "" {
			hasCompactEnv = true
		}
		if nk == osc8KeyNorm && strings.TrimSpace(val) != "" {
			hasOSC8Env = true
		}
		out = append(out, kv)
	}
	if stripSeamlessEnv {
		if origHTTPSProxy != "" {
			out = append(out, "HTTPS_PROXY="+origHTTPSProxy)
		}
		if origSSLCert != "" {
			out = append(out, "SSL_CERT_FILE="+origSSLCert)
		}
	}
	// Use official AGY_CLI_FORCE_FILE_TOKEN_STORAGE=1 instead of spoofing SSH_CONNECTION
	// so agy keeps native local terminal detection (ssh=false) while isolating tokens cleanly.
	out = append(out, ForceFileTokenEnvKey+"=1")
	// Default AGY_CLI_FORCE_OSC8=0 unless caller explicitly overrides it:
	// native agy emits un-percent-encoded UTF-8 inside OSC-8 file:// hyperlinks, so CJK chars
	// whose UTF-8 bytes contain 0x9C (ST) + 0x8D (C1 Reverse Index, e.g. '服' = E6 9C 8D)
	// prematurely terminate the link and jump the terminal cursor up a row, causing severe
	// double/triple-line ghosting on every subsequent inline redraw.
	if !hasOSC8Env {
		out = append(out, ForceOSC8EnvKey+"=0")
	}
	if overrideHome != "" {
		out = append(out, "HOME="+overrideHome)
		if runtime.GOOS == "windows" {
			// Go's os.UserHomeDir() on Windows reads USERPROFILE (not HOME).
			out = append(out, "USERPROFILE="+overrideHome)
		}
	}
	if forceCompact {
		if autoCompact != "" {
			out = append(out, AutoCompactEnvKey+"="+autoCompact)
		}
	} else if !hasCompactEnv {
		out = append(out, AutoCompactEnvKey+"="+DefaultAutoCompact)
	}
	return out
}

func NormalizeAutoCompact(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New(i18n.T("vault.compact.empty"))
	}
	low := strings.ToLower(s)
	if low == "off" || low == "default" || low == "none" {
		return "off", nil
	}
	if strings.HasSuffix(s, "%") {
		numStr := strings.TrimSpace(strings.TrimSuffix(s, "%"))
		v, err := strconv.ParseFloat(numStr, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 100 {
			return "", errors.New(i18n.T("vault.compact.pct_range"))
		}
		if float64(int64(v)) == v {
			return fmt.Sprintf("%d%%", int64(v)), nil
		}
		return strconv.FormatFloat(v, 'f', -1, 64) + "%", nil
	}
	last := s[len(s)-1]
	if last == 'k' || last == 'K' || last == 'm' || last == 'M' {
		numStr := strings.TrimSpace(s[:len(s)-1])
		v, err := strconv.ParseFloat(numStr, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 1e9 {
			return "", fmt.Errorf("%s", i18n.T("vault.compact.invalid", raw))
		}
		return numStr + strings.ToUpper(string(last)), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("%s", i18n.T("vault.compact.invalid", raw))
	}
	return strconv.FormatInt(n, 10), nil
}

func EffectiveAutoCompact(st *State) string {
	if st == nil {
		return DefaultAutoCompact
	}
	v := strings.TrimSpace(st.AutoCompact)
	if v == "" {
		return DefaultAutoCompact
	}
	if strings.EqualFold(v, "off") || strings.EqualFold(v, "default") || strings.EqualFold(v, "none") {
		return ""
	}
	return v
}

func DisplayAutoCompact(st *State) string {
	if eff := EffectiveAutoCompact(st); eff != "" {
		return eff
	}
	return "off"
}

func StepAutoCompact(current string, deltaPct int) string {
	eff := EffectiveAutoCompact(&State{AutoCompact: current})
	if eff == "" {
		if deltaPct < 0 {
			return "95%"
		}
		return DefaultAutoCompact
	}
	if strings.HasSuffix(eff, "%") {
		if v, err := strconv.ParseFloat(strings.TrimSuffix(eff, "%"), 64); err == nil {
			next := int(v+0.5) + deltaPct
			if next > 95 {
				return "off"
			}
			if next < 30 {
				return "30%"
			}
			return fmt.Sprintf("%d%%", next)
		}
	}
	return DefaultAutoCompact
}

func ResolveAutoCompact(st *State, cliOverride string) (string, bool) {
	if strings.TrimSpace(cliOverride) != "" {
		if norm, err := NormalizeAutoCompact(cliOverride); err == nil {
			if norm == "off" {
				return "", true
			}
			return norm, true
		}
	}
	return EffectiveAutoCompact(st), true
}

func shellTargetFiles(homeDir string, allowCreateIfNone bool) []string {
	candidates := []string{".zshenv", ".zshrc", ".bashrc", ".profile"}
	seen := map[string]bool{}
	var paths []string
	add := func(name string) {
		p := filepath.Join(homeDir, name)
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, name := range candidates {
		p := filepath.Join(homeDir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			add(name)
		}
	}
	// Only create a default rc file if NO existing shell rc file was found at all,
	// avoiding unsolicited creation of extra dotfiles (like .zshenv or .profile).
	if len(paths) == 0 && allowCreateIfNone && runtime.GOOS != "windows" {
		sh := strings.ToLower(filepath.Base(os.Getenv("SHELL")))
		switch {
		case runtime.GOOS == "darwin" || strings.Contains(sh, "zsh"):
			add(".zshrc")
		case runtime.GOOS == "linux" || strings.Contains(sh, "bash"):
			add(".bashrc")
		default:
			add(".profile")
		}
	}
	return paths
}

func isManagedAutoCompactComment(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "# Added by agy-pro") ||
		strings.HasPrefix(t, "# Antigravity CLI (agy")
}

func isAutoCompactExportLine(line string) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") {
		return false
	}
	return strings.HasPrefix(t, "export "+AutoCompactEnvKey+"=") ||
		strings.HasPrefix(t, AutoCompactEnvKey+"=")
}

// SyncShellAutoCompact writes or updates AGY_CLI_AUTO_COMPACT_OVERRIDE in user shell rc files.
// On first-run setup (updateExisting=false), files that already define AGY_CLI_AUTO_COMPACT_OVERRIDE
// are left untouched. On explicit config changes (updateExisting=true), existing lines are updated
// or removed when effectiveVal is empty.
func SyncShellAutoCompact(homeDir, effectiveVal string, updateExisting bool) []string {
	if homeDir == "" {
		return nil
	}
	paths := shellTargetFiles(homeDir, effectiveVal != "" && !updateExisting)
	if updateExisting && effectiveVal != "" {
		paths = shellTargetFiles(homeDir, true)
	}
	var updated []string
	exportLine := ""
	if effectiveVal != "" {
		exportLine = fmt.Sprintf("export %s=%q", AutoCompactEnvKey, effectiveVal)
	}

	for _, p := range paths {
		perm := os.FileMode(0o644)
		raw, err := os.ReadFile(p)
		if err != nil {
			if !os.IsNotExist(err) || effectiveVal == "" {
				continue
			}
		} else if info, statErr := os.Stat(p); statErr == nil {
			perm = info.Mode().Perm()
		}

		orig := strings.ReplaceAll(string(raw), "\r\n", "\n")
		var lines []string
		if orig != "" {
			trimmed := strings.TrimSuffix(orig, "\n")
			lines = strings.Split(trimmed, "\n")
		}

		hasKey := false
		for _, ln := range lines {
			if isAutoCompactExportLine(ln) {
				hasKey = true
				break
			}
		}
		if hasKey && !updateExisting {
			continue
		}
		if !hasKey && effectiveVal == "" {
			continue
		}

		var out []string
		if hasKey {
			for i := 0; i < len(lines); i++ {
				if isAutoCompactExportLine(lines[i]) {
					if effectiveVal == "" {
						if len(out) > 0 && isManagedAutoCompactComment(out[len(out)-1]) {
							out = out[:len(out)-1]
							if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
								out = out[:len(out)-1]
							}
						}
						continue
					}
					out = append(out, exportLine)
				} else {
					out = append(out, lines[i])
				}
			}
		} else {
			out = append(out, lines...)
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			out = append(out, "# Added by agy-pro (default Auto-compact for agy & agy-pro)", exportLine)
		}

		next := ""
		if len(out) > 0 {
			next = strings.Join(out, "\n") + "\n"
		}
		if next == orig {
			continue
		}
		writePath := p
		if resolved, symErr := filepath.EvalSymlinks(p); symErr == nil && resolved != "" {
			writePath = resolved
		}
		if err := AtomicWriteFile(writePath, []byte(next), perm); err == nil {
			updated = append(updated, p)
		}
	}
	return updated
}

func ApplyLaunchctlAutoCompact(effectiveVal string, overwrite bool) {
	if runtime.GOOS != "darwin" {
		return
	}
	if !overwrite {
		if out, err := exec.Command("launchctl", "getenv", AutoCompactEnvKey).Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			return
		}
	}
	if effectiveVal == "" {
		_ = exec.Command("launchctl", "unsetenv", AutoCompactEnvKey).Run()
		return
	}
	_ = exec.Command("launchctl", "setenv", AutoCompactEnvKey, effectiveVal).Run()
}

func (v *Vault) EnsureDirs() error {
	for _, d := range []string{v.StateDir, v.AccountsDir, v.LogsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		_ = os.Chmod(d, 0o700)
	}
	return nil
}

func newEmptyState() *State {
	return &State{
		Order:       []string{},
		AutoCompact: DefaultAutoCompact,
		Cooldowns:   map[string]*Cooldown{},
		Quotas:      map[string]*Quota{},
		LastConv:    map[string]string{},
	}
}

func (v *Vault) LoadState() *State {
	_ = v.EnsureDirs()
	st := newEmptyState()
	data, err := os.ReadFile(v.StateFile)
	if err != nil {
		return st
	}
	if err := json.Unmarshal(data, st); err != nil {
		return newEmptyState()
	}
	if st.Order == nil {
		st.Order = []string{}
	}
	if strings.TrimSpace(st.AutoCompact) == "" {
		st.AutoCompact = DefaultAutoCompact
	}
	if st.Cooldowns == nil {
		st.Cooldowns = map[string]*Cooldown{}
	}
	if st.Quotas == nil {
		st.Quotas = map[string]*Quota{}
	}
	if st.LastConv == nil {
		st.LastConv = map[string]string{}
	}
	return st
}

func (v *Vault) SaveState(st *State) error {
	if err := v.EnsureDirs(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return AtomicWriteFile(v.StateFile, raw, 0o600)
}

// UpdateState atomically loads, mutates, and saves state.json under an exclusive flock,
// preventing races between foreground sessions and detached background quota verifiers.
func (v *Vault) UpdateState(fn func(st *State)) error {
	return v.withLock(func() error {
		st := v.LoadState()
		fn(st)
		return v.SaveState(st)
	})
}

// EnsureFirstRunConfig performs one-time initialization on first launch:
// persists the built-in 80% Auto-compact default in state.json and configures
// shell profiles (when homeDir is non-empty) so native `agy` also inherits it.
func (v *Vault) EnsureFirstRunConfig(homeDir string, applyLaunchctl bool) (bool, []string, error) {
	if _, err := os.Stat(v.StateFile); err == nil {
		if st := v.LoadState(); st.FirstRunConfigured {
			return false, nil, nil
		}
	}
	var didConfigure bool
	var touched []string
	err := v.withLock(func() error {
		st := v.LoadState()
		if st.FirstRunConfigured {
			return nil
		}
		if strings.TrimSpace(st.AutoCompact) == "" {
			st.AutoCompact = DefaultAutoCompact
		}
		eff := EffectiveAutoCompact(st)
		if homeDir != "" && eff != "" {
			touched = SyncShellAutoCompact(homeDir, eff, false)
		}
		if applyLaunchctl && eff != "" {
			ApplyLaunchctlAutoCompact(eff, false)
			touched = append(touched, syncOSUserAutoCompact(eff, false)...)
		}
		st.FirstRunConfigured = true
		didConfigure = true
		return v.SaveState(st)
	})
	return didConfigure, touched, err
}

// SetAutoCompact validates, persists, and syncs the Auto-compact threshold across state.json and shell profiles.
func (v *Vault) SetAutoCompact(rawVal, homeDir string, applyLaunchctl bool) (string, []string, error) {
	norm, err := NormalizeAutoCompact(rawVal)
	if err != nil {
		return "", nil, err
	}
	if err := v.UpdateState(func(st *State) {
		st.AutoCompact = norm
		st.FirstRunConfigured = true
	}); err != nil {
		return "", nil, err
	}
	eff := EffectiveAutoCompact(&State{AutoCompact: norm})
	var touched []string
	if homeDir != "" {
		touched = SyncShellAutoCompact(homeDir, eff, true)
	}
	if applyLaunchctl {
		ApplyLaunchctlAutoCompact(eff, true)
		touched = append(touched, syncOSUserAutoCompact(eff, true)...)
	}
	return norm, touched, nil
}

func (v *Vault) ListAccounts() map[string]*Account {
	_ = v.EnsureDirs()
	out := map[string]*Account{}
	entries, err := os.ReadDir(v.AccountsDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slotDir := filepath.Join(v.AccountsDir, e.Name())
		tok := filepath.Join(slotDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		meta, err := ReadTokenMeta(tok)
		if err != nil || meta.Email == "" {
			continue
		}
		out[meta.Email] = &Account{
			Email:     meta.Email,
			SlotID:    e.Name(),
			HomeDir:   slotDir,
			TokenFile: tok,
		}
	}
	return out
}

func (v *Vault) OrderedEmails(accounts map[string]*Account, st *State) []string {
	seen := make(map[string]bool, len(accounts))
	order := make([]string, 0, len(accounts))
	for _, e := range st.Order {
		if _, ok := accounts[e]; ok && !seen[e] {
			order = append(order, e)
			seen[e] = true
		}
	}
	var rest []string
	for e := range accounts {
		if !seen[e] {
			rest = append(rest, e)
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

func (v *Vault) ImportTokenFile(srcPath string) (string, error) {
	if err := v.EnsureDirs(); err != nil {
		return "", err
	}
	meta, err := ReadTokenMeta(srcPath)
	if err != nil || meta.Email == "" {
		return "", fmt.Errorf("%s", i18n.T("vault.import.invalid_token", srcPath))
	}
	sid := SlotIDForEmail(meta.Email)
	homeDir := filepath.Join(v.AccountsDir, sid)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(homeDir, 0o700)
	dst := filepath.Join(homeDir, ".gemini", "antigravity-cli", "antigravity-oauth-token")

	shouldWrite := true
	if oldMeta, err := ReadTokenMeta(dst); err == nil && oldMeta.Expiry > meta.Expiry {
		shouldWrite = false
	}
	if shouldWrite {
		if err := AtomicCopySecret(srcPath, dst); err != nil {
			return "", err
		}
	}

	st := v.LoadState()
	exists := false
	for _, e := range st.Order {
		if e == meta.Email {
			exists = true
			break
		}
	}
	if !exists {
		st.Order = append(st.Order, meta.Email)
		_ = v.SaveState(st)
	}
	return meta.Email, nil
}

func (v *Vault) ImportDirectory(rootDir string) ([]string, error) {
	type cand struct {
		path  string
		mtime int64
	}
	var cands []cand
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == "antigravity-oauth-token" {
			var mt int64
			if info, statErr := d.Info(); statErr == nil {
				mt = info.ModTime().UnixNano()
			}
			cands = append(cands, cand{path: path, mtime: mt})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mtime > cands[j].mtime })

	var imported []string
	seen := map[string]bool{}
	for _, c := range cands {
		email, err := v.ImportTokenFile(c.path)
		if err == nil && !seen[email] {
			seen[email] = true
			imported = append(imported, email)
		}
	}
	return imported, nil
}

func (v *Vault) RemoveAccount(email string) bool {
	accounts := v.ListAccounts()
	acct, ok := accounts[strings.ToLower(email)]
	if !ok {
		return false
	}
	_ = os.RemoveAll(acct.HomeDir)
	st := v.LoadState()
	newOrder := make([]string, 0, len(st.Order))
	for _, e := range st.Order {
		if e != acct.Email {
			newOrder = append(newOrder, e)
		}
	}
	st.Order = newOrder
	delete(st.Cooldowns, acct.Email)
	delete(st.Quotas, acct.Email)
	if st.Active == acct.Email {
		if len(newOrder) > 0 {
			st.Active = newOrder[0]
		} else {
			st.Active = ""
		}
	}
	_ = v.SaveState(st)
	return true
}

func (v *Vault) LoginNewAccount(agyBin string) (string, error) {
	if err := v.EnsureDirs(); err != nil {
		return "", err
	}
	tmpHome, err := os.MkdirTemp(v.StateDir, "agy_login_*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpHome)
	_ = os.Chmod(tmpHome, 0o700)

	cmd := exec.Command(agyBin, "acp", "--login")
	cmd.Env = CleanNativeEnv(tmpHome)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf(i18n.T("vault.login.failed"), err)
	}
	tok := filepath.Join(tmpHome, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if _, err := os.Stat(tok); err != nil {
		return "", errors.New(i18n.T("vault.login.no_token"))
	}
	return v.ImportTokenFile(tok)
}

func (v *Vault) withLock(fn func() error) error {
	if err := v.EnsureDirs(); err != nil {
		return err
	}
	f, err := os.OpenFile(v.LockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFileExclusive(f); err != nil {
		return err
	}
	defer UnlockFile(f)
	return fn()
}

func (v *Vault) ActivateToken(acct *Account) error {
	return v.withLock(func() error {
		return AtomicCopySecret(acct.TokenFile, ActiveTokenFile())
	})
}

// AcquireLaunchLock takes an exclusive cross-process file lock on launch.lock to serialize
// the brief (~50ms) window where a launching `agy` reads ActiveTokenFile() into memory,
// preventing concurrent terminals from stealing each other's OAuth token during boot.
func (v *Vault) AcquireLaunchLock() (func(), error) {
	if err := v.EnsureDirs(); err != nil {
		return func() {}, err
	}
	f, err := os.OpenFile(v.LaunchLockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, err
	}
	if err := lockFileExclusive(f); err != nil {
		_ = f.Close()
		return func() {}, err
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = UnlockFile(f)
		_ = f.Close()
	}, nil
}

// ActivateTokenAndHold acquires the short launch lock and activates acct's token file.
// Callers MUST invoke the returned release func once child `agy` logs applyAuthResult (or on error).
func (v *Vault) ActivateTokenAndHold(acct *Account) (func(), error) {
	release, err := v.AcquireLaunchLock()
	if err != nil {
		// Fall back to regular activation if lock file cannot be opened.
		return func() {}, v.ActivateToken(acct)
	}
	if err := v.ActivateToken(acct); err != nil {
		release()
		return func() {}, err
	}
	return release, nil
}

func (v *Vault) SyncBackToken(acct *Account) {
	_ = v.withLock(func() error {
		cur, err := ReadTokenMeta(ActiveTokenFile())
		if err != nil || cur.Email != acct.Email {
			return nil
		}
		src, err := ReadTokenMeta(acct.TokenFile)
		if err != nil || cur.Expiry > src.Expiry {
			return AtomicCopySecret(ActiveTokenFile(), acct.TokenFile)
		}
		return nil
	})
}

func (v *Vault) PruneLogs(keep int) {
	entries, err := os.ReadDir(v.LogsDir)
	if err != nil {
		return
	}
	type item struct {
		path  string
		mtime int64
	}
	var logs []item
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".log") || (!strings.HasPrefix(name, "agy-pro-") && !strings.HasPrefix(name, "seamless-")) {
			continue
		}
		var mt int64
		if info, err := e.Info(); err == nil {
			mt = info.ModTime().UnixNano()
		}
		logs = append(logs, item{path: filepath.Join(v.LogsDir, e.Name()), mtime: mt})
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].mtime > logs[j].mtime })
	for i := keep; i < len(logs); i++ {
		_ = os.Remove(logs[i].path)
	}
}
