package quota

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

const defaultDirectQuotaEndpoint = "https://aicode.googleapis.com/google.gca.aicode.v1alpha.PredictionService/FetchQuotaStatus"

var (
	// DirectQuotaEndpoint is the direct HTTP/2 gRPC endpoint used by native agy for `/usage`.
	// Exported so unit tests can point it at a local HTTP/2 test server.
	DirectQuotaEndpoint = defaultDirectQuotaEndpoint

	// DirectHTTPClient reuses HTTP/2 TLS connections across concurrent account probes and honors
	// standard proxy environment variables (HTTPS_PROXY / HTTP_PROXY / NO_PROXY).
	DirectHTTPClient = newDirectHTTPClient()

	// Non-TTY pipe output: "Gemini Models\tWeekly Limit Remaining\t0%\t2026-12-01T02:03:36Z"
	// Also matches "Claude and GPT models\tFive Hour Limit Remaining\t100%\t..."
	usageTabRe = regexp.MustCompile(`(?im)^(Gemini Models|Claude and GPT models)\t(Weekly Limit Remaining|Five Hour Limit Remaining)\t(\d+(?:\.\d+)?)%\t(\S+)`)
	// Space-aligned fallback: "Gemini Models   Weekly Limit Remaining   0%   2026-12-01 10:03 CST"
	usageSpaceRe = regexp.MustCompile(`(?im)^(Gemini Models|Claude and GPT models)\s{2,}(Weekly Limit Remaining|Five Hour Limit Remaining)\s+(\d+(?:\.\d+)?)%\s+(.+?)\s*$`)
)

func newDirectHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	tr.MaxIdleConns = 32
	tr.MaxIdleConnsPerHost = 16
	tr.IdleConnTimeout = 60 * time.Second
	return &http.Client{Transport: tr}
}

func fptr(v float64) *float64 { return &v }

// ParseUsageOutput parses both Gemini and Claude/GPT 5h/7d pools from `agy --print /usage`.
func ParseUsageOutput(out string, now float64) *vault.Quota {
	q := &vault.Quota{At: now}
	matches := usageTabRe.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		matches = usageSpaceRe.FindAllStringSubmatch(out, -1)
	}
	for _, m := range matches {
		pool := strings.ToLower(m[1])
		kind := m[2]
		rem, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		used := 100.0 - rem
		if used < 0 {
			used = 0
		}
		resetTS := parseUsageResetTime(m[4])
		assignBucketQuota(q, pool, "", kind, "", rem, resetTS)
	}
	if q.H5Rem == nil && q.D7Rem == nil && q.C5Rem == nil && q.C7Rem == nil {
		q.Error = i18n.T("quota.err.no_lines")
	}
	return q
}

func assignBucketQuota(q *vault.Quota, groupName, bucketID, displayName, window string, remPct, resetTS float64) {
	if remPct < 0 {
		remPct = 0
	}
	if remPct > 100 {
		remPct = 100
	}
	rem := math.Round(remPct*10) / 10
	used := math.Round((100.0-rem)*10) / 10
	if used < 0 {
		used = 0
	}

	lowGroup := strings.ToLower(groupName)
	lowID := strings.ToLower(bucketID)
	lowDisp := strings.ToLower(displayName)
	lowWin := strings.ToLower(window)

	isGemini := strings.HasPrefix(lowGroup, "gemini") || strings.HasPrefix(lowID, "gemini")
	isClaude := strings.HasPrefix(lowGroup, "claude") || strings.HasPrefix(lowID, "3p")
	is5h := lowWin == "5h" || strings.Contains(lowID, "5h") || strings.Contains(lowDisp, "five hour")
	is7d := lowWin == "weekly" || strings.Contains(lowID, "weekly") || strings.Contains(lowDisp, "weekly")

	switch {
	case isGemini && is5h:
		q.H5Rem = fptr(rem)
		q.H5Used = fptr(used)
		q.H5Reset = resetTS
	case isGemini && is7d:
		q.D7Rem = fptr(rem)
		q.D7Used = fptr(used)
		q.D7Reset = resetTS
	case isClaude && is5h:
		q.C5Rem = fptr(rem)
		q.C5Used = fptr(used)
		q.C5Reset = resetTS
	case isClaude && is7d:
		q.C7Rem = fptr(rem)
		q.C7Used = fptr(used)
		q.C7Reset = resetTS
	}
}

