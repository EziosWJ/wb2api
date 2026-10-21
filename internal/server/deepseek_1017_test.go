package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestDeepseek1017ModelListPreservesImagesAndMaxAllowedSize(t *testing.T) {
	h, _ := mixedDeepseekFixture(t)
	h.cfg.PinnedModels = upstream.ResolvePinnedModels(nil)
	byID := modelsByID(h.modelList())
	ds, ok := byID[pool.DeepseekFreeModel]
	if !ok {
		t.Fatalf("missing %s in /v1/models", pool.DeepseekFreeModel)
	}
	if ds["supports_images"] != true {
		t.Fatalf("deepseek-v4.1-flash (1017) supports_images=%v want true", ds["supports_images"])
	}
	if ds["max_allowed_size"] != int64(1000000) {
		t.Fatalf("deepseek-v4.1-flash (1017) max_allowed_size=%v want 1000000", ds["max_allowed_size"])
	}
	if ds["credits"] != "x0.00" {
		t.Fatalf("deepseek-v4.1-flash credits=%v want x0.00", ds["credits"])
	}
}

func TestDeepseek1017AliasRoutesAndFallsBackAcrossPhases(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	if r := fallbackRequest(h, "deepseek-v4.1-flash-1017", false); r.Code != 200 || (*calls)[0] != (fallbackCall{pool.DeepseekFreeModel, "Bearer global"}) {
		t.Fatalf("1017 alias should rewrite to wire model %s on global free: code=%d calls=%v", pool.DeepseekFreeModel, r.Code, *calls)
	}
	exhaustFree(h.cfg.Pool, "global")
	if r := fallbackRequest(h, "deepseek-v4.1-flash-1017", false); r.Code != 200 || (*calls)[1] != (fallbackCall{pool.DeepseekPaidModel, "Bearer global"}) {
		t.Fatalf("1017 alias should fall back to SG: code=%d calls=%v", r.Code, *calls)
	}
	h.cfg.Pool.CooldownSoftForModel("global", time.Hour, time.Now().Add(time.Hour), pool.DeepseekPaidModel, "6004 model rate limit")
	if r := fallbackRequest(h, "deepseek-v4.1-flash-1017", false); r.Code != 200 || (*calls)[2] != (fallbackCall{pool.DeepseekFreeModel, "Bearer cn"}) {
		t.Fatalf("1017 alias should fall back to CN: code=%d calls=%v", r.Code, *calls)
	}
	if r := fallbackRequest(h, "cn:deepseek-v4.1-flash-1017", false); r.Code != 200 || (*calls)[3] != (fallbackCall{pool.DeepseekFreeModel, "Bearer cn"}) {
		t.Fatalf("explicit cn:1017 alias failed: code=%d calls=%v", r.Code, *calls)
	}
}

func TestQueueFull6021FailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 429, `{"code":6021,"msg":"queue.full.title","requestId":"b1bc87ea3a771e835e388328713d47cf"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "busy1", AccessToken: "at-busy1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "busy2", AccessToken: "at-busy2", ExpiresAt: 9999999999},
	)
	p.SetCredits("busy1", 2000, 0)
	p.SetCredits("busy2", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "upstream_busy") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls["Bearer at-busy1"] != 1 || calls["Bearer at-busy2"] != 0 {
		t.Fatalf("6021 queue.full should not rotate: calls=%v", calls)
	}
	st, _ := p.Status("busy1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Fatalf("6021 queue.full penalized healthy account: %+v", st)
	}
}

func TestModelBusy14003AndPermission11158AreModelScoped(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14003,"msg":"too many requests","displayMsg":{"zh":"模型繁忙，请换模型或稍后重试"}}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("bad")
	if st.Cooling || len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.2" {
		t.Fatalf("14003 should only park model, not whole account: %+v", st)
	}
	if p.PickByUIDForModel("bad", "hy3") == nil || p.PickByUIDForModel("bad", "glm-5.2") != nil {
		t.Fatal("14003 model exemption failed")
	}

	up11158 := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11158,"msg":"model [gpt-5.6-sol] is not allowed for user type [personal_free_user]"}`, false
	})
	p2 := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-g1", ExpiresAt: 9999999999})
	h2 := NewHandler(Config{Pool: p2, Upstream: up11158, GlobalEnabled: true})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:gpt-5.6-sol","messages":[]}`)))
	if rec2.Code != http.StatusBadRequest || !strings.Contains(rec2.Body.String(), "model_unavailable") {
		t.Fatalf("11158 should return 400 model_unavailable: code=%d body=%s", rec2.Code, rec2.Body.String())
	}
}
