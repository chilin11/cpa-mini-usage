package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"reflect"
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
			state := e.snapshot()
			if state.Recent[0].Cost == nil || state.Recent[0].MissingPrice || math.Abs(*state.Recent[0].Cost-tc.cost) > 1e-12 || state.Credentials[0].Cost == nil || math.Abs(*state.Credentials[0].Cost-*state.Recent[0].Cost) > 1e-12 {
				t.Fatalf("request and aggregate valuation disagree: %+v", state)
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
		ev := Event{Provider: "openai", Model: "test", Input: input, CacheRead: 100000, Output: 1000}
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

func TestQuotaEstimateUsesPairedObservationWithLiveActuals(t *testing.T) {
	e := newEngine()
	now := time.Now().UTC()
	fetched := now.Add(-2 * time.Minute)
	e.prices["openai/test"] = ModelPrice{Input: 2, Output: 8}
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", FetchedAt: fetched.Format(time.RFC3339Nano), Five: &QuotaWindow{Used: 25, ResetAt: now.Add(time.Hour).Format(time.RFC3339)}}
	e.events = []Event{{AuthID: "a", Provider: "codex", Model: "test", At: fetched.Add(-time.Minute), Input: 100000, Output: 20000, Total: 120000}, {AuthID: "a", Provider: "codex", Model: "test", At: fetched.Add(time.Minute), Input: 1000000, Output: 100000, Total: 1100000}}
	state := e.snapshot()
	win := state.Quotas[0].Five
	if win.ObservedTokens != 1220000 || win.ObservedCost == nil || math.Abs(*win.ObservedCost-3.16) > 1e-12 || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 1220000 || win.EstimatedTotalCost == nil || math.Abs(*win.EstimatedTotalCost-3.16) > 1e-12 || win.EstimatedBasis != "quota" || win.EstimateObservedTokens == nil || *win.EstimateObservedTokens != 120000 || win.EstimateObservedCost == nil || math.Abs(*win.EstimateObservedCost-.36) > 1e-12 {
		t.Fatalf("live actuals must retain paired forecast with actual floors %+v", win)
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

func TestLowUsageClaudeWindowEstimate(t *testing.T) {
	e := newEngine()
	now := time.Now().UTC()
	e.prices["anthropic/test"] = ModelPrice{Input: 2, Output: 8, CacheRead: floatPtr(.2), CacheWrite: floatPtr(2.5)}
	e.events = []Event{{At: now.Add(-time.Minute), AuthID: "a", Provider: "claude", Model: "test", Input: 100, CacheRead: 300, CacheWrite: 50, Output: 100, Total: 550}}
	win := e.windowSummary("a", now, &QuotaWindow{Used: 3, ResetAt: now.Add(4 * time.Hour).Format(time.RFC3339)}, 5*time.Hour)
	if win.EstimatedTotalTokens == nil || win.EstimatedTotalCost == nil {
		t.Fatalf("3%% must be supported: %+v", win)
	}
	if *win.EstimatedTotalTokens != 18333 || math.Abs(*win.EstimatedTotalCost-.04) > 1e-9 {
		t.Fatal("rounded estimated total arithmetic changed")
	}
	e.events[0].Model = "missing"
	win = e.windowSummary("a", now, &QuotaWindow{Used: 3, ResetAt: now.Add(4 * time.Hour).Format(time.RFC3339)}, 5*time.Hour)
	if win.EstimatedTotalTokens == nil || win.EstimatedTotalCost != nil || !win.MissingPrice {
		t.Fatal("missing price must not hide known tokens or invent money")
	}
	e.events = nil
	win = e.windowSummary("a", now, &QuotaWindow{Used: 3, ResetAt: now.Add(4 * time.Hour).Format(time.RFC3339)}, 5*time.Hour)
	if win.EstimatedTotalTokens != nil || win.EstimatedTotalCost != nil {
		t.Fatal("empty window must remain unknown")
	}
}

func TestInputBreakdownAndSummary(t *testing.T) {
	for _, tc := range []struct {
		event           Event
		total, uncached int64
	}{
		{Event{Provider: "claude", Input: 2, CacheRead: 60590, CacheWrite: 10960, Output: 2190, Total: 73742}, 71552, 2},
		{Event{Provider: "codex", Input: 1000, CacheRead: 600, CacheWrite: 100, Output: 100, Total: 1100}, 1000, 300},
		{Event{Provider: "codex", Input: 1000, CacheRead: 1500, CacheWrite: 500}, 1000, 0},
	} {
		total, uncached := inputBreakdown(tc.event)
		if total != tc.total || uncached != tc.uncached {
			t.Fatalf("input %d %d", total, uncached)
		}
		e := newEngine()
		event := tc.event
		event.At = time.Now().UTC()
		event.AuthID = "a"
		e.events = []Event{event, event}
		credential := e.snapshot().Credentials[0]
		if credential.TotalInput != 2*tc.total || credential.UncachedInput != 2*tc.uncached || credential.CacheWrite != 2*event.CacheWrite || credential.Input != 2*event.Input || credential.Output != 2*event.Output {
			t.Fatalf("summary %+v", credential)
		}
	}
}

func TestRecentRequestCostKnownUnknownZeroAndFailed(t *testing.T) {
	e := newEngine()
	now := time.Now().UTC()
	e.prices["openai/test"] = ModelPrice{Input: 2, Output: 8, CacheRead: floatPtr(.2), CacheWrite: floatPtr(3)}
	e.prices["openai/free"] = ModelPrice{Input: 2, CacheRead: floatPtr(0), CacheWrite: floatPtr(0)}
	e.events = []Event{
		{At: now, AuthID: "a", Provider: "codex", Model: "test", Input: 1000, CacheRead: 600, CacheWrite: 100, Output: 100, Total: 1100},
		{At: now, AuthID: "a", Provider: "codex", Model: "test", Input: 1000, Output: 100, Total: 1100, Failed: true},
		{At: now, AuthID: "zero", Provider: "codex", Model: "free", Input: 1000, CacheRead: 900, CacheWrite: 100, Total: 1000},
		{At: now, AuthID: "missing", Provider: "codex", Model: "unknown", Input: 1000, Total: 1000},
	}
	s := e.snapshot()
	wants := map[string]float64{"test": .00182, "failed": .0028, "free": 0}
	sum := 0.0
	for _, ev := range s.Recent {
		key := ev.Model
		if ev.Failed {
			key = "failed"
		}
		want, known := wants[key]
		if !known {
			if ev.Cost != nil || !ev.MissingPrice {
				t.Fatalf("missing request price %+v", ev)
			}
		} else if ev.Cost == nil || ev.MissingPrice || math.Abs(*ev.Cost-want) > 1e-12 {
			t.Fatalf("request cost %+v want %v", ev, want)
		}
		if ev.AuthID == "a" {
			sum += *ev.Cost
		}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal(b, &obj); err != nil {
			t.Fatal(err)
		}
		if obj["model"] != ev.Model || obj["input"] != float64(ev.Input) || obj["event"] != nil || obj["missing_price"] != !known {
			t.Fatalf("raw-compatible flattened JSON %s", b)
		}
		if known && obj["cost"] != want || !known && obj["cost"] != nil {
			t.Fatalf("zero/unknown cost JSON %s", b)
		}
	}
	for _, credential := range s.Credentials {
		switch credential.AuthID {
		case "a":
			if credential.Cost == nil || *credential.Cost != sum || credential.Failures != 1 || len(credential.Models) != 1 || *credential.Models[0].Cost != sum {
				t.Fatalf("aggregate agreement %+v sum=%v", credential, sum)
			}
		case "zero":
			if credential.Cost == nil || *credential.Cost != 0 || credential.MissingPrice {
				t.Fatal("known zero credential cost lost")
			}
		case "missing":
			if credential.Cost != nil || !credential.MissingPrice {
				t.Fatal("unknown credential cost invented")
			}
		}
	}
}

func TestSnapshotCostRevaluationDoesNotMutatePersistedEvents(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.aliases["index"] = "a"
	e.prices["anthropic/test"] = ModelPrice{Input: 5, Output: 25, CacheRead: floatPtr(.5), CacheWrite: floatPtr(6.25)}
	e.capture([]byte(`{"AuthID":"index","Provider":"claude","Model":"test","Failed":true,"Latency":250000000,"TTFT":100000000,"Detail":{"InputTokens":100,"OutputTokens":20,"ReasoningTokens":999,"CacheReadTokens":300,"CacheCreationTokens":50,"TotalTokens":470}}`))
	before := append([]Event(nil), e.events...)
	path := e.dir + "/events.jsonl"
	rawBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := e.snapshot()
	if first.Recent[0].AuthID != "a" || first.Recent[0].Cost == nil || math.Abs(*first.Recent[0].Cost-.0014625) > 1e-12 {
		t.Fatalf("initial request valuation %+v", first.Recent[0])
	}
	e.prices["anthropic/test"] = ModelPrice{Input: 10, Output: 50, CacheRead: floatPtr(1), CacheWrite: floatPtr(12.5)}
	second := e.snapshot()
	if second.Recent[0].Cost == nil || math.Abs(*second.Recent[0].Cost-2**first.Recent[0].Cost) > 1e-12 || second.Credentials[0].Cost == nil || *second.Credentials[0].Cost != *second.Recent[0].Cost {
		t.Fatalf("updated prices must revalue both views %+v", second)
	}
	rawAfter, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(rawBefore, rawAfter) || !reflect.DeepEqual(before, e.events) {
		t.Fatalf("snapshot mutated persisted raw event: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(rawAfter), &stored); err != nil {
		t.Fatal(err)
	}
	if _, exists := stored["cost"]; exists {
		t.Fatal("derived cost persisted")
	}
	if _, exists := stored["missing_price"]; exists {
		t.Fatal("derived price classification persisted")
	}
	if stored["auth_id"] != "index" || stored["input"] != float64(100) || stored["cache_read"] != float64(300) || stored["cache_write"] != float64(50) || stored["output"] != float64(20) || stored["total"] != float64(470) || stored["failed"] != true {
		t.Fatalf("provider-native stored fields changed %s", rawAfter)
	}
}

func TestMissingCachePricesAreUnknown(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		for _, ev := range []Event{{Input: 1000, CacheRead: 500}, {Input: 1000, CacheWrite: 100}} {
			e := newEngine()
			e.prices[priceProvider(provider)+"/test"] = ModelPrice{Input: 2}
			ev.Provider, ev.Model = provider, "test"
			if cost, ok := e.eventCost(ev); ok {
				t.Fatalf("missing cache price invented: provider=%s event=%+v cost=%v", provider, ev, cost)
			}
		}
	}
}
