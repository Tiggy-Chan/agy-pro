package seamless

import (
	"errors"
	"sync"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/quota"
	"github.com/Tiggy-Chan/agy-pro/internal/router"
	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

// TokenProvider supplies active OAuth bearer tokens and performs thread-safe seamless failover.
type TokenProvider interface {
	Current() (email string, accessToken string, err error)
	Refresh(email string) (accessToken string, ok bool)
	Failover(failedEmail string, reason string) (nextEmail string, nextToken string, ok bool)
	MaxAttempts() int
}

// VaultTokenProvider manages live multi-account OAuth tokens backed by *vault.Vault.
type VaultTokenProvider struct {
	vault      *vault.Vault
	agyBin     string
	reservePct float64
	autoSwitch bool
	onSwitch   func(oldEmail, newEmail, reason string)

	mu            sync.Mutex
	currentEmail  string
	accounts      map[string]*vault.Account
	order         []string
	cachedTokens  map[string]cachedTok
	lastRefreshAt map[string]time.Time
}

type cachedTok struct {
	token  string
	expiry float64
}

func NewVaultTokenProvider(
	v *vault.Vault,
	agyBin string,
	initialEmail string,
	reservePct float64,
	autoSwitch bool,
	onSwitch func(oldEmail, newEmail, reason string),
) *VaultTokenProvider {
	accounts := v.ListAccounts()
	st := v.LoadState()
	order := v.OrderedEmails(accounts, st)
	p := &VaultTokenProvider{
		vault:         v,
		agyBin:        agyBin,
		reservePct:    reservePct,
		autoSwitch:    autoSwitch,
		onSwitch:      onSwitch,
		currentEmail:  initialEmail,
		accounts:      accounts,
		order:         order,
		cachedTokens:  make(map[string]cachedTok),
		lastRefreshAt: make(map[string]time.Time),
	}
	p.prewarmNextStandbyAsync(initialEmail)
	return p
}

// prewarmNextStandbyAsync quietly refreshes the next standby account's OAuth token in the
// background so mid-turn 429 failover completes in <1ms with zero refresh latency.
func (p *VaultTokenProvider) prewarmNextStandbyAsync(curEmail string) {
	if !p.autoSwitch {
		return
	}
	go func() {
		now := float64(time.Now().UnixNano()) / 1e9
		st := p.vault.LoadState()
		quotas := quota.Effective(st, now)
		p.mu.Lock()
		order := append([]string(nil), p.order...)
		accounts := p.accounts
		p.mu.Unlock()

		standby := router.NextStandby(order, curEmail, st, quotas, now)
		if standby == "" {
			return
		}
		acct := accounts[standby]
		if acct == nil {
			return
		}
		if tok, exp := readBestTokenFromDisk(acct); tok != "" && (exp == 0 || exp > now+180) {
			p.mu.Lock()
			p.cachedTokens[standby] = cachedTok{token: tok, expiry: exp}
			p.mu.Unlock()
			return
		}
		_, _ = p.ensureFreshToken(acct, true)
	}()
}

func (p *VaultTokenProvider) MaxAttempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.autoSwitch || len(p.order) < 1 {
		return 1
	}
	return len(p.order)
}

func (p *VaultTokenProvider) CurrentEmail() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentEmail
}

func (p *VaultTokenProvider) Current() (string, string, error) {
	p.mu.Lock()
	email := p.currentEmail
	acct := p.accounts[email]
	p.mu.Unlock()
	if acct == nil {
		return "", "", errors.New("active account not found in vault")
	}
	tok, err := p.ensureFreshToken(acct, false)
	if err != nil {
		return email, "", err
	}
	return email, tok, nil
}

func (p *VaultTokenProvider) Refresh(email string) (string, bool) {
	p.mu.Lock()
	acct := p.accounts[email]
	p.mu.Unlock()
	if acct == nil {
		return "", false
	}
	tok, err := p.ensureFreshToken(acct, true)
	if err != nil || tok == "" {
		return "", false
	}
	return tok, true
}

