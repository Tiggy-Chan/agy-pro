package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/Tiggy-Chan/agy-pro/internal/quota"
	"github.com/Tiggy-Chan/agy-pro/internal/router"
	"github.com/Tiggy-Chan/agy-pro/internal/runner"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func fptr(v float64) *float64 { return &v }

func TestUIAlignment80Col(t *testing.T) {
	headers := []string{"", "账号", "5h已用", "7d已用", "状态"}
	rows := [][]string{
		{"●", "alice@example.com", "26%", "87%", "主号"},
		{"○", "bob@example.com", "0%", "0%", "冷却中 (1h20m)"},
		{"○", "very-long-account-email-address@example.com", "100%", "100%", "5h 已满 (11-25 15:12)"},
	}
	aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignLeft}
	out := ui.RenderTable(headers, rows, aligns, 80, 1)
	for _, line := range strings.Split(out, "\n") {
		if w := ui.DisplayWidth(line); w > 80 {
			t.Fatalf("line exceeded 80 cols (%d): %q", w, line)
		}
	}
}

func TestDrainRouterNeverBouncesBack(t *testing.T) {
	now := float64(time.Now().Unix())
	order := []string{"a@g.com", "b@g.com", "c@g.com"}
	st := &vault.State{
		Active:    "a@g.com",
		Order:     order,
		Cooldowns: map[string]*vault.Cooldown{},
		Quotas:    map[string]*vault.Quota{},
	}
	quotas := map[string]*vault.Quota{
		"a@g.com": {H5Rem: fptr(0), D7Rem: fptr(80), H5Reset: now + 3600},
		"b@g.com": {H5Rem: fptr(100), D7Rem: fptr(100)},
		"c@g.com": {H5Rem: fptr(100), D7Rem: fptr(100)},
	}

	picked, why := router.PickDrain(order, st, quotas, false, now)
	if picked != "b@g.com" || why != "drain-advanced" {
		t.Fatalf("expected b@g.com/drain-advanced, got %s/%s", picked, why)
	}

	// Even if a@g.com resets to 100%, drain must stay on b@g.com
	quotas["a@g.com"].H5Rem = fptr(100)
	picked2, why2 := router.PickDrain(order, st, quotas, false, now)
	if picked2 != "b@g.com" || why2 != "drain-current" {
		t.Fatalf("expected b@g.com/drain-current, got %s/%s", picked2, why2)
	}
}

func TestEffectiveExpiredWindowAndCooldown(t *testing.T) {
	now := float64(time.Now().Unix())
	st := &vault.State{
		Active:    "a@g.com",
		Order:     []string{"a@g.com", "b@g.com"},
		Cooldowns: map[string]*vault.Cooldown{},
		Quotas: map[string]*vault.Quota{
			"a@g.com": {H5Used: fptr(100), H5Rem: fptr(0), H5Reset: now - 10},
		},
	}
	eff := quota.Effective(st, now)
	if *eff["a@g.com"].H5Rem != 100 {
		t.Fatalf("expected expired 5h window reset to 100, got %v", *eff["a@g.com"].H5Rem)
	}
	router.MarkCooldown(st, "a@g.com", 600, "5h quota", now)
	picked, _ := router.PickDrain(st.Order, st, eff, false, now)
	if picked != "b@g.com" {
		t.Fatalf("expected b@g.com after cooldown, got %s", picked)
	}
}

