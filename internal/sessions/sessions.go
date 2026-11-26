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
			Running:    strings.EqualFold(status, "CASCADE_RUN_STATUS_RUNNING"),
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
