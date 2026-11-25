package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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
	status, result, failure := e.consumeResetWith("a", testRedeemKey, "", "", actionRPC(t, []any{codexFile("a")}), post)
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
		{"c", testRedeemKey, "invalid_credit_id", 400},
	}
	for _, tc := range cases {
		status, _, failure := e.consumeResetWith(tc.id, tc.key, "", "", rpc, post)
		if status != tc.status || failure != tc.code {
			t.Errorf("%q/%q: %d %q", tc.id, tc.key, status, failure)
		}
	}
	// Known locally but no longer a Codex credential in CPA.
	status, _, failure := e.consumeResetWith("a", testRedeemKey, "", "", actionRPC(t, []any{}), post)
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
	go func() { status, _, _ := e.consumeResetWith("a", testRedeemKey, "", "", rpc, post); done <- status }()
	<-started
	if status, _, failure := e.consumeResetWith("a", testRedeemKey, "", "", rpc, post); status != 409 || failure != "reset_busy" {
		t.Fatalf("busy %d %q", status, failure)
	}
	other := make(chan int)
	go func() { status, _, _ := e.consumeResetWith("b", testRedeemKey, "", "", rpc, post); other <- status }()
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
		status, _, failure := e.consumeResetWith("a", testRedeemKey, "", "", rpc, post)
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
	if status, result, _ := e.consumeResetWith("a", testRedeemKey, "", "", rpc, post); status != 200 || result.Code != "no_credit" || *e.quotas["a"].ManualResetRemaining != 0 {
		t.Fatalf("no_credit %d %+v", status, result)
	}
	noCount := newEngine()
	noCount.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	reset := func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error) {
		return 200, []byte(`{"code":"reset","windows_reset":1}`), nil
	}
	if noCount.consumeResetWith("a", testRedeemKey, "", "", rpc, reset); noCount.quotas["a"].ManualResetRemaining != nil {
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

const testOrg = "12345678-abcd-4321-89ab-0123456789ab"

// Exercise production host-only GET and POST parsing; reject extra/replayed calls.
type claudeStep struct {
	url       string
	status    int
	body, org string
}

func claudeRPC(t *testing.T, steps ...claudeStep) func(string, any) (any, error) {
	t.Helper()
	f := codexFile("c")
	f["provider"] = "claude"
	auth := actionRPC(t, []any{f})
	t.Cleanup(func() {
		if len(steps) != 0 {
			t.Errorf("%d host requests missing", len(steps))
		}
	})
	return func(method string, payload any) (any, error) {
		if method != "host.http.do" {
			return auth(method, payload)
		}
		if len(steps) == 0 {
			t.Fatal("unexpected host request")
		}
		s := steps[0]
		steps = steps[1:]
		r := payload.(map[string]any)
		h := r["Headers"].(http.Header)
		if r["URL"] != s.url || h.Get("Authorization") != "Bearer secret-token" || h.Get("Anthropic-Beta") != "oauth-2025-04-20" || h.Get("User-Agent") != "claude-cli/2.1.331 (external, cli)" || h.Get("Content-Type") != "application/json" || h.Get("Accept") != "application/json" || h.Get("Cache-Control") != "no-store" {
			t.Fatal("wrong endpoint or OAuth request headers")
		}
		if s.url == claudeProfileURL || s.url == claudeResetInventoryURL {
			if r["Method"] != "GET" || r["Body"] != nil {
				t.Fatal("read-only GET required")
			}
		} else {
			var sent map[string]string
			if r["Method"] != "POST" || json.Unmarshal(r["Body"].([]byte), &sent) != nil || len(sent) != 3 || sent["program"] != "cedar_ember" || sent["grant_id"] != "chosen" || sent["request_id"] != testRedeemKey {
				t.Fatal("wrong redemption body")
			}
		}
		if s.status == 0 {
			return nil, fmt.Errorf("private transport detail")
		}
		headers := map[string]any{}
		if s.org != "" {
			headers["anthropic-organization-id"] = []any{s.org}
		}
		return map[string]any{"StatusCode": float64(s.status), "Headers": headers, "Body": base64.StdEncoding.EncodeToString([]byte(s.body))}, nil
	}
}

func TestListResetClaudeInventory(t *testing.T) {
	body := `{"cedar_ember":{"eligible":true,"next_grant_id":"next","grants":[
	{"id":"forever","resets_left":2,"paused":false,"type":"weekly","label":"private-note"},
	{"id":"blocked","resets_left":1,"paused":false,"ends_at":"2099-05-02T00:00:00Z","blocking":["seven_day_sonnet"]},
	{"id":"usable","resets_left":1,"paused":false,"ends_at":"2099-05-02T00:00:00Z","clears":["five_hour"],"usable_now":true},
	{"id":"next","resets_left":1,"paused":false,"ends_at":"2099-05-02T00:00:00Z","gives_back_weekly":true,"unavailable_reason":"cannot_clear"},
	{"id":"first","resets_left":1,"paused":false,"ends_at":"2099-05-01T23:59:59.999Z","clears":["seven_day_overage_included"]},
	{"id":"paused","resets_left":1,"paused":true}, {"id":"future","resets_left":1,"paused":false,"starts_at":"2099-05-01T00:00:00Z"},
	{"id":"expired","resets_left":1,"paused":false,"ends_at":"2001-01-01T00:00:00Z"}, {"id":"empty","resets_left":0,"paused":false}]}}`
	e := newEngine()
	e.dir = t.TempDir()
	e.quotas["c"] = AccountQuota{Provider: "claude", AuthID: "c", ManualResetDiagnostic: "surface"}
	status, list, failure := e.listResetCreditsWith("c", claudeRPC(t, claudeStep{claudeResetInventoryURL, 200, body, strings.ToUpper(testOrg)}), nil)
	if status != 200 || failure != "" || list.AvailableCount != 6 || len(list.Credits) != 5 || e.resetBindings[list.ConfirmationToken].org != testOrg || !confirmationTokenPattern.MatchString(list.ConfirmationToken) || *e.quotas["c"].ManualResetRemaining != 6 || e.quotas["c"].ManualResetDiagnostic != "" {
		t.Fatal("list or cache mismatch")
	}
	for i, want := range []struct {
		id, title, reason string
		left              int64
	}{{"first", "Weekly reset", "needs_limit", 1}, {"next", "Fable reset", "cannot_clear", 1}, {"usable", "5-hour reset", "", 1}, {"blocked", "Full reset", "blocked_sonnet", 1}, {"forever", "Weekly reset", "needs_limit", 2}} {
		got := list.Credits[i]
		if got.ID != want.id || got.Title != want.title || got.Description == "" || got.UnavailableReason != want.reason || *got.UsableNow != (want.reason == "") || *got.Remaining != want.left {
			t.Errorf("unexpected credit %d: %+v", i, got)
		}
	}
	if list.Credits[4].ExpiresAt != "" || list.Credits[0].ExpiresAt != "2099-05-01T23:59:59.999Z" {
		t.Fatal("wrong expiry")
	}
	out, _ := json.Marshal(list)
	snapshot, _ := json.Marshal(e.snapshot())
	if strings.Contains(string(snapshot), "forever") || strings.Contains(string(out)+string(snapshot), testOrg) || strings.Contains(string(out)+string(snapshot), "private-note") || strings.Contains(string(out), "secret-token") {
		t.Fatal("private response data leaked")
	}
	e.mu.Lock()
	e.pruneQuotasLocked(map[string]bool{})
	e.mu.Unlock()
	if len(e.resetBindings) != 0 {
		t.Fatal("removed account retained organization")
	}
}

func TestListResetClaudeRejectsInvalidAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		status, want int
		body, msg    string
	}{
		{401, 422, "secret", "reset_auth_failed"}, {403, 422, "secret", "reset_auth_failed"}, {429, 429, "secret", "reset_rate_limited"}, {500, 502, "secret", "reset credits unavailable"}, {0, 502, "", "reset credits unavailable"},
		{200, 502, `{}`, "reset credits unavailable"}, {200, 502, `{"cedar_ember":null}`, "reset credits unavailable"}, {200, 502, `not JSON`, "reset credits invalid"},
		{200, 502, `{"cedar_ember":{"eligible":false,"ineligible_reason":"cli_version"}}`, "claude_reset_cli_version"}, {200, 502, `{"cedar_ember":{"eligible":false,"ineligible_reason":"surface"}}`, "claude_reset_surface"}, {200, 502, `{"cedar_ember":{"eligible":false,"ineligible_reason":"private"}}`, "claude_reset_not_eligible"},
		{200, 502, `{"cedar_ember":{"eligible":true,"grants":[{"id":"a","resets_left":1,"paused":false},{"id":"a","resets_left":1,"paused":true}]}}`, "reset credits invalid"},
		{200, 502, `{"cedar_ember":{"eligible":true,"grants":[{"id":"A","resets_left":1,"paused":false}]}}`, "reset credits invalid"},
		{200, 502, `{"cedar_ember":{"eligible":true,"grants":[{"id":"a","resets_left":51,"paused":false}]}}`, "reset credits invalid"},
	} {
		e := newEngine()
		e.quotas["c"] = AccountQuota{Provider: "claude"}
		s, _, msg := e.listResetCreditsWith("c", claudeRPC(t, claudeStep{claudeResetInventoryURL, tc.status, tc.body, ""}), nil)
		if s != tc.want || msg != tc.msg {
			t.Errorf("got %d %q, want %d %q", s, msg, tc.want, tc.msg)
		}
	}
}

