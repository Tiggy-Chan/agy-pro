package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func TestDashboardAutoCompactKeybinds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pool")
	v := vault.New(dir)
	m := newModel(v, "/usr/bin/true")

	if got := vault.DisplayAutoCompact(m.state); got != "80%" {
		t.Fatalf("expected default compact 80%%, got %q", got)
	}
	if !strings.Contains(m.View(), "compact 80%") {
		t.Fatalf("expected View() header to show compact 80%%")
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("[")})
	m = next.(model)
	if got := vault.DisplayAutoCompact(m.state); got != "75%" {
		t.Fatalf("expected [ to step down to 75%%, got %q", got)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("]")})
	m = next.(model)
	if got := vault.DisplayAutoCompact(m.state); got != "80%" {
		t.Fatalf("expected ] to step up to 80%%, got %q", got)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(model)
	if got := vault.DisplayAutoCompact(m.state); got != "off" {
		t.Fatalf("expected a to toggle off from 80%%, got %q", got)
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(model)
	if got := vault.DisplayAutoCompact(m.state); got != "80%" {
		t.Fatalf("expected a to restore 80%% from off, got %q", got)
	}
}
