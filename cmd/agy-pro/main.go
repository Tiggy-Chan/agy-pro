package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	return fmt.Sprintf(`agy-pro %s (Go) — 官方 agy 零入侵原生多账号增强工具（drain 切号 + 撞墙自动续跑）

用法:
  agy-pro                        秒开原生 agy（默认按 drain 主号直连 Google，完全尊重 agy 原生模型与配置）
  agy-pro [agy原生参数...]        直接兼容并透传 agy 参数（如 agy-pro --dangerously-skip-permissions -r <ID>）
  agy-pro run [选项] [-- agy参数] 显式启动并透传参数给原生 agy
  agy-pro status [-r] [--active] 秒显号池看板（默认读缓存 <5ms 出表；加 -r 在线刷新，加 --active 仅刷新主号）
  agy-pro login                  调用官方 agy acp --login 登录新账号并存入独立号池
  agy-pro import <文件或目录>     导入已有 antigravity-oauth-token 到独立号池
  agy-pro remove <关键词|邮箱>    从号池移除指定账号
  agy-pro use <关键词|邮箱>       手动指定当前 drain 主号（如: agy-pro use 003）
  agy-pro next [--resume]        切到下一个可用账号（带 --resume 直接续聊当前目录上次会话）
  agy-pro resume [会话ID]        用当前主号续聊当前目录最近一次（或指定）会话
  agy-pro order <号1> <号2> ...  自定义 drain 轮换顺序（支持邮箱关键字）
  agy-pro clear-cooldown         清空本地冷却记录
  agy-pro doctor                 环境与号池体检

启动选项（可用于 agy-pro 或 agy-pro run）:
  --account <关键词>              临时指定账号
  --no-auto-switch                关闭撞 5h/7d 墙自动重启切号
  --refresh                       启动前先联网刷新当前账号额度
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

		for e, q := range fresh {
			// Preserve previous valid numbers if this refresh attempt timed out
			if q.Error != "" && q.H5Used == nil && q.D7Used == nil {
				if prev := st.Quotas[e]; prev != nil && (prev.H5Used != nil || prev.D7Used != nil) {
					prev.Error = q.Error
					continue
				}
			}
			st.Quotas[e] = q
		}
		_ = v.SaveState(st)
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
	compact := cols < 92

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

		h5 := "-"
		if q != nil && q.H5Used != nil {
			if compact {
				h5 = fmt.Sprintf("%.0f%%", *q.H5Used)
			} else {
				h5 = fmt.Sprintf("%.1f%% (%s)", *q.H5Used, vault.FmtLocalTime(q.H5Reset))
			}
		}

		d7 := "-"
		if q != nil && q.D7Used != nil {
			if compact {
				d7 = fmt.Sprintf("%.0f%%", *q.D7Used)
			} else {
				d7 = fmt.Sprintf("%.1f%% (%s)", *q.D7Used, vault.FmtLocalTime(q.D7Reset))
			}
		}

		ok, why := router.AccountAvailability(e, st, quotas, now)
		stTxt := why
		if q != nil && q.Error != "" && q.H5Used == nil {
			stTxt = "失败:" + q.Error
		} else if ok {
			if e == active {
				stTxt = "主号"
			} else {
				stTxt = "就绪"
			}
		}
		rows = append(rows, []string{mark, e, h5, d7, stTxt})
	}

	ageTxt := "无缓存"
	if oldestAt > 0 {
		ageTxt = vault.FmtAge(oldestAt, now)
	}
	fmt.Printf("agy-pro | 原生直连 | drain | 缓存:%s\n", ageTxt)
	headers := []string{"", "账号", "5h已用", "7d已用", "状态"}
	aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignLeft}
	fmt.Println(ui.RenderTable(headers, rows, aligns, cols, 1))

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
	st.Active = email
	st.Order = order
	delete(st.Cooldowns, email)
	_ = v.SaveState(st)
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
	st.Order = newOrder
	_ = v.SaveState(st)
	fmt.Println("✓ 已更新 drain 顺序:")
	for i, e := range newOrder {
		fmt.Printf("  %d. %s\n", i+1, e)
	}
	return 0
}

func cmdDoctor(v *vault.Vault) int {
	agyBin := vault.FindAgyBinary()
	fmt.Printf("=== agy-pro %s (Go) 体检（纯原生独立模式）===\n", vault.Version)
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
		st.Active = currentEmail
		st.Order = order
		_ = v.SaveState(st)
	} else {
		picked, reason := router.PickDrain(order, st, quotas, opts.forceNext, now)
		if picked == "" {
			fmt.Fprintf(os.Stderr, "[agy-pro] %s\n", reason)
			return 2
		}
		currentEmail = picked
		_ = v.SaveState(st)
	}

	if opts.refreshBefore {
		ui.StderrProgress(fmt.Sprintf("[agy-pro] 正在刷新 %s 额度...", currentEmail), false)
		q := quota.ProbeAccount(accounts[currentEmail], agyBin, 20*time.Second)
		ui.StderrProgress("", true)
		st.Quotas[currentEmail] = q
		_ = v.SaveState(st)
		now = float64(time.Now().UnixNano()) / 1e9
		quotas = quota.Effective(st, now)
		if opts.explicitAccount == "" {
			picked, reason := router.PickDrain(order, st, quotas, false, now)
			if picked == "" {
				fmt.Fprintf(os.Stderr, "[agy-pro] %s\n", reason)
				return 2
			}
			currentEmail = picked
			_ = v.SaveState(st)
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
		// Pre-warm expired OAuth access_token silently so interactive agy starts already
		// authenticated (expired=false) and never flashes a "Sign in" screen.
		if !printMode {
			if meta, err := vault.ReadTokenMeta(acct.TokenFile); err == nil && meta.Expiry > 0 && meta.Expiry <= float64(time.Now().Unix())+60 {
				ui.StderrProgress(fmt.Sprintf("[agy-pro] 静默刷新 %s 登录态...", currentEmail), false)
				_ = quota.ProbeAccount(acct, agyBin, 15*time.Second)
				ui.StderrProgress("", true)
			}
		}

		if err := v.ActivateToken(acct); err != nil {
			fmt.Fprintf(os.Stderr, "[agy-pro] 切换 token 失败: %v\n", err)
			return 1
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
			st.LastConv[cwd] = convID
			_ = v.SaveState(st)
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

		// Verified quota exhaustion (7d OR 5h, or probe-timeout fallback) -> record exact reset & advance drain account
		now = float64(time.Now().UnixNano()) / 1e9
		resetSec := 1800 // conservative 30m fallback when live probe timed out
		kindLabel := "额度耗尽"
		cdReason := "quota exhausted"
		if res.VerifiedQuota != nil {
			st.Quotas[currentEmail] = res.VerifiedQuota
			if res.ExhaustionKind == "7d" {
				kindLabel = "7天额度耗尽"
				cdReason = "7d quota exhausted"
				if res.VerifiedQuota.D7Reset > now {
					resetSec = int(res.VerifiedQuota.D7Reset - now)
				} else {
					resetSec = 3600
				}
			} else if res.ExhaustionKind == "5h" {
				kindLabel = "5小时额度耗尽"
				cdReason = "5h quota exhausted"
				if res.VerifiedQuota.H5Reset > now {
					resetSec = int(res.VerifiedQuota.H5Reset - now)
				} else {
					resetSec = 3600
				}
			}
		} else {
			kindLabel = "疑似额度耗尽（查额度超时，临时冷却）"
			cdReason = "429 suspected quota (probe timeout)"
		}
		router.MarkCooldown(st, currentEmail, resetSec, cdReason, now)
		_ = v.SaveState(st)
		quotas = quota.Effective(st, now)

		if !opts.autoSwitch {
			if printMode && res.Stdout != "" {
				_, _ = os.Stdout.WriteString(res.Stdout)
			}
			return 2
		}

		nextEmail, msg := router.PickDrain(order, st, quotas, true, now)
		if nextEmail == "" || nextEmail == currentEmail {
			fmt.Fprintf(os.Stderr, "\n\033[31m[agy-pro]\033[0m %s %s，且无其他可用账号：%s\n", currentEmail, kindLabel, msg)
			return 2
		}
		_ = v.SaveState(st)

		resumeNote := ""
		if convID != "" {
			resumeNote = fmt.Sprintf(" 并续聊 %s", convID)
		}
		fmt.Fprintf(os.Stderr, "\n\033[33m[agy-pro]\033[0m %s %s（约 %s 后重置），自动切号 -> \033[1m%s\033[0m%s\n",
			currentEmail, kindLabel, vault.FmtDuration(float64(resetSec)), nextEmail, resumeNote)

		currentEmail = nextEmail
		if !printMode {
			promptInteractive = runner.ResumeContinuePrompt
		}
	}
	return 2
}

func runCLI(argv []string, stateDir string) int {
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
	case "status":
		return cmdStatus(v, rest)
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
		st := v.LoadState()
		st.Cooldowns = map[string]*vault.Cooldown{}
		for email, q := range st.Quotas {
			if q != nil && ((q.H5Rem != nil && *q.H5Rem <= 0.5) || (q.D7Rem != nil && *q.D7Rem <= 0.5)) {
				delete(st.Quotas, email)
			}
		}
		_ = v.SaveState(st)
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
		nxt, msg := router.PickDrain(order, st, quotas, true, now)
		if nxt == "" {
			fmt.Fprintf(os.Stderr, "[agy-pro] %s\n", msg)
			return 2
		}
		_ = v.SaveState(st)
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

	// Reject unknown bare subcommands instead of launching interactive TUI on typos
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
