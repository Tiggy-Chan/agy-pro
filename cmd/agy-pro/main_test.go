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
		"I1125 10:00:00.000000 1 common.go:496] Resuming conversation 00000000-0000-4000-8000-000000000001\n"+
			"I1125 10:00:01.000000 1 conversation_manager.go:1282] Streaming conversation 00000000-0000-4000-8000-000000000001\n"+
			"I1125 10:01:00.000000 2 server.go:1269] Created conversation 11111111-1111-4111-8111-111111111111 (backend=BACKEND_TYPE_UNSPECIFIED)\n"+
			"I1125 10:01:00.000001 2 server.go:1287] Starting conversation update stream for 11111111-1111-4111-8111-111111111111\n"+
			"I1125 10:01:20.000000 2 btw_manager.go:617] btw side question 11111111-1111-4111-8111-111111111111 ended after 20s and 2 tool calls: answered\n"+
			"I1125 10:01:25.000000 2 rpcs_backend.go:514] CancelCascadeInvocation requested: cascade_id=\"11111111-1111-4111-8111-111111111111\"\n"+
			"I1125 10:02:00.000000 3 applyAuthResult: email=alice@example.com, authMethod=consumer\n",
	), 0o600)
	_, cid, em, ql := runner.ScanLog(logPath, 0)
	if cid != "00000000-0000-4000-8000-000000000001" || em != "alice@example.com" || ql != "" {
		t.Fatalf("unexpected log scan (must ignore /btw and server.go streams): cid=%q em=%q ql=%q", cid, em, ql)
	}

	// Switching foreground session via conversation_manager.go MUST still update cid
	logPath2 := filepath.Join(tmp, "session_switch.log")
	_ = os.WriteFile(logPath2, []byte(
		"I1125 10:00:01.000000 1 conversation_manager.go:1282] Streaming conversation 00000000-0000-4000-8000-000000000001\n"+
			"I1125 10:05:00.000000 1 conversation_manager.go:1282] Streaming conversation 00000000-0000-4000-8000-000000000002\n"+
			"I1125 10:06:00.000000 2 server.go:1287] Starting conversation update stream for 99999999-9999-4999-8999-999999999999\n",
	), 0o600)
	_, cid2, _, _ := runner.ScanLog(logPath2, 0)
	if cid2 != "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("expected foreground switch to 0002 ignoring server stream 9999, got %q", cid2)
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
	if got := vault.EffectiveAutoCompact(v.LoadState()); got != "80%" {
		t.Fatalf("expected default AutoCompact=80%%, got %q", got)
	}
	if rc := runCLI([]string{"config", "auto-compact", "bad!"}, dir); rc != 2 {
		t.Fatalf("expected exit 2 on invalid auto-compact, got %d", rc)
	}
	if rc := runCLI([]string{"config", "auto-compact", "75%"}, dir); rc != 0 {
		t.Fatalf("expected exit 0 on config auto-compact 75%%, got %d", rc)
	}
	if got := vault.EffectiveAutoCompact(v.LoadState()); got != "75%" {
		t.Fatalf("expected AutoCompact=75%%, got %q", got)
	}
	if rc := runCLI([]string{"config", "auto-compact", "off"}, dir); rc != 0 {
		t.Fatalf("expected exit 0 on config auto-compact off, got %d", rc)
	}
	if got := vault.EffectiveAutoCompact(v.LoadState()); got != "" {
		t.Fatalf("expected disabled EffectiveAutoCompact=\"\", got %q", got)
	}
	// `config notify off` / `on` and invalid value rejection
	if rc := runCLI([]string{"config", "notify", "off"}, dir); rc != 0 {
		t.Fatalf("expected exit 0 on config notify off, got %d", rc)
	}
	if !v.LoadState().NotifyDisabled {
		t.Fatalf("expected NotifyDisabled=true after config notify off")
	}
	if rc := runCLI([]string{"config", "notify", "on"}, dir); rc != 0 {
		t.Fatalf("expected exit 0 on config notify on, got %d", rc)
	}
	if v.LoadState().NotifyDisabled {
		t.Fatalf("expected NotifyDisabled=false after config notify on")
	}
	if rc := runCLI([]string{"config", "notify", "maybe"}, dir); rc != 2 {
		t.Fatalf("expected exit 2 on invalid config notify value, got %d", rc)
	}
	// Default first run + `config auto-compact` without --sync-shell MUST NOT touch user dotfiles!
	cleanHome := t.TempDir()
	t.Setenv("HOME", cleanHome)
	t.Setenv("XDG_STATE_HOME", "")
	rcPath := filepath.Join(cleanHome, ".bashrc")
	origRC := "export PATH=\"$HOME/.local/bin:$PATH\"\n"
	_ = os.WriteFile(rcPath, []byte(origRC), 0o644)
	if rc := runCLI([]string{"config", "auto-compact", "75%"}, ""); rc != 0 {
		t.Fatalf("expected exit 0 on default-state config auto-compact 75%%, got %d", rc)
	}
	if afterRC, _ := os.ReadFile(rcPath); string(afterRC) != origRC {
		t.Fatalf("config auto-compact without --sync-shell MUST NOT modify .bashrc! got:\n%s", string(afterRC))
	}
	// With explicit --sync-shell it DOES sync cleanly
	if rc := runCLI([]string{"config", "auto-compact", "75%", "--sync-shell"}, ""); rc != 0 {
		t.Fatalf("expected exit 0 on config auto-compact 75%% --sync-shell, got %d", rc)
	}
	if afterRC, _ := os.ReadFile(rcPath); !strings.Contains(string(afterRC), `AGY_CLI_AUTO_COMPACT_OVERRIDE="75%"`) {
		t.Fatalf("expected --sync-shell to update .bashrc, got:\n%s", string(afterRC))
	}
	// Invalid CLI launch flags must fail fast with exit code 2 (not silently ignored)
	if rc := runCLI([]string{"--auto-compact", "NaN%"}, dir); rc != 2 {
		t.Fatalf("expected exit 2 on --auto-compact NaN%%, got %d", rc)
	}
	if rc := runCLI([]string{"--auto-compact", "InfK"}, dir); rc != 2 {
		t.Fatalf("expected exit 2 on --auto-compact InfK, got %d", rc)
	}
	if rc := runCLI([]string{"--reserve", "99"}, dir); rc != 2 {
		t.Fatalf("expected exit 2 on --reserve 99, got %d", rc)
	}
}

