package seamless

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

const maxBufferedRequestBody = 32 << 20 // 32 MiB cap for retryable request bodies

// DefaultInterceptHosts are the Google AI inference/code-assist hosts eligible for
// transparent HTTP/2 gRPC 429 failover. All other domains pass through raw TCP untouched.
var DefaultInterceptHosts = []string{
	"aicode.googleapis.com",
	"cloudcode-pa.googleapis.com",
	"daily-cloudcode-pa.googleapis.com",
	"autopush-cloudcode-pa.googleapis.com",
	"generativelanguage.googleapis.com",
}

type Options struct {
	Provider       TokenProvider
	InterceptHosts []string
	UpstreamTLS    *tls.Config // Optional custom TLS config for upstream (used in tests)
	UpstreamProxy  func(*http.Request) (*url.URL, error)
	LogPath        string
	TempDir        string
}

type Proxy struct {
	ca             *EphemeralCA
	caDir          string
	caBundlePath   string
	listener       net.Listener
	proxyServer    *http.Server
	mitmListener   *chanListener
	mitmServer     *http.Server
	upstreamTr     *http.Transport
	upstreamProxy  func(*http.Request) (*url.URL, error)
	provider       TokenProvider
	interceptHosts map[string]bool

	logMu   sync.Mutex
	logFile *os.File

	projMu       sync.Mutex
	projectByAcc map[string]string

	closeOnce sync.Once
}

func Start(opts Options) (*Proxy, error) {
	if opts.Provider == nil {
		return nil, errors.New("seamless proxy requires a TokenProvider")
	}
	ca, err := NewEphemeralCA()
	if err != nil {
		return nil, err
	}

	caDir, err := os.MkdirTemp(opts.TempDir, "agy-seamless-ca-*")
	if err != nil {
		return nil, err
	}
	bundlePath, err := ca.WriteBundle(caDir)
	if err != nil {
		_ = os.RemoveAll(caDir)
		return nil, err
	}

	hosts := make(map[string]bool)
	srcHosts := opts.InterceptHosts
	if len(srcHosts) == 0 {
		srcHosts = DefaultInterceptHosts
	}
	for _, h := range srcHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			hosts[h] = true
		}
	}
	if extra := strings.TrimSpace(os.Getenv("AGY_PRO_SEAMLESS_HOSTS")); extra != "" {
		for _, h := range strings.Split(extra, ",") {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" {
				hosts[h] = true
			}
		}
	}

	upstreamProxyFn := opts.UpstreamProxy
	if upstreamProxyFn == nil {
		upstreamProxyFn = snapshotEnvProxy()
	}

	upTr := &http.Transport{
		Proxy:                 upstreamProxyFn,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 0,
		TLSClientConfig:       opts.UpstreamTLS,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(caDir)
		return nil, err
	}

	var logF *os.File
	if opts.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(opts.LogPath), 0o700)
		logF, _ = os.OpenFile(opts.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	}

	mitmLn := newChanListener(ln.Addr())
	p := &Proxy{
		ca:             ca,
		caDir:          caDir,
		caBundlePath:   bundlePath,
		listener:       ln,
		mitmListener:   mitmLn,
		upstreamTr:     upTr,
		upstreamProxy:  upstreamProxyFn,
		provider:       opts.Provider,
		interceptHosts: hosts,
		logFile:        logF,
		projectByAcc:   make(map[string]string),
	}

	httpErrLog := log.New(&proxyLogWriter{p: p}, "[http] ", log.Ltime|log.Lmicroseconds)
	p.mitmServer = &http.Server{
		Handler:  http.HandlerFunc(p.handleInterceptedHTTP),
		ErrorLog: httpErrLog,
		TLSConfig: &tls.Config{
			GetCertificate: ca.GetCertificate,
			NextProtos:     []string{"h2", "http/1.1"},
		},
	}
	p.proxyServer = &http.Server{
		Handler:  http.HandlerFunc(p.handleProxyRequest),
		ErrorLog: httpErrLog,
	}

	go func() { _ = p.mitmServer.ServeTLS(mitmLn, "", "") }()
	go func() { _ = p.proxyServer.Serve(ln) }()

	p.logf("started loopback proxy at http://%s (bundle=%s)", ln.Addr().String(), bundlePath)
	return p, nil
}

