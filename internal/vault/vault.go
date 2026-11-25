package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	Version      = "0.1.0-dev"
	DefaultModel = "gemini-4-neon-xhigh"
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
	At      float64  `json:"at"`
	Error   string   `json:"error"`
}

type State struct {
	Active    string               `json:"active"`
	Order     []string             `json:"order"`
	Cooldowns map[string]*Cooldown `json:"cooldowns"`
	Quotas    map[string]*Quota    `json:"quotas"`
	LastConv  map[string]string    `json:"last_conv"`
}

type TokenMeta struct {
	Email      string
	Expiry     float64
	HasRefresh bool
	AuthMethod string
}

type Vault struct {
	StateDir    string
	AccountsDir string
	LogsDir     string
	StateFile   string
	LockFile    string
}

func HomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
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
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "agy-pool")
	}
	return filepath.Join(HomeDir(), ".local", "state", "agy-pool")
}

func New(stateDir string) *Vault {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return &Vault{
		StateDir:    stateDir,
		AccountsDir: filepath.Join(stateDir, "accounts"),
		LogsDir:     filepath.Join(stateDir, "logs"),
		StateFile:   filepath.Join(stateDir, "state.json"),
		LockFile:    filepath.Join(stateDir, "switch.lock"),
	}
}

func FindAgyBinary() string {
	if ov := os.Getenv("AGY_POOL_BIN"); ov != "" {
		if info, err := os.Stat(ov); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return ov
		}
	}
	candidates := []string{
		filepath.Join(HomeDir(), ".local", "bin", "agy"),
		"/usr/local/bin/agy",
		"/usr/bin/agy",
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return c
		}
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p
	}
	return filepath.Join(HomeDir(), ".local", "bin", "agy")
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
		return "无缓存"
	}
	diff := int(now - epoch)
	if diff < 0 {
		diff = 0
	}
	switch {
	case diff < 60:
		return fmt.Sprintf("%ds前", diff)
	case diff < 3600:
		return fmt.Sprintf("%dm前", diff/60)
	case diff < 86400:
		return fmt.Sprintf("%dh前", diff/3600)
	default:
		return fmt.Sprintf("%dd前", diff/86400)
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
		Email:      email,
		Expiry:     ParseISOTime(raw.Token.Expiry),
		HasRefresh: hasRefresh,
		AuthMethod: method,
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
	if err := os.Rename(tmpName, dst); err != nil {
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
	stripSet := make(map[string]struct{}, len(StripEnvKeys)+2)
	for _, k := range StripEnvKeys {
		stripSet[k] = struct{}{}
	}
	stripSet["SSH_CONNECTION"] = struct{}{}
	if overrideHome != "" {
		stripSet["HOME"] = struct{}{}
	}

	orig := os.Environ()
	out := make([]string, 0, len(orig)+2)
	for _, kv := range orig {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, drop := stripSet[k]; drop {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "SSH_CONNECTION=127.0.0.1 0 127.0.0.1 0")
	if overrideHome != "" {
		out = append(out, "HOME="+overrideHome)
	}
	return out
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
		Order:     []string{},
		Cooldowns: map[string]*Cooldown{},
		Quotas:    map[string]*Quota{},
		LastConv:  map[string]string{},
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
		return "", fmt.Errorf("无效的 agy token 文件: %s", srcPath)
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
		return "", fmt.Errorf("agy acp --login 失败: %w", err)
	}
	tok := filepath.Join(tmpHome, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if _, err := os.Stat(tok); err != nil {
		return "", errors.New("登录未生成 antigravity-oauth-token 文件")
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
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func (v *Vault) ActivateToken(acct *Account) error {
	return v.withLock(func() error {
		return AtomicCopySecret(acct.TokenFile, ActiveTokenFile())
	})
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
		if e.IsDir() || !strings.HasPrefix(e.Name(), "agy-pool-") || !strings.HasSuffix(e.Name(), ".log") {
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
