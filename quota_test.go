package main

import (
	"fmt"
	"net/http"
	"testing"
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
	for _, url := range []string{codexResetCreditsURL + "/consume", "https://example.com/wham/rate-limit-reset-credits"} {
		if _, err := safeProviderGET(url, nil); err == nil {
			t.Fatalf("endpoint must be rejected: %s", url)
		}
	}
}
