package slash

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
	"github.com/Tiggy-Chan/agy-pro/internal/quota"
	"github.com/Tiggy-Chan/agy-pro/internal/router"
	"github.com/Tiggy-Chan/agy-pro/internal/sessions"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/usage"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

type CmdKind int

const (
	CmdNone CmdKind = iota
	CmdDash
	CmdNext
)

type ParsedCommand struct {
	Kind CmdKind
	Args []string
}

type preInvocationInput struct {
	InvocationNum  int    `json:"invocationNum"`
	LastUserInput  string `json:"lastUserInput"`
	ConversationID string `json:"conversationId"`
	CWD            string `json:"cwd"`
}

type hookResponse struct {
	InjectSteps         []hookStep `json:"injectSteps,omitempty"`
	TerminationBehavior string     `json:"terminationBehavior,omitempty"`
}

type hookStep struct {
	SystemMessage *hookSystemMessage `json:"systemMessage,omitempty"`
	ErrorMessage  *hookErrorMessage  `json:"errorMessage,omitempty"`
}

type hookSystemMessage struct {
	SystemMessage string         `json:"systemMessage"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type hookErrorMessage struct {
	UserMessage string `json:"userMessage"`
}

// ParseCommand checks whether raw user input is an agy-pro in-session slash command
// (`/dash ...` or `/next ...`, plus namespaced aliases `/agy-pro:dash`, `/agy-pro:next`).
// Normal prose mentioning `/dash` mid-sentence or multi-line prompts never match.
func ParseCommand(raw string) ParsedCommand {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !strings.HasPrefix(trimmed, "/") {
		return ParsedCommand{}
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return ParsedCommand{}
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return ParsedCommand{}
	}
	head := strings.ToLower(fields[0])
	switch head {
	case "/dash", "/agy-dash", "/agy-pro:dash":
		return ParsedCommand{Kind: CmdDash, Args: fields[1:]}
	case "/next", "/agy-next", "/agy-pro:next":
		return ParsedCommand{Kind: CmdNext, Args: fields[1:]}
	default:
		return ParsedCommand{}
	}
}

func useMarkdownRender() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AGY_PRO_HOOK_RENDER")))
	return v == "markdown" || v == "user_markdown"
}

// HandlePreInvocation implements the official Antigravity PreInvocation hook JSON contract.
// Non-matching prompts or subsequent invocations (`invocationNum > 0`) exit immediately with `{}`.
//
// By default, renders clean monospace Unicode cards via `errorMessage.userMessage` +
// `terminationBehavior: "terminate"` (0 tokens, <50ms, works cleanly in current agy builds
// while bypassing agy's upstream proto_utils panic on `systemMessage.metadata`).
// Setting `AGY_PRO_HOOK_RENDER=markdown` opts into `systemMessage.metadata.user_markdown`.
func HandlePreInvocation(v *vault.Vault, agyBin string, stdin io.Reader, stdout io.Writer) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, 256<<10))
	if err != nil {
		writeHookJSON(stdout, hookResponse{})
		return 0
	}
	var in preInvocationInput
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &in) != nil {
		writeHookJSON(stdout, hookResponse{})
		return 0
	}
	// Antigravity uses 0-based invocationNum: only intercept on the initial turn invocation (0).
	if in.InvocationNum > 0 {
		writeHookJSON(stdout, hookResponse{})
		return 0
	}

	cmd := ParseCommand(in.LastUserInput)
	if cmd.Kind == CmdNone {
		writeHookJSON(stdout, hookResponse{})
		return 0
	}

	if useMarkdownRender() {
		var md string
		switch cmd.Kind {
		case CmdDash:
			md = handleDashMarkdown(v, agyBin, cmd.Args)
		case CmdNext:
			md = handleNextMarkdown(v, cmd.Args)
		}
		writeHookJSON(stdout, hookResponse{
			InjectSteps: []hookStep{
				{
					SystemMessage: &hookSystemMessage{
						SystemMessage: "",
						Metadata: map[string]any{
							"user_markdown": md,
						},
					},
				},
			},
			TerminationBehavior: "terminate",
		})
		return 0
	}

	var plain string
	switch cmd.Kind {
	case CmdDash:
		plain = handleDashPlain(v, agyBin, cmd.Args)
	case CmdNext:
		plain = handleNextPlain(v, cmd.Args)
	}
	writeHookJSON(stdout, hookResponse{
		InjectSteps: []hookStep{
			{
				ErrorMessage: &hookErrorMessage{
					UserMessage: strings.TrimRight(plain, "\n"),
				},
			},
		},
		TerminationBehavior: "terminate",
	})
	return 0
}

func writeHookJSON(w io.Writer, resp hookResponse) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

type ctrlStatusResp struct {
	Email    string `json:"email"`
	Mode     string `json:"mode"`
	Switches int    `json:"switches"`
}

type ctrlSwitchResp struct {
	OldEmail string `json:"old_email"`
	NewEmail string `json:"new_email"`
	Mode     string `json:"mode"`
	Switches int    `json:"switches"`
	Error    string `json:"error"`
}

func directLoopbackClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, // Always connect directly to 127.0.0.1 control endpoint
		},
	}
}

func queryLiveCtrlStatus() (*ctrlStatusResp, bool) {
	ctrlURL := strings.TrimSpace(os.Getenv(vault.CtrlURLEnvKey))
	ctrlTok := strings.TrimSpace(os.Getenv(vault.CtrlTokenEnvKey))
	if ctrlURL == "" || ctrlTok == "" {
		return nil, false
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(ctrlURL, "/")+"/_agy_pro/status", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+ctrlTok)
	resp, err := directLoopbackClient(3 * time.Second).Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var out ctrlStatusResp
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Email == "" {
		return nil, false
	}
	return &out, true
}

func callLiveCtrlSwitch(target string) (*ctrlSwitchResp, bool, error) {
	ctrlURL := strings.TrimSpace(os.Getenv(vault.CtrlURLEnvKey))
	ctrlTok := strings.TrimSpace(os.Getenv(vault.CtrlTokenEnvKey))
	if ctrlURL == "" || ctrlTok == "" {
		return nil, false, nil
	}
	payload, _ := json.Marshal(map[string]string{"target": target})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(ctrlURL, "/")+"/_agy_pro/switch", bytes.NewReader(payload))
	if err != nil {
		return nil, true, err
	}
	req.Header.Set("Authorization", "Bearer "+ctrlTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := directLoopbackClient(28 * time.Second).Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	var out ctrlSwitchResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("control API HTTP %d", resp.StatusCode)
		}
		return &out, true, fmt.Errorf("%s", msg)
	}
	return &out, true, nil
}

func isZH() bool {
	return i18n.Lang() == "zh"
}

func plainBar(used *float64, width int) string {
	if used == nil {
		return strings.Repeat("─", width) + "    -"
	}
	v := *used
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	filled := int(math.Round(v / 100.0 * float64(width)))
	if v > 0 && filled == 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return fmt.Sprintf("%s%s %3.0f%%", strings.Repeat("━", filled), strings.Repeat("─", width-filled), v)
}

func mdBar(used *float64, width int) string {
	return "`" + plainBar(used, width) + "`"
}

func compactPct(used *float64) string {
	if used == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *used)
}

type dashArgs struct {
	doRefresh          bool
	masked             bool
	view               string
	usageRange         usage.RangeKind
	explicitUsageRange bool
}

func parseDashArgs(args []string) dashArgs {
	da := dashArgs{
		view:       "pool",
		usageRange: usage.RangeToday,
	}
	maskFlag := false
	for _, a := range args {
		low := strings.ToLower(strings.TrimSpace(a))
		switch low {
		case "-h", "--help", "help":
			da.view = "help"
		case "-r", "--refresh", "refresh":
			da.doRefresh = true
		case "--mask", "-m", "mask":
			maskFlag = true
		case "usage", "tokens":
			da.view = "usage"
		case "today", "--today":
			da.view = "usage"
			da.usageRange = usage.RangeToday
			da.explicitUsageRange = true
		case "7d", "--7d", "week":
			da.view = "usage"
			da.usageRange = usage.Range7Days
			da.explicitUsageRange = true
		case "all", "--all":
			da.view = "usage"
			da.usageRange = usage.RangeAll
			da.explicitUsageRange = true
		case "open", "sessions", "live":
			da.view = "open"
		}
	}
	da.masked = maskFlag || ui.MaskEnabled()
	return da
}

type poolSnapshot struct {
	accounts   map[string]*vault.Account
	st         *vault.State
	order      []string
	quotas     map[string]*vault.Quota
	now        float64
	liveEmail  string
	modeTag    string
	switches   int
	readyCount int
	oldestAt   float64
}

func loadPoolSnapshot(v *vault.Vault, agyBin string, doRefresh bool) *poolSnapshot {
	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	if len(order) == 0 {
		return &poolSnapshot{accounts: accounts, st: st}
	}

	if doRefresh {
		targets := make([]*vault.Account, 0, len(order))
		for _, e := range order {
			targets = append(targets, accounts[e])
		}
		fresh := quota.RefreshAll(targets, agyBin, 8, 30*time.Second, nil)
		_ = v.UpdateState(func(s *vault.State) {
			for e, q := range fresh {
				if q == nil {
					continue
				}
				if q.Error != "" && q.H5Used == nil && q.D7Used == nil && s.Quotas[e] != nil {
					cp := *s.Quotas[e]
					cp.Error = q.Error
					s.Quotas[e] = &cp
				} else {
					s.Quotas[e] = q
				}
				router.ReconcileCooldownWithQuota(s, e, q)
			}
		})
		st = v.LoadState()
	}

	now := float64(time.Now().UnixNano()) / 1e9
	router.PruneExpiredCooldowns(st, now)
	quotas := quota.Effective(st, now)

	liveEmail := st.Active
	modeTag := "direct"
	switches := 0
	if cs, ok := queryLiveCtrlStatus(); ok {
		liveEmail = cs.Email
		modeTag = cs.Mode
		switches = cs.Switches
	}
	if _, ok := accounts[liveEmail]; !ok {
		liveEmail = order[0]
	}

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

	return &poolSnapshot{
		accounts:   accounts,
		st:         st,
		order:      order,
		quotas:     quotas,
		now:        now,
		liveEmail:  liveEmail,
		modeTag:    modeTag,
		switches:   switches,
		readyCount: readyCount,
		oldestAt:   oldestAt,
	}
}

func accountRowInfo(snap *poolSnapshot, e string) (isActive bool, g5, g7, claude, status, reset string) {
	isActive = e == snap.liveEmail
	q := snap.quotas[e]
	ok, _ := router.AccountAvailability(e, snap.st, snap.quotas, snap.now)
	kind := router.StatusKind(e, snap.st, snap.quotas, snap.now)

	if q != nil {
		g5 = plainBar(q.H5Used, 6)
		g7 = plainBar(q.D7Used, 6)
		claude = fmt.Sprintf("%s/%s", compactPct(q.C5Used), compactPct(q.C7Used))
		switch {
		case kind == "7d" && q.D7Reset > 0:
			reset = vault.FmtLocalTime(q.D7Reset)
		case kind == "5h" && q.H5Reset > 0:
			reset = vault.FmtLocalTime(q.H5Reset)
		case q.D7Reset > 0 && q.D7Used != nil && *q.D7Used > 0:
			reset = vault.FmtLocalTime(q.D7Reset)
		case q.H5Reset > 0 && q.H5Used != nil && *q.H5Used > 0:
			reset = vault.FmtLocalTime(q.H5Reset)
		default:
			reset = "-"
		}
	} else {
		g5 = plainBar(nil, 6)
		g7 = plainBar(nil, 6)
		claude = "-/-"
		reset = "-"
	}

	switch {
	case !ok && kind == "7d":
		status = "7D FULL"
	case !ok && kind == "5h":
		status = "5H FULL"
	case !ok:
		if cd := snap.st.Cooldowns[e]; cd != nil && cd.Until > snap.now {
			status = fmt.Sprintf("COOL %s", vault.FmtDuration(cd.Until-snap.now))
		} else {
			status = "COOLING"
		}
	case q != nil && q.Error != "" && q.H5Used == nil && q.D7Used == nil:
		status = "! ERR"
	case isActive:
		status = "ACTIVE"
	default:
		status = "READY"
	}
	return
}

// renderPlainTable formats a crisp zero-ANSI CJK-aligned monospace table.
func renderPlainTable(headers []string, rows [][]string, aligns []ui.Align) string {
	ncols := len(headers)
	widths := make([]int, ncols)
	for c, h := range headers {
		widths[c] = ui.DisplayWidth(h)
	}
	for _, r := range rows {
		for c := 0; c < ncols && c < len(r); c++ {
			if w := ui.DisplayWidth(r[c]); w > widths[c] {
				widths[c] = w
			}
		}
	}
	totalW := 0
	for c, w := range widths {
		totalW += w
		if c < ncols-1 {
			totalW += 2
		}
	}
	rule := strings.Repeat("─", totalW)

	var b strings.Builder
	b.WriteString(rule + "\n")
	for c, h := range headers {
		al := ui.AlignLeft
		if c < len(aligns) {
			al = aligns[c]
		}
		b.WriteString(ui.Pad(h, widths[c], al))
		if c < ncols-1 {
			b.WriteString("  ")
		}
	}
	b.WriteByte('\n')
	b.WriteString(rule + "\n")
	for _, r := range rows {
		for c := 0; c < ncols; c++ {
			cell := ""
			if c < len(r) {
				cell = r[c]
			}
			al := ui.AlignLeft
			if c < len(aligns) {
				al = aligns[c]
			}
			b.WriteString(ui.Pad(cell, widths[c], al))
			if c < ncols-1 {
				b.WriteString("  ")
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString(rule)
	return b.String()
}

// --- Plain Monospace Renderers (default 0-token PreInvocation mode) ---

func handleDashPlain(v *vault.Vault, agyBin string, args []string) string {
	da := parseDashArgs(args)
	switch da.view {
	case "help":
		return renderDashHelpPlain()
	case "usage":
		return renderDashUsagePlain(da.usageRange, da.explicitUsageRange)
	case "open":
		return renderDashOpenPlain(v, da.masked)
	default:
		return renderDashPoolPlain(v, agyBin, da.doRefresh, da.masked)
	}
}

func renderDashHelpPlain() string {
	if isZH() {
		return strings.Join([]string{
			fmt.Sprintf("agy-pro v%s · 对话内快捷命令（0 Token 零中断）", vault.Version),
			strings.Repeat("─", 64),
			"  /dash               查看号池双池额度、当前打开终端与今日用量",
			"  /dash -r            在线并发刷新全部账号最新额度",
			"  /dash usage|7d|all  查看按模型与 Top 会话拆分的 Token 消耗",
			"  /dash open          查看本机当前打开的所有 agy 终端会话",
			"  /dash --mask        隐私打码显示邮箱（适合截屏分享）",
			"  /next               当前会话无感热切到下一个可用账号",
			"  /next <关键词>      当前会话无感热切到指定账号（支持邮箱片段）",
		}, "\n")
	}
	return strings.Join([]string{
		fmt.Sprintf("agy-pro v%s · In-Session Slash Commands (0 tokens, zero restart)", vault.Version),
		strings.Repeat("─", 68),
		"  /dash               Live pool quotas, open sessions & today usage",
		"  /dash -r            Concurrently refresh live quotas across all accounts",
		"  /dash usage|7d|all  Token breakdown by model and top sessions",
		"  /dash open          List all currently open agy terminal windows",
		"  /dash --mask        Mask account emails for safe screenshots",
		"  /next               Hot-swap live session to the next ready account",
		"  /next <keyword>     Hot-swap live session to a specific account",
	}, "\n")
}

func renderDashPoolPlain(v *vault.Vault, agyBin string, doRefresh, masked bool) string {
	snap := loadPoolSnapshot(v, agyBin, doRefresh)
	if len(snap.order) == 0 {
		if isZH() {
			return "agy-pro · 号池为空，请在终端运行 agy-pro auth login 或 agy-pro auth import。"
		}
		return "agy-pro · Account pool is empty. Run `agy-pro auth login` or `agy-pro auth import`."
	}

	headMeta := []string{
		fmt.Sprintf("%d/%d ready", snap.readyCount, len(snap.order)),
		snap.modeTag,
		"compact " + vault.DisplayAutoCompact(snap.st),
	}
	if snap.st.ReservePct > 0 {
		headMeta = append(headMeta, fmt.Sprintf("reserve %.0f%%", snap.st.ReservePct))
	}
	if snap.oldestAt > 0 {
		headMeta = append(headMeta, "cache "+vault.FmtAge(snap.oldestAt, snap.now))
	}
	if snap.switches > 0 {
		if isZH() {
			headMeta = append(headMeta, fmt.Sprintf("已热切 %d 次", snap.switches))
		} else {
			headMeta = append(headMeta, fmt.Sprintf("%d switches", snap.switches))
		}
	}

	headers := []string{"", "ACCOUNT", "GEMINI 5H", "GEMINI 7D", "CLAUDE", "STATUS", "RESET"}
	if isZH() {
		headers = []string{"", "账号", "GEMINI 5H", "GEMINI 7D", "CLAUDE", "状态", "重置"}
	}
	aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignLeft, ui.AlignLeft}

	rows := make([][]string, 0, len(snap.order))
	for _, e := range snap.order {
		isActive, g5, g7, claude, stText, resetText := accountRowInfo(snap, e)
		dot := "○"
		if isActive {
			dot = "●"
		}
		shown := ui.Truncate(ui.DisplayEmail(e, masked), 28)
		rows = append(rows, []string{dot, shown, g5, g7, claude, stText, resetText})
	}

	openList, _ := sessions.ListOpen("", "")
	sc := usage.NewScanner("", "")
	rep := sc.Scan(usage.RangeToday, time.Now())
	if rep.Total.Calls == 0 {
		rep = sc.Scan(usage.Range7Days, time.Now())
	}

	var b strings.Builder
	fmt.Fprintf(&b, "agy-pro v%s · %s\n", vault.Version, strings.Join(headMeta, " · "))
	b.WriteString(renderPlainTable(headers, rows, aligns))
	b.WriteByte('\n')
	if isZH() {
		fmt.Fprintf(&b, "当前打开: %d 个终端 · %s消耗: %s tokens (%d 次调用 · 缓存命中 %.1f%%)\n",
			len(openList), rep.Range.Label(), usage.FmtTokens(rep.Total.TotalTokens()), rep.Total.Calls, rep.Total.CacheHitPct())
		b.WriteString("命令: /dash -r 刷新 · /dash usage 用量 · /dash open 会话 · /next [关键词] 热切账号")
	} else {
		fmt.Fprintf(&b, "Open sessions: %d · %s usage: %s tokens (%d calls · %.1f%% cache hit)\n",
			len(openList), rep.Range.Label(), usage.FmtTokens(rep.Total.TotalTokens()), rep.Total.Calls, rep.Total.CacheHitPct())
		b.WriteString("Commands: /dash -r refresh · /dash usage breakdown · /dash open windows · /next [keyword] hot-swap")
	}
	return b.String()
}

func renderDashUsagePlain(rk usage.RangeKind, explicit bool) string {
	sc := usage.NewScanner("", "")
	now := time.Now()
	rep := sc.Scan(rk, now)
	if !explicit && rk == usage.RangeToday && rep.Total.Calls == 0 {
		rk = usage.Range7Days
		rep = sc.Scan(rk, now)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "agy-pro v%s · %s (%dms)\n", vault.Version, rk.Label(), rep.ElapsedMs)
	if rep.Total.Calls == 0 {
		if isZH() {
			fmt.Fprintf(&b, "%s暂无本地 LLM 调用记录（可试试 /dash 7d 或 /dash all）", rk.Label())
		} else {
			fmt.Fprintf(&b, "No local LLM calls found for %s (try /dash 7d or /dash all)", rk.Label())
		}
		return b.String()
	}

	tot := rep.Total
	if isZH() {
		fmt.Fprintf(&b, "总消耗 %s tokens · %d 次调用 · %d 会话 · 输入 %s · 缓存读 %s (%.1f%%) · 输出 %s\n",
			usage.FmtTokens(tot.TotalTokens()), tot.Calls, rep.ActiveConvs,
			usage.FmtTokens(tot.Input), usage.FmtTokens(tot.CacheRead), tot.CacheHitPct(), usage.FmtTokens(tot.Output))
	} else {
		fmt.Fprintf(&b, "Total %s tokens · %d calls · %d sessions · In %s · Cache %s (%.1f%%) · Out %s\n",
			usage.FmtTokens(tot.TotalTokens()), tot.Calls, rep.ActiveConvs,
			usage.FmtTokens(tot.Input), usage.FmtTokens(tot.CacheRead), tot.CacheHitPct(), usage.FmtTokens(tot.Output))
	}

	headers := []string{"MODEL", "CALLS", "TOTAL", "IN", "CACHE", "HIT%", "OUT", "SHARE"}
	if isZH() {
		headers = []string{"模型", "调用", "总计", "输入", "缓存读", "命中", "输出", "占比"}
	}
	aligns := []ui.Align{ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignLeft}

	displayModels := usage.RollupModels(rep.ByModel, 6, func(n int) string {
		return i18n.T("usage.other_models", n)
	})
	grand := float64(tot.TotalTokens())
	if grand <= 0 {
		grand = 1
	}
	mRows := make([][]string, 0, len(displayModels))
	for _, mr := range displayModels {
		share := float64(mr.Stats.TotalTokens()) * 100.0 / grand
		sPtr := share
		mRows = append(mRows, []string{
			ui.Truncate(mr.Model, 24),
			fmt.Sprintf("%d", mr.Stats.Calls),
			usage.FmtTokens(mr.Stats.TotalTokens()),
			usage.FmtTokens(mr.Stats.Input),
			usage.FmtTokens(mr.Stats.CacheRead),
			fmt.Sprintf("%.0f%%", mr.Stats.CacheHitPct()),
			usage.FmtTokens(mr.Stats.Output),
			plainBar(&sPtr, 5),
		})
	}
	b.WriteString(renderPlainTable(headers, mRows, aligns))

	if len(rep.BySession) > 0 {
		b.WriteString("\n\n")
		sHeaders := []string{"TOP SESSIONS", "CALLS", "TOTAL", "HIT%", "OUT"}
		if isZH() {
			sHeaders = []string{"TOP 会话", "调用", "总计", "命中", "输出"}
		}
		sAligns := []ui.Align{ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight}
		maxS := 5
		if len(rep.BySession) < maxS {
			maxS = len(rep.BySession)
		}
		sRows := make([][]string, 0, maxS)
		for i := 0; i < maxS; i++ {
			sr := rep.BySession[i]
			sRows = append(sRows, []string{
				ui.Truncate(sr.Title, 40),
				fmt.Sprintf("%d", sr.Stats.Calls),
				usage.FmtTokens(sr.Stats.TotalTokens()),
				fmt.Sprintf("%.0f%%", sr.Stats.CacheHitPct()),
				usage.FmtTokens(sr.Stats.Output),
			})
		}
		b.WriteString(renderPlainTable(sHeaders, sRows, sAligns))
	}
	b.WriteString("\n/dash 号池 · /dash today · /dash 7d · /dash all · /dash open")
	return b.String()
}

func renderDashOpenPlain(v *vault.Vault, masked bool) string {
	openList, _ := sessions.ListOpen("", "")
	liveMeta := v.ListLiveSessions()
	home := vault.HomeDir()
	now := time.Now()

	var b strings.Builder
	if isZH() {
		fmt.Fprintf(&b, "agy-pro v%s · 当前打开的终端会话 (%d)\n", vault.Version, len(openList))
	} else {
		fmt.Fprintf(&b, "agy-pro v%s · Open agy Sessions (%d)\n", vault.Version, len(openList))
	}
	if len(openList) == 0 {
		if isZH() {
			b.WriteString("当前本机未检测到正在打开的 agy 会话。")
		} else {
			b.WriteString("No open agy sessions detected on this machine.")
		}
		return b.String()
	}

	headers := []string{"STATE", "SESSION", "ACCOUNT", "TTY", "WORKSPACE", "STEPS", "ACTIVE"}
	if isZH() {
		headers = []string{"状态", "会话", "账号", "终端", "工作目录", "步数", "活跃"}
	}
	aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight}
	rows := make([][]string, 0, len(openList))
	for _, s := range openList {
		st := "● open"
		if s.Running {
			st = "⠋ run"
		}
		acct := "native"
		tty := "-"
		if lm, ok := liveMeta[s.ID]; ok {
			if lm.Email != "" {
				acct = ui.Truncate(ui.DisplayEmail(lm.Email, masked), 24)
			}
			if lm.TTY != "" {
				tty = lm.TTY
			}
		}
		rows = append(rows, []string{
			st,
			ui.Truncate(s.Title, 32),
			acct,
			tty,
			ui.Truncate(s.PrimaryWorkspace(home), 22),
			fmt.Sprintf("%d", s.Steps),
			sessions.FmtRelativeTime(s.ModifiedAt, now),
		})
	}
	b.WriteString(renderPlainTable(headers, rows, aligns))
	b.WriteString("\n/dash 号池 · /dash usage 用量 · /next 热切账号")
	return b.String()
}

type nextResult struct {
	live     bool
	oldEmail string
	newEmail string
	masked   bool
	errMsg   string
}

func executeNext(v *vault.Vault, args []string) nextResult {
	var target string
	maskFlag := false
	for _, a := range args {
		if a == "--mask" || a == "-m" {
			maskFlag = true
			continue
		}
		if !strings.HasPrefix(a, "-") && target == "" {
			target = a
		}
	}
	masked := maskFlag || ui.MaskEnabled()

	// Fast path: live in-session hot-swap via seamless proxy loopback control API!
	if res, hasCtrl, err := callLiveCtrlSwitch(target); hasCtrl {
		if err != nil {
			return nextResult{live: true, masked: masked, errMsg: err.Error()}
		}
		return nextResult{
			live:     true,
			oldEmail: res.OldEmail,
			newEmail: res.NewEmail,
			masked:   masked,
		}
	}

	// Fallback (--no-seamless direct mode): update state.json + active token file for next launch
	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	if len(order) == 0 {
		if isZH() {
			return nextResult{masked: masked, errMsg: "号池为空，请先运行 agy-pro auth login。"}
		}
		return nextResult{masked: masked, errMsg: "Account pool is empty. Run `agy-pro auth login` first."}
	}

	now := float64(time.Now().UnixNano()) / 1e9
	quotas := quota.Effective(st, now)
	oldEmail := st.Active
	var nextEmail string

	if target == "" {
		nxt, msg := router.PickDrainWithReserve(order, st, quotas, true, st.ReservePct, now)
		if nxt == "" {
			return nextResult{masked: masked, errMsg: msg}
		}
		nextEmail = nxt
	} else {
		q := strings.ToLower(strings.TrimSpace(target))
		var hits []string
		for _, e := range order {
			if e == q {
				hits = []string{e}
				break
			}
			if strings.Contains(e, q) {
				hits = append(hits, e)
			}
		}
		if len(hits) != 1 {
			if isZH() {
				return nextResult{masked: masked, errMsg: fmt.Sprintf("未唯一匹配到账号 %q（匹配 %d 个）", target, len(hits))}
			}
			return nextResult{masked: masked, errMsg: fmt.Sprintf("Could not uniquely match %q (%d matches)", target, len(hits))}
		}
		nextEmail = hits[0]
	}

	_ = v.UpdateState(func(s *vault.State) {
		s.Active = nextEmail
		s.Order = order
		if target != "" {
			delete(s.Cooldowns, nextEmail)
		}
	})
	if acct := accounts[nextEmail]; acct != nil {
		_ = v.ActivateToken(acct)
	}
	return nextResult{
		live:     false,
		oldEmail: oldEmail,
		newEmail: nextEmail,
		masked:   masked,
	}
}

func handleNextPlain(v *vault.Vault, args []string) string {
	r := executeNext(v, args)
	if r.errMsg != "" {
		if isZH() {
			return fmt.Sprintf("agy-pro · 热切账号失败: %s", r.errMsg)
		}
		return fmt.Sprintf("agy-pro · Live account switch failed: %s", r.errMsg)
	}
	oldDisp := ui.DisplayEmail(r.oldEmail, r.masked)
	newDisp := ui.DisplayEmail(r.newEmail, r.masked)
	qNote := formatAccountQuotaPlain(v, r.newEmail)
	if r.live {
		if isZH() {
			return fmt.Sprintf("agy-pro · ✓ 已在当前会话无感热切账号: %s → %s%s\n零中断立即生效，后续对话将直接使用新账号（输入 /dash 查看完整号池）", oldDisp, newDisp, qNote)
		}
		return fmt.Sprintf("agy-pro · ✓ Hot-swapped live session account: %s → %s%s\nActive immediately with zero interruption (run /dash to inspect the pool).", oldDisp, newDisp, qNote)
	}
	if isZH() {
		return fmt.Sprintf("agy-pro · ✓ 默认主号已切换: %s → %s%s\n当前会话处于 --no-seamless 纯直连模式，将于下次启动生效（开启默认热切即可对话内即时生效）。", oldDisp, newDisp, qNote)
	}
	return fmt.Sprintf("agy-pro · ✓ Default active account switched: %s → %s%s\nRunning in --no-seamless direct mode; takes effect on next launch.", oldDisp, newDisp, qNote)
}

func formatAccountQuotaPlain(v *vault.Vault, email string) string {
	st := v.LoadState()
	now := float64(time.Now().UnixNano()) / 1e9
	q := quota.Effective(st, now)[email]
	if q == nil {
		return ""
	}
	return fmt.Sprintf(" · 5h %s · 7d %s", plainBar(q.H5Used, 5), plainBar(q.D7Used, 5))
}

// --- Markdown Renderers (opt-in via AGY_PRO_HOOK_RENDER=markdown once upstream user_markdown is fixed) ---

func handleDashMarkdown(v *vault.Vault, agyBin string, args []string) string {
	da := parseDashArgs(args)
	switch da.view {
	case "help":
		return "```\n" + renderDashHelpPlain() + "\n```\n"
	case "usage":
		return renderDashUsageMD(da.usageRange, da.explicitUsageRange)
	case "open":
		return renderDashOpenMD(v, da.masked)
	default:
		return renderDashPoolMD(v, agyBin, da.doRefresh, da.masked)
	}
}

func renderDashPoolMD(v *vault.Vault, agyBin string, doRefresh, masked bool) string {
	snap := loadPoolSnapshot(v, agyBin, doRefresh)
	if len(snap.order) == 0 {
		if isZH() {
			return "**agy-pro** · 号池为空，请在终端运行 `agy-pro auth login` 或 `agy-pro auth import`。"
		}
		return "**agy-pro** · Account pool is empty. Run `agy-pro auth login` or `agy-pro auth import` in your terminal."
	}

	var b strings.Builder
	headMeta := []string{
		fmt.Sprintf("`%s`", snap.modeTag),
		fmt.Sprintf("compact `%s`", vault.DisplayAutoCompact(snap.st)),
	}
	if snap.st.ReservePct > 0 {
		headMeta = append(headMeta, fmt.Sprintf("reserve `%.0f%%`", snap.st.ReservePct))
	}
	if snap.oldestAt > 0 {
		headMeta = append(headMeta, fmt.Sprintf("cache %s", vault.FmtAge(snap.oldestAt, snap.now)))
	}
	if snap.switches > 0 {
		if isZH() {
			headMeta = append(headMeta, fmt.Sprintf("已热切 %d 次", snap.switches))
		} else {
			headMeta = append(headMeta, fmt.Sprintf("%d switches", snap.switches))
		}
	}

	fmt.Fprintf(&b, "### ✦ agy-pro `v%s` · **%d/%d ready** · %s\n\n",
		vault.Version, snap.readyCount, len(snap.order), strings.Join(headMeta, " · "))

	if isZH() {
		b.WriteString("| | 账号 | Gemini 5h | Gemini 7d | Claude | 状态 | 重置 |\n")
	} else {
		b.WriteString("| | Account | Gemini 5h | Gemini 7d | Claude | Status | Reset |\n")
	}
	b.WriteString("| :-: | :--- | :--- | :--- | ---: | :--- | :--- |\n")

	for _, e := range snap.order {
		isActive, _, _, claude, stText, resetText := accountRowInfo(snap, e)
		q := snap.quotas[e]
		dot := "○"
		shown := ui.DisplayEmail(e, masked)
		acctCell := fmt.Sprintf("`%s`", shown)
		if isActive {
			dot = "●"
			acctCell = fmt.Sprintf("**`%s`**", shown)
			stText = "**ACTIVE**"
		} else if stText == "7D FULL" || stText == "5H FULL" {
			stText = "**" + stText + "**"
		}
		var g5, g7 string
		if q != nil {
			g5 = mdBar(q.H5Used, 6)
			g7 = mdBar(q.D7Used, 6)
		} else {
			g5 = mdBar(nil, 6)
			g7 = mdBar(nil, 6)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` | %s | %s |\n",
			dot, acctCell, g5, g7, claude, stText, resetText)
	}

	openList, _ := sessions.ListOpen("", "")
	sc := usage.NewScanner("", "")
	rep := sc.Scan(usage.RangeToday, time.Now())
	if rep.Total.Calls == 0 {
		rep = sc.Scan(usage.Range7Days, time.Now())
	}

	b.WriteByte('\n')
	if isZH() {
		fmt.Fprintf(&b, "> **当前打开**: `%d` 个终端 · **%s消耗**: `%s` tokens（%d 次调用，缓存命中 `%.1f%%`）\n>\n",
			len(openList), rep.Range.Label(), usage.FmtTokens(rep.Total.TotalTokens()), rep.Total.Calls, rep.Total.CacheHitPct())
		b.WriteString("> 快捷命令：`/dash -r` 刷新额度 · `/dash usage` 用量详情 · `/dash open` 打开会话 · `/next` 热切下个号 · `/next <关键词>` 指定热切\n")
	} else {
		fmt.Fprintf(&b, "> **Open sessions**: `%d` · **%s usage**: `%s` tokens (%d calls, `%.1f%%` cache hit)\n>\n",
			len(openList), rep.Range.Label(), usage.FmtTokens(rep.Total.TotalTokens()), rep.Total.Calls, rep.Total.CacheHitPct())
		b.WriteString("> Commands: `/dash -r` live refresh · `/dash usage` token breakdown · `/dash open` open windows · `/next [keyword]` hot-swap account\n")
	}
	return b.String()
}

