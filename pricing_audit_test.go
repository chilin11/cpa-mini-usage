package main

import (
	"math"
	"testing"
	"time"
)

func astraPrices(t *testing.T) map[string]ModelPrice {
	t.Helper()
	prices, err := decodePrices([]byte(`{"openai":{"models":{"gpt-6-astra":{"cost":{"input":10,"output":50,"cache_read":1,"cache_write":12.5,"tiers":[{"input":20,"output":75,"cache_read":2,"cache_write":25,"tier":{"type":"context","size":272000}}]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return prices
}

// One Standard API valuation basis for comparison, not subscription billing.
func TestAstraUnifiedStandardAPIValuation(t *testing.T) {
	e := newEngine()
	e.prices = astraPrices(t)
	for _, tc := range []struct {
		name               string
		input, read, write int64
		want               float64
	}{
		{"at threshold", 272000, 0, 0, 2.72705},
		{"one token above", 272001, 0, 0, 5.450595},
		{"304950 without cache", 304950, 0, 0, 6.109575},
		{"300k cache read", 304950, 300000, 0, .709575},
		{"fully cached", 304950, 304950, 0, .620475},
		{"cache read and write", 304950, 200000, 50000, 2.759575},
	} {
		for _, provider := range []string{"codex", "openai"} {
			t.Run(tc.name+"/"+provider, func(t *testing.T) {
				cost, ok := e.eventCost(Event{Provider: provider, Model: "gpt-6-astra", Input: tc.input, CacheRead: tc.read, CacheWrite: tc.write, Output: 141})
				if !ok || math.Abs(cost-tc.want) > 1e-12 {
					t.Fatalf("cost=%v known=%v want=%v", cost, ok, tc.want)
				}
			})
		}
	}
}

func TestCapturedAndLegacyEventsShareAPIValuation(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.prices = astraPrices(t)
	for _, provider := range []string{"codex", "openai"} {
		e.capture([]byte(`{"AuthID":"` + provider + `","Provider":"` + provider + `","Model":"gpt-6-astra","Detail":{"InputTokens":304950,"OutputTokens":141,"TotalTokens":305091}}`))
	}
	s := e.snapshot()
	for _, r := range s.Recent {
		if r.Cost == nil || math.Abs(*r.Cost-6.109575) > 1e-12 {
			t.Fatalf("request valuation wrong: %+v", r)
		}
	}
	for _, c := range s.Credentials {
		if c.Cost == nil || math.Abs(*c.Cost-6.109575) > 1e-12 || c.Models[0].Cost == nil || math.Abs(*c.Models[0].Cost-6.109575) > 1e-12 {
			t.Fatalf("aggregate valuation wrong: %+v", c)
		}
	}
	legacy := Event{Provider: "codex", Model: "gpt-6-astra", Input: 304950, Output: 141}
	if cost, ok := e.eventCost(legacy); !ok || math.Abs(cost-6.109575) > 1e-12 {
		t.Fatalf("legacy history differs: %v %v", cost, ok)
	}
}

func TestCodexForecastInvalidatesOtherPricingPolicies(t *testing.T) {
	for _, oldPolicy := range []string{"", "codex-base-v1"} {
		t.Run(oldPolicy, func(t *testing.T) {
			e := newEngine()
			e.prices = astraPrices(t)
			now := time.Now().UTC()
			reset := now.Add(time.Hour)
			q := AccountQuota{Provider: "codex", AuthID: "a", FetchedAt: now.Format(time.RFC3339Nano), Five: &QuotaWindow{Used: 50, ResetAt: reset.Format(time.RFC3339Nano)}}
			e.events = []Event{{Provider: "codex", AuthID: "a", At: now.Add(-time.Minute), Model: "gpt-6-astra", Input: 304950, Output: 141, Total: 305091}}
			key := "codex/a/5h"
			e.forecasts[key] = SavedForecast{ResetAt: reset, Seconds: 18000, PricingPolicy: oldPolicy, Estimate: WindowEstimate{EstimatedTotalCost: floatPtr(6.11), EstimatedTotalTokens: floatPtr(610182), EstimatedBasis: "quota"}}
			win, changed := e.resolveForecastLocked(q, "5h", now)
			if !changed || win.EstimateRetained || win.ObservedCost == nil || math.Abs(*win.ObservedCost-6.109575) > 1e-12 || win.EstimatedTotalCost == nil || *win.EstimatedTotalCost != 12.22 || e.forecasts[key].PricingPolicy != "api-standard-v1" {
				t.Fatalf("old basis survived: %+v", win)
			}
			saved := e.forecasts[key]
			saved.PricingPolicy = oldPolicy
			e.forecasts[key] = saved
			q.Five.Used = 0
			win, changed = e.resolveForecastLocked(q, "5h", now)
			if !changed || win.EstimateRetained || win.EstimatedTotalCost != nil {
				t.Fatalf("unusable sample reused other policy: %+v", win)
			}
		})
	}
}

func TestReportedWindowAPIValuationExtrapolation(t *testing.T) {
	e := newEngine()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	// Synthetic aggregate arithmetic, not reconstructed live history.
	e.prices["openai/synthetic"] = ModelPrice{Input: 15.01 / 4.63}
	e.events = []Event{{AuthID: "a", Provider: "codex", Model: "synthetic", At: now.Add(-time.Minute), Input: 4630000, Total: 4630000}}
	win := e.windowSummaryObserved("a", now, now, &QuotaWindow{Used: 55, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}, 5*time.Hour)
	if win.EstimatedBasis != "quota" || win.ObservedCost == nil || math.Abs(*win.ObservedCost-15.01) > 1e-12 || win.EstimatedTotalCost == nil || *win.EstimatedTotalCost != 27.29 || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 8418182 {
		t.Fatalf("unexpected extrapolation: %+v", win)
	}
}
