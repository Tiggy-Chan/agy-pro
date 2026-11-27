package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Tiggy-Chan/agy-pro/internal/sessions"
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

func TestDashboardDeleteWithConfirm(t *testing.T) {
	tmp := t.TempDir()
	v := vault.New(filepath.Join(tmp, "pool"))
	for _, em := range []string{"alice@example.com", "bob@example.com"} {
		tok := filepath.Join(tmp, em+".tok")
		raw, _ := json.Marshal(map[string]any{
			"id_token_email": em,
			"token":          map[string]any{"access_token": "a", "refresh_token": "r", "expiry": "2030-01-01T00:00:00Z"},
		})
		_ = os.WriteFile(tok, raw, 0o600)
		if _, err := v.ImportTokenFile(tok); err != nil {
			t.Fatalf("import %s failed: %v", em, err)
		}
	}

	m := newModel(v, "/usr/bin/true")
	// Press d -> prompt confirmation, do NOT delete yet
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = next.(model)
	if m.confirmDel != "alice@example.com" || len(m.order) != 2 {
		t.Fatalf("expected confirmDel=alice@example.com with 2 accounts remaining, got confirmDel=%q len=%d", m.confirmDel, len(m.order))
	}
	// Press n -> cancels delete
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = next.(model)
	if m.confirmDel != "" || len(m.order) != 2 {
		t.Fatalf("expected cancel to preserve accounts, got confirmDel=%q len=%d", m.confirmDel, len(m.order))
	}
	// Press d then y -> deletes alice
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = next.(model)
	if len(m.order) != 1 || m.order[0] != "bob@example.com" {
		t.Fatalf("expected alice deleted leaving bob, got %v", m.order)
	}
}

func TestSessionPickerBehavior(t *testing.T) {
	now := time.Now()
	list := []sessions.Session{
		{ID: "11111111-0000-4000-8000-000000000001", Title: "Refactor CLI commands", Preview: "Streamline commands", Steps: 18, ModifiedAt: now.Add(-5 * time.Minute), Workspaces: []string{"/work/app"}, Running: true},
		{ID: "22222222-0000-4000-8000-000000000002", Title: "Fix quota parser", Preview: "Dual pool parser", Steps: 9, ModifiedAt: now.Add(-2 * time.Hour), Workspaces: []string{"/work/app"}},
		{ID: "33333333-0000-4000-8000-000000000003", Title: "Other repo notes", Preview: "Docs update", Steps: 4, ModifiedAt: now.Add(-24 * time.Hour), Workspaces: []string{"/work/other"}},
	}

	// Default opens in current directory scope
	m := newSessionModel(list, "/work/app", "/work", "alice@example.com")
	if m.showAll || len(m.filtered) != 2 {
		t.Fatalf("expected 2 CWD sessions initially, got showAll=%v len=%d", m.showAll, len(m.filtered))
	}
	if !strings.Contains(m.View(), "Refactor CLI commands") {
		t.Fatalf("expected View() to render first session")
	}

	// Tab switches to All workspaces
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(sessionModel)
	if !m.showAll || len(m.filtered) != 3 {
		t.Fatalf("expected Tab to show all 3 sessions, got showAll=%v len=%d", m.showAll, len(m.filtered))
	}

	// Typing filters live
	for _, r := range "other" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(sessionModel)
	}
	if len(m.filtered) != 1 || m.filtered[0].ID != list[2].ID {
		t.Fatalf("expected live search 'other' to match 1 session, got %v", m.filtered)
	}

	// Esc clears query without quitting
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(sessionModel)
	if cmd != nil || m.query != "" || len(m.filtered) != 3 {
		t.Fatalf("expected first Esc to clear search only, got query=%q len=%d cmd=%v", m.query, len(m.filtered), cmd)
	}

	// Down + Ctrl+N selects second session with UseNextAccount=true
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(sessionModel)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = next.(sessionModel)
	if cmd == nil || !m.result.Selected || !m.result.UseNextAccount || m.result.ConversationID != list[1].ID {
		t.Fatalf("expected Ctrl+N to pick second session on next account, got %+v", m.result)
	}

	// If CWD has zero sessions, auto-fallback to All workspaces view
	m2 := newSessionModel(list, "/work/empty", "/work", "alice@example.com")
	if !m2.showAll || len(m2.filtered) != 3 {
		t.Fatalf("expected auto-fallback to showAll when CWD has no sessions, got showAll=%v len=%d", m2.showAll, len(m2.filtered))
	}
}

func TestDashboardTabSessionsMonitor(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "empty-home"))
	v := vault.New(filepath.Join(tmp, "pool"))
	m := newModel(v, "/usr/bin/true")
	now := time.Now()
	m.openCount = 2
	m.liveMeta = map[string]vault.LiveSession{
		"aaaa1111-0000-4000-8000-000000000001": {PID: 1234, ConvID: "aaaa1111-0000-4000-8000-000000000001", Email: "alice@example.com", TTY: "pts/4"},
	}
	m.sessionsList = []sessions.Session{
		{ID: "aaaa1111-0000-4000-8000-000000000001", Title: "后台构建任务", Preview: "build", Steps: 15, ModifiedAt: now.Add(-1 * time.Minute), Workspaces: []string{"/work/app"}, Running: true, Open: true},
		{ID: "bbbb2222-0000-4000-8000-000000000002", Title: "代码审查", Preview: "review", Steps: 5, ModifiedAt: now.Add(-20 * time.Minute), Workspaces: []string{"/work/app"}, Running: false, Open: true},
	}

	// Switch to Live Sessions tab via Tab (manually set tab so reloadSessions on empty HOME doesn't clear synthetic data)
	m.tab = tabSessions
	view := m.View()
	if !strings.Contains(view, "后台构建任务") || !strings.Contains(view, "运行") || !strings.Contains(view, "alice") || !strings.Contains(view, "pts/4") {
		t.Fatalf("expected live sessions view to render running session with account/TTY, got:\n%s", view)
	}

	// Down + Ctrl+N resumes second session on next account
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = next.(model)
	if cmd == nil || m.result.Action != ActionResume || !m.result.UseNextAccount || m.result.ConversationID != "bbbb2222-0000-4000-8000-000000000002" {
		t.Fatalf("expected ActionResume with UseNextAccount=true, got %+v", m.result)
	}
}
