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

func TestUIAlignment80ColDualPool(t *testing.T) {
	headers := []string{"", "账号", "Gemini(5h/7d)", "Claude(5h/7d)", "状态"}
	rows := [][]string{
		{"●", "alice@example.com", "26%/87%", "0%/0%", "主号"},
		{"○", "bob@example.com", "0%/0%", "0%/60%", "冷却中 (1h20m)"},
		{"○", "very-long-account-email-address@example.com", "-/100%", "0%/0%", "7d 已满 (12-01 10:03)"},
	}
	aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignLeft}
	out := ui.RenderTable(headers, rows, aligns, 80, 1)
	for _, line := range strings.Split(out, "\n") {
		if w := ui.DisplayWidth(line); w > 80 {
			t.Fatalf("line exceeded 80 cols (%d): %q", w, line)
		}
	}
}

func TestDualPoolQuotaParsing(t *testing.T) {
	now := float64(time.Now().Unix())
	tabOut := "Gemini Models\tWeekly Limit Remaining\t16%\t2026-12-01T02:03:36Z\n" +
		"Gemini Models\tFive Hour Limit Remaining\t83%\t2026-11-25T15:12:16Z\n" +
		"Claude and GPT models\tWeekly Limit Remaining\t40%\t2026-11-29T05:38:17Z\n" +
		"Claude and GPT models\tFive Hour Limit Remaining\t100%\t2026-11-25T16:00:00Z\n"
	q := quota.ParseUsageOutput(tabOut, now)
	if q.Error != "" {
		t.Fatalf("unexpected parse error: %s", q.Error)
	}
	if *q.D7Used != 84 || *q.H5Used != 17 || *q.C7Used != 60 || *q.C5Used != 0 {
		t.Fatalf("unexpected parsed values: G(%v/%v) C(%v/%v)", *q.H5Used, *q.D7Used, *q.C5Used, *q.C7Used)
	}

	spaceOut := "Quota:\n" +
		"Gemini Models          Weekly Limit Remaining     0%    2026-12-01 10:03 CST\n" +
		"Claude and GPT models  Five Hour Limit Remaining  100%  2026-11-25 21:42 CST\n"
	q2 := quota.ParseUsageOutput(spaceOut, now)
	if q2.Error != "" || *q2.D7Rem != 0 || *q2.C5Rem != 100 {
		t.Fatalf("space-aligned parse failed: %+v", q2)
	}
}

func TestDrainRouterAndReserveSwitching(t *testing.T) {
	now := float64(time.Now().Unix())
	order := []string{"a@example.com", "b@example.com", "c@example.com"}
	st := &vault.State{
		Active:    "a@example.com",
		Order:     order,
		Cooldowns: map[string]*vault.Cooldown{},
		Quotas:    map[string]*vault.Quota{},
	}
	quotas := map[string]*vault.Quota{
		"a@example.com": {H5Rem: fptr(0), D7Rem: fptr(80), H5Reset: now + 3600},
		"b@example.com": {H5Rem: fptr(100), D7Rem: fptr(100)},
		"c@example.com": {H5Rem: fptr(100), D7Rem: fptr(100)},
	}

	picked, why := router.PickDrain(order, st, quotas, false, now)
	if picked != "b@example.com" || why != "drain-advanced" {
		t.Fatalf("expected b/drain-advanced, got %s/%s", picked, why)
	}
	// Never bounce back even after a resets
	quotas["a@example.com"].H5Rem = fptr(100)
	picked2, why2 := router.PickDrain(order, st, quotas, false, now)
	if picked2 != "b@example.com" || why2 != "drain-current" {
		t.Fatalf("expected b/drain-current, got %s/%s", picked2, why2)
	}

	// Reserve threshold: b drops to 8% remaining <= 10% reserve -> proactively advance to c!
	quotas["b@example.com"].H5Rem = fptr(8)
	picked3, why3 := router.PickDrainWithReserve(order, st, quotas, false, 10, now)
	if picked3 != "c@example.com" || why3 != "reserve-advanced" {
		t.Fatalf("expected proactive switch to c/reserve-advanced, got %s/%s", picked3, why3)
	}

	// If ALL accounts are <= reserve (but > 0.5%), still pick available account instead of failing
	quotas["a@example.com"].H5Rem = fptr(5)
	quotas["c@example.com"].H5Rem = fptr(6)
	picked4, why4 := router.PickDrainWithReserve(order, st, quotas, false, 10, now)
	if picked4 != "c@example.com" || why4 != "drain-current" {
		t.Fatalf("expected graceful fallback to c/drain-current, got %s/%s", picked4, why4)
	}
}