func TestNextAndUsePassthrough(t *testing.T) {
	// 1. Bare `next` switches only (no launch)
	if _, _, launch := parseNextArgs(nil); launch {
		t.Fatal("expected bare next not to launch")
	}

	// 2. `next --dangerously-skip-permissions` -> forceNext + launch with flag verbatim
	opts, pass, launch := parseNextArgs([]string{"--dangerously-skip-permissions"})
	if !launch || !opts.forceNext || opts.resumeLast || len(pass) != 1 || pass[0] != "--dangerously-skip-permissions" {
		t.Fatalf("unexpected next --dangerously-skip-permissions: opts=%+v pass=%v launch=%v", opts, pass, launch)
	}

	// 3. `next -c --dangerously-skip-permissions` -> native -c preserved verbatim
	opts, pass, launch = parseNextArgs([]string{"-c", "--dangerously-skip-permissions"})
	if !launch || !opts.forceNext || opts.resumeLast || strings.Join(pass, " ") != "-c --dangerously-skip-permissions" {
		t.Fatalf("unexpected next -c: opts=%+v pass=%v launch=%v", opts, pass, launch)
	}

	// 4. `next --conversation=cid-99 --dangerously-skip-permissions` -> preserved verbatim
	opts, pass, launch = parseNextArgs([]string{"--conversation=cid-99", "--dangerously-skip-permissions"})
	if !launch || !opts.forceNext || opts.resumeLast || strings.Join(pass, " ") != "--conversation=cid-99 --dangerously-skip-permissions" {
		t.Fatalf("unexpected next --conversation: opts=%+v pass=%v launch=%v", opts, pass, launch)
	}

	// 5. `next --resume cid-42 --dangerously-skip-permissions` -> explicit ID preserved verbatim
	opts, pass, launch = parseNextArgs([]string{"--resume", "cid-42", "--dangerously-skip-permissions"})
	if !launch || !opts.forceNext || opts.resumeLast || strings.Join(pass, " ") != "--resume cid-42 --dangerously-skip-permissions" {
		t.Fatalf("unexpected next --resume cid-42: opts=%+v pass=%v launch=%v", opts, pass, launch)
	}

	// 6. Bare `next --resume` (no ID) -> resumeLast=true shorthand
	opts, pass, launch = parseNextArgs([]string{"--resume", "--dangerously-skip-permissions"})
	if !launch || !opts.forceNext || !opts.resumeLast || strings.Join(pass, " ") != "--dangerously-skip-permissions" {
		t.Fatalf("unexpected bare next --resume: opts=%+v pass=%v launch=%v", opts, pass, launch)
	}

	// 6b. `--title` flag parsed cleanly without leaking into native agy args
	tOpts, tPass := parseRunFlags([]string{"--title", "测试任务", "--dangerously-skip-permissions"})
	if tOpts.title != "测试任务" || strings.Join(tPass, " ") != "--dangerously-skip-permissions" {
		t.Fatalf("unexpected --title parse: opts=%+v pass=%v", tOpts, tPass)
	}
	tOpts2, tPass2 := parseRunFlags([]string{"--title=API重构", "-c"})
	if tOpts2.title != "API重构" || strings.Join(tPass2, " ") != "-c" {
		t.Fatalf("unexpected --title= parse: opts=%+v pass=%v", tOpts2, tPass2)
	}

	// 7. End-to-end mock agy execution: verify `agy-pro next --dangerously-skip-permissions`
	// advances active account alice -> bob and forwards `--dangerously-skip-permissions` verbatim!
	tmp := t.TempDir()
	fakeHome := filepath.Join(tmp, "home")
	_ = os.MkdirAll(fakeHome, 0o700)
	t.Setenv("HOME", fakeHome)
	poolDir := filepath.Join(tmp, "pool")
	v := vault.New(poolDir)
	for _, em := range []string{"alice@example.com", "bob@example.com"} {
		tf := filepath.Join(tmp, em+".tok")
		raw, _ := json.Marshal(map[string]any{
			"id_token_email": em,
			"token":          map[string]any{"access_token": "a", "refresh_token": "r", "expiry": "2030-01-01T00:00:00Z"},
		})
		_ = os.WriteFile(tf, raw, 0o600)
		if _, err := v.ImportTokenFile(tf); err != nil {
			t.Fatalf("import %s failed: %v", em, err)
		}
	}
	_ = v.UpdateState(func(s *vault.State) { s.Active = "alice@example.com" })

	argsOut := filepath.Join(tmp, "captured_args.txt")
	mockAgy := filepath.Join(tmp, "mock-agy")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsOut + "\"\n"
	_ = os.WriteFile(mockAgy, []byte(script), 0o755)
	t.Setenv("AGY_PRO_BIN", mockAgy)

	if rc := runCLI([]string{"next", "--dangerously-skip-permissions", "-c"}, poolDir); rc != 0 {
		t.Fatalf("expected exit 0 from next --dangerously-skip-permissions -c, got %d", rc)
	}
	if got := v.LoadState().Active; got != "bob@example.com" {
		t.Fatalf("expected active account switched to bob@example.com, got %q", got)
	}
	captured, err := os.ReadFile(argsOut)
	if err != nil {
		t.Fatalf("mock agy was not invoked: %v", err)
	}
	gotArgs := string(captured)
	if !strings.Contains(gotArgs, "--dangerously-skip-permissions") || !strings.Contains(gotArgs, "\n-c\n") {
		t.Fatalf("expected native flags forwarded verbatim, got:\n%s", gotArgs)
	}

	// Verify `agy-pro use alice --conversation=cid-7` switches back to alice and forwards flag
	if rc := runCLI([]string{"use", "alice", "--conversation=cid-7"}, poolDir); rc != 0 {
		t.Fatalf("expected exit 0 from use alice --conversation=cid-7, got %d", rc)
	}
	if got := v.LoadState().Active; got != "alice@example.com" {
		t.Fatalf("expected active account switched to alice@example.com, got %q", got)
	}
	captured2, _ := os.ReadFile(argsOut)
	if !strings.Contains(string(captured2), "--conversation=cid-7") {
		t.Fatalf("expected --conversation=cid-7 forwarded, got:\n%s", string(captured2))
	}
}

