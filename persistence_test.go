package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageTimingsAreNanoseconds(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.capture([]byte(`{"AuthID":"a","Provider":"codex","Stream":true,"Latency":2500000000,"TTFT":500000000,"Detail":{"InputTokens":900,"OutputTokens":100,"TotalTokens":1000}}`))
	ev := e.snapshot().Recent[0]
	if ev.LatencyMS == nil || *ev.LatencyMS != 2500 || ev.TTFTMS == nil || *ev.TTFTMS != 500 {
		t.Fatalf("timings %+v", ev)
	}
	if got := float64(ev.Output) / ((*ev.LatencyMS - *ev.TTFTMS) / 1000); got != 50 {
		t.Fatalf("TPS %v", got)
	}
	for _, body := range []string{`{"Stream":false,"Latency":2500000000,"TTFT":500000000}`, `{"Stream":true,"Latency":0,"TTFT":0}`, `{"Latency":-1,"TTFT":-1}`, `{"Latency":"2500000000","TTFT":"500000000"}`, `{}`} {
		e.capture([]byte(body))
		ev = e.snapshot().Recent[0]
		if ev.TTFTMS != nil {
			t.Fatalf("unmeasured TTFT should be absent: %s", body)
		}
	}
	for _, n := range []float64{math.NaN(), math.Inf(1), -1, 0, float64(math.MaxInt64)} {
		if durationMilliseconds(map[string]any{"TTFT": n}, "TTFT") != nil {
			t.Fatal("invalid duration accepted")
		}
	}
	if got := durationMilliseconds(map[string]any{"TTFT": json.Number("1250000")}, "TTFT"); got == nil || *got != 1.25 {
		t.Fatal("fractional ms lost")
	}
}

func TestPersistenceSurvivesEngineReplacement(t *testing.T) {
	dir := t.TempDir()
	e := newEngine()
	e.dir = dir
	e.load()
	e.capture([]byte(`{"AuthID":"index-a","Provider":"codex","Model":"test","Stream":true,"Latency":2500000000,"TTFT":500000000,"APIKey":"SECRET_KEY","Detail":{"OutputTokens":100,"TotalTokens":100}}`))
	e.quotas["a"] = AccountQuota{AuthID: "a", Name: "alice@example.com", Provider: "codex", Five: &QuotaWindow{Used: 25, ResetAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}
	e.names["a"] = "alice@example.com"
	e.aliases["index-a"] = "a"
	e.quotaUpdated = time.Now().UTC()
	e.prices["openai/test"] = ModelPrice{Input: 2, Output: 8}
	e.priceUpdated = time.Now().UTC()
	settings, err := e.setPersistence(true)
	if err != nil || !settings.Enabled || settings.Directory != dir {
		t.Fatalf("settings %+v %v", settings, err)
	}
	replacement := newEngine()
	replacement.dir = dir
	replacement.load()
	state := replacement.snapshot()
	if !state.Persistence.Enabled || len(state.Recent) != 1 || state.Recent[0].TTFTMS == nil || *state.Recent[0].TTFTMS != 500 || state.Recent[0].AuthID != "a" || len(state.Quotas) != 1 || state.Quotas[0].Name != "alice@example.com" || len(replacement.prices) != 1 {
		t.Fatalf("restored %+v", state)
	}
	for _, name := range []string{"events.jsonl", "quotas.json", "settings.json", "prices.json"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || strings.Contains(string(body), "SECRET_KEY") || strings.Contains(string(body), "access_token") {
			t.Fatalf("unsafe/missing %s %v", name, err)
		}
		info, _ := os.Stat(filepath.Join(dir, name))
		if info.Mode().Perm() != 0600 {
			t.Fatalf("permissions %s %v", name, info.Mode())
		}
	}
}

func TestPersistenceDisableDoesNotDeleteHistoryAndReenableFlushes(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	e.capture([]byte(`{"AuthID":"a","Detail":{"OutputTokens":1}}`))
	e.quotas["a"] = AccountQuota{AuthID: "a", Name: "old"}
	e.mu.Lock()
	e.persistQuotasLocked()
	e.mu.Unlock()
	requestFile := filepath.Join(e.dir, "events.jsonl")
	quotaFile := filepath.Join(e.dir, "quotas.json")
	before, _ := os.ReadFile(requestFile)
	beforeQuota, _ := os.ReadFile(quotaFile)
	if _, err := e.setPersistence(false); err != nil {
		t.Fatal(err)
	}
	e.capture([]byte(`{"AuthID":"b","Detail":{"OutputTokens":2}}`))
	e.recordQuotaError(AuthEntry{ID: "a", Provider: "codex", Name: "new"}, "failure")
	e.compact()
	after, _ := os.ReadFile(requestFile)
	afterQuota, _ := os.ReadFile(quotaFile)
	if string(before) != string(after) || string(beforeQuota) != string(afterQuota) {
		t.Fatal("disabled persistence changed prior history")
	}
	replacement := newEngine()
	replacement.dir = e.dir
	replacement.load()
	if replacement.persistence.Enabled || len(replacement.events) != 1 || replacement.quotas["a"].Name != "old" {
		t.Fatal("disabled setting/history not restored")
	}
	if _, err := e.setPersistence(true); err != nil {
		t.Fatal(err)
	}
	replacement = newEngine()
	replacement.dir = e.dir
	replacement.load()
	if !replacement.persistence.Enabled || len(replacement.events) != 2 || replacement.quotas["a"].Name != "new" {
		t.Fatal("reenable did not flush in-memory data")
	}
}

func TestPersistenceSettingFailurePreservesChoice(t *testing.T) {
	e := newEngine()
	dir := t.TempDir()
	e.dir = filepath.Join(dir, "blocked")
	os.WriteFile(e.dir, []byte("file"), 0600)
	if _, err := e.setPersistence(false); err == nil || !e.persistence.Enabled {
		t.Fatal("failed settings write changed enabled")
	}
}

func TestOldEventsAndMissingSettingsRemainCompatible(t *testing.T) {
	dir := t.TempDir()
	body := `{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","auth_id":"old","output":100,"total":100}` + "\n"
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0600)
	e := newEngine()
	e.dir = dir
	e.load()
	if !e.persistence.Enabled || len(e.events) != 1 || e.events[0].TTFTMS != nil || e.events[0].LatencyMS != nil {
		t.Fatal("old history incompatible")
	}
}

func TestPersistenceSettingsManagementRoute(t *testing.T) {
	old := tracker
	tracker = newEngine()
	tracker.dir = t.TempDir()
	defer func() { tracker = old }()
	for _, tc := range []struct {
		payload string
		status  int
	}{{`{}`, 400}, {`{"persistence_enabled":null}`, 400}, {`{"persistence_enabled":"false"}`, 400}, {`{"persistence_enabled":false}`, 200}, {`{"persistence_enabled":true}`, 200}} {
		body, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/v0/management/plugins/cpa-mini-usage/settings", "Body": []byte(tc.payload)})
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
}
