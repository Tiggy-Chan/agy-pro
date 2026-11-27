package i18n

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var verbRe = regexp.MustCompile(`%(\.[0-9]+)?[sdvfqwx%]`)

func formatVerbs(s string) []string {
	var out []string
	for _, m := range verbRe.FindAllString(s, -1) {
		if m == "%%" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func TestLocalePacksParityAndFormatVerbs(t *testing.T) {
	mu.RLock()
	en := catalogs["en"]
	zh := catalogs["zh"]
	mu.RUnlock()

	if len(en) == 0 || len(zh) == 0 {
		t.Fatalf("expected non-empty embedded en & zh packs, got en=%d zh=%d", len(en), len(zh))
	}
	for k, ev := range en {
		zv, ok := zh[k]
		if !ok || zv == "" {
			t.Fatalf("key %q present in en.json but missing/empty in zh.json", k)
		}
		evVerbs := formatVerbs(ev)
		zvVerbs := formatVerbs(zv)
		if len(evVerbs) != len(zvVerbs) {
			t.Fatalf("format verb count mismatch for key %q: en=%v zh=%v", k, evVerbs, zvVerbs)
		}
	}
	for k := range zh {
		if _, ok := en[k]; !ok {
			t.Fatalf("key %q present in zh.json but missing in en.json", k)
		}
	}
}

func TestAutoDetectionSwitchingAndExternalPack(t *testing.T) {
	origOSLocale := detectOSLocale
	detectOSLocale = func() string { return "" }
	t.Cleanup(func() {
		detectOSLocale = origOSLocale
		SetPref("auto")
	})
	t.Setenv("AGY_PRO_LANG", "")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANGUAGE", "")

	// Chinese locale -> zh
	t.Setenv("LANG", "zh_CN.UTF-8")
	SetPref("auto")
	if Lang() != "zh" || T("ui.tab.pool") != "号池" {
		t.Fatalf("expected zh for zh_CN.UTF-8, got lang=%q pool=%q", Lang(), T("ui.tab.pool"))
	}

	// Non-Chinese locales -> en
	for _, loc := range []string{"en_US.UTF-8", "fr_FR.UTF-8", "de_DE.UTF-8", "C", ""} {
		t.Setenv("LANG", loc)
		SetPref("auto")
		if Lang() != "en" || T("ui.tab.pool") != "Pool" {
			t.Fatalf("expected en for LANG=%q, got lang=%q pool=%q", loc, Lang(), T("ui.tab.pool"))
		}
	}

	// Windows OS locale fallback (when POSIX LANG/LC_* are unset in CMD/PowerShell)
	t.Setenv("LANG", "")
	detectOSLocale = func() string { return "zh-CN" }
	SetPref("auto")
	if Lang() != "zh" || T("ui.tab.pool") != "号池" {
		t.Fatalf("expected zh for Windows locale zh-CN, got lang=%q pool=%q", Lang(), T("ui.tab.pool"))
	}
	detectOSLocale = func() string { return "" }

	// Explicit preference overrides system locale ("cn" normalizes to "zh")
	t.Setenv("LANG", "en_US.UTF-8")
	if p, ok := NormalizePref("cn"); !ok || p != "zh" {
		t.Fatalf("expected cn -> zh, got p=%q ok=%v", p, ok)
	}
	SetPref("zh")
	if Lang() != "zh" || T("usage.range.today") != "今日" {
		t.Fatalf("expected explicit zh override, got lang=%q", Lang())
	}

	// External drop-in locale pack works with zero Go code changes
	tmp := t.TempDir()
	extDir := filepath.Join(tmp, "locales")
	_ = os.MkdirAll(extDir, 0o700)
	_ = os.WriteFile(filepath.Join(extDir, "ja.json"), []byte(`{"ui.tab.pool":"プール"}`), 0o600)
	Init("ja", tmp)
	if Lang() != "ja" || T("ui.tab.pool") != "プール" {
		t.Fatalf("expected external ja pack loaded, got lang=%q pool=%q", Lang(), T("ui.tab.pool"))
	}
	// Missing key in external pack gracefully falls back to en.json
	if got := T("ui.tab.usage"); got != "Usage" {
		t.Fatalf("expected missing key fallback to en 'Usage', got %q", got)
	}
}
