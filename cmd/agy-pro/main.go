package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/quota"
	"github.com/Tiggy-Chan/agy-pro/internal/router"
	"github.com/Tiggy-Chan/agy-pro/internal/runner"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

var validSubcommands = []string{
	"clear-cooldown",
	"config",
	"doctor",
	"help",
	"import",
	"login",
	"next",
	"order",
	"remove",
	"resume",
	"run",
	"status",
	"use",
}

func helpText() string {
	return fmt.Sprintf(`agy-pro %s (Go) — 官方 agy 零入侵原生多账号增强工具（drain 切号 + 1.5s 异步秒切续跑）

用法:
  agy-pro                        秒开原生 agy（默认按 drain 主号直连 Google，完全尊重 agy 原生模型与配置）
  agy-pro [agy原生参数...]        直接兼容并透传 agy 参数（如 agy-pro --dangerously-skip-permissions -r <ID>）
  agy-pro run [选项] [-- agy参数] 显式启动并透传参数给原生 agy
  agy-pro status [-r] [--active] 秒显号池看板（含 Gemini + Claude/GPT 双池；加 -r 刷新全部，--active 仅刷主号）
  agy-pro login                  调用官方 agy acp --login 登录新账号并存入独立号池
  agy-pro import <文件或目录>     导入已有 antigravity-oauth-token 到独立号池
  agy-pro remove <关键词|邮箱>    从号池移除指定账号
  agy-pro use <关键词|邮箱>       手动指定当前 drain 主号（如: agy-pro use 003）
  agy-pro next [--resume]        切到下一个可用账号（带 --resume 直接续聊当前目录上次会话）
  agy-pro resume [会话ID]        用当前主号续聊当前目录最近一次（或指定）会话
  agy-pro order <号1> <号2> ...  自定义 drain 轮换顺序（支持邮箱关键字）
  agy-pro config [reserve <N>]   查看或设置低余量提前切号水位线（如 reserve 10 表示剩余 <=10%% 时启动前提前换号）
  agy-pro clear-cooldown         清空本地冷却与耗尽缓存
  agy-pro doctor                 环境与号池体检

启动选项（可用于 agy-pro 或 agy-pro run）:
  --account <关键词>              临时指定账号
  --reserve <0-50>               本次覆盖低余量提前切号水位线（百分比）
  --no-auto-switch               关闭撞 5h/7d 墙自动重启切号
  --refresh                      启动前先联网刷新当前账号额度
`, vault.Version)
}

func matchEmail(order []string, needle string) (string, string) {
	q := strings.ToLower(strings.TrimSpace(needle))
	for _, e := range order {
		if e == q {
			return e, ""
		}
	}
	var hits []string
	for _, e := range order {
		if strings.Contains(e, q) {
			hits = append(hits, e)
		}
	}
	if len(hits) == 0 {
		avail := "(空)"
		if len(order) > 0 {
			avail = strings.Join(order, ", ")
		}
		return "", fmt.Sprintf("未匹配到账号 '%s'。当前号池: %s", needle, avail)
	}
	if len(hits) > 1 {
		return "", fmt.Sprintf("匹配到多个账号: %s，请写更精确的关键词。", strings.Join(hits, ", "))
	}
	return hits[0], ""
}

