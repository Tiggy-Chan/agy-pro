//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// initConsole enables ANSI Virtual Terminal Processing on stdout & stderr so traditional
// Windows CMD (conhost.exe) and PowerShell 5.1 render colors, borders, and OSC 0 titles cleanly.
func initConsole() func() {
	type savedMode struct {
		handle windows.Handle
		mode   uint32
	}
	var saved []savedMode
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(h, &mode); err != nil {
			continue
		}
		if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
			if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err == nil {
				saved = append(saved, savedMode{handle: h, mode: mode})
			}
		}
	}
	return func() {
		for _, s := range saved {
			_ = windows.SetConsoleMode(s.handle, s.mode)
		}
	}
}