func (p *Proxy) Addr() string {
	return p.listener.Addr().String()
}

func (p *Proxy) ProxyURL() string {
	return "http://" + p.Addr()
}

func (p *Proxy) CABundlePath() string {
	return p.caBundlePath
}

func (p *Proxy) CA() *EphemeralCA {
	return p.ca
}

// ChildEnv returns a copy of baseEnv configured so child `agy` routes HTTPS through this
// local proxy and trusts the ephemeral per-session CA bundle (while bypassing loopback).
func (p *Proxy) ChildEnv(baseEnv []string) []string {
	drop := map[string]bool{
		"HTTPS_PROXY": true, "https_proxy": true,
		"HTTP_PROXY": true, "http_proxy": true,
		"ALL_PROXY": true, "all_proxy": true,
		"SSL_CERT_FILE":              true,
		"NODE_EXTRA_CA_CERTS":        true,
		vault.SeamlessActiveProxyEnv: true,
		vault.SeamlessActiveCAEnv:    true,
		vault.SeamlessOrigProxyEnv:   true,
		vault.SeamlessOrigSSLCertEnv: true,
	}
	var existingNoProxy, origHTTPSProxy, origSSLCert string
	out := make([]string, 0, len(baseEnv)+12)
	for _, kv := range baseEnv {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(k, "NO_PROXY") {
			existingNoProxy = val
			continue
		}
		if (k == "HTTPS_PROXY" || k == "https_proxy") && origHTTPSProxy == "" && strings.TrimSpace(val) != "" {
			origHTTPSProxy = strings.TrimSpace(val)
		}
		if k == "SSL_CERT_FILE" && origSSLCert == "" && strings.TrimSpace(val) != "" {
			origSSLCert = strings.TrimSpace(val)
		}
		if drop[k] {
			continue
		}
		out = append(out, kv)
	}
	proxyURL := p.ProxyURL()
	noProxy := mergeNoProxy(existingNoProxy, "localhost", "127.0.0.1", "::1")
	out = append(out,
		"HTTPS_PROXY="+proxyURL,
		"https_proxy="+proxyURL,
		"NO_PROXY="+noProxy,
		"no_proxy="+noProxy,
		"SSL_CERT_FILE="+p.caBundlePath,
		"NODE_EXTRA_CA_CERTS="+p.caBundlePath,
		vault.SeamlessActiveProxyEnv+"="+proxyURL,
		vault.SeamlessActiveCAEnv+"="+p.caBundlePath,
	)
	if origHTTPSProxy != "" {
		out = append(out, vault.SeamlessOrigProxyEnv+"="+origHTTPSProxy)
	}
	if origSSLCert != "" {
		out = append(out, vault.SeamlessOrigSSLCertEnv+"="+origSSLCert)
	}
	return out
}

func mergeNoProxy(existing string, extras ...string) string {
	seen := map[string]bool{}
	var parts []string
	for _, item := range strings.Split(existing, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !seen[strings.ToLower(item)] {
			seen[strings.ToLower(item)] = true
			parts = append(parts, item)
		}
	}
	for _, item := range extras {
		if !seen[strings.ToLower(item)] {
			seen[strings.ToLower(item)] = true
			parts = append(parts, item)
		}
	}
	return strings.Join(parts, ",")
}

func (p *Proxy) Close() {
	p.closeOnce.Do(func() {
		p.logf("shutting down seamless proxy")
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		_ = p.proxyServer.Shutdown(ctx)
		_ = p.mitmServer.Shutdown(ctx)
		_ = p.proxyServer.Close()
		_ = p.mitmServer.Close()
		_ = p.mitmListener.Close()
		_ = p.listener.Close()
		p.upstreamTr.CloseIdleConnections()
		p.logMu.Lock()
		if p.logFile != nil {
			_ = p.logFile.Close()
			p.logFile = nil
		}
		p.logMu.Unlock()
		if p.caDir != "" {
			_ = os.RemoveAll(p.caDir)
		}
	})
}

func (p *Proxy) logf(format string, args ...any) {
	if p == nil || p.logFile == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	ts := time.Now().Format("15:04:05.000")
	_, _ = fmt.Fprintf(p.logFile, "[%s] "+format+"\n", append([]any{ts}, args...)...)
}