func fmtPct(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

func fmtPair(h5, d7 *float64) string {
	if h5 == nil && d7 == nil {
		return "-"
	}
	return fmtPct(h5) + "/" + fmtPct(d7)
}

func mergeQuotaResult(prev, fresh *vault.Quota) *vault.Quota {
	if fresh == nil {
		return prev
	}
	// Preserve previous valid numbers if this refresh attempt timed out/errored
	if fresh.Error != "" && fresh.H5Used == nil && fresh.D7Used == nil && fresh.C5Used == nil && fresh.C7Used == nil {
		if prev != nil && (prev.H5Used != nil || prev.D7Used != nil || prev.C5Used != nil || prev.C7Used != nil) {
			cp := *prev
			cp.Error = fresh.Error
			return &cp
		}
	}
	return fresh
}

func spawnDetachedSelf(stateDir string, args ...string) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if stateDir != "" {
		cmd.Env = append(os.Environ(), "AGY_PRO_STATE_DIR="+stateDir)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Start()
}

// applyVerifiedQuota updates state.json with a freshly probed quota for an account
// that triggered a fast failover: locks exact Google reset time if exhausted, OR
// automatically clears the provisional cooldown if all pools still have > 0.5% remaining.
func applyVerifiedQuota(st *vault.State, email string, q *vault.Quota, now float64) string {
	if q == nil {
		return "error"
	}
	st.Quotas[email] = mergeQuotaResult(st.Quotas[email], q)
	if q.Error != "" {
		return "probe-error"
	}

	kind := runner.ExhaustionKind(q)
	if kind == "" {
		// Transient 429 self-heal: clear provisional cooldown so healthy account returns to pool
		if cd := st.Cooldowns[email]; cd != nil && strings.Contains(cd.Reason, "verifying") {
			delete(st.Cooldowns, email)
		}
		return "cleared-transient"
	}

	resetSec := 3600
	reason := kind + " quota exhausted"
	switch kind {
	case "7d":
		if q.D7Reset > now {
			resetSec = int(q.D7Reset - now)
		}
	case "5h":
		if q.H5Reset > now {
			resetSec = int(q.H5Reset - now)
		}
	case "claude-7d":
		if q.C7Reset > now {
			resetSec = int(q.C7Reset - now)
		}
	case "claude-5h":
		if q.C5Reset > now {
			resetSec = int(q.C5Reset - now)
		}
	}
	router.MarkCooldown(st, email, resetSec, reason, now)
	return "confirmed-" + kind
}

// cmdInternalVerify runs detached in the background after a fast failover to replace the
// provisional 30m cooldown with Google's exact 5h/7d reset timestamp (or unfreeze transient 429).
func cmdInternalVerify(v *vault.Vault, args []string) int {
	if len(args) < 1 {
		return 1
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	accounts := v.ListAccounts()
	acct, ok := accounts[email]
	if !ok {
		return 1
	}
	agyBin := vault.FindAgyBinary()
	q := quota.ProbeAccount(acct, agyBin, 35*time.Second)
	now := float64(time.Now().UnixNano()) / 1e9
	_ = v.UpdateState(func(st *vault.State) {
		applyVerifiedQuota(st, email, q, now)
	})
	return 0
}

// cmdInternalWarm runs detached in the background to silently refresh a standby account's
// expired 1h access_token (0 quota cost) so failover has zero Sign-in flash.
func cmdInternalWarm(v *vault.Vault, args []string) int {
	if len(args) < 1 {
		return 1
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	accounts := v.ListAccounts()
	acct, ok := accounts[email]
	if !ok {
		return 1
	}
	meta, err := vault.ReadTokenMeta(acct.TokenFile)
	if err == nil && meta.Expiry > float64(time.Now().Unix())+300 {
		return 0 // already fresh
	}
	agyBin := vault.FindAgyBinary()
	q := quota.ProbeAccount(acct, agyBin, 30*time.Second)
	if q != nil && q.Error == "" {
		_ = v.UpdateState(func(st *vault.State) {
			st.Quotas[email] = mergeQuotaResult(st.Quotas[email], q)
		})
	}
	return 0
}

func cmdStatus(v *vault.Vault, args []string) int {
	var doRefresh, onlyActive, noAutoInit, asJSON bool
	for _, a := range args {
		switch a {
		case "-r", "--refresh":
			doRefresh = true
		case "--active":
			onlyActive = true
		case "--no-auto-init":
			noAutoInit = true
		case "--json":
			asJSON = true
		case "-h", "--help":
			fmt.Println("用法: agy-pro status [-r|--refresh] [--active] [--no-auto-init] [--json]")
			return 0
		default:
			fmt.Fprintf(os.Stderr, "未知选项: %s\n", a)
			return 2
		}
	}

	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	if len(order) == 0 {
		fmt.Println("号池为空。请运行 `agy-pro login` 登录账号，或 `agy-pro import <目录>` 导入已有 token。")
		return 1
	}

	now := float64(time.Now().UnixNano()) / 1e9
	quotas := quota.Effective(st, now)

	allMissing := true
	for _, e := range order {
		if _, ok := quotas[e]; ok {
			allMissing = false
			break
		}
	}

	needRefresh := doRefresh || (!noAutoInit && allMissing)
	if needRefresh {
		agyBin := vault.FindAgyBinary()
		var targets []*vault.Account
		if onlyActive {
			targetEmail := st.Active
			if _, ok := accounts[targetEmail]; !ok {
				targetEmail = order[0]
			}
			targets = []*vault.Account{accounts[targetEmail]}
		} else {
			for _, e := range order {
				targets = append(targets, accounts[e])
			}
		}

		ui.StderrProgress(fmt.Sprintf("[0/%d] 正在通过原生 agy 查询额度...", len(targets)), false)
		fresh := quota.RefreshAll(targets, agyBin, 3, 35*time.Second, func(done, total int, email string, q *vault.Quota) {
			tag := "OK"
			if q.Error != "" {
				tag = q.Error
			}
			ui.StderrProgress(fmt.Sprintf("[%d/%d] %s: %s", done, total, email, tag), false)
		})
		ui.StderrProgress("", true)

		_ = v.UpdateState(func(s *vault.State) {
			for e, q := range fresh {
				s.Quotas[e] = mergeQuotaResult(s.Quotas[e], q)
			}
		})
		st = v.LoadState()
		now = float64(time.Now().UnixNano()) / 1e9
		quotas = quota.Effective(st, now)
	}

	router.PruneExpiredCooldowns(st, now)
	active := st.Active
	if _, ok := accounts[active]; !ok {
		active = order[0]
	}

	if asJSON {
		type jsonRow struct {
			Email     string          `json:"email"`
			Active    bool            `json:"active"`
			Available bool            `json:"available"`
			Status    string          `json:"status"`
			Quota     *vault.Quota    `json:"quota,omitempty"`
			Cooldown  *vault.Cooldown `json:"cooldown,omitempty"`
		}
		out := make([]jsonRow, 0, len(order))
		for _, e := range order {
			ok, why := router.AccountAvailability(e, st, quotas, now)
			out = append(out, jsonRow{
				Email:     e,
				Active:    e == active,
				Available: ok,
				Status:    why,
				Quota:     quotas[e],
				Cooldown:  st.Cooldowns[e],
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}

	cols := ui.TerminalColumns(80)
	compact := cols < 102

	rows := make([][]string, 0, len(order))
	oldestAt := 0.0
	for _, e := range order {
		mark := "○"
		if e == active {
			mark = "●"
		}
		q := quotas[e]
		if q != nil && q.At > 0 && (oldestAt == 0 || q.At < oldestAt) {
			oldestAt = q.At
		}

		ok, why := router.AccountAvailability(e, st, quotas, now)
		stTxt := why
		if q != nil && q.Error != "" && q.H5Used == nil && q.D7Used == nil {
			stTxt = "失败:" + q.Error
		} else if ok {
			if st.ReservePct > 0 && !router.AboveReserve(q, st.ReservePct) {
				if e == active {
					stTxt = "主号(低余量)"
				} else {
					stTxt = "低余量"
				}
			} else if e == active {
				stTxt = "主号"
			} else {
				stTxt = "就绪"
			}
		}

		if compact {
			var gStr, cStr string
			if q != nil {
				gStr = fmtPair(q.H5Used, q.D7Used)
				cStr = fmtPair(q.C5Used, q.C7Used)
			} else {
				gStr, cStr = "-", "-"
			}
			rows = append(rows, []string{mark, e, gStr, cStr, stTxt})
		} else {
			h5, d7, claude := "-", "-", "-"
			if q != nil {
				if q.H5Used != nil {
					h5 = fmt.Sprintf("%.0f%% (%s)", *q.H5Used, vault.FmtLocalTime(q.H5Reset))
				}
				if q.D7Used != nil {
					d7 = fmt.Sprintf("%.0f%% (%s)", *q.D7Used, vault.FmtLocalTime(q.D7Reset))
				}
				claude = fmtPair(q.C5Used, q.C7Used)
			}
			rows = append(rows, []string{mark, e, h5, d7, claude, stTxt})
		}
	}

	ageTxt := "无缓存"
	if oldestAt > 0 {
		ageTxt = vault.FmtAge(oldestAt, now)
	}
	resTxt := ""
	if st.ReservePct > 0 {
		resTxt = fmt.Sprintf(" | 水位线:%.0f%%", st.ReservePct)
	}
	fmt.Printf("agy-pro | 原生直连 | drain%s | 缓存:%s\n", resTxt, ageTxt)
	if compact {
		headers := []string{"", "账号", "Gemini(5h/7d)", "Claude(5h/7d)", "状态"}
		aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignLeft}
		fmt.Println(ui.RenderTable(headers, rows, aligns, cols, 1))
	} else {
		headers := []string{"", "账号", "Gemini 5h", "Gemini 7d", "Claude(5h/7d)", "状态"}
		aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignLeft}
		fmt.Println(ui.RenderTable(headers, rows, aligns, cols, 1))
	}

	cwd, _ := os.Getwd()
	lastConv := st.LastConv[cwd]
	var hints []string
	if !needRefresh {
		hints = append(hints, "刷新实时额度: agy-pro status -r")
	}
	if lastConv != "" {
		shortID := lastConv
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		hints = append(hints, fmt.Sprintf("续聊上次会话: agy-pro resume (%s)", shortID))
	}
	if len(hints) > 0 {
		fmt.Println(ui.Truncate(strings.Join(hints, " | "), cols))
	}
	return 0
}

func cmdConfig(v *vault.Vault, args []string) int {
	st := v.LoadState()
	if len(args) == 0 {
		fmt.Printf("reserve = %.0f%%  （启动新会话前若当前主号剩余 <= %.0f%%，自动提前切至满血备号；0 表示用尽再切）\n",
			st.ReservePct, st.ReservePct)
		return 0
	}
	if args[0] == "reserve" && len(args) == 2 {
		val, err := strconv.ParseFloat(strings.TrimSuffix(args[1], "%"), 64)
		if err != nil || val < 0 || val > 50 {
			fmt.Fprintln(os.Stderr, "错误: reserve 需为 0 到 50 之间的数字（例如: agy-pro config reserve 10）")
			return 2
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.ReservePct = val
		})
		if val == 0 {
			fmt.Println("✓ 已关闭低余量提前切号（reserve = 0%，单号用到耗尽再切）")
		} else {
			fmt.Printf("✓ 已设置低余量提前切号水位线: %.0f%%（新会话启动前剩余 <= %.0f%% 时优先切满血号）\n", val, val)
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "用法: agy-pro config [reserve <0-50>]")
	return 2
}

func cmdLogin(v *vault.Vault) int {
	agyBin := vault.FindAgyBinary()
	fmt.Println("正在调用官方 `agy acp --login` 登录新 Google 账号（隔离临时目录，不影响系统钥匙串）...")
	email, err := v.LoginNewAccount(agyBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 登录失败: %v\n", err)
		return 1
	}
	fmt.Printf("✓ 已登录并存入独立号池: %s\n", email)
	return 0
}

func cmdImport(v *vault.Vault, args []string) int {
	if len(args) != 1 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "用法: agy-pro import <token文件或目录>")
		return 2
	}
	p := args[0]
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(vault.HomeDir(), p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 路径无效: %v\n", err)
		return 1
	}
	info, err := os.Stat(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ 路径不存在: %s\n", abs)
		return 1
	}
	if !info.IsDir() {
		email, err := v.ImportTokenFile(abs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ 导入失败: %v\n", err)
			return 1
		}
		fmt.Printf("✓ 已导入账号: %s\n", email)
		return 0
	}
	emails, err := v.ImportDirectory(abs)
	if err != nil || len(emails) == 0 {
		fmt.Fprintf(os.Stderr, "未在 %s 下找到任何有效的 antigravity-oauth-token 文件。\n", abs)
		return 1
	}
	fmt.Printf("✓ 已导入 %d 个账号到独立号池 (%s):\n", len(emails), v.AccountsDir)
	for _, e := range emails {
		fmt.Printf("  - %s\n", e)
	}
	return 0
}

func cmdRemove(v *vault.Vault, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "用法: agy-pro remove <关键词|邮箱>")
		return 2
	}
	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	email, errMsg := matchEmail(order, args[0])
	if email == "" {
		fmt.Fprintln(os.Stderr, errMsg)
		return 1
	}
	v.RemoveAccount(email)
	fmt.Printf("✓ 已从号池移除: %s\n", email)
	return 0
}

