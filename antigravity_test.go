package main

import (
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func antigravityEngine(t *testing.T) *Engine {
	t.Helper()
	e := newEngine()
	e.dir = t.TempDir()
	e.prices["google/gemini-3.8-flash"] = ModelPrice{Input: .75, Output: 3.75, CacheRead: floatPtr(.075)}
	e.prices["anthropic/claude-opus-4-6"] = ModelPrice{Input: 5, Output: 25, CacheRead: floatPtr(.5), CacheWrite: floatPtr(6.25)}
	return e
}

func near(a *float64, want float64) bool { return a != nil && math.Abs(*a-want) < 1e-12 }

// sub2api: input = prompt - cached, output = candidates + thoughts, cache read = cached.
func TestAntigravityCostFollowsSub2API(t *testing.T) {
	e := antigravityEngine(t)
	e.capture([]byte(`{"AuthID":"g","Provider":"antigravity","Model":"gemini-3.8-flash-high","Detail":{"InputTokens":10000,"OutputTokens":500,"ReasoningTokens":1500,"CachedTokens":4000,"CacheReadTokens":4000,"TotalTokens":12000}}`))
	stored := e.events[0]
	if stored.Output != 500 || stored.Reasoning != 1500 || stored.Input != 10000 || stored.CacheRead != 4000 {
		t.Fatalf("raw counts must be stored unchanged: %+v", stored)
	}
	want := 6000*.75/1e6 + 2000*3.75/1e6 + 4000*.075/1e6
	s := e.snapshot()
	r, c := s.Recent[0], s.Credentials[0]
	if r.UsageQuality != "complete" || !near(r.Cost, want) || !near(c.Cost, want) || r.Output != 2000 || c.Output != 2000 {
		t.Fatalf("recent=%+v credential=%+v want=%v", r, c, want)
	}
	if e.events[0].Output != 500 {
		t.Fatal("snapshot rewrote stored output")
	}
	// Events stored before Reasoning existed recover thoughts from totalTokenCount.
	legacy := Event{Provider: "antigravity", Model: "gemini-3.8-flash", Input: 10000, Output: 500, CacheRead: 4000, Total: 12000}
	if cost, ok := e.eventCost(legacy); !ok || math.Abs(cost-want) > 1e-12 {
		t.Fatalf("legacy cost=%v ok=%v", cost, ok)
	}
}

func TestAntigravityCachedAbovePromptFloorsAtZero(t *testing.T) {
	e := antigravityEngine(t)
	ev := Event{Provider: "antigravity", Model: "gemini-3.8-flash", Input: 100, CacheRead: 300, Output: 10, Total: 110}
	if q := usageQuality(ev); q != "complete" {
		t.Fatalf("quality=%s", q)
	}
	cost, ok := e.eventCost(ev)
	if want := 10*3.75/1e6 + 300*.075/1e6; !ok || math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost=%v ok=%v want=%v", cost, ok, want)
	}
	if usageQuality(Event{Provider: "antigravity", Input: 100, CacheWrite: 1}) != "inconsistent" {
		t.Fatal("Gemini usage has no cache writes")
	}
}

func TestCodexReasoningIsNotAddedTwice(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.capture([]byte(`{"AuthID":"c","Provider":"codex","Model":"gpt","Detail":{"InputTokens":100,"OutputTokens":50,"ReasoningTokens":40,"TotalTokens":150}}`))
	if ev := e.events[0]; ev.Reasoning != 0 || billableOutput(ev) != 50 {
		t.Fatalf("codex reasoning is part of output: %+v", ev)
	}
}

func TestAntigravityPriceKeys(t *testing.T) {
	for model, want := range map[string][]string{
		"claude-opus-4-6-thinking":   {"anthropic/claude-opus-4-6-thinking", "anthropic/claude-opus-4-6"},
		"claude-sonnet-5-5-high":     {"anthropic/claude-sonnet-5-5-high", "anthropic/claude-sonnet-5-5"},
		"gemini-3-flash":             {"google/gemini-3-flash", "google/gemini-3-flash-preview"},
		"models/gemini-4-argon-high": {"google/gemini-4-argon-high", "google/gemini-4-argon", "google/gemini-4-argon-preview"},
		"gpt-oss-120b-medium":        {"openai/gpt-oss-120b-medium", "openai/gpt-oss-120b"},
		"tab_flash_lite":             nil,
	} {
		if got := antigravityPriceKeys(model); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", model, got, want)
		}
	}
	e := antigravityEngine(t)
	e.prices["google/gemini-3.8-flash-high"] = ModelPrice{Input: 9, Output: 9}
	if p, ok := e.priceFor(Event{Provider: "antigravity", Model: "gemini-3.8-flash-high"}); !ok || p.Input != 9 {
		t.Fatal("an exact tier entry must take precedence over the base model")
	}
	if _, ok := e.priceFor(Event{Provider: "antigravity", Model: "gpt-oss-120b-medium"}); ok {
		t.Fatal("unknown model must stay unpriced, not guessed")
	}
}

