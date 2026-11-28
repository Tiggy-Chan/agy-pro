package seamless

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

type fakeProvider struct {
	mu       sync.Mutex
	idx      int
	accounts []struct {
		email string
		token string
	}
	switches []string
	refreshes int
}

func (f *fakeProvider) Current() (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[f.idx]
	return a.email, a.token, nil
}

func (f *fakeProvider) Refresh(email string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.accounts {
		if f.accounts[i].email == email {
			f.refreshes++
			f.accounts[i].token = "refreshed-" + email
			return f.accounts[i].token, true
		}
	}
	return "", false
}

func (f *fakeProvider) Failover(failedEmail, reason string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accounts[f.idx].email != failedEmail {
		a := f.accounts[f.idx]
		return a.email, a.token, true
	}
	if f.idx+1 >= len(f.accounts) {
		return "", "", false
	}
	f.idx++
	a := f.accounts[f.idx]
	f.switches = append(f.switches, failedEmail+"->"+a.email)
	return a.email, a.token, true
}

func (f *fakeProvider) MaxAttempts() int { return len(f.accounts) }

func TestSeamlessHTTP2GRPC429FailoverAndStreaming(t *testing.T) {
	var sawTokens []string
	var mu sync.Mutex
	var upstreamCalls atomic.Int32

	// Mock Google HTTP/2 gRPC inference server:
	// - tok-alice -> HTTP 200 + 0-byte body + Trailer Grpc-Status: 8 (RESOURCE_EXHAUSTED!)
	// - tok-bob   -> HTTP 429 Too Many Requests
	// - tok-carol -> HTTP 200 streaming gRPC chunks + Trailer Grpc-Status: 0!
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if string(body) != "request-payload-42" {
			t.Errorf("upstream got corrupted body: %q", string(body))
		}
		auth := r.Header.Get("Authorization")
		mu.Lock()
		sawTokens = append(sawTokens, auth)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/grpc")
		switch auth {
		case "Bearer tok-alice":
			w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Grpc-Status", "8")
			w.Header().Set("Grpc-Message", "Individual quota reached (RESOURCE_EXHAUSTED)")
		case "Bearer tok-bob":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"quota exhausted"}`))
		case "Bearer tok-carol":
			w.Header().Set("Trailer", "Grpc-Status")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("chunk-1-"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = w.Write([]byte("chunk-2-done"))
			w.Header().Set("Grpc-Status", "0")
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	upURL, _ := url.Parse(upstream.URL)
	prov := &fakeProvider{
		accounts: []struct {
			email string
			token string
		}{
			{"alice@gmail.com", "tok-alice"},
			{"bob@gmail.com", "tok-bob"},
			{"carol@gmail.com", "tok-carol"},
		},
	}

	p, err := Start(Options{
		Provider:       prov,
		InterceptHosts: []string{upURL.Hostname()},
		UpstreamTLS:    &tls.Config{InsecureSkipVerify: true},
		TempDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer p.Close()

	// Client configured like child `agy`: HTTPS_PROXY -> local proxy, trusts ephemeral CA bundle
	pURL, _ := url.Parse(p.ProxyURL())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(pURL),
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: p.CA().CertPool()},
		},
	}

	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/google.gca.aicode.v1alpha.PredictionService/Generate", strings.NewReader("request-payload-42"))
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	// Client STILL holds stale initial token from startup; proxy transparently injects & rotates!
	req.Header.Set("Authorization", "Bearer tok-alice")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client Do through seamless proxy failed: %v", err)
	}
	defer resp.Body.Close()

	gotBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(gotBody) != "chunk-1-chunk-2-done" {
		t.Fatalf("expected seamless 200 streaming response after failover, got status=%d body=%q", resp.StatusCode, string(gotBody))
	}
	if gs := resp.Trailer.Get("Grpc-Status"); gs != "0" {
		t.Fatalf("expected preserved HTTP/2 trailer Grpc-Status=0, got %q (headers=%v trailers=%v)", gs, resp.Header, resp.Trailer)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("expected HTTP/2 negotiated end-to-end, got %s", resp.Proto)
	}
	if got := upstreamCalls.Load(); got != 3 {
		t.Fatalf("expected 3 upstream attempts (alice 429 -> bob 429 -> carol 200), got %d", got)
	}
	if len(prov.switches) != 2 || prov.switches[0] != "alice@gmail.com->bob@gmail.com" || prov.switches[1] != "bob@gmail.com->carol@gmail.com" {
		t.Fatalf("unexpected failover chain: %v", prov.switches)
	}
}

func TestSeamlessPassthroughLeavesNonInterceptedHostsUntouched(t *testing.T) {
	// Non-intercepted upstream server with its OWN TLS cert (NOT signed by ephemeral CA)
	otherSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("untouched-passthrough"))
	}))
	defer otherSrv.Close()

	prov := &fakeProvider{accounts: []struct{ email, token string }{{"a@gmail.com", "tok-a"}}}
	p, err := Start(Options{
		Provider:       prov,
		InterceptHosts: []string{"only-google.example.invalid"},
		TempDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer p.Close()

	// Client trusts ONLY otherSrv's cert (does NOT trust ephemeral CA!) -> proves zero MITM happened!
	pURL, _ := url.Parse(p.ProxyURL())
	tr := otherSrv.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(pURL)
	client := &http.Client{Transport: tr}

	resp, err := client.Get(otherSrv.URL + "/ping")
	if err != nil {
		t.Fatalf("passthrough GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "untouched-passthrough" {
		t.Fatalf("unexpected passthrough body: %q", string(body))
	}
}

func TestSeamlessAuthRefreshOn401AndCABundleCleanup(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer expired-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") == "Bearer refreshed-alice@gmail.com" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("refreshed-ok"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	upURL, _ := url.Parse(upstream.URL)
	prov := &fakeProvider{
		accounts: []struct{ email, token string }{{"alice@gmail.com", "expired-tok"}},
	}
	p, err := Start(Options{
		Provider:       prov,
		InterceptHosts: []string{upURL.Hostname()},
		UpstreamTLS:    &tls.Config{InsecureSkipVerify: true},
		TempDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	bundlePath := p.CABundlePath()
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("expected CA bundle on disk while running: %v", err)
	}

	pURL, _ := url.Parse(p.ProxyURL())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(pURL),
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: p.CA().CertPool()},
		},
	}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/test", strings.NewReader("hi"))
	req.Header.Set("Authorization", "Bearer expired-tok")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "refreshed-ok" || prov.refreshes != 1 {
		t.Fatalf("expected transparent 401 refresh, got status=%d body=%q refreshes=%d", resp.StatusCode, string(body), prov.refreshes)
	}

	p.Close()
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatalf("expected ephemeral CA bundle removed on Close()")
	}
}

func TestSeamlessConcurrent429DoesNotDoubleSkipAccount(t *testing.T) {
	var bobHits atomic.Int32
	var carolHits atomic.Int32

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/grpc")
		switch auth {
		case "Bearer tok-alice":
			w.Header().Set("Trailer", "Grpc-Status")
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Grpc-Status", "8")
		case "Bearer tok-bob":
			bobHits.Add(1)
			w.Header().Set("Trailer", "Grpc-Status")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("served-by-bob"))
			w.Header().Set("Grpc-Status", "0")
		case "Bearer tok-carol":
			carolHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("served-by-carol"))
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	upURL, _ := url.Parse(upstream.URL)
	prov := &fakeProvider{
		accounts: []struct{ email, token string }{
			{"alice@gmail.com", "tok-alice"},
			{"bob@gmail.com", "tok-bob"},
			{"carol@gmail.com", "tok-carol"},
		},
	}
	p, err := Start(Options{
		Provider:       prov,
		InterceptHosts: []string{upURL.Hostname()},
		UpstreamTLS:    &tls.Config{InsecureSkipVerify: true},
		TempDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer p.Close()

	pURL, _ := url.Parse(p.ProxyURL())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(pURL),
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: p.CA().CertPool()},
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/gen", strings.NewReader("hello"))
			req.Header.Set("Authorization", "Bearer tok-alice")
			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("concurrent Do failed: %v", err)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != "served-by-bob" {
				t.Errorf("expected served-by-bob, got status=%d body=%q", resp.StatusCode, string(body))
			}
		}()
	}
	wg.Wait()

	if bobHits.Load() != 4 || carolHits.Load() != 0 {
		t.Fatalf("expected all 4 concurrent requests served by bob without skipping to carol, got bob=%d carol=%d", bobHits.Load(), carolHits.Load())
	}
}

func TestSeamlessRewritesProjectIDOn403(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			w.Header().Set("Content-Type", "application/json")
			if auth == "Bearer tok-bob" {
				_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj-bob-999"}`))
				return
			}
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"proj-alice-111"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if auth == "Bearer tok-alice" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if auth == "Bearer tok-bob" {
			if strings.Contains(string(body), "proj-alice-111") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"PERMISSION_DENIED on proj-alice-111"}`))
				return
			}
			if strings.Contains(string(body), "proj-bob-999") {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("rewritten-project-ok"))
				return
			}
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	upURL, _ := url.Parse(upstream.URL)
	prov := &fakeProvider{
		accounts: []struct{ email, token string }{
			{"alice@gmail.com", "tok-alice"},
			{"bob@gmail.com", "tok-bob"},
		},
	}
	p, err := Start(Options{
		Provider:       prov,
		InterceptHosts: []string{upURL.Hostname()},
		UpstreamTLS:    &tls.Config{InsecureSkipVerify: true},
		TempDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer p.Close()

	pURL, _ := url.Parse(p.ProxyURL())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(pURL),
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{RootCAs: p.CA().CertPool()},
		},
	}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1internal:streamGenerateContent",
		strings.NewReader(`{"project":"proj-alice-111","model":"gemini-4-neon"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-alice")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "rewritten-project-ok" {
		t.Fatalf("expected 403 project rewrite to succeed with 200, got status=%d body=%q", resp.StatusCode, string(body))
	}
}

