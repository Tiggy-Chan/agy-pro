package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func (v *Vault) liveDir() string {
	if v.LiveDir != "" {
		return v.LiveDir
	}
	return filepath.Join(v.StateDir, "live")
}

// CurrentTTYName returns a compact terminal name like "pts/4" or "ttys002", or "" if not a TTY.
func CurrentTTYName() string {
	for _, p := range []string{"/proc/self/fd/0", "/dev/fd/0"} {
		target, err := os.Readlink(p)
		if err != nil || !strings.HasPrefix(target, "/dev/") {
			continue
		}
		short := strings.TrimPrefix(target, "/dev/")
		if short != "" && short != "null" {
			return short
		}
	}
	return ""
}

func (v *Vault) WriteLiveSession(ls LiveSession) {
	if ls.PID <= 0 {
		return
	}
	dir := v.liveDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(ls)
	if err != nil {
		return
	}
	dst := filepath.Join(dir, fmt.Sprintf("%d.json", ls.PID))
	_ = AtomicWriteFile(dst, raw, 0o600)
}

func (v *Vault) RemoveLiveSession(pid int) {
	if pid <= 0 {
		return
	}
	_ = os.Remove(filepath.Join(v.liveDir(), fmt.Sprintf("%d.json", pid)))
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
}

// ListLiveSessions returns active `agy-pro` managed sessions keyed by conversation ID,
// automatically pruning any stale files whose owning PID has exited.
func (v *Vault) ListLiveSessions() map[string]LiveSession {
	out := map[string]LiveSession{}
	dir := v.liveDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var ls LiveSession
		if err := json.Unmarshal(raw, &ls); err != nil || !isProcessAlive(ls.PID) {
			_ = os.Remove(full)
			continue
		}
		if ls.ConvID != "" {
			out[ls.ConvID] = ls
		}
	}
	return out
}
