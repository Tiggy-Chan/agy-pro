package sessions

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

type Session struct {
	ID         string
	Title      string
	Preview    string
	Steps      int
	ModifiedAt time.Time
	Workspaces []string
	Running    bool
	Open       bool
}

func DefaultDBPath() string {
	return filepath.Join(vault.AgyDir(), "conversation_summaries.db")
}

// ParseWorkspaceURIs decodes agy's JSON array of URL-encoded file:// URIs into clean local paths.
func ParseWorkspaceURIs(raw string) []string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	var uris []string
	if err := json.Unmarshal([]byte(s), &uris); err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		p := strings.TrimSpace(u)
		if strings.HasPrefix(p, "file://") {
			if parsed, err := url.Parse(p); err == nil && parsed.Path != "" {
				p = parsed.Path
			} else {
				p = strings.TrimPrefix(p, "file://")
				if unesc, err := url.PathUnescape(p); err == nil {
					p = unesc
				}
			}
		}
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func parseDBTime(raw string) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// List opens agy's conversation_summaries.db strictly read-only and returns
// top-level conversations ordered by last_modified_time DESC.
func List(dbPath string, limit int) ([]Session, error) {
	if dbPath == "" {
		dbPath = DefaultDBPath()
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 300
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT conversation_id, title, preview, step_count, last_modified_time, workspace_uris, status
		FROM conversation_summaries
		WHERE nesting_depth = 0 AND (parent_conversation_id IS NULL OR parent_conversation_id = '')
		ORDER BY last_modified_time DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var id, title, preview, modRaw, wsRaw, status string
		var steps int
		if err := rows.Scan(&id, &title, &preview, &steps, &modRaw, &wsRaw, &status); err != nil {
			continue
		}
		tClean := singleLine(title)
		pClean := singleLine(preview)
		if tClean == "" {
			if pClean != "" {
				tClean = pClean
			} else {
				tClean = "(无标题会话)"
			}
		}
		out = append(out, Session{
			ID:         id,
			Title:      tClean,
			Preview:    pClean,
			Steps:      steps,
			ModifiedAt: parseDBTime(modRaw),
			Workspaces: ParseWorkspaceURIs(wsRaw),
			Running:    strings.EqualFold(status, "CASCADE_RUN_STATUS_RUNNING") || strings.EqualFold(status, "CASCADE_RUN_STATUS_BUSY"),
		})
	}
	return out, rows.Err()
}

// MatchesDir reports whether this session belongs to cwd.
func (s Session) MatchesDir(cwd string) bool {
	if cwd == "" {
		return false
	}
	cleanCwd := filepath.Clean(cwd)
	evalCwd, _ := filepath.EvalSymlinks(cleanCwd)
	for _, w := range s.Workspaces {
		cw := filepath.Clean(w)
		if cw == cleanCwd {
			return true
		}
		if evalCwd != "" {
			if ew, err := filepath.EvalSymlinks(cw); err == nil && ew == evalCwd {
				return true
			}
		}
	}
	return false
}

// PrimaryWorkspace returns a compact display path for the session's first workspace.
func (s Session) PrimaryWorkspace(homeDir string) string {
	if len(s.Workspaces) == 0 {
		return "-"
	}
	p := s.Workspaces[0]
	if homeDir != "" {
		cleanHome := filepath.Clean(homeDir)
		if p == cleanHome {
			return "~"
		}
		if strings.HasPrefix(p, cleanHome+string(os.PathSeparator)) {
			return "~" + p[len(cleanHome):]
		}
	}
	return p
}

// MatchesQuery performs case-insensitive multi-term substring matching across title, preview, workspace, and ID.
func (s Session) MatchesQuery(query string) bool {
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" {
		return true
	}
	haystack := strings.ToLower(s.Title + " " + s.Preview + " " + strings.Join(s.Workspaces, " ") + " " + s.ID)
	for _, term := range strings.Fields(q) {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func FmtRelativeTime(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%dm前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh前", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd前", int(d.Hours()/24))
	default:
		return t.Local().Format("01-02")
	}
}

// ResolveRootID climbs parent_conversation_id links in conversation_summaries.db (strictly read-only)
// so a subagent conversation ID is always normalized back to its top-level user conversation ID.
// If the database or row does not exist, convID is returned unchanged.
func ResolveRootID(dbPath, convID string) string {
	id := strings.TrimSpace(convID)
	if id == "" {
		return ""
	}
	if dbPath == "" {
		dbPath = DefaultDBPath()
	}
	if _, err := os.Stat(dbPath); err != nil {
		return id
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return id
	}
	defer db.Close()

	seen := map[string]bool{id: true}
	cur := id
	for step := 0; step < 8; step++ {
		var parent string
		var depth int
		err := db.QueryRow(
			`SELECT COALESCE(parent_conversation_id, ''), COALESCE(nesting_depth, 0) FROM conversation_summaries WHERE conversation_id = ?`,
			cur,
		).Scan(&parent, &depth)
		if err != nil || depth <= 0 || strings.TrimSpace(parent) == "" {
			break
		}
		parent = strings.TrimSpace(parent)
		if seen[parent] {
			break
		}
		seen[parent] = true
		cur = parent
	}
	return cur
}

// LiveMeta holds lightweight real-time metadata for an active conversation.
type LiveMeta struct {
	Title   string
	Preview string
	Running bool
}