type protoField struct {
	num     uint64
	wtype   uint64
	varint  uint64
	fixed32 uint32
	bytes   []byte
}

func readProtoVarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, 0
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

func parseProtoFields(b []byte) ([]protoField, bool) {
	var out []protoField
	for len(b) > 0 {
		tag, n := readProtoVarint(b)
		if n <= 0 {
			return nil, false
		}
		b = b[n:]
		num, wt := tag>>3, tag&7
		if num == 0 {
			return nil, false
		}
		f := protoField{num: num, wtype: wt}
		switch wt {
		case 0:
			v, m := readProtoVarint(b)
			if m <= 0 {
				return nil, false
			}
			f.varint = v
			b = b[m:]
		case 1:
			if len(b) < 8 {
				return nil, false
			}
			b = b[8:]
		case 2:
			l, m := readProtoVarint(b)
			if m <= 0 || l > uint64(len(b[m:])) {
				return nil, false
			}
			f.bytes = b[m : m+int(l)]
			b = b[m+int(l):]
		case 5:
			if len(b) < 4 {
				return nil, false
			}
			f.fixed32 = binary.LittleEndian.Uint32(b[:4])
			b = b[4:]
		default:
			return nil, false
		}
		out = append(out, f)
	}
	return out, true
}

func parseProtoBucket(q *vault.Quota, groupName string, raw []byte) {
	fields, ok := parseProtoFields(raw)
	if !ok {
		return
	}
	var bucketID, displayName, window string
	// In proto3, float remaining_fraction == 0.0 (0% remaining / 100% used) is omitted on the wire.
	var remFrac float32
	var resetSec int64

	for _, bf := range fields {
		switch {
		case bf.num == 1 && bf.wtype == 2:
			bucketID = string(bf.bytes)
		case bf.num == 2 && bf.wtype == 2:
			displayName = string(bf.bytes)
		case bf.num == 4 && bf.wtype == 2:
			window = string(bf.bytes)
		case bf.num == 5 && bf.wtype == 5:
			v := math.Float32frombits(bf.fixed32)
			if !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0) {
				remFrac = v
			}
		case bf.num == 7 && bf.wtype == 2:
			if tFields, tok := parseProtoFields(bf.bytes); tok {
				for _, tf := range tFields {
					if tf.num == 1 && tf.wtype == 0 {
						resetSec = int64(tf.varint)
					}
				}
			}
		}
	}
	if bucketID == "" && displayName == "" && window == "" {
		return
	}
	resetTS := 0.0
	if resetSec > 86400 {
		resetTS = float64(resetSec)
	}
	assignBucketQuota(q, groupName, bucketID, displayName, window, float64(remFrac)*100.0, resetTS)
}

// ParseFetchQuotaStatusProto decodes a FetchQuotaStatusResponse protobuf payload into a Quota snapshot.
func ParseFetchQuotaStatusProto(msg []byte, now float64) *vault.Quota {
	q := &vault.Quota{At: now}
	top, ok := parseProtoFields(msg)
	if !ok {
		q.Error = i18n.T("quota.err.parse_failed")
		return q
	}
	for _, f := range top {
		if f.num != 2 || f.wtype != 2 {
			continue
		}
		summaryFields, sok := parseProtoFields(f.bytes)
		if !sok {
			continue
		}
		for _, sf := range summaryFields {
			if sf.num == 2 && sf.wtype == 2 {
				groupFields, gok := parseProtoFields(sf.bytes)
				if !gok {
					continue
				}
				var groupName string
				for _, gf := range groupFields {
					if gf.num == 1 && gf.wtype == 2 {
						groupName = string(gf.bytes)
					}
				}
				for _, gf := range groupFields {
					if gf.num == 3 && gf.wtype == 2 {
						parseProtoBucket(q, groupName, gf.bytes)
					}
				}
			} else if sf.num == 1 && sf.wtype == 2 {
				parseProtoBucket(q, "", sf.bytes)
			}
		}
	}
	if q.H5Rem == nil && q.D7Rem == nil && q.C5Rem == nil && q.C7Rem == nil {
		q.Error = i18n.T("quota.err.no_lines")
	}
	return q
}