func TestFirstRunAutoConfigAndShellSync(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")

	// Symlinked .zshrc must preserve its symlink target (never replaced with regular file)
	realZshrc := filepath.Join(fakeHome, "dotfiles_zshrc")
	_ = os.WriteFile(realZshrc, []byte("export PATH=\"$HOME/.local/bin:$PATH\"\n"), 0o644)
	zshrc := filepath.Join(fakeHome, ".zshrc")
	if err := os.Symlink(realZshrc, zshrc); err != nil {
		t.Fatalf("symlink failed: %v", err)
	}
	bashrc := filepath.Join(fakeHome, ".bashrc")
	_ = os.WriteFile(bashrc, []byte("# existing user setting\nexport AGY_CLI_AUTO_COMPACT_OVERRIDE=\"80%\"\n"), 0o644)

	v := vault.New(filepath.Join(fakeHome, ".local", "state", "agy-pro"))
	configured, _, err := v.EnsureFirstRunConfig(fakeHome, false)
	if err != nil || !configured {
		t.Fatalf("expected first run to configure, configured=%v err=%v", configured, err)
	}
	st := v.LoadState()
	if !st.FirstRunConfigured || st.AutoCompact != "80%" {
		t.Fatalf("unexpected first-run state: %+v", st)
	}
	// Must NOT create unsolicited .zshenv or .profile when .zshrc/.bashrc already exist
	if _, err := os.Stat(filepath.Join(fakeHome, ".zshenv")); !os.IsNotExist(err) {
		t.Fatalf("must not create unsolicited .zshenv when .zshrc exists")
	}
	if info, err := os.Lstat(zshrc); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected .zshrc to remain a symlink after SyncShellAutoCompact")
	}

	zshData, _ := os.ReadFile(zshrc)
	if !strings.Contains(string(zshData), `export AGY_CLI_AUTO_COMPACT_OVERRIDE="80%"`) {
		t.Fatalf("expected .zshrc to receive default export, got:\n%s", string(zshData))
	}
	bashData, _ := os.ReadFile(bashrc)
	if strings.Count(string(bashData), "AGY_CLI_AUTO_COMPACT_OVERRIDE") != 1 {
		t.Fatalf("expected .bashrc not to duplicate existing export, got:\n%s", string(bashData))
	}

	// Second run must be a fast no-op
	configured2, touched2, err2 := v.EnsureFirstRunConfig(fakeHome, false)
	if err2 != nil || configured2 || len(touched2) != 0 {
		t.Fatalf("expected second run no-op, got configured=%v touched=%v err=%v", configured2, touched2, err2)
	}

	// Explicit update modifies shell exports and persists to state.json
	if _, _, err := v.SetAutoCompact("75%", fakeHome, false); err != nil {
		t.Fatalf("SetAutoCompact 75%% failed: %v", err)
	}
	zshUpdated, _ := os.ReadFile(zshrc)
	if !strings.Contains(string(zshUpdated), `export AGY_CLI_AUTO_COMPACT_OVERRIDE="75%"`) {
		t.Fatalf("expected .zshrc updated to 75%%, got:\n%s", string(zshUpdated))
	}

	// Disabling removes export cleanly
	if _, _, err := v.SetAutoCompact("off", fakeHome, false); err != nil {
		t.Fatalf("SetAutoCompact off failed: %v", err)
	}
	zshOff, _ := os.ReadFile(zshrc)
	if strings.Contains(string(zshOff), "AGY_CLI_AUTO_COMPACT_OVERRIDE") {
		t.Fatalf("expected .zshrc export removed on off, got:\n%s", string(zshOff))
	}
}