func TestClaudeResetDerivedReasons(t *testing.T) {
	for _, tc := range []struct{ meta, grant, want string }{
		{`"at_limit":false`, `"unavailable_reason":"private"`, "needs_limit"}, {`"at_limit":true`, `"unavailable_reason":"private"`, "unavailable"}, {`"at_limit":false`, `"use_requires_limit":false`, "unavailable"},
		{`"cooldown_until":"2099-01-01T00:00:00Z"`, `"blocking":["five_hour"]`, "cooldown"}, {`"cooldown_until":"2099-01-01T00:00:00Z"`, `"unavailable_reason":"cannot_clear"`, "cannot_clear"},
		{`"at_limit":true`, `"blocking":["five_hour","seven_day"]`, "blocked_five_hour"}, {`"at_limit":true`, `"blocking":["seven_day"]`, "blocked_seven_day"}, {`"at_limit":true`, `"blocking":["seven_day_opus"]`, "blocked_opus"}, {`"at_limit":true`, `"blocking":["private"]`, "blocked_other"},
	} {
		body := `{"cedar_ember":{"eligible":true,` + tc.meta + `,"grants":[{"id":"a","paused":false,"resets_left":1,` + tc.grant + `}]}}`
		list, msg := parseClaudeResetCredits([]byte(body), time.Now())
		if msg != "" || list.Credits[0].UnavailableReason != tc.want {
			t.Errorf("want %s", tc.want)
		}
	}
}