func cmdUse(v *vault.Vault, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "用法: agy-pro use <关键词|邮箱>")
		return 2
	}
	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	email, errMsg := matchEmail(order, args[0])
	if email == "" {
		fmt.Fprintln(os.Stderr, errMsg)
		return 1
	}
	_ = v.UpdateState(func(s *vault.State) {
		s.Active = email
		s.Order = order
		delete(s.Cooldowns, email)
	})
	fmt.Printf("✓ 当前主号已切换为: %s\n", email)
	return 0
}

func cmdOrder(v *vault.Vault, args []string) int {
	accounts := v.ListAccounts()
	st := v.LoadState()
	cur := v.OrderedEmails(accounts, st)
	if len(args) == 0 {
		for i, e := range cur {
			fmt.Printf("  %d. %s\n", i+1, e)
		}
		return 0
	}
	var newOrder []string
	seen := map[string]bool{}
	for _, t := range args {
		email, errMsg := matchEmail(cur, t)
		if email == "" {
			fmt.Fprintln(os.Stderr, errMsg)
			return 1
		}
		if !seen[email] {
			newOrder = append(newOrder, email)
			seen[email] = true
		}
	}
	for _, e := range cur {
		if !seen[e] {
			newOrder = append(newOrder, e)
		}
	}
	_ = v.UpdateState(func(s *vault.State) {
		s.Order = newOrder
	})
	fmt.Println("✓ 已更新 drain 顺序:")
	for i, e := range newOrder {
		fmt.Printf("  %d. %s\n", i+1, e)
	}
	return 0
}