// proxyLogWriter routes net/http internal server logs strictly to the proxy log file (or discards them),
// guaranteeing ZERO stray bytes ever reach the user's live interactive TTY / stderr.
type proxyLogWriter struct {
	p *Proxy
}

func (w *proxyLogWriter) Write(b []byte) (int, error) {
	if w.p == nil || w.p.logFile == nil {
		return len(b), nil
	}
	w.p.logMu.Lock()
	defer w.p.logMu.Unlock()
	if w.p.logFile != nil {
		_, _ = w.p.logFile.Write(b)
	}
	return len(b), nil
}

func (p *Proxy) shouldIntercept(hostPort string) bool {
	host := strings.ToLower(strings.TrimSpace(hostPort))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return false
	}
	if p.interceptHosts[host] {
		return true
	}
	// Never intercept OAuth token issuance / login endpoints
	if host == "oauth2.googleapis.com" || host == "accounts.google.com" {
		return false
	}
	return false
}

func (p *Proxy) handleProxyRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	// Plain HTTP proxy fallback: forward directly without modification
	p.forwardPlainHTTP(w, r)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	if p.shouldIntercept(target) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		clientConn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			_ = clientConn.Close()
			return
		}
		p.logf("CONNECT intercept %s", target)
		if !p.mitmListener.Push(&taggedConn{Conn: clientConn, host: target}) {
			_ = clientConn.Close()
		}
		return
	}

	// Non-intercepted host: transparent raw TCP tunnel (zero TLS termination)
	p.logf("CONNECT passthrough %s", target)
	upConn, err := p.dialUpstreamTunnel(r.Context(), target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upConn.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, bufRW, err := hj.Hijack()
	if err != nil {
		_ = upConn.Close()
		return
	}
	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = clientConn.Close()
		_ = upConn.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if bufRW.Reader.Buffered() > 0 {
			_, _ = io.CopyN(upConn, bufRW.Reader, int64(bufRW.Reader.Buffered()))
		}
		_, _ = io.Copy(upConn, clientConn)
		if tc, ok := upConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, upConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	wg.Wait()
	_ = clientConn.Close()
	_ = upConn.Close()
}

func (p *Proxy) dialUpstreamTunnel(ctx context.Context, target string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	if p.upstreamProxy != nil {
		probeReq := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Scheme: "https", Host: target},
			Host:   target,
		}
		if proxyURL, err := p.upstreamProxy(probeReq); err == nil && proxyURL != nil && proxyURL.Host != "" {
			conn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
			if err != nil {
				return nil, err
			}
			connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
			if proxyURL.User != nil {
				// Forward upstream proxy auth if configured in user's original HTTPS_PROXY
				req := &http.Request{Header: make(http.Header)}
				pw, _ := proxyURL.User.Password()
				req.SetBasicAuth(proxyURL.User.Username(), pw)
				connectReq += "Proxy-Authorization: " + req.Header.Get("Authorization") + "\r\n"
			}
			connectReq += "\r\n"
			if _, err := io.WriteString(conn, connectReq); err != nil {
				_ = conn.Close()
				return nil, err
			}
			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, probeReq)
			if err != nil {
				_ = conn.Close()
				return nil, err
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				_ = conn.Close()
				return nil, fmt.Errorf("upstream proxy CONNECT %s failed: %s", target, resp.Status)
			}
			return conn, nil
		}
	}
	return dialer.DialContext(ctx, "tcp", target)
}

