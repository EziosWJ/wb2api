package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func busyTestPool(t *testing.T, ids ...string) *Pool {
	t.Helper()
	previous := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(previous) })
	p := New("")
	p.SetReserveCredits(50)
	for _, uid := range ids {
		p.Add(&auth.Auth{UID: uid, Domain: "www.workbuddy.ai"})
		p.SetCredits(uid, 100, 1000)
	}
	return p
}

func busyWindow(t *testing.T, p *Pool, uid, model string) modelCooldown {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	if e == nil {
		t.Fatal("missing busy test account")
	}
	mc, present := e.modelCooldowns[model]
	if !present {
		t.Fatal("missing model busy window")
	}
	return mc
}

func TestModelBusyDefaultWindowAndCap(t *testing.T) {
	p := busyTestPool(t, "account")
	started := time.Now()
	p.CooldownModelBusy("account", "model", "14003 model busy")
	finished := time.Now()
	first := busyWindow(t, p, "account", "model")
	if first.Until.Before(started.Add(10*time.Second)) || first.Until.After(finished.Add(10*time.Second)) || first.Hits != 1 {
		t.Fatalf("first window=%+v must be 10 seconds", first)
	}
	if !first.ResetAt.IsZero() || first.Reason != "14003 model busy" {
		t.Fatalf("default window incorrectly reported upstream reset: %+v", first)
	}
	p.CooldownModelBusy("account", "model", "14003 model busy")
	if second := busyWindow(t, p, "account", "model"); second != first {
		t.Fatalf("duplicate busy response extended window: before=%+v after=%+v", first, second)
	}
	for _, seconds := range []int{20, 40, 60, 60, 60} {
		// Keep the expired record to check the bound independently of pick pruning.
		p.mu.Lock()
		mc := p.byUID["account"].modelCooldowns["model"]
		mc.Until = time.Now().Add(-time.Millisecond)
		p.byUID["account"].modelCooldowns["model"] = mc
		p.mu.Unlock()
		started = time.Now()
		p.CooldownModelBusy("account", "model", "14003 model busy")
		finished = time.Now()
		mc = busyWindow(t, p, "account", "model")
		want := time.Duration(seconds) * time.Second
		if mc.Until.Before(started.Add(want)) || mc.Until.After(finished.Add(want)) {
			t.Fatalf("busy escalation duration must be %s, window=%+v", want, mc)
		}
	}
	st, _ := p.Status("account")
	if st.Cooling || !st.Until.IsZero() || st.SoftStreak != 0 {
		t.Fatalf("model busy polluted account-wide cooldown: %+v", st)
	}
	if p.PickByUIDForModel("account", "other") == nil || p.PickByUIDForModel("account", "model") != nil {
		t.Fatal("model busy did not preserve other model eligibility")
	}
}

