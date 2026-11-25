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

var usageLineRe = regexp.MustCompile(`(?m)^Gemini Models\t(Weekly Limit Remaining|Five Hour Limit Remaining)\t(\d+(?:\.\d+)?)%\t(\S+)`)

func fptr(v float64) *float64 { return &v }

// ProbeAccount queries a single account's live Gemini 5h/7d quota using official
// `agy --print /usage`. Strictly detached via Setsid + Stdin=nil (/dev/null) so
// it can NEVER touch or corrupt the user's controlling terminal.
func ProbeAccount(acct *vault.Account, agyBin string, timeout time.Duration) *vault.Quota {
	q := &vault.Quota{At: float64(time.Now().UnixNano()) / 1e9}

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

	matches := usageLineRe.FindAllStringSubmatch(stdout.String(), -1)
	for _, m := range matches {
		kind := m[1]
		rem, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		used := 100.0 - rem
		if used < 0 {
			used = 0
		}
		resetTS := vault.ParseISOTime(m[3])
		if strings.Contains(kind, "Five Hour") {
			q.H5Rem = fptr(rem)
			q.H5Used = fptr(used)
			q.H5Reset = resetTS
		} else if strings.Contains(kind, "Weekly") {
			q.D7Rem = fptr(rem)
			q.D7Used = fptr(used)
			q.D7Reset = resetTS
		}
	}

	if q.H5Rem == nil && q.D7Rem == nil {
		q.Error = "未返回 Gemini 额度行"
	}
	return q
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

// Effective returns cached quotas from state with expired 5h/7d windows automatically reset.
func Effective(st *vault.State, now float64) map[string]*vault.Quota {
	out := make(map[string]*vault.Quota, len(st.Quotas))
	for email, q := range st.Quotas {
		if q == nil {
			continue
		}
		cp := *q
		if q.H5Used != nil {
			cp.H5Used = fptr(*q.H5Used)
		}
		if q.H5Rem != nil {
			cp.H5Rem = fptr(*q.H5Rem)
		}
		if q.D7Used != nil {
			cp.D7Used = fptr(*q.D7Used)
		}
		if q.D7Rem != nil {
			cp.D7Rem = fptr(*q.D7Rem)
		}
		if cp.H5Reset > 0 && now >= cp.H5Reset {
			cp.H5Rem = fptr(100.0)
			cp.H5Used = fptr(0.0)
		}
		if cp.D7Reset > 0 && now >= cp.D7Reset {
			cp.D7Rem = fptr(100.0)
			cp.D7Used = fptr(0.0)
		}
		out[email] = &cp
	}
	return out
}
