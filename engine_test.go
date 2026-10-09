package main

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrices(t *testing.T) {
	raw := `{"openai":{"models":{"gpt-5.5":{"cost":{"input":2,"output":8,"cache_read":0.25}}}},"anthropic":{"models":{"claude-opus-4":{"cost":{"input":5,"output":25,"cache_write":6}}}}}`
	m, err := decodePrices([]byte(raw))
	if err != nil || len(m) != 2 {
		t.Fatalf("prices=%v, err=%v", m, err)
	}
	e := newEngine()
	e.prices = m
	c, ok := e.eventCost(Event{Provider: "codex", Model: "openai/gpt-5.5", Input: 1000000, CacheRead: 200000, Output: 100000})
	// 800k ordinary input at $2/M + 200k cache at $0.25/M + 100k output at $8/M.
	if !ok || math.Abs(c-2.45) > 0.00001 {
		t.Fatalf("cost=%f ok=%v", c, ok)
	}
}
func TestClaudeQuota(t *testing.T) {
	raw := `{"five_hour":{"utilization":34.5,"resets_at":"2026-10-09T21:00:00Z"},"seven_day":{"utilization":67,"resets_at":"2026-10-13T00:00:00Z"}}`
	five, week, err := parseClaudeQuota([]byte(raw))
	if err != nil || five.Used != 34.5 || week.Used != 67 {
		t.Fatalf("%v %v %v", five, week, err)
	}
}
func TestCodexQuota(t *testing.T) {
	raw := `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":1791580000},"secondary_window":{"used_percent":70,"limit_window_seconds":604800,"reset_at":1792000000}}}`
	five, week, err := parseCodexQuota([]byte(raw))
	if err != nil || five.Used != 20 || week.Used != 70 || five.ResetAt == "" {
		t.Fatalf("%v %v %v", five, week, err)
	}
}
func TestEstimateAndCredentialSeparation(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		t.Fatal(err)
	}
	e.prices = map[string]ModelPrice{"openai/gpt-5.5": {Input: 2, Output: 8}}
	now := time.Now().UTC()
	reset := now.Add(2 * time.Hour).Format(time.RFC3339)
	e.quotas["A"] = AccountQuota{AuthID: "A", Provider: "codex", Name: "Account A", Five: &QuotaWindow{Used: 25, ResetAt: reset}}
	body, _ := json.Marshal(map[string]any{"AuthID": "A", "Provider": "codex", "Model": "gpt-5.5", "RequestedAt": now.Add(-time.Minute).Format(time.RFC3339), "Detail": map[string]any{"InputTokens": 100000, "OutputTokens": 20000, "TotalTokens": 120000}})
	e.capture(body)
	other, _ := json.Marshal(map[string]any{"AuthID": "B", "Provider": "claude", "Model": "nonexisting", "Detail": map[string]any{"InputTokens": 1, "OutputTokens": 1, "TotalTokens": 2}})
	e.capture(other)
	out := e.snapshot()
	if len(out.Credentials) != 2 {
		t.Fatalf("unexpected creds %+v", out.Credentials)
	}
	win := out.Quotas[0].Five
	if win.ObservedTokens != 120000 || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 480000 {
		t.Fatalf("bad estimate %+v", win)
	}
	if win.EstimatedTotalCost == nil || math.Abs(*win.EstimatedTotalCost-1.44) > .00001 {
		t.Fatalf("bad USD %+v", win)
	}
	txt, err := os.ReadFile(e.dir + "/events.jsonl")
	if err != nil || strings.Contains(string(txt), "Bearer") {
		t.Fatalf("unexpected data on disk: %v", err)
	}
}
func TestAuthDecode(t *testing.T) {
	list := map[string]any{"files": []any{map[string]any{"auth_index": "idx1", "id": "A", "provider": "claude", "name": "alice@example.com.json"}}}
	auths := parseAuthList(list)
	if len(auths) != 1 || auths[0].ID != "A" || auths[0].Name != "alice@example.com" {
		t.Fatalf("auths %+v", auths)
	}
	creds := findAuthJSON(map[string]any{"Content": "{\"access_token\":\"secret\"}"})
	if findString(creds, 0, "access_token") != "secret" {
		t.Fatalf("bad credential parse: %+v", creds)
	}
}
func TestNoCredentialLeakage(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.prices = map[string]ModelPrice{}
	e.capture([]byte(`{"AuthID":"id_abc","Provider":"claude","Model":"opus","APIKey":"TOP_SECRET_CLIENT_KEY","Detail":{"InputTokens":3}}`))
	b, _ := json.Marshal(e.snapshot())
	if strings.Contains(string(b), "TOP_SECRET_CLIENT_KEY") {
		t.Fatal("API key in snapshot")
	}
}

