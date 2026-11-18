package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func discoveryRecord(id, provider string) any {
	return map[string]any{"id": id, "auth_index": "index-" + id, "type": provider, "name": id + ".json"}
}

func waitDiscoveryRefresh(t *testing.T, e *Engine) Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := e.snapshot()
		if !s.QuotaRefreshing && s.QuotaRefreshFinishedAt != "" && e.refreshMu.TryLock() {
			e.refreshMu.Unlock()
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("discovery full refresh did not release the batch lock")
	return Snapshot{}
}

func TestAuthDiscoverySyncRegistersNewAccountsAndPreservesHistory(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	old := AccountQuota{AuthID: "old", Provider: "codex", Name: "Old", Five: &QuotaWindow{Used: 37}, FetchedAt: "previous", Error: "previous error"}
	e.quotas["old"] = old
	e.quotas["historical"] = AccountQuota{AuthID: "historical", Provider: "claude"}
	e.events = []Event{{AuthID: "historical", At: time.Now().UTC(), Total: 12}}
	clock := time.Now()
	e.lastRefresh = clock
	files := []any{discoveryRecord("old", "codex"), discoveryRecord("historical", "claude")}
	rpc := func(method string, _ any) (any, error) {
		if method != "host.auth.list" {
			t.Errorf("sync must not read credential material: %s", method)
		}
		return map[string]any{"files": files}, nil
	}
	if e.syncAuthStatus(rpc) {
		t.Fatal("unchanged identities should not request an early scan")
	}
	files = files[:1] // Next list is partial/malformed: keep the historical card.
	for _, provider := range []string{"claude", "codex", "antigravity"} {
		files = append(files, discoveryRecord(provider, provider))
	}
	files = append(files, discoveryRecord("unsupported", "gemini"), map[string]any{"id": "no-index", "type": "codex"})
	files[1].(map[string]any)["disabled"] = true
	if !e.syncAuthStatus(rpc) || !e.authDiscoveryPending {
		t.Fatal("new identities must leave a pending early refresh")
	}
	if len(e.quotas) != 5 || len(e.events) != 1 || e.events[0].Total != 12 || !e.lastRefresh.Equal(clock) || !e.quotaRefreshFinished.IsZero() {
		t.Fatal("discovery removed history, accepted unsupported records or advanced the scan clock")
	}
	if q := e.quotas["old"]; q.Five != old.Five || q.FetchedAt != old.FetchedAt || q.Error != old.Error || q.Name != old.Name {
		t.Fatal("identity sync replaced the previous quota")
	}
	for _, provider := range []string{"claude", "codex", "antigravity"} {
		q := e.quotas[provider]
		if q.AuthID != provider || q.Provider != provider || q.Name != provider || q.Five != nil || q.Week != nil || len(q.Groups) != 0 || q.FetchedAt != "" || q.Error == "" || q.Disabled != (provider == "claude") {
			t.Fatalf("new account must have visible, unknown quota: %+v", q)
		}
		if e.aliases["index-"+provider] != provider || e.names[provider] != provider {
			t.Fatal("new account identity was not reconciled")
		}
	}
	if !e.syncAuthStatus(rpc) {
		t.Fatal("an unaccepted early refresh must remain pending on subsequent syncs")
	}
	for _, raw := range []any{nil, map[string]any{"files": nil}, map[string]any{"unexpected": files}} {
		if e.syncAuthStatus(func(string, any) (any, error) { return raw, nil }) {
			t.Fatal("null or unrecognized host list should not trigger a scan")
		}
	}
	if e.syncAuthStatus(func(string, any) (any, error) { return nil, fmt.Errorf("private host detail") }) || len(e.quotas) != 5 || !e.authDiscoveryPending {
		t.Fatal("failed host sync cleared cached accounts or pending discovery")
	}
	restored := newEngine()
	restored.dir = e.dir
	restored.loadPersistence()
	if restored.storageReadFailed || len(restored.quotas) != 5 || restored.quotas["old"].Five.Used != 37 || restored.quotas["claude"].Error == "" {
		t.Fatal("discovery did not follow existing quota persistence")
	}
	if _, ok := e.beginFullRefresh(func(string, any) (any, error) { return nil, fmt.Errorf("private host detail") }, func(AuthEntry) bool { t.Error("read after list failure"); return false }); !ok {
		t.Fatal("early scan was not accepted")
	}
	s := waitDiscoveryRefresh(t, e)
	if s.QuotaError != "CPA credential callback is unavailable" || len(s.Quotas) != 5 || e.syncAuthStatus(rpc) {
		t.Fatal("failed discovery scan lost accounts or caused a polling retry loop")
	}
}