func TestModelBusyKeepsQuotaAndPermissionWindows(t *testing.T) {
	for _, reason := range []string{"6004 model rate limit", "11158 model not allowed for account type"} {
		t.Run(reason, func(t *testing.T) {
			p := busyTestPool(t, "account")
			if reason[0] == '6' {
				p.CooldownSoftForModel("account", time.Minute, time.Now().Add(time.Hour), "model", reason)
			} else {
				p.BlockModelBackoff("account", "model", reason)
			}
			before := busyWindow(t, p, "account", "model")
			p.CooldownModelBusy("account", "model", "14003 model busy")
			p.CooldownModelBusyUntil("account", "model", time.Now().Add(50*time.Millisecond), "14003 model busy")
			p.CooldownModelBusyUntil("account", "model", time.Now().Add(24*time.Hour), "14003 model busy")
			if after := busyWindow(t, p, "account", "model"); before != after {
				t.Fatalf("busy response replaced quota/permission lock: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestModelBusyExplicitResetPreservesBusyClassification(t *testing.T) {
	p := busyTestPool(t, "global")
	reset := time.Now().Add(time.Second)
	p.CooldownModelBusyUntil("global", DeepseekFreeModel, reset, "14003 model busy")
	mc := busyWindow(t, p, "global", DeepseekFreeModel)
	if !mc.ResetAt.Equal(reset) || !mc.Until.Equal(reset) || mc.Reason != "14003 model busy" {
		t.Fatalf("explicit reset lost busy origin: %+v", mc)
	}
	if p.DeepseekFreeExhausted() || p.DeepseekCNFallbackReady() || p.PickDeepseekPaidFallback(nil) != nil {
		t.Fatal("temporary busy with an explicit deadline triggered paid fallback")
	}
	p.ClearModelCooldown("global", DeepseekFreeModel)
	p.CooldownSoftForModel("global", time.Minute, time.Now().Add(time.Hour), DeepseekFreeModel, "6004 model rate limit")
	p.CooldownModelBusyUntil("global", DeepseekPaidModel, reset, "14003 model busy")
	if !p.DeepseekFreeExhausted() || p.DeepseekCNFallbackReady() || p.PickDeepseekCNFallback(nil) != nil {
		t.Fatal("temporary SG busy triggered paid CN fallback")
	}
}

func TestModelBusyDuplicateCappedResetDoesNotExtendWindow(t *testing.T) {
	p := busyTestPool(t, "account")
	cap := 2 * time.Minute
	p.SetSoftRateMax(cap)
	reset := time.Now().Add(time.Hour)
	started := time.Now()
	p.CooldownModelBusyUntil("account", "model", reset, "14003 model busy")
	finished := time.Now()
	first := busyWindow(t, p, "account", "model")
	if !first.ResetAt.Equal(reset) || first.Until.Before(started.Add(cap)) || first.Until.After(finished.Add(cap)) {
		t.Fatalf("explicit busy reset must retain its origin and use soft-rate cap: %+v", first)
	}
	// Model elapsed time without sleeping: the active capped window now ends sooner,
	// while the same upstream reset remains later than that effective deadline.
	p.mu.Lock()
	first.Until = first.Until.Add(-time.Second)
	p.byUID["account"].modelCooldowns["model"] = first
	p.mu.Unlock()
	for i := 0; i < 3; i++ {
		p.CooldownModelBusyUntil("account", "model", reset, "14003 model busy")
		if after := busyWindow(t, p, "account", "model"); after != first {
			t.Fatalf("duplicate capped deadline extended window: before=%+v after=%+v", first, after)
		}
	}
}

func TestModelBusyNeverReportsQuotaExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		quotaUIDs []string
		wantQuota bool
		wantBusy  bool
	}{
		{name: "all busy", wantBusy: true},
		{name: "quota plus busy", quotaUIDs: []string{"b"}, wantBusy: true},
		{name: "all quota", quotaUIDs: []string{"a", "b"}, wantQuota: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := busyTestPool(t, "a", "b")
			for _, uid := range []string{"a", "b"} {
				p.CooldownModelBusyUntil(uid, "model", time.Now().Add(time.Minute), "14003 model busy")
			}
			for _, uid := range tc.quotaUIDs {
				p.CooldownSoftForModel(uid, time.Minute, time.Now().Add(time.Hour), "model", "6004 model rate limit")
			}
			for i := 0; i < 20; i++ {
				d, exhausted := p.ModelRateLimitExhausted("model", "global")
				if exhausted != tc.wantQuota || (!tc.wantQuota && d != 0) || (tc.wantQuota && d <= 0) {
					t.Fatalf("incorrect quota classification: wait=%s exhausted=%v wantQuota=%v", d, exhausted, tc.wantQuota)
				}
				blocked := p.ModelBlocked("model", "global")
				if !blocked.Blocked || blocked.Count != 2 || blocked.HasBusy != tc.wantBusy {
					t.Fatalf("incorrect mixed-pool blocked classification: %+v wantBusy=%v", blocked, tc.wantBusy)
				}
			}
		})
	}
}

func TestModelBusyExhaustedRequiresOnlyBusyInEveryAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*Pool)
	}{
		{"quota lock", func(p *Pool) {
			p.CooldownSoftForModel("b", time.Minute, time.Now().Add(time.Hour), "model", "6004 model rate limit")
		}},
		{"permission lock", func(p *Pool) { p.BlockModelBackoff("b", "model", "11158 model not allowed for account type") }},
		{"account fault", func(p *Pool) { p.Cooldown("b", CoolSoft, time.Hour, "account fault") }},
		{"in-flight full", func(p *Pool) { p.SetMaxInFlightGlobal(1); p.Acquire("b") }},
		{"daily quota", func(p *Pool) {
			p.SetDailyTokenLimit("b", "model", 1)
			p.RecordTokenUsage("b", TokenUsageDelta{Model: "model", HasTotalTokens: true, TotalTokens: 1})
		}},
		{"reserve floor", func(p *Pool) { p.SetCredits("b", 50, 1000) }},
		{"healthy model", func(p *Pool) { p.ClearModelCooldown("b", "model") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := busyTestPool(t, "a", "b")
			for _, uid := range []string{"a", "b"} {
				p.CooldownModelBusyUntil(uid, "model", time.Now().Add(time.Second), "14003 model busy")
			}
			if d, onlyBusy := p.ModelBusyExhausted("model", "global", false); !onlyBusy || d <= 0 || d > time.Second {
				t.Fatalf("all busy pool not recognized: wait=%s busy=%v", d, onlyBusy)
			}
			tc.alter(p)
			for i := 0; i < 20; i++ {
				if d, onlyBusy := p.ModelBusyExhausted("model", "global", false); onlyBusy || d != 0 {
					t.Fatalf("mixed state falsely reported as only busy: wait=%s busy=%v", d, onlyBusy)
				}
			}
		})
	}
}

