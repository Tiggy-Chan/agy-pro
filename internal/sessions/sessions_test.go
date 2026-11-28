package sessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
)

func TestParseWorkspaceURIs(t *testing.T) {
	raw := `["file:///tmp/%E6%96%87%E6%A1%A3/project-a","file:///tmp/%E6%96%87%E6%A1%A3/project-a/","file:///opt/app"]`
	got := ParseWorkspaceURIs(raw)
	if len(got) != 2 {
		t.Fatalf("expected 2 deduplicated paths, got %v", got)
	}
	if got[0] != filepath.FromSlash("/tmp/文档/project-a") || got[1] != filepath.FromSlash("/opt/app") {
		t.Fatalf("unexpected decoded workspaces: %v", got)
	}

	// Windows drive-letter file:///C:/... and percent-encoded file:///c%3A/... URIs
	winRaw := `["file:///C:/Users/%E6%96%87%E6%A1%A3/project-a","file:///c%3A/Work/app/"]`
	winGot := ParseWorkspaceURIs(winRaw)
	if len(winGot) != 2 {
		t.Fatalf("expected 2 Windows workspace paths, got %v", winGot)
	}
	if winGot[0] != filepath.FromSlash("C:/Users/文档/project-a") || winGot[1] != filepath.FromSlash("c:/Work/app") {
		t.Fatalf("unexpected decoded Windows workspaces: %v", winGot)
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
		{
			id:      "00000000-0000-4000-8000-000000000004",
			title:   "",
			preview: "",
			steps:   0,
			mod:     "0001-01-01 00:00:00+00:00",
			ws:      `["file:///tmp/%E6%96%87%E6%A1%A3/demo"]`,
			status:  "CASCADE_RUN_STATUS_IDLE",
			depth:   0,
			parent:  "",
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
	if !list[0].MatchesDir(filepath.FromSlash("/tmp/文档/demo")) || list[0].MatchesDir(filepath.FromSlash("/work/other")) {
		t.Fatalf("MatchesDir failed for %+v", list[0].Workspaces)
	}
	if got := list[0].PrimaryWorkspace(filepath.FromSlash("/tmp")); got != filepath.FromSlash("~/文档/demo") {
		t.Fatalf("expected ~/文档/demo, got %q", got)
	}
	if list[1].Title != "Fallback preview title" || list[1].Running {
		t.Fatalf("unexpected second session: %+v", list[1])
	}
	if !list[0].MatchesQuery("picker demo") || list[0].MatchesQuery("picker nonexistent") {
		t.Fatalf("MatchesQuery multi-term failed")
	}

	now := time.Date(2026, 11, 26, 10, 35, 0, 0, time.UTC)
	i18n.SetPref("zh")
	t.Cleanup(func() { i18n.SetPref("auto") })
	if rel := FmtRelativeTime(list[0].ModifiedAt, now); rel != "6m前" {
		t.Fatalf("expected 6m前 in zh, got %q", rel)
	}
	i18n.SetPref("en")
	if rel := FmtRelativeTime(list[0].ModifiedAt, now); rel != "6m ago" {
		t.Fatalf("expected 6m ago in en, got %q", rel)
	}

	// Subagent ID must resolve back to its top-level parent conversation ID
	if root := ResolveRootID(dbPath, "00000000-0000-4000-8000-000000000003"); root != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("expected subagent to resolve to parent 00000000-...-0001, got %q", root)
	}
	if root := ResolveRootID(dbPath, "00000000-0000-4000-8000-000000000001"); root != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("expected top-level session to stay unchanged, got %q", root)
	}

	// Live MetaReader observes status & live /rename title updates
	mr := OpenMetaReader(dbPath)
	defer mr.Close()
	meta, ok := mr.Lookup("00000000-0000-4000-8000-000000000001")
	if !ok || meta.Title != "Build CLI Picker" || !meta.Running {
		t.Fatalf("unexpected MetaReader lookup #1: ok=%v meta=%+v", ok, meta)
	}
	wdb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen db for update failed: %v", err)
	}
	_, _ = wdb.Exec(`UPDATE conversation_summaries SET title = '测试', status = 'CASCADE_RUN_STATUS_IDLE' WHERE conversation_id = ?`, "00000000-0000-4000-8000-000000000001")
	_ = wdb.Close()

	meta2, ok2 := mr.Lookup("00000000-0000-4000-8000-000000000001")
	if !ok2 || meta2.Title != "测试" || meta2.Running {
		t.Fatalf("expected MetaReader to observe /rename '测试' and IDLE, got ok=%v meta=%+v", ok2, meta2)
	}

	// CheckSchema validates required upstream columns and counts
	st := CheckSchema(dbPath)
	if !st.Exists || !st.OK || st.TotalTopLevel != 2 || st.RunningCount != 0 {
		t.Fatalf("unexpected CheckSchema result: %+v", st)
	}

	// ListActiveFirst pins running sessions to the top
	wdb2, _ := sql.Open("sqlite", dbPath)
	_, _ = wdb2.Exec(`UPDATE conversation_summaries SET status = 'CASCADE_RUN_STATUS_RUNNING' WHERE conversation_id = ?`, "00000000-0000-4000-8000-000000000002")
	_ = wdb2.Close()

	activeList, err := ListActiveFirst(dbPath, 10)
	if err != nil || len(activeList) != 2 || activeList[0].ID != "00000000-0000-4000-8000-000000000002" || !activeList[0].Running {
		t.Fatalf("expected running session pinned first, got err=%v list=%+v", err, activeList)
	}
}

