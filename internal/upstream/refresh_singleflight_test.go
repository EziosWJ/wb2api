package upstream

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestRefreshTokenSingleFlightPerAccount(t *testing.T) {
	var hits, inFlight, maxInFlight int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&hits, 1)
		n := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if n <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"at1","refreshToken":"rt1","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{UID: "uid-a", AccessToken: "at0", RefreshToken: "rt0", ExpiresAt: 1}

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.RefreshToken(a)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream refresh hits = %d, want 1 (single-flight)", got)
	}
	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Errorf("max concurrent refresh = %d, want 1", got)
	}
	if a.AccessToken != "at1" || a.RefreshToken != "rt1" {
		t.Errorf("tokens not updated: at=%q rt=%q", a.AccessToken, a.RefreshToken)
	}
}

func TestRefreshAdoptsNewerDiskTokenAfterSessionDead(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "uid-a.json")
	disk := `{"auth":{"accessToken":"at-disk","refreshToken":"rt-disk","expiresAt":1999999999,"domain":"copilot.tencent.com","realm":"cn"},"account":{"uid":"uid-a"}}`
	if err := os.WriteFile(fp, []byte(disk), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		rt := r.Header.Get("X-Refresh-Token")
		mu.Lock()
		seen = append(seen, rt)
		mu.Unlock()
		if rt != "rt-disk" {
			return &http.Response{
				StatusCode: 401,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"refresh token is invalid"}`)),
			}, nil
		}
		return jsonResp(200, `{"code":0,"data":{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{UID: "uid-a", AccessToken: "at0", RefreshToken: "rt0", ExpiresAt: 1, FilePath: fp}

	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh should recover from disk token: %v", err)
	}
	if a.RefreshToken != "rt2" || a.AccessToken != "at2" {
		t.Errorf("tokens not updated after retry: at=%q rt=%q", a.AccessToken, a.RefreshToken)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "rt0" || seen[1] != "rt-disk" {
		t.Errorf("refresh attempts = %v, want [rt0 rt-disk]", seen)
	}
}
