package slash

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
	"github.com/Tiggy-Chan/agy-pro/internal/seamless"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func fptr(v float64) *float64 { return &v }

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		kind CmdKind
		args string
	}{
		{"/dash", CmdDash, ""},
		{"  /dash -r --mask  ", CmdDash, "-r --mask"},
		{"/agy-pro:dash usage 7d", CmdDash, "usage 7d"},
		{"/next", CmdNext, ""},
		{"/next bob", CmdNext, "bob"},
		{"/agy-pro:next alice --mask", CmdNext, "alice --mask"},
		{"please fix /dash command", CmdNone, ""},
		{"/dash\nsecond line", CmdNone, ""},
		{"/usage", CmdNone, ""},
		{"hello", CmdNone, ""},
	}
	for _, tc := range cases {
		got := ParseCommand(tc.in)
		if got.Kind != tc.kind || strings.Join(got.Args, " ") != tc.args {
			t.Fatalf("ParseCommand(%q) = {kind=%v args=%v}, want {kind=%v args=%q}", tc.in, got.Kind, got.Args, tc.kind, tc.args)
		}
	}
}

func TestEnsurePluginInDirsIdempotentAndRespectsUserSkills(t *testing.T) {
	tmp := t.TempDir()
	pluginDir := filepath.Join(tmp, "plugins", "agy-pro")
	skillsDir := filepath.Join(tmp, "skills")

	// Seed a legacy plugin-scoped skill to ensure EnsurePluginInDirs cleans it up.
	legacySkill := filepath.Join(pluginDir, "skills", "dash", "SKILL.md")
	_ = os.MkdirAll(filepath.Dir(legacySkill), 0o700)
	_ = os.WriteFile(legacySkill, []byte("legacy"), 0o600)

	if err := EnsurePluginInDirs(pluginDir, skillsDir, "/usr/local/bin/agy-pro"); err != nil {
		t.Fatalf("EnsurePluginInDirs failed: %v", err)
	}
	// Second call must be an idempotent no-op
	if err := EnsurePluginInDirs(pluginDir, skillsDir, "/usr/local/bin/agy-pro"); err != nil {
		t.Fatalf("second EnsurePluginInDirs failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(pluginDir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("expected legacy pluginDir/skills to be cleaned up, got stat err=%v", err)
	}

	var manifest map[string]any
	rawM, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
	if err != nil || json.Unmarshal(rawM, &manifest) != nil || manifest["name"] != "agy-pro" || manifest["version"] != vault.Version {
		t.Fatalf("invalid plugin.json: err=%v raw=%q", err, string(rawM))
	}

	var hooks map[string]any
	rawH, err := os.ReadFile(filepath.Join(pluginDir, "hooks.json"))
	if err != nil || json.Unmarshal(rawH, &hooks) != nil || !strings.Contains(string(rawH), "_hook") {
		t.Fatalf("invalid hooks.json: err=%v raw=%q", err, string(rawH))
	}

	for _, sk := range []string{"dash", "next"} {
		rawS, err := os.ReadFile(filepath.Join(skillsDir, sk, "SKILL.md"))
		if err != nil || !strings.Contains(string(rawS), "name: "+sk) || !strings.Contains(string(rawS), managedSkillMarker) {
			t.Fatalf("missing or invalid bare SKILL.md for %s: err=%v", sk, err)
		}
	}

	st := CheckStatusInDirs(pluginDir, skillsDir)
	if !st.HookInstalled || st.SkillsInstalled != 2 {
		t.Fatalf("unexpected status: %+v", st)
	}

	// If a user replaces skills/dash/SKILL.md with their own custom skill (no managed marker),
	// EnsurePluginInDirs must NEVER clobber it.
	customContent := []byte("---\nname: dash\ndescription: user custom dash\n---\n# custom\n")
	dashPath := filepath.Join(skillsDir, "dash", "SKILL.md")
	_ = os.WriteFile(dashPath, customContent, 0o600)
	if err := EnsurePluginInDirs(pluginDir, skillsDir, "/usr/local/bin/agy-pro"); err != nil {
		t.Fatalf("EnsurePluginInDirs after user customization failed: %v", err)
	}
	gotDash, _ := os.ReadFile(dashPath)
	if !bytes.Equal(gotDash, customContent) {
		t.Fatalf("EnsurePluginInDirs clobbered user-authored skill! got:\n%s", string(gotDash))
	}
}

func TestPreInvocationPassthroughForNormalPromptAndSubsequentInvocations(t *testing.T) {
	v := vault.New(t.TempDir())
	for _, payload := range []string{
		`{"invocationNum":0,"lastUserInput":"fix the login bug"}`,
		`{"invocationNum":1,"lastUserInput":"/dash"}`,
		`{"invocationNum":2,"lastUserInput":"/next"}`,
		`{"invocationNum":0,"lastUserInput":"/help"}`,
		`not valid json`,
	} {
		var out bytes.Buffer
		rc := HandlePreInvocation(v, "", strings.NewReader(payload), &out)
		if rc != 0 || strings.TrimSpace(out.String()) != "{}" {
			t.Fatalf("expected empty {} passthrough for %s, got rc=%d out=%q", payload, rc, out.String())
		}
	}
}

func TestLiveSlashNextAndDashEndToEndWithSeamlessProxy(t *testing.T) {
	i18n.SetPref("en")
	t.Cleanup(func() { i18n.SetPref("auto") })

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AGY_PRO_HOOK_RENDER", "")
	v := vault.New(filepath.Join(tmp, "pool"))

	for _, item := range []struct{ email, tok string }{
		{"alice@gmail.com", "tok-alice"},
		{"bob@gmail.com", "tok-bob"},
	} {
		tf := filepath.Join(tmp, item.email+".tok")
		raw, _ := json.Marshal(map[string]any{
			"id_token_email": item.email,
			"token": map[string]any{
				"access_token":  item.tok,
				"refresh_token": "ref",
				"expiry":        time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			},
		})
		_ = os.WriteFile(tf, raw, 0o600)
		if _, err := v.ImportTokenFile(tf); err != nil {
			t.Fatalf("import %s failed: %v", item.email, err)
		}
	}
	now := float64(time.Now().Unix())
	_ = v.UpdateState(func(s *vault.State) {
		s.Active = "alice@gmail.com"
		s.Quotas["alice@gmail.com"] = &vault.Quota{H5Used: fptr(40), H5Rem: fptr(60), D7Used: fptr(80), D7Rem: fptr(20), At: now}
		s.Quotas["bob@gmail.com"] = &vault.Quota{H5Used: fptr(0), H5Rem: fptr(100), D7Used: fptr(10), D7Rem: fptr(90), At: now}
	})

	// Mock upstream HTTP/2 server that echoes the Bearer token it receives
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.Header.Get("Authorization")))
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	prov := seamless.NewVaultTokenProvider(v, "", "alice@gmail.com", 0, true, nil)
	proxy, err := seamless.Start(seamless.Options{
		Provider:       prov,
		InterceptHosts: []string{upURL.Hostname()},
		UpstreamTLS:    &tls.Config{InsecureSkipVerify: true},
		TempDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("seamless.Start failed: %v", err)
	}
	defer proxy.Close()

	t.Setenv(vault.CtrlURLEnvKey, proxy.CtrlURL())
	t.Setenv(vault.CtrlTokenEnvKey, proxy.CtrlToken())
	t.Setenv("AGY_PRO_MASK", "")

	// 1. Default plain mode: `/dash` at invocationNum=0 reports alice@gmail.com via errorMessage + terminate
	var dashOut bytes.Buffer
	HandlePreInvocation(v, "", strings.NewReader(`{"invocationNum":0,"lastUserInput":"/dash"}`), &dashOut)
	var dashResp hookResponse
	if err := json.Unmarshal(dashOut.Bytes(), &dashResp); err != nil {
		t.Fatalf("decode /dash response failed: %v (raw=%q)", err, dashOut.String())
	}
	if dashResp.TerminationBehavior != "terminate" || len(dashResp.InjectSteps) != 1 || dashResp.InjectSteps[0].ErrorMessage == nil {
		t.Fatalf("expected terminate + 1 errorMessage step, got %+v", dashResp)
	}
	txt1 := dashResp.InjectSteps[0].ErrorMessage.UserMessage
	if !strings.Contains(txt1, "●  alice@gmail.com") || !strings.Contains(txt1, "seamless") || strings.Contains(txt1, "\x1b[") {
		t.Fatalf("expected crisp plain /dash card with active alice@gmail.com and zero ANSI, got:\n%s", txt1)
	}

	// 2. `/next` hot-swaps live seamless proxy from alice -> bob in-memory!
	var nextOut bytes.Buffer
	HandlePreInvocation(v, "", strings.NewReader(`{"invocationNum":0,"lastUserInput":"/next"}`), &nextOut)
	var nextResp hookResponse
	if err := json.Unmarshal(nextOut.Bytes(), &nextResp); err != nil {
		t.Fatalf("decode /next response failed: %v", err)
	}
	if nextResp.TerminationBehavior != "terminate" || len(nextResp.InjectSteps) != 1 || nextResp.InjectSteps[0].ErrorMessage == nil {
		t.Fatalf("expected terminate + 1 errorMessage step for /next, got %+v", nextResp)
	}
	txtNext := nextResp.InjectSteps[0].ErrorMessage.UserMessage
	if !strings.Contains(txtNext, "alice@gmail.com → bob@gmail.com") {
		t.Fatalf("expected /next confirmation alice -> bob, got:\n%s", txtNext)
	}

	// 3. Subsequent HTTP/2 request through proxy (still carrying stale alice header from agy) now sends bob's token!
	pURL, _ := url.Parse(proxy.ProxyURL())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(pURL),
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: proxy.CA().CertPool()},
		},
	}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1internal:streamGenerateContent", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok-alice")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through proxy after /next failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "Bearer tok-bob" {
		t.Fatalf("expected live proxy to inject Bearer tok-bob immediately after /next, got %q", string(body))
	}

	// 4. `/dash --mask` highlights masked bob (`bo***@gmail.com`) as active without leaking real email!
	var dashOut2 bytes.Buffer
	HandlePreInvocation(v, "", strings.NewReader(`{"invocationNum":0,"lastUserInput":"/dash --mask"}`), &dashOut2)
	var dashResp2 hookResponse
	_ = json.Unmarshal(dashOut2.Bytes(), &dashResp2)
	txt2 := dashResp2.InjectSteps[0].ErrorMessage.UserMessage
	if !strings.Contains(txt2, "●  bo***@gmail.com") || strings.Contains(txt2, "bob@gmail.com") {
		t.Fatalf("expected /dash --mask to show masked bob as active, got:\n%s", txt2)
	}

	// 5. `/dash help`, `/dash usage`, `/dash open` all return clean non-empty plain cards
	for _, cmd := range []string{"/dash help", "/dash usage", "/dash open"} {
		var buf bytes.Buffer
		HandlePreInvocation(v, "", strings.NewReader(`{"invocationNum":0,"lastUserInput":"`+cmd+`"}`), &buf)
		var r hookResponse
		if err := json.Unmarshal(buf.Bytes(), &r); err != nil || len(r.InjectSteps) != 1 || r.InjectSteps[0].ErrorMessage == nil || r.InjectSteps[0].ErrorMessage.UserMessage == "" {
			t.Fatalf("unexpected response for %s: raw=%q", cmd, buf.String())
		}
	}

	// 6. Opt-in AGY_PRO_HOOK_RENDER=markdown emits systemMessage.metadata.user_markdown
	t.Setenv("AGY_PRO_HOOK_RENDER", "markdown")
	var mdOut bytes.Buffer
	HandlePreInvocation(v, "", strings.NewReader(`{"invocationNum":0,"lastUserInput":"/dash"}`), &mdOut)
	var mdResp hookResponse
	if err := json.Unmarshal(mdOut.Bytes(), &mdResp); err != nil {
		t.Fatalf("decode markdown /dash response failed: %v", err)
	}
	if len(mdResp.InjectSteps) != 1 || mdResp.InjectSteps[0].SystemMessage == nil {
		t.Fatalf("expected systemMessage step in markdown mode, got %+v", mdResp)
	}
	mdText, _ := mdResp.InjectSteps[0].SystemMessage.Metadata["user_markdown"].(string)
	if !strings.Contains(mdText, "**`bob@gmail.com`**") {
		t.Fatalf("expected markdown mode to highlight bob@gmail.com, got:\n%s", mdText)
	}
}

