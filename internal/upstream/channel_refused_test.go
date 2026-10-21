package upstream

import (
	"net/http"
	"strings"
	"testing"
)

func TestChannelRefusedClassification(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       ErrKind
	}{
		{"channel numeric code", `{"code":11128,"msg":"request illegal"}`, 400, ErrChannelRefused},
		{"channel string code", `{"code":"11128","msg":"request illegal"}`, 403, ErrChannelRefused},
		{"historical spelling", `{"code":"11-128","msg":"request illegal"}`, 400, ErrChannelRefused},
		{"historical alternate spelling", `{"code":"111-28","msg":"conversation id missing"}`, 400, ErrChannelRefused},
		{"missing conversation ID", `{"code":11128,"msg":"conversation id missing"}`, 400, ErrChannelRefused},
		{"channel hint with generic message", `{"code":11128,"msg":"Request blocked","displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`, 400, ErrChannelRefused},
		{"Chinese channel hint", `{"code":11128,"msg":"Request blocked","displayTips":{"zh":"这次调用没通过渠道校验，请重新发送"}}`, 400, ErrChannelRefused},
		{"nested OpenAI envelope", `{"error":{"code":"11128","message":"request illegal"}}`, 400, ErrChannelRefused},
		{"nested business envelope", `{"error":{"data":{"code":11128,"msg":"request illegal"}}}`, 400, ErrChannelRefused},
		{"code inherited by nested message", `{"code":11128,"data":{"msg":"conversation id missing"}}`, 400, ErrChannelRefused},
		{"content policy unchanged", `{"code":11128,"msg":"blocked by security policy"}`, 400, ErrContentBlocked},
		{"content policy wins over generic display hint", `{"code":11128,"msg":"blocked by security policy","displayTips":{"en":"channel verification failed"}}`, 400, ErrContentBlocked},
		{"account restricted", `{"code":11140,"msg":"request illegal"}`, 400, ErrAccountFault},
		{"nested account restriction", `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, 429, ErrAccountFault},
		{"account rate-limiting variant", `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, 400, ErrSoftRate},
		{"trial activation unchanged", `{"code":14017,"msg":"trial not activated"}`, 429, ErrAccountFault},
		{"request ID is not business code", `{"requestId":"11128","msg":"request illegal"}`, 400, ErrClient},
		{"request ID is not account restriction code", `{"requestId":"11140","msg":"request illegal"}`, 400, ErrClient},
		{"missing code", `{"msg":"request illegal"}`, 400, ErrClient},
		{"plain missing code", `request illegal`, 400, ErrClient},
		{"nearby numeric code", `{"code":111280,"msg":"request illegal"}`, 400, ErrClient},
		{"wrong code with display hint", `{"code":1,"displayTips":{"en":"channel verification failed"}}`, 400, ErrClient},
		{"missing code with display hint", `{"displayTips":{"en":"channel verification failed"}}`, 400, ErrClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d,%s)=%s want %s", tc.status, tc.body, got, tc.want)
			}
			if got := IsChannelRefused(tc.body); got != (tc.want == ErrChannelRefused) {
				t.Fatalf("channel predicate=%v want %v", got, tc.want == ErrChannelRefused)
			}
			if got := IsAccountRestricted(tc.body); got != (tc.want == ErrAccountFault && !strings.Contains(tc.body, "14017")) {
				t.Fatalf("account restriction predicate=%v", got)
			}
		})
	}
	if ErrChannelRefused <= ErrClient {
		t.Fatal("new error kind must be appended after existing kinds")
	}
	if ErrChannelRefused.String() != "channel_refused" {
		t.Fatal("unexpected channel-refused error name")
	}
}

func TestFrameKindPreservesBusinessCode(t *testing.T) {
	cases := []struct {
		name, payload string
		want          ErrKind
	}{
		{"channel code and plain message", `{"error":{"code":11128,"message":"request illegal"}}`, ErrChannelRefused},
		{"channel historical code", `{"error":{"code":"11-128","message":"conversation id missing"}}`, ErrChannelRefused},
		{"account restriction code", `{"error":{"code":11140,"message":"request illegal"}}`, ErrAccountFault},
		{"account limit variant", `{"error":{"code":11140,"message":"The model provider is rate-limiting requests."}}`, ErrSoftRate},
		{"content policy", `{"error":{"code":11128,"message":"blocked by security policy"}}`, ErrContentBlocked},
		{"code-only permission failure", `{"error":{"code":11158}}`, ErrModelBlocked},
		{"nested original code overrides OpenAI wrapper", `{"error":{"code":"no_healthy_account","message":"{\"code\":11128,\"msg\":\"request illegal\"}"}}`, ErrChannelRefused},
		{"nested original account restriction", `{"error":{"code":"no_healthy_account","message":"{\"code\":11140,\"msg\":\"request illegal\"}"}}`, ErrAccountFault},
		{"missing code cannot imply account restriction", `{"error":{"message":"request illegal","requestId":"11140"}}`, ErrClient},
		{"request ID is not an expired session", `{"error":{"code":"unknown","message":"request illegal","requestId":"trace-12153"}}`, ErrClient},
		{"channel hint survives frame projection", `{"error":{"code":11128,"message":"Request blocked","displayTips":{"en":"channel verification failed"}}}`, ErrChannelRefused},
		{"missing message with no business error", `{"choices":[]}`, ErrNone},
		{"message alias", `{"error":{"code":11128,"msg":"request illegal"}}`, ErrChannelRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FrameKind(tc.payload); got != tc.want {
				t.Fatalf("FrameKind(%s)=%s want %s", tc.payload, got, tc.want)
			}
		})
	}
}

func TestChannelRefusedHintPreservesOriginalBody(t *testing.T) {
	const payload = `{"error":{"code":"111-28","message":"request illegal","requestId":"upstream-original"}}`
	hint := GatewayHint(FrameKind(payload), payload, HintContext{})
	if hint == "" || !strings.Contains(hint, "request channel") {
		t.Fatalf("channel diagnostic missing: %q", hint)
	}
	if IsChannelRefused(`{"code":11128,"msg":"blocked by security policy"}`) {
		t.Fatal("security policy refusal is not a channel refusal")
	}
	// The caller still receives the original body; classification only reads it.
	if Classify(http.StatusBadRequest, payload) != ErrChannelRefused || !strings.Contains(payload, `"code":"111-28"`) {
		t.Fatal("historical spelling was not classified verbatim")
	}
}
