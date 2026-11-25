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

	"github.com/charmbracelet/lipgloss"

	"github.com/Tiggy-Chan/agy-pro/internal/quota"
	"github.com/Tiggy-Chan/agy-pro/internal/router"
	"github.com/Tiggy-Chan/agy-pro/internal/runner"
	"github.com/Tiggy-Chan/agy-pro/internal/tui"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

var validSubcommands = []string{
	"clear-cooldown",
	"config",
	"dash",
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
	"tui",
	"ui",
	"use",
}

func renderHelp() string {
	cmdLine := func(cmd, desc string) string {
		return "  " + ui.SHeadDim.Render(ui.Pad(cmd, 26, ui.AlignLeft)) + " " + desc + "\n"
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(ui.Brand("agy-pro") + " " + ui.SDim.Render("v"+vault.Version) + "  " + ui.SDim.Render("官方 agy 零入侵原生多账号增强工具") + "\n\n")
	b.WriteString(ui.SHead.Render("用法") + "\n")
	b.WriteString("  " + ui.SBold.Render("agy-pro") + " " + ui.SDim.Render("[命令 | agy 原生参数...]") + "\n\n")

	b.WriteString(ui.SHead.Render("日常使用") + "\n")
	b.WriteString(cmdLine("agy-pro", "秒开原生 agy（直连 Google，完全尊重 agy 原生模型与 /model 切换）"))
	b.WriteString(cmdLine("agy-pro ui", "打开交互式 TUI 仪表盘（aliases: dash, tui）"))
	b.WriteString(cmdLine("agy-pro status [-r]", "查看号池双池看板（加 -r 在线刷新全部，--active 仅刷主号）"))
	b.WriteString(cmdLine("agy-pro resume [会话ID]", "用当前主号续聊当前目录最近一次（或指定）会话"))
	b.WriteString(cmdLine("agy-pro next [--resume]", "切到下一个可用账号（加 --resume 直接续聊）"))
	b.WriteString(cmdLine("agy-pro run [选项] -- ...", "显式透传参数给原生 agy"))
	b.WriteString("\n")

	b.WriteString(ui.SHead.Render("号池管理") + "\n")
	b.WriteString(cmdLine("agy-pro login", "调用官方 agy acp --login 登录新账号并存入独立号池"))
	b.WriteString(cmdLine("agy-pro import <路径>", "导入已有 antigravity-oauth-token 文件或目录"))
	b.WriteString(cmdLine("agy-pro use <关键词>", "手动指定当前 drain 主号（支持邮箱子串）"))
	b.WriteString(cmdLine("agy-pro order <号1> ...", "自定义 drain 轮换顺序"))
	b.WriteString(cmdLine("agy-pro remove <关键词>", "从号池移除指定账号"))
	b.WriteString(cmdLine("agy-pro config reserve <N>", "设置低余量提前切号水位线（0–50，单位 %）"))
	b.WriteString(cmdLine("agy-pro clear-cooldown", "清空本地冷却与耗尽缓存"))
	b.WriteString(cmdLine("agy-pro doctor", "环境与号池健康体检"))
	b.WriteString("\n")

	b.WriteString(ui.SHead.Render("启动选项") + "\n")
	b.WriteString(cmdLine("--account <关键词>", "本次临时指定某个账号"))
	b.WriteString(cmdLine("--reserve <0-50>", "本次覆盖低余量提前切号水位线"))
	b.WriteString(cmdLine("--refresh", "启动前先联网刷新当前主号额度"))
	b.WriteString(cmdLine("--no-auto-switch", "关闭撞 5h/7d 墙自动秒切续跑"))
	b.WriteString("\n")

	b.WriteString(ui.SHead.Render("示例") + "\n")
	b.WriteString(ui.SDim.Render("  打开交互式多号面板（上下选择、Enter 直接启动 agy）") + "\n")
	b.WriteString("    " + ui.SAccent.Render("agy-pro ui") + "\n")
	b.WriteString(ui.SDim.Render("  透传原生参数继续指定会话") + "\n")
	b.WriteString("    " + ui.SAccent.Render("agy-pro --dangerously-skip-permissions -r <conversation-id>") + "\n")
	return b.String()
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	dp := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		dp[j] = j
	}
	for i := 1; i <= la; i++ {
		prev := dp[0]
		dp[0] = i
		for j := 1; j <= lb; j++ {
			tmp := dp[j]
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			m := dp[j] + 1
			if dp[j-1]+1 < m {
				m = dp[j-1] + 1
			}
			if prev+cost < m {
				m = prev + cost
			}
			dp[j] = m
			prev = tmp
		}
	}
	return dp[lb]
}

