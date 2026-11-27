package usage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Tiggy-Chan/agy-pro/internal/sessions"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

type RangeKind int

const (
	RangeToday RangeKind = iota
	Range7Days
	RangeAll
)

func (r RangeKind) Label() string {
	switch r {
	case RangeToday:
		return "今日"
	case Range7Days:
		return "近 7 天"
	default:
		return "全部"
	}
}

type CallRecord struct {
	Timestamp  int64
	Model      string
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	Thinking   int64
}

type BucketStats struct {
	Calls      int
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	Thinking   int64
}

func (b *BucketStats) Add(c CallRecord) {
	b.Calls++
	b.Input += c.Input
	b.CacheRead += c.CacheRead
	b.CacheWrite += c.CacheWrite
	b.Output += c.Output
	b.Thinking += c.Thinking
}

func (b BucketStats) TotalInput() int64 {
	return b.Input + b.CacheRead + b.CacheWrite
}

func (b BucketStats) TotalTokens() int64 {
	return b.TotalInput() + b.Output
}

func (b BucketStats) CacheHitPct() float64 {
	ti := b.TotalInput()
	if ti <= 0 {
		return 0
	}
	return float64(b.CacheRead) * 100.0 / float64(ti)
}

func (b BucketStats) ThinkingPct() float64 {
	if b.Output <= 0 {
		return 0
	}
	return float64(b.Thinking) * 100.0 / float64(b.Output)
}

type ModelRow struct {
	Model string
	Stats BucketStats
}

type SessionRow struct {
	ConvID string
	Title  string
	Stats  BucketStats
	LastAt time.Time
}

type Report struct {
	Range       RangeKind
	Total       BucketStats
	ActiveConvs int
	ByModel     []ModelRow
	BySession   []SessionRow
	ScannedDBs  int
	ElapsedMs   int64
}

type fileCacheEntry struct {
	size    int64
	modNano int64
	records []CallRecord
}

// Scanner caches parsed per-conversation token records by (size, mtime) so switching
// time ranges in `agy-pro ui` is instantaneous (<1ms) and refreshes stay sub-100ms.
type Scanner struct {
	convDir   string
	summaryDB string
	mu        sync.Mutex
	cache     map[string]fileCacheEntry
}

func NewScanner(convDir, summaryDB string) *Scanner {
	if convDir == "" {
		convDir = vault.ConversationsDir()
	}
	if summaryDB == "" {
		summaryDB = sessions.DefaultDBPath()
	}
	return &Scanner{
		convDir:   convDir,
		summaryDB: summaryDB,
		cache:     map[string]fileCacheEntry{},
	}
}

// FmtTokens formats a token count compactly (e.g. 1.45B, 72.8M, 630.6K, 919).
func FmtTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 100_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	case n >= 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
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

func normalizeModelName(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "agy-cli/")
	if s == "" {
		return "unknown"
	}
	return s
}

func readVarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, 0
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

type pbField struct {
	num    uint64
	wt     uint64
	varint uint64
	bytes  []byte
}

