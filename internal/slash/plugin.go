package slash

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

const (
	pluginDirName      = "agy-pro"
	managedSkillMarker = "<!-- managed-by: agy-pro -->"
)

// DefaultPluginDir returns ~/.gemini/config/plugins/agy-pro.
func DefaultPluginDir() string {
	return filepath.Join(vault.HomeDir(), ".gemini", "config", "plugins", pluginDirName)
}

// DefaultSkillsDir returns ~/.gemini/config/skills (bare /dash and /next autocomplete).
func DefaultSkillsDir() string {
	return filepath.Join(vault.HomeDir(), ".gemini", "config", "skills")
}

func quoteHookCommand(binPath string) string {
	if runtime.GOOS == "windows" {
		return `"` + binPath + `" _hook`
	}
	return "'" + strings.ReplaceAll(binPath, "'", `'\''`) + "' _hook"
}

// EnsurePlugin installs or refreshes the isolated agy-pro PreInvocation hook plugin
// (~/.gemini/config/plugins/agy-pro/) and bare slash skills (~/.gemini/config/skills/{dash,next}/)
// without ever touching user hooks.json, config.json, or user-authored skills.
func EnsurePlugin(agyProBin string) error {
	return EnsurePluginInDirs(DefaultPluginDir(), DefaultSkillsDir(), agyProBin)
}

// EnsurePluginInDirs writes the plugin bundle (plugin.json + hooks.json) into pluginDir
// and bare slash command SKILL.md descriptors into skillsDir idempotently.
// Existing skills without agy-pro's managed marker are never overwritten.
func EnsurePluginInDirs(pluginDir, skillsDir, agyProBin string) error {
	bin := strings.TrimSpace(agyProBin)
	if bin == "" {
		bin = "agy-pro"
	}

	pluginManifest := map[string]any{
		"name":        "agy-pro",
		"version":     vault.Version,
		"description": "In-session /dash dashboard and /next live account hot-swap for agy-pro",
		"repository":  "https://github.com/Tiggy-Chan/agy-pro",
	}
	manifestBytes, err := json.MarshalIndent(pluginManifest, "", "  ")
	if err != nil {
		return err
	}
	manifestBytes = append(manifestBytes, '\n')

	hooksConfig := map[string]any{
		"agy-pro-slash": map[string]any{
			"PreInvocation": []map[string]any{
				{
					"type":    "command",
					"command": quoteHookCommand(bin),
					"timeout": 35,
				},
			},
		},
	}
	hooksBytes, err := json.MarshalIndent(hooksConfig, "", "  ")
	if err != nil {
		return err
	}
	hooksBytes = append(hooksBytes, '\n')

	pluginFiles := map[string][]byte{
		filepath.Join(pluginDir, "plugin.json"): manifestBytes,
		filepath.Join(pluginDir, "hooks.json"):  hooksBytes,
	}
	for path, content := range pluginFiles {
		if err := writeIfChanged(path, content); err != nil {
			return err
		}
	}

	// Clean up any legacy plugin-scoped skills so the / menu never shows duplicate
	// /dash + /agy-pro:dash entries when bare skills are installed in skillsDir.
	_ = os.RemoveAll(filepath.Join(pluginDir, "skills"))

	if strings.TrimSpace(skillsDir) != "" {
		dashSkill := `---
name: dash
description: Show agy-pro live account pool quotas, open sessions, and token usage inline (supports -r, usage, open, --mask).
---
` + managedSkillMarker + `

# /dash

Displays the agy-pro inline status dashboard directly inside the conversation with zero token usage via agy-pro's PreInvocation hook.
If hooks are disabled, run ` + "`agy-pro status`" + ` or ` + "`agy-pro usage`" + ` via run_command.
`
		nextSkill := `---
name: next
description: Hot-swap to the next ready Google account (or /next <keyword>) live inside the current agy session without restarting.
---
` + managedSkillMarker + `

# /next

Switches the active Google account live inside the current session without interrupting ongoing work via agy-pro's PreInvocation hook.
If hooks are disabled, run ` + "`agy-pro next`" + ` via run_command.
`
		skillFiles := map[string][]byte{
			filepath.Join(skillsDir, "dash", "SKILL.md"): []byte(dashSkill),
			filepath.Join(skillsDir, "next", "SKILL.md"): []byte(nextSkill),
		}
		for path, content := range skillFiles {
			if existing, err := os.ReadFile(path); err == nil {
				// Never overwrite a user-authored custom skill that lacks our managed marker.
				if !bytes.Contains(existing, []byte(managedSkillMarker)) {
					continue
				}
				if bytes.Equal(existing, content) {
					continue
				}
			}
			if err := writeIfChanged(path, content); err != nil {
				return err
			}
		}
	}
	return nil
}

// Status reports whether the agy-pro slash plugin and bare skills are currently installed.
type Status struct {
	HookInstalled   bool
	HookPath        string
	SkillsInstalled int
}

// CheckStatus inspects default plugin and bare skill locations without modifying files.
func CheckStatus() Status {
	return CheckStatusInDirs(DefaultPluginDir(), DefaultSkillsDir())
}

// CheckStatusInDirs inspects pluginDir and skillsDir without modifying files.
func CheckStatusInDirs(pluginDir, skillsDir string) Status {
	hookPath := filepath.Join(pluginDir, "hooks.json")
	st := Status{HookPath: hookPath}
	if _, err := os.Stat(hookPath); err == nil {
		st.HookInstalled = true
	}
	for _, sk := range []string{"dash", "next"} {
		if _, err := os.Stat(filepath.Join(skillsDir, sk, "SKILL.md")); err == nil {
			st.SkillsInstalled++
		}
	}
	return st
}

func writeIfChanged(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return vault.AtomicWriteFile(path, content, 0o600)
}
