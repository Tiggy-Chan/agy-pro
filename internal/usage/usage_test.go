package usage

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendTag(dst []byte, num, wt uint64) []byte {
	return appendVarint(dst, (num<<3)|wt)
}

func appendVarintField(dst []byte, num, v uint64) []byte {
	dst = appendTag(dst, num, 0)
	return appendVarint(dst, v)
}

func appendBytesField(dst []byte, num uint64, b []byte) []byte {
	dst = appendTag(dst, num, 2)
	dst = appendVarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func buildStepMeta(ts int64) []byte {
	var tsMsg []byte
	tsMsg = appendVarintField(tsMsg, 1, uint64(ts))
	return appendBytesField(nil, 1, tsMsg)
}

func buildGenMeta(stepIdx int, model string, inp, out, cw, cr, think int64) []byte {
	var stats []byte
	stats = appendVarintField(stats, 2, uint64(inp))
	stats = appendVarintField(stats, 3, uint64(out))
	if cw > 0 {
		stats = appendVarintField(stats, 4, uint64(cw))
	}
	if cr > 0 {
		stats = appendVarintField(stats, 5, uint64(cr))
	}
	if think > 0 {
		stats = appendVarintField(stats, 9, uint64(think))
	}

	var f1 []byte
	f1 = appendBytesField(f1, 4, stats)
	f1 = appendBytesField(f1, 19, []byte(model))

	var stepPacked []byte
	stepPacked = appendVarint(stepPacked, uint64(stepIdx))

	var root []byte
	root = appendBytesField(root, 1, f1)
	root = appendBytesField(root, 2, stepPacked)
	return root
}

func TestUsageScannerAggregatesModelsSessionsAndRanges(t *testing.T) {
	tmp := t.TempDir()
	convDir := filepath.Join(tmp, "conversations")
	_ = os.MkdirAll(convDir, 0o700)
	sumDB := filepath.Join(tmp, "conversation_summaries.db")

	now := time.Date(2026, 11, 27, 17, 0, 0, 0, time.Local)
	tsToday := now.Add(-2 * time.Hour).Unix()
	ts3Days := now.Add(-72 * time.Hour).Unix()
	tsOld := now.Add(-20 * 24 * time.Hour).Unix()

	mainID := "11111111-0000-4000-8000-000000000001"
	subID := "22222222-0000-4000-8000-000000000002"

	// Summary DB maps mainID title and subID -> parent mainID
	sdb, err := sql.Open("sqlite", sumDB)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sdb.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			title TEXT, preview TEXT,
			parent_conversation_id TEXT DEFAULT '',
			nesting_depth INTEGER DEFAULT 0
		);
	`)
	_, _ = sdb.Exec(`INSERT INTO conversation_summaries VALUES (?, '核心架构重构', 'preview', '', 0)`, mainID)
	_, _ = sdb.Exec(`INSERT INTO conversation_summaries VALUES (?, '子任务', '', ?, 1)`, subID, mainID)
	_ = sdb.Close()

	// Main conversation DB has 3 calls spanning today, 3 days ago, and 20 days ago
	mdb, err := sql.Open("sqlite", filepath.Join(convDir, mainID+".db"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = mdb.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY, metadata BLOB);`)
	_, _ = mdb.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB);`)
	_, _ = mdb.Exec(`INSERT INTO steps VALUES (1, ?), (2, ?), (3, ?)`, buildStepMeta(tsOld), buildStepMeta(ts3Days), buildStepMeta(tsToday))
	_, _ = mdb.Exec(`INSERT INTO gen_metadata VALUES (0, ?), (1, ?), (2, ?)`,
		buildGenMeta(1, "gemini-3.8-flash-n", 1000, 200, 0, 9000, 50),
		buildGenMeta(2, "agy-cli/gemini-4-neon", 5000, 1000, 0, 45000, 400),
		buildGenMeta(3, "gemini-4-neon", 2000, 500, 0, 18000, 200),
	)
	_ = mdb.Close()

	// Subagent DB has 1 call today — must roll up into mainID!
	subdb, err := sql.Open("sqlite", filepath.Join(convDir, subID+".db"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = subdb.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY, metadata BLOB);`)
	_, _ = subdb.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB);`)
	_, _ = subdb.Exec(`INSERT INTO steps VALUES (1, ?)`, buildStepMeta(tsToday))
	_, _ = subdb.Exec(`INSERT INTO gen_metadata VALUES (0, ?)`, buildGenMeta(1, "gemini-4-neon", 500, 100, 0, 4500, 40))
	_ = subdb.Close()

	sc := NewScanner(convDir, sumDB)

	// 1. Today: should see 2 calls (main today + subagent today), rolled into 1 session (mainID)
	repToday := sc.Scan(RangeToday, now)
	if repToday.Total.Calls != 2 || repToday.Total.Input != 2500 || repToday.Total.CacheRead != 22500 || repToday.Total.Output != 600 || repToday.Total.Thinking != 240 {
		t.Fatalf("unexpected Today totals: %+v", repToday.Total)
	}
	if len(repToday.BySession) != 1 || repToday.BySession[0].ConvID != mainID || repToday.BySession[0].Title != "核心架构重构" {
		t.Fatalf("expected subagent rolled into mainID, got %+v", repToday.BySession)
	}

	// 2. 7 Days: should see 3 calls, normalized model 'gemini-4-neon' combining 'agy-cli/gemini-4-neon'
	rep7d := sc.Scan(Range7Days, now)
	if rep7d.Total.Calls != 3 || len(rep7d.ByModel) != 1 || rep7d.ByModel[0].Model != "gemini-4-neon" {
		t.Fatalf("unexpected 7d report: total=%+v models=%+v", rep7d.Total, rep7d.ByModel)
	}

	// 3. All: should see all 4 calls across 2 models
	repAll := sc.Scan(RangeAll, now)
	if repAll.Total.Calls != 4 || len(repAll.ByModel) != 2 {
		t.Fatalf("unexpected All report: total=%+v models=%+v", repAll.Total, repAll.ByModel)
	}
	if FmtTokens(1_454_413_413) != "1.45B" || FmtTokens(72_763_694) != "72.8M" || FmtTokens(630_647) != "631K" {
		t.Fatalf("unexpected FmtTokens formatting")
	}
}
