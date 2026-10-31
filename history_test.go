package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func historyTestQuery(t *testing.T, start, end time.Time, limit int) historyQuery {
	t.Helper()
	q, err := parseHistoryQuery(url.Values{"start": {start.Format(time.RFC3339Nano)}, "end": {end.Format(time.RFC3339Nano)}, "limit": {fmt.Sprint(limit)}})
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func captureHistoryEvent(e *Engine, at time.Time, model, response, provider string) {
	b, _ := json.Marshal(map[string]any{"RequestedAt": at.Format(time.RFC3339Nano), "Model": model, "ResponseModel": response, "Provider": provider, "AuthID": "account", "ReasoningEffort": "high", "Detail": map[string]int{"InputTokens": 100, "OutputTokens": 10}})
	e.capture(b)
}
func TestHistoryQueryValidationAndLocalDates(t *testing.T) {
	// America/New_York spring-forward day: midnight to next midnight is 23h.
	base := url.Values{"start": {"2026-03-08T00:00:00-05:00"}, "end": {"2026-03-09T00:00:00-04:00"}}
	q, err := parseHistoryQuery(base)
	if err != nil || q.End.Sub(q.Start) != 23*time.Hour || q.Limit != 20 {
		t.Fatalf("%+v %v", q, err)
	}
	for _, change := range []url.Values{
		{"start": {"bad"}}, {"start": {"2026-03-09T00:00:00-04:00"}}, {"end": {"2026-05-09T00:00:00Z"}},
		{"start": {"2026-03-08"}}, {"limit": {"0"}}, {"limit": {"61"}}, {"limit": {"-1"}}, {"limit": {"1e2"}}, {"limit": {"20", "40"}},
		{"model_status": {"mismatch"}}, {"provider": {"Codex"}}, {"provider": {"codex/../../"}}, {"provider": {strings.Repeat("a", 65)}},
		{"cursor": {strings.Repeat("x", 2049)}}, {"offset": {"0"}},
	} {
		values := url.Values{}
		for k, v := range base {
			values[k] = v
		}
		for k, v := range change {
			values[k] = v
		}
		if _, err := parseHistoryQuery(values); err == nil {
			t.Fatalf("accepted %v", values)
		}
	}
	for _, status := range []string{"", "same", "different", "variant", "unknown"} {
		v := url.Values{}
		for k, values := range base {
			v[k] = values
		}
		v.Set("model_status", status)
		v.Set("provider", "custom-provider")
		if _, err := parseHistoryQuery(v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryBeyondSnapshotAndStablePagination(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	now := time.Now().UTC()
	old := now.Add(-12 * 24 * time.Hour)
	// Same timestamps and out-of-order delivery must have a stable total order.
	for i := 0; i < 145; i++ {
		at := old.Add(time.Duration(i%7) * time.Minute)
		captureHistoryEvent(e, at, fmt.Sprintf("m-%03d", i), "different", "codex")
	}
	captureHistoryEvent(e, now.Add(-time.Minute), "new", "new", "claude")
	captureHistoryEvent(e, now.Add(-time.Minute), "m", "m-20261001", "codex")
	captureHistoryEvent(e, now.Add(-time.Minute), "m", "", "codex")
	captureHistoryEvent(e, now.Add(time.Hour), "future", "different", "codex")
	if s := e.snapshot(); len(s.Recent) != 3 || s.TotalRequests != 3 {
		t.Fatalf("seven-day stats changed: %+v", s)
	}
	q := historyTestQuery(t, old.Add(-time.Hour), old.Add(24*time.Hour), 20)
	q.Provider = "codex"
	q.ModelStatus = "different"
	seen := map[string]bool{}
	var last time.Time
	var lastSequence uint64
	var firstToken, firstPageCursor string
	for pageNo := 0; ; pageNo++ {
		p, status, failure := e.history(q)
		if status != 200 {
			t.Fatalf("%d %s", status, failure)
		}
		if p.Total != 145 || p.Offset != pageNo*20 || p.Retention.Days != 35 || p.Retention.MaxRecords != 200000 || p.Retention.RetainedRecords != 148 {
			t.Fatalf("page: %+v", p)
		}
		for _, ev := range p.Events {
			if seen[ev.Model] {
				t.Fatalf("duplicate %s", ev.Model)
			}
			seen[ev.Model] = true
			if !last.IsZero() && (ev.At.After(last) || ev.At.Equal(last) && ev.sequence >= lastSequence) {
				t.Fatal("not stable descending")
			}
			last = ev.At
			lastSequence = ev.sequence
			if ev.ReasoningEffort != "high" || ev.ReasoningEffortSource != "host_usage" {
				t.Fatal("lost reasoning metadata")
			}
		}
		if pageNo == 0 {
			firstToken = p.NextCursor
			firstPageCursor = p.Cursor
			captureHistoryEvent(e, old.Add(30*time.Minute), "appended", "different", "codex")
		}
		if p.NextCursor == "" {
			break
		}
		q.Cursor = p.NextCursor
	}
	if len(seen) != 145 || seen["appended"] {
		t.Fatalf("anchored results: %d", len(seen))
	}
	q.Cursor = firstToken
	p, _, _ := e.history(q)
	if p.Offset != 20 || p.Events[0].Model == "appended" {
		t.Fatal("cursor replay changed")
	}
	q.Cursor = firstPageCursor
	p, status, _ := e.history(q)
	if status != 200 || p.Offset != 0 || p.Total != 145 {
		t.Fatal("returning to page one changed the anchor")
	}
}

func TestHistoryFiltersBoundariesAndValuation(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	e.prices["openai/m"] = ModelPrice{Input: 2, Output: 8}
	e.names["account"] = "Old credential"
	start := time.Now().UTC().Add(-10 * 24 * time.Hour)
	end := start.Add(24 * time.Hour)
	for _, tc := range []struct {
		at                 time.Time
		response, provider string
	}{{start.Add(-time.Nanosecond), "other", "codex"}, {start, "other", "codex"}, {end.Add(-time.Nanosecond), "m", "codex"}, {end, "other", "codex"}, {start, "m-20260101", "codex"}, {start, "", "codex"}, {start, "other", "claude"}} {
		captureHistoryEvent(e, tc.at, "m", tc.response, tc.provider)
	}
	q := historyTestQuery(t, start, end, 60)
	q.Provider = "codex"
	p, _, _ := e.history(q)
	if p.Total != 4 {
		t.Fatalf("boundaries: %+v", p)
	}
	for _, status := range []string{"different", "same", "variant", "unknown"} {
		q.ModelStatus = status
		p, code, _ := e.history(q)
		if code != 200 || p.Total != 1 || p.Events[0].ModelStatus != status || p.Events[0].CredentialName != "Old credential" || p.Events[0].Cost == nil {
			t.Fatalf("%s %+v", status, p)
		}
	}
	q.Provider = "missing"
	p, _, _ = e.history(q)
	if p.Total != 0 || p.Events == nil {
		t.Fatal("empty not []")
	}
}

func TestHistoryCursorSafety(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		captureHistoryEvent(e, now.Add(-time.Minute), fmt.Sprint(i), "other", "codex")
	}
	q := historyTestQuery(t, now.Add(-time.Hour), now.Add(time.Hour), 1)
	p, _, _ := e.history(q)
	q.Cursor = p.NextCursor
	for _, change := range []func(*historyQuery){func(q *historyQuery) { q.Provider = "claude" }, func(q *historyQuery) { q.ModelStatus = "different" }, func(q *historyQuery) { q.Limit = 2 }, func(q *historyQuery) { q.End = q.End.Add(time.Second) }, func(q *historyQuery) { q.Cursor += "x" }} {
		v := q
		change(&v)
		if _, status, _ := e.history(v); status != 400 {
			t.Fatalf("accepted changed query %d", status)
		}
	}
	other := newEngine()
	if _, status, _ := other.history(q); status != 400 {
		t.Fatal("restart cursor accepted")
	}
	var c historyCursor
	e.decodeHistoryCursor(q.Cursor, &c)
	c.AsOf = now.Add(-historyCursorTTL - time.Second)
	q.Cursor = e.signHistoryCursor(c)
	if _, status, _ := e.history(q); status != 409 {
		t.Fatal("expired cursor accepted")
	}
	q.Cursor = p.NextCursor
	e.mu.Lock()
	e.historyRevision++
	e.mu.Unlock()
	if _, status, _ := e.history(q); status != 409 {
		t.Fatal("removal silently shifted pagination")
	}
}

func TestHistoryRetentionAndConcurrentCapture(t *testing.T) {
	e := newEngine()
	e.persistence.Enabled = false
	now := time.Now().UTC()
	// Over-cap fixture avoids 200k file writes; the next capture applies the cap.
	e.events = make([]Event, maxHistoryRecords)
	for i := range e.events {
		e.events[i] = Event{At: now.Add(-36 * 24 * time.Hour), sequence: uint64(i + 1)}
	}
	e.eventSequence = maxHistoryRecords
	captureHistoryEvent(e, now.Add(-time.Minute), "retained", "other", "codex")
	if len(e.events) != maxHistoryRecords || e.historyRevision != 1 {
		t.Fatal("record cap not enforced")
	}
	q := historyTestQuery(t, now.Add(-34*24*time.Hour), now.Add(time.Hour), 20)
	p, _, _ := e.history(q)
	if p.Total != 1 || p.Retention.RetainedRecords != 1 {
		t.Fatal("expired events exposed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				captureHistoryEvent(e, now, "concurrent", "other", "codex")
				if _, status, _ := e.history(q); status != 200 {
					t.Error(status)
				}
			}
		}()
	}
	wg.Wait()
}

func TestReasoningEffortCapturePersistenceAndLegacy(t *testing.T) {
	for _, raw := range []string{`{"ReasoningEffort":"xhigh"}`, `{"record":{"reasoning_effort":"xhigh"}}`} {
		e := newEngine()
		e.dir = t.TempDir()
		e.load()
		e.capture([]byte(raw))
		restored := newEngine()
		restored.dir = e.dir
		restored.load()
		ev := restored.snapshot().Recent[0]
		if ev.ReasoningEffort != "xhigh" || ev.ReasoningEffortSource != "host_usage" {
			t.Fatalf("not persisted: %+v", ev)
		}
	}
	for _, raw := range []string{`{"Model":"gpt(high)","Alias":"gpt(xhigh)","Detail":{"ReasoningTokens":999}}`, `{"ReasoningEffort":3}`, `{"ReasoningEffort":"bad\nvalue"}`, `{"ReasoningEffort":"` + strings.Repeat("x", 129) + `"}`} {
		e := newEngine()
		e.persistence.Enabled = false
		e.capture([]byte(raw))
		ev := e.snapshot().Recent[0]
		if ev.ReasoningEffort != "" || ev.ReasoningEffortSource != "" {
			t.Fatalf("invented reasoning: %+v", ev)
		}
	}
	e := newEngine()
	e.dir = t.TempDir()
	raw := fmt.Sprintf("{\"at\":%q,\"model\":\"gpt(high)\",\"input\":1}\n", time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(e.dir, "events.jsonl"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	e.load()
	ev := e.snapshot().Recent[0]
	if ev.ReasoningEffort != "" || ev.ReasoningEffortSource != "" {
		t.Fatal("legacy backfilled")
	}
}

func TestHistoryManagementContract(t *testing.T) {
	var registration struct {
		OK     bool
		Result struct {
			Routes []struct{ Method, Path string }
		}
	}
	if err := json.Unmarshal(dispatch("management.register", []byte(`{}`)), &registration); err != nil {
		t.Fatal(err)
	}
	registered := false
	for _, route := range registration.Result.Routes {
		if route.Method == "GET" && route.Path == "/plugins/"+pluginID+"/history" {
			registered = true
		}
	}
	if !registration.OK || !registered {
		t.Fatal("history GET route is not registered with CPA")
	}

	old := tracker
	tracker = newEngine()
	tracker.persistence.Enabled = false
	defer func() { tracker = old }()
	now := time.Now().UTC()
	captureHistoryEvent(tracker, now.Add(-10*24*time.Hour), "m", "other", "codex")
	q := url.Values{"start": {now.Add(-12 * 24 * time.Hour).Format(time.RFC3339)}, "end": {now.Format(time.RFC3339)}, "provider": {"codex"}, "model_status": {"different"}}
	for _, tc := range []struct {
		method string
		query  url.Values
		status int
	}{{"GET", q, 200}, {"GET", nil, 400}, {"POST", q, 404}} {
		body, _ := json.Marshal(map[string]any{"Method": tc.method, "Path": "/v0/management/plugins/cpa-mini-usage/history", "Query": tc.query})
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
			t.Fatalf("%+v", response)
		}
		if tc.status == 200 {
			var p HistoryPage
			if err := json.Unmarshal(response.Result.Body, &p); err != nil || p.Total != 1 || p.Events[0].ReasoningEffort != "high" {
				t.Fatalf("wire page %+v %v", p, err)
			}
		}
	}
}
