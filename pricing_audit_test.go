package main

import (
	"math"
	"testing"
	"time"
)

// Standard API-equivalent valuation, not Codex subscription billing.
// Sources: https://developers.openai.com/api/docs/models/gpt-6-astra
// and https://developers.openai.com/api/docs/pricing .
func TestAstraStandardAPIValuationExample(t *testing.T) {
	prices, err := decodePrices([]byte(`{"openai":{"models":{"gpt-6-astra":{"cost":{"input":10,"output":50,"cache_read":1,"cache_write":12.5,"tiers":[{"input":20,"output":75,"cache_read":2,"cache_write":25,"tier":{"type":"context","size":272000}}]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine()
	e.prices = prices
	for _, tc := range []struct {
		name        string
		input, read int64
		want        float64
	}{
		{"threshold stays standard", 272000, 0, 2.72705},
		{"above threshold prices entire request", 272001, 0, 5.450595},
		{"reported rounded-input example without cache", 304950, 0, 6.109575},
		{"hypothetical 300k cache reads remain in context", 304950, 300000, .709575},
		{"hypothetical fully cached input", 304950, 304950, .620475},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost, ok := e.eventCost(Event{Provider: "codex", Model: "gpt-6-astra", Input: tc.input, CacheRead: tc.read, Output: 141})
			if !ok || math.Abs(cost-tc.want) > 1e-12 {
				t.Fatalf("cost=%v known=%v want=%v", cost, ok, tc.want)
			}
		})
	}
}

func TestReportedWindowAPIValuationExtrapolation(t *testing.T) {
	e := newEngine()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	// A synthetic rate isolates the screenshot's rounded aggregate values;
	// this is not a reconstruction of its underlying requests or prices.
	e.prices["openai/synthetic"] = ModelPrice{Input: 15.01 / 4.63}
	e.events = []Event{{AuthID: "a", Provider: "codex", Model: "synthetic", At: now.Add(-time.Minute), Input: 4630000, Total: 4630000}}
	win := e.windowSummaryObserved("a", now, now, &QuotaWindow{Used: 55, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}, 5*time.Hour)
	if win.EstimatedBasis != "quota" || win.ObservedCost == nil || math.Abs(*win.ObservedCost-15.01) > 1e-12 || win.EstimatedTotalCost == nil || *win.EstimatedTotalCost != 27.29 || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 8418182 {
		t.Fatalf("unexpected API valuation extrapolation: %+v", win)
	}
}
