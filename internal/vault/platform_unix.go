//go:build !windows

package vault

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func lockFileExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// TryLockFileExclusive attempts a non-blocking exclusive kernel lock on f.
func TryLockFileExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

// UnlockFile releases a kernel lock acquired on f.
func UnlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// IsFileLockedByOther reports whether another process currently holds an exclusive kernel lock on path.
func IsFileLockedByOther(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if err != nil {
		return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || err == unix.EPERM
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

// IsExecutableFile reports whether path is an existing executable regular file.
func IsExecutableFile(_ string, info os.FileInfo) bool {
	return info != nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// DetachCmd detaches a child command into its own session so it never interferes with /dev/tty.
func DetachCmd(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// KillProcessTree terminates a detached child process group immediately.
func KillProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
}

func renameFileAtomic(src, dst string) error {
	return os.Rename(src, dst)
}

func syncOSUserAutoCompact(_ string, _ bool) []string {
	return nil
}
