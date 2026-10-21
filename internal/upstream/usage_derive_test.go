package upstream

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestUsageHitDerivesOnlyFromExplicitMiss(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    map[string]any
		hit  float64
		ok   bool
	}{
		{"miss only", map[string]any{"prompt_tokens": 52703.0, "prompt_cache_miss_tokens": 3.0}, 52700, true},
		{"zero hit", map[string]any{"prompt_tokens": 52703.0, "prompt_cache_miss_tokens": 3.0, "prompt_cache_hit_tokens": 0.0}, 52700, true},
		{"zero miss", map[string]any{"prompt_tokens": 10, "prompt_cache_miss_tokens": 0}, 10, true},
		{"reported nested hit wins", map[string]any{"prompt_tokens": 100, "prompt_cache_miss_tokens": 3, "prompt_cache_hit_tokens": 0, "prompt_tokens_details": map[string]any{"cached_tokens": 80}}, 80, true},
		{"positive flat hit wins", map[string]any{"prompt_tokens": 100, "prompt_cache_miss_tokens": 3, "cache_read_input_tokens": 80}, 80, true},
		{"true zero", map[string]any{"prompt_tokens": 10, "prompt_cache_miss_tokens": 10, "prompt_cache_hit_tokens": 0}, 0, true},
		{"prompt below miss", map[string]any{"prompt_tokens": 10, "prompt_cache_miss_tokens": 20}, 0, false},
		{"no miss", map[string]any{"prompt_tokens": 100, "prompt_cache_hit_tokens": 0}, 0, true},
		{"no cache", map[string]any{"prompt_tokens": 100}, 0, false},
		{"null miss", map[string]any{"prompt_tokens": 100, "prompt_cache_miss_tokens": nil}, 0, false},
		{"negative miss", map[string]any{"prompt_tokens": 100, "prompt_cache_miss_tokens": -1}, 0, false},
		{"negative prompt", map[string]any{"prompt_tokens": -1, "prompt_cache_miss_tokens": 0}, 0, false},
		{"infinite prompt", map[string]any{"prompt_tokens": math.Inf(1), "prompt_cache_miss_tokens": 3}, 0, false},
		{"NaN miss", map[string]any{"prompt_tokens": 100, "prompt_cache_miss_tokens": math.NaN()}, 0, false},
		{"infinite hit rejected", map[string]any{"cached_tokens": math.Inf(1)}, 0, false},
		{"JSON numbers", map[string]any{"prompt_tokens": json.Number("100"), "prompt_cache_miss_tokens": json.Number("1")}, 99, true},
		{"nil usage", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit, ok := UsageCacheHitTokens(tc.u)
			if hit != tc.hit || ok != tc.ok {
				t.Fatalf("hit=%v present=%v want %v/%v", hit, ok, tc.hit, tc.ok)
			}
		})
	}
}

func TestDerivedUsageHitNormalizationDoesNotMutateInput(t *testing.T) {
	u := map[string]any{
		"prompt_tokens": 100.0, "completion_tokens": 2.0, "total_tokens": 102.0,
		"prompt_cache_hit_tokens": 0.0, "prompt_cache_miss_tokens": 3.0, "prompt_cache_write_tokens": 7.0,
		"credit":                0.03,
		"prompt_tokens_details": map[string]any{"cached_tokens": 0.0, "other": 4.0},
		"input_tokens_details":  map[string]any{"cached_tokens": 0.0, "other": 8.0},
	}
	before, _ := json.Marshal(u)
	normalized := normalizeUsageCacheAliases(u)
	for _, key := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens"} {
		if normalized[key] != 97.0 {
			t.Fatalf("%s=%v want 97", key, normalized[key])
		}
	}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "prompt_cache_miss_tokens", "prompt_cache_write_tokens", "credit"} {
		if !reflect.DeepEqual(normalized[key], u[key]) {
			t.Fatalf("derived cache hit changed %s", key)
		}
	}
	for _, section := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if normalized[section].(map[string]any)["cached_tokens"] != 97.0 {
			t.Fatal("nested aliases did not mirror hit")
		}
	}
	after, _ := json.Marshal(u)
	if string(before) != string(after) {
		t.Fatal("normalization mutated upstream input map")
	}
	normalized["prompt_tokens_details"].(map[string]any)["cached_tokens"] = 1.0
	if u["prompt_tokens_details"].(map[string]any)["cached_tokens"] != 0.0 {
		t.Fatal("normalized nested details share mutable input map")
	}
}

func TestUsageWithoutEvidenceRemainsUnchanged(t *testing.T) {
	for _, u := range []map[string]any{
		{"prompt_tokens": 100.0, "prompt_cache_hit_tokens": 0.0},
		{"prompt_tokens": 100.0, "prompt_cache_hit_tokens": 0.0, "prompt_cache_miss_tokens": 100.0},
		{"prompt_tokens": 100.0},
	} {
		if got := normalizeUsageCacheAliases(u); !reflect.DeepEqual(got, u) {
			t.Fatalf("cache aliases manufactured without evidence: got=%v input=%v", got, u)
		}
	}
}

func TestUsageDerivedHitStreamingAndAggregateAgree(t *testing.T) {
	sse := "data: {\"id\":\"cache-derived\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":52703,\"completion_tokens\":2,\"total_tokens\":52705,\"prompt_cache_hit_tokens\":0,\"prompt_cache_miss_tokens\":3,\"prompt_cache_write_tokens\":7,\"credit\":0.03}}\n\ndata: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	aggregate := resp["usage"].(map[string]any)
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}
	var streamed map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			streamed = u
		}
	}
	if aggregate["prompt_cache_hit_tokens"] != 52700.0 || !reflect.DeepEqual(streamed, aggregate) {
		t.Fatalf("stream and aggregate differ: stream=%v aggregate=%v", streamed, aggregate)
	}
}
