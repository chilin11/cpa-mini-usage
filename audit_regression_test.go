package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnreadableHistoryNeverOverwritten(t *testing.T) {
	good, _ := json.Marshal(Event{At: time.Now().UTC(), AuthID: "old", Model: "test", Total: 10})
	for _, broken := range []string{"{broken}", strings.Repeat("x", 2*1024*1024+1), `{"at":"invalid"}`, "null"} {
		t.Run(fmt.Sprintf("bytes=%d", len(broken)), func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			original := string(good) + "\n" + broken + "\n" + string(good) + "\n"
			path := filepath.Join(e.dir, "events.jsonl")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			e.load()
			e.capture([]byte(`{"Model":"new","Detail":{"OutputTokens":1}}`))
			e.compact()
			if _, err := e.setPersistence(true); err == nil {
				t.Error("unsafe rewrite accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != original {
				t.Fatal("unreadable history overwritten")
			}
			if e.snapshot().StorageError == "" {
				t.Fatal("storage protection not reported")
			}
		})
	}
}

func TestUnreadableQuotaAndSettingsNeverOverwritten(t *testing.T) {
	for _, name := range []string{"quotas.json", "settings.json", "prices.json"} {
		t.Run(name, func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			path := filepath.Join(e.dir, name)
			original := []byte(`{"broken":`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			e.load()
			e.recordQuotaError(AuthEntry{ID: "new"}, "offline")
			if _, err := e.setPersistence(true); err == nil {
				t.Error("unsafe save accepted")
			}
			if _, err := e.setPersistence(false); err == nil {
				t.Error("settings overwrite accepted while recovery required")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(original) {
				t.Fatal("damaged original overwritten")
			}
			if e.snapshot().StorageError == "" {
				t.Fatal("read error hidden")
			}
		})
	}
}

func TestSnapshotTiesHaveStableOrder(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	for _, id := range []string{"z", "b", "a"} {
		e.quotas[id] = AccountQuota{AuthID: id, Name: "same", Provider: "codex"}
		for _, model := range []string{"z-model", "a-model"} {
			e.events = append(e.events, Event{At: time.Now().UTC(), AuthID: id, Model: model, Total: 10})
		}
	}
	for n := 0; n < 30; n++ {
		s := e.snapshot()
		for i, id := range []string{"a", "b", "z"} {
			if s.Quotas[i].AuthID != id || s.Credentials[i].AuthID != id {
				t.Fatalf("unstable account ordering: %+v %+v", s.Quotas, s.Credentials)
			}
			if s.Credentials[i].Models[0].Model != "a-model" {
				t.Fatal("unstable model ordering")
			}
		}
	}
}

func TestFullRefreshCoversAllAccountsWithBoundedConcurrency(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	var records []any
	for i := 0; i < 65; i++ {
		records = append(records, map[string]any{"id": fmt.Sprint(i), "auth_index": fmt.Sprint(i), "provider": "codex"})
	}
	records = append(records, map[string]any{"id": "other", "auth_index": "other", "provider": "unsupported"})
	var active, maxActive atomic.Int32
	var mu sync.Mutex
	seen := map[string]int{}
	e.refreshQuotaWith(true, func(string, any) (any, error) { return records, nil }, func(a AuthEntry) bool {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		mu.Lock()
		seen[a.ID]++
		mu.Unlock()
		return true
	})
	if len(seen) != 65 {
		t.Fatalf("only %d/65 accounts refreshed", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("duplicate account %s: %d", id, n)
		}
	}
	if maxActive.Load() > 4 {
		t.Fatalf("unbounded concurrency: %d", maxActive.Load())
	}
	if e.quotaRefreshing || e.quotaRefreshFinished.IsZero() || e.quotaError != "" {
		t.Fatal("full refresh did not settle")
	}
}

func TestStorageProtectionRecoveryAndHealthyWrites(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	p := filepath.Join(e.dir, "events.jsonl")
	if err := os.WriteFile(p, []byte("bad\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e.load()
	if !e.snapshot().Persistence.ReadOnly {
		t.Fatal("read-only state missing")
	}
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	repaired := newEngine()
	repaired.dir = e.dir
	repaired.load()
	if repaired.snapshot().Persistence.ReadOnly {
		t.Fatal("repaired file did not recover after replacement")
	}
	repaired.capture([]byte(`{"Model":"new","Detail":{"OutputTokens":1}}`))
	raw, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(raw), `"model":"new"`) {
		t.Fatal("healthy persistence broken")
	}
}

func TestAtomicWriteUsesPrivateTemporaryFilesAndCleansFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "data.json")
	if err := writeAtomic(p, []byte("original")); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe permissions")
	}
	target := filepath.Join(dir, "directory")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(target, []byte("replacement")); err == nil {
		t.Fatal("expected rename error")
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}

func TestInvalidComputedCostCannotBreakSnapshotJSON(t *testing.T) {
	for _, price := range []float64{math.MaxFloat64, -1, math.Inf(1), math.NaN()} {
		e := newEngine()
		e.persistence.Enabled = false
		e.prices["openai/test"] = ModelPrice{Input: price, Output: price}
		e.events = []Event{{At: time.Now().UTC(), AuthID: "a", Provider: "codex", Model: "test", Input: 1000, Output: 1000, Total: 2000}}
		s := e.snapshot()
		if _, err := json.Marshal(s); err != nil {
			t.Fatalf("invalid price breaks data API: %v", err)
		}
		if !s.Recent[0].MissingPrice || s.Recent[0].Cost != nil || s.Credentials[0].Cost != nil {
			t.Fatal("invalid price is reported as known")
		}
	}
}

func TestDataEncodingFailureReturnsExplicitError(t *testing.T) {
	old := tracker
	tracker = newEngine()
	tracker.persistence.Enabled = false
	defer func() { tracker = old }()
	tracker.events = []Event{{At: time.Now().UTC(), Model: "test", LatencyMS: floatPtr(math.Inf(1))}}
	var response struct {
		OK     bool
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if err := json.Unmarshal(dispatch("management.handle", []byte(`{"Method":"GET","Path":"/v0/management/plugins/cpa-mini-usage/data"}`)), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Result.StatusCode != 500 || string(response.Result.Body) != `{"error":"snapshot_encoding_failed"}` {
		t.Fatalf("empty or misleading response: %+v", response)
	}
}