// MetaReader keeps a single read-only SQLite handle to conversation_summaries.db
// for low-overhead (<0.1ms) live polling of conversation title (/rename) and run status.
type MetaReader struct {
	dbPath string
	db     *sql.DB
}

func OpenMetaReader(dbPath string) *MetaReader {
	if dbPath == "" {
		dbPath = DefaultDBPath()
	}
	return &MetaReader{dbPath: dbPath}
}

func (r *MetaReader) ensureOpen() bool {
	if r == nil {
		return false
	}
	if r.db != nil {
		return true
	}
	if _, err := os.Stat(r.dbPath); err != nil {
		return false
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(500)&_pragma=query_only(1)", r.dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false
	}
	db.SetMaxOpenConns(1)
	r.db = db
	return true
}

func (r *MetaReader) Lookup(convID string) (LiveMeta, bool) {
	id := strings.TrimSpace(convID)
	if id == "" || !r.ensureOpen() {
		return LiveMeta{}, false
	}
	var title, preview, status string
	var notFullyIdle int
	err := r.db.QueryRow(`
		SELECT COALESCE(title, ''), COALESCE(preview, ''), COALESCE(status, ''), COALESCE(not_fully_idle, 0)
		FROM conversation_summaries
		WHERE conversation_id = ?
	`, id).Scan(&title, &preview, &status, &notFullyIdle)
	if err != nil {
		// Backward-compatible fallback if an older agy schema lacks not_fully_idle
		if err2 := r.db.QueryRow(`
			SELECT COALESCE(title, ''), COALESCE(preview, ''), COALESCE(status, '')
			FROM conversation_summaries
			WHERE conversation_id = ?
		`, id).Scan(&title, &preview, &status); err2 != nil {
			return LiveMeta{}, false
		}
	}
	up := strings.ToUpper(strings.TrimSpace(status))
	running := up == "CASCADE_RUN_STATUS_RUNNING" || up == "CASCADE_RUN_STATUS_BUSY" || notFullyIdle != 0
	return LiveMeta{
		Title:   singleLine(title),
		Preview: singleLine(preview),
		Running: running,
	}, true
}

func (r *MetaReader) Close() {
	if r != nil && r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

// SchemaStatus summarizes read-only upstream schema compatibility for conversation_summaries.db.
type SchemaStatus struct {
	Exists        bool
	OK            bool
	Missing       []string
	TotalTopLevel int
	RunningCount  int
	Err           string
}

var requiredSummaryColumns = []string{
	"conversation_id",
	"title",
	"preview",
	"step_count",
	"last_modified_time",
	"workspace_uris",
	"status",
	"nesting_depth",
	"parent_conversation_id",
}

// CheckSchema inspects conversation_summaries.db read-only and verifies expected upstream columns.
func CheckSchema(dbPath string) SchemaStatus {
	if dbPath == "" {
		dbPath = DefaultDBPath()
	}
	if _, err := os.Stat(dbPath); err != nil {
		return SchemaStatus{Exists: false, Err: "未找到会话库（首次启动 agy 后自动生成）"}
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return SchemaStatus{Exists: true, Err: err.Error()}
	}
	defer db.Close()

	rows, err := db.Query(`PRAGMA table_info(conversation_summaries)`)
	if err != nil {
		return SchemaStatus{Exists: true, Err: err.Error()}
	}
	defer rows.Close()

	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
			have[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	if len(have) == 0 {
		return SchemaStatus{Exists: true, Err: "缺少 conversation_summaries 表"}
	}
	var missing []string
	for _, col := range requiredSummaryColumns {
		if !have[col] {
			missing = append(missing, col)
		}
	}
	st := SchemaStatus{Exists: true, OK: len(missing) == 0, Missing: missing}
	if st.OK {
		_ = db.QueryRow(`SELECT COUNT(*) FROM conversation_summaries WHERE nesting_depth = 0 AND (parent_conversation_id IS NULL OR parent_conversation_id = '')`).Scan(&st.TotalTopLevel)
		if have["not_fully_idle"] {
			_ = db.QueryRow(`SELECT COUNT(*) FROM conversation_summaries WHERE nesting_depth = 0 AND (parent_conversation_id IS NULL OR parent_conversation_id = '') AND (UPPER(status) IN ('CASCADE_RUN_STATUS_RUNNING','CASCADE_RUN_STATUS_BUSY') OR COALESCE(not_fully_idle,0) != 0)`).Scan(&st.RunningCount)
		} else {
			_ = db.QueryRow(`SELECT COUNT(*) FROM conversation_summaries WHERE nesting_depth = 0 AND (parent_conversation_id IS NULL OR parent_conversation_id = '') AND UPPER(status) IN ('CASCADE_RUN_STATUS_RUNNING','CASCADE_RUN_STATUS_BUSY')`).Scan(&st.RunningCount)
		}
	}
	return st
}

// ListActiveFirst returns top-level sessions with currently running sessions pinned to the top.
func ListActiveFirst(dbPath string, limit int) ([]Session, error) {
	list, err := List(dbPath, limit)
	if err != nil || len(list) == 0 {
		return list, err
	}
	running := make([]Session, 0, len(list))
	idle := make([]Session, 0, len(list))
	for _, s := range list {
		if s.Running {
			running = append(running, s)
		} else {
			idle = append(idle, s)
		}
	}
	return append(running, idle...), nil
}