func suggestSubcommand(input string) string {
	best, bestDist := "", 99
	low := strings.ToLower(input)
	for _, cand := range validSubcommands {
		d := levenshtein(low, cand)
		if d < bestDist {
			bestDist = d
			best = cand
		}
	}
	if bestDist <= 2 {
		return best
	}
	return ""
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

func mergeQuotaResult(prev, fresh *vault.Quota) *vault.Quota {
	if fresh == nil {
		return prev
	}
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
		return 0
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

func statusBadge(isActive, ok bool, why string, q *vault.Quota, reservePct float64) string {
	if !ok {
		switch {
		case strings.HasPrefix(why, "7d"):
			return ui.BadgeFull.Render("7D FULL")
		case strings.HasPrefix(why, "5h"):
			return ui.BadgeFull.Render("5H FULL")
		default:
			return ui.BadgeCooling.Render("COOLING")
		}
	}
	if q != nil && q.Error != "" && q.H5Used == nil && q.D7Used == nil {
		return ui.SAmber.Render("! ERR")
	}
	if reservePct > 0 && !router.AboveReserve(q, reservePct) {
		if isActive {
			return ui.BadgeCooling.Render("ACTIVE·LOW")
		}
		return ui.SAmber.Render("LOW")
	}
	if isActive {
		return ui.BadgeActive.Render("ACTIVE")
	}
	return ui.SGood.Render(" READY ")
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
			fmt.Fprintf(os.Stderr, "%s 未知选项: %s\n", ui.SRedB.Render("error:"), a)
			return 2
		}
	}

	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	if len(order) == 0 {
		fmt.Println(ui.SAmber.Render("!") + " 号池为空。请运行 " + ui.SHeadDim.Render("agy-pro login") + " 登录账号，或 " + ui.SHeadDim.Render("agy-pro import <目录>") + " 导入。")
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

		ui.StderrProgress(fmt.Sprintf("正在查询额度 [0/%d] ...", len(targets)), false)
		fresh := quota.RefreshAll(targets, agyBin, 3, 35*time.Second, func(done, total int, email string, q *vault.Quota) {
			tag := "ok"
			if q.Error != "" {
				tag = q.Error
			}
			ui.StderrProgress(fmt.Sprintf("正在查询额度 [%d/%d] %s (%s)", done, total, email, tag), false)
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

	termW := ui.TerminalColumns(80)
	cardW := termW
	if cardW > 110 {
		cardW = 110
	}
	if cardW < 60 {
		cardW = 60
	}
	innerW := cardW - 6

	readyCount := 0
	oldestAt := 0.0
	for _, e := range order {
		if ok, _ := router.AccountAvailability(e, st, quotas, now); ok {
			readyCount++
		}
		if q := quotas[e]; q != nil && q.At > 0 && (oldestAt == 0 || q.At < oldestAt) {
			oldestAt = q.At
		}
	}

	// ---- Card Header ----
	leftHead := ui.Brand("agy-pro") + " " + ui.SDim.Render("v"+vault.Version)
	meta := []string{ui.SHeadDim.Render("native"), ui.SDim.Render("drain")}
	if st.ReservePct > 0 {
		meta = append(meta, ui.SDim.Render(fmt.Sprintf("reserve %.0f%%", st.ReservePct)))
	}
	if oldestAt > 0 {
		meta = append(meta, ui.SDim.Render("cache "+vault.FmtAge(oldestAt, now)))
	}
	readyText := ui.SGoodB.Render(fmt.Sprintf("%d/%d ready", readyCount, len(order)))
	if readyCount == 0 {
		readyText = ui.SRedB.Render(fmt.Sprintf("0/%d ready", len(order)))
	}
	headLine := leftHead + "  " + strings.Join(meta, ui.SDim.Render(" · "))
	if gap := innerW - lipgloss.Width(headLine) - lipgloss.Width(readyText); gap > 1 {
		headLine += strings.Repeat(" ", gap) + readyText
	} else {
		headLine += "  " + readyText
	}

	// ---- Account Rows ----
	compact := innerW < 88
	barW := 7
	if compact {
		barW = 5
	}
	showReset := innerW >= 96

	rows := make([][]string, 0, len(order))
	for _, e := range order {
		isActive := e == active
		q := quotas[e]
		ok, why := router.AccountAvailability(e, st, quotas, now)

		dot := ui.SDim.Render("○")
		if isActive {
			dot = ui.SGoodB.Render("●")
		}
		emailCell := e
		if isActive {
			emailCell = ui.SBold.Render(e)
		} else if !ok {
			emailCell = ui.SDim.Render(e)
		}

		var g5, g7, claude string
		if q != nil {
			g5 = ui.RenderBar(q.H5Used, barW)
			g7 = ui.RenderBar(q.D7Used, barW)
			if showReset && q.D7Reset > 0 && q.D7Used != nil && *q.D7Used > 0 {
				g7 += " " + ui.SDim.Render("↻"+vault.FmtLocalTime(q.D7Reset)[:5])
			}
			claude = ui.RenderCompactPct(q.C5Used) + ui.SDim.Render("/") + ui.RenderCompactPct(q.C7Used)
		} else {
			g5 = ui.RenderBar(nil, barW)
			g7 = ui.RenderBar(nil, barW)
			claude = ui.SDim.Render("-/-")
		}

		stBadge := statusBadge(isActive, ok, why, q, st.ReservePct)
		if !ok && showReset && strings.HasPrefix(why, "7d") && q != nil && q.D7Reset > 0 {
			stBadge += " " + ui.SDim.Render(vault.FmtLocalTime(q.D7Reset))
		} else if !ok && showReset && strings.HasPrefix(why, "冷却中") {
			stBadge += " " + ui.SDim.Render(strings.TrimPrefix(why, "冷却中 "))
		}

		if compact {
			rows = append(rows, []string{dot, emailCell, g5, g7, stBadge})
		} else {
			rows = append(rows, []string{dot, emailCell, g5, g7, claude, stBadge})
		}
	}

	var body strings.Builder
	body.WriteString(headLine + "\n")
	body.WriteString(ui.Hairline(innerW) + "\n")
	if compact {
		headers := []string{"", "ACCOUNT", "G·5H", "G·7D", "STATUS"}
		aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft}
		body.WriteString(ui.RenderTable(headers, rows, aligns, innerW, 1))
	} else {
		headers := []string{"", "ACCOUNT", "GEMINI 5H", "GEMINI 7D", "CLAUDE", "STATUS"}
		aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignLeft}
		body.WriteString(ui.RenderTable(headers, rows, aligns, innerW, 1))
	}

	fmt.Println(ui.RenderCard(body.String(), cardW, ui.ColorRule))

	cwd, _ := os.Getwd()
	lastConv := st.LastConv[cwd]
	hints := []string{
		ui.KeyHint("agy-pro ui", "交互面板"),
	}
	if !needRefresh {
		hints = append(hints, ui.KeyHint("agy-pro status -r", "刷新实时额度"))
	}
	if lastConv != "" {
		shortID := lastConv
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		hints = append(hints, ui.KeyHint("agy-pro resume", shortID))
	}
	fmt.Println("  " + strings.Join(hints, ui.SDim.Render("  ·  ")))
	return 0
}

func cmdUI(v *vault.Vault) int {
	agyBin := vault.FindAgyBinary()
	for {
		res, err := tui.Run(v, agyBin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s 无法启动交互面板: %v\n", ui.SRedB.Render("error:"), err)
			return 1
		}
		switch res.Action {
		case tui.ActionLaunch:
			return cmdRun(v, nil, runOptions{autoSwitch: true, explicitAccount: res.Email})
		case tui.ActionLogin:
			_ = cmdLogin(v)
			// Return to TUI so user immediately sees the newly logged-in account
			continue
		default:
			return 0
		}
	}
}

func cmdConfig(v *vault.Vault, args []string) int {
	st := v.LoadState()
	if len(args) == 0 {
		fmt.Printf("%s  %s  %s\n",
			ui.SHeadDim.Render("reserve"),
			ui.SBold.Render(fmt.Sprintf("%.0f%%", st.ReservePct)),
			ui.SDim.Render("（启动新会话前若当前主号剩余 <= 水位线则自动提前切至满血备号；0 表示用尽再切）"))
		return 0
	}
	if args[0] == "reserve" && len(args) == 2 {
		val, err := strconv.ParseFloat(strings.TrimSuffix(args[1], "%"), 64)
		if err != nil || val < 0 || val > 50 {
			fmt.Fprintf(os.Stderr, "%s reserve 需为 0 到 50 之间的数字（例如: agy-pro config reserve 10）\n", ui.SRedB.Render("error:"))
			return 2
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.ReservePct = val
		})
		if val == 0 {
			fmt.Printf("%s 已关闭低余量提前切号 %s\n", ui.SGoodB.Render("✓"), ui.SDim.Render("(reserve = 0%)"))
		} else {
			fmt.Printf("%s 已设置低余量提前切号水位线: %s\n", ui.SGoodB.Render("✓"), ui.SGoodB.Render(fmt.Sprintf("%.0f%%", val)))
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "用法: agy-pro config [reserve <0-50>]")
	return 2
}

func cmdLogin(v *vault.Vault) int {
	agyBin := vault.FindAgyBinary()
	fmt.Println(ui.SHeadDim.Render("→") + " 正在调用官方 " + ui.SBold.Render("agy acp --login") + " 登录新 Google 账号" + ui.SDim.Render("（隔离临时目录，不影响系统钥匙串）"))
	email, err := v.LoginNewAccount(agyBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s 登录失败: %v\n", ui.SRedB.Render("✗"), err)
		return 1
	}
	fmt.Printf("%s 已登录并存入独立号池: %s\n", ui.SGoodB.Render("✓"), ui.SBold.Render(email))
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
		fmt.Fprintf(os.Stderr, "%s 路径无效: %v\n", ui.SRedB.Render("✗"), err)
		return 1
	}
	info, err := os.Stat(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s 路径不存在: %s\n", ui.SRedB.Render("✗"), abs)
		return 1
	}
	if !info.IsDir() {
		email, err := v.ImportTokenFile(abs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s 导入失败: %v\n", ui.SRedB.Render("✗"), err)
			return 1
		}
		fmt.Printf("%s 已导入账号: %s\n", ui.SGoodB.Render("✓"), ui.SBold.Render(email))
		return 0
	}
	emails, err := v.ImportDirectory(abs)
	if err != nil || len(emails) == 0 {
		fmt.Fprintf(os.Stderr, "%s 未在 %s 下找到任何有效的 antigravity-oauth-token 文件。\n", ui.SAmberB.Render("!"), abs)
		return 1
	}
	fmt.Printf("%s 已导入 %d 个账号到独立号池 %s\n", ui.SGoodB.Render("✓"), len(emails), ui.SDim.Render("("+v.AccountsDir+")"))
	for _, e := range emails {
		fmt.Printf("  %s %s\n", ui.SGood.Render("·"), e)
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
		fmt.Fprintf(os.Stderr, "%s %s\n", ui.SRedB.Render("error:"), errMsg)
		return 1
	}
	v.RemoveAccount(email)
	fmt.Printf("%s 已从号池移除: %s\n", ui.SGoodB.Render("✓"), email)
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
		fmt.Fprintf(os.Stderr, "%s %s\n", ui.SRedB.Render("error:"), errMsg)
		return 1
	}
	_ = v.UpdateState(func(s *vault.State) {
		s.Active = email
		s.Order = order
		delete(s.Cooldowns, email)
	})
	fmt.Printf("%s 当前主号已切换为: %s\n", ui.SGoodB.Render("✓"), ui.SBold.Render(email))
	return 0
}

