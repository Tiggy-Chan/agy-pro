package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func fptr(v float64) *float64 { return &v }

func TestDashboardAutoRefreshClearsStaleCooldown(t *testing.T) {
	tmp := t.TempDir()
	v := vault.New(filepath.Join(tmp, "pool"))
	tok := filepath.Join(tmp, "alice.tok")
	raw, _ := json.Marshal(map[string]any{
		"id_token_email": "alice@example.com",
		"token":          map[string]any{"access_token": "a", "refresh_token": "r", "expiry": "2030-01-01T00:00:00Z"},
	})
	_ = os.WriteFile(tok, raw, 0o600)
	if _, err := v.ImportTokenFile(tok); err != nil {
		t.Fatalf("import failed: %v", err)
	}

	now := float64(time.Now().Unix())
	_ = v.UpdateState(func(s *vault.State) {
		s.Active = "alice@example.com"
		s.Cooldowns["alice@example.com"] = &vault.Cooldown{
			Until:  now + 5*86400,
			Reason: "7d quota exhausted",
			At:     now - 3600,
		}
		s.Quotas["alice@example.com"] = &vault.Quota{
			H5Used: fptr(0), H5Rem: fptr(100),
			D7Used: fptr(100), D7Rem: fptr(0), D7Reset: now + 5*86400,
			At: now - 3600,
		}
	})

	m := newModel(v, "/usr/bin/true")
	// 1. Auto-refresh on open must enqueue stale/exhausted account
	next, cmd := m.Update(autoRefreshStartMsg{})
	m = next.(model)
	if cmd == nil || !m.refreshing["alice@example.com"] {
		t.Fatalf("expected autoRefreshStartMsg to start probe for stale exhausted account")
	}

	// 2. When Google returns early reset (100% remaining), probeDoneMsg must clear stale cooldown!
	next, _ = m.Update(probeDoneMsg{
		email: "alice@example.com",
		q: &vault.Quota{
			H5Used: fptr(0), H5Rem: fptr(100), H5Reset: now + 5*3600,
			D7Used: fptr(0), D7Rem: fptr(100), D7Reset: now + 7*86400,
			At: now,
		},
	})
	m = next.(model)
	if m.state.Cooldowns["alice@example.com"] != nil {
		t.Fatalf("expected stale cooldown cleared after live probe confirmed 100%% remaining")
	}
	if !strings.Contains(m.View(), "ACTIVE") || strings.Contains(m.View(), "7D FULL") || strings.Contains(m.View(), "COOLING") {
		t.Fatalf("expected view to reflect recovered ACTIVE status, got:\n%s", m.View())
	}
}

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