func parsePB(b []byte) []pbField {
	var out []pbField
	for len(b) > 0 {
		tag, n := readVarint(b)
		if n <= 0 {
			break
		}
		b = b[n:]
		num, wt := tag>>3, tag&7
		if num == 0 {
			break
		}
		f := pbField{num: num, wt: wt}
		switch wt {
		case 0:
			v, m := readVarint(b)
			if m <= 0 {
				return out
			}
			f.varint = v
			b = b[m:]
		case 1:
			if len(b) < 8 {
				return out
			}
			b = b[8:]
		case 2:
			l, m := readVarint(b)
			if m <= 0 || l > uint64(len(b[m:])) {
				return out
			}
			f.bytes = b[m : m+int(l)]
			b = b[m+int(l):]
		case 5:
			if len(b) < 4 {
				return out
			}
			b = b[4:]
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}

func parseStepTimestamp(meta []byte) int64 {
	for _, f := range parsePB(meta) {
		if f.num == 1 && f.wt == 2 {
			for _, sf := range parsePB(f.bytes) {
				if sf.num == 1 && sf.wt == 0 && sf.varint >= 1_500_000_000 && sf.varint <= 2_500_000_000 {
					return int64(sf.varint)
				}
			}
		}
	}
	return 0
}

// ParseGenMetadataRecord decodes one `gen_metadata.data` protobuf blob into a CallRecord + first step index.
func ParseGenMetadataRecord(data []byte) (CallRecord, int, bool) {
	var rec CallRecord
	stepIdx := -1
	foundUsage := false

	for _, f := range parsePB(data) {
		switch {
		case f.num == 2 && f.wt == 2 && len(f.bytes) > 0:
			// Packed varint step_indices
			if v, n := readVarint(f.bytes); n > 0 {
				stepIdx = int(v)
			}
		case f.num == 2 && f.wt == 0:
			if stepIdx < 0 {
				stepIdx = int(f.varint)
			}
		case f.num == 1 && f.wt == 2:
			for _, f2 := range parsePB(f.bytes) {
				switch {
				case f2.num == 19 && f2.wt == 2:
					rec.Model = normalizeModelName(string(f2.bytes))
				case f2.num == 4 && f2.wt == 2:
					for _, f3 := range parsePB(f2.bytes) {
						if f3.wt != 0 {
							continue
						}
						switch f3.num {
						case 2:
							rec.Input = int64(f3.varint)
						case 3:
							rec.Output = int64(f3.varint)
						case 4:
							rec.CacheWrite = int64(f3.varint)
						case 5:
							rec.CacheRead = int64(f3.varint)
						case 9:
							rec.Thinking = int64(f3.varint)
						}
					}
					if rec.Input > 0 || rec.Output > 0 || rec.CacheRead > 0 || rec.CacheWrite > 0 {
						foundUsage = true
					}
				}
			}
		}
	}
	if !foundUsage {
		return CallRecord{}, -1, false
	}
	if rec.Model == "" {
		rec.Model = "unknown"
	}
	return rec, stepIdx, true
}

func scanConversationDB(dbPath string, fallbackTS int64) []CallRecord {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(500)&_pragma=query_only(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil
	}
	defer db.Close()

	stepTS := map[int]int64{}
	if sRows, sErr := db.Query(`SELECT idx, metadata FROM steps WHERE metadata IS NOT NULL`); sErr == nil {
		for sRows.Next() {
			var idx int
			var meta []byte
			if sRows.Scan(&idx, &meta) == nil {
				if ts := parseStepTimestamp(meta); ts > 0 {
					stepTS[idx] = ts
				}
			}
		}
		_ = sRows.Close()
	}

	gRows, err := db.Query(`SELECT data FROM gen_metadata WHERE data IS NOT NULL`)
	if err != nil {
		return nil
	}
	defer gRows.Close()

	var out []CallRecord
	for gRows.Next() {
		var data []byte
		if gRows.Scan(&data) != nil {
			continue
		}
		rec, sIdx, ok := ParseGenMetadataRecord(data)
		if !ok {
			continue
		}
		if sIdx >= 0 {
			if ts, found := stepTS[sIdx]; found {
				rec.Timestamp = ts
			}
		}
		if rec.Timestamp == 0 {
			rec.Timestamp = fallbackTS
		}
		out = append(out, rec)
	}
	return out
}

type summaryInfo struct {
	title    string
	parentID string
	depth    int
}

func loadSummaryMap(dbPath string) map[string]summaryInfo {
	out := map[string]summaryInfo{}
	if _, err := os.Stat(dbPath); err != nil {
		return out
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return out
	}
	defer db.Close()

	rows, err := db.Query(`SELECT conversation_id, COALESCE(title, ''), COALESCE(preview, ''), COALESCE(parent_conversation_id, ''), COALESCE(nesting_depth, 0) FROM conversation_summaries`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, preview, parent string
		var depth int
		if rows.Scan(&id, &title, &preview, &parent, &depth) != nil {
			continue
		}
		t := strings.TrimSpace(strings.Join(strings.Fields(title), " "))
		if t == "" {
			t = strings.TrimSpace(strings.Join(strings.Fields(preview), " "))
		}
		out[id] = summaryInfo{title: t, parentID: strings.TrimSpace(parent), depth: depth}
	}
	return out
}

func resolveRootFromMap(id string, m map[string]summaryInfo) string {
	cur := id
	seen := map[string]bool{cur: true}
	for i := 0; i < 8; i++ {
		info, ok := m[cur]
		if !ok || info.depth <= 0 || info.parentID == "" || seen[info.parentID] {
			break
		}
		seen[info.parentID] = true
		cur = info.parentID
	}
	return cur
}

func cutoffForRange(rk RangeKind, now time.Time) int64 {
	switch rk {
	case RangeToday:
		loc := now.Location()
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		return start.Unix()
	case Range7Days:
		return now.Add(-7 * 24 * time.Hour).Unix()
	default:
		return 0
	}
}

// Scan aggregates local token usage across all conversations/*.db for the requested range.
// Safe to call concurrently or repeatedly; unchanged DB files are served from in-memory cache.
func (s *Scanner) Scan(rk RangeKind, now time.Time) Report {
	t0 := time.Now()
	rep := Report{Range: rk}
	cutoff := cutoffForRange(rk, now)

	entries, err := os.ReadDir(s.convDir)
	if err != nil {
		rep.ElapsedMs = time.Since(t0).Milliseconds()
		return rep
	}

	type dbJob struct {
		id      string
		path    string
		size    int64
		modNano int64
		modUnix int64
	}
	var jobs []dbJob
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".db")
		if len(id) != 36 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Account for WAL file mtime when checking whether DB changed or falls in range
		modTime := info.ModTime()
		size := info.Size()
		full := filepath.Join(s.convDir, e.Name())
		if walInfo, walErr := os.Stat(full + "-wal"); walErr == nil {
			if walInfo.ModTime().After(modTime) {
				modTime = walInfo.ModTime()
			}
			size += walInfo.Size()
		}
		if cutoff > 0 && modTime.Unix() < cutoff {
			continue
		}
		jobs = append(jobs, dbJob{
			id:      id,
			path:    full,
			size:    size,
			modNano: modTime.UnixNano(),
			modUnix: modTime.Unix(),
		})
	}

	type dbResult struct {
		id      string
		records []CallRecord
	}
	resCh := make(chan dbResult, len(jobs))
	jobCh := make(chan dbJob, len(jobs))
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)

	workers := 6
	if len(jobs) < workers {
		workers = len(jobs)
	}
	if workers < 1 {
		workers = 1
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				s.mu.Lock()
				cached, hit := s.cache[j.path]
				s.mu.Unlock()
				if hit && cached.size == j.size && cached.modNano == j.modNano {
					resCh <- dbResult{id: j.id, records: cached.records}
					continue
				}
				recs := scanConversationDB(j.path, j.modUnix)
				s.mu.Lock()
				s.cache[j.path] = fileCacheEntry{size: j.size, modNano: j.modNano, records: recs}
				s.mu.Unlock()
				resCh <- dbResult{id: j.id, records: recs}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(resCh)
	}()

	summaries := loadSummaryMap(s.summaryDB)
	byModel := map[string]*BucketStats{}
	bySession := map[string]*SessionRow{}

	for r := range resCh {
		rootID := resolveRootFromMap(r.id, summaries)
		for _, rec := range r.records {
			if cutoff > 0 && rec.Timestamp < cutoff {
				continue
			}
			rep.Total.Add(rec)
			ms, ok := byModel[rec.Model]
			if !ok {
				ms = &BucketStats{}
				byModel[rec.Model] = ms
			}
			ms.Add(rec)

			sr, ok := bySession[rootID]
			if !ok {
				title := summaries[rootID].title
				if title == "" {
					title = rootID[:8]
				}
				sr = &SessionRow{ConvID: rootID, Title: title}
				bySession[rootID] = sr
			}
			sr.Stats.Add(rec)
			if rec.Timestamp > sr.LastAt.Unix() {
				sr.LastAt = time.Unix(rec.Timestamp, 0)
			}
		}
	}

	rep.ScannedDBs = len(jobs)
	rep.ActiveConvs = len(bySession)

	rep.ByModel = make([]ModelRow, 0, len(byModel))
	for m, st := range byModel {
		rep.ByModel = append(rep.ByModel, ModelRow{Model: m, Stats: *st})
	}
	sort.Slice(rep.ByModel, func(i, j int) bool {
		ti, tj := rep.ByModel[i].Stats.TotalTokens(), rep.ByModel[j].Stats.TotalTokens()
		if ti != tj {
			return ti > tj
		}
		return rep.ByModel[i].Model < rep.ByModel[j].Model
	})

	rep.BySession = make([]SessionRow, 0, len(bySession))
	for _, sr := range bySession {
		rep.BySession = append(rep.BySession, *sr)
	}
	sort.Slice(rep.BySession, func(i, j int) bool {
		ti, tj := rep.BySession[i].Stats.TotalTokens(), rep.BySession[j].Stats.TotalTokens()
		if ti != tj {
			return ti > tj
		}
		return rep.BySession[i].LastAt.After(rep.BySession[j].LastAt)
	})

	rep.ElapsedMs = time.Since(t0).Milliseconds()
	return rep
}
