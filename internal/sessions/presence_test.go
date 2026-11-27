package sessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func TestFindOpenAndListOpenDetectsKernelFlockOnly(t *testing.T) {
	tmp := t.TempDir()
	pdir := filepath.Join(tmp, "presence")
	_ = os.MkdirAll(pdir, 0o700)

	openID := "11111111-0000-4000-8000-000000000001"
	closedID := "22222222-0000-4000-8000-000000000002"
	subID := "33333333-0000-4000-8000-000000000003"

	// Create 3 lock files; hold exclusive kernel lock ONLY on openID and subID (simulating live agy + subagent)
	lockOpen, err := os.OpenFile(filepath.Join(pdir, openID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockOpen.Close()
	if err := vault.TryLockFileExclusive(lockOpen); err != nil {
		t.Fatal(err)
	}
	defer vault.UnlockFile(lockOpen)

	lockSub, err := os.OpenFile(filepath.Join(pdir, subID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockSub.Close()
	if err := vault.TryLockFileExclusive(lockSub); err != nil {
		t.Fatal(err)
	}
	defer vault.UnlockFile(lockSub)

	// Closed session has a leftover lock file with NO active flock
	_ = os.WriteFile(filepath.Join(pdir, closedID+".lock"), nil, 0o600)

	ids, err := FindOpenConversationIDs(pdir)
	if err != nil {
		t.Fatalf("FindOpenConversationIDs failed: %v", err)
	}
	if len(ids) != 2 || ids[0] != openID || ids[1] != subID {
		t.Fatalf("expected [openID, subID] locked, got %v", ids)
	}

	// Build summary DB so ListOpen enriches openID and filters out subID (nesting_depth=1)
	dbPath := filepath.Join(tmp, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT, preview TEXT, step_count INTEGER,
			last_modified_time TEXT, workspace_uris TEXT, status TEXT,
			nesting_depth INTEGER DEFAULT 0, parent_conversation_id TEXT DEFAULT ''
		);
	`)
	_, _ = db.Exec(`INSERT INTO conversation_summaries VALUES (?, '活跃主终端', 'preview', 25, '2026-11-27 08:00:00+00:00', '["file:///work/demo"]', 'CASCADE_RUN_STATUS_RUNNING', 0, '')`, openID)
	_, _ = db.Exec(`INSERT INTO conversation_summaries VALUES (?, '历史已关闭会话', 'old', 10, '2026-11-27 07:00:00+00:00', '["file:///work/demo"]', 'CASCADE_RUN_STATUS_IDLE', 0, '')`, closedID)
	_, _ = db.Exec(`INSERT INTO conversation_summaries VALUES (?, '内部子代理', 'sub', 2, '2026-11-27 08:01:00+00:00', '["file:///work/demo"]', 'CASCADE_RUN_STATUS_RUNNING', 1, ?)`, subID, openID)
	_ = db.Close()

	list, err := ListOpen(dbPath, pdir)
	if err != nil {
		t.Fatalf("ListOpen failed: %v", err)
	}
	if len(list) != 1 || list[0].ID != openID || list[0].Title != "活跃主终端" || !list[0].Running || !list[0].Open {
		t.Fatalf("expected ONLY top-level openID, got %+v", list)
	}
}
