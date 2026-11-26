package sessions

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestParseWorkspaceURIs(t *testing.T) {
	raw := `["file:///tmp/%E6%96%87%E6%A1%A3/project-a","file:///tmp/%E6%96%87%E6%A1%A3/project-a/","file:///opt/app"]`
	got := ParseWorkspaceURIs(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 deduplicated paths, got %v", got)
	}
	if got[0] != "/tmp/文档/project-a" || got[1] != "/opt/app" {
		t.Fatalf("unexpected decoded workspaces: %v", got)
	}
	if len(ParseWorkspaceURIs("")) != 0 || len(ParseWorkspaceURIs("not-json")) != 0 {
		t.Fatalf("expected empty slice for invalid input")
	}
}

func TestListReadsTopLevelSessionsReadOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open temp db failed: %v", err)
	}
	schema := `
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT,
			preview TEXT,
			step_count INTEGER,
			last_modified_time TEXT,
			workspace_uris TEXT,
			status TEXT,
			nesting_depth INTEGER DEFAULT 0,
			parent_conversation_id TEXT DEFAULT ''
		);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create table failed: %v", err)
	}

	inserts := []struct {
		id, title, preview string
		steps              int
		mod, ws, status    string
		depth              int
		parent             string
	}{
		{
			id:      "00000000-0000-4000-8000-000000000001",
			title:   "Build\n  CLI   Picker",
			preview: "Implementing interactive resume",
			steps:   42,
			mod:     "2026-11-26 10:28:35.909127457+00:00",
			ws:      `["file:///tmp/%E6%96%87%E6%A1%A3/demo"]`,
			status:  "CASCADE_RUN_STATUS_RUNNING",
			depth:   0,
			parent:  "",
		},
		{
			id:      "00000000-0000-4000-8000-000000000002",
			title:   "",
			preview: "Fallback preview title",
			steps:   7,
			mod:     "2026-11-25 08:00:00+00:00",
			ws:      `["file:///work/other"]`,
			status:  "CASCADE_RUN_STATUS_IDLE",
			depth:   0,
			parent:  "",
		},
		{
			id:      "00000000-0000-4000-8000-000000000003",
			title:   "Subagent should be hidden",
			preview: "internal",
			steps:   3,
			mod:     "2026-11-26 11:00:00+00:00",
			ws:      `["file:///tmp/%E6%96%87%E6%A1%A3/demo"]`,
			status:  "CASCADE_RUN_STATUS_IDLE",
			depth:   1,
			parent:  "00000000-0000-4000-8000-000000000001",
		},
	}
	for _, r := range inserts {
		if _, err := db.Exec(
			`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.title, r.preview, r.steps, r.mod, r.ws, r.status, r.depth, r.parent,
		); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}
	_ = db.Close()

	list, err := List(dbPath, 50)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 top-level sessions (excluding subagent), got %d", len(list))
	}
	if list[0].ID != "00000000-0000-4000-8000-000000000001" || list[0].Title != "Build CLI Picker" || !list[0].Running || list[0].Steps != 42 {
		t.Fatalf("unexpected first session: %+v", list[0])
	}
	if !list[0].MatchesDir("/tmp/文档/demo") || list[0].MatchesDir("/work/other") {
		t.Fatalf("MatchesDir failed for %+v", list[0].Workspaces)
	}
	if got := list[0].PrimaryWorkspace("/tmp"); got != "~/文档/demo" {
		t.Fatalf("expected ~/文档/demo, got %q", got)
	}
	if list[1].Title != "Fallback preview title" || list[1].Running {
		t.Fatalf("unexpected second session: %+v", list[1])
	}
	if !list[0].MatchesQuery("picker demo") || list[0].MatchesQuery("picker nonexistent") {
		t.Fatalf("MatchesQuery multi-term failed")
	}

	now := time.Date(2026, 11, 26, 10, 35, 0, 0, time.UTC)
	if rel := FmtRelativeTime(list[0].ModifiedAt, now); rel != "6m前" {
		t.Fatalf("expected 6m前, got %q", rel)
	}
}
