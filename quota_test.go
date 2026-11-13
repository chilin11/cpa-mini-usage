package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Contract: openai/codex codex-rs/backend-client/src/types.rs and
// client/rate_limit_resets.rs, RateLimitResetCreditsSummary.available_count.
func TestCodexResetCreditsOfficialContract(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int64
	}{
		{`{"rate_limit_reset_credits":{"available_count":3}}`, 3},
		{`{"rate_limit_reset_credits":{"available_count":0}}`, 0},
		{`{"rate_limit_reset_credits":{"available_count":2},"manual_reset_remaining":9}`, 2},
	} {
		calls := 0
		got := codexResetRemaining([]byte(tc.body), nil, func(string, http.Header) ([]byte, error) { calls++; return nil, fmt.Errorf("should not fetch details") })
		if got == nil || *got != tc.want || calls != 0 {
			t.Fatalf("usage credits %s: count=%v calls=%d", tc.body, got, calls)
		}
	}
}

func TestCodexResetCreditsDetailsFallback(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int64
	}{
		{`{"available_count":4,"credits":[{"status":"redeemed"}]}`, 4},
		{`{"available_count":0,"credits":[]}`, 0},
	} {
		calls := 0
		headers := http.Header{"Authorization": {"Bearer test-only"}, "Chatgpt-Account-Id": {"account"}}
		got := codexResetRemaining([]byte(`{"credits":{"balance":99}}`), headers, func(url string, h http.Header) ([]byte, error) {
			calls++
			if url != codexResetCreditsURL || h.Get("Authorization") != "Bearer test-only" || h.Get("Chatgpt-Account-Id") != "account" {
				t.Fatalf("wrong request: %s", url)
			}
			return []byte(tc.body), nil
		})
		if got == nil || *got != tc.want || calls != 1 {
			t.Fatalf("details %s: count=%v calls=%d", tc.body, got, calls)
		}
	}
}

func TestCodexResetCreditsInvalidAndMissing(t *testing.T) {
	for _, body := range []string{`{}`, `{"available_count":null}`, `{"available_count":-1}`, `{"available_count":1.5}`, `{"available_count":"3"}`, `{"credits":[{"status":"available"}]}`, `not JSON`} {
		got := codexResetRemaining([]byte(`{}`), nil, func(string, http.Header) ([]byte, error) { return []byte(body), nil })
		if got != nil {
			t.Fatalf("invalid/missing count must stay hidden: %s", body)
		}
	}
	got := codexResetRemaining([]byte(`{}`), nil, func(string, http.Header) ([]byte, error) { return nil, fmt.Errorf("unavailable") })
	if got != nil {
		t.Fatal("failed optional endpoint must not invent zero")
	}
	if parseManualResetRemaining([]byte(`{"available_count":5,"credits":{"balance":50}}`)) != nil {
		t.Fatal("ordinary usage credits cannot be treated as reset credits")
	}
}

func TestResetConsumeEndpointNotAllowed(t *testing.T) {
	for _, url := range []string{codexResetCreditsURL + "/consume", "https://example.com/wham/rate-limit-reset-credits", claudeUsageURL + "/consume", claudeResetInventoryURL + "&redirect=https://example.com", claudeUsageURL + "?cedar_ember=2", "https://api.anthropic.com.example.com/api/oauth/usage?cedar_ember=1"} {
		if _, err := safeProviderGET(url, nil); err == nil {
			t.Fatalf("endpoint must be rejected: %s", url)
		}
	}
}