const testConfirmation = "synthetic_confirmation_token"

func seedResetBinding(e *Engine, id string) {
	e.resetBindings[testConfirmation] = resetBinding{authID: id, org: testOrg, usableCredits: map[string]bool{"chosen": true}, createdAt: time.Now()}
}

func TestConsumeResetClaudeOutcomes(t *testing.T) {
	profile := claudeStep{claudeProfileURL, 200, `{"organization":{"uuid":"` + strings.ToUpper(testOrg) + `"}}`, ""}
	url := "https://api.anthropic.com/api/organizations/" + testOrg + "/reset_rate_limits"
	for _, tc := range []struct {
		up, want             int
		body, org, code, msg string
	}{
		{200, 200, `{"result":"reset","cleared":["five_hour","seven_day"]}`, strings.ToUpper(testOrg), "reset", ""},
		{200, 200, `{"result":"already_used"}`, "", "already_redeemed", ""}, {200, 200, `{"result":"not_limited"}`, "", "nothing_to_reset", ""}, {200, 200, `{"result":"cooldown"}`, "", "cooldown", ""},
		{200, 200, `{"result":"ineligible","reason":"not_next_grant"}`, "", "not_next_grant", ""}, {200, 200, `{"result":"ineligible","reason":"private"}`, "", "no_credit", ""},
		{401, 422, "private", "", "", "reset_auth_failed"}, {403, 422, "private", "", "", "reset_auth_failed"}, {429, 429, "private", "", "", "reset_rate_limited"}, {503, 502, "private", "", "", "reset HTTP 503"},
		{0, 502, "", "", "", "reset_unconfirmed"}, {200, 502, `{"result":"unavailable"}`, "", "", "reset_unconfirmed"}, {200, 502, `{"result":"reset"}`, testRedeemKey, "", "reset_unconfirmed"}, {200, 502, `{"result":"reset"}`, "private", "", "reset_unconfirmed"},
		{200, 502, `{"result":"private"}`, "", "", "reset response invalid"}, {200, 502, `{"result":"reset","cleared":42}`, "", "", "reset response invalid"}, {200, 502, `private`, "", "", "reset response invalid"},
	} {
		e := newEngine()
		count := int64(2)
		e.quotas["c"] = AccountQuota{Provider: "claude", ManualResetRemaining: &count}
		seedResetBinding(e, "c")
		s, r, msg := e.consumeResetWith("c", testRedeemKey, "chosen", testConfirmation, claudeRPC(t, profile, claudeStep{url, tc.up, tc.body, tc.org}), providerPOST)
		if s != tc.want || msg != tc.msg || r.Code != tc.code {
			t.Errorf("upstream %d: got %d %+v %s", tc.up, s, r, msg)
		}
		if _, retained := e.resetBindings[testConfirmation]; retained != (tc.want != 200) {
			t.Fatalf("binding retention wrong after %d", s)
		}
		wantLeft := int64(2)
		if tc.code == "reset" {
			wantLeft--
			if r.WindowsReset != 2 {
				t.Fatal("wrong window count")
			}
		} else if tc.code != "" {
			out, _ := json.Marshal(r)
			if string(out) != `{"code":"`+tc.code+`"}` {
				t.Fatalf("unexpected success response: %s", out)
			}
		}
		if *e.quotas["c"].ManualResetRemaining != wantLeft || e.consuming["c"] {
			t.Fatal("wrong count or lock retained")
		}
	}
}

