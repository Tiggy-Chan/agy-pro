package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed locales/*.json
var embeddedLocales embed.FS

const (
	DefaultLang = "en"
	PrefAuto    = "auto"
)

var (
	mu       sync.RWMutex
	catalogs map[string]map[string]string
	active   = DefaultLang
	pref     = PrefAuto
	loaded   bool
)

func init() {
	ensureLoadedLocked()
	active = DetectSystemLang()
}

func ensureLoadedLocked() {
	if loaded {
		return
	}
	catalogs = map[string]map[string]string{}
	entries, err := embeddedLocales.ReadDir("locales")
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			code := strings.ToLower(strings.TrimSuffix(e.Name(), ".json"))
			raw, readErr := embeddedLocales.ReadFile("locales/" + e.Name())
			if readErr != nil {
				continue
			}
			var m map[string]string
			if json.Unmarshal(raw, &m) == nil && len(m) > 0 {
				catalogs[code] = m
			}
		}
	}
	loaded = true
}

// LoadExternalDir merges optional user-provided locale packs from `<dir>/*.json`.
// Adding a new language (e.g. `ja.json`) requires zero code changes.
func LoadExternalDir(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ensureLoadedLocked()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.ToLower(strings.TrimSuffix(e.Name(), ".json"))
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m map[string]string
		if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
			continue
		}
		if existing, ok := catalogs[code]; ok {
			for k, v := range m {
				existing[k] = v
			}
		} else {
			catalogs[code] = m
		}
	}
}

// NormalizeLangCode maps locale identifiers (e.g. "zh_CN.UTF-8", "cn", "en_US.UTF-8") to a pack code.
func NormalizeLangCode(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if idx := strings.IndexAny(s, ".:@"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.ReplaceAll(s, "-", "_")
	switch {
	case s == "cn" || s == "zh" || strings.HasPrefix(s, "zh_"):
		return "zh"
	case s == "en" || strings.HasPrefix(s, "en_"):
		return "en"
	default:
		// Support future external locale packs like "ja" from "ja_JP"
		if base, _, ok := strings.Cut(s, "_"); ok && base != "" {
			return base
		}
		return s
	}
}

// DetectSystemLang inspects AGY_PRO_LANG and standard POSIX locale env vars.
// Chinese locales resolve to "zh"; all other locales default to "en" (unless an exact pack exists).
func DetectSystemLang() string {
	mu.RLock()
	hasCode := func(code string) bool {
		_, ok := catalogs[code]
		return ok
	}
	mu.RUnlock()

	candidates := []string{
		os.Getenv("AGY_PRO_LANG"),
		os.Getenv("LC_ALL"),
		os.Getenv("LC_MESSAGES"),
		os.Getenv("LANG"),
		os.Getenv("LANGUAGE"),
	}
	for _, raw := range candidates {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		// LANGUAGE can be colon-separated (e.g. "zh_CN:zh:en")
		for _, part := range strings.Split(raw, ":") {
			part = strings.TrimSpace(part)
			if part == "" || strings.EqualFold(part, "c") || strings.EqualFold(part, "posix") {
				continue
			}
			code := NormalizeLangCode(part)
			if code == "zh" {
				return "zh"
			}
			if hasCode(code) {
				return code
			}
			return DefaultLang
		}
	}
	return DefaultLang
}

// NormalizePref validates a user language preference ("auto", "en", "zh"/"cn", or any loaded pack).
func NormalizePref(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == PrefAuto {
		return PrefAuto, true
	}
	code := NormalizeLangCode(s)
	mu.RLock()
	_, ok := catalogs[code]
	mu.RUnlock()
	if !ok {
		return "", false
	}
	return code, true
}

// Init loads optional external locale overrides from `<stateDir>/locales` and activates preference.
func Init(userPref, stateDir string) {
	if stateDir != "" {
		LoadExternalDir(filepath.Join(stateDir, "locales"))
	}
	SetPref(userPref)
}

// SetPref updates the active language from a saved preference ("auto" | "en" | "zh" | ...).
// Explicit AGY_PRO_LANG env var always takes precedence when set.
func SetPref(userPref string) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoadedLocked()

	p := strings.ToLower(strings.TrimSpace(userPref))
	if p == "" || p == PrefAuto {
		pref = PrefAuto
	} else {
		code := NormalizeLangCode(p)
		if _, ok := catalogs[code]; ok {
			pref = code
		} else {
			pref = PrefAuto
		}
	}

	if envOverride := strings.TrimSpace(os.Getenv("AGY_PRO_LANG")); envOverride != "" {
		code := NormalizeLangCode(envOverride)
		if _, ok := catalogs[code]; ok {
			active = code
			return
		}
	}

	if pref == PrefAuto {
		mu.Unlock()
		det := DetectSystemLang()
		mu.Lock()
		active = det
		return
	}
	active = pref
}

func Lang() string {
	mu.RLock()
	defer mu.RUnlock()
	return active
}

func Pref() string {
	mu.RLock()
	defer mu.RUnlock()
	return pref
}

func Available() []string {
	mu.RLock()
	defer mu.RUnlock()
	ensureLoadedLocked()
	out := make([]string, 0, len(catalogs))
	for k := range catalogs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NextPref cycles through auto -> available languages (e.g. auto -> en -> zh -> auto).
func NextPref(cur string) string {
	norm, ok := NormalizePref(cur)
	if !ok {
		norm = PrefAuto
	}
	cycle := append([]string{PrefAuto}, Available()...)
	for i, c := range cycle {
		if c == norm {
			return cycle[(i+1)%len(cycle)]
		}
	}
	return PrefAuto
}

// DisplayPref formats the saved preference cleanly for UI/CLI display.
func DisplayPref(savedPref string) string {
	norm, ok := NormalizePref(savedPref)
	if !ok || norm == PrefAuto {
		return fmt.Sprintf("auto (%s)", Lang())
	}
	return norm
}

// T resolves a message key from the active locale pack, falling back to English then key itself.
func T(key string, args ...any) string {
	mu.RLock()
	ensureLoadedLocked()
	tmpl := ""
	if cat, ok := catalogs[active]; ok {
		tmpl = cat[key]
	}
	if tmpl == "" && active != DefaultLang {
		if defCat, ok := catalogs[DefaultLang]; ok {
			tmpl = defCat[key]
		}
	}
	mu.RUnlock()

	if tmpl == "" {
		tmpl = key
	}
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}
