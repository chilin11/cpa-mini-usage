package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func waitAccountRefresh(t *testing.T, e *Engine, id string) AccountRefreshState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state := e.snapshot().AccountRefresh[id]
		if !state.Refreshing && state.FinishedAt != "" {
			// Also wait for the worker to release the provider-read lock.
			if e.refreshMu.TryLock() {
				e.refreshMu.Unlock()
				return state
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("account refresh did not complete")
	return AccountRefreshState{}
}

func TestAccountRefreshTargetsOnlyOneCredential(t *testing.T) {
	e := newEngine()
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex", Name: "Alice"}
	e.quotas["b"] = AccountQuota{AuthID: "b", Provider: "claude", Name: "Bob", Error: "keep"}
	e.accountRefresh["a"] = AccountRefreshState{FinishedAt: "previous"}
	fullClock := time.Now().Add(-time.Hour)
	e.lastRefresh = fullClock
	started, release := make(chan struct{}), make(chan struct{})
	reads := []string{}
	rpc := func(method string, payload any) (any, error) {
		if method != "host.auth.list" {
			t.Errorf("unexpected RPC %s", method)
		}
		return map[string]any{"files": []any{map[string]any{"id": "a", "auth_index": "index-a", "provider": "codex"}, map[string]any{"id": "b", "auth_index": "index-b", "provider": "claude"}}}, nil
	}
	status, previous, errCode := e.beginAccountRefresh("a", rpc, func(entry AuthEntry) bool {
		reads = append(reads, entry.ID)
		close(started)
		<-release
		e.mu.Lock()
		q := e.quotas[entry.ID]
		q.Name = "Updated"
		e.quotas[entry.ID] = q
		e.mu.Unlock()
		return true
	})
	if status != 202 || previous != "previous" || errCode != "" {
		t.Fatalf("accept %d %q %q", status, previous, errCode)
	}
	<-started
	state := e.snapshot()
	if !state.AccountRefresh["a"].Refreshing || state.QuotaRefreshing {
		t.Fatal("independent state missing")
	}
	if s, _, code := e.beginAccountRefresh("b", rpc, func(AuthEntry) bool { return true }); s != 409 || code != "refresh_busy" {
		t.Fatalf("busy %d %s", s, code)
	}
	if _, ok := e.kickFullRefresh(); ok {
		t.Fatal("full refresh must reject while account refresh is active")
	}
	close(release)
	done := waitAccountRefresh(t, e, "a")
	if done.Error != "" || done.FinishedAt == previous || len(reads) != 1 || reads[0] != "a" {
		t.Fatalf("completion %+v reads %v", done, reads)
	}
	if !e.lastRefresh.Equal(fullClock) || !e.quotaRefreshFinished.IsZero() || e.quotas["b"].Error != "keep" || e.quotas["b"].Name != "Bob" {
		t.Fatal("other account/full refresh state changed")
	}
	state.AccountRefresh["a"] = AccountRefreshState{Error: "mutated"}
	if e.snapshot().AccountRefresh["a"].Error == "mutated" {
		t.Fatal("snapshot aliases mutable state")
	}
}

func TestAccountRefreshFailuresComplete(t *testing.T) {
	for _, kind := range []string{"rpc", "missing", "provider", "read"} {
		t.Run(kind, func(t *testing.T) {
			e := newEngine()
			e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
			called := false
			rpc := func(string, any) (any, error) {
				if kind == "rpc" {
					return nil, fmt.Errorf("internal secret detail")
				}
				provider := "codex"
				if kind == "provider" {
					provider = "api-key"
				}
				id := "a"
				if kind == "missing" {
					id = "b"
				}
				return map[string]any{"files": []any{map[string]any{"id": id, "auth_index": "index", "provider": provider}}}, nil
			}
			status, _, _ := e.beginAccountRefresh("a", rpc, func(entry AuthEntry) bool {
				called = true
				e.recordQuotaError(entry, "Quota endpoint unavailable")
				return false
			})
			if status != 202 {
				t.Fatal(status)
			}
			state := waitAccountRefresh(t, e, "a")
			if state.Error == "" || strings.Contains(state.Error, "secret") || called != (kind == "read") {
				t.Fatalf("state %+v read %v", state, called)
			}
		})
	}
}

func TestAccountRefreshRejectsInvalidTargets(t *testing.T) {
	e := newEngine()
	for _, tc := range []struct {
		id     string
		status int
	}{{"", 400}, {" ", 400}, {strings.Repeat("a", 1025), 400}, {"unknown", 404}} {
		status, _, _ := e.beginAccountRefresh(tc.id, func(string, any) (any, error) { t.Fatal("RPC called"); return nil, nil }, func(AuthEntry) bool { t.Fatal("read called"); return false })
		if status != tc.status {
			t.Fatalf("got %d want %d", status, tc.status)
		}
	}
	if quotaInterval != 15*time.Minute {
		t.Fatal("quota cadence must be 15 minutes")
	}
}

func TestAccountRefreshManagementRoute(t *testing.T) {
	old := tracker
	tracker = newEngine()
	defer func() { tracker = old }()
	for _, tc := range []struct {
		payload string
		status  int
	}{{`{}`, 400}, {`{"auth_id":123}`, 400}, {`not-json`, 400}, {`{"auth_id":"unknown"}`, 404}} {
		body, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/v0/management/plugins/cpa-mini-usage/refresh-account", "Body": []byte(tc.payload)})
		var response struct {
			OK     bool
			Result struct {
				StatusCode int
				Body       []byte
			}
		}
		if err := json.Unmarshal(dispatch("management.handle", body), &response); err != nil {
			t.Fatal(err)
		}
		if !response.OK || response.Result.StatusCode != tc.status {
			t.Fatalf("response %+v", response)
		}
	}
	tracker.refreshMu.Lock()
	body, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/v0/management/plugins/cpa-mini-usage/refresh"})
	var response struct{ Result struct{ StatusCode int } }
	json.Unmarshal(dispatch("management.handle", body), &response)
	tracker.refreshMu.Unlock()
	if response.Result.StatusCode != 409 {
		t.Fatal("busy all-account refresh must return HTTP 409")
	}
}
