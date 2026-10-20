package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const testRedeemKey = "0f8fad5b-d9cb-469f-a165-70867728950e"

func actionRPC(t *testing.T, files []any) func(string, any) (any, error) {
	t.Helper()
	return func(method string, payload any) (any, error) {
		switch method {
		case "host.auth.list":
			return map[string]any{"files": files}, nil
		case "host.auth.get":
			return map[string]any{"json": map[string]any{"access_token": "secret-token", "chatgpt_account_id": "acct-1", "email": "alice@example.com"}}, nil
		}
		t.Errorf("unexpected RPC %s", method)
		return nil, fmt.Errorf("unexpected")
	}
}

func codexFile(id string) map[string]any {
	return map[string]any{"id": id, "auth_index": "idx-" + id, "provider": "codex"}
}

func TestConsumeResetPostsOfficialRequestOnce(t *testing.T) {
	e := newEngine()
	three := int64(3)
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", ManualResetRemaining: &three}
	posts := 0
	post := func(url string, headers http.Header, body []byte, _ func(string, any) (any, error)) (int, []byte, error) {
		posts++
		if url != codexResetConsumeURL {
			t.Fatalf("url %s", url)
		}
		if headers.Get("Authorization") != "Bearer secret-token" || headers.Get("Chatgpt-Account-Id") != "acct-1" || headers.Get("Content-Type") != "application/json" {
			t.Fatalf("headers %v", headers)
		}
		var sent map[string]any
		if json.Unmarshal(body, &sent) != nil || len(sent) != 1 || sent["redeem_request_id"] != testRedeemKey {
			t.Fatalf("body %s", body)
		}
		return 200, []byte(`{"code":"reset","windows_reset":2}`), nil
	}
	status, result, failure := e.consumeResetWith("a", testRedeemKey, actionRPC(t, []any{codexFile("a")}), post)
	if status != 200 || failure != "" || result.Code != "reset" || result.WindowsReset != 2 || posts != 1 {
		t.Fatalf("got %d %+v %q posts=%d", status, result, failure, posts)
	}
	if got := *e.quotas["a"].ManualResetRemaining; got != 2 {
		t.Fatalf("remaining %d", got)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("token leaked")
	}
}

func TestConsumeResetRejectsUnsafeRequestsWithoutPosting(t *testing.T) {
	e := newEngine()
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	e.quotas["c"] = AccountQuota{AuthID: "c", Provider: "claude"}
	post := func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error) {
		t.Fatal("must not post")
		return 0, nil, nil
	}
	rpc := actionRPC(t, []any{codexFile("a")})
	cases := []struct {
		id, key, code string
		status        int
	}{
		{"", testRedeemKey, "invalid_auth_id", 400},
		{"a", "short", "invalid_redeem_request_id", 400},
		{"a", "bad key with spaces and more chars", "invalid_redeem_request_id", 400},
		{"missing", testRedeemKey, "account_not_found", 404},
		{"c", testRedeemKey, "reset_unsupported", 400},
	}
	for _, tc := range cases {
		status, _, failure := e.consumeResetWith(tc.id, tc.key, rpc, post)
		if status != tc.status || failure != tc.code {
			t.Errorf("%q/%q: %d %q", tc.id, tc.key, status, failure)
		}
	}
	// Known locally but no longer a Codex credential in CPA.
	status, _, failure := e.consumeResetWith("a", testRedeemKey, actionRPC(t, []any{}), post)
	if status != 404 || failure != "account_not_found" {
		t.Fatalf("stale account %d %q", status, failure)
	}
}

func TestConsumeResetAllowsOneInFlightPerAccount(t *testing.T) {
	e := newEngine()
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	e.quotas["b"] = AccountQuota{AuthID: "b", Provider: "codex"}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	post := func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error) {
		once.Do(func() { close(started) })
		<-release
		return 200, []byte(`{"code":"nothing_to_reset"}`), nil
	}
	rpc := actionRPC(t, []any{codexFile("a"), codexFile("b")})
	done := make(chan int)
	go func() { status, _, _ := e.consumeResetWith("a", testRedeemKey, rpc, post); done <- status }()
	<-started
	if status, _, failure := e.consumeResetWith("a", testRedeemKey, rpc, post); status != 409 || failure != "reset_busy" {
		t.Fatalf("busy %d %q", status, failure)
	}
	other := make(chan int)
	go func() { status, _, _ := e.consumeResetWith("b", testRedeemKey, rpc, post); other <- status }()
	close(release)
	if <-done != 200 || <-other != 200 {
		t.Fatal("consume did not finish")
	}
	if e.consuming["a"] || e.consuming["b"] {
		t.Fatal("in-flight state leaked")
	}
}