func cmdDoctor(v *vault.Vault) int {
	agyBin := vault.FindAgyBinary()
	fmt.Printf("=== agy-pro %s (Go) 体检（纯原生零入侵模式）===\n", vault.Version)
	okAll := true

	if info, err := os.Stat(agyBin); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		c := exec.Command(agyBin, "--version")
		c.Stdin = nil
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, _ := c.Output()
		fmt.Printf("✓ 原生 agy: %s (v%s)\n", agyBin, strings.TrimSpace(string(out)))
	} else {
		fmt.Printf("✗ 未找到原生 agy: %s\n", agyBin)
		okAll = false
	}

	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	fmt.Printf("✓ 独立号池 (%s): %d 个账号\n", v.AccountsDir, len(order))
	if len(order) == 0 {
		okAll = false
	}
	for _, e := range order {
		meta, err := vault.ReadTokenMeta(accounts[e].TokenFile)
		good := err == nil && meta.HasRefresh
		if !good {
			okAll = false
		}
		mark := "○"
		if e == st.Active {
			mark = "●"
		}
		status := "refresh_token 正常"
		if !good {
			status = "token 异常"
		}
		fmt.Printf("  %s %-26s %s\n", mark, e, status)
	}

	env := vault.CleanNativeEnv("")
	var leaked []string
	var homeVal string
	for _, kv := range env {
		k, val, _ := strings.Cut(kv, "=")
		if k == "HOME" {
			homeVal = val
		}
		for _, bad := range vault.StripEnvKeys {
			if k == bad {
				leaked = append(leaked, k)
			}
		}
	}
	if len(leaked) == 0 {
		fmt.Println("✓ 网关环境变量隔离: 干净（零中转直连 Google）")
	} else {
		fmt.Printf("✗ 网关环境变量泄漏: %v\n", leaked)
		okAll = false
	}
	fmt.Printf("✓ 真实 HOME 保留: %s\n", homeVal)
	fmt.Println("✓ 模型配置: 遵循 agy 原生设置与 /model 切换（零强制覆盖）")
	fmt.Printf("✓ 低余量提前切号水位线: %.0f%%\n", st.ReservePct)
	if okAll {
		return 0
	}
	return 1
}