func TestCleanNativeEnvStripsParentSeamlessProxyAndRestoresOriginal(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:62860")
	t.Setenv("https_proxy", "http://127.0.0.1:62860")
	t.Setenv("SSL_CERT_FILE", "/tmp/agy-seamless-ca-123/ca-bundle.pem")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/tmp/agy-seamless-ca-123/ca-bundle.pem")
	t.Setenv("AGY_PRO_SEAMLESS_ACTIVE_PROXY", "http://127.0.0.1:62860")
	t.Setenv("AGY_PRO_SEAMLESS_ACTIVE_CA", "/tmp/agy-seamless-ca-123/ca-bundle.pem")
	t.Setenv("AGY_PRO_SEAMLESS_ORIG_HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("AGY_PRO_SEAMLESS_ORIG_SSL_CERT_FILE", "")

	env := vault.CleanNativeEnv("")
	foundProxy := ""
	for _, kv := range env {
		if strings.Contains(kv, "62860") || strings.Contains(kv, "agy-seamless-ca-123") || strings.HasPrefix(kv, "AGY_PRO_SEAMLESS_") {
			t.Fatalf("parent seamless proxy leaked into CleanNativeEnv: %q", kv)
		}
		if strings.HasPrefix(kv, "HTTPS_PROXY=") {
			foundProxy = strings.TrimPrefix(kv, "HTTPS_PROXY=")
		}
	}
	if foundProxy != "http://127.0.0.1:7890" {
		t.Fatalf("expected original HTTPS_PROXY restored to http://127.0.0.1:7890, got %q", foundProxy)
	}
}
