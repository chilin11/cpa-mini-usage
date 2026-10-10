package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cachedForecastFixture(t *testing.T) (*Engine, time.Time, AccountQuota) {
	t.Helper()
	e, now, win := forecastFixture()
	e.dir = t.TempDir()
	e.events = []Event{forecastEvent(now.Add(-time.Minute), 100000, 100000)}
	q := AccountQuota{AuthID: "a", Provider: "codex", FetchedAt: now.Format(time.RFC3339Nano), Five: win}
	return e, now, q
}
func TestForecastRetainedUntilCycleReset(t *testing.T) {
	e, now, q := cachedForecastFixture(t)
	first, changed := e.resolveForecastLocked(q, "5h", now)
	if !changed || first.EstimatedTotalTokens == nil || *first.EstimatedTotalTokens != 400000 || first.EstimatedTotalCost == nil || *first.EstimatedTotalCost != .4 {
		t.Fatalf("first %+v", first)
	}
	_, changed = e.resolveForecastLocked(q, "5h", now)
	if changed {
		t.Fatal("unchanged snapshot rewrote cache")
	}
	q.Five.Used = 0
	q.FetchedAt = now.Add(time.Minute).Format(time.RFC3339Nano)
	e.events = append(e.events, forecastEvent(now.Add(time.Minute), 10000, 10000))
	retained, _ := e.resolveForecastLocked(q, "5h", now.Add(time.Minute))
	if !retained.EstimateRetained || retained.EstimatedTotalTokens == nil || *retained.EstimatedTotalTokens != 400000 || retained.EstimatedTotalCost == nil || *retained.EstimatedTotalCost != .4 || retained.ObservedTokens != 110000 || retained.UsedPercent != 0 || retained.EstimateUsedPercent == nil || *retained.EstimateUsedPercent != 25 {
		t.Fatalf("zero quota erased last estimate %+v", retained)
	}
	q.Five.Used = 50
	updated, _ := e.resolveForecastLocked(q, "5h", now.Add(time.Minute))
	if updated.EstimateRetained || *updated.EstimatedTotalTokens != 220000 || *updated.EstimatedTotalCost != .22 {
		t.Fatalf("valid new sample must replace %+v", updated)
	}
	q.Five.Used = 100
	full, _ := e.resolveForecastLocked(q, "5h", now.Add(time.Minute))
	if *full.EstimatedTotalTokens != 110000 || *full.EstimatedTotalCost != .11 {
		t.Fatalf("fully used quota must still show total %+v", full)
	}
	reset, _ := time.Parse(time.RFC3339Nano, q.Five.ResetAt)
	expired, _ := e.resolveForecastLocked(q, "5h", reset)
	assertNoForecast(t, expired)
	if len(e.forecasts) != 0 {
		t.Fatal("expired forecast not removed")
	}
	q.Five = &QuotaWindow{Used: 0, ResetAt: reset.Add(5 * time.Hour).Format(time.RFC3339Nano)}
	next, _ := e.resolveForecastLocked(q, "5h", reset.Add(time.Minute))
	if next.EstimateRetained {
		t.Fatal("new cycle reused old saved forecast")
	}
}
func TestCachedForecastFloorsMissingPriceAndIsolation(t *testing.T) {
	e, now, q := cachedForecastFixture(t)
	e.resolveForecastLocked(q, "5h", now)
	e.events = append(e.events, forecastEvent(now.Add(time.Minute), 400000, 400000))
	q.Five.Used = 0
	q.FetchedAt = now.Add(time.Minute).Format(time.RFC3339Nano)
	floor, _ := e.resolveForecastLocked(q, "5h", now.Add(time.Minute))
	if !floor.EstimateRetained || *floor.EstimatedTotalTokens != 500000 || *floor.EstimatedTotalCost != .5 {
		t.Fatalf("retained estimate below actual %+v", floor)
	}
	q.Five.Used = 25
	e.events[0].Model = "missing"
	e.events[1].Model = "missing"
	unknown, _ := e.resolveForecastLocked(q, "5h", now.Add(time.Minute))
	if !unknown.EstimateRetained || unknown.ObservedCost != nil || unknown.EstimatedTotalCost == nil {
		t.Fatalf("unknown new price erased prior cost %+v", unknown)
	}
	other := q
	other.AuthID = "other"
	win, _ := e.resolveForecastLocked(other, "5h", now.Add(time.Minute))
	assertNoForecast(t, win)
	week := q
	week.Week = &QuotaWindow{Used: 0, ResetAt: now.Add(24 * time.Hour).Format(time.RFC3339Nano)}
	win, _ = e.resolveForecastLocked(week, "7d", now.Add(time.Minute))
	assertNoForecast(t, win)
	q.Five = &QuotaWindow{Used: 0, ResetAt: now.Add(2 * time.Hour).Format(time.RFC3339Nano)}
	win, _ = e.resolveForecastLocked(q, "5h", now.Add(time.Minute))
	if win.EstimateRetained {
		t.Fatal("changed reset must invalidate")
	}
}
func TestForecastCachePersistsAndDisablePreservesFile(t *testing.T) {
	e, _, q := cachedForecastFixture(t)
	now := time.Now().UTC()
	q.Five.ResetAt = now.Add(time.Hour).Format(time.RFC3339Nano)
	q.FetchedAt = now.Format(time.RFC3339Nano)
	e.events = []Event{forecastEvent(now.Add(-time.Minute), 100000, 100000)}
	e.quotas["a"] = q
	e.snapshot()
	q.Five.Used = 0
	e.quotas["a"] = q
	if _, err := e.setPersistence(true); err != nil {
		t.Fatal(err)
	}
	restored := newEngine()
	restored.dir = e.dir
	restored.load()
	state := restored.snapshot()
	win := state.Quotas[0].Five
	if !win.EstimateRetained || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 400000 || win.EstimatedTotalCost == nil || *win.EstimatedTotalCost != .4 {
		t.Fatalf("restart erased forecast %+v", win)
	}
	if _, err := restored.setPersistence(false); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(e.dir, "quotas.json")
	before, _ := os.ReadFile(file)
	restored.prices["openai/test"] = ModelPrice{Input: 2}
	q.Five.Used = 50
	restored.quotas["a"] = q
	restored.snapshot()
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("disabled persistence wrote cache")
	}
}
func TestQuotaRefreshRemembersOldForecastWithoutDashboardRead(t *testing.T) {
	e, now, q := cachedForecastFixture(t)
	e.rememberQuotaForecastsLocked(q, now)
	q.Five.Used = 0
	e.rememberQuotaForecastsLocked(q, now)
	win, _ := e.resolveForecastLocked(q, "5h", now)
	if !win.EstimateRetained || win.EstimatedTotalTokens == nil {
		t.Fatal("refresh-before-snapshot lost valid forecast")
	}
}

func TestUnusedFiveHourIgnoresCachedAndPreviousTotals(t *testing.T) {
	e, now, q := cachedForecastFixture(t)
	e.resolveForecastLocked(q, "5h", now)
	// Simulate an empty current window while an old forecast and history exist.
	e.events = []Event{forecastEvent(now.Add(-6*time.Hour), 7090000, 1980000)}
	q.Five.Used = 0
	unused, _ := e.resolveForecastLocked(q, "5h", now)
	assertUnusedForecast(t, unused)
	// Actual new usage must still be able to use the valid same-cycle cache.
	e.events = append(e.events, forecastEvent(now, 1000, 1000))
	used, _ := e.resolveForecastLocked(q, "5h", now)
	if !used.EstimateRetained || used.EstimatedTotalTokens == nil || *used.EstimatedTotalTokens != 400000 {
		t.Fatalf("new usage lost valid same-cycle forecast: %+v", used)
	}
}