func TestAuthDiscoveryPrunesOnlyActiveState(t *testing.T) {
	for _, path := range []string{"sync", "full"} {
		t.Run(path, func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			for _, id := range []string{"keep", "gone"} {
				e.quotas[id] = AccountQuota{AuthID: id, Provider: "codex", Five: &QuotaWindow{Used: 37}}
				e.accountRefresh[id] = AccountRefreshState{FinishedAt: "prior"}
			}
			e.names["gone"], e.aliases["index-gone"] = "Historical", "gone"
			e.forecasts["gone:week"] = SavedForecast{Seconds: 42, ResetAt: time.Now().Add(time.Hour)}
			e.events = []Event{{AuthID: "gone", Total: 12}}
			apply := func(files []any) {
				rpc := func(string, any) (any, error) { return map[string]any{"files": files}, nil }
				if path == "sync" {
					if e.syncAuthStatus(rpc) {
						t.Fatal("removal requested a provider refresh")
					}
				} else {
					e.refreshMu.Lock()
					e.refreshQuotaWith(true, rpc, func(AuthEntry) bool { return true })
					e.refreshMu.Unlock()
				}
			}
			apply([]any{discoveryRecord("keep", "codex")})
			if len(e.snapshot().Quotas) != 1 || e.quotas["keep"].Five.Used != 37 || len(e.accountRefresh) != 1 || e.accountRefresh["keep"].FinishedAt != "prior" {
				t.Fatal("did not remove exactly the obsolete card and refresh state")
			}
			e.persistence.Enabled = false
			apply([]any{})
			if len(e.snapshot().Quotas) != 0 || len(e.accountRefresh) != 0 || len(e.events) != 1 || e.events[0].Total != 12 || e.quotaError != "" {
				t.Fatal("explicit empty list should remove the last card but not request history")
			}
			restored := newEngine()
			restored.dir = e.dir
			restored.loadPersistence()
			if restored.storageReadFailed || len(restored.quotas) != 1 || restored.quotas["keep"].AuthID != "keep" || restored.names["gone"] != "Historical" || restored.aliases["index-gone"] != "gone" || restored.forecasts["gone:week"].Seconds != 42 {
				t.Fatal("enabled removal must persist history; disabled removal must not overwrite disk")
			}
		})
	}
}

func TestAuthDiscoveryUntrustedListsNeverPrune(t *testing.T) {
	row := discoveryRecord("new", "codex")
	for _, raw := range []any{
		discoveryOnlyAuthList{map[string]any{"files": []any{row}}},
		[]any{row}, map[string]any{"items": []any{row}}, map[string]any{"files": []any{row}, "next_cursor": "p2"},
		map[string]any{"files": []any{row, nil}}, map[string]any{"files": []any{row, row}},
		map[string]any{"files": []any{row, map[string]any{"name": "disk.json", "type": "codex"}}},
		map[string]any{"files": []any{row, map[string]any{"auth_index": "idx", "type": "codex"}}},
		map[string]any{"files": []any{row, map[string]any{"id": "bad", "auth_index": "idx", "provider": false}}},
	} {
		e := newEngine()
		e.persistence.Enabled = false
		e.quotas["keep"] = AccountQuota{AuthID: "keep"}
		if !e.syncAuthStatus(func(string, any) (any, error) { return raw, nil }) || e.quotas["new"].AuthID != "new" || e.quotas["keep"].AuthID != "keep" {
			t.Fatal("untrusted list must allow discovery but never removal")
		}
	}
}

func TestAuthDiscoveryOutOfOrderListCannotUndoNewerInventory(t *testing.T) {
	for _, slow := range []string{"sync", "full"} {
		t.Run(slow, func(t *testing.T) {
			e := newEngine()
			e.persistence.Enabled = false
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			stale := func(string, any) (any, error) {
				close(started)
				<-release
				return map[string]any{"files": []any{discoveryRecord("obsolete", "codex")}}, nil
			}
			fresh := func(string, any) (any, error) {
				return map[string]any{"files": []any{discoveryRecord("new", "codex")}}, nil
			}
			read := func(entry AuthEntry) bool { e.recordQuotaError(entry, "synthetic"); return false }
			full := func(rpc func(string, any) (any, error)) {
				e.refreshMu.Lock()
				defer e.refreshMu.Unlock()
				e.refreshQuotaWith(true, rpc, read)
			}
			if slow == "sync" {
				e.quotas["obsolete"] = AccountQuota{AuthID: "obsolete"}
			}
			go func() {
				defer close(done)
				if slow == "sync" {
					e.syncAuthStatus(stale)
				} else {
					full(stale)
				}
			}()
			<-started
			if slow == "sync" {
				full(fresh)
			} else {
				e.syncAuthStatus(fresh)
			}
			unblock()
			<-done
			if len(e.quotas) != 1 || e.quotas["new"].AuthID != "new" {
				t.Fatal("old list removed a newer account or resurrected an obsolete one")
			}
		})
	}
}

