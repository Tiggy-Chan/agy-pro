//go:build windows

package i18n

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// osLocaleFallback reads the active Windows user locale (e.g. "zh-CN", "en-US")
// via kernel32!GetUserDefaultLocaleName since CMD/PowerShell rarely set LANG/LC_ALL.
func osLocaleFallback() string {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("GetUserDefaultLocaleName")
	if proc.Find() != nil {
		return ""
	}
	var buf [85]uint16 // LOCALE_NAME_MAX_LENGTH = 85
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}