func TestAutoCompactEnvResolutionAndStepping(t *testing.T) {
	if got := vault.StepAutoCompact("80%", +5); got != "85%" {
		t.Fatalf("expected 85%%, got %s", got)
	}
	if got := vault.StepAutoCompact("95%", +5); got != "off" {
		t.Fatalf("expected off after 95%%+5, got %s", got)
	}
	if got := vault.StepAutoCompact("off", -5); got != "95%" {
		t.Fatalf("expected 95%% from off-5, got %s", got)
	}
	if got := vault.StepAutoCompact("30%", -5); got != "30%" {
		t.Fatalf("expected clamp at 30%%, got %s", got)
	}

	t.Setenv(vault.AutoCompactEnvKey, "")
	if origSSH, hadSSH := os.LookupEnv("SSH_CONNECTION"); hadSSH {
		t.Cleanup(func() { _ = os.Setenv("SSH_CONNECTION", origSSH) })
		_ = os.Unsetenv("SSH_CONNECTION")
	}
	if origOSC8, hadOSC8 := os.LookupEnv(vault.ForceOSC8EnvKey); hadOSC8 {
		t.Cleanup(func() { _ = os.Setenv(vault.ForceOSC8EnvKey, origOSC8) })
		_ = os.Unsetenv(vault.ForceOSC8EnvKey)
	}
	envDefault := vault.CleanNativeEnv("")
	found := ""
	foundForceFile := ""
	foundOSC8 := ""
	for _, kv := range envDefault {
		if strings.HasPrefix(kv, vault.AutoCompactEnvKey+"=") {
			found = strings.TrimPrefix(kv, vault.AutoCompactEnvKey+"=")
		}
		if strings.HasPrefix(kv, vault.ForceFileTokenEnvKey+"=") {
			foundForceFile = strings.TrimPrefix(kv, vault.ForceFileTokenEnvKey+"=")
		}
		if strings.HasPrefix(kv, vault.ForceOSC8EnvKey+"=") {
			foundOSC8 = strings.TrimPrefix(kv, vault.ForceOSC8EnvKey+"=")
		}
		if strings.HasPrefix(kv, "SSH_CONNECTION=") {
			t.Fatalf("CleanNativeEnv must NOT spoof SSH_CONNECTION when unset, found %q", kv)
		}
	}
	if found != "80%" {
		t.Fatalf("expected CleanNativeEnv default 80%%, got %q", found)
	}
	if foundForceFile != "1" {
		t.Fatalf("expected CleanNativeEnv to set %s=1, got %q", vault.ForceFileTokenEnvKey, foundForceFile)
	}
	if foundOSC8 != "0" {
		t.Fatalf("expected CleanNativeEnv default %s=0 to prevent CJK OSC-8 C1 ghosting, got %q", vault.ForceOSC8EnvKey, foundOSC8)
	}

	// Explicit caller override AGY_CLI_FORCE_OSC8=1 must be respected
	t.Setenv(vault.ForceOSC8EnvKey, "1")
	osc8Override := ""
	for _, kv := range vault.CleanNativeEnv("") {
		if strings.HasPrefix(kv, vault.ForceOSC8EnvKey+"=") {
			osc8Override = strings.TrimPrefix(kv, vault.ForceOSC8EnvKey+"=")
		}
	}
	if osc8Override != "1" {
		t.Fatalf("expected explicit %s=1 preserved, got %q", vault.ForceOSC8EnvKey, osc8Override)
	}

	t.Setenv(vault.AutoCompactEnvKey, "75%")
	val, forced := vault.ResolveAutoCompact(&vault.State{AutoCompact: "80%"}, "")
	if !forced || val != "80%" {
		t.Fatalf("expected state config 80%% to win over stale shell env 75%%, got val=%q forced=%v", val, forced)
	}
	envOff := vault.CleanNativeEnvWithCompact("", "", true)
	for _, kv := range envOff {
		if strings.HasPrefix(kv, vault.AutoCompactEnvKey+"=") {
			t.Fatalf("expected %s stripped when off, found %s", vault.AutoCompactEnvKey, kv)
		}
	}
}

