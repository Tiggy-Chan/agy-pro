package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Tiggy-Chan/agy-pro/internal/quota"
	"github.com/Tiggy-Chan/agy-pro/internal/router"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionLaunch
	ActionLogin
)

type Result struct {
	Action ActionKind
	Email  string
}

type probeDoneMsg struct {
	email string
	q     *vault.Quota
}

type autoRefreshStartMsg struct{}

const (
	autoRefreshStaleAge = 180.0 // auto-refresh cached rows older than 3m when opening `agy-pro ui`
	maxConcurrentProbes = 8
)

type model struct {
	vault         *vault.Vault
	agyBin        string
	accounts      map[string]*vault.Account
	state         *vault.State
	order         []string
	quotas        map[string]*vault.Quota
	cursor        int
	width         int
	height        int
	spinner       spinner.Model
	refreshing     map[string]bool
	refreshQueue   []string
	autoRefreshOn  bool
	confirmDel     string
	toast          string
	toastStyle     lipgloss.Style
	result         Result
}

func newModel(v *vault.Vault, agyBin string) model {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = ui.SAccentB

	m := model{
		vault:         v,
		agyBin:        agyBin,
		spinner:       sp,
		refreshing:    map[string]bool{},
		autoRefreshOn: true,
		width:         ui.TerminalColumns(100),
		height:        24,
		toastStyle:    ui.SDim,
	}
	m.reload()
	for i, e := range m.order {
		if e == m.state.Active {
			m.cursor = i
			break
		}
	}
	return m
}

func (m *model) needsAutoRefresh(email string, now float64) bool {
	q := m.quotas[email]
	if q == nil || q.At <= 0 || q.Error != "" {
		return true
	}
	if now-q.At > autoRefreshStaleAge {
		return true
	}
	// Also re-check accounts marked exhausted/cooling so early official resets clear automatically
	if ok, _ := router.AccountAvailability(email, m.state, m.quotas, now); !ok && now-q.At > 60 {
		return true
	}
	return false
}

func (m *model) enqueueProbes(emails []string) []tea.Cmd {
	for _, e := range emails {
		if m.refreshing[e] || m.accounts[e] == nil {
			continue
		}
		queued := false
		for _, q := range m.refreshQueue {
			if q == e {
				queued = true
				break
			}
		}
		if !queued {
			m.refreshQueue = append(m.refreshQueue, e)
		}
	}
	return m.drainRefreshQueue()
}

func (m *model) drainRefreshQueue() []tea.Cmd {
	var cmds []tea.Cmd
	for len(m.refreshing) < maxConcurrentProbes && len(m.refreshQueue) > 0 {
		e := m.refreshQueue[0]
		m.refreshQueue = m.refreshQueue[1:]
		if m.refreshing[e] {
			continue
		}
		acct := m.accounts[e]
		if acct == nil {
			continue
		}
		m.refreshing[e] = true
		cmds = append(cmds, probeCmd(acct, m.agyBin))
	}
	return cmds
}

func (m *model) pendingRefreshCount() int {
	return len(m.refreshing) + len(m.refreshQueue)
}

