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
			return state
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
	if s, _, code := e.beginAccountRefresh("a", rpc, func(AuthEntry) bool { return true }); s != 409 || code != "refresh_busy" {
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

func TestAccountRefreshConcurrentAccounts(t *testing.T) {
	for _, failA := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_errors=%t", failA), func(t *testing.T) {
			e := newEngine()
			e.persistence.Enabled = false
			e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
			e.quotas["b"] = AccountQuota{AuthID: "b", Provider: "claude"}
			fullClock := time.Now().Add(-time.Hour)
			e.lastRefresh = fullClock
			started := make(chan string, 2)
			releases := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
			retryStarted, retryRelease := make(chan struct{}), make(chan struct{})
			release := func(ch chan struct{}) {
				select {
				case <-ch:
				default:
					close(ch)
				}
			}
			t.Cleanup(func() {
				for _, ch := range releases {
					release(ch)
				}
				release(retryRelease)
			})
			rpc := func(method string, _ any) (any, error) {
				if method != "host.auth.list" {
					t.Errorf("unexpected RPC %s", method)
				}
				return []any{
					map[string]any{"id": "a", "auth_index": "index-a", "provider": "codex"},
					map[string]any{"id": "b", "auth_index": "index-b", "provider": "claude"},
				}, nil
			}
			read := func(entry AuthEntry) bool {
				started <- entry.ID
				<-releases[entry.ID]
				if entry.ID == "a" && failA {
					e.recordQuotaError(entry, "Quota endpoint unavailable")
					return false
				}
				return true
			}
			type startResult struct {
				id     string
				status int
				code   string
			}
			gate := make(chan struct{})
			results := make(chan startResult, 3)
			for _, id := range []string{"a", "a", "b"} {
				go func() {
					<-gate
					status, _, code := e.beginAccountRefresh(id, rpc, read)
					results <- startResult{id, status, code}
				}()
			}
			close(gate)
			accepted, rejected := map[string]int{}, map[string]int{}
			for range 3 {
				select {
				case result := <-results:
					switch {
					case result.status == 202 && result.code == "":
						accepted[result.id]++
					case result.status == 409 && result.code == "refresh_busy":
						rejected[result.id]++
					default:
						t.Fatalf("unexpected start result: %+v", result)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("concurrent start calls did not return")
				}
			}
			if accepted["a"] != 1 || accepted["b"] != 1 || rejected["a"] != 1 || rejected["b"] != 0 {
				t.Fatalf("reservation not atomic: accepted %v rejected %v", accepted, rejected)
			}
			seen := map[string]bool{}
			for range 2 {
				select {
				case id := <-started:
					seen[id] = true
				case <-time.After(3 * time.Second):
					t.Fatal("both account reads must start before either is released")
				}
			}
			if !seen["a"] || !seen["b"] {
				t.Fatalf("wrong concurrent targets: %v", seen)
			}
			assertRefreshing := func(a, b bool) {
				t.Helper()
				snapshot := e.snapshot()
				if snapshot.AccountRefresh["a"].Refreshing != a || snapshot.AccountRefresh["b"].Refreshing != b || snapshot.QuotaRefreshing {
					t.Fatalf("unexpected refresh state: %+v", snapshot.AccountRefresh)
				}
			}
			assertFullBlocked := func() {
				t.Helper()
				if _, ok := e.kickFullRefresh(); ok {
					t.Fatal("full refresh accepted while an individual job is active")
				}
				e.refreshQuota(true)
				e.mu.RLock()
				unchanged := e.lastRefresh.Equal(fullClock) && e.quotaRefreshFinished.IsZero()
				e.mu.RUnlock()
				if !unchanged {
					t.Fatal("individual refresh changed full refresh cadence")
				}
			}
			assertDuplicate := func(id string) {
				t.Helper()
				if status, _, code := e.beginAccountRefresh(id, rpc, read); status != 409 || code != "refresh_busy" {
					t.Fatalf("duplicate %s: %d %s", id, status, code)
				}
			}
			assertRefreshing(true, true)
			assertDuplicate("a")
			assertDuplicate("b")
			assertFullBlocked()
			release(releases["a"])
			first := waitAccountRefresh(t, e, "a")
			if (first.Error != "") != failA || (failA && first.Error != "Quota endpoint unavailable") {
				t.Fatalf("unexpected completion: %+v", first)
			}
			assertRefreshing(false, true)
			assertDuplicate("b")
			assertFullBlocked()
			status, previous, code := e.beginAccountRefresh("a", rpc, func(AuthEntry) bool {
				close(retryStarted)
				<-retryRelease
				return true
			})
			if status != 202 || previous != first.FinishedAt || code != "" {
				t.Fatalf("retry while b active: %d %q %q", status, previous, code)
			}
			select {
			case <-retryStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("retry did not start while b was active")
			}
			assertRefreshing(true, true)
			if e.snapshot().AccountRefresh["a"].Error != "" {
				t.Fatal("retry retained previous error")
			}
			assertDuplicate("a")
			assertFullBlocked()
			release(releases["b"])
			if done := waitAccountRefresh(t, e, "b"); done.Error != "" {
				t.Fatalf("b completion: %+v", done)
			}
			assertRefreshing(true, false)
			assertFullBlocked()
			release(retryRelease)
			if done := waitAccountRefresh(t, e, "a"); done.Error != "" || done.FinishedAt == first.FinishedAt {
				t.Fatalf("retry completion: %+v", done)
			}
			assertRefreshing(false, false)
			// Acquiring the batch lock proves full refresh can resume without a real callback.
			if !e.refreshMu.TryLock() {
				t.Fatal("batch lock still held after all individual jobs completed")
			}
			e.refreshMu.Unlock()
		})
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
	e.quotas["a"] = AccountQuota{AuthID: "a", Provider: "codex"}
	for _, tc := range []struct {
		id     string
		status int
	}{{"", 400}, {" ", 400}, {strings.Repeat("a", 1025), 400}, {"unknown", 404}, {" a ", 404}} {
		status, _, _ := e.beginAccountRefresh(tc.id, func(string, any) (any, error) { t.Fatal("RPC called"); return nil, nil }, func(AuthEntry) bool { t.Fatal("read called"); return false })
		if status != tc.status {
			t.Fatalf("got %d want %d", status, tc.status)
		}
	}
	e.refreshMu.Lock()
	status, _, code := e.beginAccountRefresh("a", func(string, any) (any, error) { t.Error("RPC called during full refresh"); return nil, nil }, func(AuthEntry) bool { t.Error("read called during full refresh"); return false })
	e.refreshMu.Unlock()
	if status != 409 || code != "refresh_busy" {
		t.Fatalf("individual refresh during full refresh: %d %s", status, code)
	}
	if !e.refreshMu.TryLock() {
		t.Fatal("rejected target leaked the refresh lock")
	}
	e.refreshMu.Unlock()
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
