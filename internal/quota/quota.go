package quota

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

var (
	// Non-TTY pipe output: "Gemini Models\tWeekly Limit Remaining\t0%\t2026-12-01T02:03:36Z"
	// Also matches "Claude and GPT models\tFive Hour Limit Remaining\t100%\t..."
	usageTabRe = regexp.MustCompile(`(?im)^(Gemini Models|Claude and GPT models)\t(Weekly Limit Remaining|Five Hour Limit Remaining)\t(\d+(?:\.\d+)?)%\t(\S+)`)
	// Space-aligned fallback: "Gemini Models   Weekly Limit Remaining   0%   2026-12-01 10:03 CST"
	usageSpaceRe = regexp.MustCompile(`(?im)^(Gemini Models|Claude and GPT models)\s{2,}(Weekly Limit Remaining|Five Hour Limit Remaining)\s+(\d+(?:\.\d+)?)%\s+(.+?)\s*$`)
)

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
		if strings.HasPrefix(pool, "gemini") {
			if strings.Contains(kind, "Five Hour") {
				q.H5Rem = fptr(rem)
				q.H5Used = fptr(used)
				q.H5Reset = resetTS
			} else if strings.Contains(kind, "Weekly") {
				q.D7Rem = fptr(rem)
				q.D7Used = fptr(used)
				q.D7Reset = resetTS
			}
		} else if strings.HasPrefix(pool, "claude") {
			if strings.Contains(kind, "Five Hour") {
				q.C5Rem = fptr(rem)
				q.C5Used = fptr(used)
				q.C5Reset = resetTS
			} else if strings.Contains(kind, "Weekly") {
				q.C7Rem = fptr(rem)
				q.C7Used = fptr(used)
				q.C7Reset = resetTS
			}
		}
	}
	if q.H5Rem == nil && q.D7Rem == nil && q.C5Rem == nil && q.C7Rem == nil {
		q.Error = "未返回额度行"
	}
	return q
}

// ProbeAccount queries a single account's live Gemini + Claude/GPT 5h/7d quotas using official
// `agy --print /usage`. Strictly detached via Setsid + Stdin=nil (/dev/null) so
// it can NEVER touch or corrupt the user's controlling terminal.
func ProbeAccount(acct *vault.Account, agyBin string, timeout time.Duration) *vault.Quota {
	now := float64(time.Now().UnixNano()) / 1e9
	q := &vault.Quota{At: now}

	cmd := exec.Command(agyBin, "--settings", `{"modelProvider":""}`, "--print", "/usage")
	cmd.Env = vault.CleanNativeEnv(acct.HomeDir)
	cmd.Stdin = nil // Go attaches /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

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
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		<-done
		q.Error = fmt.Sprintf("超时 (%ds)", int(timeout.Seconds()))
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
