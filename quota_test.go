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
			got, diagnostic := parseClaudeResetRemaining(body, now)
			if got == nil || *got != tc.want || diagnostic != "" {
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
		if got, diagnostic := parseClaudeResetRemaining([]byte(`{"cedar_ember":`+block+`}`), now); got != nil || diagnostic == "" {
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
		if got, diagnostic := parseClaudeResetRemaining([]byte(body), now); got != nil || diagnostic != manualResetInvalidResponse {
			t.Fatalf("malformed inventory must stay unknown: %s", grant)
		}
	}
	for _, body := range []string{
		`not JSON`, `{}`, `null`, `[]`,
		`{"manual_reset_remaining":9,"rate_limit_reset_credits":{"available_count":3},"reset_bank":{"remaining":4}}`,
		`{"five_hour":{"utilization":30,"resets_at":"2026-01-03T00:00:00Z"},"extra_usage":{"used_credits":1,"monthly_limit":1000},"credits":{"balance":9}}`,
		`{"cedar_ember":{"eligible":true,"grants":[{"resets_left":26,"paused":false},{"resets_left":25,"paused":false}]}}`,
	} {
		if got, diagnostic := parseClaudeResetRemaining([]byte(body), now); got != nil || diagnostic == "" {
			t.Fatalf("unsupported data must not become reset inventory: %s", body)
		}
	}
	for _, n := range []int{200, 201} {
		grants := strings.Repeat(`{"resets_left":0,"paused":false},`, n-1) + `{"resets_left":1,"paused":false}`
		got, diagnostic := parseClaudeResetRemaining([]byte(`{"cedar_ember":{"eligible":true,"grants":[`+grants+`]}}`), now)
		if (n == 200 && (got == nil || *got != 1 || diagnostic != "")) || (n == 201 && (got != nil || diagnostic != manualResetInvalidResponse)) {
			t.Fatalf("grant record bound failed for %d records", n)
		}
	}
}

func TestClaudeResetInventoryRequestIdentity(t *testing.T) {
	headers := http.Header{"Authorization": {"Bearer synthetic-only-token"}, "User-Agent": {"claude-code/2.1.0"}, "Anthropic-Beta": {"oauth-2025-04-20"}, "Accept": {"application/json"}}
	calls := 0
	got, diagnostic := claudeResetRemaining(headers, func(url string, h http.Header) ([]byte, error) {
		calls++
		if url != claudeResetInventoryURL || h.Get("Authorization") != headers.Get("Authorization") || h.Get("Anthropic-Beta") != "oauth-2025-04-20" || h.Get("User-Agent") != "claude-cli/2.1.331 (external, cli)" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Type") != "application/json" || h.Get("Accept") != "application/json" {
			t.Fatal("inventory must retain OAuth identity with the opt-in URL and headers")
		}
		return []byte(`{"cedar_ember":{"eligible":true,"grants":[{"resets_left":2,"paused":false}]}}`), nil
	})
	if calls != 1 || got == nil || *got != 2 || diagnostic != "" || headers.Get("User-Agent") != "claude-code/2.1.0" || headers.Get("Cache-Control") != "" || headers.Get("Content-Type") != "" {
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
		if !ok || calls != wantCalls || q.Five == nil || q.Five.Used != 20 || q.ManualResetRemaining == nil || *q.ManualResetRemaining != 2 || q.ManualResetDiagnostic != "" {
			t.Fatal("ordinary Codex usage and reset fallback must remain intact")
		}
		encoded, err := json.Marshal(e.snapshot())
		if err != nil || strings.Contains(string(encoded), "manual_reset_diagnostic") {
			t.Fatal("Claude inventory diagnostics must not appear for Codex")
		}
	}
}