func TestNextDirectModeFallbackWithoutSeamlessCtrl(t *testing.T) {
	i18n.SetPref("en")
	t.Cleanup(func() { i18n.SetPref("auto") })

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(vault.CtrlURLEnvKey, "")
	t.Setenv(vault.CtrlTokenEnvKey, "")
	t.Setenv("AGY_PRO_HOOK_RENDER", "")
	v := vault.New(filepath.Join(tmp, "pool"))

	for _, email := range []string{"alpha@gmail.com", "beta@gmail.com"} {
		tf := filepath.Join(tmp, email+".tok")
		raw, _ := json.Marshal(map[string]any{
			"id_token_email": email,
			"token":          map[string]any{"access_token": "tok-" + email, "refresh_token": "ref"},
		})
		_ = os.WriteFile(tf, raw, 0o600)
		_, _ = v.ImportTokenFile(tf)
	}
	_ = v.UpdateState(func(s *vault.State) { s.Active = "alpha@gmail.com" })

	var out bytes.Buffer
	HandlePreInvocation(v, "", strings.NewReader(`{"invocationNum":0,"lastUserInput":"/next beta"}`), &out)
	var resp hookResponse
	_ = json.Unmarshal(out.Bytes(), &resp)
	msg := resp.InjectSteps[0].ErrorMessage.UserMessage
	if !strings.Contains(msg, "alpha@gmail.com → beta@gmail.com") || !strings.Contains(msg, "--no-seamless") {
		t.Fatalf("expected direct-mode fallback confirmation, got:\n%s", msg)
	}
	if got := v.LoadState().Active; got != "beta@gmail.com" {
		t.Fatalf("expected state.Active=beta@gmail.com, got %q", got)
	}
}