func TestConsumeResetClaudeGuardsBeforePost(t *testing.T) {
	for _, tc := range []struct {
		up, want  int
		body, msg string
	}{
		{0, 502, "", "reset organization unavailable"}, {401, 422, "", "reset_auth_failed"}, {403, 422, "", "reset_auth_failed"}, {429, 429, "", "reset_rate_limited"}, {500, 502, "", "reset organization unavailable"},
		{200, 502, `{"organization":{"uuid":" ` + testOrg + `"}}`, "reset organization unavailable"}, {200, 502, `{"uuid":"` + testOrg + `"}`, "reset organization unavailable"}, {200, 409, `{"organization":{"uuid":"` + testRedeemKey + `"}}`, "reset_account_changed"},
	} {
		e := newEngine()
		e.quotas["c"] = AccountQuota{Provider: "claude"}
		seedResetBinding(e, "c")
		s, _, msg := e.consumeResetWith("c", testRedeemKey, "chosen", testConfirmation, claudeRPC(t, claudeStep{claudeProfileURL, tc.up, tc.body, ""}), providerPOST)
		if s != tc.want || msg != tc.msg {
			t.Errorf("got %d %s", s, msg)
		}
	}
	e := newEngine()
	e.quotas["c"] = AccountQuota{Provider: "claude"}
	for _, id := range []string{"", "x/y", "ABC", strings.Repeat("a", 41)} {
		s, _, msg := e.consumeResetWith("c", testRedeemKey, id, testConfirmation, nil, nil)
		if s != 400 || msg != "invalid_credit_id" {
			t.Fatal("accepted invalid grant")
		}
	}
	if s, _, msg := e.consumeResetWith("c", strings.Repeat("x", 65), "chosen", testConfirmation, nil, nil); s != 400 || msg != "invalid_redeem_request_id" {
		t.Fatal("accepted oversized request ID")
	}
	seedResetBinding(e, "c")
	e.consuming["c"] = true
	if s, _, msg := e.consumeResetWith("c", testRedeemKey, "chosen", testConfirmation, nil, nil); s != 409 || msg != "reset_busy" {
		t.Fatal("ignored account lock")
	}
}

