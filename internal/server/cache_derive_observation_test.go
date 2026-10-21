package server

import (
	"encoding/json"
	"io"
	"math"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestCacheObservationsMatchForRawStreamingAndSync(t *testing.T) {
	for _, tc := range []struct {
		name             string
		u                string
		hit, miss, write int64
		ok               bool
	}{
		{"zero hit derives", `{"prompt_tokens":100,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":3,"prompt_cache_write_tokens":5}`, 97, 3, 5, true},
		{"miss only", `{"prompt_tokens":100,"prompt_cache_miss_tokens":3}`, 97, 3, 0, true},
		{"positive nested hit", `{"prompt_tokens":100,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":3,"prompt_tokens_details":{"cached_tokens":80}}`, 80, 3, 0, true},
		{"positive input details", `{"prompt_tokens":100,"input_tokens_details":{"cached_tokens":90}}`, 90, 0, 0, true},
		{"flat alias", `{"prompt_tokens":100,"cache_read_input_tokens":90}`, 90, 0, 0, true},
		{"true zero", `{"prompt_tokens":100,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":100}`, 0, 100, 0, true},
		{"miss larger than prompt", `{"prompt_tokens":100,"prompt_cache_miss_tokens":110}`, 0, 110, 0, true},
		{"missing miss", `{"prompt_tokens":100,"prompt_cache_hit_tokens":0}`, 0, 0, 0, true},
		{"no cache", `{"prompt_tokens":100}`, 0, 0, 0, false},
		{"negative cache", `{"prompt_tokens":100,"prompt_cache_hit_tokens":-1,"prompt_cache_miss_tokens":-3}`, 0, 0, 0, false},
		{"invalid alias ignored", `{"prompt_tokens":100,"cached_tokens":"invalid","prompt_tokens_details":{"cached_tokens":"invalid"}}`, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var u map[string]any
			json.Unmarshal([]byte(tc.u), &u)
			u["credit"] = 0.03
			before, _ := json.Marshal(u)
			hit, miss, write, ok, credit, hasCredit := usageExtraFromResponse(map[string]any{"usage": u})
			if hit != tc.hit || miss != tc.miss || write != tc.write || ok != tc.ok || credit != 0.03 || !hasCredit {
				t.Fatalf("sync cache=%d/%d/%d/%v credit=%v/%v", hit, miss, write, ok, credit, hasCredit)
			}
			payload, _ := json.Marshal(map[string]any{"usage": u})
			s := newChatStatsReaderSince(strings.NewReader("data: "+string(payload)+"\n\ndata: [DONE]\n\n"), time.Now())
			io.Copy(io.Discard, s)
			sh, sm, sw, sok := s.CacheTokens()
			scredit, shasCredit := s.Credit()
			if int64(sh) != hit || int64(sm) != miss || int64(sw) != write || sok != ok || scredit != credit || shasCredit != hasCredit {
				t.Fatalf("raw streaming disagrees with sync: stream=%d/%d/%d/%v sync=%d/%d/%d/%v", sh, sm, sw, sok, hit, miss, write, ok)
			}
			after, _ := json.Marshal(u)
			if string(after) != string(before) {
				t.Fatal("sync observation mutated response usage")
			}
		})
	}
}