// Contract evidence: CodexBar 7447bc77c2497323d9c41e40164ce705533493c8,
// ClaudeRateLimitResetCredits.swift and ClaudeOAuthResetCreditsTests.swift.
func TestClaudeResetInventoryCountsSavedGrants(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		grants string
		want   int64
	}{
		{"sum and filter", `[
			{"resets_left":2,"resets_total":3,"paused":false,"starts_at":"2026-01-02T12:00:00Z","ends_at":"2026-01-03T12:00:00.123Z","usable_now":false},
			{"resets_left":1,"paused":false,"starts_at":null,"ends_at":null},
			{"resets_left":4,"paused":true},
			{"resets_left":8,"paused":false,"starts_at":"2026-01-02T12:00:01Z"},
			{"resets_left":16,"paused":false,"ends_at":"2026-01-02T12:00:00Z"},
			{"resets_left":0,"resets_total":1,"paused":false}
		]`, 3},
		{"empty eligible inventory", `[]`, 0},
		{"explicit exhausted grant", `[{"resets_left":0,"resets_total":2,"paused":false}]`, 0},
		{"all unavailable", `[{"resets_left":1,"paused":true},{"resets_left":1,"paused":false,"ends_at":"2020-01-01T00:00:00Z"}]`, 0},
		{"optional total and bounds", `[{"resets_left":1,"resets_total":null,"paused":false}]`, 1},
		{"timezone bound", `[{"resets_left":2,"paused":false,"starts_at":"2026-01-02T13:00:00+01:00"}]`, 2},
		{"plausibility boundary", `[{"resets_left":50,"paused":false}]`, 50},
		{"future grant does not trip cap", `[{"resets_left":51,"paused":false,"starts_at":"2099-01-01T00:00:00Z"},{"resets_left":1,"paused":false}]`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"cedar_ember":{"eligible":true,"grants":` + tc.grants + `}}`)
			got := parseClaudeResetRemaining(body, now)
			if got == nil || *got != tc.want {
				t.Fatalf("want %d saved resets, got %v", tc.want, got)
			}
		})
	}
}

func TestClaudeResetInventoryUnknownNeverBecomesZero(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, block := range []string{
		`null`, `[]`, `{}`, `{"eligible":false,"grants":[]}`, `{"eligible":"true","grants":[]}`,
		`{"eligible":1,"grants":[]}`, `{"eligible":null,"grants":[]}`, `{"grants":[]}`,
		`{"eligible":true}`, `{"eligible":true,"grants":null}`, `{"eligible":true,"grants":{}}`,
		`{"eligible":true,"grants":"[]"}`,
	} {
		if parseClaudeResetRemaining([]byte(`{"cedar_ember":`+block+`}`), now) != nil {
			t.Fatalf("unavailable block must stay unknown: %s", block)
		}
	}
	for _, grant := range []string{
		`null`, `[]`, `42`, `{}`, `{"paused":false}`, `{"resets_left":1}`, `{"resets_left":1,"paused":null}`,
		`{"resets_left":1,"paused":"false"}`, `{"resets_left":null,"paused":false}`,
		`{"resets_left":"2","paused":false}`, `{"resets_left":true,"paused":false}`,
		`{"resets_left":-1,"paused":false}`, `{"resets_left":1.5,"paused":false}`,
		`{"resets_left":9223372036854775808,"paused":false}`,
		`{"resets_left":2,"resets_total":1,"paused":false}`,
		`{"resets_left":0,"resets_total":-1,"paused":false}`,
		`{"resets_left":1,"resets_total":"2","paused":false}`,
		`{"resets_left":1,"paused":false,"starts_at":"soon"}`,
		`{"resets_left":1,"paused":false,"ends_at":""}`,
		`{"resets_left":1,"paused":false,"ends_at":12345}`,
		`{"resets_left":1,"paused":false,"starts_at":"2026-01-03T00:00:00Z","ends_at":"2026-01-02T00:00:00Z"}`,
		`{"resets_left":51,"paused":false}`,
	} {
		// Do not return a partial count even when a valid sibling exists.
		body := `{"cedar_ember":{"eligible":true,"grants":[{"resets_left":1,"paused":false},` + grant + `]}}`
		if parseClaudeResetRemaining([]byte(body), now) != nil {
			t.Fatalf("malformed inventory must stay unknown: %s", grant)
		}
	}
	for _, body := range []string{
		`not JSON`, `{}`, `null`, `[]`,
		`{"manual_reset_remaining":9,"rate_limit_reset_credits":{"available_count":3},"reset_bank":{"remaining":4}}`,
		`{"five_hour":{"utilization":30,"resets_at":"2026-01-03T00:00:00Z"},"extra_usage":{"used_credits":1,"monthly_limit":1000},"credits":{"balance":9}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"resets_left":26,"paused":false},{"resets_left":25,"paused":false}]}}`,
	} {
		if parseClaudeResetRemaining([]byte(body), now) != nil {
			t.Fatalf("unsupported data must not become reset inventory: %s", body)
		}
	}
	for _, n := range []int{200, 201} {
		grants := strings.Repeat(`{"resets_left":0,"paused":false},`, n-1) + `{"resets_left":1,"paused":false}`
		got := parseClaudeResetRemaining([]byte(`{"cedar_ember":{"eligible":true,"grants":[`+grants+`]}}`), now)
		if (n == 200 && (got == nil || *got != 1)) || (n == 201 && got != nil) {
			t.Fatalf("grant record bound failed for %d records", n)
		}
	}
}

func TestClaudeResetInventoryRequestIdentity(t *testing.T) {
	headers := http.Header{"Authorization": {"Bearer synthetic-only-token"}, "User-Agent": {"claude-code/2.1.0"}, "Anthropic-Beta": {"oauth-2025-04-20"}, "Accept": {"application/json"}}
	calls := 0
	got := claudeResetRemaining(headers, func(url string, h http.Header) ([]byte, error) {
		calls++
		if url != claudeResetInventoryURL || h.Get("Authorization") != headers.Get("Authorization") || h.Get("Anthropic-Beta") != "oauth-2025-04-20" || h.Get("User-Agent") != "claude-cli/2.1.0 (external, cli)" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Type") != "application/json" || h.Get("Accept") != "application/json" {
			t.Fatal("inventory must retain OAuth identity with the opt-in URL and headers")
		}
		return []byte(`{"cedar_ember":{"eligible":true,"grants":[{"resets_left":2,"paused":false}]}}`), nil
	})
	if calls != 1 || got == nil || *got != 2 || headers.Get("User-Agent") != "claude-code/2.1.0" || headers.Get("Cache-Control") != "" || headers.Get("Content-Type") != "" {
		t.Fatal("inventory request changed ordinary usage headers or count")
	}
}

func TestClaudeResetInventorySingleHostGET(t *testing.T) {
	for _, tc := range []struct {
		name string
		data any
		err  error
		ok   bool
	}{
		{"success", map[string]any{"StatusCode": 200, "Body": base64.StdEncoding.EncodeToString([]byte(`{"cedar_ember":null}`))}, nil, true},
		{"plain body", map[string]any{"status_code": 200, "body": `{"cedar_ember":null}`}, nil, true},
		{"unsupported", map[string]any{"StatusCode": 400}, nil, false},
		{"unauthorized", map[string]any{"StatusCode": 401}, nil, false},
		{"forbidden", map[string]any{"StatusCode": 403}, nil, false},
		{"rate limited", map[string]any{"StatusCode": 429}, nil, false},
		{"server failure", map[string]any{"StatusCode": 503}, nil, false},
		{"redirect", map[string]any{"StatusCode": 302, "Location": "https://example.com"}, nil, false},
		{"callback failure", nil, fmt.Errorf("synthetic-private-token timeout"), false},
		{"malformed host reply", []any{}, nil, false},
		{"oversized", map[string]any{"StatusCode": 200, "Body": strings.Repeat("x", 700000)}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			body, err := safeProviderGETWithRPC(claudeResetInventoryURL, nil, func(method string, payload any) (any, error) {
				calls++
				r := payload.(map[string]any)
				if method != "host.http.do" || r["Method"] != "GET" || r["URL"] != claudeResetInventoryURL || r["Body"] != nil {
					t.Fatal("only the fixed read-only inventory GET is permitted")
				}
				// hostRPC JSON-decodes callback data, including numeric status codes.
				encoded, marshalErr := json.Marshal(tc.data)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				var decoded any
				if decodeErr := json.Unmarshal(encoded, &decoded); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				return decoded, tc.err
			})
			if calls != 1 || (err == nil) != tc.ok {
				t.Fatalf("inventory must stop after one host request: calls=%d error=%v", calls, err)
			}
			if tc.ok && string(body) != `{"cedar_ember":null}` {
				t.Fatal("host body decoding changed")
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-private-token") {
				t.Fatal("host error leaked credential text")
			}
		})
	}
}

func TestCodexQuotaRefreshKeepsResetCreditContract(t *testing.T) {
	for _, embedded := range []bool{true, false} {
		e := newEngine()
		e.dir = t.TempDir()
		calls := 0
		rpc := func(string, any) (any, error) {
			return map[string]any{"access_token": "synthetic-codex-token", "chatgpt_account_id": "synthetic-account"}, nil
		}
		ok := e.refreshAccountQuotaWith(AuthEntry{ID: "codex", Index: "index", Provider: "codex"}, rpc, func(url string, headers http.Header) ([]byte, error) {
			calls++
			if headers.Get("User-Agent") != "codex-cli/0.1" || headers.Get("Authorization") != "Bearer synthetic-codex-token" || headers.Get("Chatgpt-Account-Id") != "synthetic-account" {
				t.Fatal("Codex authentication changed")
			}
			if calls == 1 {
				if url != "https://chatgpt.com/backend-api/wham/usage" {
					t.Fatal("wrong ordinary Codex URL")
				}
				credits := ""
				if embedded {
					credits = `,"rate_limit_reset_credits":{"available_count":2}`
				}
				return []byte(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000}}` + credits + `}`), nil
			}
			if embedded || calls != 2 || url != codexResetCreditsURL {
				t.Fatal("Codex must not query Claude inventory or repeat reads")
			}
			return []byte(`{"available_count":2}`), nil
		})
		q := e.quotas["codex"]
		wantCalls := 2
		if embedded {
			wantCalls = 1
		}
		if !ok || calls != wantCalls || q.Five == nil || q.Five.Used != 20 || q.ManualResetRemaining == nil || *q.ManualResetRemaining != 2 {
			t.Fatal("ordinary Codex usage and reset fallback must remain intact")
		}
	}
}