const bindingInventory = `{"cedar_ember":{"eligible":true,"grants":[{"id":"chosen","resets_left":1,"paused":false,"usable_now":true}]}}`

func TestConsumeResetBindsEachTabToItsOwnOrganization(t *testing.T) {
	e := newEngine()
	e.quotas["c"] = AccountQuota{Provider: "claude"}
	const orgB = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	profile := claudeStep{claudeProfileURL, 200, `{"organization":{"uuid":"` + orgB + `"}}`, ""}
	list := func(steps ...claudeStep) string {
		s, l, m := e.listResetCreditsWith("c", claudeRPC(t, steps...), nil)
		if s != 200 || m != "" || !confirmationTokenPattern.MatchString(l.ConfirmationToken) || strings.Contains(l.ConfirmationToken, testOrg) || strings.Contains(l.ConfirmationToken, orgB) {
			t.Fatal("missing or invalid opaque token")
		}
		return l.ConfirmationToken
	}
	a := list(claudeStep{claudeResetInventoryURL, 200, bindingInventory, testOrg})
	b := list(claudeStep{claudeResetInventoryURL, 200, bindingInventory, "invalid"}, profile)
	if a == b || e.resetBindings[b].org != orgB {
		t.Fatal("second list overwrote first tab")
	}
	s, _, m := e.consumeResetWith("c", testRedeemKey, "chosen", a, claudeRPC(t, profile), providerPOST)
	if s != 409 || m != "reset_account_changed" || len(e.resetBindings) != 1 || e.resetBindings[b].org != orgB {
		t.Fatal("wrong organization was not refused or wrong token invalidated")
	}
	url := "https://api.anthropic.com/api/organizations/" + orgB + "/reset_rate_limits"
	s, _, m = e.consumeResetWith("c", testRedeemKey, "chosen", b, claudeRPC(t, profile, claudeStep{url, 200, `{"result":"reset"}`, testOrg}), providerPOST)
	if s != 502 || m != "reset_unconfirmed" || len(e.resetBindings) != 1 {
		t.Fatal("uncertain result did not retain binding")
	}
	s, r, m := e.consumeResetWith("c", testRedeemKey, "chosen", b, claudeRPC(t, profile, claudeStep{url, 200, `{"result":"already_used"}`, orgB}), providerPOST)
	if s != 200 || m != "" || r.Code != "already_redeemed" || len(e.resetBindings) != 0 {
		t.Fatal("idempotent retry of new tab failed")
	}
}

