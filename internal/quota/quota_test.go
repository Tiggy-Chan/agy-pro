package quota

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func appendTag(dst []byte, num, wt uint64) []byte {
	return appendVarint(dst, (num<<3)|wt)
}

func appendStringField(dst []byte, num uint64, s string) []byte {
	dst = appendTag(dst, num, 2)
	dst = appendVarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func appendBytesField(dst []byte, num uint64, b []byte) []byte {
	dst = appendTag(dst, num, 2)
	dst = appendVarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func appendFloat32Field(dst []byte, num uint64, v float32) []byte {
	dst = appendTag(dst, num, 5)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	return append(dst, b[:]...)
}

func buildBucket(id, disp, win string, rem float32, includeRem bool, resetSec int64) []byte {
	var b []byte
	b = appendStringField(b, 1, id)
	b = appendStringField(b, 2, disp)
	b = appendStringField(b, 4, win)
	if includeRem {
		b = appendFloat32Field(b, 5, rem)
	}
	if resetSec > 0 {
		var ts []byte
		ts = appendTag(ts, 1, 0)
		ts = appendVarint(ts, uint64(resetSec))
		b = appendBytesField(b, 7, ts)
	}
	return b
}

func buildTestQuotaResponse(g5Rem float32, g5Omit bool, g7Rem float32, g5Reset, g7Reset int64) []byte {
	var gGroup []byte
	gGroup = appendStringField(gGroup, 1, "Gemini Models")
	gGroup = appendBytesField(gGroup, 3, buildBucket("gemini-weekly", "Weekly Limit Remaining", "weekly", g7Rem, g7Rem != 0, g7Reset))
	gGroup = appendBytesField(gGroup, 3, buildBucket("gemini-5h", "Five Hour Limit Remaining", "5h", g5Rem, !g5Omit, g5Reset))

	var cGroup []byte
	cGroup = appendStringField(cGroup, 1, "Claude and GPT models")
	cGroup = appendBytesField(cGroup, 3, buildBucket("3p-weekly", "Weekly Limit Remaining", "weekly", 0.4, true, g7Reset))
	cGroup = appendBytesField(cGroup, 3, buildBucket("3p-5h", "Five Hour Limit Remaining", "5h", 1.0, true, 0))

	var summary []byte
	summary = appendBytesField(summary, 2, gGroup)
	summary = appendBytesField(summary, 2, cGroup)

	var resp []byte
	resp = appendBytesField(resp, 2, summary)
	return resp
}

func grpcFrame(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func TestParseFetchQuotaStatusProtoIncludingZeroOmitted(t *testing.T) {
	now := float64(time.Now().Unix())
	g5Reset := int64(now) + 3600
	g7Reset := int64(now) + 5*86400

	// Proto3 omits remaining_fraction when 0.0 (exhausted!)
	msg := buildTestQuotaResponse(0, true, 0.7331, g5Reset, g7Reset)
	q := ParseFetchQuotaStatusProto(msg, now)
	if q.Error != "" {
		t.Fatalf("unexpected parse error: %s", q.Error)
	}
	if q.H5Rem == nil || *q.H5Rem != 0 || *q.H5Used != 100 || q.H5Reset != float64(g5Reset) {
		t.Fatalf("expected exhausted 5h (0%% rem, 100%% used), got rem=%v used=%v reset=%v", q.H5Rem, q.H5Used, q.H5Reset)
	}
	if q.D7Rem == nil || *q.D7Rem != 73.3 || *q.D7Used != 26.7 || q.D7Reset != float64(g7Reset) {
		t.Fatalf("expected 7d 73.3%% rem / 26.7%% used, got rem=%v used=%v", q.D7Rem, q.D7Used)
	}
	if q.C7Rem == nil || *q.C7Rem != 40 || *q.C7Used != 60 || q.C5Rem == nil || *q.C5Rem != 100 || q.C5Reset != 0 {
		t.Fatalf("unexpected Claude/GPT pool: C5=%v/%v(reset=%v) C7=%v/%v", q.C5Rem, q.C5Used, q.C5Reset, q.C7Rem, q.C7Used)
	}
}

func TestProbeAccountDirectGRPCFastPathAndCLIFallback(t *testing.T) {
	now := time.Now()
	payload := buildTestQuotaResponse(0.85, false, 0.90, now.Unix()+3600, now.Unix()+86400)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/grpc")
		if auth == "Bearer valid-token" {
			w.Header().Set("Trailer", "Grpc-Status")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(grpcFrame(payload))
			w.Header().Set("Grpc-Status", "0")
			return
		}
		w.Header().Set("Grpc-Status", "16")
		w.Header().Set("Grpc-Message", "UNAUTHENTICATED")
		w.WriteHeader(http.StatusOK)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	origEndpoint := DirectQuotaEndpoint
	origClient := DirectHTTPClient
	DirectQuotaEndpoint = srv.URL + "/google.gca.aicode.v1alpha.PredictionService/FetchQuotaStatus"
	DirectHTTPClient = srv.Client()
	t.Cleanup(func() {
		DirectQuotaEndpoint = origEndpoint
		DirectHTTPClient = origClient
	})

	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "empty-home"))

	writeToken := func(name, email, tok, exp string) *vault.Account {
		dir := filepath.Join(tmp, name)
		tf := filepath.Join(dir, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		_ = os.MkdirAll(filepath.Dir(tf), 0o700)
		raw, _ := json.Marshal(map[string]any{
			"id_token_email": email,
			"token":          map[string]any{"access_token": tok, "refresh_token": "r", "expiry": exp},
		})
		_ = os.WriteFile(tf, raw, 0o600)
		return &vault.Account{Email: email, HomeDir: dir, TokenFile: tf}
	}

	// 1. Valid token -> MUST hit direct gRPC fast path without ever invoking agyBin!
	acctValid := writeToken("valid", "alice@example.com", "valid-token", now.Add(30*time.Minute).Format(time.RFC3339))
	q1 := ProbeAccount(acctValid, "/nonexistent/must-not-be-called", 5*time.Second)
	if q1.Error != "" || q1.H5Rem == nil || *q1.H5Rem != 85 || q1.D7Rem == nil || *q1.D7Rem != 90 {
		t.Fatalf("expected direct gRPC fast path result 85%%/90%%, got %+v", q1)
	}

	// 2. Expired token or gRPC UNAUTHENTICATED -> MUST fall back seamlessly to agy CLI!
	mockAgy := filepath.Join(tmp, "mock-agy")
	script := "#!/bin/sh\nprintf 'Gemini Models\\tFive Hour Limit Remaining\\t50%%\\t2030-01-01T00:00:00Z\\nGemini Models\\tWeekly Limit Remaining\\t70%%\\t2030-01-05T00:00:00Z\\n'\n"
	_ = os.WriteFile(mockAgy, []byte(script), 0o755)

	acctExpired := writeToken("expired", "bob@example.com", "expired-token", now.Add(-5*time.Minute).Format(time.RFC3339))
	q2 := ProbeAccount(acctExpired, mockAgy, 5*time.Second)
	if q2.Error != "" || q2.H5Rem == nil || *q2.H5Rem != 50 || q2.D7Rem == nil || *q2.D7Rem != 70 {
		t.Fatalf("expected CLI fallback for expired token, got %+v", q2)
	}

	acctRejected := writeToken("rejected", "carol@example.com", "revoked-token", now.Add(30*time.Minute).Format(time.RFC3339))
	q3 := ProbeAccount(acctRejected, mockAgy, 5*time.Second)
	if q3.Error != "" || q3.H5Rem == nil || *q3.H5Rem != 50 {
		t.Fatalf("expected CLI fallback after gRPC status=16, got %+v", q3)
	}
}
