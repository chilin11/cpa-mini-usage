package main

import (
	"math"
	"testing"
	"time"
)

// References: CodexBar CostUsagePricing.swift codexCostUSD / claudeCostUSD,
// CPA v8.0.22 helps/usage_helpers.go parseClaudeUsage and usage/accounting.go.
func TestProviderTokenAccountingContract(t *testing.T) {
	for _, tc := range []struct {
		name, body                string
		input, read, write, total int64
		cost                      float64
	}{
		{"Claude independent cache", `{"Provider":"claude","Model":"test","Detail":{"InputTokens":100,"OutputTokens":20,"CachedTokens":300,"CacheReadTokens":300,"CacheCreationTokens":50}}`, 100, 300, 50, 470, .0014625},
		{"Claude cache write not mistaken for read", `{"Provider":"claude","Model":"test","Detail":{"InputTokens":100,"OutputTokens":20,"CachedTokens":50,"CacheReadTokens":0,"CacheCreationTokens":50,"TotalTokens":170}}`, 100, 0, 50, 170, .0013125},
		{"Codex subset cache", `{"Provider":"codex","Model":"test","Detail":{"InputTokens":1000,"OutputTokens":100,"CachedTokens":600,"CacheReadTokens":600,"CacheCreationTokens":100}}`, 1000, 600, 100, 1100, .004925},
		{"Codex explicit cache read beats legacy total", `{"Provider":"codex","Model":"test","Detail":{"InputTokens":1000,"OutputTokens":100,"CachedTokens":700,"CacheReadTokens":600,"CacheCreationTokens":100,"TotalTokens":1100}}`, 1000, 600, 100, 1100, .004925},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			price := ModelPrice{Input: 5, Output: 25, CacheRead: floatPtr(.5), CacheWrite: floatPtr(6.25)}
			e.prices["openai/test"] = price
			e.prices["anthropic/test"] = price
			e.capture([]byte(tc.body))
			ev := e.events[0]
			if ev.Input != tc.input || ev.CacheRead != tc.read || ev.CacheWrite != tc.write || ev.Total != tc.total {
				t.Fatalf("event %+v", ev)
			}
			cost, known := e.eventCost(ev)
			if !known || math.Abs(cost-tc.cost) > 1e-12 {
				t.Fatalf("cost %.12f want %.12f", cost, tc.cost)
			}
		})
	}
}

func TestContextPricingUsesDeclaredThreshold(t *testing.T) {
	raw := []byte(`{"openai":{"models":{"test":{"cost":{"input":2.5,"output":15,"cache_read":0.25,"context_over_200k":{"input":5,"output":22.5},"tiers":[{"input":5,"output":22.5,"cache_read":0.5,"tier":{"type":"context","size":272000}}]}}}}}`)
	prices, err := decodePrices(raw)
	if err != nil {
		t.Fatal(err)
	}
	if prices["openai/test"].ContextTier == nil || prices["openai/test"].ContextTier.Threshold != 272000 {
		t.Fatal("wrong context threshold")
	}
	e := newEngine()
	e.prices = prices
	for _, input := range []int64{200001, 272000, 272001} {
		ev := Event{Provider: "codex", Model: "test", Input: input, CacheRead: 100000, Output: 1000}
		want := float64(input-100000)*2.5/1e6 + .025 + .015
		if input > 272000 {
			want = float64(input-100000)*5/1e6 + .05 + .0225
		}
		got, ok := e.eventCost(ev)
		if !ok || math.Abs(got-want) > 1e-12 {
			t.Fatalf("input %d got %v want %v", input, got, want)
		}
	}
	// Anthropic context totals include independent read/write buckets.
	e.prices["anthropic/test"] = prices["openai/test"]
	got, _ := e.eventCost(Event{Provider: "claude", Model: "test", Input: 200000, CacheRead: 73000, Output: 1000})
	if math.Abs(got-(1+.0365+.0225)) > 1e-12 {
		t.Fatal("Claude full prompt was not used for threshold")
	}
}

func TestQuotaEstimateStopsAtQuotaObservation(t *testing.T) {
	e := newEngine()
	now := time.Now().UTC()
	fetched := now.Add(-2 * time.Minute)
	e.prices["openai/test"] = ModelPrice{Input: 2, Output: 8}
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", FetchedAt: fetched.Format(time.RFC3339Nano), Five: &QuotaWindow{Used: 25, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}}
	e.events = []Event{{AuthID: "a", Provider: "codex", Model: "test", At: fetched.Add(-time.Minute), Input: 100000, Output: 20000, Total: 120000}, {AuthID: "a", Provider: "codex", Model: "test", At: fetched.Add(time.Minute), Input: 1000000, Output: 100000, Total: 1100000}}
	state := e.snapshot()
	win := state.Quotas[0].Five
	if win.ObservedTokens != 120000 || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 480000 || win.EstimatedTotalCost == nil || math.Abs(*win.EstimatedTotalCost-1.44) > 1e-12 {
		t.Fatalf("stale ratio included later usage %+v", win)
	}
	if state.Credentials[0].Total != 1220000 {
		t.Fatal("request totals should still include new requests")
	}
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", FetchedAt: now.Format(time.RFC3339Nano), Five: &QuotaWindow{Used: 50, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}}
	win = e.snapshot().Quotas[0].Five
	if win.ObservedTokens != 1220000 || *win.EstimatedTotalTokens != 2440000 {
		t.Fatal("new quota fetch must include later requests")
	}
}