func probeDirectGRPC(accessToken string, timeout time.Duration, now float64) (*vault.Quota, error) {
	if accessToken == "" || DirectQuotaEndpoint == "" {
		return nil, errors.New("missing access token or endpoint")
	}
	reqTimeout := timeout
	if reqTimeout <= 0 || reqTimeout > 8*time.Second {
		reqTimeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), reqTimeout)
	defer cancel()

	// Empty unary gRPC frame: compressed=0, length=0
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DirectQuotaEndpoint, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "antigravity")
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := DirectHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	gs := resp.Trailer.Get("Grpc-Status")
	if gs == "" {
		gs = resp.Header.Get("Grpc-Status")
	}
	if gs != "" && gs != "0" {
		gm := resp.Trailer.Get("Grpc-Message")
		if gm == "" {
			gm = resp.Header.Get("Grpc-Message")
		}
		return nil, fmt.Errorf("grpc %s: %s", gs, gm)
	}
	if len(body) < 5 || body[0] != 0 {
		return nil, errors.New("invalid or compressed grpc frame")
	}
	msgLen := binary.BigEndian.Uint32(body[1:5])
	if int(5+msgLen) > len(body) {
		return nil, errors.New("truncated grpc frame")
	}
	q := ParseFetchQuotaStatusProto(body[5:5+msgLen], now)
	if q.Error != "" {
		return nil, errors.New(q.Error)
	}
	return q, nil
}

func resolveFreshAccessToken(acct *vault.Account, now float64) string {
	var bestToken string
	var bestExpiry float64

	if meta, err := vault.ReadTokenMeta(acct.TokenFile); err == nil && meta.AccessToken != "" {
		bestToken = meta.AccessToken
		bestExpiry = meta.Expiry
	}
	// If the currently active native session refreshed the same account's token more recently, use it.
	if activeMeta, err := vault.ReadTokenMeta(vault.ActiveTokenFile()); err == nil &&
		activeMeta.Email == acct.Email && activeMeta.AccessToken != "" && activeMeta.Expiry > bestExpiry {
		bestToken = activeMeta.AccessToken
		bestExpiry = activeMeta.Expiry
	}
	if bestToken == "" {
		return ""
	}
	if bestExpiry > 0 && bestExpiry <= now+15 {
		return ""
	}
	return bestToken
}

// ProbeAccount queries a single account's live Gemini + Claude/GPT 5h/7d quotas.
// Fast path (~0.5s): direct HTTP/2 gRPC FetchQuotaStatus using the account's valid OAuth access_token.
// Fallback path: detached official `agy --print /usage` (handles expired token refresh & future schema changes).
func ProbeAccount(acct *vault.Account, agyBin string, timeout time.Duration) *vault.Quota {
	now := float64(time.Now().UnixNano()) / 1e9
	if token := resolveFreshAccessToken(acct, now); token != "" {
		if q, err := probeDirectGRPC(token, timeout, now); err == nil && q != nil && q.Error == "" {
			return q
		}
	}
	return probeViaAgyCLI(acct, agyBin, timeout, now)
}

// ProbeAccountCLI forces a detached official `agy --print /usage` probe, which also triggers
// official OAuth refresh_token renewal when the access_token is expired or about to expire.
func ProbeAccountCLI(acct *vault.Account, agyBin string, timeout time.Duration) *vault.Quota {
	now := float64(time.Now().UnixNano()) / 1e9
	return probeViaAgyCLI(acct, agyBin, timeout, now)
}