func (p *Proxy) forwardPlainHTTP(w http.ResponseWriter, r *http.Request) {
	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	stripHopHeaders(outReq.Header)
	resp, err := p.upstreamTr.RoundTrip(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) handleInterceptedHTTP(w http.ResponseWriter, r *http.Request) {
	hasBearer := strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Authorization"))), "bearer ")

	// Buffer request body up to maxBufferedRequestBody so we can replay transparently on 429.
	var bodyBytes []byte
	retryable := false
	if r.Body == nil || r.Body == http.NoBody {
		retryable = true
	} else {
		limited := io.LimitReader(r.Body, maxBufferedRequestBody+1)
		data, err := io.ReadAll(limited)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(data) <= maxBufferedRequestBody {
			bodyBytes = data
			retryable = true
			_ = r.Body.Close()
		} else {
			// Exceeded cap -> stream once without retry
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(data), r.Body), r.Body}
		}
	}

	if !hasBearer || !retryable {
		p.forwardOnce(w, r, bodyBytes, retryable, "")
		return
	}

	maxAttempts := p.provider.MaxAttempts()
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	refreshedEmails := map[string]bool{}
	var lastResp *http.Response
	var lastReader *bufio.Reader
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		email, token, err := p.provider.Current()
		if err != nil || token == "" {
			if lastResp != nil {
				break
			}
			// Fall back to forwarding client's original Authorization header if provider has no token
			p.forwardOnce(w, r, bodyBytes, true, "")
			return
		}

		resp, br, err := p.sendUpstream(r, bodyBytes, true, token)
		if err != nil {
			lastErr = err
			break
		}

		// Inspect status & trailers BEFORE committing any bytes to the client!
		// For HTTP/2 gRPC responses, 200 OK with 0-byte body delivers Grpc-Status in Trailers.
		_, peekErr := br.Peek(1)
		bodyEmpty := errors.Is(peekErr, io.EOF)
		grpcStatus, grpcMsg := extractGrpcStatus(resp, bodyEmpty)

		errSnippet := ""
		if resp.StatusCode >= 400 && !bodyEmpty {
			if peeked, _ := br.Peek(240); len(peeked) > 0 {
				errSnippet = strings.ReplaceAll(string(peeked), "\n", " ")
			}
		}
		p.logf("RPC %s%s acct=%s http=%d grpc=%q bodyEmpty=%v msg=%q err=%q",
			r.Host, r.URL.Path, email, resp.StatusCode, grpcStatus, bodyEmpty, truncStr(grpcMsg, 120), truncStr(errSnippet, 240))

		// Case 1: Expired access token (HTTP 401 or gRPC 16 UNAUTHENTICATED) -> refresh same account once
		if isAuthExpired(resp.StatusCode, grpcStatus, bodyEmpty) && !refreshedEmails[email] {
			refreshedEmails[email] = true
			_ = resp.Body.Close()
			if newTok, ok := p.provider.Refresh(email); ok && newTok != "" {
				p.logf("REFRESH %s -> retrying %s", email, r.URL.Path)
				resp2, br2, err2 := p.sendUpstream(r, bodyBytes, true, newTok)
				if err2 != nil {
					lastErr = err2
					break
				}
				_, peekErr2 := br2.Peek(1)
				bodyEmpty2 := errors.Is(peekErr2, io.EOF)
				grpcStatus2, grpcMsg2 := extractGrpcStatus(resp2, bodyEmpty2)
				resp, br, bodyEmpty, grpcStatus, grpcMsg = resp2, br2, bodyEmpty2, grpcStatus2, grpcMsg2
				p.logf("RPC(after refresh) %s%s acct=%s http=%d grpc=%q bodyEmpty=%v msg=%q",
					r.Host, r.URL.Path, email, resp.StatusCode, grpcStatus, bodyEmpty, truncStr(grpcMsg, 120))
			}
		}

		// Case 2b: Cross-account project mismatch (HTTP 403 on switched account's request carrying old project ID)
		if resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json") && len(bodyBytes) > 0 {
			if targetProj := p.resolveAccountProject(r.Context(), r.Host, email, token); targetProj != "" {
				if rewritten, changed := rewriteJSONProject(bodyBytes, targetProj); changed {
					p.logf("PROJECT REWRITE acct=%s -> project=%s on %s", email, targetProj, r.URL.Path)
					_ = resp.Body.Close()
					resp2, br2, err2 := p.sendUpstream(r, rewritten, true, token)
					if err2 == nil {
						_, peekErr2 := br2.Peek(1)
						bodyEmpty2 := errors.Is(peekErr2, io.EOF)
						grpcStatus2, grpcMsg2 := extractGrpcStatus(resp2, bodyEmpty2)
						resp, br, bodyEmpty, grpcStatus, grpcMsg = resp2, br2, bodyEmpty2, grpcStatus2, grpcMsg2
						p.logf("RPC(after project rewrite) %s%s acct=%s http=%d grpc=%q", r.Host, r.URL.Path, email, resp.StatusCode, grpcStatus)
					}
				}
			}
		}

		// Case 3: Quota 429 / gRPC 8 RESOURCE_EXHAUSTED before any response payload bytes emitted!
		if isQuotaExhausted(resp.StatusCode, grpcStatus, grpcMsg, bodyEmpty) {
			if attempt+1 < maxAttempts {
				reason := fmt.Sprintf("http=%d grpc=%s", resp.StatusCode, grpcStatus)
				if nextEmail, _, switched := p.provider.Failover(email, reason); switched {
					p.logf("SEAMLESS SWITCH %s -> %s on %s (%s)", email, nextEmail, r.URL.Path, reason)
					_ = resp.Body.Close()
					continue
				}
			}
			// No alternative account available -> deliver this 429 cleanly to agy
			lastResp, lastReader = resp, br
			break
		}

		// Cache cloudaicompanionProject passively from loadCodeAssist 200 responses
		if resp.StatusCode == http.StatusOK && strings.Contains(r.URL.Path, "loadCodeAssist") && !bodyEmpty {
			if peeked, _ := br.Peek(4096); len(peeked) > 0 {
				if proj := extractProjectFromJSON(peeked); proj != "" {
					p.setAccountProject(email, proj)
				}
			}
		}

		// Success or non-quota response -> stream immediately to agy with zero buffering delay!
		writeStreamingResponse(w, resp, br)
		return
	}

	if lastResp != nil {
		writeStreamingResponse(w, lastResp, lastReader)
		return
	}
	if lastErr != nil {
		http.Error(w, lastErr.Error(), http.StatusBadGateway)
	}
}

