package router

import (
	"fmt"

	"github.com/Tiggy-Chan/agy-pro/internal/vault"
)

func PruneExpiredCooldowns(st *vault.State, now float64) {
	for email, cd := range st.Cooldowns {
		if cd == nil || cd.Until <= now {
			delete(st.Cooldowns, email)
		}
	}
}

func AccountAvailability(email string, st *vault.State, quotas map[string]*vault.Quota, now float64) (bool, string) {
	if cd := st.Cooldowns[email]; cd != nil && cd.Until > now {
		return false, fmt.Sprintf("冷却中 (%s)", vault.FmtDuration(cd.Until-now))
	}
	if q := quotas[email]; q != nil {
		if q.D7Rem != nil && *q.D7Rem <= 0.5 {
			return false, fmt.Sprintf("7d 已满 (%s)", vault.FmtLocalTime(q.D7Reset))
		}
		if q.H5Rem != nil && *q.H5Rem <= 0.5 {
			return false, fmt.Sprintf("5h 已满 (%s)", vault.FmtLocalTime(q.H5Reset))
		}
	}
	return true, "ok"
}

// AboveReserve reports whether an account has remaining Gemini 5h & 7d quota strictly above reservePct.
// Unknown (uncached) quotas are optimistically treated as above reserve.
func AboveReserve(q *vault.Quota, reservePct float64) bool {
	if reservePct <= 0.5 || q == nil {
		return true
	}
	if q.D7Rem != nil && *q.D7Rem <= reservePct {
		return false
	}
	if q.H5Rem != nil && *q.H5Rem <= reservePct {
		return false
	}
	return true
}

func MarkCooldown(st *vault.State, email string, seconds int, reason string, now float64) {
	if seconds < 60 {
		seconds = 60
	}
	if st.Cooldowns == nil {
		st.Cooldowns = map[string]*vault.Cooldown{}
	}
	st.Cooldowns[email] = &vault.Cooldown{
		Until:  now + float64(seconds),
		Reason: reason,
		At:     now,
	}
}

// ReconcileCooldownWithQuota clears any stale cooldown for email when a live quota
// probe confirms the account actually has usable quota again (e.g. Google reset early).
func ReconcileCooldownWithQuota(st *vault.State, email string, q *vault.Quota) bool {
	if st == nil || q == nil || q.Error != "" {
		return false
	}
	if st.Cooldowns[email] == nil {
		return false
	}
	if q.H5Rem == nil && q.D7Rem == nil {
		return false
	}
	if q.D7Rem != nil && *q.D7Rem <= 0.5 {
		return false
	}
	if q.H5Rem != nil && *q.H5Rem <= 0.5 {
		return false
	}
	if q.C7Rem != nil && *q.C7Rem <= 0.5 {
		return false
	}
	if q.C5Rem != nil && *q.C5Rem <= 0.5 {
		return false
	}
	delete(st.Cooldowns, email)
	return true
}

// NextStandby returns the next available account after currentEmail in cyclic drain order.
func NextStandby(order []string, currentEmail string, st *vault.State, quotas map[string]*vault.Quota, now float64) string {
	idx := -1
	for i, e := range order {
		if e == currentEmail {
			idx = i
			break
		}
	}
	if idx < 0 || len(order) < 2 {
		return ""
	}
	for step := 1; step < len(order); step++ {
		e := order[(idx+step)%len(order)]
		if ok, _ := AccountAvailability(e, st, quotas, now); ok {
			return e
		}
	}
	return ""
}

// PickDrain selects the account to run under strict drain semantics.
// When reservePct > 0 (e.g. 10), if the current account's cached remaining quota is <= reservePct
// AND another account has quota > reservePct, it proactively advances before starting a new task
// to minimize mid-task quota walls. If all accounts are <= reservePct, it falls back cleanly to
// any available account (> 0.5%).
func PickDrain(
	order []string,
	st *vault.State,
	quotas map[string]*vault.Quota,
	forceNext bool,
	now float64,
) (string, string) {
	return PickDrainWithReserve(order, st, quotas, forceNext, 0, now)
}

func PickDrainWithReserve(
	order []string,
	st *vault.State,
	quotas map[string]*vault.Quota,
	forceNext bool,
	reservePct float64,
	now float64,
) (string, string) {
	PruneExpiredCooldowns(st, now)
	if len(order) == 0 {
		return "", "号池为空，请先运行 `agy-pro login` 登录账号，或 `agy-pro import <目录>` 导入。"
	}

	active := st.Active
	idx := -1
	for i, e := range order {
		if e == active {
			idx = i
			break
		}
	}

	if idx < 0 {
		active = order[0]
		idx = 0
		for i, e := range order {
			q := quotas[e]
			if q != nil && q.D7Used != nil && *q.D7Used > 0 && *q.D7Used < 99.5 {
				if ok, _ := AccountAvailability(e, st, quotas, now); ok && AboveReserve(q, reservePct) {
					active = e
					idx = i
					break
				}
			}
		}
	}

	if !forceNext {
		if ok, _ := AccountAvailability(active, st, quotas, now); ok {
			if AboveReserve(quotas[active], reservePct) {
				st.Active = active
				st.Order = append([]string(nil), order...)
				return active, "drain-current"
			}
			// Proactive low-quota switch: look for a fresher account strictly above reservePct
			for step := 1; step < len(order); step++ {
				e := order[(idx+step)%len(order)]
				if ok2, _ := AccountAvailability(e, st, quotas, now); ok2 && AboveReserve(quotas[e], reservePct) {
					st.Active = e
					st.Order = append([]string(nil), order...)
					return e, "reserve-advanced"
				}
			}
			// No account above reservePct -> keep draining current available account
			st.Active = active
			st.Order = append([]string(nil), order...)
			return active, "drain-current"
		}
	}

	// Prefer accounts above reservePct first when advancing
	if reservePct > 0.5 {
		for step := 1; step < len(order); step++ {
			e := order[(idx+step)%len(order)]
			if ok, _ := AccountAvailability(e, st, quotas, now); ok && AboveReserve(quotas[e], reservePct) {
				st.Active = e
				st.Order = append([]string(nil), order...)
				return e, "drain-advanced"
			}
		}
	}

	// Fallback: any available account (> 0.5%)
	for step := 1; step < len(order); step++ {
		e := order[(idx+step)%len(order)]
		if ok, _ := AccountAvailability(e, st, quotas, now); ok {
			st.Active = e
			st.Order = append([]string(nil), order...)
			return e, "drain-advanced"
		}
	}

	if forceNext {
		if ok, _ := AccountAvailability(active, st, quotas, now); ok {
			return active, "only-available"
		}
	}

	soonest := 0.0
	for _, e := range order {
		wait := 0.0
		if cd := st.Cooldowns[e]; cd != nil && cd.Until > now && cd.Until > wait {
			wait = cd.Until
		}
		if q := quotas[e]; q != nil {
			if q.H5Rem != nil && *q.H5Rem <= 0.5 && q.H5Reset > now && q.H5Reset > wait {
				wait = q.H5Reset
			}
			if q.D7Rem != nil && *q.D7Rem <= 0.5 && q.D7Reset > now && q.D7Reset > wait {
				wait = q.D7Reset
			}
		}
		if wait > 0 && (soonest == 0 || wait < soonest) {
			soonest = wait
		}
	}

	msg := fmt.Sprintf("全部 %d 个账号均已耗尽或处于冷却中", len(order))
	if soonest > now {
		msg += fmt.Sprintf("（最早恢复: %s，约 %s 后）", vault.FmtLocalTime(soonest), vault.FmtDuration(soonest-now))
	}
	return "", msg
}