func TestDecodePricesIncludesGoogle(t *testing.T) {
	prices, err := decodePrices([]byte(`{"google":{"models":{"gemini-3.1-pro-preview":{"cost":{"input":2,"output":12,"cache_read":0.2,"tiers":[{"input":4,"output":18,"cache_read":0.4,"tier":{"type":"context","size":200000}}]}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := prices["google/gemini-3.1-pro-preview"]
	if !ok || p.Input != 2 || len(p.ContextTiers) != 1 || p.ContextTiers[0].Threshold != 200000 {
		t.Fatalf("google price not decoded: %+v", prices)
	}
	e := newEngine()
	e.prices = prices
	ev := Event{Provider: "antigravity", Model: "gemini-3.1-pro-high", Input: 250000, Output: 1000, Total: 251000}
	if cost, ok := e.eventCost(ev); !ok || math.Abs(cost-(250000*4+1000*18)/1e6) > 1e-12 {
		t.Fatalf("prompt above 200k must use the high tier: %v %v", cost, ok)
	}
}

const antigravitySummary = `{"groups":[
 {"displayName":"Gemini Models","description":"Models within this group: Gemini 3.8 Flash","buckets":[
  {"window":"5h","displayName":"Five Hour Limit Remaining","remainingFraction":0.7,"resetTime":"2026-10-24T15:00:00Z"},
  {"window":"weekly","remainingFraction":"45%","resetTime":"2026-10-30T00:00:00Z"},
  {"window":"monthly","remainingFraction":0.1}]},
 {"displayName":"Claude and GPT Models","buckets":[{"window":"5h","resetTime":"2026-10-24T15:00:00Z"},{"window":"weekly","remainingFraction":0,"resetTime":"2026-10-30T00:00:00Z"}]},
 {"displayName":"Empty","buckets":[{"window":"daily","remainingFraction":1}]}]}`

func TestParseAntigravityQuota(t *testing.T) {
	groups, err := parseAntigravityQuota([]byte(antigravitySummary))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Key != "gemini" || groups[1].Key != "claude-gpt" {
		t.Fatalf("groups=%+v", groups)
	}
	g := groups[0]
	if g.Five == nil || g.Five.Used != 30 || g.Five.Seconds != 18000 || g.Five.ResetAt != "2026-10-24T15:00:00Z" || g.Week == nil || g.Week.Used != 55 || g.Week.Seconds != 604800 {
		t.Fatalf("gemini=%+v five=%+v week=%+v", g, g.Five, g.Week)
	}
	if c := groups[1]; c.Five != nil || c.Week == nil || c.Week.Used != 100 {
		t.Fatalf("missing fraction must stay unknown, exhausted must be 100%%: %+v", c)
	}
	for _, bad := range []string{`{}`, `{"groups":[]}`, `{"groups":[{"buckets":[{"window":"5h","remainingFraction":1.5}]}]}`, `[`} {
		if _, err := parseAntigravityQuota([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if antigravityGroupKey("Imagen Models!") != "other-imagen-models" {
		t.Fatal(antigravityGroupKey("Imagen Models!"))
	}
}

func TestFetchAntigravityQuotaFallsBackAcrossEndpoints(t *testing.T) {
	var calls []string
	var bodies []string
	post := func(url string, headers http.Header, body []byte) (int, []byte, error) {
		calls = append(calls, url)
		bodies = append(bodies, string(body))
		if url == antigravityQuotaURLs[0] {
			return 403, nil, nil
		}
		return 200, []byte(antigravitySummary), nil
	}
	groups, failure := fetchAntigravityQuota(http.Header{}, "proj-1", post)
	if failure != "" || len(groups) != 2 || len(calls) != 2 || bodies[0] != `{"project":"proj-1"}` {
		t.Fatalf("failure=%q calls=%v bodies=%v", failure, calls, bodies)
	}
	_, failure = fetchAntigravityQuota(http.Header{}, "p", func(string, http.Header, []byte) (int, []byte, error) { return 401, nil, nil })
	if !strings.Contains(failure, "expired") {
		t.Fatal(failure)
	}
	if _, _, err := antigravityPOST("https://example.com/v1internal:retrieveUserQuotaSummary", nil, nil); err == nil {
		t.Fatal("non-allow-listed endpoint accepted")
	}
}

func TestRefreshAntigravityQuotaStoresGroupsAndRequiresProject(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	entry := AuthEntry{Index: "i", ID: "ag", Name: "a@example.com", Provider: "antigravity"}
	ok200 := func(string, http.Header, []byte) (int, []byte, error) { return 200, []byte(antigravitySummary), nil }
	if e.refreshAntigravityQuota(entry, http.Header{}, map[string]any{"access_token": "x"}, ok200) {
		t.Fatal("missing project id accepted")
	}
	if e.quotas["ag"].Error == "" {
		t.Fatal("missing project id must be reported")
	}
	if !e.refreshAntigravityQuota(entry, http.Header{}, map[string]any{"metadata": map[string]any{"project_id": "p"}}, ok200) {
		t.Fatal("refresh failed")
	}
	q := e.quotas["ag"]
	if q.Error != "" || len(q.Groups) != 2 || q.Five != nil || q.Week != nil || q.FetchedAt == "" {
		t.Fatalf("quota=%+v", q)
	}
}

func TestQuotaScanIncludesAntigravity(t *testing.T) {
	e := newEngine()
	var mu sync.Mutex
	var seen []string
	rpc := func(string, any) (any, error) {
		return []any{map[string]any{"auth_index": "1", "id": "ag", "type": "antigravity"}, map[string]any{"auth_index": "2", "id": "x", "type": "gemini-cli"}}, nil
	}
	e.refreshQuotaWith(true, rpc, func(a AuthEntry) bool { mu.Lock(); seen = append(seen, a.Provider); mu.Unlock(); return true })
	if !reflect.DeepEqual(seen, []string{"antigravity"}) || e.quotaError != "" {
		t.Fatalf("seen=%v err=%q", seen, e.quotaError)
	}
}

// Each group estimates from its own model family; Claude usage never inflates Gemini.
func TestAntigravityGroupForecastsAreScoped(t *testing.T) {
	e := antigravityEngine(t)
	now := time.Now().UTC()
	reset := now.Add(time.Hour).Format(time.RFC3339Nano)
	e.events = []Event{
		{At: now.Add(-time.Hour), AuthID: "ag", Provider: "antigravity", Model: "gemini-3.8-flash-high", Input: 1000000, Output: 100000, Total: 1100000},
		{At: now.Add(-time.Hour), AuthID: "ag", Provider: "antigravity", Model: "claude-opus-4-6-thinking", Input: 9000, Output: 1000, Total: 10000},
	}
	e.quotas["ag"] = AccountQuota{Provider: "antigravity", AuthID: "ag", Name: "a", FetchedAt: now.Format(time.RFC3339Nano), Groups: []QuotaGroup{
		{Key: "gemini", Label: "Gemini Models", Five: &QuotaWindow{Used: 25, ResetAt: reset, Seconds: 18000}},
		{Key: "claude-gpt", Label: "Claude and GPT Models", Five: &QuotaWindow{Used: 50, ResetAt: reset, Seconds: 18000}},
	}}
	s := e.snapshot()
	if len(s.Quotas) != 1 || len(s.Quotas[0].Groups) != 2 || s.Quotas[0].Five != nil {
		t.Fatalf("snapshot=%+v", s.Quotas)
	}
	gem, cl := s.Quotas[0].Groups[0].Five, s.Quotas[0].Groups[1].Five
	if gem.ObservedTokens != 1100000 || gem.EstimatedTotalTokens == nil || *gem.EstimatedTotalTokens != 4400000 || gem.EstimatedBasis != "quota" {
		t.Fatalf("gemini=%+v", gem)
	}
	if cl.ObservedTokens != 10000 || *cl.EstimatedTotalTokens != 20000 {
		t.Fatalf("claude=%+v", cl)
	}
	gemCost := 1000000*.75/1e6 + 100000*3.75/1e6
	if !near(gem.ObservedCost, gemCost) || !near(gem.EstimatedTotalCost, 4.5) {
		t.Fatalf("gemini cost=%v est=%v", gem.ObservedCost, gem.EstimatedTotalCost)
	}
	if _, ok := e.forecasts["antigravity/ag/gemini/5h"]; !ok {
		t.Fatalf("group forecast cache missing: %v", e.forecasts)
	}
	if _, ok := e.forecasts["antigravity/ag/claude-gpt/5h"]; !ok {
		t.Fatal("groups must not share a cache entry")
	}
	body, _ := json.Marshal(s.Quotas[0])
	if !strings.Contains(string(body), `"groups":[{"key":"gemini","label":"Gemini Models","five_hour":`) {
		t.Fatal(string(body))
	}
}