func TestCountCompactsAndCacheInvalidation(t *testing.T) {
	tmp := t.TempDir()
	sumDB := filepath.Join(tmp, "conversation_summaries.db")
	convsDir := filepath.Join(tmp, "conversations")
	if err := os.MkdirAll(convsDir, 0o700); err != nil {
		t.Fatalf("mkdir conversations failed: %v", err)
	}

	db, err := sql.Open("sqlite", sumDB)
	if err != nil {
		t.Fatalf("open summary db failed: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT, preview TEXT, step_count INTEGER,
			last_modified_time TEXT, workspace_uris TEXT,
			status TEXT, parent_conversation_id TEXT, nesting_depth INTEGER
		);
		INSERT INTO conversation_summaries VALUES
			('11111111-0000-4000-8000-000000000001', 'Long Session', 'p', 120, '2026-11-28 22:00:00Z', '[]', 'IDLE', '', 0),
			('22222222-0000-4000-8000-000000000002', 'Fresh Session', 'p', 8, '2026-11-28 21:00:00Z', '[]', 'IDLE', '', 0),
			('33333333-0000-4000-8000-000000000003', 'Subagent A', 'p', 5, '2026-11-28 22:00:00Z', '[]', 'IDLE', '11111111-0000-4000-8000-000000000001', 1),
			('44444444-0000-4000-8000-000000000004', 'Subagent B', 'p', 5, '2026-11-28 22:00:00Z', '[]', 'IDLE', '11111111-0000-4000-8000-000000000001', 1);
	`)
	_ = db.Close()
	if err != nil {
		t.Fatalf("seed summary db failed: %v", err)
	}

	conv1 := filepath.Join(convsDir, "11111111-0000-4000-8000-000000000001.db")
	cdb, err := sql.Open("sqlite", conv1)
	if err != nil {
		t.Fatalf("open conv1 db failed: %v", err)
	}
	_, err = cdb.Exec(`
		CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL, status INTEGER NOT NULL DEFAULT 3);
		INSERT INTO steps VALUES (0, 14, 3), (1, 15, 3), (2, 132, 3), (3, 23, 3), (4, 14, 3), (5, 23, 3);
	`)
	_ = cdb.Close()
	if err != nil {
		t.Fatalf("seed conv1 db failed: %v", err)
	}

	// Seed transcript.jsonl for conv1 with two PLANNER_RESPONSE records (must pick the latest!)
	tDir := filepath.Join(tmp, "brain", "11111111-0000-4000-8000-000000000001", ".system_generated", "logs")
	_ = os.MkdirAll(tDir, 0o700)
	tPath := filepath.Join(tDir, "transcript.jsonl")
	_ = os.WriteFile(tPath, []byte(
		`{"step_index":1,"type":"PLANNER_RESPONSE","model":"gemini-3.8-flash","input_tokens":10000,"cache_read_tokens":5000}`+"\n"+
			`{"step_index":2,"type":"GENERIC","content":"tool output"}`+"\n"+
			`{"step_index":4,"type":"PLANNER_RESPONSE","model":"gemini-4-neon-xhigh","input_tokens":7000,"cache_read_tokens":330000}`+"\n",
	), 0o600)

	list, err := List(sumDB, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("List failed: err=%v len=%d", err, len(list))
	}
	if list[0].Turns != 2 || list[0].Compacts != 2 || list[0].ContextTokens != 337000 || list[0].Model != "gemini-4-neon-xhigh" || list[0].Subagents != 2 {
		t.Fatalf("unexpected enriched metrics on conv1: %+v", list[0])
	}
	if list[1].Turns != 0 || list[1].Compacts != 0 || list[1].ContextTokens != 0 || list[1].Subagents != 0 {
		t.Fatalf("unexpected metrics on conv2: %+v", list[1])
	}
	if FmtContextTokens(337000) != "337K" || FmtContextTokens(19615) != "19.6K" || FmtContextTokens(1050000) != "1.05M" || FmtContextTokens(0) != "-" {
		t.Fatalf("unexpected FmtContextTokens outputs")
	}

	// Append a third CHECKPOINT step (simulating manual /compact) and bump mtime so cache refreshes
	cdb2, _ := sql.Open("sqlite", conv1)
	_, _ = cdb2.Exec(`INSERT INTO steps VALUES (6, 23, 3)`)
	_ = cdb2.Close()
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(conv1, future, future)

	if got := CountCompacts(conv1); got != 3 {
		t.Fatalf("expected cache invalidation to observe 3 compacts, got %d", got)
	}
}
