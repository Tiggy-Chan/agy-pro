package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
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
	poolOffset    int
	maskEmails    bool
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
		maskEmails:    ui.MaskEnabled(),
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
	m.clampPoolOffset()
	return m
}

func (m model) dispEmail(email string) string {
	return ui.DisplayEmail(email, m.maskEmails)
}

func (m *model) poolPageRows() int {
	rows := m.height - 14
	if rows < 5 {
		rows = 5
	}
	if rows > 16 {
		rows = 16
	}
	return rows
}

func (m *model) clampPoolOffset() {
	page := m.poolPageRows()
	if m.cursor < m.poolOffset {
		m.poolOffset = m.cursor
	}
	if m.cursor >= m.poolOffset+page {
		m.poolOffset = m.cursor - page + 1
	}
	if maxOff := len(m.order) - page; maxOff >= 0 && m.poolOffset > maxOff {
		m.poolOffset = maxOff
	}
	if m.poolOffset < 0 {
		m.poolOffset = 0
	}
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
		allList, err := sessions.ListActiveFirst("", 300)
		if err != nil {
			m.sessionsList = openList
		} else {
			// Keep every genuinely open session (even brand-new 0-step windows) pinned first,
			// followed by non-open historical conversations.
			merged := make([]sessions.Session, 0, len(openList)+len(allList))
			merged = append(merged, openList...)
			for _, s := range allList {
				if !openSet[s.ID] {
					merged = append(merged, s)
				}
			}
			m.sessionsList = merged
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
	m.clampPoolOffset()
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
		m.clampPoolOffset()
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
			return m, scanUsageCmd(m.usageScanner, m.usageRange, true, false)
		}
		rep := msg.rep
		m.usageReport = &rep
		if msg.manual {
			m.toast = i18n.T("ui.toast.usage_rescanned", rep.ElapsedMs)
			m.toastStyle = ui.SGood
		} else if m.toast == i18n.T("ui.toast.usage_scanning") {
			m.toast = ""
		}
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
			m.toast = i18n.T("ui.toast.auto_refresh", m.pendingRefreshCount())
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
				m.toast = fmt.Sprintf("! %s: %s", m.dispEmail(msg.email), msg.q.Error)
				m.toastStyle = ui.SAmber
			} else if clearedCooldown {
				m.toast = i18n.T("ui.toast.recovered", m.dispEmail(msg.email))
				m.toastStyle = ui.SGood
			} else {
				m.toast = i18n.T("ui.toast.refreshed")
				m.toastStyle = ui.SGood
			}
		} else {
			m.toast = i18n.T("ui.toast.refreshing_rem", rem)
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
				m.toast = i18n.T("ui.toast.removed", m.dispEmail(target))
				m.toastStyle = ui.SGood
				return m, nil
			}
			m.confirmDel = ""
			m.toast = i18n.T("ui.toast.remove_cancelled")
			m.toastStyle = ui.SDim
			return m, nil
		}
		switch k {
		case "q", "esc", "ctrl+c":
			m.result = Result{Action: ActionNone}
			return m, tea.Quit

		case "m":
			m.maskEmails = !m.maskEmails
			if m.maskEmails {
				m.toast = i18n.T("ui.toast.mask_on")
			} else {
				m.toast = i18n.T("ui.toast.mask_off")
			}
			m.toastStyle = ui.SGood
			return m, nil

		case "g":
			nextLang := i18n.NextPref(m.state.Language)
			_ = m.vault.UpdateState(func(s *vault.State) { s.Language = nextLang })
			i18n.SetPref(nextLang)
			m.reload()
			m.toast = i18n.T("ui.toast.lang_set", i18n.DisplayPref(nextLang))
			m.toastStyle = ui.SGood
			return m, nil

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
				m.toast = ""
				return m, scanUsageCmd(m.usageScanner, m.usageRange, true, false)
			case "right", "l", "t":
				m.usageRange = (m.usageRange + 1) % 3
				m.usageCursor = 0
				m.usageLoading = true
				m.toast = ""
				return m, scanUsageCmd(m.usageScanner, m.usageRange, true, false)
			case "r":
				m.usageLoading = true
				m.toast = i18n.T("ui.toast.usage_scanning")
				m.toastStyle = ui.SHeadDim
				return m, scanUsageCmd(m.usageScanner, m.usageRange, true, true)
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
				m.toast = i18n.T("ui.toast.sessions_refreshed")
				m.toastStyle = ui.SGood
				return m, nil
			case "a":
				m.showAllSess = !m.showAllSess
				m.sessCursor = 0
				m.sessOffset = 0
				m.reloadSessions()
				if m.showAllSess {
					m.toast = i18n.T("ui.toast.scope_all")
				} else {
					m.toast = i18n.T("ui.toast.scope_open")
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
				m.clampPoolOffset()
			}
			return m, nil

		case "down", "j":
			if len(m.order) > 0 && m.cursor < len(m.order)-1 {
				m.cursor++
				m.clampPoolOffset()
			}
			return m, nil

		case "K": // Move selected account up in drain priority
			if len(m.order) > 1 && m.cursor > 0 {
				m.order[m.cursor], m.order[m.cursor-1] = m.order[m.cursor-1], m.order[m.cursor]
				m.cursor--
				m.clampPoolOffset()
				newOrd := append([]string(nil), m.order...)
				_ = m.vault.UpdateState(func(s *vault.State) { s.Order = newOrd })
				m.reload()
				m.toast = i18n.T("ui.toast.order_up")
				m.toastStyle = ui.SGood
			}
			return m, nil

		case "J": // Move selected account down in drain priority
			if len(m.order) > 1 && m.cursor < len(m.order)-1 {
				m.order[m.cursor], m.order[m.cursor+1] = m.order[m.cursor+1], m.order[m.cursor]
				m.cursor++
				m.clampPoolOffset()
				newOrd := append([]string(nil), m.order...)
				_ = m.vault.UpdateState(func(s *vault.State) { s.Order = newOrd })
				m.reload()
				m.toast = i18n.T("ui.toast.order_down")
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
				m.toast = i18n.T("ui.toast.set_active", m.dispEmail(chosen))
				m.toastStyle = ui.SGood
			}
			return m, nil

		case "r":
			if len(m.order) == 0 {
				return m, nil
			}
			cmds := m.enqueueProbes(m.order)
			if len(cmds) > 0 {
				m.toast = i18n.T("ui.toast.refreshing_n", m.pendingRefreshCount())
				m.toastStyle = ui.SHeadDim
				return m, tea.Batch(cmds...)
			}
			return m, nil

		case "R":
			if len(m.order) > 0 {
				e := m.order[m.cursor]
				cmds := m.enqueueProbes([]string{e})
				if len(cmds) > 0 {
					m.toast = i18n.T("ui.toast.refreshing_one", m.dispEmail(e))
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
			m.toast = i18n.T("ui.toast.cleared_cd")
			m.toastStyle = ui.SGood
			return m, nil

		case "+", "=":
			nextVal := m.state.ReservePct + 5
			if nextVal > 30 {
				nextVal = 30
			}
			_ = m.vault.UpdateState(func(s *vault.State) { s.ReservePct = nextVal })
			m.reload()
			m.toast = i18n.T("ui.toast.reserve_set", nextVal)
			m.toastStyle = ui.SGood
			return m, nil

		case "-", "_":
			nextVal := m.state.ReservePct - 5
			if nextVal < 0 {
				nextVal = 0
			}
			_ = m.vault.UpdateState(func(s *vault.State) { s.ReservePct = nextVal })
			m.reload()
			m.toast = i18n.T("ui.toast.reserve_set", nextVal)
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
				m.toast = i18n.T("ui.toast.confirm_del", m.dispEmail(target))
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
		m.toast = i18n.T("ui.toast.compact_off")
	} else {
		m.toast = i18n.T("ui.toast.compact_set", norm)
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
			return m, scanUsageCmd(m.usageScanner, m.usageRange, false, false)
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
	sessLabel := i18n.T("ui.tab.open")
	if m.openCount > 0 {
		sessLabel = i18n.T("ui.tab.open_n", m.openCount)
	}
	pillPool := ui.SDim.Render(i18n.T("ui.tab.pool"))
	pillSess := ui.SDim.Render(sessLabel)
	pillUsage := ui.SDim.Render(i18n.T("ui.tab.usage"))
	switch m.tab {
	case tabPool:
		pillPool = ui.BadgeActive.Render(i18n.T("ui.tab.pool"))
	case tabSessions:
		pillSess = ui.BadgeActive.Render(sessLabel)
	case tabUsage:
		pillUsage = ui.BadgeActive.Render(i18n.T("ui.tab.usage"))
	}

	leftHead := ui.Brand("agy-pro") + " " + ui.SDim.Render("v"+vault.Version) + "  " + pillPool + " " + pillSess + " " + pillUsage
	var headerLine string
	switch m.tab {
	case tabPool:
		rightHead := ui.SGoodB.Render(fmt.Sprintf("%d/%d ready", readyCount, len(m.order)))
		metaParts := []string{}
		if m.state.ReservePct > 0 {
			metaParts = append(metaParts, ui.SDim.Render(fmt.Sprintf("reserve %.0f%%", m.state.ReservePct)))
		}
		metaParts = append(metaParts, ui.SDim.Render("compact "+vault.DisplayAutoCompact(m.state)))
		if len(m.order) > m.poolPageRows() {
			pEnd := m.poolOffset + m.poolPageRows()
			if pEnd > len(m.order) {
				pEnd = len(m.order)
			}
			metaParts = append(metaParts, ui.SDim.Render(fmt.Sprintf("%d-%d/%d", m.poolOffset+1, pEnd, len(m.order))))
		}
		if m.maskEmails {
			metaParts = append(metaParts, ui.SHeadDim.Render("mask"))
		}
		if oldestAt > 0 {
			cand := append(append([]string(nil), metaParts...), ui.SDim.Render("cache "+vault.FmtAge(oldestAt, now)))
			candLine := leftHead + "  " + strings.Join(cand, ui.SDim.Render(" · "))
			if lipgloss.Width(candLine)+lipgloss.Width(rightHead)+2 <= innerW {
				metaParts = cand
			}
		}
		for len(metaParts) > 0 {
			candLine := leftHead + "  " + strings.Join(metaParts, ui.SDim.Render(" · "))
			if lipgloss.Width(candLine)+lipgloss.Width(rightHead)+2 <= innerW {
				break
			}
			metaParts = metaParts[:len(metaParts)-1]
		}
		headerLine = leftHead
		if len(metaParts) > 0 {
			headerLine += "  " + strings.Join(metaParts, ui.SDim.Render(" · "))
		}
		if gap := innerW - lipgloss.Width(headerLine) - lipgloss.Width(rightHead); gap > 1 {
			headerLine += strings.Repeat(" ", gap) + rightHead
		} else {
			headerLine += "  " + rightHead
		}
	case tabSessions:
		scopeTag := i18n.T("ui.scope.open_only")
		if m.showAllSess {
			scopeTag = i18n.T("ui.scope.with_history")
		}
		rightHead := ui.SDim.Render(i18n.T("ui.open_summary", scopeTag, runCnt, m.openCount))
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
		body.WriteString("\n" + ui.SAmber.Render(i18n.T("ui.pool_empty_hint")) + "\n\n")
	} else {
		compact := innerW < 84
		barW := 8
		if compact {
			barW = 5
		}

		page := m.poolPageRows()
		start := m.poolOffset
		if start < 0 {
			start = 0
		}
		if start >= len(m.order) {
			start = 0
		}
		end := start + page
		if end > len(m.order) {
			end = len(m.order)
		}

		var rows [][]string
		for i := start; i < end; i++ {
			e := m.order[i]
			isSel := i == m.cursor
			isActive := e == m.state.Active
			q := m.quotas[e]
			ok, _ := router.AccountAvailability(e, m.state, m.quotas, now)
			kind := router.StatusKind(e, m.state, m.quotas, now)

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

			shownEmail := m.dispEmail(e)
			emailCell := shownEmail
			switch {
			case isSel:
				emailCell = ui.SAccentB.Render(shownEmail)
			case isActive:
				emailCell = ui.SBold.Render(shownEmail)
			case !ok:
				emailCell = ui.SDim.Render(shownEmail)
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

			badge := renderStatusBadge(isActive, ok, kind, q, m.state.ReservePct, m.refreshing[e])
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
		body.WriteString("\n" + m.spinner.View() + " " + ui.SHeadDim.Render(i18n.T("ui.probing_n", len(m.refreshing))))
	}

	card := ui.RenderCard(body.String(), cardW, ui.ColorRule)

	// ---- Footer Keybinds ----
	var keys []string
	switch m.tab {
	case tabUsage:
		keys = []string{
			ui.KeyHint("tab/1-3", i18n.T("ui.key.tabs")),
			ui.KeyHint("←/→/t", i18n.T("ui.key.range")),
			ui.KeyHint("↑↓", i18n.T("ui.key.pick_sess")),
			ui.KeyHint("enter", i18n.T("ui.key.resume_cur")),
			ui.KeyHint("ctrl+n", i18n.T("ui.key.resume_next")),
			ui.KeyHint("r", i18n.T("ui.key.rescan")),
			ui.KeyHint("g", i18n.T("ui.key.lang")),
			ui.KeyHint("q", i18n.T("ui.key.quit")),
		}
	case tabSessions:
		keys = []string{
			ui.KeyHint("tab/1-3", i18n.T("ui.key.tabs")),
			ui.KeyHint("↑↓", i18n.T("ui.key.select")),
			ui.KeyHint("a", i18n.T("ui.key.scope_open_hist")),
			ui.KeyHint("enter", i18n.T("ui.key.resume_cur")),
			ui.KeyHint("ctrl+n", i18n.T("ui.key.resume_next")),
			ui.KeyHint("r", i18n.T("ui.key.refresh")),
			ui.KeyHint("m", i18n.T("ui.key.mask")),
			ui.KeyHint("g", i18n.T("ui.key.lang")),
			ui.KeyHint("q", i18n.T("ui.key.quit")),
		}
	default:
		keys = []string{
			ui.KeyHint("tab/1-3", i18n.T("ui.key.tabs")),
			ui.KeyHint("↑↓", i18n.T("ui.key.select")),
			ui.KeyHint("enter", i18n.T("ui.key.launch")),
			ui.KeyHint("s", i18n.T("ui.key.set_active")),
			ui.KeyHint("r/R", i18n.T("ui.key.refresh_all_one")),
			ui.KeyHint("J/K", i18n.T("ui.key.reorder")),
			ui.KeyHint("+/-", i18n.T("ui.key.reserve")),
			ui.KeyHint("[/]/a", i18n.T("ui.key.compact")),
			ui.KeyHint("c", i18n.T("ui.key.clear_cd")),
			ui.KeyHint("l/d", i18n.T("ui.key.login_rm")),
			ui.KeyHint("m", i18n.T("ui.key.mask")),
			ui.KeyHint("g", i18n.T("ui.key.lang")),
			ui.KeyHint("q", i18n.T("ui.key.quit")),
		}
	}
	sep := ui.SDim.Render("  ·  ")
	sepW := lipgloss.Width(sep)
	var footerLines []string
	curLine := "  "
	curW := 2
	for _, k := range keys {
		kw := lipgloss.Width(k)
		if curW > 2 && curW+sepW+kw > cardW {
			footerLines = append(footerLines, curLine)
			curLine = "  " + k
			curW = 2 + kw
		} else {
			if curW > 2 {
				curLine += sep
				curW += sepW
			}
			curLine += k
			curW += kw
		}
	}
	if curW > 2 {
		footerLines = append(footerLines, curLine)
	}
	footer := strings.Join(footerLines, "\n")

	return "\n" + card + "\n" + footer + "\n"
}

func shortAccountLabel(email string, masked bool) string {
	if email == "" {
		return "native"
	}
	shown := ui.DisplayEmail(email, masked)
	if local, _, ok := strings.Cut(shown, "@"); ok && local != "" {
		return local
	}
	return shown
}

func (m model) renderSessionsBody(innerW int) string {
	if len(m.sessionsList) == 0 {
		if !m.showAllSess {
			return "\n" + ui.SAmber.Render(i18n.T("ui.sessions.empty_open")) + "\n"
		}
		return "\n" + ui.SAmber.Render(i18n.T("ui.sessions.empty_all")) + "\n"
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
			stCell = ui.SGoodB.Render(m.spinner.View() + " " + i18n.T("ui.sessions.state_running"))
		case s.Open:
			stCell = ui.SHeadDim.Render(i18n.T("ui.sessions.state_open"))
		default:
			stCell = ui.SDim.Render(i18n.T("ui.sessions.state_history"))
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
				acctCell = ui.SGood.Render(ui.Truncate(shortAccountLabel(live.Email, m.maskEmails), acctColW))
			} else {
				acctCell = ui.SDim.Render("native")
			}
		}

		stepsCell := ui.SDim.Render(i18n.T("ui.sessions.steps", s.Steps))
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
			metaParts = append(metaParts, i18n.T("ui.sessions.detail_tty", live.TTY))
		}
		if live.Email != "" {
			metaParts = append(metaParts, i18n.T("ui.sessions.detail_acct", m.dispEmail(live.Email)))
		}
	} else if sel.Open {
		metaParts = append(metaParts, i18n.T("ui.sessions.detail_presence"))
	}
	metaParts = append(metaParts, "ID "+idShort)
	dirLabel := i18n.T("ui.sessions.detail_dir")
	rightMeta := ui.SDim.Render("  ·  " + strings.Join(metaParts, "  ·  "))
	wsMax := innerW - lipgloss.Width(dirLabel) - lipgloss.Width(rightMeta)
	if wsMax < 12 {
		wsMax = 12
	}
	line1 := ui.SHeadDim.Render(dirLabel) + ui.SDim.Render(ui.Truncate(sel.PrimaryWorkspace(m.homeDir), wsMax)) + rightMeta
	prev := sel.Preview
	if prev == "" {
		prev = sel.Title
	}
	line2 := ui.SHeadDim.Render(i18n.T("ui.sessions.detail_preview")) + ui.SDim.Render(ui.Truncate(prev, innerW-8))
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
	line1 := ui.SHeadDim.Render(i18n.T("ui.detail.reset_label")) + ui.SDim.Render(i18n.T("ui.detail.reset_empty"))
	if len(resetParts) > 0 {
		line1 = ui.SHeadDim.Render(i18n.T("ui.detail.reset_label")) + ui.SDim.Render(strings.Join(resetParts, "  ·  "))
	}

	tokInfo := ui.SDim.Render(i18n.T("ui.detail.tok_unknown"))
	if acct != nil {
		if meta, err := vault.ReadTokenMeta(acct.TokenFile); err == nil {
			if meta.HasRefresh {
				if meta.Expiry > now {
					tokInfo = ui.SGood.Render(i18n.T("ui.detail.tok_ok")) + ui.SDim.Render(i18n.T("ui.detail.tok_access_rem", vault.FmtDuration(meta.Expiry-now)))
				} else {
					tokInfo = ui.SGood.Render(i18n.T("ui.detail.tok_ok")) + ui.SDim.Render(i18n.T("ui.detail.tok_silent_renew"))
				}
			} else {
				tokInfo = ui.SRed.Render(i18n.T("ui.detail.tok_missing_refresh"))
			}
		}
	}
	if cd := m.state.Cooldowns[email]; cd != nil && cd.Until > now {
		tokInfo += "  " + ui.SAmber.Render(i18n.T("ui.detail.cooling_rem", vault.FmtDuration(cd.Until-now), cd.Reason))
	} else if q != nil && q.Error != "" {
		tokInfo += "  " + ui.SAmber.Render("! "+q.Error)
	}
	line2 := ui.SHeadDim.Render(i18n.T("ui.detail.cred_label")) + tokInfo
	_ = width
	return line1 + "\n" + line2
}

func renderStatusBadge(isActive, ok bool, kind string, q *vault.Quota, reservePct float64, refreshing bool) string {
	if refreshing {
		return ui.SHeadDim.Render("SYNC")
	}
	if !ok {
		switch kind {
		case "7d":
			return ui.BadgeFull.Render("7D FULL")
		case "5h":
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
func Run(v *vault.Vault, agyBin string, forceMask bool) (Result, error) {
	m := newModel(v, agyBin)
	if forceMask {
		m.maskEmails = true
	}
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