func TestBillionTokenTotalsRemainExact(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	for i := 0; i < 2; i++ {
		e.capture([]byte(`{"AuthID":"large","Provider":"codex","Model":"gpt","Detail":{"InputTokens":2000000000,"OutputTokens":500000000,"CachedTokens":1000000000,"TotalTokens":2500000000}}`))
	}
	out := e.snapshot()
	if len(out.Credentials) != 1 {
		t.Fatalf("credentials: %+v", out.Credentials)
	}
	s := out.Credentials[0]
	if s.Input != 4000000000 || s.Output != 1000000000 || s.Cached != 2000000000 || s.Total != 5000000000 || s.Requests != 2 {
		t.Fatalf("large token totals changed: %+v", s)
	}
	if len(s.Models) != 1 || s.Models[0].Tokens != s.Total {
		t.Fatalf("model totals: %+v", s.Models)
	}
}

func TestAccountNamesAndManualReset(t *testing.T) {
	for _, name := range []string{"alice@example.com.json", "/auth/alice@example.com.JSON", " alice@example.com "} {
		if accountDisplayName(name) != "alice@example.com" {
			t.Fatalf("bad account name: %q", name)
		}
	}
	auths := parseAuthList(map[string]any{"files": []any{map[string]any{"auth_index": "idx", "provider": "claude", "name": "wrong.json", "email": "actual@example.com"}}})
	if len(auths) != 1 || auths[0].Name != "actual@example.com" {
		t.Fatalf("email precedence: %+v", auths)
	}
	for _, tc := range []struct {
		body string
		want int64
	}{{`{"manual_reset_remaining":2}`, 2}, {`{"reset_bank":{"remaining":0}}`, 0}, {`{"manual_resets":{"remaining_count":3}}`, 3}} {
		got := parseManualResetRemaining([]byte(tc.body))
		if got == nil || *got != tc.want {
			t.Fatalf("manual reset: %s => %v", tc.body, got)
		}
	}
	for _, body := range []string{`{}`, `{"reset_bank":{"total":3}}`, `{"manual_reset_remaining":null}`, `{"manual_reset_remaining":-1}`, `{"manual_reset_remaining":1.5}`, `{"manual_reset_remaining":"3"}`} {
		if parseManualResetRemaining([]byte(body)) != nil {
			t.Fatalf("must remain unknown: %s", body)
		}
	}
}

func TestRefreshCompletionState(t *testing.T) {
	e := newEngine()
	e.refreshQuota(true) // No native host in a unit test: fails quickly, but completes.
	s := e.snapshot()
	if s.QuotaRefreshing || s.QuotaRefreshFinishedAt == "" || s.QuotaError == "" {
		t.Fatalf("refresh state: %+v", s)
	}
}

func TestRecentAccountIdentityAndResetOmission(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.aliases["index"] = "account"
	e.names["account"] = "alice@example.com"
	e.quotas["account"] = AccountQuota{AuthID: "account", Provider: "claude"}
	e.capture([]byte(`{"AuthIndex":"index","Provider":"claude","Model":"claude-model","Detail":{"TotalTokens":1000}}`))
	s := e.snapshot()
	if len(s.Recent) != 1 || s.Recent[0].AuthID != "account" || s.Recent[0].Model != "claude-model" || s.Credentials[0].Name != "alice@example.com" {
		t.Fatalf("request account/model: %+v", s)
	}
	b, _ := json.Marshal(s.Quotas[0])
	if strings.Contains(string(b), "manual_reset_remaining") {
		t.Fatal("missing reset count should be omitted")
	}
	zero := int64(0)
	q := e.quotas["account"]
	q.ManualResetRemaining = &zero
	e.quotas["account"] = q
	b, _ = json.Marshal(e.snapshot().Quotas[0])
	if !strings.Contains(string(b), `"manual_reset_remaining":0`) {
		t.Fatal("explicit zero count must be preserved")
	}
}