type runOptions struct {
	explicitAccount string
	autoSwitch      bool
	refreshBefore   bool
	forceNext       bool
	resumeLast      bool
	hasReserve      bool
	reservePct      float64
}

func parseRunFlags(argv []string) (runOptions, []string) {
	opts := runOptions{autoSwitch: true}
	var pass []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			pass = append(pass, argv[i+1:]...)
			break
		}
		switch {
		case a == "--account" && i+1 < len(argv):
			opts.explicitAccount = argv[i+1]
			i++
		case strings.HasPrefix(a, "--account="):
			opts.explicitAccount = strings.TrimPrefix(a, "--account=")
		case a == "--reserve" && i+1 < len(argv):
			if val, err := strconv.ParseFloat(strings.TrimSuffix(argv[i+1], "%"), 64); err == nil && val >= 0 && val <= 50 {
				opts.hasReserve = true
				opts.reservePct = val
			}
			i++
		case strings.HasPrefix(a, "--reserve="):
			raw := strings.TrimSuffix(strings.TrimPrefix(a, "--reserve="), "%")
			if val, err := strconv.ParseFloat(raw, 64); err == nil && val >= 0 && val <= 50 {
				opts.hasReserve = true
				opts.reservePct = val
			}
		case a == "--no-auto-switch":
			opts.autoSwitch = false
		case a == "--refresh":
			opts.refreshBefore = true
		default:
			pass = append(pass, a)
		}
	}
	return opts, pass
}

func isPrintModeArgs(args []string) bool {
	for _, a := range args {
		if a == "-p" || a == "--print" || a == "--prompt" || strings.HasPrefix(a, "--print=") {
			return true
		}
	}
	return false
}

