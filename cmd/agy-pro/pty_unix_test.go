//go:build !windows

package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/Tiggy-Chan/agy-pro/internal/runner"
)

func TestTerminalGuardRestoresTermiosAndResetSeq(t *testing.T) {
	ptmx, ttyFile, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open failed: %v", err)
	}
	defer ptmx.Close()
	defer ttyFile.Close()

	sfd := int(ttyFile.Fd())
	before, err := getTermios(sfd)
	if err != nil {
		t.Fatalf("getTermios before failed: %v", err)
	}

	g := runner.NewTerminalGuard(sfd, ttyFile)
	if err := g.EnterRaw(); err != nil {
		t.Fatalf("EnterRaw failed: %v", err)
	}
	inside, _ := getTermios(sfd)
	if *inside == *before {
		t.Fatal("expected termios to change in raw mode")
	}
	g.Restore()

	after, err := getTermios(sfd)
	if err != nil {
		t.Fatalf("getTermios after failed: %v", err)
	}
	if *after != *before {
		t.Fatalf("termios was not restored bit-for-bit! before=%+v after=%+v", *before, *after)
	}

	buf := make([]byte, 4096)
	_ = ptmx.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	n, _ := ptmx.Read(buf)
	emitted := buf[:n]
	for _, seq := range [][]byte{[]byte("\x1b[<u"), []byte("\x1b[>4;0m"), []byte("\x1b[?2004l")} {
		if !bytes.Contains(emitted, seq) {
			t.Fatalf("missing reset sequence %q in emitted %q", seq, emitted)
		}
	}
}