func TestParsersAndVault(t *testing.T) {
	raw := "\x1b[33m⚠ Individual quota reached.\x1b[0m Resets in 1h18m40s."
	clean := runner.StripANSI(raw)
	if !runner.PrintModeQuotaRe.MatchString(clean) {
		t.Fatal("print-mode quota marker not matched")
	}
	if got := runner.ParseResetsIn(clean); got != 3600+18*60+40 {
		t.Fatalf("unexpected resets duration: %d", got)
	}
	// Historical screen text MUST NOT match BackendLogQuotaRe
	if runner.BackendLogQuotaRe.MatchString("⚠ Your AI credits balance is too low to continue.") {
		t.Fatal("historical/screen text must NOT trigger BackendLogQuotaRe!")
	}
	// Genuine Go backend 429 error MUST match BackendLogQuotaRe
	realBackendLine := `E1124 19:13:26.306214 175784 errorreport.go:282] agent executor error: generating and executing: transport error (ResourceExhausted, HTTP 429): Resource has been exhausted (e.g. check quota).`
	if !runner.BackendLogQuotaRe.MatchString(realBackendLine) {
		t.Fatal("real backend HTTP 429 ResourceExhausted line was not matched!")
	}
	// Live quota verification:
	// 1. Healthy account (both 5h & 7d > 0.5%) -> transient 429 MUST NOT failover
	if ok, _ := runner.ShouldFailoverAfterProbe(&vault.Quota{H5Rem: fptr(74), D7Rem: fptr(13)}, realBackendLine); ok {
		t.Fatal("healthy account must NOT failover on transient 429!")
	}
	// 2. 7-day exhausted while 5-hour still has 71% -> MUST failover as "7d"!
	if ok, kind := runner.ShouldFailoverAfterProbe(&vault.Quota{H5Rem: fptr(71), D7Rem: fptr(0)}, realBackendLine); !ok || kind != "7d" {
		t.Fatalf("7d=0%% with 5h=71%% MUST failover as 7d, got ok=%v kind=%q", ok, kind)
	}
	// 3. 5-hour exhausted while 7-day still has 50% -> MUST failover as "5h"!
	if ok, kind := runner.ShouldFailoverAfterProbe(&vault.Quota{H5Rem: fptr(0), D7Rem: fptr(50)}, realBackendLine); !ok || kind != "5h" {
		t.Fatalf("5h=0%% with 7d=50%% MUST failover as 5h, got ok=%v kind=%q", ok, kind)
	}
	// 4. Probe timed out on fatal turn-ending agent executor 429 -> MUST fallback failover as "suspected"!
	if ok, kind := runner.ShouldFailoverAfterProbe(&vault.Quota{Error: "超时 (35s)"}, realBackendLine); !ok || kind != "suspected" {
		t.Fatalf("fatal 429 with probe timeout MUST fallback failover as suspected, got ok=%v kind=%q", ok, kind)
	}
	stripped := runner.StripLockedArgs([]string{"--model", "flash", "-r", "abc"}, false)
	if len(stripped) != 2 || stripped[0] != "-r" || stripped[1] != "abc" {
		t.Fatalf("unexpected stripped args: %v", stripped)
	}

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "session.log")
	_ = os.WriteFile(logPath, []byte(
		"I1125 Resuming conversation 00000000-0000-4000-8000-000000000001\n"+
			"I1125 applyAuthResult: email=alice@example.com, authMethod=consumer\n",
	), 0o600)
	_, cid, em, ql := runner.ScanLog(logPath, 0)
	if cid != "00000000-0000-4000-8000-000000000001" || em != "alice@example.com" || ql != "" {
		t.Fatalf("unexpected log scan: cid=%q em=%q ql=%q", cid, em, ql)
	}

	tokPath := filepath.Join(tmp, "antigravity-oauth-token")
	tokJSON, _ := json.Marshal(map[string]any{
		"id_token_email": "alice@gmail.com",
		"token": map[string]any{
			"access_token":  "a",
			"refresh_token": "r",
			"expiry":        "2030-01-01T00:00:00Z",
		},
	})
	_ = os.WriteFile(tokPath, tokJSON, 0o600)

	v := vault.New(filepath.Join(tmp, "pool"))
	email, err := v.ImportTokenFile(tokPath)
	if err != nil || email != "alice@gmail.com" {
		t.Fatalf("import failed: email=%q err=%v", email, err)
	}
	if len(v.ListAccounts()) != 1 {
		t.Fatalf("expected 1 account")
	}
	if !v.RemoveAccount("alice@gmail.com") || len(v.ListAccounts()) != 0 {
		t.Fatalf("remove failed")
	}
}

func TestUnknownSubcommandRejected(t *testing.T) {
	if rc := runCLI([]string{"stauts"}, t.TempDir()); rc != 2 {
		t.Fatalf("expected exit 2 on typo subcommand, got %d", rc)
	}
}

func TestTerminalGuardRestoresTermiosAndResetSeq(t *testing.T) {
	ptmx, ttyFile, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open failed: %v", err)
	}
	defer ptmx.Close()
	defer ttyFile.Close()

	sfd := int(ttyFile.Fd())
	before, err := unix.IoctlGetTermios(sfd, unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS before failed: %v", err)
	}

	g := runner.NewTerminalGuard(sfd, ttyFile)
	if err := g.EnterRaw(); err != nil {
		t.Fatalf("EnterRaw failed: %v", err)
	}
	inside, _ := unix.IoctlGetTermios(sfd, unix.TCGETS)
	if *inside == *before {
		t.Fatal("expected termios to change in raw mode")
	}
	g.Restore()

	after, err := unix.IoctlGetTermios(sfd, unix.TCGETS)
	if err != nil {
		t.Fatalf("TCGETS after failed: %v", err)
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