func TestAuthDiscoveryPeriodicAndManualFullRefresh(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual=%t", force), func(t *testing.T) {
			e := newEngine()
			e.persistence.Enabled = false
			files := []any{discoveryRecord("old", "codex")}
			fail := false
			rpc := func(method string, _ any) (any, error) {
				switch method {
				case "host.auth.list":
					return map[string]any{"files": files}, nil
				case "host.auth.get":
					if fail {
						return nil, fmt.Errorf("Bearer synthetic-private-token credential path")
					}
					return map[string]any{"json": map[string]any{"access_token": "synthetic-private-token"}}, nil
				default:
					t.Errorf("unexpected RPC %s", method)
					return nil, fmt.Errorf("unexpected RPC")
				}
			}
			read := func(entry AuthEntry) bool {
				return e.refreshAccountQuotaWith(entry, rpc, func(string, http.Header) ([]byte, error) {
					if fail {
						t.Error("failed credentials must not reach a quota endpoint")
						return nil, fmt.Errorf("unexpected quota read")
					}
					return []byte(`{"rate_limit":{"primary_window":{"used_percent":37,"limit_window_seconds":18000},"secondary_window":{"used_percent":12,"limit_window_seconds":604800}}}`), nil
				})
			}
			e.refreshQuotaWith(true, rpc, read)
			prior := e.quotas["old"]
			if prior.Five == nil || prior.Five.Used != 37 || prior.FetchedAt == "" || prior.Error != "" {
				t.Fatal("initial existing account was not fetched")
			}
			for _, provider := range []string{"claude", "codex", "antigravity"} {
				files = append(files, discoveryRecord("new-"+provider, provider))
			}
			fail = true
			if !force {
				e.lastRefresh = time.Now().Add(-quotaInterval - time.Second)
			}
			e.authDiscoveryPending = true
			e.refreshQuotaWith(force, rpc, read)
			if len(e.quotas) != 4 || e.authDiscoveryPending || e.quotaRefreshing || e.quotaError == "" {
				t.Fatal("full refresh did not discover and settle failed new accounts")
			}
			for _, id := range []string{"old", "new-claude", "new-codex", "new-antigravity"} {
				if e.quotas[id].Error != "Credential could not be read" {
					t.Fatalf("account %s has no safe visible error", id)
				}
			}
			if q := e.quotas["old"]; q.Five != prior.Five || q.Week != prior.Week || q.FetchedAt != prior.FetchedAt {
				t.Fatal("failed new-account scan overwrote existing successful quota")
			}
			encoded, _ := json.Marshal(e.snapshot())
			if strings.Contains(string(encoded), "synthetic-private-token") || strings.Contains(string(encoded), "credential path") {
				t.Fatal("credential callback details leaked into the snapshot")
			}
			if e.syncAuthStatus(rpc) {
				t.Fatal("polling must not repeatedly auto-retry already attempted failures")
			}
		})
	}
}

func TestAuthDiscoveryNewQuotaFailureKeepsOtherAccounts(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	files := []any{discoveryRecord("old", "codex")}
	rpc := func(method string, payload any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": files}, nil
		}
		index := payload.(map[string]string)["auth_index"]
		return map[string]any{"json": map[string]any{"access_token": "synthetic-token-" + index}}, nil
	}
	read := func(entry AuthEntry) bool {
		return e.refreshAccountQuotaWith(entry, rpc, func(_ string, headers http.Header) ([]byte, error) {
			if headers.Get("Authorization") == "Bearer synthetic-token-index-new" {
				return nil, fmt.Errorf("private response includes synthetic-token-index-new")
			}
			return []byte(`{"rate_limit":{"primary_window":{"used_percent":37,"limit_window_seconds":18000}},"manual_reset_remaining":0}`), nil
		})
	}
	e.refreshQuotaWith(true, rpc, read)
	files = append(files, discoveryRecord("new", "codex"))
	if !e.syncAuthStatus(rpc) {
		t.Fatal("new account not discovered")
	}
	if _, ok := e.beginFullRefresh(rpc, read); !ok {
		t.Fatal("early refresh not accepted")
	}
	s := waitDiscoveryRefresh(t, e)
	if len(s.Quotas) != 2 || s.QuotaError != "" || e.quotas["old"].Five == nil || e.quotas["old"].Five.Used != 37 || e.quotas["old"].Error != "" {
		t.Fatal("one new failure affected the successful existing account")
	}
	if q := e.quotas["new"]; q.Error != "Quota endpoint unavailable" || q.Five != nil || q.FetchedAt != "" {
		t.Fatal("new quota failure is missing or fabricated quota")
	}
	encoded, _ := json.Marshal(s)
	if strings.Contains(string(encoded), "synthetic-token") || strings.Contains(string(encoded), "private response") || e.syncAuthStatus(rpc) {
		t.Fatal("new failure leaked details or triggered a polling retry loop")
	}
}