func TestConsumeResetRejectsUnboundBeforeCredential(t *testing.T) {
	for _, c := range []struct {
		name, token, account, credit string
		age                          time.Duration
		want                         int
	}{
		{"missing", "", "c", "chosen", 0, 400}, {"malformed", "bad token", "c", "chosen", 0, 400}, {"unknown", "absent_confirmation_token", "c", "chosen", 0, 409}, {"expired", testConfirmation, "c", "chosen", resetBindingTTL, 409}, {"other account", testConfirmation, "d", "chosen", 0, 409}, {"unusable credit", testConfirmation, "c", "other", 0, 409},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine()
			e.quotas[c.account] = AccountQuota{Provider: "claude"}
			seedResetBinding(e, "c")
			b := e.resetBindings[testConfirmation]
			b.createdAt = time.Now().Add(-c.age)
			e.resetBindings[testConfirmation] = b
			s, _, m := e.consumeResetWith(c.account, testRedeemKey, c.credit, c.token, nil, nil)
			msg := "reset_account_changed"
			if c.want == 400 {
				msg = "invalid_confirmation_token"
			}
			if s != c.want || m != msg {
				t.Fatalf("got %d %s", s, m)
			}
		})
	}
}

func TestListResetClaudeBindingOnlyForUsableGrants(t *testing.T) {
	e := newEngine()
	e.quotas["c"] = AccountQuota{Provider: "claude"}
	body := strings.Replace(bindingInventory, `"usable_now":true`, `"usable_now":false`, 1)
	s, l, m := e.listResetCreditsWith("c", claudeRPC(t, claudeStep{claudeResetInventoryURL, 200, body, ""}), nil)
	if s != 200 || m != "" || l.ConfirmationToken != "" || len(e.resetBindings) != 0 {
		t.Fatal("read-only inventory acquired a binding")
	}
	for _, c := range []struct {
		up, want int
		msg      string
	}{{401, 422, "reset_auth_failed"}, {403, 422, "reset_auth_failed"}, {429, 429, "reset_rate_limited"}, {503, 502, "reset organization unavailable"}, {200, 502, "reset organization unavailable"}} {
		s, l, m = e.listResetCreditsWith("c", claudeRPC(t, claudeStep{claudeResetInventoryURL, 200, bindingInventory, ""}, claudeStep{claudeProfileURL, c.up, `{}`, ""}), nil)
		if s != c.want || m != c.msg || l.ConfirmationToken != "" || len(e.resetBindings) != 0 {
			t.Errorf("got %d %s", s, m)
		}
	}
}

func TestClaudeResetBindingCacheBounded(t *testing.T) {
	e := newEngine()
	now := time.Now()
	e.storeResetBindingLocked("expired", resetBinding{createdAt: now.Add(-resetBindingTTL)})
	for i := 0; i <= resetBindingLimit; i++ {
		e.storeResetBindingLocked(fmt.Sprint(i), resetBinding{createdAt: now.Add(time.Duration(i) * time.Millisecond)})
	}
	_, oldest := e.resetBindings["0"]
	_, expired := e.resetBindings["expired"]
	if oldest || expired || len(e.resetBindings) != resetBindingLimit {
		t.Fatal("cache retained expired/oldest or exceeded its bound")
	}
}

func TestListResetAndConsumeResetRejectLiveDisabledBeforeCredential(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			e := newEngine()
			e.quotas["c"] = AccountQuota{Provider: provider, Disabled: false}
			seedResetBinding(e, "c")
			calls := 0
			rpc := func(method string, _ any) (any, error) {
				calls++
				if method != "host.auth.list" {
					t.Fatalf("disabled account reached %s", method)
				}
				return map[string]any{"files": []any{map[string]any{"id": "c", "auth_index": "i", "provider": provider, "disabled": true}}}, nil
			}
			s, _, m := e.listResetCreditsWith("c", rpc, nil)
			if s != 409 || m != "account_disabled" {
				t.Errorf("list: %d %s", s, m)
			}
			s, _, m = e.consumeResetWith("c", testRedeemKey, "chosen", testConfirmation, rpc, nil)
			if s != 409 || m != "account_disabled" || calls != 2 || e.consuming["c"] {
				t.Errorf("consume: %d %s", s, m)
			}
		})
	}
}

