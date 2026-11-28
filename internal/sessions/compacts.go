package sessions

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

// Official agy SQLite step_type enums in conversations/<id>.db:
//   - 14: USER_INPUT (user prompt turn)
//   - 23: CHECKPOINT (both auto-compact and manual `/compact`)
const (
	stepTypeUserInput  = 14
	stepTypeCheckpoint = 23
)

type stepCountsEntry struct {
	modTimeNano int64
	size        int64
	turns       int
	compacts    int
}

type transcriptMetricsEntry struct {
	modTimeNano   int64
	size          int64
	contextTokens int64
	model         string
}

var (
	metricsCacheMu  sync.RWMutex
	stepCountsCache = map[string]stepCountsEntry{}
	transcriptCache = map[string]transcriptMetricsEntry{}
)

func agyDirFor(summaryDBPath string) string {
	if strings.TrimSpace(summaryDBPath) == "" {
		return vault.AgyDir()
	}
	return filepath.Dir(summaryDBPath)
}

func convsDirFor(summaryDBPath string) string {
	if strings.TrimSpace(summaryDBPath) == "" {
		return vault.ConversationsDir()
	}
	return filepath.Join(filepath.Dir(summaryDBPath), "conversations")
}

// FmtContextTokens formats a context window token count compactly (e.g. 1.02M, 337K, 19.6K, 850).
func FmtContextTokens(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 100_000:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ReadStepCounts reads conversations/<id>.db strictly read-only and returns
// (turns, compacts) from steps WHERE step_type IN (14, 23), cached by (mtime, size).
func ReadStepCounts(convDBPath string) (turns int, compacts int) {
	if strings.TrimSpace(convDBPath) == "" {
		return 0, 0
	}
	st, err := os.Stat(convDBPath)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return 0, 0
	}
	modNano := st.ModTime().UnixNano()
	size := st.Size()

	metricsCacheMu.RLock()
	if ent, ok := stepCountsCache[convDBPath]; ok && ent.modTimeNano == modNano && ent.size == size {
		metricsCacheMu.RUnlock()
		return ent.turns, ent.compacts
	}
	metricsCacheMu.RUnlock()

	dsn := vault.SQLiteReadOnlyDSN(convDBPath, 500)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, 0
	}
	defer db.Close()

	rows, err := db.Query(`SELECT step_type, COUNT(*) FROM steps WHERE step_type IN (?, ?) GROUP BY step_type`, stepTypeUserInput, stepTypeCheckpoint)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()

	for rows.Next() {
		var stType, cnt int
		if err := rows.Scan(&stType, &cnt); err != nil {
			continue
		}
		if cnt < 0 {
			cnt = 0
		}
		switch stType {
		case stepTypeUserInput:
			turns = cnt
		case stepTypeCheckpoint:
			compacts = cnt
		}
	}

	metricsCacheMu.Lock()
	stepCountsCache[convDBPath] = stepCountsEntry{
		modTimeNano: modNano,
		size:        size,
		turns:       turns,
		compacts:    compacts,
	}
	metricsCacheMu.Unlock()
	return turns, compacts
}

// CountCompacts returns the compaction checkpoint count (step_type = 23) for convDBPath.
func CountCompacts(convDBPath string) int {
	_, compacts := ReadStepCounts(convDBPath)
	return compacts
}