func TestConsumeResetMapsProviderFailures(t *testing.T) {
	e := newEngine()
	two := int64(2)
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", ManualResetRemaining: &two}
	rpc := actionRPC(t, []any{codexFile("a")})
	cases := []struct {
		status int
		body   string
		err    error
		want   string
	}{
		{0, "", fmt.Errorf("timeout access_token=secret"), "reset endpoint unavailable"},
		{429, `{"error":"rate"}`, nil, "reset HTTP 429"},
		{200, `{"code":"surprise"}`, nil, "reset response invalid"},
		{200, `not json`, nil, "reset response invalid"},
		{200, `{"code":"reset","windows_reset":-1}`, nil, "reset response invalid"},
	}
	for _, tc := range cases {
		post := func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error) {
			return tc.status, []byte(tc.body), tc.err
		}
		status, _, failure := e.consumeResetWith("a", testRedeemKey, rpc, post)
		if status != 502 || failure != tc.want {
			t.Errorf("%d %s: %d %q", tc.status, tc.body, status, failure)
		}
	}
	if *e.quotas["a"].ManualResetRemaining != 2 {
		t.Fatal("failed consume changed remaining count")
	}
	post := func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error) {
		return 200, []byte(`{"code":"no_credit","windows_reset":0}`), nil
	}
	if status, result, _ := e.consumeResetWith("a", testRedeemKey, rpc, post); status != 200 || result.Code != "no_credit" || *e.quotas["a"].ManualResetRemaining != 0 {
		t.Fatalf("no_credit %d %+v", status, result)
	}
	noCount := newEngine()
	noCount.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	reset := func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error) {
		return 200, []byte(`{"code":"reset","windows_reset":1}`), nil
	}
	if noCount.consumeResetWith("a", testRedeemKey, rpc, reset); noCount.quotas["a"].ManualResetRemaining != nil {
		t.Fatal("unknown count must stay unknown")
	}
}

func TestProviderPOSTUsesHostOnlyForConsumeURL(t *testing.T) {
	var sent map[string]any
	rpc := func(method string, payload any) (any, error) {
		if method != "host.http.do" {
			t.Fatalf("method %s", method)
		}
		sent = payload.(map[string]any)
		return map[string]any{"status_code": float64(200), "body": base64.StdEncoding.EncodeToString([]byte(`{"code":"reset"}`))}, nil
	}
	status, body, err := providerPOST(codexResetConsumeURL, http.Header{"A": {"b"}}, []byte(`{"x":1}`), rpc)
	if err != nil || status != 200 || string(body) != `{"code":"reset"}` {
		t.Fatalf("%d %s %v", status, body, err)
	}
	if sent["Method"] != "POST" || sent["URL"] != codexResetConsumeURL || string(sent["Body"].([]byte)) != `{"x":1}` {
		t.Fatalf("sent %v", sent)
	}
	encoded, _ := json.Marshal(sent)
	var wire struct {
		Method string
		URL    string
		Body   []byte
	}
	if json.Unmarshal(encoded, &wire) != nil || string(wire.Body) != `{"x":1}` {
		t.Fatalf("wire %s", encoded)
	}
	for _, url := range []string{codexResetCreditsURL, "https://example.com/consume", codexResetConsumeURL + "?x=1"} {
		if _, _, err := providerPOST(url, nil, nil, func(string, any) (any, error) { t.Fatal("must not call host"); return nil, nil }); err == nil {
			t.Errorf("%s permitted", url)
		}
	}
	failing := func(string, any) (any, error) { return nil, fmt.Errorf("network") }
	if _, _, err := providerPOST(codexResetConsumeURL, nil, nil, failing); err == nil {
		t.Fatal("host failure must not fall back")
	}
}

func TestDisabledStateFollowsCPA(t *testing.T) {
	files := []any{
		map[string]any{"id": "a", "auth_index": "1", "provider": "codex", "disabled": true},
		map[string]any{"id": "b", "auth_index": "2", "provider": "claude", "status": "disabled"},
		map[string]any{"id": "c", "auth_index": "3", "provider": "codex", "status": "active"},
	}
	entries := parseAuthList(map[string]any{"files": files})
	if len(entries) != 3 || !entries[0].Disabled || !entries[1].Disabled || entries[2].Disabled {
		t.Fatalf("entries %+v", entries)
	}
	e := newEngine()
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	e.quotas["c"] = AccountQuota{AuthID: "c", Provider: "codex", Disabled: true}
	e.syncAuthStatus(func(string, any) (any, error) { return map[string]any{"files": files}, nil })
	views := map[string]QuotaView{}
	for _, q := range e.snapshot().Quotas {
		views[q.AuthID] = q
	}
	if !views["a"].Disabled || views["c"].Disabled {
		t.Fatalf("views %+v", views)
	}
	if _, ok := views["b"]; ok {
		t.Fatal("sync must not invent quota accounts")
	}
	// Host failure or an empty list keeps the last known state.
	e.syncAuthStatus(func(string, any) (any, error) { return nil, fmt.Errorf("down") })
	e.syncAuthStatus(func(string, any) (any, error) { return map[string]any{"files": []any{}}, nil })
	if !e.quotas["a"].Disabled {
		t.Fatal("state cleared on failure")
	}
	encoded, _ := json.Marshal(e.snapshot())
	if !strings.Contains(string(encoded), `"disabled":true`) {
		t.Fatal("snapshot missing disabled flag")
	}
}

func TestQuotaReadKeepsDisabledFlag(t *testing.T) {
	e := newEngine()
	e.recordQuotaError(AuthEntry{ID: "a", Index: "1", Provider: "codex", Name: "A", Disabled: true}, "Quota endpoint unavailable")
	if !e.quotas["a"].Disabled {
		t.Fatal("error path lost disabled")
	}
	e.recordQuotaError(AuthEntry{ID: "a", Index: "1", Provider: "codex", Name: "A"}, "Quota endpoint unavailable")
	if e.quotas["a"].Disabled {
		t.Fatal("re-enabled account still disabled")
	}
}
