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
	"github.com/Tiggy-Chan/agy-pro/internal/sessions"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/usage"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionLaunch
	ActionLogin
	ActionResume
)

type Result struct {
	Action         ActionKind
	Email          string
	ConversationID string
	UseNextAccount bool
}

type probeDoneMsg struct {
	email string
	q     *vault.Quota
}

type autoRefreshStartMsg struct{}
type sessionTickMsg struct{}

const (
	autoRefreshStaleAge = 180.0 // auto-refresh cached rows older than 3m when opening `agy-pro ui`
	maxConcurrentProbes = 8
	tabPool             = 0
	tabSessions         = 1
	tabUsage            = 2
)

type model struct {
	vault         *vault.Vault
	agyBin        string
	accounts      map[string]*vault.Account
	state         *vault.State
	order         []string
	quotas        map[string]*vault.Quota
	cursor        int
	tab           int
	showAllSess   bool
	openCount     int
	sessionsList  []sessions.Session
	liveMeta      map[string]vault.LiveSession
	sessCursor    int
	sessOffset    int
	usageScanner  *usage.Scanner
	usageRange    usage.RangeKind
	usageReport   *usage.Report
	usageLoading  bool
	usageCursor   int
	homeDir       string
	width         int
	height        int
	spinner       spinner.Model
	refreshing    map[string]bool
	refreshQueue  []string
	autoRefreshOn bool
	confirmDel    string
	toast         string
	toastStyle    lipgloss.Style
	result        Result
}

func newModel(v *vault.Vault, agyBin string) model {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = ui.SAccentB

	m := model{
		vault:         v,
		agyBin:        agyBin,
		homeDir:       vault.HomeDir(),
		liveMeta:      map[string]vault.LiveSession{},
		usageScanner:  usage.NewScanner("", ""),
		usageRange:    usage.RangeToday,
		spinner:       sp,
		refreshing:    map[string]bool{},
		autoRefreshOn: true,
		width:         ui.TerminalColumns(100),
		height:        24,
		toastStyle:    ui.SDim,
	}
	m.reload()
	m.reloadSessions()
	for i, e := range m.order {
		if e == m.state.Active {
			m.cursor = i
			break
		}
	}
	return m
}

func (m *model) reloadSessions() {
	m.liveMeta = m.vault.ListLiveSessions()
	openList, _ := sessions.ListOpen("", "")
	m.openCount = len(openList)
	openSet := make(map[string]bool, len(openList))
	for _, s := range openList {
		openSet[s.ID] = true
	}

	if !m.showAllSess {
		m.sessionsList = openList
	} else {
		allList, err := sessions.ListActiveFirst("", 60)
		if err != nil {
			m.sessionsList = openList
		} else {
			for i := range allList {
				if openSet[allList[i].ID] {
					allList[i].Open = true
				}
			}
			// Keep genuinely open sessions pinned first even in All-history view
			var pinned, rest []sessions.Session
			for _, s := range allList {
				if s.Open {
					pinned = append(pinned, s)
				} else {
					rest = append(rest, s)
				}
			}
			m.sessionsList = append(pinned, rest...)
		}
	}
	if m.sessCursor >= len(m.sessionsList) && len(m.sessionsList) > 0 {
		m.sessCursor = len(m.sessionsList) - 1
	}
	if m.sessCursor < 0 {
		m.sessCursor = 0
	}
	m.clampSessionOffset()
}

func (m *model) sessionPageRows() int {
	rows := m.height - 14
	if rows < 5 {
		rows = 5
	}
	if rows > 14 {
		rows = 14
	}
	return rows
}

func (m *model) clampSessionOffset() {
	page := m.sessionPageRows()
	if m.sessCursor < m.sessOffset {
		m.sessOffset = m.sessCursor
	}
	if m.sessCursor >= m.sessOffset+page {
		m.sessOffset = m.sessCursor - page + 1
	}
	if m.sessOffset < 0 {
		m.sessOffset = 0
	}
}

func sessionTickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return sessionTickMsg{} })
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
	cmds := []tea.Cmd{m.spinner.Tick, sessionTickCmd()}
	if m.autoRefreshOn && len(m.order) > 0 {
		cmds = append(cmds, func() tea.Msg { return autoRefreshStartMsg{} })
	}
	return tea.Batch(cmds...)
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
		m.clampSessionOffset()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case sessionTickMsg:
		m.reloadSessions()
		return m, sessionTickCmd()

	case usageLoadedMsg:
		m.usageLoading = false
		if msg.rep.Range != m.usageRange {
			return m, nil
		}
		// If Today has 0 calls on initial open, auto-fallback once to Last 7 Days
		if !msg.autoTried && m.usageRange == usage.RangeToday && msg.rep.Total.Calls == 0 {
			m.usageRange = usage.Range7Days
			m.usageLoading = true
			return m, scanUsageCmd(m.usageScanner, m.usageRange, true)
		}
		rep := msg.rep
		m.usageReport = &rep
		if maxS := len(rep.BySession); maxS > 5 {
			maxS = 5
		} else if m.usageCursor >= maxS && maxS > 0 {
			m.usageCursor = maxS - 1
		}
		if m.usageCursor < 0 {
			m.usageCursor = 0
		}
		return m, nil

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

		case "tab":
			return m.switchTab((m.tab + 1) % 3)

		case "shift+tab":
			return m.switchTab((m.tab + 2) % 3)

		case "1":
			return m.switchTab(tabPool)

		case "2":
			return m.switchTab(tabSessions)

		case "3":
			return m.switchTab(tabUsage)
		}

		if m.tab == tabUsage {
			switch k {
			case "left", "h":
				m.usageRange = (m.usageRange + 2) % 3
				m.usageCursor = 0
				m.usageLoading = true
				return m, scanUsageCmd(m.usageScanner, m.usageRange, true)
			case "right", "l", "t":
				m.usageRange = (m.usageRange + 1) % 3
				m.usageCursor = 0
				m.usageLoading = true
				return m, scanUsageCmd(m.usageScanner, m.usageRange, true)
			case "r":
				m.usageLoading = true
				m.toast = "正在重新统计本地 Token 用量..."
				m.toastStyle = ui.SHeadDim
				return m, scanUsageCmd(m.usageScanner, m.usageRange, true)
			case "up", "k":
				if m.usageCursor > 0 {
					m.usageCursor--
				}
				return m, nil
			case "down", "j":
				if m.usageReport != nil {
					maxS := len(m.usageReport.BySession)
					if maxS > 5 {
						maxS = 5
					}
					if m.usageCursor < maxS-1 {
						m.usageCursor++
					}
				}
				return m, nil
			case "enter":
				if m.usageReport != nil && m.usageCursor < len(m.usageReport.BySession) {
					sel := m.usageReport.BySession[m.usageCursor]
					m.result = Result{Action: ActionResume, ConversationID: sel.ConvID, UseNextAccount: false}
					return m, tea.Quit
				}
				return m, nil
			case "ctrl+n":
				if m.usageReport != nil && m.usageCursor < len(m.usageReport.BySession) {
					sel := m.usageReport.BySession[m.usageCursor]
					m.result = Result{Action: ActionResume, ConversationID: sel.ConvID, UseNextAccount: true}
					return m, tea.Quit
				}
				return m, nil
			}
			return m, nil
		}

		if m.tab == tabSessions {
			switch k {
			case "up", "k":
				if len(m.sessionsList) > 0 && m.sessCursor > 0 {
					m.sessCursor--
					m.clampSessionOffset()
				}
				return m, nil
			case "down", "j":
				if len(m.sessionsList) > 0 && m.sessCursor < len(m.sessionsList)-1 {
					m.sessCursor++
					m.clampSessionOffset()
				}
				return m, nil
			case "r":
				m.reloadSessions()
				m.toast = "✓ 已刷新活跃会话"
				m.toastStyle = ui.SGood
				return m, nil
			case "a":
				m.showAllSess = !m.showAllSess
				m.sessCursor = 0
				m.sessOffset = 0
				m.reloadSessions()
				if m.showAllSess {
					m.toast = "已切换为：含历史会话（打开中置顶）"
				} else {
					m.toast = "已切换为：仅显示当前真实打开的会话"
				}
				m.toastStyle = ui.SHeadDim
				return m, nil
			case "enter":
				if len(m.sessionsList) > 0 {
					sel := m.sessionsList[m.sessCursor]
					m.result = Result{Action: ActionResume, ConversationID: sel.ID, UseNextAccount: false}
					return m, tea.Quit
				}
				return m, nil
			case "ctrl+n":
				if len(m.sessionsList) > 0 {
					sel := m.sessionsList[m.sessCursor]
					m.result = Result{Action: ActionResume, ConversationID: sel.ID, UseNextAccount: true}
					return m, tea.Quit
				}
				return m, nil
			}
			return m, nil
		}

		switch k {
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
	// Never touch user shell dotfiles from TUI; persist strictly inside agy-pro state.json.
	norm, _, err := m.vault.SetAutoCompact(raw, "", false)
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

func (m model) switchTab(target int) (tea.Model, tea.Cmd) {
	m.tab = target
	m.toast = ""
	switch target {
	case tabSessions:
		m.reloadSessions()
		return m, nil
	case tabUsage:
		if m.usageReport == nil && !m.usageLoading {
			m.usageLoading = true
			return m, scanUsageCmd(m.usageScanner, m.usageRange, false)
		}
		return m, nil
	default:
		return m, nil
	}
}

func (m model) runningSessionCount() int {
	n := 0
	for _, s := range m.sessionsList {
		if s.Running {
			n++
		}
	}
	return n
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

	// ---- Header with 3 Tabs: Pool / Open Sessions / Token Usage ----
	runCnt := m.runningSessionCount()
	sessLabel := "当前打开"
	if m.openCount > 0 {
		sessLabel = fmt.Sprintf("当前打开 %d", m.openCount)
	}
	pillPool := ui.SDim.Render("号池")
	pillSess := ui.SDim.Render(sessLabel)
	pillUsage := ui.SDim.Render("用量统计")
	switch m.tab {
	case tabPool:
		pillPool = ui.BadgeActive.Render("号池")
	case tabSessions:
		pillSess = ui.BadgeActive.Render(sessLabel)
	case tabUsage:
		pillUsage = ui.BadgeActive.Render("用量统计")
	}

	leftHead := ui.Brand("agy-pro") + " " + ui.SDim.Render("v"+vault.Version) + "  " + pillPool + " " + pillSess + " " + pillUsage
	var headerLine string
	switch m.tab {
	case tabPool:
		metaParts := []string{
			ui.SDim.Render(fmt.Sprintf("reserve %.0f%%", m.state.ReservePct)),
			ui.SDim.Render("compact " + vault.DisplayAutoCompact(m.state)),
		}
		if oldestAt > 0 {
			metaParts = append(metaParts, ui.SDim.Render("cache "+vault.FmtAge(oldestAt, now)))
		}
		rightHead := ui.SGoodB.Render(fmt.Sprintf("%d/%d ready", readyCount, len(m.order)))
		headerLine = leftHead + "  " + strings.Join(metaParts, ui.SDim.Render(" · "))
		if gap := innerW - lipgloss.Width(headerLine) - lipgloss.Width(rightHead); gap > 1 {
			headerLine += strings.Repeat(" ", gap) + rightHead
		} else {
			headerLine += "  " + rightHead
		}
	case tabSessions:
		scopeTag := "仅打开"
		if m.showAllSess {
			scopeTag = "含历史"
		}
		rightHead := ui.SDim.Render(fmt.Sprintf("%s · %d 运行 / %d 打开", scopeTag, runCnt, m.openCount))
		headerLine = leftHead
		if gap := innerW - lipgloss.Width(leftHead) - lipgloss.Width(rightHead); gap > 1 {
			headerLine += strings.Repeat(" ", gap) + rightHead
		} else {
			headerLine += "  " + rightHead
		}
	case tabUsage:
		rangePills := make([]string, 0, 3)
		for _, rk := range []usage.RangeKind{usage.RangeToday, usage.Range7Days, usage.RangeAll} {
			if rk == m.usageRange {
				rangePills = append(rangePills, ui.SAccentB.Render("["+rk.Label()+"]"))
			} else {
				rangePills = append(rangePills, ui.SDim.Render(rk.Label()))
			}
		}
		rightHead := strings.Join(rangePills, " ")
		headerLine = leftHead
		if gap := innerW - lipgloss.Width(leftHead) - lipgloss.Width(rightHead); gap > 1 {
			headerLine += strings.Repeat(" ", gap) + rightHead
		} else {
			headerLine += "  " + rightHead
		}
	}

	// ---- Body ----
	var body strings.Builder
	body.WriteString(headerLine + "\n")
	body.WriteString(ui.Hairline(innerW) + "\n")

	if m.tab == tabUsage {
		body.WriteString(m.renderUsageBody(innerW))
	} else if m.tab == tabSessions {
		body.WriteString(m.renderSessionsBody(innerW))
	} else if len(m.order) == 0 {
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
	} else if m.tab == tabPool && len(m.refreshing) > 0 {
		body.WriteString("\n" + m.spinner.View() + " " + ui.SHeadDim.Render(fmt.Sprintf("正在查询 %d 个账号...", len(m.refreshing))))
	}

	card := ui.RenderCard(body.String(), cardW, ui.ColorRule)

	// ---- Footer Keybinds ----
	var keys []string
	switch m.tab {
	case tabUsage:
		keys = []string{
			ui.KeyHint("tab/1-3", "切视图"),
			ui.KeyHint("←/→/t", "今日/7天/全部"),
			ui.KeyHint("↑↓", "选会话"),
			ui.KeyHint("enter", "当前号恢复"),
			ui.KeyHint("ctrl+n", "下个号恢复"),
			ui.KeyHint("r", "重扫"),
			ui.KeyHint("q", "退出"),
		}
	case tabSessions:
		keys = []string{
			ui.KeyHint("tab/1-3", "切视图"),
			ui.KeyHint("↑↓", "选择"),
			ui.KeyHint("a", "仅打开/含历史"),
			ui.KeyHint("enter", "当前号恢复"),
			ui.KeyHint("ctrl+n", "下个号恢复"),
			ui.KeyHint("r", "刷新"),
			ui.KeyHint("q", "退出"),
		}
	default:
		keys = []string{
			ui.KeyHint("tab/1-3", "切视图"),
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
	}
	footer := "  " + strings.Join(keys, ui.SDim.Render("  ·  "))
	if lipgloss.Width(footer) > cardW {
		mid := (len(keys) + 1) / 2
		row1 := "  " + strings.Join(keys[:mid], ui.SDim.Render("  ·  "))
		row2 := "  " + strings.Join(keys[mid:], ui.SDim.Render("  ·  "))
		footer = row1 + "\n" + row2
	}

	return "\n" + card + "\n" + footer + "\n"
}

func shortAccountLabel(email string) string {
	if email == "" {
		return "native"
	}
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" {
		return local
	}
	return email
}

func (m model) renderSessionsBody(innerW int) string {
	if len(m.sessionsList) == 0 {
		if !m.showAllSess {
			return "\n" + ui.SAmber.Render("  当前本机没有正在打开的 agy 会话（按 a 可查看全部历史，或按 Tab 切回号池启动）") + "\n"
		}
		return "\n" + ui.SAmber.Render("  暂无会话记录（启动一次 agy 后自动同步）") + "\n"
	}
	var b strings.Builder
	now := time.Now()
	page := m.sessionPageRows()
	end := m.sessOffset + page
	if end > len(m.sessionsList) {
		end = len(m.sessionsList)
	}

	showWS := innerW >= 86
	wsColW := 20
	if !showWS {
		wsColW = 0
	}
	showAcct := innerW >= 74
	acctColW := 13
	if !showAcct {
		acctColW = 0
	}
	titleMaxW := innerW - 26 - wsColW - acctColW
	if titleMaxW < 16 {
		titleMaxW = 16
	}

	var rows [][]string
	for idx := m.sessOffset; idx < end; idx++ {
		s := m.sessionsList[idx]
		isSel := idx == m.sessCursor

		ptr := " "
		if isSel {
			ptr = ui.SAccentB.Render("❯")
		}
		var stCell string
		switch {
		case s.Open && s.Running:
			stCell = ui.SGoodB.Render(m.spinner.View() + " 运行")
		case s.Open:
			stCell = ui.SHeadDim.Render("● 打开")
		default:
			stCell = ui.SDim.Render("✦ 历史")
		}

		title := ui.Truncate(s.Title, titleMaxW)
		if isSel {
			title = ui.SAccentB.Render(title)
		} else if s.Running {
			title = ui.SBold.Render(title)
		} else if !s.Open {
			title = ui.SDim.Render(title)
		}

		live, hasLive := m.liveMeta[s.ID]
		acctCell := ui.SDim.Render("-")
		if s.Open {
			if hasLive {
				acctCell = ui.SGood.Render(ui.Truncate(shortAccountLabel(live.Email), acctColW))
			} else {
				acctCell = ui.SDim.Render("native")
			}
		}

		stepsCell := ui.SDim.Render(fmt.Sprintf("%d步", s.Steps))
		ageCell := ui.SDim.Render(sessions.FmtRelativeTime(s.ModifiedAt, now))
		row := []string{ptr, stCell, title}
		if showAcct {
			row = append(row, acctCell)
		}
		if showWS {
			row = append(row, ui.SHeadDim.Render(ui.Truncate(s.PrimaryWorkspace(m.homeDir), wsColW)))
		}
		row = append(row, stepsCell, ageCell)
		rows = append(rows, row)
	}

	headers := []string{"", "STATE", "SESSION"}
	aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft}
	if showAcct {
		headers = append(headers, "ACCOUNT")
		aligns = append(aligns, ui.AlignLeft)
	}
	if showWS {
		headers = append(headers, "WORKSPACE")
		aligns = append(aligns, ui.AlignLeft)
	}
	headers = append(headers, "STEPS", "TIME")
	aligns = append(aligns, ui.AlignRight, ui.AlignRight)
	b.WriteString(ui.RenderTable(headers, rows, aligns, innerW, 2))
	b.WriteByte('\n')

	sel := m.sessionsList[m.sessCursor]
	b.WriteString(ui.Hairline(innerW) + "\n")
	idShort := sel.ID
	if len(idShort) > 8 {
		idShort = idShort[:8]
	}
	var metaParts []string
	if live, ok := m.liveMeta[sel.ID]; ok {
		if live.TTY != "" {
			metaParts = append(metaParts, "终端 "+live.TTY)
		}
		if live.Email != "" {
			metaParts = append(metaParts, "账号 "+live.Email)
		}
	} else if sel.Open {
		metaParts = append(metaParts, "本机进程已持有 presence 锁")
	}
	metaParts = append(metaParts, "ID "+idShort)
	rightMeta := ui.SDim.Render("  ·  " + strings.Join(metaParts, "  ·  "))
	wsMax := innerW - lipgloss.Width("目录  ") - lipgloss.Width(rightMeta)
	if wsMax < 12 {
		wsMax = 12
	}
	line1 := ui.SHeadDim.Render("目录  ") + ui.SDim.Render(ui.Truncate(sel.PrimaryWorkspace(m.homeDir), wsMax)) + rightMeta
	prev := sel.Preview
	if prev == "" {
		prev = sel.Title
	}
	line2 := ui.SHeadDim.Render("预览  ") + ui.SDim.Render(ui.Truncate(prev, innerW-8))
	b.WriteString(line1 + "\n" + line2)
	return b.String()
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