func cmdRun(v *vault.Vault, agyArgs []string, opts runOptions) int {
	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	if len(order) == 0 {
		fmt.Fprintln(os.Stderr, "错误: 独立号池为空。请先运行 `agy-pro login` 或 `agy-pro import <目录>`。")
		return 1
	}

	reservePct := st.ReservePct
	if opts.hasReserve {
		reservePct = opts.reservePct
	}

	agyBin := vault.FindAgyBinary()
	now := float64(time.Now().UnixNano()) / 1e9
	quotas := quota.Effective(st, now)

	var currentEmail string
	if opts.explicitAccount != "" {
		email, errMsg := matchEmail(order, opts.explicitAccount)
		if email == "" {
			fmt.Fprintln(os.Stderr, errMsg)
			return 1
		}
		currentEmail = email
		_ = v.UpdateState(func(s *vault.State) {
			s.Active = currentEmail
			s.Order = order
		})
	} else {
		oldActive := st.Active
		picked, reason := router.PickDrainWithReserve(order, st, quotas, opts.forceNext, reservePct, now)
		if picked == "" {
			fmt.Fprintf(os.Stderr, "[agy-pro] %s\n", reason)
			return 2
		}
		currentEmail = picked
		if reason == "reserve-advanced" && oldActive != "" && oldActive != currentEmail {
			fmt.Fprintf(os.Stderr, "\033[33m[agy-pro]\033[0m %s 剩余额度低于水位线 %.0f%%，启动前提前切至满血账号 -> \033[1m%s\033[0m\n",
				oldActive, reservePct, currentEmail)
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.Active = currentEmail
			s.Order = order
		})
	}

	if opts.refreshBefore {
		ui.StderrProgress(fmt.Sprintf("[agy-pro] 正在刷新 %s 额度...", currentEmail), false)
		q := quota.ProbeAccount(accounts[currentEmail], agyBin, 35*time.Second)
		ui.StderrProgress("", true)
		_ = v.UpdateState(func(s *vault.State) {
			s.Quotas[currentEmail] = mergeQuotaResult(s.Quotas[currentEmail], q)
		})
		st = v.LoadState()
		now = float64(time.Now().UnixNano()) / 1e9
		quotas = quota.Effective(st, now)
		if opts.explicitAccount == "" {
			picked, reason := router.PickDrainWithReserve(order, st, quotas, false, reservePct, now)
			if picked == "" {
				fmt.Fprintf(os.Stderr, "[agy-pro] %s\n", reason)
				return 2
			}
			currentEmail = picked
			_ = v.UpdateState(func(s *vault.State) {
				s.Active = currentEmail
				s.Order = order
			})
		}
	}

	printMode := isPrintModeArgs(agyArgs)
	cleanArgs := runner.StripLogFileArg(agyArgs)
	convID := runner.ExtractCLIConversationID(cleanArgs)

	cwd, _ := os.Getwd()
	if opts.resumeLast && convID == "" {
		if saved := st.LastConv[cwd]; saved != "" {
			convID = saved
			cleanArgs = runner.StripResumeAndPromptArgs(cleanArgs)
		} else {
			fmt.Fprintln(os.Stderr, "[agy-pro] 当前目录无最近会话记录，将开启新会话。")
		}
	}

	promptInteractive := ""
	maxAttempts := len(order)
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	v.PruneLogs(20)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		acct := accounts[currentEmail]
		// Pre-warm current account ONLY if expired right now; standby is warmed asynchronously below.
		if !printMode {
			if meta, err := vault.ReadTokenMeta(acct.TokenFile); err == nil && meta.Expiry > 0 && meta.Expiry <= float64(time.Now().Unix())+60 {
				ui.StderrProgress(fmt.Sprintf("[agy-pro] 静默刷新 %s 登录态...", currentEmail), false)
				qWarm := quota.ProbeAccount(acct, agyBin, 20*time.Second)
				ui.StderrProgress("", true)
				if qWarm != nil && qWarm.Error == "" {
					_ = v.UpdateState(func(s *vault.State) {
						s.Quotas[currentEmail] = mergeQuotaResult(s.Quotas[currentEmail], qWarm)
					})
					st = v.LoadState()
					quotas = quota.Effective(st, float64(time.Now().UnixNano())/1e9)
				}
			}
		}

		if err := v.ActivateToken(acct); err != nil {
			fmt.Fprintf(os.Stderr, "[agy-pro] 切换 token 失败: %v\n", err)
			return 1
		}

		// Detached background pre-warm for the NEXT standby account so failover has 0ms auth latency
		if !printMode {
			if standby := router.NextStandby(order, currentEmail, st, quotas, float64(time.Now().UnixNano())/1e9); standby != "" {
				if sm, err := vault.ReadTokenMeta(accounts[standby].TokenFile); err == nil && sm.Expiry > 0 && sm.Expiry <= float64(time.Now().Unix())+300 {
					spawnDetachedSelf(v.StateDir, "_warm", standby)
				}
			}
		}

		if !printMode {
			h5Txt, d7Txt := "5h=?", "7d=?"
			if q := quotas[currentEmail]; q != nil {
				if q.H5Used != nil {
					h5Txt = fmt.Sprintf("5h=%.0f%%", *q.H5Used)
				}
				if q.D7Used != nil {
					d7Txt = fmt.Sprintf("7d=%.0f%%", *q.D7Used)
				}
			}
			fmt.Fprintf(os.Stderr, "\033[36m[agy-pro]\033[0m 原生直连: \033[1m%s\033[0m (%s, %s)\n",
				currentEmail, h5Txt, d7Txt)
		}

		logPath := filepath.Join(v.LogsDir, fmt.Sprintf("agy-pro-%d-%d-%d.log", time.Now().Unix(), os.Getpid(), attempt))
		var fullArgs []string
		if attempt == 0 && !opts.resumeLast {
			fullArgs = runner.BuildAgyArgs(cleanArgs, logPath, "", "")
		} else {
			relaunchBase := runner.StripResumeAndPromptArgs(cleanArgs)
			fullArgs = runner.BuildAgyArgs(relaunchBase, logPath, convID, promptInteractive)
		}

		var res *runner.RunResult
		if printMode {
			res = runner.RunPrintOnce(agyBin, fullArgs, logPath, acct, opts.autoSwitch)
		} else {
			res = runner.RunInteractiveOnce(agyBin, fullArgs, logPath, convID, acct, opts.autoSwitch)
		}
		v.SyncBackToken(acct)

		if res.ConvID != "" {
			convID = res.ConvID
			savedConv := convID
			_ = v.UpdateState(func(s *vault.State) {
				s.LastConv[cwd] = savedConv
			})
		}

		if !res.QuotaHit {
			if printMode {
				if res.Stdout != "" {
					_, _ = os.Stdout.WriteString(res.Stdout)
				}
				if res.Stderr != "" {
					_, _ = os.Stderr.WriteString(res.Stderr)
				}
			}
			return res.ReturnCode
		}

		// Fast path vs verified path cooldown update
		now = float64(time.Now().UnixNano()) / 1e9
		resetSec := 1800 // 30m provisional cooldown for fast failover / probe timeout
		kindLabel := "额度耗尽"
		cdReason := "429 verifying..."
		spawnAsyncVerify := false

		if res.ExhaustionKind == "fast" {
			kindLabel = "触发 429 额度墙（秒切中，后台异步核验精准重置时间）"
			cdReason = "429 fast-failover verifying"
			spawnAsyncVerify = true
		} else if res.VerifiedQuota != nil {
			switch res.ExhaustionKind {
			case "7d":
				kindLabel = "Gemini 7天额度耗尽"
				cdReason = "7d quota exhausted"
				if res.VerifiedQuota.D7Reset > now {
					resetSec = int(res.VerifiedQuota.D7Reset - now)
				} else {
					resetSec = 3600
				}
			case "5h":
				kindLabel = "Gemini 5小时额度耗尽"
				cdReason = "5h quota exhausted"
				if res.VerifiedQuota.H5Reset > now {
					resetSec = int(res.VerifiedQuota.H5Reset - now)
				} else {
					resetSec = 3600
				}
			case "claude-7d":
				kindLabel = "Claude/GPT 7天额度耗尽"
				cdReason = "claude-7d quota exhausted"
				if res.VerifiedQuota.C7Reset > now {
					resetSec = int(res.VerifiedQuota.C7Reset - now)
				} else {
					resetSec = 3600
				}
			case "claude-5h":
				kindLabel = "Claude/GPT 5小时额度耗尽"
				cdReason = "claude-5h quota exhausted"
				if res.VerifiedQuota.C5Reset > now {
					resetSec = int(res.VerifiedQuota.C5Reset - now)
				} else {
					resetSec = 3600
				}
			}
		} else {
			kindLabel = "疑似额度耗尽（临时冷却）"
			cdReason = "429 suspected quota"
		}

		oldEmail := currentEmail
		_ = v.UpdateState(func(s *vault.State) {
			if res.VerifiedQuota != nil {
				s.Quotas[oldEmail] = mergeQuotaResult(s.Quotas[oldEmail], res.VerifiedQuota)
			}
			router.MarkCooldown(s, oldEmail, resetSec, cdReason, now)
		})
		if spawnAsyncVerify {
			spawnDetachedSelf(v.StateDir, "_verify", oldEmail)
		}

		st = v.LoadState()
		quotas = quota.Effective(st, now)

		if !opts.autoSwitch {
			if printMode && res.Stdout != "" {
				_, _ = os.Stdout.WriteString(res.Stdout)
			}
			return 2
		}

		nextEmail, msg := router.PickDrainWithReserve(order, st, quotas, true, reservePct, now)
		if nextEmail == "" || nextEmail == currentEmail {
			fmt.Fprintf(os.Stderr, "\n\033[31m[agy-pro]\033[0m %s %s，且无其他可用账号：%s\n", currentEmail, kindLabel, msg)
			return 2
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.Active = nextEmail
			s.Order = order
		})

		resumeNote := ""
		if convID != "" {
			resumeNote = fmt.Sprintf(" 并续聊 %s", convID)
		}
		if spawnAsyncVerify {
			fmt.Fprintf(os.Stderr, "\n\033[33m[agy-pro]\033[0m %s %s -> \033[1m%s\033[0m%s\n",
				oldEmail, kindLabel, nextEmail, resumeNote)
		} else {
			fmt.Fprintf(os.Stderr, "\n\033[33m[agy-pro]\033[0m %s %s（约 %s 后重置），自动切号 -> \033[1m%s\033[0m%s\n",
				oldEmail, kindLabel, vault.FmtDuration(float64(resetSec)), nextEmail, resumeNote)
		}

		currentEmail = nextEmail
		if !printMode {
			promptInteractive = runner.ResumeContinuePrompt
		}
	}
	return 2
}