func TestClaudeQuotaInventoryRefreshIsolation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		combined    string
		combinedErr error
		plain       string
		wantCalls   int
		wantOK      bool
		wantErr     string
		want        int64 // -1 means unknown.
		diagnostic  string
	}{
		{"available single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"manual_reset_remaining":99,"extra_usage":{"used_credits":500},"cedar_ember":{"eligible":true,"grants":[{"id":"private-grant-handle","label":"private-grant-label","resets_left":2,"paused":false,"usable_now":false}]}}`, nil, "", 1, true, "", 2, ""},
		{"known zero single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"cedar_ember":{"eligible":true,"grants":[{"resets_left":0,"paused":false}]}}`, nil, "", 1, true, "", 0, ""},
		{"old CLI single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"cedar_ember":{"eligible":false,"ineligible_reason":"cli_version"}}`, nil, "", 1, true, "", -1, "cli_version"},
		{"wrong surface single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"cedar_ember":{"eligible":false,"ineligible_reason":"surface"}}`, nil, "", 1, true, "", -1, "surface"},
		{"ineligible single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"cedar_ember":{"eligible":false,"grants":[]}}`, nil, "", 1, true, "", -1, "not_eligible"},
		{"private reason single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"cedar_ember":{"eligible":false,"ineligible_reason":"synthetic-only-token private-grant-handle private-grant-label"}}`, nil, "", 1, true, "", -1, "not_eligible"},
		{"missing inventory single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`, nil, "", 1, true, "", -1, "unavailable"},
		{"malformed inventory single request", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},"cedar_ember":{"eligible":true,"grants":[{}]}}`, nil, "", 1, true, "", -1, "invalid_response"},
		{"no opt-in support falls back once to plain", "", &quotaHTTPError{StatusCode: 400}, `{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`, 2, true, "", -1, "request_failed"},
		{"upgrade required falls back once to plain", "", &quotaHTTPError{StatusCode: 426}, `{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`, 2, true, "", -1, "request_failed"},
		{"cedar-only response falls back once to plain", `{"cedar_ember":{"eligible":true,"grants":[]}}`, nil, `{"five_hour":{"utilization":12},"seven_day":{"utilization":34}}`, 2, true, "", -1, "request_failed"},
		{"rate limited stops after one request", "", &quotaHTTPError{StatusCode: 429, RetryAfter: 45 * time.Minute}, "", 1, false, quotaErrRateLimited, -1, ""},
		{"forbidden stops after one request", "", &quotaHTTPError{StatusCode: 403}, "", 1, false, "Quota endpoint unavailable", -1, ""},
		{"timeout stops after one request", "", fmt.Errorf("Bearer synthetic-only-token timeout"), "", 1, false, "Quota endpoint unavailable", -1, ""},
		{"invalid JSON stops after one request", `broken`, nil, "", 1, false, "Quota response was not recognized", -1, ""},
		{"empty JSON stops after one request", `{}`, nil, "", 1, false, "Quota response was not recognized", -1, ""},
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
					if url != claudeResetInventoryURL || headers.Get("User-Agent") != "claude-cli/2.1.331 (external, cli)" {
						t.Fatalf("combined usage+reset request must run first: url=%s ua=%s", url, headers.Get("User-Agent"))
					}
					return []byte(tc.combined), tc.combinedErr
				}
				if calls != 2 || url != claudeUsageURL || headers.Get("User-Agent") != "claude-code/2.1.0" {
					t.Fatalf("unexpected fallback call #%d: url=%s ua=%s", calls, url, headers.Get("User-Agent"))
				}
				return []byte(tc.plain), nil
			})
			q := e.quotas["account"]
			if ok != tc.wantOK || calls != tc.wantCalls {
				t.Fatalf("ok=%v (want %v) calls=%d (want %d) quota=%+v", ok, tc.wantOK, calls, tc.wantCalls, q)
			}
			if !tc.wantOK {
				if q.Five.Used != 70 || q.Error != tc.wantErr || q.ManualResetRemaining == nil || *q.ManualResetRemaining != oldCount || q.ManualResetDiagnostic != "" {
					t.Fatalf("failed refresh must keep prior usage and report %q: %+v", tc.wantErr, q)
				}
			} else {
				if q.Five == nil || q.Five.Used != 12 || q.Week == nil || q.Week.Used != 34 || q.Error != "" || q.FetchedAt == "" || !q.Disabled || e.accountRefresh["account"].Error != "" {
					t.Fatalf("successful refresh produced unexpected quota state: %+v", q)
				}
				if (tc.want < 0 && q.ManualResetRemaining != nil) || (tc.want >= 0 && (q.ManualResetRemaining == nil || *q.ManualResetRemaining != tc.want)) || q.ManualResetDiagnostic != tc.diagnostic {
					t.Fatalf("incorrect reset count or diagnostic: got count=%v diag=%q want=%d/%q", q.ManualResetRemaining, q.ManualResetDiagnostic, tc.want, tc.diagnostic)
				}
				encoded, err := json.Marshal(q)
				if err != nil || strings.Contains(string(encoded), "manual_reset_remaining") != (tc.want >= 0) || strings.Contains(string(encoded), "manual_reset_diagnostic") != (tc.diagnostic != "") {
					t.Fatal("count/diagnostic JSON omission contract changed")
				}
			}
			saved, err := os.ReadFile(e.dir + "/quotas.json")
			if err != nil {
				t.Fatal(err)
			}
			restored := newEngine()
			restored.dir = e.dir
			restored.loadPersistence()
			if restored.storageReadFailed || restored.quotas["account"].ManualResetDiagnostic != tc.diagnostic || restored.snapshot().Quotas[0].ManualResetDiagnostic != tc.diagnostic {
				t.Fatal("diagnostic must survive persistence and reach the snapshot unchanged")
			}
			encodedSnapshot, err := json.Marshal(restored.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"synthetic-only-token", "private-grant-handle", "private-grant-label", "cedar_ember", "ineligible_reason", "HTTP 400", "HTTP 403", "HTTP 429"} {
				if strings.Contains(string(saved), secret) || strings.Contains(string(encodedSnapshot), secret) {
					t.Fatal("raw inventory, reason, HTTP status or credential leaked into persisted/API data")
				}
			}
		})
	}
}

func TestClaudeRateLimitBackoffFallbackCacheAndBackgroundCadence(t *testing.T) {
	// 1. Retry-After header parsing from host RPC (seconds and HTTP-date).
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfterDuration("120", now); got != 2*time.Minute {
		t.Fatalf("expected 2m from seconds, got %v", got)
	}
	if got := parseRetryAfterDuration(now.Add(45*time.Minute).Format(http.TimeFormat), now); got != 45*time.Minute {
		t.Fatalf("expected 45m from HTTP-date, got %v", got)
	}
	_, err := safeProviderGETWithRPC(claudeResetInventoryURL, nil, func(string, any) (any, error) {
		return map[string]any{
			"StatusCode": float64(429),
			"Headers":    map[string]any{"Retry-After": []any{"2700"}},
		}, nil
	})
	if ra, ok := isQuotaRateLimitedErr(err); !ok || ra != 45*time.Minute {
		t.Fatalf("expected 429 with 45m Retry-After, got ok=%v ra=%v err=%v", ok, ra, err)
	}

	// 2. Exponential 429 backoff (30m -> 60m -> 120m) honors larger Retry-After and clears on success.
	e := newEngine()
	e.dir = t.TempDir()
	entry := AuthEntry{ID: "cl-1", Index: "i1", Provider: "claude", Name: "cl@example.com"}
	authRPC := func(string, any) (any, error) {
		return map[string]any{"access_token": "tok"}, nil
	}
	calls := 0
	if ok := e.refreshAccountQuotaWith(entry, authRPC, func(string, http.Header) ([]byte, error) {
		calls++
		return nil, &quotaHTTPError{StatusCode: 429, RetryAfter: 45 * time.Minute}
	}); ok || calls != 1 {
		t.Fatalf("429 must fail after 1 call, ok=%v calls=%d", ok, calls)
	}
	b1 := e.quotaBackoff["cl-1"]
	if b1.Count != 1 || time.Until(b1.Until) < 44*time.Minute {
		t.Fatalf("first 429 must honor 45m Retry-After: %+v", b1)
	}
	e.refreshAccountQuotaWith(entry, authRPC, func(string, http.Header) ([]byte, error) {
		return nil, &quotaHTTPError{StatusCode: 429}
	})
	if b2 := e.quotaBackoff["cl-1"]; b2.Count != 2 || time.Until(b2.Until) < 59*time.Minute {
		t.Fatalf("second 429 must escalate to 60m: %+v", b2)
	}
	e.refreshAccountQuotaWith(entry, authRPC, func(string, http.Header) ([]byte, error) {
		return nil, &quotaHTTPError{StatusCode: 429}
	})
	if b3 := e.quotaBackoff["cl-1"]; b3.Count != 3 || time.Until(b3.Until) < 119*time.Minute {
		t.Fatalf("third 429 must escalate to 120m: %+v", b3)
	}
	if ok := e.refreshAccountQuotaWith(entry, authRPC, func(string, http.Header) ([]byte, error) {
		return []byte(`{"five_hour":{"utilization":5},"cedar_ember":{"eligible":true,"grants":[]}}`), nil
	}); !ok {
		t.Fatal("expected manual recovery refresh to succeed")
	}
	if _, backedOff := e.quotaBackoff["cl-1"]; backedOff {
		t.Fatal("successful refresh must clear 429 backoff")
	}

	// 3. Non-429 incompatibility caches plain fallback for 6h so the next refresh sends only 1 plain request.
	e2 := newEngine()
	e2.dir = t.TempDir()
	var urls []string
	if ok := e2.refreshAccountQuotaWith(entry, authRPC, func(url string, _ http.Header) ([]byte, error) {
		urls = append(urls, url)
		if url == claudeResetInventoryURL {
			return nil, &quotaHTTPError{StatusCode: 426}
		}
		return []byte(`{"five_hour":{"utilization":10}}`), nil
	}); !ok || len(urls) != 2 {
		t.Fatalf("expected single compatibility fallback on 426: ok=%v urls=%v", ok, urls)
	}
	urls = nil
	if ok := e2.refreshAccountQuotaWith(entry, authRPC, func(url string, _ http.Header) ([]byte, error) {
		urls = append(urls, url)
		return []byte(`{"five_hour":{"utilization":11}}`), nil
	}); !ok || len(urls) != 1 || urls[0] != claudeUsageURL {
		t.Fatalf("subsequent refresh within fallback TTL must make 1 plain call: ok=%v urls=%v", ok, urls)
	}

	// 4. Background refresh cadence: Claude 30m (skips fresh, disabled, and backed-off) without false errors;
	// Codex stays on 15m; force=true still refreshes all.
	e3 := newEngine()
	e3.dir = t.TempDir()
	freshAt := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339Nano)
	e3.quotas["cl-fresh"] = AccountQuota{AuthID: "cl-fresh", Provider: "claude", FetchedAt: freshAt, Five: &QuotaWindow{Used: 5}}
	e3.quotas["cl-backoff"] = AccountQuota{AuthID: "cl-backoff", Provider: "claude", FetchedAt: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano), Five: &QuotaWindow{Used: 5}}
	e3.quotaBackoff["cl-backoff"] = quotaBackoffState{Until: time.Now().UTC().Add(30 * time.Minute), Count: 1}
	e3.quotas["cx-due"] = AccountQuota{AuthID: "cx-due", Provider: "codex", FetchedAt: freshAt, Five: &QuotaWindow{Used: 5}}
	listRPC := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{
				map[string]any{"id": "cl-fresh", "auth_index": "i1", "provider": "claude", "email": "fresh@example.com"},
				map[string]any{"id": "cl-disabled", "auth_index": "i2", "provider": "claude", "email": "dis@example.com", "disabled": true},
				map[string]any{"id": "cl-backoff", "auth_index": "i3", "provider": "claude", "email": "bo@example.com"},
				map[string]any{"id": "cx-due", "auth_index": "i4", "provider": "codex", "email": "cx@example.com"},
			}}, nil
		}
		return nil, nil
	}
	var seen []string
	e3.refreshQuotaWith(false, listRPC, func(a AuthEntry) bool {
		seen = append(seen, a.ID)
		return true
	})
	if len(seen) != 1 || seen[0] != "cx-due" || e3.quotaError != "" {
		t.Fatalf("expected only cx-due on 15m background tick, got seen=%v err=%q", seen, e3.quotaError)
	}

	// If ALL supported accounts are Claude and cleanly skipped, quotaError must remain empty.
	e4 := newEngine()
	e4.dir = t.TempDir()
	e4.quotas["cl-fresh"] = AccountQuota{AuthID: "cl-fresh", Provider: "claude", FetchedAt: freshAt, Five: &QuotaWindow{Used: 5}}
	claudeOnlyRPC := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{
				map[string]any{"id": "cl-fresh", "auth_index": "i1", "provider": "claude", "email": "fresh@example.com"},
			}}, nil
		}
		return nil, nil
	}
	called := 0
	e4.refreshQuotaWith(false, claudeOnlyRPC, func(AuthEntry) bool { called++; return true })
	if called != 0 || e4.quotaError != "" {
		t.Fatalf("all-skipped Claude refresh must not set quotaError: called=%d err=%q", called, e4.quotaError)
	}
}

func TestRefreshAccountQuotaReconcilesExternalResetCooldown(t *testing.T) {
	future := time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339)

	// Case 1: External reset card reduced remaining credits while CPA still held routing cooldown.
	e := newEngine()
	e.dir = t.TempDir()
	three := int64(3)
	e.quotas["cx"] = AccountQuota{
		AuthID:               "cx",
		Provider:             "codex",
		Five:                 &QuotaWindow{Used: 80, ResetAt: future},
		Week:                 &QuotaWindow{Used: 40, ResetAt: future},
		ManualResetRemaining: &three,
	}
	e.routingCooldown["cx"] = routingCooldownState{Cooling: true, NextRetryAfter: time.Now().UTC().Add(2 * time.Hour)}
	e.wakeupLastFail["cx"] = time.Now().UTC()

	resetCalled := 0
	rpc := func(method string, payload any) (any, error) {
		switch method {
		case "host.auth.get":
			return map[string]any{"json": map[string]any{"access_token": "tok", "chatgpt_account_id": "acct"}}, nil
		case "host.routing.reset_cooldown":
			resetCalled++
			if payload.(map[string]string)["auth_index"] != "idx-cx" {
				t.Fatalf("unexpected auth_index: %#v", payload)
			}
			return map[string]any{"auth_index": "idx-cx", "models": []any{"gpt-5.5"}}, nil
		}
		t.Fatalf("unexpected RPC %s", method)
		return nil, nil
	}
	get := func(url string, _ http.Header) ([]byte, error) {
		return []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_after_seconds":500000}},"rate_limit_reset_credits":{"available_count":2}}`), nil
	}
	entry := AuthEntry{ID: "cx", Index: "idx-cx", Provider: "codex", Name: "cx@example.com", QuotaCooling: true, NextRetryAfter: future}
	if !e.refreshAccountQuotaWith(entry, rpc, get) {
		t.Fatal("expected refreshAccountQuotaWith to succeed")
	}
	if resetCalled != 1 {
		t.Fatalf("expected 1 host.routing.reset_cooldown reconciliation call, got %d", resetCalled)
	}
	if _, ok := e.routingCooldown["cx"]; ok {
		t.Fatal("expected local routingCooldown cleared after reconciliation")
	}
	if _, ok := e.wakeupLastFail["cx"]; ok {
		t.Fatal("expected wakeupLastFail cleared after reconciliation")
	}

	// Case 2: Previously 100%-exhausted window with future reset time recovered early (even without reset count).
	e2 := newEngine()
	e2.dir = t.TempDir()
	e2.quotas["cx2"] = AccountQuota{
		AuthID:   "cx2",
		Provider: "codex",
		Five:     &QuotaWindow{Used: 100, ResetAt: future},
		Week:     &QuotaWindow{Used: 50, ResetAt: future},
	}
	resetCalled = 0
	entry2 := AuthEntry{ID: "cx2", Index: "idx-cx", Provider: "codex", Name: "cx2@example.com", QuotaCooling: true, NextRetryAfter: future}
	if !e2.refreshAccountQuotaWith(entry2, rpc, get) {
		t.Fatal("expected refreshAccountQuotaWith to succeed")
	}
	if resetCalled != 1 {
		t.Fatalf("expected early recovery of 100%% window to trigger cooldown reset, got %d", resetCalled)
	}

	// Case 3: Short-term cooldown WITHOUT reset evidence (prior < 100%, remaining credits unchanged) must NOT clear CPA cooldown.
	e3 := newEngine()
	e3.dir = t.TempDir()
	two := int64(2)
	e3.quotas["cx3"] = AccountQuota{
		AuthID:               "cx3",
		Provider:             "codex",
		Five:                 &QuotaWindow{Used: 20, ResetAt: future},
		Week:                 &QuotaWindow{Used: 40, ResetAt: future},
		ManualResetRemaining: &two,
	}
	resetCalled = 0
	entry3 := AuthEntry{ID: "cx3", Index: "idx-cx", Provider: "codex", Name: "cx3@example.com", QuotaCooling: true, NextRetryAfter: future}
	if !e3.refreshAccountQuotaWith(entry3, rpc, get) {
		t.Fatal("expected refreshAccountQuotaWith to succeed")
	}
	if resetCalled != 0 {
		t.Fatalf("short-term cooldown without reset evidence must not clear CPA cooldown, got %d", resetCalled)
	}

	// Case 4: Still exhausted (7d = 100%) must NOT clear CPA cooldown even if 5h recovered.
	e4 := newEngine()
	e4.dir = t.TempDir()
	e4.quotas["cx4"] = AccountQuota{
		AuthID:               "cx4",
		Provider:             "codex",
		Five:                 &QuotaWindow{Used: 100, ResetAt: future},
		Week:                 &QuotaWindow{Used: 100, ResetAt: future},
		ManualResetRemaining: &three,
	}
	getStillExhausted := func(url string, _ http.Header) ([]byte, error) {
		return []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":500000}},"rate_limit_reset_credits":{"available_count":2}}`), nil
	}
	resetCalled = 0
	entry4 := AuthEntry{ID: "cx4", Index: "idx-cx", Provider: "codex", Name: "cx4@example.com", QuotaCooling: true, NextRetryAfter: future}
	if !e4.refreshAccountQuotaWith(entry4, rpc, getStillExhausted) {
		t.Fatal("expected refreshAccountQuotaWith to succeed")
	}
	if resetCalled != 0 {
		t.Fatalf("still-exhausted 7d window must not clear CPA cooldown, got %d", resetCalled)
	}
}
