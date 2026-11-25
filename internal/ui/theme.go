package ui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

func init() {
	// Modern Linux/macOS terminals render East-Asian Ambiguous glyphs (● ○ ─ ━ ✓ ✗ · ╭ ╰)
	// as 1 cell even under zh_CN.UTF-8, while CJK ideographs stay 2 cells.
	runewidth.DefaultCondition.EastAsianWidth = false
	// Our palette uses fixed 256-color / TrueColor values, so disable lipgloss/termenv's
	// blocking OSC 11 background-color probe (keeps startup <5ms and prevents TUI startup lag).
	lipgloss.SetHasDarkBackground(true)
}

// Restrained, high-contrast 256-color palette consistent across dark terminals.
var (
	ColorCyan   = lipgloss.Color("45")  // headings, flags, active highlights
	ColorAccent = lipgloss.Color("213") // selection cursor, keycaps, interactive focus
	ColorGood   = lipgloss.Color("114") // healthy, active, ok
	ColorAmber  = lipgloss.Color("179") // warning, moderate-high usage, cooling
	ColorRed    = lipgloss.Color("203") // exhausted, error
	ColorDim    = lipgloss.Color("244") // secondary info, timestamps, hints
	ColorRule   = lipgloss.Color("240") // subtle borders & hairlines
	ColorTrack  = lipgloss.Color("238") // unfilled progress bar track
	ColorDark   = lipgloss.Color("16")  // dark ink on bright badge background
	ColorWhite  = lipgloss.Color("15")  // bright ink on dark/red badge background
)

// Reusable text styles.
var (
	SHead    = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true)
	SHeadDim = lipgloss.NewStyle().Foreground(ColorCyan)
	SAccent  = lipgloss.NewStyle().Foreground(ColorAccent)
	SAccentB = lipgloss.NewStyle().Foreground(ColorAccent).Bold(true)
	SGood    = lipgloss.NewStyle().Foreground(ColorGood)
	SGoodB   = lipgloss.NewStyle().Foreground(ColorGood).Bold(true)
	SAmber   = lipgloss.NewStyle().Foreground(ColorAmber)
	SAmberB  = lipgloss.NewStyle().Foreground(ColorAmber).Bold(true)
	SRed     = lipgloss.NewStyle().Foreground(ColorRed)
	SRedB    = lipgloss.NewStyle().Foreground(ColorRed).Bold(true)
	SDim     = lipgloss.NewStyle().Foreground(ColorDim)
	SRule    = lipgloss.NewStyle().Foreground(ColorRule)
	STrack   = lipgloss.NewStyle().Foreground(ColorTrack)
	SBold    = lipgloss.NewStyle().Bold(true)
)

// Status pill badges.
var (
	BadgeActive = lipgloss.NewStyle().
			Background(ColorGood).Foreground(ColorDark).Bold(true).Padding(0, 1)
	BadgeFull = lipgloss.NewStyle().
			Background(ColorRed).Foreground(ColorWhite).Bold(true).Padding(0, 1)
	BadgeCooling = lipgloss.NewStyle().
			Background(ColorAmber).Foreground(ColorDark).Bold(true).Padding(0, 1)
	BadgePass = lipgloss.NewStyle().
			Background(ColorGood).Foreground(ColorDark).Bold(true).Padding(0, 1)
	BadgeFail = lipgloss.NewStyle().
			Background(ColorRed).Foreground(ColorWhite).Bold(true).Padding(0, 1)
	BadgeTag = lipgloss.NewStyle().
			Foreground(ColorDim)
)

// Cool electric sky -> indigo -> violet brand gradient for `agy-pro`.
var brandStops = []string{"#38BDF8", "#60A5FA", "#818CF8", "#A78BFA", "#C084FC"}

type rgb struct{ r, g, b float64 }

func parseHex(s string) rgb {
	var r, g, b int
	_, _ = fmt.Sscanf(strings.TrimPrefix(s, "#"), "%02x%02x%02x", &r, &g, &b)
	return rgb{float64(r), float64(g), float64(b)}
}

func gradientAt(stops []string, t float64) string {
	if len(stops) == 1 {
		return stops[0]
	}
	t = math.Max(0, math.Min(1, t))
	seg := t * float64(len(stops)-1)
	i := int(seg)
	if i > len(stops)-2 {
		i = len(stops) - 2
	}
	f := seg - float64(i)
	a, b := parseHex(stops[i]), parseHex(stops[i+1])
	clamp := func(v float64) int {
		x := int(math.Round(v))
		if x < 0 {
			return 0
		}
		if x > 255 {
			return 255
		}
		return x
	}
	return fmt.Sprintf("#%02X%02X%02X",
		clamp(a.r+(b.r-a.r)*f),
		clamp(a.g+(b.g-a.g)*f),
		clamp(a.b+(b.b-a.b)*f),
	)
}

// Brand renders the `agy-pro` wordmark with a crisp horizontal gradient.
func Brand(text string) string {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return ""
	}
	var b strings.Builder
	for i, ch := range runes {
		if ch == ' ' {
			b.WriteRune(ch)
			continue
		}
		t := 0.0
		if n > 1 {
			t = float64(i) / float64(n-1)
		}
		col := lipgloss.Color(gradientAt(brandStops, t))
		b.WriteString(lipgloss.NewStyle().Foreground(col).Bold(true).Render(string(ch)))
	}
	return b.String()
}

// UsageStyle chooses a semantic color based on percentage used (0..100).
func UsageStyle(used float64) lipgloss.Style {
	switch {
	case used >= 90:
		return SRedB
	case used >= 60:
		return SAmber
	default:
		return SGood
	}
}

// RenderBar renders a crisp visual usage micro-bar (e.g. `━━━─────  35%`).
func RenderBar(used *float64, barWidth int) string {
	if barWidth < 4 {
		barWidth = 4
	}
	if used == nil {
		return STrack.Render(strings.Repeat("─", barWidth)) + " " + SDim.Render("  - ")
	}
	v := *used
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	filled := int(math.Round(v / 100.0 * float64(barWidth)))
	if v > 0 && filled == 0 {
		filled = 1
	}
	if filled > barWidth {
		filled = barWidth
	}
	st := UsageStyle(v)
	bar := st.Render(strings.Repeat("━", filled)) + STrack.Render(strings.Repeat("─", barWidth-filled))
	pct := st.Render(fmt.Sprintf("%3.0f%%", v))
	return bar + " " + pct
}

// RenderCompactPct renders a single colorized percentage string like `23%` or `-`.
func RenderCompactPct(used *float64) string {
	if used == nil {
		return SDim.Render("-")
	}
	return UsageStyle(*used).Render(fmt.Sprintf("%.0f%%", *used))
}

// RenderCard wraps content inside a sleek rounded card bounded to maxWidth.
func RenderCard(content string, width int, borderColor lipgloss.Color) string {
	if width < 40 {
		width = 40
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 2).
		Width(width - 2).
		Render(content)
}

// Hairline renders a subtle dim horizontal divider line.
func Hairline(width int) string {
	if width < 1 {
		return ""
	}
	return SRule.Render(strings.Repeat("─", width))
}

// KeyHint renders a styled keycap + dim action label (e.g. `enter launch`).
func KeyHint(key, desc string) string {
	return SAccentB.Render(key) + " " + SDim.Render(desc)
}
