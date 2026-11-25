package router

import (
	"fmt"

	"agy-pool/internal/vault"
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
		if q.H5Rem != nil && *q.H5Rem <= 0.5 {
			return false, fmt.Sprintf("5h 已满 (%s)", vault.FmtLocalTime(q.H5Reset))
		}
		if q.D7Rem != nil && *q.D7Rem <= 0.5 {
			return false, fmt.Sprintf("7d 已满 (%s)", vault.FmtLocalTime(q.D7Reset))
		}
	}
	return true, "ok"
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

// PickDrain selects the account to run under strict drain semantics:
// stays on active until exhausted/cooldown, then advances cyclically and stays there.
func PickDrain(
	order []string,
	st *vault.State,
	quotas map[string]*vault.Quota,
	forceNext bool,
	now float64,
) (string, string) {
	PruneExpiredCooldowns(st, now)
	if len(order) == 0 {
		return "", "号池为空，请先运行 `agy-pool login` 登录账号，或 `agy-pool import <目录>` 导入。"
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
		// First-time bootstrap: prefer partially-used account to finish draining it first
		active = order[0]
		idx = 0
		for i, e := range order {
			q := quotas[e]
			if q != nil && q.D7Used != nil && *q.D7Used > 0 && *q.D7Used < 99.5 {
				if ok, _ := AccountAvailability(e, st, quotas, now); ok {
					active = e
					idx = i
					break
				}
			}
		}
	}

	if !forceNext {
		if ok, _ := AccountAvailability(active, st, quotas, now); ok {
			st.Active = active
			st.Order = append([]string(nil), order...)
			return active, "drain-current"
		}
	}

	// Scan forward cyclically
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