func renderDashUsageMD(rk usage.RangeKind, explicit bool) string {
	sc := usage.NewScanner("", "")
	now := time.Now()
	rep := sc.Scan(rk, now)
	if !explicit && rk == usage.RangeToday && rep.Total.Calls == 0 {
		rk = usage.Range7Days
		rep = sc.Scan(rk, now)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "### ✦ agy-pro · %s (`%dms`)\n\n", rk.Label(), rep.ElapsedMs)
	if rep.Total.Calls == 0 {
		if isZH() {
			fmt.Fprintf(&b, "%s暂无本地 LLM 调用记录（可试试 `/dash 7d` 或 `/dash all`）。\n", rk.Label())
		} else {
			fmt.Fprintf(&b, "No local LLM calls found for %s (try `/dash 7d` or `/dash all`).\n", rk.Label())
		}
		return b.String()
	}

	tot := rep.Total
	if isZH() {
		fmt.Fprintf(&b, "**总消耗** `%s` tokens · `%d` 次调用 · `%d` 个会话 · 未缓存输入 `%s` · 缓存读 `%s` (`%.1f%%`) · 输出 `%s`\n\n",
			usage.FmtTokens(tot.TotalTokens()), tot.Calls, rep.ActiveConvs,
			usage.FmtTokens(tot.Input), usage.FmtTokens(tot.CacheRead), tot.CacheHitPct(), usage.FmtTokens(tot.Output))
		b.WriteString("| 模型 | 调用 | 总计 | 输入 | 缓存读 | 命中率 | 输出 | 占比 |\n")
	} else {
		fmt.Fprintf(&b, "**Total** `%s` tokens · `%d` calls · `%d` sessions · Input `%s` · Cache Read `%s` (`%.1f%%`) · Output `%s`\n\n",
			usage.FmtTokens(tot.TotalTokens()), tot.Calls, rep.ActiveConvs,
			usage.FmtTokens(tot.Input), usage.FmtTokens(tot.CacheRead), tot.CacheHitPct(), usage.FmtTokens(tot.Output))
		b.WriteString("| Model | Calls | Total | Input | Cache | Hit% | Output | Share |\n")
	}
	b.WriteString("| :--- | ---: | ---: | ---: | ---: | ---: | ---: | :--- |\n")

	displayModels := usage.RollupModels(rep.ByModel, 6, func(n int) string {
		return i18n.T("usage.other_models", n)
	})
	grand := float64(tot.TotalTokens())
	if grand <= 0 {
		grand = 1
	}
	for _, mr := range displayModels {
		share := float64(mr.Stats.TotalTokens()) * 100.0 / grand
		sPtr := share
		fmt.Fprintf(&b, "| `%s` | %d | **%s** | %s | %s | %.0f%% | %s | %s |\n",
			mr.Model, mr.Stats.Calls, usage.FmtTokens(mr.Stats.TotalTokens()),
			usage.FmtTokens(mr.Stats.Input), usage.FmtTokens(mr.Stats.CacheRead),
			mr.Stats.CacheHitPct(), usage.FmtTokens(mr.Stats.Output), mdBar(&sPtr, 5))
	}

	if len(rep.BySession) > 0 {
		b.WriteByte('\n')
		if isZH() {
			b.WriteString("| Top 会话 | 调用 | 总计 | 命中率 | 输出 |\n")
		} else {
			b.WriteString("| Top Sessions | Calls | Total | Hit% | Output |\n")
		}
		b.WriteString("| :--- | ---: | ---: | ---: | ---: |\n")
		maxS := 5
		if len(rep.BySession) < maxS {
			maxS = len(rep.BySession)
		}
		for i := 0; i < maxS; i++ {
			sr := rep.BySession[i]
			title := strings.ReplaceAll(ui.Truncate(sr.Title, 44), "|", "/")
			fmt.Fprintf(&b, "| %s | %d | **%s** | %.0f%% | %s |\n",
				title, sr.Stats.Calls, usage.FmtTokens(sr.Stats.TotalTokens()),
				sr.Stats.CacheHitPct(), usage.FmtTokens(sr.Stats.Output))
		}
	}
	b.WriteString("\n> `/dash` 号池 · `/dash today` · `/dash 7d` · `/dash all` · `/dash open`\n")
	return b.String()
}

