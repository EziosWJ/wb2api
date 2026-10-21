package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func channelPolicyPool(t *testing.T, ids ...string) (*pool.Pool, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	p := pool.New(path)
	p.SetPickMode(pool.PickSequential)
	for _, id := range ids {
		p.Add(&auth.Auth{UID: id, AccessToken: "at-" + id, ExpiresAt: 9999999999})
		p.SetCredits(id, 100, 1000)
	}
	p.SetOrder(ids)
	return p, path
}

func channelFaultCount(t *testing.T, p *pool.Pool, path, uid string) int {
	t.Helper()
	p.Flush()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Accounts map[string]struct {
			Faults int `json:"account_fault_fails"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	entry, ok := file.Accounts[uid]
	if !ok {
		t.Fatalf("missing %s in persisted pool snapshot", uid)
	}
	return entry.Faults
}

func assertChannelHealthy(t *testing.T, p *pool.Pool, path, uid string) {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok || st.Disabled || st.Cooling || st.ErrTotal != 0 || st.BreakerFails != 0 || st.ConsecutiveFails != 0 || st.SoftStreak != 0 || len(st.RateLimitedModels) != 0 || st.InFlight != 0 {
		t.Fatalf("channel refusal changed healthy account state: %+v found=%v", st, ok)
	}
	if count := channelFaultCount(t, p, path, uid); count != 0 {
		t.Fatalf("channel refusal increased account fault count to %d", count)
	}
}

func postChannelRequest(h *Handler, stream bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"model": "glm-5.2", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
	return rec
}

func channelFrame(t *testing.T, code any, message, requestID string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": message, "requestId": requestID}})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(raw) + "\n\ndata: [DONE]\n\n"
}

func readChannelError(t *testing.T, raw string) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("invalid error JSON: %v: %s", err, raw)
	}
	entry, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("response did not include error: %s", raw)
	}
	return entry
}

func readChannelStreamError(t *testing.T, raw string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
			continue
		}
		if entry, ok := frame["error"].(map[string]any); ok {
			return entry
		}
	}
	t.Fatalf("missing original SSE error frame: %s", raw)
	return nil
}

func TestChannelRefusedHTTPRotatesWithoutPenalties(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"numeric code", `{"code":11128,"msg":"request illegal","requestId":"raw-channel-id"}`},
		{"historical code", `{"code":"11-128","msg":"request illegal","requestId":"raw-channel-id"}`},
		{"historical alternate code", `{"code":"111-28","msg":"conversation id missing","requestId":"raw-channel-id"}`},
		{"unrelated session-looking request ID", `{"code":11128,"msg":"request illegal","requestId":"trace-12153"}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				calls := []string{}
				up := newFakeUpstream(t, func(authz string) (int, string, bool) {
					calls = append(calls, authz)
					if authz == "Bearer at-rejected" {
						return 400, tc.body, false
					}
					return 200, sseOK, true
				})
				p, path := channelPolicyPool(t, "rejected", "good")
				h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
				for i := 0; i < 4; i++ {
					before := len(calls)
					rec := postChannelRequest(h, stream)
					if rec.Code != 200 || len(calls) != before+2 || calls[before] != "Bearer at-rejected" || calls[before+1] != "Bearer at-good" {
						t.Fatalf("channel refusal failed to rotate: status=%d calls=%v body=%s", rec.Code, calls, rec.Body.String())
					}
					assertChannelHealthy(t, p, path, "rejected")
				}
			})
		}
	}
}

func TestChannelRefusedHTTPPreservesOriginalErrorAfterPoolRotation(t *testing.T) {
	for _, code := range []any{11128, "11-128", "111-28"} {
		name := strings.TrimSpace(string(mustChannelJSON(t, code)))
		t.Run(name, func(t *testing.T) {
			original := string(mustChannelJSON(t, map[string]any{"code": code, "msg": "request illegal", "requestId": "trace-12153"}))
			calls := 0
			up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 400, original, false })
			p, path := channelPolicyPool(t, "one", "two")
			h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
			for i := 0; i < 4; i++ {
				rec := postChannelRequest(h, false)
				entry := readChannelError(t, rec.Body.String())
				if rec.Code != 503 || entry["code"] != "channel_refused" || entry["message"] != original {
					t.Fatalf("channel terminal error lost original body: status=%d error=%v", rec.Code, entry)
				}
				assertChannelHealthy(t, p, path, "one")
				assertChannelHealthy(t, p, path, "two")
			}
			if calls != 8 {
				t.Fatalf("rotation suppressed healthy accounts after channel errors: calls=%d want 8", calls)
			}
		})
	}
}

func mustChannelJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestChannelRefusedSSEPreservesCodeWithoutPenalties(t *testing.T) {
	for _, code := range []any{11128, "11-128", "111-28"} {
		for _, nested := range []bool{false, true} {
			t.Run(string(mustChannelJSON(t, code))+map[bool]string{false: "/plain", true: "/nested"}[nested], func(t *testing.T) {
				frameCode, message := code, "request illegal"
				if nested {
					message = string(mustChannelJSON(t, map[string]any{"code": code, "msg": "request illegal", "requestId": "trace-12153"}))
					frameCode = "api_error"
				}
				frame := channelFrame(t, frameCode, message, "trace-12153")
				calls := 0
				up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, frame, true })
				p, path := channelPolicyPool(t, "one")
				h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})
				for i := 0; i < 4; i++ {
					rec := postChannelRequest(h, true)
					entry := readChannelStreamError(t, rec.Body.String())
					if rec.Code != 200 || string(mustChannelJSON(t, entry["code"])) != string(mustChannelJSON(t, frameCode)) || entry["message"] != message || entry["requestId"] != "trace-12153" || !strings.Contains(rec.Body.String(), "data: [DONE]") {
						t.Fatalf("channel SSE error changed: status=%d error=%v", rec.Code, entry)
					}
					if strings.Contains(rec.Body.String(), "upstream stream aborted") {
						t.Fatal("channel rejection was reclassified as an aborted stream")
					}
					assertChannelHealthy(t, p, path, "one")
				}
				if calls != 4 {
					t.Fatalf("SSE channel refusal disabled subsequent requests: calls=%d", calls)
				}
			})
		}
	}
}

func TestChannelRefusedDoesNotEraseOrIncrementAccountFaultThreshold(t *testing.T) {
	p, path := channelPolicyPool(t, "one")
	h := NewHandler(Config{Pool: p, SoftCooldown: time.Minute})
	for i := 0; i < 2; i++ {
		if p.NoteAccountFault("one") {
			t.Fatal("unexpected account-fault disable before threshold")
		}
	}
	if count := channelFaultCount(t, p, path, "one"); count != 2 {
		t.Fatalf("preexisting fault count=%d want 2", count)
	}
	body := `{"code":11128,"msg":"request illegal"}`
	for i := 0; i < 4; i++ {
		h.applyErrorPolicy("one", upstream.Classify(400, body), body, "glm-5.2", nil, nil)
		if count := channelFaultCount(t, p, path, "one"); count != 2 {
			t.Fatalf("channel error changed fault streak: count=%d want 2", count)
		}
	}
	if st, _ := p.Status("one"); st.Disabled || st.Cooling {
		t.Fatalf("channel errors changed healthy state: %+v", st)
	}
	if !p.NoteAccountFault("one") {
		t.Fatal("third real account restriction no longer disables")
	}
}

func TestChannelPolicyRetains11140ThreeStrikeHTTPAndSSE(t *testing.T) {
	for _, mode := range []string{"http", "sse", "sse-inner"} {
		t.Run(mode, func(t *testing.T) {
			const original = `{"code":11140,"msg":"request illegal"}`
			body, status, isStream := original, 400, false
			if mode == "sse" {
				body, status, isStream = channelFrame(t, 11140, "request illegal", "raw-restriction"), 200, true
			}
			if mode == "sse-inner" {
				body, status, isStream = channelFrame(t, "api_error", original, "raw-restriction"), 200, true
			}
			up := newFakeUpstream(t, func(string) (int, string, bool) { return status, body, isStream })
			p, path := channelPolicyPool(t, "one")
			h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1, SoftCooldown: time.Nanosecond})
			for i := 1; i <= pool.AccountFaultThreshold(); i++ {
				rec := postChannelRequest(h, isStream)
				st, _ := p.Status("one")
				wantHTTP := 503
				if isStream {
					wantHTTP = 200
				}
				if rec.Code != wantHTTP {
					t.Fatalf("mode=%s unexpected restriction status=%d want %d", mode, rec.Code, wantHTTP)
				}
				if st.Disabled != (i == pool.AccountFaultThreshold()) {
					t.Fatalf("mode=%s strike=%d disabled=%v status=%d body=%s", mode, i, st.Disabled, rec.Code, rec.Body.String())
				}
				if i < pool.AccountFaultThreshold() && channelFaultCount(t, p, path, "one") != i {
					t.Fatalf("mode=%s did not retain strike %d", mode, i)
				}
				if isStream {
					entry := readChannelStreamError(t, rec.Body.String())
					if entry["message"] != map[bool]string{false: "request illegal", true: original}[mode == "sse-inner"] {
						t.Fatalf("account restriction body changed: %v", entry)
					}
				}
				if !isStream {
					entry := readChannelError(t, rec.Body.String())
					if !strings.Contains(entry["message"].(string), original) {
						t.Fatalf("HTTP restriction lost original body: %v", entry)
					}
				}
			}
		})
	}
}

func TestChannelPolicyLeaves11128SecurityPolicyUnpenalized(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "sse"}[stream], func(t *testing.T) {
			body, status := `{"code":11128,"msg":"blocked by security policy"}`, 400
			if stream {
				body, status = channelFrame(t, 11128, "blocked by security policy", "security-original"), 200
			}
			up := newFakeUpstream(t, func(string) (int, string, bool) { return status, body, stream })
			p, path := channelPolicyPool(t, "one")
			h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
			for i := 0; i < 4; i++ {
				rec := postChannelRequest(h, stream)
				if !stream && (rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"content_blocked"`)) {
					t.Fatalf("security refusal was reclassified as channel refusal: status=%d body=%s", rec.Code, rec.Body.String())
				}
				assertChannelHealthy(t, p, path, "one")
			}
		})
	}
}
