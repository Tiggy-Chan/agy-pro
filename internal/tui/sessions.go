package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Tiggy-Chan/agy-pro/internal/sessions"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

type SessionPickResult struct {
	Selected       bool
	ConversationID string
	UseNextAccount bool
}

type sessionModel struct {
	all           []sessions.Session
	filtered      []sessions.Session
	cwd           string
	homeDir       string
	activeAccount string
	showAll       bool
	query         string
	cursor        int
	offset        int
	width         int
	height        int
	result        SessionPickResult
}

func newSessionModel(list []sessions.Session, cwd, homeDir, activeAccount string) sessionModel {
	m := sessionModel{
		all:           list,
		cwd:           cwd,
		homeDir:       homeDir,
		activeAccount: activeAccount,
		showAll:       false,
		width:         ui.TerminalColumns(100),
		height:        26,
	}
	m.applyFilter()
	// If current directory has no conversations yet, automatically open in All-workspaces view
	if len(m.filtered) == 0 && len(m.all) > 0 {
		m.showAll = true
		m.applyFilter()
	}
	return m
}

func (m *sessionModel) applyFilter() {
	out := make([]sessions.Session, 0, len(m.all))
	for _, s := range m.all {
		if !m.showAll && !s.MatchesDir(m.cwd) {
			continue
		}
		if !s.MatchesQuery(m.query) {
			continue
		}
		out = append(out, s)
	}
	m.filtered = out
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.clampOffset()
}

func (m *sessionModel) pageRows() int {
	// Card header + search + detail strip + footer take ~14 rows
	rows := m.height - 14
	if rows < 5 {
		rows = 5
	}
	if rows > 16 {
		rows = 16
	}
	return rows
}

func (m *sessionModel) clampOffset() {
	page := m.pageRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+page {
		m.offset = m.cursor - page + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m sessionModel) Init() tea.Cmd { return nil }

func (m sessionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.clampOffset()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.result = SessionPickResult{}
			return m, tea.Quit

		case tea.KeyEsc:
			if m.query != "" {
				m.query = ""
				m.cursor = 0
				m.offset = 0
				m.applyFilter()
				return m, nil
			}
			m.result = SessionPickResult{}
			return m, tea.Quit

		case tea.KeyTab:
			m.showAll = !m.showAll
			m.cursor = 0
			m.offset = 0
			m.applyFilter()
			return m, nil

		case tea.KeyUp, tea.KeyCtrlP:
			if m.cursor > 0 {
				m.cursor--
				m.clampOffset()
			}
			return m, nil

		case tea.KeyDown, tea.KeyCtrlJ:
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
				m.clampOffset()
			}
			return m, nil

		case tea.KeyPgUp:
			m.cursor -= m.pageRows()
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.clampOffset()
			return m, nil

		case tea.KeyPgDown:
			m.cursor += m.pageRows()
			if m.cursor >= len(m.filtered) {
				m.cursor = len(m.filtered) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.clampOffset()
			return m, nil

		case tea.KeyEnter:
			if len(m.filtered) > 0 {
				m.result = SessionPickResult{
					Selected:       true,
					ConversationID: m.filtered[m.cursor].ID,
					UseNextAccount: false,
				}
				return m, tea.Quit
			}
			return m, nil

		case tea.KeyCtrlN:
			if len(m.filtered) > 0 {
				m.result = SessionPickResult{
					Selected:       true,
					ConversationID: m.filtered[m.cursor].ID,
					UseNextAccount: true,
				}
				return m, tea.Quit
			}
			return m, nil

		case tea.KeyBackspace:
			if len(m.query) > 0 {
				r := []rune(m.query)
				m.query = string(r[:len(r)-1])
				m.cursor = 0
				m.offset = 0
				m.applyFilter()
			}
			return m, nil

		case tea.KeyCtrlU:
			if m.query != "" {
				m.query = ""
				m.cursor = 0
				m.offset = 0
				m.applyFilter()
			}
			return m, nil

		case tea.KeyRunes, tea.KeySpace:
			if msg.Type == tea.KeySpace {
				m.query += " "
			} else {
				m.query += string(msg.Runes)
			}
			m.cursor = 0
			m.offset = 0
			m.applyFilter()
			return m, nil
		}
	}
	return m, nil
}

