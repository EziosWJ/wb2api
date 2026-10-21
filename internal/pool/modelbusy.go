package pool

import "time"

// ModelBusyExhausted requires every eligible account to be blocked only by an
// active 14003 window. A mixed quota/permission/health lock is not pure busy.
// It is read-only and never selects an account or extends a cooldown.
func (p *Pool) ModelBusyExhausted(model, realm string, paid bool) (time.Duration, bool) {
	if model == "" {
		return 0, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	gate := p.reserveGateFor(model, false)
	if paid {
		gate = reserveGate{p: p, active: true, line: p.reserveCredits, requireKnown: true}
	}
	count := 0
	var earliest time.Time
	for _, e := range p.byUID {
		if e.disabled || e.manualDisabled || (realm != "" && e.a.Realm() != realm) {
			continue
		}
		count++
		mc := e.modelCooldowns[model]
		if !e.healthy(now) || p.inFlightFull(e) || e.dailyTokenBlocked(now, model) ||
			!gate.allows(e, model) || !isModelBusyReason(mc.Reason) || !mc.Until.After(now) {
			return 0, false
		}
		if earliest.IsZero() || mc.Until.Before(earliest) {
			earliest = mc.Until
		}
	}
	if count == 0 || earliest.IsZero() {
		return 0, false
	}
	return earliest.Sub(now), true
}