func renderDashOpenMD(v *vault.Vault, masked bool) string {
	openList, _ := sessions.ListOpen("", "")
	liveMeta := v.ListLiveSessions()
	home := vault.HomeDir()
	now := time.Now()

	var b strings.Builder
	if isZH() {
		fmt.Fprintf(&b, "### ✦ agy-pro · 当前打开的终端会话（`%d`）\n\n", len(openList))
	} else {
		fmt.Fprintf(&b, "### ✦ agy-pro · Open `agy` Sessions (`%d`)\n\n", len(openList))
	}
	if len(openList) == 0 {
		if isZH() {
			b.WriteString("当前本机未检测到正在打开的 `agy` 会话。\n")
		} else {
			b.WriteString("No open `agy` sessions detected on this machine.\n")
		}
		return b.String()
	}

	if isZH() {
		b.WriteString("| 状态 | 会话 | 账号 | 终端 | 工作目录 | 步数 | 活跃 |\n")
	} else {
		b.WriteString("| State | Session | Account | TTY | Workspace | Steps | Active |\n")
	}
	b.WriteString("| :--- | :--- | :--- | :--- | :--- | ---: | ---: |\n")
	for _, s := range openList {
		st := "● open"
		if s.Running {
			st = "**⠋ run**"
		}
		acct := "native"
		tty := "-"
		if lm, ok := liveMeta[s.ID]; ok {
			if lm.Email != "" {
				acct = fmt.Sprintf("`%s`", ui.DisplayEmail(lm.Email, masked))
			}
			if lm.TTY != "" {
				tty = fmt.Sprintf("`%s`", lm.TTY)
			}
		}
		title := strings.ReplaceAll(ui.Truncate(s.Title, 38), "|", "/")
		ws := strings.ReplaceAll(ui.Truncate(s.PrimaryWorkspace(home), 26), "|", "/")
		fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` | %d | %s |\n",
			st, title, acct, tty, ws, s.Steps, sessions.FmtRelativeTime(s.ModifiedAt, now))
	}
	b.WriteString("\n> `/dash` 号池 · `/dash usage` · `/next`\n")
	return b.String()
}

func handleNextMarkdown(v *vault.Vault, args []string) string {
	r := executeNext(v, args)
	if r.errMsg != "" {
		if isZH() {
			return fmt.Sprintf("⚠️ **agy-pro** · 热切账号失败：%s", r.errMsg)
		}
		return fmt.Sprintf("⚠️ **agy-pro** · Live account switch failed: %s", r.errMsg)
	}
	oldDisp := ui.DisplayEmail(r.oldEmail, r.masked)
	newDisp := ui.DisplayEmail(r.newEmail, r.masked)
	quotaNote := formatAccountQuotaMD(v, r.newEmail)
	if r.live {
		if isZH() {
			return fmt.Sprintf("✓ **agy-pro** · 已在当前会话**无感热切账号**：`%s` → **`%s`**%s\n\n> 零中断立即生效，后续对话将直接使用新账号（输入 `/dash` 查看完整号池）。", oldDisp, newDisp, quotaNote)
		}
		return fmt.Sprintf("✓ **agy-pro** · Hot-swapped live session account: `%s` → **`%s`**%s\n\n> Active immediately with zero interruption (run `/dash` to inspect the pool).", oldDisp, newDisp, quotaNote)
	}
	if isZH() {
		return fmt.Sprintf("✓ **agy-pro** · 默认主号已切换：`%s` → **`%s`**%s\n\n> 当前会话处于 `--no-seamless` 纯直连模式，将于下次启动生效（开启默认热切即可对话内即时生效）。", oldDisp, newDisp, quotaNote)
	}
	return fmt.Sprintf("✓ **agy-pro** · Default active account switched: `%s` → **`%s`**%s\n\n> Running in `--no-seamless` direct mode; takes effect on next launch.", oldDisp, newDisp, quotaNote)
}

func formatAccountQuotaMD(v *vault.Vault, email string) string {
	st := v.LoadState()
	now := float64(time.Now().UnixNano()) / 1e9
	q := quota.Effective(st, now)[email]
	if q == nil {
		return ""
	}
	return fmt.Sprintf(" · 5h %s · 7d %s", mdBar(q.H5Used, 5), mdBar(q.D7Used, 5))
}
