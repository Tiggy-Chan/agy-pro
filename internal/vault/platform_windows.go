//go:build windows

package vault

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/term"
)

func lockFileExclusive(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

// TryLockFileExclusive attempts a non-blocking exclusive kernel lock on f.
func TryLockFileExclusive(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol,
	)
}

// UnlockFile releases a kernel lock acquired on f.
func UnlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

// IsFileLockedByOther reports whether another process currently holds an exclusive lock on path.
func IsFileLockedByOther(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
	}
	defer f.Close()
	var ol windows.Overlapped
	// Non-blocking shared lock on byte 0: fails with ERROR_LOCK_VIOLATION if held exclusively.
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
	if err != nil {
		return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
	}
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
	return false
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

// CurrentTTYName returns a compact terminal identifier on Windows ("wt", TERM_PROGRAM, or "conhost").
func CurrentTTYName() string {
	if os.Getenv("WT_SESSION") != "" {
		return "wt"
	}
	if tp := strings.TrimSpace(os.Getenv("TERM_PROGRAM")); tp != "" {
		return tp
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return "conhost"
	}
	return ""
}

// IsExecutableFile reports whether path is an existing executable file on Windows.
func IsExecutableFile(path string, info os.FileInfo) bool {
	if info == nil || info.IsDir() {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".exe", ".cmd", ".bat", ".com":
		return true
	default:
		return false
	}
}

// DetachCmd runs a background child in its own process group with no visible console window.
func DetachCmd(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}

// KillProcessTree terminates a child process on Windows.
func KillProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = proc.Kill()
}

// renameFileAtomic retries brief sharing violations on Windows when a concurrent reader opens dst.
func renameFileAtomic(src, dst string) error {
	var err error
	for i := 0; i < 6; i++ {
		err = os.Rename(src, dst)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return err
		}
		time.Sleep(time.Duration(10*(i+1)) * time.Millisecond)
	}
	return err
}

func syncOSUserAutoCompact(effectiveVal string, overwrite bool) []string {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()

	if !overwrite {
		if existing, _, err := k.GetStringValue(AutoCompactEnvKey); err == nil && strings.TrimSpace(existing) != "" {
			return nil
		}
	}
	if effectiveVal == "" {
		if err := k.DeleteValue(AutoCompactEnvKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return nil
		}
	} else {
		if err := k.SetStringValue(AutoCompactEnvKey, effectiveVal); err != nil {
			return nil
		}
	}
	broadcastEnvironmentChange()
	return []string{`HKCU\Environment`}
}

func broadcastEnvironmentChange() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	proc := user32.NewProc("SendMessageTimeoutW")
	if proc.Find() != nil {
		return
	}
	const (
		hwndBroadcast    = 0xffff
		wmSettingChange  = 0x001a
		smtoAbortIfHung  = 0x0002
	)
	envPtr, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	var res uintptr
	_, _, _ = proc.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(envPtr)),
		uintptr(smtoAbortIfHung),
		uintptr(1000),
		uintptr(unsafe.Pointer(&res)),
	)
}
