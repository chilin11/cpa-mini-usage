package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyCPAUsageQualityAndCost(t *testing.T) {
	for _, tc := range []struct {
		name, provider, detail, quality string
		known                           bool
		want                            float64
	}{
		{"total only", "codex", `"TotalTokens":1000`, "unclassified", false, 0},
		{"read exceeds input", "codex", `"InputTokens":100,"OutputTokens":20,"CacheReadTokens":200,"TotalTokens":120`, "inconsistent", false, 0},
		{"read and write exceed input", "codex", `"InputTokens":100,"CacheReadTokens":80,"CacheCreationTokens":30,"TotalTokens":100`, "inconsistent", false, 0},
		{"Claude independent caches", "claude", `"InputTokens":100,"OutputTokens":20,"CacheReadTokens":200,"CacheCreationTokens":50,"TotalTokens":370`, "complete", true, .0014125},
		{"Codex mirrored legacy cache", "codex", `"InputTokens":1000,"OutputTokens":100,"CachedTokens":600,"CacheReadTokens":600,"TotalTokens":1100`, "complete", true, .0048},
		{"Claude explicit zero read", "claude", `"InputTokens":100,"OutputTokens":20,"CachedTokens":50,"CacheReadTokens":0,"CacheCreationTokens":50,"TotalTokens":170`, "complete", true, .0013125},
		{"empty callback", "codex", `"TotalTokens":0`, "complete", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			e.prices[priceProvider(tc.provider)+"/test"] = ModelPrice{Input: 5, Output: 25, CacheRead: floatPtr(.5), CacheWrite: floatPtr(6.25)}
			e.capture([]byte(`{"AuthID":"a","Provider":"` + tc.provider + `","Model":"test","Detail":{` + tc.detail + `}}`))
			if len(e.events) != 1 {
				t.Fatal("callback lost")
			}
			before, err := os.ReadFile(filepath.Join(e.dir, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			s := e.snapshot()
			r, c := s.Recent[0], s.Credentials[0]
			if r.UsageQuality != tc.quality || r.MissingPrice == tc.known || c.MissingPrice == tc.known {
				t.Fatalf("quality or availability wrong: recent=%+v credential=%+v", r, c)
			}
			for _, cost := range []*float64{r.Cost, c.Cost, c.Models[0].Cost} {
				if tc.known && (cost == nil || math.Abs(*cost-tc.want) > 1e-12) || !tc.known && cost != nil {
					t.Fatalf("cost=%v known=%v want=%v", cost, tc.known, tc.want)
				}
			}
			win := e.windowSummary("a", time.Now().UTC(), &QuotaWindow{Used: 50, ResetAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}, 5*time.Hour)
			if win.ObservedTokens != e.events[0].Total || win.MissingPrice == tc.known || !tc.known && (win.ObservedCost != nil || win.EstimatedTotalCost != nil) {
				t.Fatalf("window claims a partial price: %+v", win)
			}
			after, err := os.ReadFile(filepath.Join(e.dir, "events.jsonl"))
			if err != nil || string(before) != string(after) {
				t.Fatal("valuation changed raw history")
			}
		})
	}
}

func TestContextTiersInheritanceHighestAndPersistence(t *testing.T) {
	// Deliberately unsorted. Missing fields inherit BASE prices, not lower tiers.
	prices, err := decodePrices([]byte(`{"openai":{"models":{"test":{"cost":{"input":2,"output":10,"cache_read":0.2,"cache_write":3,"tiers":[{"input":6,"cache_read":0,"tier":{"type":"context","size":2000}},{"input":4,"output":20,"tier":{"type":"context","size":1000}},{"input":999,"tier":{"type":"speed","size":500}}]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(prices["openai/test"].ContextTiers) != 2 {
		t.Fatal("lost context tiers")
	}
	// Persistence round-trip must retain all tiers and explicit zero rates.
	raw, err := json.Marshal(prices)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &prices); err != nil {
		t.Fatal(err)
	}
	e := newEngine()
	e.prices = prices
	for _, tc := range []struct {
		input int64
		want  float64
	}{{1000, .0023}, {1001, .003904}, {2000, .0079}, {2001, .009406}} {
		cost, ok := e.eventCost(Event{Provider: "openai", Model: "test", Input: tc.input, CacheRead: 500, CacheWrite: 200, Output: 100})
		if !ok || math.Abs(cost-tc.want) > 1e-12 {
			t.Fatalf("input=%d cost=%v known=%v want=%v", tc.input, cost, ok, tc.want)
		}
	}
	// Old prices.json used a single fully resolved context_tier.
	var legacy ModelPrice
	if err := json.Unmarshal([]byte(`{"input":2,"output":10,"context_tier":{"threshold":1000,"input":4,"output":20}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	e.prices["openai/legacy"] = legacy
	if cost, ok := e.eventCost(Event{Provider: "openai", Model: "legacy", Input: 1500, Output: 100}); !ok || math.Abs(cost-.008) > 1e-12 {
		t.Fatalf("legacy cache incompatible: %v %v", cost, ok)
	}
}

func TestPriceDecoderDistinguishesMissingAndExplicitFree(t *testing.T) {
	prices, err := decodePrices([]byte(`{"openai":{"models":{"free":{"cost":{"input":0,"output":0,"cache_read":0,"cache_write":0}},"missing-output":{"cost":{"input":2}},"missing-input":{"cost":{"output":8}},"absent":{},"test":{"cost":{"input":2,"output":8}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) != 2 {
		t.Fatalf("missing rates treated as zero: %+v", prices)
	}
	e := newEngine()
	e.prices = prices
	for _, ev := range []Event{{Input: 1000, Output: 100, CacheRead: 500, CacheWrite: 100}, {}} {
		ev.Provider, ev.Model = "codex", "free"
		if cost, ok := e.eventCost(ev); !ok || cost != 0 {
			t.Fatalf("explicit free price lost: %v %v", cost, ok)
		}
	}
	if cost, ok := e.eventCost(Event{Provider: "codex", Model: "test", Input: 1000, Output: 100}); !ok || math.Abs(cost-.0028) > 1e-12 {
		t.Fatalf("absent unused cache rate hid ordinary price: %v %v", cost, ok)
	}
}

func TestInvalidUsageNeverGetsClampedIntoKnownCost(t *testing.T) {
	e := newEngine()
	e.prices["openai/test"] = ModelPrice{Input: 2, Output: 8, CacheRead: floatPtr(.2), CacheWrite: floatPtr(3)}
	e.prices["anthropic/test"] = e.prices["openai/test"]
	for _, ev := range []Event{
		{Provider: "codex", Input: -1},
		{Provider: "codex", Input: 100, CacheRead: 200},
		{Provider: "codex", Input: 100, CacheRead: 80, CacheWrite: 30},
		{Provider: "codex", Input: math.MaxInt64, Output: 1},
		{Provider: "claude", Input: math.MaxInt64, CacheWrite: 1},
	} {
		ev.Model = "test"
		if _, ok := e.eventCost(ev); ok || usageQuality(ev) != "inconsistent" {
			t.Fatalf("invalid usage priced: %+v", ev)
		}
	}
}

func TestNegativeCPACallbackIsRejected(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	e.capture([]byte(`{"Provider":"codex","Model":"test","Detail":{"InputTokens":-1,"OutputTokens":10}}`))
	if len(e.events) != 0 {
		t.Fatal("negative callback was silently clamped into valid usage")
	}
}