func TestAsyncVerifySelfHealAndExactCooldown(t *testing.T) {
	now := float64(time.Now().Unix())
	st := &vault.State{
		Active: "b@example.com",
		Cooldowns: map[string]*vault.Cooldown{
			"a@example.com": {Until: now + 1800, Reason: "429 fast-failover verifying", At: now},
		},
		Quotas: map[string]*vault.Quota{},
	}

	// Case 1: Background probe finds account is actually healthy -> self-heal removes provisional cooldown!
	res1 := applyVerifiedQuota(st, "a@example.com", &vault.Quota{H5Rem: fptr(65), D7Rem: fptr(40), C5Rem: fptr(100), C7Rem: fptr(100)}, now)
	if res1 != "cleared-transient" || st.Cooldowns["a@example.com"] != nil {
		t.Fatalf("expected transient cooldown cleared, got res=%s cd=%+v", res1, st.Cooldowns["a@example.com"])
	}

	// Case 2: Background probe confirms 7d=0% -> upgrades cooldown to exact official 7d reset!
	resetTarget := now + 5*86400
	res2 := applyVerifiedQuota(st, "a@example.com", &vault.Quota{H5Rem: fptr(71), D7Rem: fptr(0), D7Reset: resetTarget}, now)
	if res2 != "confirmed-7d" || st.Cooldowns["a@example.com"] == nil || st.Cooldowns["a@example.com"].Until < resetTarget-5 {
		t.Fatalf("expected exact 7d cooldown, got res=%s cd=%+v", res2, st.Cooldowns["a@example.com"])
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
	if runner.BackendLogQuotaRe.MatchString("⚠ Your AI credits balance is too low to continue.") {
		t.Fatal("historical/screen text must NOT trigger BackendLogQuotaRe!")
	}
	realBackendLine := `E1124 19:13:26.306214 175784 errorreport.go:282] agent executor error: generating and executing: transport error (ResourceExhausted, HTTP 429): Resource has been exhausted (e.g. check quota).`
	if !runner.FatalTurnQuotaRe.MatchString(realBackendLine) {
		t.Fatal("real fatal backend HTTP 429 ResourceExhausted line was not matched!")
	}
	if ok, _ := runner.ShouldFailoverAfterProbe(&vault.Quota{H5Rem: fptr(74), D7Rem: fptr(13)}, realBackendLine); ok {
		t.Fatal("healthy account must NOT failover on transient 429!")
	}
	if ok, kind := runner.ShouldFailoverAfterProbe(&vault.Quota{H5Rem: fptr(71), D7Rem: fptr(0)}, realBackendLine); !ok || kind != "7d" {
		t.Fatalf("7d=0%% with 5h=71%% MUST failover as 7d, got ok=%v kind=%q", ok, kind)
	}
	if ok, kind := runner.ShouldFailoverAfterProbe(&vault.Quota{H5Rem: fptr(80), D7Rem: fptr(80), C5Rem: fptr(0), C7Rem: fptr(50)}, realBackendLine); !ok || kind != "claude-5h" {
		t.Fatalf("claude-5h=0%% MUST failover as claude-5h, got ok=%v kind=%q", ok, kind)
	}

	stripped := runner.StripLogFileArg([]string{"--model", "flash", "--log-file", "/tmp/old.log", "-r", "abc"})
	if len(stripped) != 4 || stripped[0] != "--model" || stripped[1] != "flash" || stripped[2] != "-r" || stripped[3] != "abc" {
		t.Fatalf("unexpected stripped args: %v", stripped)
	}
	built := runner.BuildAgyArgs([]string{"--dangerously-skip-permissions"}, "/tmp/a.log", "cid-1", "cont")
	for _, a := range built {
		if a == "--model" || strings.HasPrefix(a, "--model=") || a == "--settings" || strings.HasPrefix(a, "--settings=") {
			t.Fatalf("BuildAgyArgs must not inject --model/--settings overrides: %v", built)
		}
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
		"id_token_email": "alice@example.com",
		"token": map[string]any{
			"access_token":  "a",
			"refresh_token": "r",
			"expiry":        "2030-01-01T00:00:00Z",
		},
	})
	_ = os.WriteFile(tokPath, tokJSON, 0o600)

	v := vault.New(filepath.Join(tmp, "pool"))
	email, err := v.ImportTokenFile(tokPath)
	if err != nil || email != "alice@example.com" {
		t.Fatalf("import failed: email=%q err=%v", email, err)
	}
	if len(v.ListAccounts()) != 1 {
		t.Fatalf("expected 1 account")
	}
	if !v.RemoveAccount("alice@example.com") || len(v.ListAccounts()) != 0 {
		t.Fatalf("remove failed")
	}
}

func TestUnknownSubcommandAndConfig(t *testing.T) {
	dir := t.TempDir()
	if rc := runCLI([]string{"stauts"}, dir); rc != 2 {
		t.Fatalf("expected exit 2 on typo subcommand, got %d", rc)
	}
	if rc := runCLI([]string{"config", "reserve", "10"}, dir); rc != 0 {
		t.Fatalf("expected exit 0 on config reserve 10, got %d", rc)
	}
	v := vault.New(dir)
	if got := v.LoadState().ReservePct; got != 10 {
		t.Fatalf("expected saved ReservePct=10, got %v", got)
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
