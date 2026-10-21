package upstream

import (
	"net/http"
	"testing"
)

func TestModelErrorCodesAreExactAndIndependent(t *testing.T) {
	for _, tc := range []struct {
		body        string
		quota, busy bool
	}{
		{`{"code":6004,"msg":"limited"}`, true, false},
		{`{"code":"6004","msg":"limited"}`, true, false},
		{`{"error":{"data":{"code":14003,"msg":"busy"}}}`, false, true},
		{`{"extError":{"code":"14003"}}`, false, true},
		{`{"error":{"errCode":14003}}`, false, true},
		{`{"error_code":"6004"}`, true, false},
		{`{"error":{"code":"upstream_error","message":"{\"code\":14003,\"msg\":\"busy\"}"}}`, false, true},
		{`{"code":140030}`, false, false},
		{`{"code":"14003-suffix"}`, false, false},
		{`{"code":60040}`, false, false},
		{`{"code":"6004-suffix"}`, false, false},
		{`{"code":11140,"requestId":"14003-6004"}`, false, false},
		{`{"message":"error code 14003"}`, false, false},
		{`{"code":1,"metadata":{"code":14003}}`, false, false},
		{`{"data":{"models":[{"code":6004}]}}`, false, false},
		{`{"code":null,"message":"busy"}`, false, false},
		{`{"code":14003`, false, false},
	} {
		if quota, busy := IsModelRateLimit(tc.body), IsModelBusy(tc.body); quota != tc.quota || busy != tc.busy {
			t.Errorf("%s: quota=%v busy=%v want %v/%v", tc.body, quota, busy, tc.quota, tc.busy)
		}
	}
}

func TestModelBusyCodeClassifiesAcrossStatusAndFrames(t *testing.T) {
	const body = `{"code":14003,"msg":"upstream inference overloaded","displayMsg":{"zh":"模型繁忙"}}`
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		if kind := Classify(status, body); kind != ErrSoftRate {
			t.Fatalf("status=%d kind=%s want soft_rate", status, kind)
		}
	}
	if kind := FrameKind(`{"error":{"code":14003,"message":"upstream inference overloaded"}}`); kind != ErrSoftRate {
		t.Fatalf("busy frame=%s", kind)
	}
	if kind := FrameKind(`{"error":{"code":140030,"message":"unknown upstream error"}}`); kind != ErrClient {
		t.Fatalf("nearby code was misclassified: %s", kind)
	}
}

func TestAccountErrorClassificationIgnoresRequestIDs(t *testing.T) {
	for _, tc := range []struct {
		body string
		want ErrKind
	}{
		{`{"code":11128,"msg":"request illegal","requestId":"trace-12153"}`, ErrChannelRefused},
		{`{"code":11140,"msg":"request illegal","requestId":"trace-12153"}`, ErrAccountFault},
		{`{"code":1,"msg":"unknown request error","requestId":"12153"}`, ErrClient},
		{`{"code":121530,"msg":"unknown request error"}`, ErrClient},
		{`{"code":12153,"msg":"refresh token is invalid"}`, ErrSessionDead},
		{`{"error":{"message":"Offline user session not found"}}`, ErrSessionDead},
		{`Offline user session not found`, ErrSessionDead},
		{`{"error":{"code":"upstream_error","message":"{\"code\":11140,\"msg\":\"request illegal\"}"}}`, ErrAccountFault},
	} {
		if got := Classify(http.StatusBadRequest, tc.body); got != tc.want {
			t.Errorf("Classify(%s)=%s want %s", tc.body, got, tc.want)
		}
	}
	const wrapped = `{"error":{"code":"upstream_error","message":"{\"code\":11140,\"msg\":\"request illegal\"}"}}`
	if !IsAccountRestricted(wrapped) {
		t.Fatal("wrapped SSE account restriction must retain the account-fault policy")
	}
}

func TestFrameKindPreservesNestedBusinessErrors(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    ErrKind
	}{
		{`{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		{`{"error":{"extError":{"code":"11-128","msg":"request illegal"}}}`, ErrChannelRefused},
		{`{"error":{"message":"unknown error","data":{"code":11128,"msg":"conversation id missing"}}}`, ErrChannelRefused},
		{`{"error":{"message":"unknown error","data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		{`{"error":{"message":"unknown error","data":{"code":111280,"msg":"request illegal","requestId":"trace-12153"}}}`, ErrClient},
		{`{"error":{"data":{"requestId":"12153","type":"rate-limiting"}}}`, ErrNone},
	} {
		if got := FrameKind(tc.payload); got != tc.want {
			t.Errorf("FrameKind(%s)=%s want %s", tc.payload, got, tc.want)
		}
	}
}