func runCLI(argv []string, stateDir string) int {
	if stateDir == "" {
		stateDir = os.Getenv("AGY_PRO_STATE_DIR")
	}
	v := vault.New(stateDir)
	if len(argv) == 0 {
		return cmdRun(v, nil, runOptions{autoSwitch: true})
	}

	cmd := argv[0]
	rest := argv[1:]

	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(helpText())
		return 0
	case "-V", "--version":
		fmt.Printf("agy-pro %s\n", vault.Version)
		return 0
	case "_verify":
		return cmdInternalVerify(v, rest)
	case "_warm":
		return cmdInternalWarm(v, rest)
	case "status":
		return cmdStatus(v, rest)
	case "config":
		return cmdConfig(v, rest)
	case "login":
		return cmdLogin(v)
	case "import":
		return cmdImport(v, rest)
	case "remove":
		return cmdRemove(v, rest)
	case "use":
		return cmdUse(v, rest)
	case "order":
		return cmdOrder(v, rest)
	case "clear-cooldown":
		_ = v.UpdateState(func(st *vault.State) {
			st.Cooldowns = map[string]*vault.Cooldown{}
			for email, q := range st.Quotas {
				if q != nil && ((q.H5Rem != nil && *q.H5Rem <= 0.5) || (q.D7Rem != nil && *q.D7Rem <= 0.5)) {
					delete(st.Quotas, email)
				}
			}
		})
		fmt.Println("✓ 已清空所有账号的本地冷却与耗尽缓存（可运行 agy-pro status -r 重新拉取实时额度）。")
		return 0
	case "doctor":
		return cmdDoctor(v)
	case "next":
		doResume := false
		var rem []string
		for _, a := range rest {
			if a == "--resume" || a == "-r" {
				doResume = true
			} else {
				rem = append(rem, a)
			}
		}
		if doResume {
			opts, pass := parseRunFlags(rem)
			opts.forceNext = true
			opts.resumeLast = true
			return cmdRun(v, pass, opts)
		}
		accounts := v.ListAccounts()
		st := v.LoadState()
		order := v.OrderedEmails(accounts, st)
		now := float64(time.Now().UnixNano()) / 1e9
		quotas := quota.Effective(st, now)
		old := st.Active
		if old == "" {
			old = "(无)"
		}
		nxt, msg := router.PickDrainWithReserve(order, st, quotas, true, st.ReservePct, now)
		if nxt == "" {
			fmt.Fprintf(os.Stderr, "[agy-pro] %s\n", msg)
			return 2
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.Active = nxt
			s.Order = order
		})
		fmt.Printf("✓ 主号已切换: %s -> %s\n", old, nxt)
		return 0
	case "resume":
		opts, pass := parseRunFlags(rest)
		if len(pass) > 0 && !strings.HasPrefix(pass[0], "-") {
			conv := pass[0]
			return cmdRun(v, append([]string{"--resume", conv}, pass[1:]...), opts)
		}
		opts.resumeLast = true
		return cmdRun(v, pass, opts)
	case "run":
		opts, pass := parseRunFlags(rest)
		return cmdRun(v, pass, opts)
	}

	if !strings.HasPrefix(cmd, "-") {
		sort.Strings(validSubcommands)
		fmt.Fprintf(os.Stderr, "未知子命令: '%s'\n可用子命令: %s\n如需把参数直接传给 agy，请用: agy-pro run -- %s ...\n",
			cmd, strings.Join(validSubcommands, ", "), cmd)
		return 2
	}

	opts, pass := parseRunFlags(argv)
	return cmdRun(v, pass, opts)
}

func main() {
	os.Exit(runCLI(os.Args[1:], ""))
}