func probeViaAgyCLI(acct *vault.Account, agyBin string, timeout time.Duration, now float64) *vault.Quota {
	q := &vault.Quota{At: now}

	cmd := exec.Command(agyBin, "--settings", `{"modelProvider":""}`, "--print", "/usage")
	cmd.Env = vault.CleanNativeEnv(acct.HomeDir)
	cmd.Stdin = nil // Go attaches /dev/null (NUL on Windows)
	vault.DetachCmd(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		q.Error = truncErr(err.Error())
		return q
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(stdout.String())
			}
			if msg != "" {
				lines := strings.Split(msg, "\n")
				msg = strings.TrimSpace(lines[len(lines)-1])
			} else {
				msg = err.Error()
			}
			q.Error = truncErr(msg)
			return q
		}
	case <-timer.C:
		vault.KillProcessTree(cmd.Process)
		<-done
		q.Error = i18n.T("quota.err.timeout", int(timeout.Seconds()))
		return q
	}

	return ParseUsageOutput(stdout.String(), now)
}

func parseUsageResetTime(raw string) float64 {
	if ts := vault.ParseISOTime(raw); ts > 86400 {
		return ts
	}
	s := strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02 15:04 MST", "2006-01-02 15:04:05 MST", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if t.Unix() > 86400 {
				return float64(t.Unix())
			}
			return 0
		}
	}
	return 0
}

func truncErr(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80])
	}
	return s
}

// RefreshAll probes accounts concurrently with bounded parallelism and reports progress.
func RefreshAll(
	accounts []*vault.Account,
	agyBin string,
	maxWorkers int,
	timeout time.Duration,
	onProgress func(done, total int, email string, q *vault.Quota),
) map[string]*vault.Quota {
	results := make(map[string]*vault.Quota, len(accounts))
	if len(accounts) == 0 {
		return results
	}
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if maxWorkers > len(accounts) {
		maxWorkers = len(accounts)
	}

	type jobResult struct {
		email string
		q     *vault.Quota
	}
	jobs := make(chan *vault.Account, len(accounts))
	resCh := make(chan jobResult, len(accounts))

	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for acct := range jobs {
				q := ProbeAccount(acct, agyBin, timeout)
				resCh <- jobResult{email: acct.Email, q: q}
			}
		}()
	}

	for _, a := range accounts {
		jobs <- a
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(resCh)
	}()

	done := 0
	total := len(accounts)
	for r := range resCh {
		results[r.email] = r.q
		done++
		if onProgress != nil {
			onProgress(done, total, r.email, r.q)
		}
	}
	return results
}

func clonePtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Effective returns cached quotas from state with expired 5h/7d windows automatically reset.
func Effective(st *vault.State, now float64) map[string]*vault.Quota {
	out := make(map[string]*vault.Quota, len(st.Quotas))
	for email, q := range st.Quotas {
		if q == nil {
			continue
		}
		cp := *q
		cp.H5Used = clonePtr(q.H5Used)
		cp.H5Rem = clonePtr(q.H5Rem)
		cp.D7Used = clonePtr(q.D7Used)
		cp.D7Rem = clonePtr(q.D7Rem)
		cp.C5Used = clonePtr(q.C5Used)
		cp.C5Rem = clonePtr(q.C5Rem)
		cp.C7Used = clonePtr(q.C7Used)
		cp.C7Rem = clonePtr(q.C7Rem)

		if cp.H5Reset > 0 && now >= cp.H5Reset {
			cp.H5Rem = fptr(100.0)
			cp.H5Used = fptr(0.0)
		}
		if cp.D7Reset > 0 && now >= cp.D7Reset {
			cp.D7Rem = fptr(100.0)
			cp.D7Used = fptr(0.0)
		}
		if cp.C5Reset > 0 && now >= cp.C5Reset {
			cp.C5Rem = fptr(100.0)
			cp.C5Used = fptr(0.0)
		}
		if cp.C7Reset > 0 && now >= cp.C7Reset {
			cp.C7Rem = fptr(100.0)
			cp.C7Used = fptr(0.0)
		}
		out[email] = &cp
	}
	return out
}
