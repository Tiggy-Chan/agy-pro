package sessions

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func DefaultPresenceDir() string {
	return filepath.Join(vault.AgyDir(), "presence")
}

// FindOpenConversationIDs detects currently open `agy` conversations on this machine by
// probing non-blocking shared flocks on ~/.gemini/antigravity-cli/presence/<uuid>.lock.
// Official `agy` holds an exclusive kernel flock on each open conversation and releases it
// immediately on exit or crash, giving 100% live accuracy for both `agy-pro` and bare `agy`.
func FindOpenConversationIDs(presenceDir string) ([]string, error) {
	if presenceDir == "" {
		presenceDir = DefaultPresenceDir()
	}
	entries, err := os.ReadDir(presenceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var openIDs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".lock") {
			continue
		}
		id := strings.TrimSuffix(name, ".lock")
		if len(id) != 36 {
			continue
		}
		full := filepath.Join(presenceDir, name)
		if isPresenceLocked(full) {
			openIDs = append(openIDs, id)
		}
	}
	sort.Strings(openIDs)
	return openIDs, nil
}

func isPresenceLocked(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if err != nil {
		return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
}

// ListOpen returns ONLY genuinely open top-level `agy` conversations on this machine,
// enriched with live title/preview/status/workspace from conversation_summaries.db.
// Subagents (nesting_depth > 0) are excluded so rows map 1:1 to open user terminals.
func ListOpen(dbPath, presenceDir string) ([]Session, error) {
	openIDs, err := FindOpenConversationIDs(presenceDir)
	if err != nil || len(openIDs) == 0 {
		return nil, err
	}
	if dbPath == "" {
		dbPath = DefaultDBPath()
	}

	byID := map[string]Session{}
	subagentIDs := map[string]bool{}

	if _, statErr := os.Stat(dbPath); statErr == nil {
		dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)", dbPath)
		if db, openErr := sql.Open("sqlite", dsn); openErr == nil {
			defer db.Close()
			placeholders := strings.Repeat("?,", len(openIDs))
			placeholders = strings.TrimSuffix(placeholders, ",")
			args := make([]any, len(openIDs))
			for i, id := range openIDs {
				args[i] = id
			}
			query := fmt.Sprintf(`
				SELECT conversation_id, COALESCE(title, ''), COALESCE(preview, ''), COALESCE(step_count, 0),
				       COALESCE(last_modified_time, ''), COALESCE(workspace_uris, ''), COALESCE(status, ''),
				       COALESCE(nesting_depth, 0), COALESCE(parent_conversation_id, '')
				FROM conversation_summaries
				WHERE conversation_id IN (%s)
			`, placeholders)
			if rows, qErr := db.Query(query, args...); qErr == nil {
				defer rows.Close()
				for rows.Next() {
					var id, title, preview, modRaw, wsRaw, status, parent string
					var steps, depth int
					if scanErr := rows.Scan(&id, &title, &preview, &steps, &modRaw, &wsRaw, &status, &depth, &parent); scanErr != nil {
						continue
					}
					if depth > 0 || strings.TrimSpace(parent) != "" {
						subagentIDs[id] = true
						continue
					}
					tClean := singleLine(title)
					pClean := singleLine(preview)
					if tClean == "" {
						if pClean != "" {
							tClean = pClean
						} else {
							tClean = i18n.T("sessions.new_session")
						}
					}
					up := strings.ToUpper(strings.TrimSpace(status))
					byID[id] = Session{
						ID:         id,
						Title:      tClean,
						Preview:    pClean,
						Steps:      steps,
						ModifiedAt: parseDBTime(modRaw),
						Workspaces: ParseWorkspaceURIs(wsRaw),
						Running:    up == "CASCADE_RUN_STATUS_RUNNING" || up == "CASCADE_RUN_STATUS_BUSY",
						Open:       true,
					}
				}
			}
		}
	}

	out := make([]Session, 0, len(openIDs))
	for _, id := range openIDs {
		if subagentIDs[id] {
			continue
		}
		if s, ok := byID[id]; ok {
			out = append(out, s)
		} else {
			out = append(out, Session{
				ID:    id,
				Title: i18n.T("sessions.new_session"),
				Open:  true,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		if !out[i].ModifiedAt.Equal(out[j].ModifiedAt) {
			return out[i].ModifiedAt.After(out[j].ModifiedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