func (m sessionModel) View() string {
	cardW := m.width - 2
	if cardW > 116 {
		cardW = 116
	}
	if cardW < 64 {
		cardW = 64
	}
	innerW := cardW - 6

	scopeCur := ui.SDim.Render("当前目录")
	scopeAll := ui.SDim.Render("全部目录")
	if !m.showAll {
		scopeCur = ui.BadgeActive.Render("当前目录")
	} else {
		scopeAll = ui.BadgeActive.Render("全部目录")
	}

	leftHead := ui.Brand("agy-pro") + " " + ui.SDim.Render("resume") + "  " + scopeCur + " " + scopeAll
	rightHead := ui.SDim.Render(fmt.Sprintf("%d/%d sessions", len(m.filtered), len(m.all)))
	headLine := leftHead
	if gap := innerW - lipgloss.Width(leftHead) - lipgloss.Width(rightHead); gap > 1 {
		headLine += strings.Repeat(" ", gap) + rightHead
	} else {
		headLine += "  " + rightHead
	}

	searchHint := ui.SDim.Render("直接输入关键词实时搜索（标题 / 预览 / 目录 / ID）...")
	searchLine := ui.SAccentB.Render("❯ ") + searchHint
	if m.query != "" {
		searchLine = ui.SAccentB.Render("❯ ") + ui.SBold.Render(m.query) + ui.SAccent.Render("▏")
	}

	var body strings.Builder
	body.WriteString(headLine + "\n")
	body.WriteString(searchLine + "\n")
	body.WriteString(ui.Hairline(innerW) + "\n")

	now := time.Now()
	if len(m.filtered) == 0 {
		body.WriteString("\n" + ui.SAmber.Render("  没有匹配的会话（按 Tab 切换全部目录，或按 Esc 清空搜索）") + "\n\n")
	} else {
		page := m.pageRows()
		end := m.offset + page
		if end > len(m.filtered) {
			end = len(m.filtered)
		}

		showWS := m.showAll && innerW >= 82
		wsColW := 24
		if !showWS {
			wsColW = 0
		}
		metaW := 18 // steps + age + padding
		titleMaxW := innerW - 6 - metaW - wsColW
		if titleMaxW < 20 {
			titleMaxW = 20
		}

		var rows [][]string
		for idx := m.offset; idx < end; idx++ {
			s := m.filtered[idx]
			isSel := idx == m.cursor

			ptr := " "
			if isSel {
				ptr = ui.SAccentB.Render("❯")
			}
			dot := ui.SDim.Render("○")
			if s.Running {
				dot = ui.SGoodB.Render("●")
			}
			lead := ptr + " " + dot

			title := ui.Truncate(s.Title, titleMaxW)
			if isSel {
				title = ui.SAccentB.Render(title)
			} else if s.Running {
				title = ui.SBold.Render(title)
			}

			stepsCell := ui.SDim.Render(fmt.Sprintf("%d步", s.Steps))
			ageCell := ui.SDim.Render(sessions.FmtRelativeTime(s.ModifiedAt, now))

			if showWS {
				wsCell := ui.SHeadDim.Render(ui.Truncate(s.PrimaryWorkspace(m.homeDir), wsColW))
				rows = append(rows, []string{lead, title, wsCell, stepsCell, ageCell})
			} else {
				rows = append(rows, []string{lead, title, stepsCell, ageCell})
			}
		}

		if showWS {
			headers := []string{"", "SESSION", "WORKSPACE", "STEPS", "TIME"}
			aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight}
			body.WriteString(ui.RenderTable(headers, rows, aligns, innerW, 1))
		} else {
			headers := []string{"", "SESSION", "STEPS", "TIME"}
			aligns := []ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight}
			body.WriteString(ui.RenderTable(headers, rows, aligns, innerW, 1))
		}
		body.WriteByte('\n')

		// Selected session detail strip
		sel := m.filtered[m.cursor]
		body.WriteString(ui.Hairline(innerW) + "\n")
		wsDisp := sel.PrimaryWorkspace(m.homeDir)
		idShort := sel.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		acctInfo := m.activeAccount
		if acctInfo == "" {
			acctInfo = "(自动选号)"
		}
		rightMeta := ui.SDim.Render("  ·  ID ") + ui.SHeadDim.Render(idShort) +
			ui.SDim.Render("  ·  账号 ") + ui.SGood.Render(acctInfo)
		wsMax := innerW - lipgloss.Width("目录  ") - lipgloss.Width(rightMeta)
		if wsMax < 12 {
			wsMax = 12
		}
		line1 := ui.SHeadDim.Render("目录  ") + ui.SDim.Render(ui.Truncate(wsDisp, wsMax)) + rightMeta
		prevText := sel.Preview
		if prevText == "" {
			prevText = sel.Title
		}
		line2 := ui.SHeadDim.Render("预览  ") + ui.SDim.Render(ui.Truncate(prevText, innerW-8))
		body.WriteString(line1 + "\n" + line2)
	}

	card := ui.RenderCard(body.String(), cardW, ui.ColorRule)
	keys := []string{
		ui.KeyHint("↑↓", "选择"),
		ui.KeyHint("tab", "当前/全部目录"),
		ui.KeyHint("enter", "当前号恢复"),
		ui.KeyHint("ctrl+n", "下个号恢复"),
		ui.KeyHint("esc", "清空/退出"),
	}
	footer := "  " + strings.Join(keys, ui.SDim.Render("  ·  "))
	return "\n" + card + "\n" + footer + "\n"
}

// RunSessionPicker opens the interactive BubbleTea session picker and returns the chosen conversation.
func RunSessionPicker(list []sessions.Session, cwd, activeAccount string) (SessionPickResult, error) {
	m := newSessionModel(list, cwd, vault.HomeDir(), activeAccount)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return SessionPickResult{}, err
	}
	fm, ok := final.(sessionModel)
	if !ok {
		return SessionPickResult{}, nil
	}
	return fm.result, nil
}
