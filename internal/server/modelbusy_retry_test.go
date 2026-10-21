package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestModelBusyRetryDelayNeverWakesEarly(t *testing.T) {
	for _, wait := range []time.Duration{-time.Second, 0, modelBusyWaitMax + time.Nanosecond} {
		if got := modelBusyRetryDelay(wait); got != 0 {
			t.Fatalf("invalid wait %s produced %s", wait, got)
		}
	}
	for _, wait := range []time.Duration{time.Millisecond, 100 * time.Millisecond, modelBusyWaitMax - time.Millisecond, modelBusyWaitMax} {
		for i := 0; i < 100; i++ {
			got := modelBusyRetryDelay(wait)
			if got < wait || got > modelBusyWaitMax || got > wait+125*time.Millisecond {
				t.Fatalf("wait=%s retry=%s", wait, got)
			}
		}
	}
}

func TestModelBusyShortWindowRetriesOneAccount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			h, calls := mixedDeepseekFixture(t)
			until := time.Now().Add(30 * time.Millisecond)
			h.cfg.Pool.CooldownModelBusyUntil("global", pool.DeepseekFreeModel, until, "14003 model busy")
			r := fallbackRequest(h, pool.DeepseekFreeModel, stream)
			if r.Code != http.StatusOK || len(*calls) != 1 || (*calls)[0] != (fallbackCall{pool.DeepseekFreeModel, "Bearer global"}) {
				t.Fatalf("busy retry should stay in free phase: code=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
			}
			if time.Now().Before(until) {
				t.Fatal("busy retry woke before upstream deadline")
			}
			st, _ := h.cfg.Pool.Status("global")
			if st.InFlight != 0 || st.Cooling || st.ConsecutiveFails != 0 {
				t.Fatalf("retry leaked lease or punished account: %+v", st)
			}
		})
	}
}

func TestModelBusyWaitIsCancellable(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	h.cfg.Pool.CooldownModelBusyUntil("global", pool.DeepseekFreeModel, time.Now().Add(time.Second), "14003 model busy")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(30*time.Millisecond, cancel)
	defer timer.Stop()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond || len(*calls) != 0 {
		t.Fatalf("cancel did not stop busy wait: elapsed=%s calls=%v", elapsed, *calls)
	}
	st, _ := h.cfg.Pool.Status("global")
	if st.InFlight != 0 || st.Cooling || st.ConsecutiveFails != 0 {
		t.Fatalf("cancel leaked lease or punished account: %+v", st)
	}
}

func TestModelBusyWaitRetriesAtMostOnce(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	const raw = `{"code":14003,"msg":"too many requests"}`
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls = append(*calls, fallbackCall{pool.DeepseekFreeModel, r.Header.Get("Authorization")})
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After-Ms": []string{"20"}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
	})
	start := time.Now()
	r := fallbackRequest(h, pool.DeepseekFreeModel, false)
	if r.Code != 503 || len(*calls) != 2 || time.Since(start) > time.Second {
		t.Fatalf("busy wait retried again: code=%d calls=%v elapsed=%s body=%s", r.Code, *calls, time.Since(start), r.Body.String())
	}
	var e hintEnvelope
	json.Unmarshal(r.Body.Bytes(), &e)
	if e.Error.Code != "model_busy" || !strings.Contains(e.Error.Message, raw) || r.Header().Get("Retry-After") == "" {
		t.Fatalf("busy terminal lost error/delay: %s", r.Body.String())
	}
}

func TestModelBusyHTTPShortRetrySucceedsWithoutPaidFallback(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	base := h.cfg.Upstream.HTTP.Transport
	first := true
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if first {
			first = false
			*calls = append(*calls, fallbackCall{pool.DeepseekFreeModel, r.Header.Get("Authorization")})
			return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After-Ms": []string{"20"}}, Body: io.NopCloser(strings.NewReader(`{"code":14003,"msg":"inference overloaded"}`))}, nil
		}
		return base.RoundTrip(r)
	})
	r := fallbackRequest(h, pool.DeepseekFreeModel, false)
	if r.Code != 200 || len(*calls) != 2 {
		t.Fatalf("short HTTP busy did not retry: status=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
	}
	for _, call := range *calls {
		if call != (fallbackCall{pool.DeepseekFreeModel, "Bearer global"}) {
			t.Fatalf("temporary busy caused paid escalation: %v", *calls)
		}
	}
}

func TestModelBusyDeadlineNeverEscalatesToPaid(t *testing.T) {
	for _, phase := range []string{"free", "sg", "cn"} {
		for _, source := range []string{"body", "header"} {
			t.Run(phase+"/"+source, func(t *testing.T) {
				h, calls := mixedDeepseekFixture(t)
				uid, model := "global", pool.DeepseekFreeModel
				if phase != "free" {
					exhaustFree(h.cfg.Pool, "global")
					model = pool.DeepseekPaidModel
				}
				if phase == "cn" {
					h.cfg.Pool.CooldownSoftForModel("global", time.Minute, time.Now().Add(time.Hour), pool.DeepseekPaidModel, "6004 model rate limit")
					uid, model = "cn", pool.DeepseekFreeModel
				}
				reset := time.Now().Add(30 * time.Minute).In(upstream.SoftRateResetLoc()).Truncate(time.Second)
				raw := `{"code":14003,"msg":"too many requests"}`
				if source == "body" {
					raw = `{"code":14003,"msg":"too many requests, will reset at ` + reset.Format("2006-01-02 15:04:05") + ` UTC+8"}`
				}
				h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					var b map[string]any
					json.NewDecoder(r.Body).Decode(&b)
					*calls = append(*calls, fallbackCall{b["model"].(string), r.Header.Get("Authorization")})
					return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"1800"}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
				})
				r := fallbackRequest(h, pool.DeepseekFreeModel, false)
				if r.Code != 503 || len(*calls) != 1 || (*calls)[0] != (fallbackCall{model, "Bearer " + uid}) || !strings.Contains(r.Body.String(), `"code":"model_busy"`) {
					t.Fatalf("busy changed route/classification: code=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
				}
				delay, err := strconv.Atoi(r.Header().Get("Retry-After"))
				if err != nil || delay < 1795 || delay > 1800 {
					t.Fatalf("retry delay=%q", r.Header().Get("Retry-After"))
				}
				st, _ := h.cfg.Pool.Status(uid)
				found := false
				for _, row := range st.RateLimitedModels {
					if row.Model == model {
						found = strings.HasPrefix(row.Reason, "14003") && !row.ResetAt.IsZero()
						if source == "body" && !row.ResetAt.Equal(reset) {
							t.Fatalf("message deadline should win over header: %+v", row)
						}
					}
				}
				if !found || st.Cooling || st.ConsecutiveFails != 0 {
					t.Fatalf("busy must preserve model deadline and account health: %+v", st)
				}
			})
		}
	}
}