func TestAuthDiscoveryRetriesAfterBusyRefreshWithoutOverlap(t *testing.T) {
	for _, busy := range []string{"account", "full"} {
		t.Run(busy, func(t *testing.T) {
			e := newEngine()
			e.persistence.Enabled = false
			e.quotas["old"] = AccountQuota{AuthID: "old", Provider: "codex", Five: &QuotaWindow{Used: 37}}
			oldFiles := []any{discoveryRecord("old", "codex")}
			oldRPC := func(string, any) (any, error) { return map[string]any{"files": oldFiles}, nil }
			started, release := make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseOnce)
			blockedRead := func(AuthEntry) bool { close(started); <-release; return true }
			if busy == "account" {
				if status, _, _ := e.beginAccountRefresh("old", oldRPC, blockedRead); status != 202 {
					t.Fatal(status)
				}
			} else if _, ok := e.beginFullRefresh(oldRPC, blockedRead); !ok {
				t.Fatal("initial batch rejected")
			}
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("initial provider read did not start")
			}
			files := []any{} // The busy provider read targets an account CPA just removed.
			for _, provider := range []string{"claude", "codex", "antigravity"} {
				files = append(files, discoveryRecord(provider, provider))
			}
			rpc := func(string, any) (any, error) { return map[string]any{"files": files}, nil }
			var syncs sync.WaitGroup
			for range 8 {
				syncs.Add(1)
				go func() {
					defer syncs.Done()
					if !e.syncAuthStatus(rpc) {
						t.Error("discovery lost while refresh was busy")
					}
					if _, ok := e.beginFullRefresh(rpc, func(AuthEntry) bool { t.Error("overlapping read"); return false }); ok {
						t.Error("discovery bypassed the refresh lock")
					}
				}()
			}
			syncs.Wait()
			if len(e.snapshot().Quotas) != 4 {
				t.Fatal("new accounts should be visible even while the prior scan is busy")
			}
			releaseOnce()
			if busy == "account" {
				waitAccountRefresh(t, e, "old")
			} else {
				waitDiscoveryRefresh(t, e)
			}
			if !e.syncAuthStatus(rpc) || len(e.snapshot().Quotas) != 3 {
				t.Fatal("first idle sync must remove the old account and retain pending discoveries")
			}
			// CPA re-adds the same identity; it is new again, not a stale worker result.
			files = append(files, oldFiles...)
			if !e.syncAuthStatus(rpc) {
				t.Fatal("discovery during a busy batch must survive its completion")
			}
			reads, finish := make(chan string, 4), make(chan struct{})
			finishOnce := sync.OnceFunc(func() { close(finish) })
			t.Cleanup(finishOnce)
			if _, ok := e.beginFullRefresh(rpc, func(entry AuthEntry) bool {
				reads <- entry.ID
				<-finish
				e.mu.Lock()
				q := e.quotas[entry.ID]
				q.Error, q.FetchedAt = "", time.Now().UTC().Format(time.RFC3339Nano)
				q.Five = &QuotaWindow{Used: 25}
				e.quotas[entry.ID] = q
				e.mu.Unlock()
				return true
			}); !ok {
				t.Fatal("pending discovery did not resume after the busy job")
			}
			seen := map[string]bool{}
			for range 4 {
				select {
				case id := <-reads:
					seen[id] = true
				case <-time.After(3 * time.Second):
					t.Fatal("all four account reads should run concurrently")
				}
			}
			if len(seen) != 4 || e.syncAuthStatus(rpc) {
				t.Fatal("batch duplicated accounts or rediscovered pending placeholders")
			}
			if _, ok := e.beginFullRefresh(rpc, func(AuthEntry) bool { return true }); ok {
				t.Fatal("duplicate full batch accepted")
			}
			if status, _, _ := e.beginAccountRefresh("claude", rpc, func(AuthEntry) bool { return true }); status != 409 {
				t.Fatal("individual refresh must not overlap the discovery batch")
			}
			finishOnce()
			s := waitDiscoveryRefresh(t, e)
			if s.QuotaError != "" || len(s.Quotas) != 4 || e.syncAuthStatus(rpc) {
				t.Fatal("completed discovery failed or triggered another polling scan")
			}
			for _, q := range s.Quotas {
				if q.Error != "" || q.FetchedAt == "" || q.Five == nil || q.Five.UsedPercent != 25 {
					t.Fatal("new successful account did not replace its pending state")
				}
			}
		})
	}
}