func (p *VaultTokenProvider) Failover(failedEmail, reason string) (string, string, bool) {
	if !p.autoSwitch {
		return "", "", false
	}

	p.mu.Lock()
	// Concurrent 429 guard: if another request already switched away from failedEmail,
	// reuse the newly active account instead of skipping it!
	if p.currentEmail != "" && p.currentEmail != failedEmail {
		cur := p.currentEmail
		acct := p.accounts[cur]
		p.mu.Unlock()
		if acct == nil {
			return "", "", false
		}
		tok, err := p.ensureFreshToken(acct, false)
		if err != nil || tok == "" {
			return "", "", false
		}
		return cur, tok, true
	}

	now := float64(time.Now().UnixNano()) / 1e9
	_ = p.vault.UpdateState(func(s *vault.State) {
		router.MarkCooldown(s, failedEmail, 1800, "seamless 429 ("+reason+")", now)
	})

	p.accounts = p.vault.ListAccounts()
	st := p.vault.LoadState()
	p.order = p.vault.OrderedEmails(p.accounts, st)
	quotas := quota.Effective(st, now)

	nextEmail, _ := router.PickDrainWithReserve(p.order, st, quotas, true, p.reservePct, now)
	if nextEmail == "" || nextEmail == failedEmail {
		p.mu.Unlock()
		return "", "", false
	}
	nextAcct := p.accounts[nextEmail]
	if nextAcct == nil {
		p.mu.Unlock()
		return "", "", false
	}

	p.currentEmail = nextEmail
	_ = p.vault.UpdateState(func(s *vault.State) {
		s.Active = nextEmail
		s.Order = p.order
	})
	_ = p.vault.ActivateToken(nextAcct)
	cb := p.onSwitch
	p.mu.Unlock()

	tok, err := p.ensureFreshToken(nextAcct, false)
	if err != nil || tok == "" {
		return "", "", false
	}
	if cb != nil {
		cb(failedEmail, nextEmail, reason)
	}
	p.prewarmNextStandbyAsync(nextEmail)
	return nextEmail, tok, true
}

func (p *VaultTokenProvider) ensureFreshToken(acct *vault.Account, forceRefresh bool) (string, error) {
	now := float64(time.Now().UnixNano()) / 1e9

	if !forceRefresh {
		p.mu.Lock()
		if ct, ok := p.cachedTokens[acct.Email]; ok && ct.token != "" && (ct.expiry == 0 || ct.expiry > now+30) {
			p.mu.Unlock()
			return ct.token, nil
		}
		p.mu.Unlock()

		if tok, exp := readBestTokenFromDisk(acct); tok != "" && (exp == 0 || exp > now+30) {
			p.mu.Lock()
			p.cachedTokens[acct.Email] = cachedTok{token: tok, expiry: exp}
			p.mu.Unlock()
			return tok, nil
		}
	}

	// Rate-limit forced CLI refreshes to at most once every 15s per account
	p.mu.Lock()
	if time.Since(p.lastRefreshAt[acct.Email]) < 15*time.Second {
		ct := p.cachedTokens[acct.Email]
		p.mu.Unlock()
		if ct.token != "" {
			return ct.token, nil
		}
		return "", errors.New("token refresh throttled")
	}
	p.lastRefreshAt[acct.Email] = time.Now()
	p.mu.Unlock()

	// Trigger official silent OAuth refresh via detached `agy --print /usage` in isolated acct.HomeDir
	_ = quota.ProbeAccountCLI(acct, p.agyBin, 25*time.Second)
	tok, exp := readBestTokenFromDisk(acct)
	if tok == "" {
		return "", errors.New("no fresh access_token available after refresh")
	}
	p.mu.Lock()
	p.cachedTokens[acct.Email] = cachedTok{token: tok, expiry: exp}
	p.mu.Unlock()
	return tok, nil
}

func readBestTokenFromDisk(acct *vault.Account) (string, float64) {
	var bestTok string
	var bestExp float64
	if meta, err := vault.ReadTokenMeta(acct.TokenFile); err == nil && meta.AccessToken != "" {
		bestTok = meta.AccessToken
		bestExp = meta.Expiry
	}
	if activeMeta, err := vault.ReadTokenMeta(vault.ActiveTokenFile()); err == nil &&
		activeMeta.Email == acct.Email && activeMeta.AccessToken != "" && activeMeta.Expiry > bestExp {
		bestTok = activeMeta.AccessToken
		bestExp = activeMeta.Expiry
	}
	return bestTok, bestExp
}
