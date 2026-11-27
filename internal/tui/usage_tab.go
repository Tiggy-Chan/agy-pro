package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
	"github.com/Tiggy-Chan/agy-pro/internal/ui"
	"github.com/Tiggy-Chan/agy-pro/internal/usage"
)

type usageLoadedMsg struct {
	rep       usage.Report
	autoTried bool
	manual    bool
}

func scanUsageCmd(sc *usage.Scanner, rk usage.RangeKind, autoTried, forceRescan bool) tea.Cmd {
	return func() tea.Msg {
		if forceRescan && sc != nil {
			sc.ClearCache()
		}
		rep := sc.Scan(rk, time.Now())
		return usageLoadedMsg{rep: rep, autoTried: autoTried, manual: forceRescan}
	}
}

func renderShareBar(share float64, width int) string {
	if width < 4 {
		width = 4
	}
	if share < 0 {
		share = 0
	}
	if share > 100 {
		share = 100
	}
	filled := int(share/100.0*float64(width) + 0.5)
	if filled == 0 && share > 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return ui.SAccent.Render(strings.Repeat("━", filled)) +
		ui.SDim.Render(strings.Repeat("─", width-filled)) +
		" " + ui.SDim.Render(fmt.Sprintf("%3.0f%%", share))
}

func (m model) renderUsageBody(innerW int) string {
	if m.usageLoading && m.usageReport == nil {
		return "\n  " + m.spinner.View() + " " + ui.SHeadDim.Render(i18n.T("usage.loading")) + "\n"
	}
	rep := m.usageReport
	if rep == nil || rep.Total.Calls == 0 {
		return "\n" + ui.SAmber.Render(i18n.T("usage.empty", m.usageRange.Label())) + "\n"
	}

	var b strings.Builder

	// ---- KPI Summary Strip ----
	tot := rep.Total
	kpi1 := ui.SHeadDim.Render(i18n.T("usage.kpi.total")) + ui.SBold.Render(usage.FmtTokens(tot.TotalTokens())) +
		ui.SDim.Render(i18n.T("usage.kpi.tokens_calls")) + ui.SAccentB.Render(fmt.Sprintf("%d", tot.Calls)) + ui.SDim.Render(i18n.T("usage.kpi.calls_unit")) +
		ui.SBold.Render(fmt.Sprintf("%d", rep.ActiveConvs)) + ui.SDim.Render(i18n.T("usage.kpi.convs_unit"))
	if m.usageLoading {
		kpi1 += "  " + m.spinner.View()
	} else {
		kpi1 += ui.SDim.Render(fmt.Sprintf("  (%dms)", rep.ElapsedMs))
	}

	hitPct := tot.CacheHitPct()
	hitStyle := ui.SGood
	if hitPct < 50 {
		hitStyle = ui.SAmber
	}
	outInfo := ui.SBold.Render(usage.FmtTokens(tot.Output))
	if tot.Thinking > 0 {
		outInfo += ui.SDim.Render(i18n.T("usage.kpi.thinking", tot.ThinkingPct()))
	}
	kpi2 := ui.SHeadDim.Render(i18n.T("usage.kpi.input")) + ui.SBold.Render(usage.FmtTokens(tot.Input)) +
		ui.SDim.Render(i18n.T("usage.kpi.cache_read")) + ui.SBold.Render(usage.FmtTokens(tot.CacheRead)) +
		" " + hitStyle.Render(fmt.Sprintf("%.1f%%", hitPct)) +
		ui.SDim.Render(i18n.T("usage.kpi.output")) + outInfo

	b.WriteString("  " + kpi1 + "\n")
	b.WriteString("  " + kpi2 + "\n")
	b.WriteString(ui.Hairline(innerW) + "\n")

	// ---- By Model Table (up to 6 rows; roll up tail into "other (+N)" so sums always equal KPI total) ----
	displayModels := rep.ByModel
	if len(rep.ByModel) > 6 {
		displayModels = make([]usage.ModelRow, 0, 6)
		displayModels = append(displayModels, rep.ByModel[:5]...)
		var rest usage.BucketStats
		for _, mr := range rep.ByModel[5:] {
			rest.Calls += mr.Stats.Calls
			rest.Input += mr.Stats.Input
			rest.CacheRead += mr.Stats.CacheRead
			rest.CacheWrite += mr.Stats.CacheWrite
			rest.Output += mr.Stats.Output
			rest.Thinking += mr.Stats.Thinking
		}
		displayModels = append(displayModels, usage.ModelRow{
			Model: i18n.T("usage.other_models", len(rep.ByModel)-5),
			Stats: rest,
		})
	}
	compact := innerW < 88
	barW := 6
	if compact {
		barW = 4
	}
	grandTotal := float64(tot.TotalTokens())
	if grandTotal <= 0 {
		grandTotal = 1
	}

	var mRows [][]string
	for i, mr := range displayModels {
		share := float64(mr.Stats.TotalTokens()) * 100.0 / grandTotal
		nameCell := ui.SHeadDim.Render(ui.Truncate(mr.Model, 22))
		if i == 0 {
			nameCell = ui.SBold.Render(ui.Truncate(mr.Model, 22))
		}
		callsCell := ui.SDim.Render(fmt.Sprintf("%d", mr.Stats.Calls))
		totCell := ui.SBold.Render(usage.FmtTokens(mr.Stats.TotalTokens()))
		inCell := usage.FmtTokens(mr.Stats.Input)
		crCell := ui.SDim.Render(usage.FmtTokens(mr.Stats.CacheRead))
		hitCell := ui.SGood.Render(fmt.Sprintf("%.0f%%", mr.Stats.CacheHitPct()))
		outCell := usage.FmtTokens(mr.Stats.Output)
		shareCell := renderShareBar(share, barW)
		if compact {
			mRows = append(mRows, []string{nameCell, callsCell, totCell, outCell, shareCell})
		} else {
			mRows = append(mRows, []string{nameCell, callsCell, totCell, inCell, crCell, hitCell, outCell, shareCell})
		}
	}
	if compact {
		b.WriteString(ui.RenderTable(
			[]string{"MODEL", "CALLS", "TOTAL", "OUTPUT", "SHARE"},
			mRows,
			[]ui.Align{ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignLeft},
			innerW, 0,
		))
	} else {
		b.WriteString(ui.RenderTable(
			[]string{"MODEL", "CALLS", "TOTAL", "INPUT", "CACHE", "HIT%", "OUTPUT", "SHARE"},
			mRows,
			[]ui.Align{ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignLeft},
			innerW, 0,
		))
	}
	b.WriteByte('\n')

	// ---- Top Sessions Table (Top 5, selectable via ↑↓ & Enter to resume) ----
	if len(rep.BySession) > 0 {
		b.WriteString(ui.Hairline(innerW) + "\n")
		maxSess := 5
		if len(rep.BySession) < maxSess {
			maxSess = len(rep.BySession)
		}
		titleMaxW := innerW - 40
		if titleMaxW < 18 {
			titleMaxW = 18
		}
		var sRows [][]string
		for i := 0; i < maxSess; i++ {
			sr := rep.BySession[i]
			isSel := i == m.usageCursor
			ptr := " "
			if isSel {
				ptr = ui.SAccentB.Render("❯")
			}
			title := ui.Truncate(sr.Title, titleMaxW)
			if isSel {
				title = ui.SAccentB.Render(title)
			}
			sRows = append(sRows, []string{
				ptr,
				title,
				ui.SDim.Render(i18n.T("usage.calls_short", sr.Stats.Calls)),
				ui.SBold.Render(usage.FmtTokens(sr.Stats.TotalTokens())),
				ui.SGood.Render(fmt.Sprintf("%.0f%%", sr.Stats.CacheHitPct())),
				ui.SDim.Render(usage.FmtTokens(sr.Stats.Output)),
			})
		}
		b.WriteString(ui.RenderTable(
			[]string{"", "TOP SESSIONS", "CALLS", "TOTAL", "HIT%", "OUTPUT"},
			sRows,
			[]ui.Align{ui.AlignLeft, ui.AlignLeft, ui.AlignRight, ui.AlignRight, ui.AlignRight, ui.AlignRight},
			innerW, 1,
		))
	}

	_ = lipgloss.Width("")
	return b.String()
}