func cmdOrder(v *vault.Vault, args []string) int {
	accounts := v.ListAccounts()
	st := v.LoadState()
	cur := v.OrderedEmails(accounts, st)
	if len(args) == 0 {
		for i, e := range cur {
			fmt.Printf("  %s %s\n", ui.SDim.Render(fmt.Sprintf("%d.", i+1)), e)
		}
		return 0
	}
	var newOrder []string
	seen := map[string]bool{}
	for _, t := range args {
		email, errMsg := matchEmail(cur, t)
		if email == "" {
			fmt.Fprintf(os.Stderr, "%s %s\n", ui.SRedB.Render("error:"), errMsg)
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
	fmt.Printf("%s 已更新 drain 顺序:\n", ui.SGoodB.Render("✓"))
	for i, e := range newOrder {
		fmt.Printf("  %s %s\n", ui.SDim.Render(fmt.Sprintf("%d.", i+1)), e)
	}
	return 0
}

func cmdDoctor(v *vault.Vault) int {
	agyBin := vault.FindAgyBinary()
	okAll := true

	var envLines []string
	if info, err := os.Stat(agyBin); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		c := exec.Command(agyBin, "--version")
		c.Stdin = nil
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, _ := c.Output()
		envLines = append(envLines, fmt.Sprintf("  %s 原生 agy    %s %s", ui.SGood.Render("✓"), agyBin, ui.SDim.Render("v"+strings.TrimSpace(string(out)))))
	} else {
		envLines = append(envLines, fmt.Sprintf("  %s 原生 agy    %s", ui.SRed.Render("✗"), ui.SRed.Render("未找到: "+agyBin)))
		okAll = false
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
		envLines = append(envLines, fmt.Sprintf("  %s 网络隔离    %s", ui.SGood.Render("✓"), ui.SDim.Render("干净（零中转直连 Google）")))
	} else {
		envLines = append(envLines, fmt.Sprintf("  %s 网络隔离    %s", ui.SRed.Render("✗"), ui.SRed.Render(fmt.Sprintf("泄漏: %v", leaked))))
		okAll = false
	}
	envLines = append(envLines, fmt.Sprintf("  %s 真实 HOME   %s", ui.SGood.Render("✓"), ui.SDim.Render(homeVal)))
	envLines = append(envLines, fmt.Sprintf("  %s 模型配置    %s", ui.SGood.Render("✓"), ui.SDim.Render("遵循 agy 原生设置与 /model 切换（零强制覆盖）")))

	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	envLines = append(envLines, fmt.Sprintf("  %s 提前切号    %s", ui.SGood.Render("✓"), ui.SDim.Render(fmt.Sprintf("水位线 %.0f%%", st.ReservePct))))

	var acctLines []string
	if len(order) == 0 {
		acctLines = append(acctLines, fmt.Sprintf("  %s %s", ui.SRed.Render("✗"), ui.SAmber.Render("号池为空（请运行 agy-pro login）")))
		okAll = false
	}
	for _, e := range order {
		meta, err := vault.ReadTokenMeta(accounts[e].TokenFile)
		good := err == nil && meta.HasRefresh
		if !good {
			okAll = false
		}
		mark := ui.SDim.Render("○")
		name := e
		if e == st.Active {
			mark = ui.SGoodB.Render("●")
			name = ui.SBold.Render(e)
		}
		tokStatus := ui.SGood.Render("✓ refresh_token 正常")
		if !good {
			tokStatus = ui.SRed.Render("✗ token 异常")
		}
		acctLines = append(acctLines, fmt.Sprintf("  %s %s  %s", mark, ui.Pad(name, 26, ui.AlignLeft), tokStatus))
	}

	termW := ui.TerminalColumns(80)
	cardW := termW
	if cardW > 86 {
		cardW = 86
	}
	if cardW < 56 {
		cardW = 56
	}
	innerW := cardW - 6

	badge := ui.BadgePass.Render("PASS")
	if !okAll {
		badge = ui.BadgeFail.Render("FAIL")
	}

	var b strings.Builder
	b.WriteString(badge + "  " + ui.Brand("agy-pro") + " " + ui.SDim.Render("v"+vault.Version+" · doctor") + "\n")
	b.WriteString(ui.Hairline(innerW) + "\n")
	b.WriteString(ui.SHead.Render("运行环境") + "\n")
	b.WriteString(strings.Join(envLines, "\n") + "\n\n")
	b.WriteString(ui.SHead.Render(fmt.Sprintf("独立号池 (%d)", len(order))) + " " + ui.SDim.Render(v.AccountsDir) + "\n")
	b.WriteString(strings.Join(acctLines, "\n"))

	fmt.Println(ui.RenderCard(b.String(), cardW, ui.ColorRule))
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
		fmt.Fprintf(os.Stderr, "%s 独立号池为空。请先运行 %s 或 %s。\n",
			ui.SRedB.Render("error:"), ui.SHeadDim.Render("agy-pro login"), ui.SHeadDim.Render("agy-pro import <目录>"))
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
			fmt.Fprintf(os.Stderr, "%s %s\n", ui.SRedB.Render("error:"), errMsg)
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
			fmt.Fprintf(os.Stderr, "%s %s\n", ui.SAmberB.Render("!"), reason)
			return 2
		}
		currentEmail = picked
		if reason == "reserve-advanced" && oldActive != "" && oldActive != currentEmail {
			fmt.Fprintf(os.Stderr, "%s %s %s %s %s\n",
				ui.Brand("agy-pro"),
				ui.SDim.Render("·"),
				ui.SAmber.Render(fmt.Sprintf("%s 剩余 <= %.0f%%", oldActive, reservePct)),
				ui.SDim.Render("→"),
				ui.SGoodB.Render(currentEmail))
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.Active = currentEmail
			s.Order = order
		})
	}

	if opts.refreshBefore {
		ui.StderrProgress(fmt.Sprintf("正在刷新 %s 额度...", currentEmail), false)
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
				fmt.Fprintf(os.Stderr, "%s %s\n", ui.SAmberB.Render("!"), reason)
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
			fmt.Fprintln(os.Stderr, ui.SDim.Render("agy-pro · 当前目录无最近会话记录，开启新会话"))
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
		if !printMode {
			if meta, err := vault.ReadTokenMeta(acct.TokenFile); err == nil && meta.Expiry > 0 && meta.Expiry <= float64(time.Now().Unix())+60 {
				ui.StderrProgress(fmt.Sprintf("静默刷新 %s 登录态...", currentEmail), false)
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
			fmt.Fprintf(os.Stderr, "%s 切换 token 失败: %v\n", ui.SRedB.Render("error:"), err)
			return 1
		}

		if !printMode {
			if standby := router.NextStandby(order, currentEmail, st, quotas, float64(time.Now().UnixNano())/1e9); standby != "" {
				if sm, err := vault.ReadTokenMeta(accounts[standby].TokenFile); err == nil && sm.Expiry > 0 && sm.Expiry <= float64(time.Now().Unix())+300 {
					spawnDetachedSelf(v.StateDir, "_warm", standby)
				}
			}
		}

		if !printMode {
			q := quotas[currentEmail]
			var qParts []string
			if q != nil {
				qParts = append(qParts, ui.SDim.Render("5h ")+ui.RenderBar(q.H5Used, 5))
				qParts = append(qParts, ui.SDim.Render("7d ")+ui.RenderBar(q.D7Used, 5))
			} else {
				qParts = append(qParts, ui.SDim.Render("native direct"))
			}
			fmt.Fprintf(os.Stderr, "%s %s %s %s %s\n",
				ui.Brand("agy-pro"),
				ui.SDim.Render("·"),
				ui.SGoodB.Render("●")+" "+ui.SBold.Render(currentEmail),
				ui.SDim.Render("·"),
				strings.Join(qParts, ui.SDim.Render("  ")))
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

		now = float64(time.Now().UnixNano()) / 1e9
		resetSec := 1800
		kindLabel := "额度耗尽"
		cdReason := "429 verifying..."
		spawnAsyncVerify := false

		if res.ExhaustionKind == "fast" {
			kindLabel = "触发 429 额度墙（秒切，后台核验精准重置时间）"
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
			fmt.Fprintf(os.Stderr, "\n%s %s %s：%s\n", ui.SRedB.Render("✗"), oldEmail, kindLabel, msg)
			return 2
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.Active = nextEmail
			s.Order = order
		})

		resumeNote := ""
		if convID != "" {
			shortID := convID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			resumeNote = " " + ui.SDim.Render("resume "+shortID)
		}
		fmt.Fprintf(os.Stderr, "\n%s %s %s %s %s%s\n",
			ui.Brand("agy-pro"),
			ui.SAmber.Render("↻"),
			ui.SDim.Render(oldEmail+" "+kindLabel),
			ui.SAccentB.Render("→"),
			ui.SGoodB.Render(nextEmail),
			resumeNote)

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
		fmt.Print(renderHelp())
		return 0
	case "-V", "--version":
		fmt.Printf("%s %s\n", ui.Brand("agy-pro"), vault.Version)
		return 0
	case "_verify":
		return cmdInternalVerify(v, rest)
	case "_warm":
		return cmdInternalWarm(v, rest)
	case "ui", "dash", "tui":
		return cmdUI(v)
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
		fmt.Printf("%s 已清空所有账号的本地冷却与耗尽缓存 %s\n", ui.SGoodB.Render("✓"), ui.SDim.Render("(可运行 agy-pro status -r 刷新)"))
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
			fmt.Fprintf(os.Stderr, "%s %s\n", ui.SAmberB.Render("!"), msg)
			return 2
		}
		_ = v.UpdateState(func(s *vault.State) {
			s.Active = nxt
			s.Order = order
		})
		fmt.Printf("%s 主号已切换: %s %s %s\n", ui.SGoodB.Render("✓"), ui.SDim.Render(old), ui.SAccentB.Render("→"), ui.SBold.Render(nxt))
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
		fmt.Fprintf(os.Stderr, "%s 未知子命令: %s\n", ui.SRedB.Render("error:"), cmd)
		if sug := suggestSubcommand(cmd); sug != "" {
			fmt.Fprintf(os.Stderr, "最相似的命令是：%s\n", ui.SHeadDim.Render(sug))
		}
		fmt.Fprintf(os.Stderr, "%s\n", ui.SDim.Render("查看全部命令：agy-pro --help（如需透传参数给 agy，请用 agy-pro run -- ...）"))
		return 2
	}

	opts, pass := parseRunFlags(argv)
	return cmdRun(v, pass, opts)
}

func main() {
	os.Exit(runCLI(os.Args[1:], ""))
}