func (m *model) reload() {
	m.accounts = m.vault.ListAccounts()
	m.state = m.vault.LoadState()
	m.order = m.vault.OrderedEmails(m.accounts, m.state)
	now := float64(time.Now().UnixNano()) / 1e9
	router.PruneExpiredCooldowns(m.state, now)
	m.quotas = quota.Effective(m.state, now)
	if m.cursor >= len(m.order) && len(m.order) > 0 {
		m.cursor = len(m.order) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m model) Init() tea.Cmd {
	if m.autoRefreshOn && len(m.order) > 0 {
		return tea.Batch(
			m.spinner.Tick,
			func() tea.Msg { return autoRefreshStartMsg{} },
		)
	}
	return m.spinner.Tick
}

func probeCmd(acct *vault.Account, agyBin string) tea.Cmd {
	return func() tea.Msg {
		q := quota.ProbeAccount(acct, agyBin, 35*time.Second)
		return probeDoneMsg{email: acct.Email, q: q}
	}
}

func mergeQuota(prev, fresh *vault.Quota) *vault.Quota {
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

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case autoRefreshStartMsg:
		if !m.autoRefreshOn || len(m.order) == 0 {
			return m, nil
		}
		now := float64(time.Now().UnixNano()) / 1e9
		var stale []string
		for _, e := range m.order {
			if m.needsAutoRefresh(e, now) {
				stale = append(stale, e)
			}
		}
		if cmds := m.enqueueProbes(stale); len(cmds) > 0 {
			m.toast = fmt.Sprintf("正在后台核对 %d 个账号实时额度...", m.pendingRefreshCount())
			m.toastStyle = ui.SHeadDim
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case probeDoneMsg:
		delete(m.refreshing, msg.email)
		clearedCooldown := false
		_ = m.vault.UpdateState(func(s *vault.State) {
			s.Quotas[msg.email] = mergeQuota(s.Quotas[msg.email], msg.q)
			clearedCooldown = router.ReconcileCooldownWithQuota(s, msg.email, msg.q)
		})
		m.reload()
		nextCmds := m.drainRefreshQueue()
		rem := m.pendingRefreshCount()
		if rem == 0 {
			if msg.q != nil && msg.q.Error != "" {
				m.toast = fmt.Sprintf("! %s: %s", msg.email, msg.q.Error)
				m.toastStyle = ui.SAmber
			} else if clearedCooldown {
				m.toast = fmt.Sprintf("✓ %s 额度已恢复（自动清除旧冷却）", msg.email)
				m.toastStyle = ui.SGood
			} else {
				m.toast = "✓ 额度已刷新"
				m.toastStyle = ui.SGood
			}
		} else {
			m.toast = fmt.Sprintf("正在刷新实时额度（剩余 %d 个）...", rem)
			m.toastStyle = ui.SHeadDim
		}
		if len(nextCmds) > 0 {
			return m, tea.Batch(nextCmds...)
		}
		return m, nil

	case tea.KeyMsg:
		k := msg.String()
		if m.confirmDel != "" {
			if k == "y" || k == "Y" || k == "d" || k == "enter" {
				target := m.confirmDel
				m.confirmDel = ""
				m.vault.RemoveAccount(target)
				m.reload()
				m.toast = fmt.Sprintf("✓ 已移除账号: %s", target)
				m.toastStyle = ui.SGood
				return m, nil
			}
			m.confirmDel = ""
			m.toast = "已取消移除"
			m.toastStyle = ui.SDim
			return m, nil
		}
		switch k {
		case "q", "esc", "ctrl+c":
			m.result = Result{Action: ActionNone}
			return m, tea.Quit

		case "up", "k":
			if len(m.order) > 0 && m.cursor > 0 {
				m.cursor--
			}
			return m, nil

		case "down", "j":
			if len(m.order) > 0 && m.cursor < len(m.order)-1 {
				m.cursor++
			}
			return m, nil

		case "K": // Move selected account up in drain priority
			if len(m.order) > 1 && m.cursor > 0 {
				m.order[m.cursor], m.order[m.cursor-1] = m.order[m.cursor-1], m.order[m.cursor]
				m.cursor--
				newOrd := append([]string(nil), m.order...)
				_ = m.vault.UpdateState(func(s *vault.State) { s.Order = newOrd })
				m.reload()
				m.toast = "✓ 已上移 drain 优先级"
				m.toastStyle = ui.SGood
			}
			return m, nil

		case "J": // Move selected account down in drain priority
			if len(m.order) > 1 && m.cursor < len(m.order)-1 {
				m.order[m.cursor], m.order[m.cursor+1] = m.order[m.cursor+1], m.order[m.cursor]
				m.cursor++
				newOrd := append([]string(nil), m.order...)
				_ = m.vault.UpdateState(func(s *vault.State) { s.Order = newOrd })
				m.reload()
				m.toast = "✓ 已下移 drain 优先级"
				m.toastStyle = ui.SGood
			}
			return m, nil

		case "enter":
			if len(m.order) > 0 {
				chosen := m.order[m.cursor]
				ord := append([]string(nil), m.order...)
				_ = m.vault.UpdateState(func(s *vault.State) {
					s.Active = chosen
					s.Order = ord
					delete(s.Cooldowns, chosen)
				})
				m.result = Result{Action: ActionLaunch, Email: chosen}
				return m, tea.Quit
			}
			return m, nil

		case "s", " ":
			if len(m.order) > 0 {
				chosen := m.order[m.cursor]
				ord := append([]string(nil), m.order...)
				_ = m.vault.UpdateState(func(s *vault.State) {
					s.Active = chosen
					s.Order = ord
					delete(s.Cooldowns, chosen)
				})
				m.reload()
				m.toast = fmt.Sprintf("✓ 已设 %s 为当前主号", chosen)
				m.toastStyle = ui.SGood
			}
			return m, nil

		case "r":
			if len(m.order) == 0 {
				return m, nil
			}
			cmds := m.enqueueProbes(m.order)
			if len(cmds) > 0 {
				m.toast = fmt.Sprintf("正在刷新 %d 个账号实时额度...", m.pendingRefreshCount())
				m.toastStyle = ui.SHeadDim
				return m, tea.Batch(cmds...)
			}
			return m, nil

		case "R":
			if len(m.order) > 0 {
				e := m.order[m.cursor]
				cmds := m.enqueueProbes([]string{e})
				if len(cmds) > 0 {
					m.toast = fmt.Sprintf("正在刷新 %s 实时额度...", e)
					m.toastStyle = ui.SHeadDim
					return m, tea.Batch(cmds...)
				}
			}
			return m, nil

		case "c":
			_ = m.vault.UpdateState(func(s *vault.State) {
				s.Cooldowns = map[string]*vault.Cooldown{}
				for e, q := range s.Quotas {
					if q != nil && ((q.H5Rem != nil && *q.H5Rem <= 0.5) ||
						(q.D7Rem != nil && *q.D7Rem <= 0.5) ||
						(q.C5Rem != nil && *q.C5Rem <= 0.5) ||
						(q.C7Rem != nil && *q.C7Rem <= 0.5)) {
						delete(s.Quotas, e)
					}
				}
			})
			m.reload()
			m.toast = "✓ 已清空本地冷却与耗尽缓存"
			m.toastStyle = ui.SGood
			return m, nil

		case "+", "=":
			nextVal := m.state.ReservePct + 5
			if nextVal > 30 {
				nextVal = 30
			}
			_ = m.vault.UpdateState(func(s *vault.State) { s.ReservePct = nextVal })
			m.reload()
			m.toast = fmt.Sprintf("✓ 低余量提前切号水位线: %.0f%%", nextVal)
			m.toastStyle = ui.SGood
			return m, nil

		case "-", "_":
			nextVal := m.state.ReservePct - 5
			if nextVal < 0 {
				nextVal = 0
			}
			_ = m.vault.UpdateState(func(s *vault.State) { s.ReservePct = nextVal })
			m.reload()
			m.toast = fmt.Sprintf("✓ 低余量提前切号水位线: %.0f%%", nextVal)
			m.toastStyle = ui.SGood
			return m, nil

		case "[":
			m.applyAutoCompact(vault.StepAutoCompact(m.state.AutoCompact, -5))
			return m, nil

		case "]":
			m.applyAutoCompact(vault.StepAutoCompact(m.state.AutoCompact, +5))
			return m, nil

		case "a":
			cur := vault.EffectiveAutoCompact(m.state)
			if cur == vault.DefaultAutoCompact {
				m.applyAutoCompact("off")
			} else {
				m.applyAutoCompact(vault.DefaultAutoCompact)
			}
			return m, nil

		case "d":
			if len(m.order) > 0 {
				target := m.order[m.cursor]
				m.confirmDel = target
				m.toast = fmt.Sprintf("! 确认移除 %s？按 y/d 确认，其他键取消", target)
				m.toastStyle = ui.SAmberB
			}
			return m, nil

		case "l":
			m.result = Result{Action: ActionLogin}
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *model) applyAutoCompact(raw string) {
	shellHome := ""
	applyLaunchctl := false
	if m.vault.IsDefaultStateDir() {
		shellHome = vault.HomeDir()
		applyLaunchctl = true
	}
	norm, _, err := m.vault.SetAutoCompact(raw, shellHome, applyLaunchctl)
	if err != nil {
		m.toast = fmt.Sprintf("! %v", err)
		m.toastStyle = ui.SAmber
		return
	}
	m.reload()
	if norm == "off" {
		m.toast = "✓ Auto-compact 已关闭（跟随 agy 服务端默认）"
	} else {
		m.toast = fmt.Sprintf("✓ 默认 Auto-compact: %s", norm)
	}
	m.toastStyle = ui.SGood
}

func (m model) View() string {
	cardW := m.width - 2
	if cardW > 112 {
		cardW = 112
	}
	if cardW < 64 {
		cardW = 64
	}
	innerW := cardW - 6

	now := float64(time.Now().UnixNano()) / 1e9
	readyCount := 0
	oldestAt := 0.0
	for _, e := range m.order {
		if ok, _ := router.AccountAvailability(e, m.state, m.quotas, now); ok {
			readyCount++
		}
		if q := m.quotas[e]; q != nil && q.At > 0 && (oldestAt == 0 || q.At < oldestAt) {
			oldestAt = q.At
		}
	}

	// ---- Header ----
	leftHead := ui.Brand("agy-pro") + " " + ui.SDim.Render("v"+vault.Version)
	metaParts := []string{
		ui.SHeadDim.Render("native"),
		ui.SDim.Render("drain"),
		ui.SDim.Render(fmt.Sprintf("reserve %.0f%%", m.state.ReservePct)),
		ui.SDim.Render("compact " + vault.DisplayAutoCompact(m.state)),
	}
	if oldestAt > 0 {
		metaParts = append(metaParts, ui.SDim.Render("cache "+vault.FmtAge(oldestAt, now)))
	}
	rightHead := ui.SGoodB.Render(fmt.Sprintf("%d/%d ready", readyCount, len(m.order)))
	headerLine := leftHead + "  " + strings.Join(metaParts, ui.SDim.Render(" · "))
	gap := innerW - lipgloss.Width(headerLine) - lipgloss.Width(rightHead)
	if gap > 1 {
		headerLine += strings.Repeat(" ", gap) + rightHead
	} else {
		headerLine += "  " + rightHead
	}

	// ---- Body: Account Pool Table ----
	var body strings.Builder
	body.WriteString(headerLine + "\n")
	body.WriteString(ui.Hairline(innerW) + "\n")

	if len(m.order) == 0 {
		body.WriteString("\n" + ui.SAmber.Render("  号池为空 — 按 l 调用官方 agy acp --login 添加首个账号") + "\n\n")
	} else {
		compact := innerW < 84
		barW := 8
		if compact {
			barW = 5
		}

		var rows [][]string
		for i, e := range m.order {
			isSel := i == m.cursor
			isActive := e == m.state.Active
			q := m.quotas[e]
			ok, why := router.AccountAvailability(e, m.state, m.quotas, now)

			// Selection + priority + active dot
			ptr := " "
			if isSel {
				ptr = ui.SAccentB.Render("❯")
			}
			dot := ui.SDim.Render("○")
			if isActive {
				dot = ui.SGoodB.Render("●")
			}
			idxCell := fmt.Sprintf("%s %s", ptr, dot)

			emailCell := e
			switch {
			case isSel:
				emailCell = ui.SAccentB.Render(e)
			case isActive:
				emailCell = ui.SBold.Render(e)
			case !ok:
				emailCell = ui.SDim.Render(e)
			}

			var g5Cell, g7Cell, claudeCell string
			if m.refreshing[e] {
				spin := m.spinner.View() + " " + ui.SDim.Render("probing...")
				g5Cell, g7Cell, claudeCell = spin, ui.SDim.Render("…"), ui.SDim.Render("…")
			} else if q != nil {
				g5Cell = ui.RenderBar(q.H5Used, barW)
				g7Cell = ui.RenderBar(q.D7Used, barW)
				claudeCell = ui.RenderCompactPct(q.C5Used) + ui.SDim.Render("/") + ui.RenderCompactPct(q.C7Used)
			} else {
				g5Cell = ui.RenderBar(nil, barW)
				g7Cell = ui.RenderBar(nil, barW)
				claudeCell = ui.SDim.Render("-/-")
			}

			badge := renderStatusBadge(isActive, ok, why, q, m.state.ReservePct, m.refreshing[e])
			if compact {
				rows = append(rows, []string{idxCell, emailCell, g5Cell, g7Cell, badge})
			} else {
				rows = append(rows, []string{idxCell, emailCell, g5Cell, g7Cell, claudeCell, badge})
			}
		}

		if compact {
			headers := []string{"", "ACCOUNT", "GEMINI 5H", "GEMINI 7D", "STATUS"}
			aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft}
			body.WriteString(ui.RenderTable(headers, rows, aligns, innerW, 1))
		} else {
			headers := []string{"", "ACCOUNT", "GEMINI 5H", "GEMINI 7D", "CLAUDE", "STATUS"}
			aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignLeft}
			body.WriteString(ui.RenderTable(headers, rows, aligns, innerW, 1))
		}
		body.WriteByte('\n')

		// ---- Selected Account Detail Strip ----
		selEmail := m.order[m.cursor]
		body.WriteString(ui.Hairline(innerW) + "\n")
		body.WriteString(m.renderDetailStrip(selEmail, now, innerW))
	}

	// ---- Toast Feedback ----
	if m.toast != "" {
		body.WriteString("\n" + m.toastStyle.Render(m.toast))
	} else if len(m.refreshing) > 0 {
		body.WriteString("\n" + m.spinner.View() + " " + ui.SHeadDim.Render(fmt.Sprintf("正在查询 %d 个账号...", len(m.refreshing))))
	}

	card := ui.RenderCard(body.String(), cardW, ui.ColorRule)

	// ---- Footer Keybinds ----
	keys := []string{
		ui.KeyHint("↑↓", "选择"),
		ui.KeyHint("enter", "启动 agy"),
		ui.KeyHint("s", "设为主号"),
		ui.KeyHint("r/R", "刷新全部/单个"),
		ui.KeyHint("J/K", "调序"),
		ui.KeyHint("+/-", "水位线"),
		ui.KeyHint("[/]/a", "压缩阈值"),
		ui.KeyHint("c", "清冷却"),
		ui.KeyHint("l/d", "登录/移除"),
		ui.KeyHint("q", "退出"),
	}
	footer := "  " + strings.Join(keys, ui.SDim.Render("  ·  "))
	if lipgloss.Width(footer) > cardW {
		row1 := "  " + strings.Join(keys[:5], ui.SDim.Render("  ·  "))
		row2 := "  " + strings.Join(keys[5:], ui.SDim.Render("  ·  "))
		footer = row1 + "\n" + row2
	}

	return "\n" + card + "\n" + footer + "\n"
}

func (m model) renderDetailStrip(email string, now float64, width int) string {
	acct := m.accounts[email]
	q := m.quotas[email]

	var resetParts []string
	if q != nil {
		if q.H5Reset > 0 {
			resetParts = append(resetParts, "G5h ↻ "+vault.FmtLocalTime(q.H5Reset))
		}
		if q.D7Reset > 0 {
			resetParts = append(resetParts, "G7d ↻ "+vault.FmtLocalTime(q.D7Reset))
		}
		if q.C7Reset > 0 && q.C7Used != nil && *q.C7Used > 0 {
			resetParts = append(resetParts, "C7d ↻ "+vault.FmtLocalTime(q.C7Reset))
		}
	}
	line1 := ui.SHeadDim.Render("重置时间  ") + ui.SDim.Render("暂无缓存（按 R 查询）")
	if len(resetParts) > 0 {
		line1 = ui.SHeadDim.Render("重置时间  ") + ui.SDim.Render(strings.Join(resetParts, "  ·  "))
	}

	tokInfo := ui.SDim.Render("token 未知")
	if acct != nil {
		if meta, err := vault.ReadTokenMeta(acct.TokenFile); err == nil {
			if meta.HasRefresh {
				if meta.Expiry > now {
					tokInfo = ui.SGood.Render("✓ refresh_token 正常") + ui.SDim.Render(fmt.Sprintf(" · access 剩 %s", vault.FmtDuration(meta.Expiry-now)))
				} else {
					tokInfo = ui.SGood.Render("✓ refresh_token 正常") + ui.SDim.Render(" · access 启动时静默续签")
				}
			} else {
				tokInfo = ui.SRed.Render("✗ 缺少 refresh_token")
			}
		}
	}
	if cd := m.state.Cooldowns[email]; cd != nil && cd.Until > now {
		tokInfo += "  " + ui.SAmber.Render(fmt.Sprintf("! 冷却剩 %s (%s)", vault.FmtDuration(cd.Until-now), cd.Reason))
	} else if q != nil && q.Error != "" {
		tokInfo += "  " + ui.SAmber.Render("! "+q.Error)
	}
	line2 := ui.SHeadDim.Render("凭据状态  ") + tokInfo
	_ = width
	return line1 + "\n" + line2
}

func renderStatusBadge(isActive, ok bool, why string, q *vault.Quota, reservePct float64, refreshing bool) string {
	if refreshing {
		return ui.SHeadDim.Render("SYNC")
	}
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

// Run launches the interactive BubbleTea dashboard and returns the chosen follow-up action.
func Run(v *vault.Vault, agyBin string) (Result, error) {
	m := newModel(v, agyBin)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return Result{Action: ActionNone}, err
	}
	fm, ok := final.(model)
	if !ok {
		return Result{Action: ActionNone}, nil
	}
	return fm.result, nil
}