func TestModelBusyExhaustedUsesScopedPaidReserveGate(t *testing.T) {
	p := busyTestPool(t, "global")
	p.SetCredits("global", 50, 1000)
	p.CooldownModelBusyUntil("global", DeepseekFreeModel, time.Now().Add(time.Second), "14003 model busy")
	if _, ok := p.ModelBusyExhausted(DeepseekFreeModel, "global", false); !ok {
		t.Fatal("international free model lost reserve exemption")
	}
	if d, ok := p.ModelBusyExhausted(DeepseekFreeModel, "global", true); ok || d != 0 {
		t.Fatal("paid CN interpretation inherited free-model reserve exemption")
	}
	p.SetCredits("global", 100, 0)
	if d, ok := p.ModelBusyExhausted(DeepseekFreeModel, "global", true); ok || d != 0 {
		t.Fatal("unknown paid balance reported as temporary busy")
	}
	p.SetCredits("global", 100, 1000)
	p.Add(&auth.Auth{UID: "cn", Domain: "www.codebuddy.cn"})
	p.SetCredits("cn", 100, 1000)
	if d, ok := p.ModelBusyExhausted(DeepseekFreeModel, "", false); ok || d != 0 {
		t.Fatal("healthy CN candidate masked by busy global group")
	}
	if _, ok := p.ModelBusyExhausted(DeepseekFreeModel, "global", false); !ok {
		t.Fatal("scoped global busy group included unrelated CN account")
	}
	p.Disable("global", "test")
	if d, ok := p.ModelBusyExhausted(DeepseekFreeModel, "global", false); ok || d != 0 {
		t.Fatal("empty eligible pool reported as busy")
	}
}

func TestQuotaWithoutResetRemainsModelScopedAndIdempotent(t *testing.T) {
	p := busyTestPool(t, "global")
	p.CooldownSoftForModel("global", time.Minute, time.Time{}, DeepseekFreeModel, "6004 model rate limit")
	first := busyWindow(t, p, "global", DeepseekFreeModel)
	p.CooldownSoftForModel("global", time.Minute, time.Time{}, DeepseekFreeModel, "6004 model rate limit")
	if second := busyWindow(t, p, "global", DeepseekFreeModel); second != first {
		t.Fatalf("duplicate quota without reset extended window: before=%+v after=%+v", first, second)
	}
	st, _ := p.Status("global")
	if st.Cooling || !st.Until.IsZero() || st.SoftStreak != 0 {
		t.Fatalf("quota without reset cooled whole account: %+v", st)
	}
	if !p.DeepseekFreeExhausted() {
		t.Fatal("model-only quota without reset lost exhaustion classification")
	}
	if p.PickByUIDForModel("global", "other") == nil || p.PickByUIDForModel("global", DeepseekFreeModel) != nil {
		t.Fatal("quota without reset did not preserve other models")
	}
}
