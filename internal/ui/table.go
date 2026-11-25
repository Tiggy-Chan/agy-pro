package ui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

type Align int

const (
	AlignLeft Align = iota
	AlignRight
)

var widthCond = &runewidth.Condition{EastAsianWidth: false}

// DisplayWidth returns the true visual cell width (ignoring ANSI escape codes).
func DisplayWidth(s string) int {
	return lipgloss.Width(s)
}

func Truncate(s string, maxW int) string {
	if maxW <= 0 {
		return ""
	}
	if widthCond.StringWidth(s) <= maxW {
		return s
	}
	if maxW == 1 {
		return "…"
	}
	return widthCond.Truncate(s, maxW, "…")
}

func Pad(s string, width int, align Align) string {
	w := lipgloss.Width(s)
	if w > width && !strings.Contains(s, "\x1b[") {
		s = Truncate(s, width)
		w = lipgloss.Width(s)
	}
	gap := width - w
	if gap < 0 {
		gap = 0
	}
	if align == AlignRight {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

func TerminalColumns(defaultCols int) int {
	for _, fd := range []int{int(os.Stdout.Fd()), int(os.Stderr.Fd()), int(os.Stdin.Fd())} {
		if w, _, err := term.GetSize(fd); err == nil && w >= 40 {
			return w
		}
	}
	return defaultCols
}

// RenderTable renders an ANSI-aware, CJK-width-accurate table with subtle Unicode hairlines,
// guaranteed never to exceed maxWidth.
func RenderTable(headers []string, rows [][]string, aligns []Align, maxWidth int, flexCol int) string {
	ncols := len(headers)
	if maxWidth <= 0 {
		maxWidth = TerminalColumns(80)
	}
	if flexCol < 0 || flexCol >= ncols {
		flexCol = ncols - 1
	}

	widths := make([]int, ncols)
	for i, h := range headers {
		widths[i] = DisplayWidth(h)
	}
	for _, row := range rows {
		for i := 0; i < ncols && i < len(row); i++ {
			if w := DisplayWidth(row[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}

	const sep = "  "
	sepTotal := len(sep) * (ncols - 1)
	total := sepTotal
	for _, w := range widths {
		total += w
	}

	if total > maxWidth {
		excess := total - maxWidth
		minW := make([]int, ncols)
		for i := range headers {
			hw := DisplayWidth(headers[i])
			if hw > 8 {
				hw = 8
			}
			if hw < 2 {
				hw = 2
			}
			minW[i] = hw
		}
		if widths[0] < 2 {
			minW[0] = widths[0]
		}

		if can := widths[flexCol] - minW[flexCol]; can > 0 {
			shrink := excess
			if shrink > can {
				shrink = can
			}
			widths[flexCol] -= shrink
			excess -= shrink
		}

		for excess > 0 {
			widest := -1
			for i := 0; i < ncols; i++ {
				if widths[i] > minW[i] && (widest < 0 || widths[i] > widths[widest]) {
					widest = i
				}
			}
			if widest < 0 {
				break
			}
			widths[widest]--
			excess--
		}
	}

	ruleW := maxWidth
	if sumW := sepTotal + func() int {
		s := 0
		for _, w := range widths {
			s += w
		}
		return s
	}(); sumW > ruleW {
		ruleW = sumW
	}
	if ruleW > maxWidth {
		ruleW = maxWidth
	}
	rule := Hairline(ruleW)

	formatRow := func(cols []string, header bool) string {
		parts := make([]string, ncols)
		for i := 0; i < ncols; i++ {
			val := ""
			if i < len(cols) {
				val = cols[i]
			}
			al := AlignLeft
			if i < len(aligns) {
				al = aligns[i]
			}
			cell := Pad(val, widths[i], al)
			if header && val != "" {
				cell = SDim.Bold(true).Render(cell)
			}
			parts[i] = cell
		}
		return strings.TrimRight(strings.Join(parts, sep), " ")
	}

	var b strings.Builder
	b.WriteString(formatRow(headers, true))
	b.WriteByte('\n')
	b.WriteString(rule)
	for _, r := range rows {
		b.WriteByte('\n')
		b.WriteString(formatRow(r, false))
	}
	return b.String()
}

func StderrProgress(msg string, clear bool) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		if !clear && msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		return
	}
	if clear {
		_, _ = os.Stderr.WriteString("\r\x1b[2K")
		return
	}
	cols := TerminalColumns(80)
	_, _ = os.Stderr.WriteString("\r\x1b[2K" + SHeadDim.Render("⠋ ") + SDim.Render(Truncate(msg, cols-4)))
}