func (p *Proxy) forwardOnce(w http.ResponseWriter, r *http.Request, bodyBytes []byte, buffered bool, overrideToken string) {
	resp, br, err := p.sendUpstream(r, bodyBytes, buffered, overrideToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeStreamingResponse(w, resp, br)
}

func (p *Proxy) sendUpstream(r *http.Request, bodyBytes []byte, buffered bool, overrideToken string) (*http.Response, *bufio.Reader, error) {
	targetURL := &url.URL{
		Scheme:   "https",
		Host:     r.Host,
		Path:     r.URL.Path,
		RawPath:  r.URL.RawPath,
		RawQuery: r.URL.RawQuery,
	}
	var body io.Reader
	if buffered {
		if len(bodyBytes) > 0 {
			body = bytes.NewReader(bodyBytes)
		} else {
			body = http.NoBody
		}
	} else {
		body = r.Body
	}

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), body)
	if err != nil {
		return nil, nil, err
	}
	upReq.Host = r.Host
	copyHeaders(upReq.Header, r.Header)
	stripHopHeaders(upReq.Header)
	if buffered {
		upReq.ContentLength = int64(len(bodyBytes))
	}
	if overrideToken != "" {
		upReq.Header.Set("Authorization", "Bearer "+overrideToken)
	}

	resp, err := p.upstreamTr.RoundTrip(upReq)
	if err != nil {
		return nil, nil, err
	}
	return resp, bufio.NewReaderSize(resp.Body, 8192), nil
}

func writeStreamingResponse(w http.ResponseWriter, resp *http.Response, br *bufio.Reader) {
	defer resp.Body.Close()

	dst := w.Header()
	copyHeaders(dst, resp.Header)
	stripHopHeaders(dst)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

	buf := make([]byte, 8192)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}

	// Propagate HTTP/2 Trailers (e.g. Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin)
	for k, vals := range resp.Trailer {
		for _, v := range vals {
			dst.Add(http.TrailerPrefix+k, v)
		}
	}
}

func extractGrpcStatus(resp *http.Response, bodyEmpty bool) (string, string) {
	gs := resp.Header.Get("Grpc-Status")
	gm := resp.Header.Get("Grpc-Message")
	if bodyEmpty {
		if tgs := resp.Trailer.Get("Grpc-Status"); tgs != "" {
			gs = tgs
		}
		if tgm := resp.Trailer.Get("Grpc-Message"); tgm != "" {
			gm = tgm
		}
	}
	return strings.TrimSpace(gs), strings.TrimSpace(gm)
}

func isAuthExpired(statusCode int, grpcStatus string, bodyEmpty bool) bool {
	if statusCode == http.StatusUnauthorized {
		return true
	}
	if grpcStatus == "16" && (bodyEmpty || statusCode == http.StatusOK) {
		return true
	}
	return false
}

