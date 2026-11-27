package runner

import (
	"io"
	"os"
	"strings"

	"github.com/Tiggy-Chan/agy-pro/internal/ui"
)

type TabState int

const (
	TabIdle TabState = iota
	TabRunning
	TabSwitching
)

var tabSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	tabIdlePrefix      = "✦"
	tabSwitchingPrefix = "↻"
	tabTitleMaxCols    = 28
)

// TabTitleSync updates the terminal window/tab title via OSC 0 (\x1b]0;...\x07)
// without wrapping agy in a PTY proxy, and cleanly restores the prior title on exit.
type TabTitleSync struct {
	w             io.Writer
	enabled       bool
	pushed        bool
	fallbackTitle string
	customTitle   string
	baselineDB    string
	hasBaseline   bool
	currentTitle  string
	state         TabState
	frame         int
	lastEmitted   string
}

func NewTabTitleSync(w io.Writer, isTTY bool, customTitle, fallbackTitle string) *TabTitleSync {
	enabled := isTTY && w != nil &&
		os.Getenv("AGY_PRO_NO_TITLE") != "1" &&
		!strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")

	s := &TabTitleSync{
		w:             w,
		enabled:       enabled,
		fallbackTitle: sanitizeTabTitle(fallbackTitle),
		customTitle:   sanitizeTabTitle(customTitle),
	}
	if s.fallbackTitle == "" {
		s.fallbackTitle = "agy"
	}
	if s.customTitle != "" {
		s.currentTitle = s.customTitle
	} else {
		s.currentTitle = s.fallbackTitle
	}
	return s
}

func sanitizeTabTitle(raw string) string {
	raw = strings.ToValidUTF8(raw, "")
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// Never emit C0, DEL, or C1 control bytes inside OSC 0
			continue
		default:
			b.WriteRune(r)
		}
	}
	clean := strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
	if clean == "" {
		return ""
	}
	return ui.Truncate(clean, tabTitleMaxCols)
}

func (s *TabTitleSync) ensurePushed() {
	if !s.pushed {
		// Always push the terminal's existing title BEFORE our first OSC 0 write.
		_, _ = s.w.Write([]byte("\x1b[22;0t"))
		s.pushed = true
	}
}

func (s *TabTitleSync) Start() {
	if s == nil || !s.enabled {
		return
	}
	s.ensurePushed()
	s.emitIfChanged()
}

// Update syncs the tab title and live run status from conversation_summaries.db.
// If the user passed --title at launch, it stays pinned until the conversation title changes
// in the DB (e.g. via `/rename <name>` inside agy), at which point the new title takes over.
func (s *TabTitleSync) Update(dbTitle, dbPreview string, running bool) {
	if s == nil || !s.enabled {
		return
	}
	cleanDB := sanitizeTabTitle(dbTitle)
	cleanPrev := sanitizeTabTitle(dbPreview)

	if s.customTitle != "" {
		if !s.hasBaseline {
			if cleanDB != "" {
				s.baselineDB = cleanDB
				s.hasBaseline = true
			}
		} else if cleanDB != "" && cleanDB != s.baselineDB {
			// User renamed the conversation via /rename (or switched sessions): follow live title.
			s.customTitle = ""
			s.currentTitle = cleanDB
		}
	}

	if s.customTitle == "" {
		switch {
		case cleanDB != "":
			s.currentTitle = cleanDB
		case cleanPrev != "":
			s.currentTitle = cleanPrev
		default:
			s.currentTitle = s.fallbackTitle
		}
	}

	if running {
		if s.state != TabRunning {
			s.state = TabRunning
			s.frame = 0
		}
	} else {
		s.state = TabIdle
	}
	s.emitIfChanged()
}

// TickSpinner advances the braille spinner frame smoothly while running (no-op when idle).
func (s *TabTitleSync) TickSpinner() {
	if s == nil || !s.enabled || s.state != TabRunning {
		return
	}
	s.frame = (s.frame + 1) % len(tabSpinnerFrames)
	s.emitIfChanged()
}

// OnConversationSwitch clears any launch-time --title pin when the user switches sessions in-place.
func (s *TabTitleSync) OnConversationSwitch() {
	if s == nil {
		return
	}
	s.customTitle = ""
	s.hasBaseline = false
	s.baselineDB = ""
}

// SetSwitching displays a brief account-switch indicator during 429 failover.
func (s *TabTitleSync) SetSwitching() {
	if s == nil || !s.enabled {
		return
	}
	s.state = TabSwitching
	s.emitIfChanged()
}

func (s *TabTitleSync) CurrentTitle() string {
	if s == nil {
		return ""
	}
	if s.currentTitle != "" {
		return s.currentTitle
	}
	return s.fallbackTitle
}

func (s *TabTitleSync) formatted() string {
	title := s.currentTitle
	if title == "" {
		title = s.fallbackTitle
	}
	switch s.state {
	case TabRunning:
		return tabSpinnerFrames[s.frame%len(tabSpinnerFrames)] + " " + title
	case TabSwitching:
		return tabSwitchingPrefix + " " + title
	default:
		return tabIdlePrefix + " " + title
	}
}

func (s *TabTitleSync) emitIfChanged() {
	txt := s.formatted()
	if txt == s.lastEmitted {
		return
	}
	s.ensurePushed()
	s.lastEmitted = txt
	// Single atomic write (<120 bytes) so kernel TTY lock serializes cleanly against child output.
	_, _ = s.w.Write([]byte("\x1b]0;" + txt + "\x07"))
}

func (s *TabTitleSync) Restore() {
	if s == nil || !s.enabled || !s.pushed {
		return
	}
	s.pushed = false
	s.lastEmitted = ""
	// Pop saved title from xterm/VTE title stack.
	_, _ = s.w.Write([]byte("\x1b[23;0t"))
}