func TestModelBusyMixedLocksAreNotQuotaOrMissingModel(t *testing.T) {
	for _, other := range []string{"6004", "11158", "health", "inflight", "daily", "reserve"} {
		t.Run(other, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999}, &auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999})
			p.CooldownModelBusyUntil("a", "glm-5.2", time.Now().Add(time.Minute), "14003 model busy")
			p.CooldownModelBusyUntil("b", "glm-5.2", time.Now().Add(time.Minute), "14003 model busy")
			switch other {
			case "6004":
				p.CooldownSoftForModel("b", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
			case "11158":
				p.BlockModelBackoff("b", "glm-5.2", "11158 model forbidden")
			case "health":
				p.Cooldown("b", pool.CoolHard, time.Hour, "credits exhausted")
			case "inflight":
				p.SetMaxInFlight(1)
				p.Acquire("b")
				defer p.Release("b")
			case "daily":
				p.SetDailyTokenLimit("b", "glm-5.2", 1)
				p.RecordTokenUsage("b", pool.TokenUsageDelta{Model: "glm-5.2", HasTotalTokens: true, TotalTokens: 1})
			case "reserve":
				p.SetReserveCredits(50)
				p.SetCredits("b", 50, 1000)
			}
			h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, nil)})
			r := fallbackRequest(h, "glm-5.2", false)
			if r.Code != 503 || strings.Contains(r.Body.String(), `"code":"model_rate_limited"`) || strings.Contains(r.Body.String(), `"code":"model_unavailable"`) {
				t.Fatalf("mixed lock was converted to quota/missing model: code=%d body=%s", r.Code, r.Body.String())
			}
		})
	}
}

func TestModelBusyWaitUsesRecoveredCheaperDeepseekPhase(t *testing.T) {
	for _, recovery := range []string{"sg_to_free", "cn_to_sg", "cn_to_free"} {
		t.Run(recovery, func(t *testing.T) {
			h, calls := mixedDeepseekFixture(t)
			soon := time.Now().Add(150 * time.Millisecond)
			later := soon.Add(150 * time.Millisecond)
			p := h.cfg.Pool
			if recovery == "sg_to_free" {
				p.CooldownSoftForModel("global", time.Minute, soon, pool.DeepseekFreeModel, "6004 model rate limit")
				p.CooldownModelBusyUntil("global", pool.DeepseekPaidModel, later, "14003 model busy")
			} else {
				freeUntil := time.Now().Add(time.Hour)
				if recovery == "cn_to_free" {
					freeUntil = soon
				}
				p.CooldownSoftForModel("global", time.Minute, freeUntil, pool.DeepseekFreeModel, "6004 model rate limit")
				p.CooldownSoftForModel("global", time.Minute, soon, pool.DeepseekPaidModel, "6004 model rate limit")
				p.CooldownModelBusyUntil("cn", pool.DeepseekFreeModel, later, "14003 model busy")
			}
			r := fallbackRequest(h, pool.DeepseekFreeModel, false)
			want := pool.DeepseekFreeModel
			if recovery == "cn_to_sg" {
				want = pool.DeepseekPaidModel
			}
			if r.Code != 200 || len(*calls) != 1 || (*calls)[0] != (fallbackCall{want, "Bearer global"}) || r.Header().Get("X-WB2API-Actual-Model") != "global:"+want {
				t.Fatalf("recovered cheaper phase was skipped: code=%d calls=%v actual=%s body=%s", r.Code, *calls, r.Header().Get("X-WB2API-Actual-Model"), r.Body.String())
			}
		})
	}
}

func TestModelBusyAfterOutputDoesNotReplay(t *testing.T) {
	h, calls := mixedDeepseekFixture(t)
	const frame = "data: {\"id\":\"partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"}}]}\n\n" +
		"data: {\"error\":{\"code\":14003,\"message\":\"model busy\"}}\n\ndata: [DONE]\n\n"
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls = append(*calls, fallbackCall{pool.DeepseekFreeModel, r.Header.Get("Authorization")})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frame))}, nil
	})
	r := fallbackRequest(h, pool.DeepseekFreeModel, true)
	if r.Code != 200 || len(*calls) != 1 || !strings.Contains(r.Body.String(), "partial answer") || !strings.Contains(r.Body.String(), "14003") {
		t.Fatalf("partial busy response replayed or changed: code=%d calls=%v body=%s", r.Code, *calls, r.Body.String())
	}
}