func isQuotaExhausted(statusCode int, grpcStatus, grpcMsg string, bodyEmpty bool) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if grpcStatus == "8" && (bodyEmpty || statusCode == http.StatusOK) {
		return true
	}
	if bodyEmpty && strings.Contains(strings.ToUpper(grpcMsg), "RESOURCE_EXHAUSTED") {
		return true
	}
	return false
}

func (p *Proxy) setAccountProject(email, proj string) {
	if email == "" || proj == "" {
		return
	}
	p.projMu.Lock()
	p.projectByAcc[email] = proj
	p.projMu.Unlock()
}

func (p *Proxy) resolveAccountProject(ctx context.Context, host, email, token string) string {
	p.projMu.Lock()
	if proj := p.projectByAcc[email]; proj != "" {
		p.projMu.Unlock()
		return proj
	}
	p.projMu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, "https://"+host+"/v1internal:loadCodeAssist", strings.NewReader(`{"metadata":{"ideType":"ANTIGRAVITY"}}`))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.upstreamTr.RoundTrip(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if proj := extractProjectFromJSON(raw); proj != "" {
		p.setAccountProject(email, proj)
		return proj
	}
	return ""
}

func extractProjectFromJSON(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if s, ok := m["cloudaicompanionProject"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return ""
}

func rewriteJSONProject(raw []byte, newProject string) ([]byte, bool) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	cur, has := m["project"].(string)
	if !has || cur == newProject {
		return raw, false
	}
	m["project"] = newProject
	out, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return out, true
}

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func stripHopHeaders(h http.Header) {
	for _, k := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"Upgrade",
	} {
		h.Del(k)
	}
	// Note: preserve "TE: trailers" which is mandatory for HTTP/2 gRPC!
}

func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func snapshotEnvProxy() func(*http.Request) (*url.URL, error) {
	activeSeamless := strings.TrimSpace(os.Getenv(vault.SeamlessActiveProxyEnv))
	httpsProxy := firstNonEmpty(os.Getenv("HTTPS_PROXY"), os.Getenv("https_proxy"))
	if activeSeamless != "" && httpsProxy == activeSeamless {
		// Nested invocation inside a parent seamless session: bypass parent loopback proxy
		httpsProxy = strings.TrimSpace(os.Getenv(vault.SeamlessOrigProxyEnv))
	}
	httpProxy := firstNonEmpty(os.Getenv("HTTP_PROXY"), os.Getenv("http_proxy"))
	allProxy := firstNonEmpty(os.Getenv("ALL_PROXY"), os.Getenv("all_proxy"))
	noProxy := firstNonEmpty(os.Getenv("NO_PROXY"), os.Getenv("no_proxy"))
	if httpsProxy == "" && httpProxy == "" && allProxy == "" {
		return nil
	}
	var parsedHTTPS, parsedHTTP *url.URL
	if httpsProxy != "" {
		parsedHTTPS, _ = url.Parse(httpsProxy)
	} else if allProxy != "" {
		parsedHTTPS, _ = url.Parse(allProxy)
	}
	if httpProxy != "" {
		parsedHTTP, _ = url.Parse(httpProxy)
	} else if allProxy != "" {
		parsedHTTP, _ = url.Parse(allProxy)
	}
	noList := strings.Split(noProxy, ",")
	return func(req *http.Request) (*url.URL, error) {
		host := strings.ToLower(req.URL.Hostname())
		for _, item := range noList {
			item = strings.ToLower(strings.TrimSpace(item))
			if item == "" {
				continue
			}
			if item == "*" || host == item || (strings.HasPrefix(item, ".") && strings.HasSuffix(host, item)) || strings.HasSuffix(host, "."+item) {
				return nil, nil
			}
		}
		if req.URL.Scheme == "https" {
			return parsedHTTPS, nil
		}
		return parsedHTTP, nil
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type taggedConn struct {
	net.Conn
	host string
}

func (c *taggedConn) TargetHost() string { return c.host }

// chanListener feeds hijacked TLS client connections into http.Server.ServeTLS.
type chanListener struct {
	addr   net.Addr
	ch     chan net.Conn
	done   chan struct{}
	once   sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		addr: addr,
		ch:   make(chan net.Conn, 64),
		done: make(chan struct{}),
	}
}

func (l *chanListener) Push(c net.Conn) bool {
	select {
	case <-l.done:
		return false
	case l.ch <- c:
		return true
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case c := <-l.ch:
		return c, nil
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
