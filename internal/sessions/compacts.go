package sessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

// stepTypeCheckpoint is official agy's SQLite step_type for CHECKPOINT steps.
// Both auto-compact and manual `/compact` emit a CHECKPOINT step in conversations/<id>.db.
const stepTypeCheckpoint = 23

type compactCacheEntry struct {
	modTimeNano int64
	size        int64
	count       int
}

var (
	compactCacheMu sync.RWMutex
	compactCache   = map[string]compactCacheEntry{}
)

func convsDirFor(summaryDBPath string) string {
	if strings.TrimSpace(summaryDBPath) == "" {
		return vault.ConversationsDir()
	}
	return filepath.Join(filepath.Dir(summaryDBPath), "conversations")
}

// CountCompacts reads conversations/<id>.db strictly read-only and counts completed
// compaction checkpoints (step_type = 23), caching by (mtime, size) so repeated UI
// polls incur zero SQLite opens for unchanged conversations.
func CountCompacts(convDBPath string) int {
	if strings.TrimSpace(convDBPath) == "" {
		return 0
	}
	st, err := os.Stat(convDBPath)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return 0
	}
	modNano := st.ModTime().UnixNano()
	size := st.Size()

	compactCacheMu.RLock()
	if ent, ok := compactCache[convDBPath]; ok && ent.modTimeNano == modNano && ent.size == size {
		compactCacheMu.RUnlock()
		return ent.count
	}
	compactCacheMu.RUnlock()

	dsn := vault.SQLiteReadOnlyDSN(convDBPath, 500)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0
	}
	defer db.Close()

	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steps WHERE step_type = ?`, stepTypeCheckpoint).Scan(&cnt); err != nil {
		return 0
	}
	if cnt < 0 {
		cnt = 0
	}

	compactCacheMu.Lock()
	compactCache[convDBPath] = compactCacheEntry{
		modTimeNano: modNano,
		size:        size,
		count:       cnt,
	}
	compactCacheMu.Unlock()
	return cnt
}

// PopulateCompacts fills Session.Compacts in-place from convsDir/<id>.db.
// Runs bounded concurrency when scanning larger history lists.
func PopulateCompacts(convsDir string, list []Session) {
	if len(list) == 0 || strings.TrimSpace(convsDir) == "" {
		return
	}
	if len(list) <= 6 {
		for i := range list {
			if list[i].ID != "" && list[i].Steps > 0 {
				list[i].Compacts = CountCompacts(filepath.Join(convsDir, list[i].ID+".db"))
			}
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
				list[idx].Compacts = CountCompacts(filepath.Join(convsDir, list[idx].ID+".db"))
			}
		}()
	}
	wg.Wait()
}