func TestCacheObservationPreservesFieldsAcrossUsageChunks(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	s.parseSSELine(`data: {"usage":{"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":3,"prompt_cache_write_tokens":7,"credit":0.02}}`)
	if hit, miss, _, ok := s.CacheTokens(); hit != 0 || miss != 3 || !ok {
		t.Fatalf("cannot derive before prompt arrives: %d/%d/%v", hit, miss, ok)
	}
	s.parseSSELine(`data: {"usage":{"prompt_tokens":100,"completion_tokens":2,"total_tokens":102}}`)
	if hit, miss, write, ok := s.CacheTokens(); hit != 97 || miss != 3 || write != 7 || !ok {
		t.Fatalf("missing later fields erased prior cache counts: %d/%d/%d/%v", hit, miss, write, ok)
	}
	s.parseSSELine(`data: {"usage":{"prompt_tokens":120}}`)
	if hit, _, _, _ := s.CacheTokens(); hit != 117 {
		t.Fatal("derived hit was incorrectly persisted as reported hit")
	}
	s.parseSSELine(`data: {"usage":{"prompt_tokens_details":{"cached_tokens":90}}}`)
	s.parseSSELine(`data: {"usage":{"completion_tokens":5,"prompt_tokens_details":{"other":1}}}`)
	if hit, miss, write, ok := s.CacheTokens(); hit != 90 || miss != 3 || write != 7 || !ok {
		t.Fatalf("positive nested hit was lost: %d/%d/%d/%v", hit, miss, write, ok)
	}
	if u := s.Usage(); u.PromptTokens != 120 || u.CompletionTokens != 5 || u.TotalTokens != 102 {
		t.Fatalf("cache derivation changed usage counts: %+v", u)
	}
	if c, ok := s.Credit(); !ok || c != 0.02 {
		t.Fatal("cache derivation changed credit")
	}
}

func TestInvalidCacheAliasDoesNotDiscardCoreUsage(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	s.parseSSELine(`data: {"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"credit":0.02,"cached_tokens":"not-a-number","prompt_tokens_details":{"cached_tokens":false},"prompt_cache_miss_tokens":3}}`)
	if u := s.Usage(); !u.HasPromptTokens || u.PromptTokens != 100 || !u.HasCompletionTokens || u.CompletionTokens != 5 || !u.HasTotalTokens || u.TotalTokens != 105 {
		t.Fatalf("invalid cache alias discarded core usage: %+v", u)
	}
	if hit, miss, _, ok := s.CacheTokens(); hit != 97 || miss != 3 || !ok {
		t.Fatalf("invalid alias blocked valid miss derivation: %d/%d/%v", hit, miss, ok)
	}
	if c, ok := s.Credit(); !ok || c != 0.02 {
		t.Fatal("invalid alias discarded credit")
	}
}

func TestCacheObservationRejectsInvalidNumericCounts(t *testing.T) {
	for _, invalid := range []any{math.NaN(), math.Inf(1), math.Inf(-1), -1.0} {
		u := map[string]any{"prompt_cache_hit_tokens": invalid, "prompt_cache_miss_tokens": invalid, "prompt_cache_write_tokens": invalid}
		if h, m, w, ok := cacheTokensFromUsage(u); h != 0 || m != 0 || w != 0 || ok {
			t.Fatalf("invalid cache evidence accepted: %v -> %d/%d/%d/%v", invalid, h, m, w, ok)
		}
	}
}

func TestRawCacheObservationMatchesNormalizedOutput(t *testing.T) {
	sse := "data: {\"id\":\"cache-observed\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2,\"total_tokens\":102,\"prompt_cache_hit_tokens\":0,\"prompt_cache_miss_tokens\":3,\"prompt_cache_write_tokens\":7}}\n\ndata: [DONE]\n\n"
	rawStats := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	rec := httptest.NewRecorder()
	if err := upstream.Stream(rec, rawStats); err != nil {
		t.Fatal(err)
	}
	var streamed map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk map[string]any
		json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk)
		if u, ok := chunk["usage"].(map[string]any); ok {
			streamed = u
		}
	}
	h, m, w, ok := rawStats.CacheTokens()
	sync, err := upstream.Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	ah, am, aw, aok, _, _ := usageExtraFromResponse(sync)
	if h != 97 || int64(h) != ah || int64(m) != am || int64(w) != aw || ok != aok || !reflect.DeepEqual(streamed["prompt_cache_hit_tokens"], float64(h)) {
		t.Fatalf("raw watcher=%d/%d/%d/%v aggregate=%d/%d/%d/%v streamed=%v", h, m, w, ok, ah, am, aw, aok, streamed)
	}
}