func TestProviderPOSTClaudeURLAllowlist(t *testing.T) {
	u := "https://api.anthropic.com/api/organizations/" + testOrg + "/reset_rate_limits"
	for _, url := range []string{u + "/", u + "?q=x", u + "#frag", strings.Replace(u, ".com/", ".com.example/", 1), strings.Replace(u, "api.anthropic.com", "apiXanthropicXcom", 1), strings.Replace(u, testOrg, strings.ToUpper(testOrg), 1), strings.Replace(u, testOrg, "../"+testOrg, 1), claudeProfileURL, claudeResetInventoryURL} {
		if _, _, err := providerPOST(url, nil, nil, nil); err == nil {
			t.Errorf("accepted %s", url)
		}
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
	e.persistence.Enabled = false
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	e.quotas["c"] = AccountQuota{AuthID: "c", Provider: "codex", Disabled: true}
	if !e.syncAuthStatus(func(string, any) (any, error) { return map[string]any{"files": files}, nil }) {
		t.Fatal("new account must request a discovery refresh")
	}
	views := map[string]QuotaView{}
	for _, q := range e.snapshot().Quotas {
		views[q.AuthID] = q
	}
	if !views["a"].Disabled || views["c"].Disabled {
		t.Fatalf("views %+v", views)
	}
	if q, ok := views["b"]; !ok || !q.Disabled || q.Error == "" || q.FetchedAt != "" {
		t.Fatal("new host account must be visible with pending, not fabricated, quota")
	}
	// Host failure or a null (not explicitly empty) list keeps the last known state.
	e.syncAuthStatus(func(string, any) (any, error) { return nil, fmt.Errorf("down") })
	e.syncAuthStatus(func(string, any) (any, error) { return map[string]any{"files": nil}, nil })
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

const sampleResetCredits = `{"available_count":3,"credits":[
 {"id":"late","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":"2026-12-01T00:00:00Z","title":"Full reset"},
 {"id":"forever","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":null},
 {"id":"used","reset_type":"codex_rate_limits","status":"redeemed","granted_at":"2026-09-01T00:00:00Z","expires_at":"2026-10-22T00:00:00Z"},
 {"id":"soon","reset_type":"codex_rate_limits","status":"available","granted_at":"2026-10-01T00:00:00Z","expires_at":"2026-10-25T08:00:00+08:00","description":"  Reset your current usage limits  "}
]}`

func TestListResetCreditsOrdersEarliestExpiryFirst(t *testing.T) {
	e := newEngine()
	one := int64(1)
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", ManualResetRemaining: &one}
	gets := 0
	get := func(url string, headers http.Header) ([]byte, error) {
		gets++
		if url != codexResetCreditsURL || headers.Get("Authorization") != "Bearer secret-token" {
			t.Fatalf("get %s %v", url, headers)
		}
		return []byte(sampleResetCredits), nil
	}
	status, list, failure := e.listResetCreditsWith("a", actionRPC(t, []any{codexFile("a")}), get)
	if status != 200 || failure != "" || gets != 1 {
		t.Fatalf("got %d %q gets=%d", status, failure, gets)
	}
	ids := []string{}
	for _, c := range list.Credits {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "soon,late,forever" || list.AvailableCount != 3 {
		t.Fatalf("order %v count %d", ids, list.AvailableCount)
	}
	if list.Credits[0].ExpiresAt != "2026-10-25T00:00:00Z" || list.Credits[0].Description != "Reset your current usage limits" || list.Credits[2].ExpiresAt != "" {
		t.Fatalf("credits %+v", list.Credits)
	}
	if *e.quotas["a"].ManualResetRemaining != 3 {
		t.Fatal("fresh count must replace the cached one")
	}
	encoded, _ := json.Marshal(list)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "used") {
		t.Fatalf("leak or redeemed credit: %s", encoded)
	}
}

func TestParseResetCreditsFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"credits":[]}`, `{"available_count":-1,"credits":[]}`, `{"available_count":1.5}`,
		`{"available_count":1,"credits":[{"id":"x","status":"available","expires_at":"tomorrow"}]}`,
		`{"available_count":1,"credits":[{"id":"has space","status":"available"}]}`,
		`{"available_count":1,"credits":[{"id":"","status":"available"}]}`,
	} {
		if _, ok := parseResetCredits([]byte(body)); ok {
			t.Errorf("accepted %s", body)
		}
	}
	// Count without details stays usable; the dashboard lets OpenAI choose.
	list, ok := parseResetCredits([]byte(`{"available_count":2}`))
	if !ok || list.AvailableCount != 2 || list.Credits == nil || len(list.Credits) != 0 {
		t.Fatalf("%+v %v", list, ok)
	}
	// Never offer more credits than the official available count.
	list, ok = parseResetCredits([]byte(strings.Replace(sampleResetCredits, `"available_count":3`, `"available_count":1`, 1)))
	if !ok || len(list.Credits) != 1 || list.Credits[0].ID != "soon" {
		t.Fatalf("%+v", list)
	}
}

func TestListResetCreditsRejectsUnknownAndFailedReads(t *testing.T) {
	e := newEngine()
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	e.quotas["c"] = AccountQuota{AuthID: "c", Provider: "claude"}
	never := func(string, http.Header) ([]byte, error) { t.Fatal("must not read"); return nil, nil }
	rpc := actionRPC(t, []any{codexFile("a")})
	for id, want := range map[string]string{"": "invalid_auth_id", "missing": "account_not_found", "c": "account_not_found"} {
		if _, _, failure := e.listResetCreditsWith(id, rpc, never); failure != want {
			t.Errorf("%q: %q", id, failure)
		}
	}
	fail := func(string, http.Header) ([]byte, error) { return nil, fmt.Errorf("quota HTTP 500") }
	if status, _, failure := e.listResetCreditsWith("a", rpc, fail); status != 502 || failure != "reset credits unavailable" {
		t.Fatalf("%d %q", status, failure)
	}
	bad := func(string, http.Header) ([]byte, error) { return []byte(`{"credits":"x"}`), nil }
	if status, _, failure := e.listResetCreditsWith("a", rpc, bad); status != 502 || failure != "reset credits invalid" {
		t.Fatalf("%d %q", status, failure)
	}
}

func TestConsumeResetSendsChosenCreditID(t *testing.T) {
	e := newEngine()
	two := int64(2)
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", ManualResetRemaining: &two}
	var sent map[string]any
	code := "reset"
	post := func(_ string, _ http.Header, body []byte, _ func(string, any) (any, error)) (int, []byte, error) {
		sent = nil
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatal(err)
		}
		return 200, []byte(`{"code":"` + code + `","windows_reset":1}`), nil
	}
	rpc := actionRPC(t, []any{codexFile("a")})
	if status, _, failure := e.consumeResetWith("a", testRedeemKey, "soon", "", rpc, post); status != 200 || failure != "" {
		t.Fatalf("%d %q", status, failure)
	}
	if len(sent) != 2 || sent["credit_id"] != "soon" || sent["redeem_request_id"] != testRedeemKey || *e.quotas["a"].ManualResetRemaining != 1 {
		t.Fatalf("sent %v remaining %d", sent, *e.quotas["a"].ManualResetRemaining)
	}
	// A chosen credit that is gone says nothing about the remaining ones.
	code = "no_credit"
	if _, result, _ := e.consumeResetWith("a", testRedeemKey, "soon", "", rpc, post); result.Code != "no_credit" || *e.quotas["a"].ManualResetRemaining != 1 {
		t.Fatalf("%+v remaining %d", result, *e.quotas["a"].ManualResetRemaining)
	}
	never := func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error) {
		t.Fatal("must not post")
		return 0, nil, nil
	}
	for _, id := range []string{"has space", strings.Repeat("x", 257), "中文"} {
		if status, _, failure := e.consumeResetWith("a", testRedeemKey, id, "", rpc, never); status != 400 || failure != "invalid_credit_id" {
			t.Errorf("%q: %d %q", id, status, failure)
		}
	}
}
