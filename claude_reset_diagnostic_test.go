package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClaudeResetDiagnosticResponseMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		diagnostic string
		knownZero  bool
	}{
		{"missing block", `{}`, "unavailable", false},
		{"null block", `{"cedar_ember":null}`, "unavailable", false},
		{"private upstream error", `{"error":{"message":"private-upstream-text"}}`, "unavailable", false},
		{"null response", `null`, "invalid_response", false},
		{"array response", `[]`, "invalid_response", false},
		{"nonobject block", `{"cedar_ember":[]}`, "invalid_response", false},
		{"missing eligibility", `{"cedar_ember":{}}`, "invalid_response", false},
		{"null eligibility", `{"cedar_ember":{"eligible":null}}`, "invalid_response", false},
		{"missing grants", `{"cedar_ember":{"eligible":true}}`, "invalid_response", false},
		{"null grants", `{"cedar_ember":{"eligible":true,"grants":null}}`, "invalid_response", false},
		{"CLI gate before grants", `{"cedar_ember":{"eligible":false,"ineligible_reason":"cli_version","grants":"private-upstream-text"}}`, "cli_version", false},
		{"surface gate", `{"cedar_ember":{"eligible":false,"ineligible_reason":"surface"}}`, "surface", false},
		{"unknown reason", `{"cedar_ember":{"eligible":false,"ineligible_reason":"private-upstream-text"}}`, "not_eligible", false},
		{"missing reason", `{"cedar_ember":{"eligible":false}}`, "not_eligible", false},
		{"null reason", `{"cedar_ember":{"eligible":false,"ineligible_reason":null}}`, "not_eligible", false},
		{"structured reason", `{"cedar_ember":{"eligible":false,"ineligible_reason":{"secret":"private-upstream-text"}}}`, "not_eligible", false},
		{"nonexact reason", `{"cedar_ember":{"eligible":false,"ineligible_reason":"cli_version private-upstream-text"}}`, "not_eligible", false},
		{"eligible ignores stale reason", `{"cedar_ember":{"eligible":true,"ineligible_reason":"cli_version","grants":[]}}`, "", true},
		{"eligible ignores malformed reason", `{"cedar_ember":{"eligible":true,"ineligible_reason":{"secret":"private-upstream-text"},"grants":[]}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count, diagnostic := parseClaudeResetRemaining([]byte(tc.body), time.Now().UTC())
			if diagnostic != tc.diagnostic || (tc.knownZero && (count == nil || *count != 0)) || (!tc.knownZero && count != nil) {
				t.Fatalf("count/diagnostic contract: count=%v diagnostic=%q", count, diagnostic)
			}
		})
	}
}

func TestClaudeResetDiagnosticJSONBoundaries(t *testing.T) {
	zero, positive := int64(0), int64(2)
	type diagnosticCase struct {
		name       string
		provider   string
		count      *int64
		diagnostic string
		want       string
	}
	cases := []diagnosticCase{
		{"old Claude cache", "claude", nil, "", "unavailable"},
		{"unsafe cached text", "claude", nil, "Bearer private-upstream-text", "unavailable"},
		{"known zero hides diagnostic", "claude", &zero, "surface", ""},
		{"known count hides unsafe text", "claude", &positive, "private-upstream-text", ""},
		{"Codex hides diagnostic", "codex", nil, "surface", ""},
		{"Antigravity hides diagnostic", "antigravity", nil, "private-upstream-text", ""},
	}
	for _, diagnostic := range []string{"request_failed", "cli_version", "surface", "not_eligible", "unavailable", "invalid_response"} {
		cases = append(cases, diagnosticCase{diagnostic, "claude", nil, diagnostic, diagnostic})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := AccountQuota{AuthID: "account", Provider: tc.provider, ManualResetRemaining: tc.count, ManualResetDiagnostic: tc.diagnostic}
			view := QuotaView{Provider: tc.provider, ManualResetRemaining: tc.count, ManualResetDiagnostic: tc.diagnostic}
			e := newEngine()
			e.dir = t.TempDir()
			e.quotas[q.AuthID] = q
			snapshot := e.snapshot().Quotas[0]
			if snapshot.ManualResetDiagnostic != tc.want {
				t.Fatal("snapshot must normalize cached diagnostics before returning them")
			}
			for _, value := range []any{q, view, snapshot} {
				body, err := json.Marshal(value)
				if err != nil || strings.Contains(string(body), "private-upstream-text") {
					t.Fatal("unsafe diagnostic reached a JSON boundary")
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				raw, present := fields["manual_reset_diagnostic"]
				if present != (tc.want != "") || (present && string(raw) != `"`+tc.want+`"`) {
					t.Fatal("diagnostic allow-list/omission contract changed")
				}
			}
		})
	}
}

func TestClaudeResetDiagnosticRetainedUntilSuccessfulRefresh(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.quotas["account"] = AccountQuota{AuthID: "account", Provider: "claude", ManualResetDiagnostic: "surface", Five: &QuotaWindow{Used: 70}}
	for _, failure := range []string{"credential", "usage", "schema", ""} {
		calls := 0
		ok := e.refreshAccountQuotaWith(AuthEntry{ID: "account", Index: "index", Provider: "claude"}, func(string, any) (any, error) {
			if failure == "credential" {
				return nil, fmt.Errorf("private-upstream-text")
			}
			return map[string]any{"access_token": "synthetic-only-token"}, nil
		}, func(url string, _ http.Header) ([]byte, error) {
			calls++
			if url == claudeUsageURL {
				if failure == "usage" {
					return nil, fmt.Errorf("private-upstream-text")
				}
				if failure == "schema" {
					return []byte(`{}`), nil
				}
				return []byte(`{"five_hour":{"utilization":12}}`), nil
			}
			if url != claudeResetInventoryURL || calls != 2 {
				t.Fatal("unexpected optional request")
			}
			return []byte(`{"cedar_ember":{"eligible":true,"grants":[]}}`), nil
		})
		q := e.quotas["account"]
		if failure != "" {
			wantCalls := 1
			if failure == "credential" {
				wantCalls = 0
			}
			if ok || calls != wantCalls || q.ManualResetRemaining != nil || q.ManualResetDiagnostic != "surface" || q.Five.Used != 70 || q.Error == "" {
				t.Fatal("ordinary failure must preserve prior inventory diagnostic and usage")
			}
		} else if !ok || calls != 2 || q.ManualResetRemaining == nil || *q.ManualResetRemaining != 0 || q.ManualResetDiagnostic != "" || q.Error != "" {
			t.Fatal("successful known inventory must clear the previous diagnostic")
		}
		restored := newEngine()
		restored.dir = e.dir
		restored.loadPersistence()
		if restored.storageReadFailed || restored.snapshot().Quotas[0].ManualResetDiagnostic != q.ManualResetDiagnostic {
			t.Fatal("retained/cleared diagnostic must persist across restarts")
		}
	}
}
