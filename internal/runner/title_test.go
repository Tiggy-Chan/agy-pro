package runner

import (
	"bytes"
	"strings"
	"testing"
)

func TestTabTitleSyncLifecycleSpinnerAndRename(t *testing.T) {
	t.Setenv("AGY_PRO_NO_TITLE", "")
	t.Setenv("TERM", "xterm-256color")

	var buf bytes.Buffer
	s := NewTabTitleSync(&buf, true, "", "my-project")
	s.Start()

	got := buf.String()
	if !strings.Contains(got, "\x1b[22;0t") || !strings.Contains(got, "\x1b]0;✦ my-project\x07") {
		t.Fatalf("expected title push + idle fallback title, got %q", got)
	}

	// Repeated idle update with same title must emit ZERO extra bytes
	buf.Reset()
	s.Update("", "", false)
	if buf.Len() != 0 {
		t.Fatalf("expected zero redundant writes when unchanged, got %q", buf.String())
	}

	// Running state animates braille spinner frames smoothly via TickSpinner
	buf.Reset()
	s.Update("测试会话", "", true)
	if buf.String() != "\x1b]0;⠋ 测试会话\x07" {
		t.Fatalf("expected first spinner frame, got %q", buf.String())
	}
	buf.Reset()
	s.TickSpinner()
	if buf.String() != "\x1b]0;⠙ 测试会话\x07" {
		t.Fatalf("expected second spinner frame, got %q", buf.String())
	}

	// Completing turn settles back to ✦ and TickSpinner becomes a no-op
	buf.Reset()
	s.Update("测试会话", "", false)
	if buf.String() != "\x1b]0;✦ 测试会话\x07" {
		t.Fatalf("expected idle four-point star on completion, got %q", buf.String())
	}
	buf.Reset()
	s.TickSpinner()
	if buf.Len() != 0 {
		t.Fatalf("expected TickSpinner no-op when idle, got %q", buf.String())
	}

	// Failover displays ↻ indicator
	buf.Reset()
	s.SetSwitching()
	if buf.String() != "\x1b]0;↻ 测试会话\x07" {
		t.Fatalf("expected switching prefix, got %q", buf.String())
	}

	// Restore clears OSC 0 fallback and pops title stack cleanly in one atomic write
	buf.Reset()
	s.Restore()
	if buf.String() != "\x1b]0;\x07\x1b[23;0t" {
		t.Fatalf("expected OSC 0 clear + title stack pop on Restore, got %q", buf.String())
	}
}

func TestTabTitleCustomOverrideYieldsOnRenameAndSanitizes(t *testing.T) {
	t.Setenv("AGY_PRO_NO_TITLE", "")
	t.Setenv("TERM", "xterm-256color")

	var buf bytes.Buffer
	// Control chars (ESC/BEL/newline/C1 0x8d) must be stripped
	s := NewTabTitleSync(&buf, true, "测试\x1b]0;evil\x07\x8d\n标题", "fallback")
	s.Start()
	if !strings.Contains(buf.String(), "\x1b]0;✦ 测试]0;evil 标题\x07") {
		t.Fatalf("expected sanitized custom title, got %q", buf.String())
	}

	// First DB title is captured as baseline; custom --title remains pinned
	buf.Reset()
	s.Update("Old Auto Title", "", false)
	if buf.Len() != 0 {
		t.Fatalf("expected custom title to stay pinned on initial DB title, got %q", buf.String())
	}

	// When user runs `/rename 新名字` inside agy, DB title changes -> live title takes over!
	buf.Reset()
	s.Update("新名字", "", false)
	if buf.String() != "\x1b]0;✦ 新名字\x07" {
		t.Fatalf("expected /rename DB change to override launch --title, got %q", buf.String())
	}
}

func TestTabTitleRespectsNoTitleAndDumbTerm(t *testing.T) {
	var buf bytes.Buffer
	t.Setenv("AGY_PRO_NO_TITLE", "1")
	s := NewTabTitleSync(&buf, true, "test", "dir")
	s.Start()
	s.Update("title", "", true)
	s.Restore()
	if buf.Len() != 0 {
		t.Fatalf("expected zero output when AGY_PRO_NO_TITLE=1, got %q", buf.String())
	}

	t.Setenv("AGY_PRO_NO_TITLE", "")
	t.Setenv("TERM", "dumb")
	s2 := NewTabTitleSync(&buf, true, "test", "dir")
	s2.Start()
	if buf.Len() != 0 {
		t.Fatalf("expected zero output for TERM=dumb, got %q", buf.String())
	}
}