type transcriptLine struct {
	Type            string `json:"type"`
	Model           string `json:"model"`
	InputTokens     int64  `json:"input_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
}

// ReadLatestTranscriptMetrics tail-reads brain/<id>/.system_generated/logs/transcript.jsonl
// to extract the latest PLANNER_RESPONSE context size (input_tokens + cache_read_tokens)
// and active model name in sub-millisecond time, cached by (mtime, size).
func ReadLatestTranscriptMetrics(transcriptPath string) (contextTokens int64, model string) {
	if strings.TrimSpace(transcriptPath) == "" {
		return 0, ""
	}
	st, err := os.Stat(transcriptPath)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return 0, ""
	}
	modNano := st.ModTime().UnixNano()
	size := st.Size()

	metricsCacheMu.RLock()
	if ent, ok := transcriptCache[transcriptPath]; ok && ent.modTimeNano == modNano && ent.size == size {
		metricsCacheMu.RUnlock()
		return ent.contextTokens, ent.model
	}
	metricsCacheMu.RUnlock()

	f, err := os.Open(transcriptPath)
	if err != nil {
		return 0, ""
	}
	defer f.Close()

	// Fast tail scan: check last 256 KiB first; expand to 1 MiB only if the tail
	// contained huge tool outputs without a PLANNER_RESPONSE line.
	windows := []int64{256 << 10, 1 << 20}
	for _, win := range windows {
		readSize := size
		offset := int64(0)
		if readSize > win {
			offset = size - win
			readSize = win
		}
		buf := make([]byte, readSize)
		n, rerr := f.ReadAt(buf, offset)
		if rerr != nil && rerr != io.EOF {
			break
		}
		buf = buf[:n]
		if offset > 0 {
			// Drop potentially truncated first line
			if nl := bytes.IndexByte(buf, '\n'); nl >= 0 {
				buf = buf[nl+1:]
			}
		}
		ctxTok, mod, found := parseLastPlannerResponse(buf)
		if found {
			contextTokens, model = ctxTok, mod
			break
		}
		if offset == 0 {
			break
		}
	}

	metricsCacheMu.Lock()
	transcriptCache[transcriptPath] = transcriptMetricsEntry{
		modTimeNano:   modNano,
		size:          size,
		contextTokens: contextTokens,
		model:         model,
	}
	metricsCacheMu.Unlock()
	return contextTokens, model
}

func parseLastPlannerResponse(buf []byte) (int64, string, bool) {
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.Contains(line, []byte(`"PLANNER_RESPONSE"`)) {
			continue
		}
		var rec transcriptLine
		if json.Unmarshal(line, &rec) != nil || rec.Type != "PLANNER_RESPONSE" {
			continue
		}
		ctx := rec.InputTokens + rec.CacheReadTokens
		mod := strings.TrimPrefix(strings.TrimSpace(rec.Model), "agy-cli/")
		if ctx > 0 || mod != "" {
			return ctx, mod, true
		}
	}
	return 0, "", false
}

// PopulateCompacts fills Turns and Compacts from convsDir/<id>.db.
func PopulateCompacts(convsDir string, list []Session) {
	if len(list) == 0 || strings.TrimSpace(convsDir) == "" {
		return
	}
	PopulateMetrics(filepath.Dir(convsDir), list)
}

// PopulateMetrics enriches each non-empty Session in-place with Turns, Compacts,
// ContextTokens, and Model from agyDir's read-only conversations/*.db and brain/*/transcript.jsonl.
func PopulateMetrics(agyDir string, list []Session) {
	if len(list) == 0 || strings.TrimSpace(agyDir) == "" {
		return
	}
	convsDir := filepath.Join(agyDir, "conversations")
	brainDir := filepath.Join(agyDir, "brain")

	fillOne := func(i int) {
		id := list[i].ID
		if id == "" || list[i].Steps <= 0 {
			return
		}
		turns, compacts := ReadStepCounts(filepath.Join(convsDir, id+".db"))
		list[i].Turns = turns
		list[i].Compacts = compacts

		tPath := filepath.Join(brainDir, id, ".system_generated", "logs", "transcript.jsonl")
		ctxTok, mod := ReadLatestTranscriptMetrics(tPath)
		list[i].ContextTokens = ctxTok
		list[i].Model = mod
	}

	if len(list) <= 6 {
		for i := range list {
			fillOne(i)
		}
		return
	}

	workers := 8
	if len(list) < workers {
		workers = len(list)
	}
	jobs := make(chan int, len(list))
	for i := range list {
		if list[i].ID != "" && list[i].Steps > 0 {
			jobs <- i
		}
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				fillOne(idx)
			}
		}()
	}
	wg.Wait()
}

func loadSubagentCounts(db *sql.DB) map[string]int {
	counts := map[string]int{}
	if db == nil {
		return counts
	}
	rows, err := db.Query(`
		SELECT parent_conversation_id, COUNT(*)
		FROM conversation_summaries
		WHERE parent_conversation_id IS NOT NULL AND parent_conversation_id != ''
		GROUP BY parent_conversation_id
	`)
	if err != nil {
		return counts
	}
	defer rows.Close()
	for rows.Next() {
		var parent string
		var cnt int
		if rows.Scan(&parent, &cnt) == nil && parent != "" && cnt > 0 {
			counts[parent] = cnt
		}
	}
	return counts
}