func TestClaudeQuotaInventoryRefreshIsolation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inventory  string
		failure    error
		usage      string
		usageError bool
		want       int64 // -1 means unknown.
	}{
		{"available", `{"cedar_ember":{"eligible":true,"grants":[{"id":"private-grant-handle","label":"private-grant-label","resets_left":2,"paused":false,"usable_now":false}]}}`, nil, "", false, 2},
		{"known zero", `{"cedar_ember":{"eligible":true,"grants":[{"resets_left":0,"paused":false}]}}`, nil, "", false, 0},
		{"no opt-in support", "", fmt.Errorf("HTTP 400"), "", false, -1},
		{"forbidden", "", fmt.Errorf("HTTP 403"), "", false, -1},
		{"rate limited", "", fmt.Errorf("HTTP 429"), "", false, -1},
		{"timeout", "", fmt.Errorf("Bearer synthetic-only-token timeout"), "", false, -1},
		{"ineligible", `{"cedar_ember":{"eligible":false,"grants":[]}}`, nil, "", false, -1},
		{"missing inventory", `{}`, nil, "", false, -1},
		{"malformed inventory", `{"cedar_ember":{"eligible":true,"grants":[{}]}}`, nil, "", false, -1},
		{"invalid JSON", `broken`, nil, "", false, -1},
		{"ordinary read failure", "", nil, "", true, -1},
		{"ordinary schema failure", "", nil, `{}`, false, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			oldCount := int64(8)
			e.quotas["account"] = AccountQuota{AuthID: "account", Provider: "claude", ManualResetRemaining: &oldCount, Five: &QuotaWindow{Used: 70}}
			e.accountRefresh["account"] = AccountRefreshState{Error: "prior error"}
			calls := 0
			rpc := func(method string, payload any) (any, error) {
				if method != "host.auth.get" || payload.(map[string]string)["auth_index"] != "index" {
					t.Fatal("unexpected credential lookup")
				}
				return map[string]any{"access_token": "synthetic-only-token"}, nil
			}
			ok := e.refreshAccountQuotaWith(AuthEntry{ID: "account", Index: "index", Provider: "claude", Name: "test", Disabled: true}, rpc, func(url string, headers http.Header) ([]byte, error) {
				calls++
				if headers.Get("Authorization") != "Bearer synthetic-only-token" {
					t.Fatal("wrong credential")
				}
				if calls == 1 {
					if url != claudeUsageURL || headers.Get("User-Agent") != "claude-code/2.1.0" {
						t.Fatal("ordinary usage request must remain first and unchanged")
					}
					if tc.usageError {
						return nil, fmt.Errorf("synthetic-only-token unavailable")
					}
					if tc.usage != "" {
						return []byte(tc.usage), nil
					}
					return []byte(`{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"manual_reset_remaining":99,"extra_usage":{"used_credits":500}}`), nil
				}
				if calls != 2 || url != claudeResetInventoryURL || headers.Get("User-Agent") != "claude-cli/2.1.0 (external, cli)" {
					t.Fatal("only one optional inventory request may follow ordinary usage")
				}
				return []byte(tc.inventory), tc.failure
			})
			q := e.quotas["account"]
			if tc.usageError || tc.usage != "" {
				if ok || calls != 1 || q.Five.Used != 70 || q.Error == "" {
					t.Fatal("ordinary failures must keep prior usage and skip optional inventory")
				}
			} else {
				if !ok || calls != 2 || q.Five == nil || q.Five.Used != 12 || q.Week == nil || q.Week.Used != 34 || q.Error != "" || q.FetchedAt == "" || !q.Disabled || e.accountRefresh["account"].Error != "" {
					t.Fatal("optional inventory must not break ordinary usage or account state")
				}
				if (tc.want < 0 && q.ManualResetRemaining != nil) || (tc.want >= 0 && (q.ManualResetRemaining == nil || *q.ManualResetRemaining != tc.want)) {
					t.Fatal("incorrect reset count or stale inventory was retained")
				}
				encoded, err := json.Marshal(q)
				if err != nil || strings.Contains(string(encoded), "manual_reset_remaining") != (tc.want >= 0) {
					t.Fatal("existing JSON omission/explicit-zero contract changed")
				}
			}
			saved, err := os.ReadFile(e.dir + "/quotas.json")
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"synthetic-only-token", "private-grant-handle", "private-grant-label", "cedar_ember"} {
				if strings.Contains(string(saved), secret) {
					t.Fatal("raw inventory or credential leaked into persisted quota data")
				}
			}
		})
	}
}