func TestStreamlinedCLICommands(t *testing.T) {
	tmp := t.TempDir()
	fakeHome := filepath.Join(tmp, "home")
	_ = os.MkdirAll(fakeHome, 0o700)
	t.Setenv("HOME", fakeHome)
	poolDir := filepath.Join(tmp, "pool")

	// Pruned legacy top-level commands must be rejected with exit code 2
	for _, removed := range []string{"dash", "tui", "order", "clear-cooldown", "run", "login", "import", "remove"} {
		if rc := runCLI([]string{removed}, poolDir); rc != 2 {
			t.Fatalf("expected removed command %q to exit 2, got %d", removed, rc)
		}
	}

	// `agy-pro auth import` & `agy-pro auth rm`
	tokPath := filepath.Join(tmp, "alice.tok")
	tokJSON, _ := json.Marshal(map[string]any{
		"id_token_email": "alice@example.com",
		"token":          map[string]any{"access_token": "a", "refresh_token": "r", "expiry": "2030-01-01T00:00:00Z"},
	})
	_ = os.WriteFile(tokPath, tokJSON, 0o600)

	if rc := runCLI([]string{"auth", "--help"}, poolDir); rc != 0 {
		t.Fatalf("expected auth --help exit 0, got %d", rc)
	}
	if rc := runCLI([]string{"resume", "--help"}, poolDir); rc != 0 {
		t.Fatalf("expected resume --help exit 0, got %d", rc)
	}
	if rc := runCLI([]string{"auth", "import", tokPath}, poolDir); rc != 0 {
		t.Fatalf("expected auth import exit 0, got %d", rc)
	}

	v := vault.New(poolDir)
	now := float64(time.Now().Unix())
	_ = v.UpdateState(func(s *vault.State) {
		s.Active = "alice@example.com"
		s.Cooldowns["alice@example.com"] = &vault.Cooldown{Until: now + 3600, Reason: "5h quota exhausted", At: now}
		s.Quotas["alice@example.com"] = &vault.Quota{H5Used: fptr(100), H5Rem: fptr(0), At: now}
	})

	// `agy-pro status --reset` clears cooldowns and exhausted cache without blocking
	if rc := runCLI([]string{"status", "--reset", "--json"}, poolDir); rc != 0 {
		t.Fatalf("expected status --reset --json exit 0, got %d", rc)
	}
	st := v.LoadState()
	if len(st.Cooldowns) != 0 || st.Quotas["alice@example.com"] != nil {
		t.Fatalf("expected status --reset to clear cooldown and exhausted quota, got cd=%+v q=%+v", st.Cooldowns, st.Quotas)
	}

	// `agy-pro -- <args...>` POSIX passthrough forwards args verbatim to mock agy
	argsOut := filepath.Join(tmp, "dashdash_args.txt")
	mockAgy := filepath.Join(tmp, "mock-agy")
	_ = os.WriteFile(mockAgy, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+argsOut+"\"\n"), 0o755)
	t.Setenv("AGY_PRO_BIN", mockAgy)

	if rc := runCLI([]string{"--", "--dangerously-skip-permissions", "-p", "hello"}, poolDir); rc != 0 {
		t.Fatalf("expected agy-pro -- passthrough exit 0, got %d", rc)
	}
	captured, err := os.ReadFile(argsOut)
	if err != nil || !strings.Contains(string(captured), "--dangerously-skip-permissions") || !strings.Contains(string(captured), "\nhello\n") {
		t.Fatalf("expected verbatim args after --, got err=%v out=%q", err, string(captured))
	}

	// `agy-pro auth rm alice` removes account cleanly
	if rc := runCLI([]string{"auth", "rm", "alice"}, poolDir); rc != 0 {
		t.Fatalf("expected auth rm exit 0, got %d", rc)
	}
	if len(v.ListAccounts()) != 0 {
		t.Fatalf("expected account removed via auth rm")
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
