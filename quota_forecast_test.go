package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func forecastFixture() (*Engine, time.Time, *QuotaWindow) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	e := newEngine()
	e.prices["openai/test"] = ModelPrice{Input: 1}
	return e, now, &QuotaWindow{Used: 25, ResetAt: now.Add(time.Hour).Format(time.RFC3339Nano)}
}

func forecastEvent(at time.Time, tokens, input int64) Event {
	return Event{At: at, AuthID: "a", Provider: "codex", Model: "test", Total: tokens, Input: input}
}

func assertNoForecast(t *testing.T, win *WindowEstimate) {
	t.Helper()
	if win.EstimatedTotalTokens != nil || win.EstimatedTotalCost != nil || win.EstimatedBasis != "" {
		t.Fatalf("unexpected forecast: %+v", win)
	}
}

func TestManagerPlusForecastExamples(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		used, cost, wantTokens, wantCost float64
	}{
		{"one percent", 1, .02, 640000, 2},
		{"seven percent", 7, .06, 91429, .86},
		{"complete", 100, .06, 6400, .06},
		{"rounding floor", 100, .001185, 6400, .001185},
		{"round down cannot undercut actual", 99, .004, 6465, .004},
		{"explicit zero", 7, 0, 91429, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, now, q := forecastFixture()
			q.Used = tc.used
			e.prices["openai/test"] = ModelPrice{Input: tc.cost * 1e6}
			e.events = []Event{forecastEvent(now, 6400, 1)}
			win := e.windowSummary("a", now, q, 5*time.Hour)
			if win.EstimatedBasis != "quota" || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != tc.wantTokens || win.EstimatedTotalCost == nil || math.Abs(*win.EstimatedTotalCost-tc.wantCost) > 1e-12 {
				t.Fatalf("forecast %+v, want %.0f / %v", win, tc.wantTokens, tc.wantCost)
			}
		})
	}
}

func TestForecastInvalidPercentAndOverflow(t *testing.T) {
	for _, used := range []float64{0, -1, 101, math.NaN(), math.Inf(1), math.Inf(-1), math.SmallestNonzeroFloat64, 1e-307} {
		e, now, q := forecastFixture()
		q.Used = used
		e.events = []Event{forecastEvent(now, 6400, 20000)}
		win := e.windowSummary("a", now, q, 5*time.Hour)
		assertNoForecast(t, win)
		if win.ObservedTokens != 6400 || win.ObservedCost == nil {
			t.Fatalf("invalid percent hid actuals: %+v", win)
		}
		if _, err := json.Marshal(win); err != nil {
			t.Fatalf("invalid percent broke JSON: %v", err)
		}
	}
	e, now, q := forecastFixture()
	e.events = []Event{forecastEvent(now, 1, 1)}
	e.prices["openai/test"] = ModelPrice{Input: 1e308}
	q.Used = .0001
	win := e.windowSummary("a", now, q, 5*time.Hour)
	if win.EstimatedTotalCost == nil || !finite(*win.EstimatedTotalCost) || math.Abs(*win.EstimatedTotalCost/1e308-1) > 1e-12 {
		t.Fatalf("scaled cost overflow should preserve finite result: %+v", win)
	}
	q.Used = .00001 // The unscaled result itself overflows: do not emit it.
	assertNoForecast(t, e.windowSummary("a", now, q, 5*time.Hour))
}

func TestForecastPreviousAdjacentWindowAndBasis(t *testing.T) {
	e, now, q := forecastFixture()
	start := now.Add(-4 * time.Hour)
	e.aliases["index"] = "a"
	previous := forecastEvent(start.Add(-5*time.Hour), 200000, 200000)
	previous.AuthID = "index"
	e.events = []Event{
		forecastEvent(start.Add(-5*time.Hour-time.Nanosecond), 9000000, 9000000), // Too old.
		previous, // Inclusive previous start, canonical identity.
		forecastEvent(start.Add(-time.Nanosecond), 300000, 300000), // Previous end excluded.
		forecastEvent(start, 100000, 100000),                       // Inclusive current start, not previous.
		forecastEvent(now, 50000, 50000),                           // Inclusive now.
		forecastEvent(now.Add(time.Nanosecond), 9000000, 9000000),  // Future ignored.
	}
	other := forecastEvent(start.Add(-time.Hour), 9000000, 9000000)
	other.AuthID = "other"
	e.events = append(e.events, other)
	for _, used := range []float64{0, -1, 101, math.NaN(), math.Inf(1), math.SmallestNonzeroFloat64} {
		q.Used = used
		win := e.windowSummary("a", now, q, 5*time.Hour)
		if win.ObservedTokens != 150000 || win.ObservedCost == nil || math.Abs(*win.ObservedCost-.15) > 1e-12 || win.EstimatedBasis != "previous" || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 500000 || win.EstimatedTotalCost == nil || *win.EstimatedTotalCost != .5 {
			t.Fatalf("adjacent fallback used=%v: %+v", used, win)
		}
		b, err := json.Marshal(win)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if json.Unmarshal(b, &obj) != nil || obj["estimated_basis"] != "previous" {
			t.Fatalf("basis JSON %s", b)
		}
	}
	// A previous range is the full duration, not the elapsed portion of current.
	q.Seconds = int64((2 * time.Hour) / time.Second)
	q.Used = 0
	e.events = []Event{forecastEvent(now.Add(-2*time.Hour), 10000, 10000)}
	win := e.windowSummary("a", now, q, 5*time.Hour)
	if win.EstimatedBasis != "previous" || *win.EstimatedTotalTokens != 10000 || win.ObservedTokens != 0 {
		t.Fatalf("declared duration ignored: %+v", win)
	}
}

func TestForecastPreviousEvidenceAndTooSmallGuard(t *testing.T) {
	for _, tc := range []struct {
		name                                                       string
		currentTokens, currentInput, previousTokens, previousInput int64
		want                                                       bool
	}{
		{"previous only", 0, 0, 500, 1000, true},
		{"equal components", 500, 1000, 500, 1000, true},
		{"too few tokens", 501, 1000, 500, 1000, false},
		{"too little cost", 400, 1001, 500, 1000, false},
		{"previous known zero", 0, 0, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, now, q := forecastFixture()
			q.Used = 0
			e.events = []Event{forecastEvent(now.Add(-6*time.Hour), tc.previousTokens, tc.previousInput)}
			if tc.currentTokens > 0 || tc.currentInput > 0 {
				e.events = append(e.events, forecastEvent(now, tc.currentTokens, tc.currentInput))
			}
			win := e.windowSummary("a", now, q, 5*time.Hour)
			if !tc.want {
				assertNoForecast(t, win)
			} else if win.EstimatedBasis != "previous" || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != float64(tc.previousTokens) || win.EstimatedTotalCost == nil || *win.EstimatedTotalCost != float64(tc.previousInput)/1e6 {
				t.Fatalf("previous actuals %+v", win)
			}
		})
	}
	e, now, q := forecastFixture()
	e.events = []Event{forecastEvent(now, 500, 1000)}
	q.Used = 0
	assertNoForecast(t, e.windowSummary("a", now, q, 5*time.Hour))
	e.events = nil
	q.Used = 25
	assertNoForecast(t, e.windowSummary("a", now, q, 5*time.Hour))
	e.events = []Event{forecastEvent(now.Add(-6*time.Hour), 50000, 50000)}
	win := e.windowSummary("a", now, q, 5*time.Hour)
	if win.EstimatedBasis != "previous" || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 50000 || win.EstimatedTotalCost == nil || *win.EstimatedTotalCost != .05 {
		t.Fatalf("valid ratio with no current records must fall back: %+v", win)
	}
}

func TestForecastObservationSafety(t *testing.T) {
	e, now, q := forecastFixture()
	e.events = []Event{forecastEvent(now.Add(-6*time.Hour), 50000, 50000), forecastEvent(now, 5000, 5000)}
	for _, observation := range []time.Time{now.Add(-5 * time.Hour), now.Add(time.Nanosecond)} {
		for _, used := range []float64{25, 0} {
			q.Used = used
			win := e.windowSummaryObserved("a", now, observation, q, 5*time.Hour)
			if win.ObservedTokens != 5000 || win.ObservedCost == nil || *win.ObservedCost != .005 {
				t.Fatalf("unsafe observation hid actuals %+v", win)
			}
			assertNoForecast(t, win)
		}
	}
	q.Used = 25
	win := e.windowSummaryObserved("a", now, now, q, 5*time.Hour)
	if win.EstimatedBasis != "quota" {
		t.Fatal("equal event and quota observation is safe")
	}
	// Missing observation cannot create a quota forecast; recorded history can
	// still supply the independent previous-period fallback, as upstream does.
	win = e.windowSummaryObserved("a", now, time.Time{}, q, 5*time.Hour)
	if win.EstimatedBasis != "previous" {
		t.Fatalf("missing observation fallback %+v", win)
	}
	e.events = e.events[1:]
	assertNoForecast(t, e.windowSummaryObserved("a", now, time.Time{}, q, 5*time.Hour))
}

func TestForecastSnapshotUsesActualFetchTimestamp(t *testing.T) {
	e := newEngine()
	now := time.Now().UTC()
	e.prices["openai/test"] = ModelPrice{Input: 1}
	e.events = []Event{forecastEvent(now.Add(-time.Minute), 5000, 5000)}
	for _, fetched := range []string{"", "invalid", now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(-5 * time.Hour).Format(time.RFC3339Nano), now.Add(-2 * time.Minute).Format(time.RFC3339Nano)} {
		e.quotas["a"] = AccountQuota{AuthID: "a", FetchedAt: fetched, Five: &QuotaWindow{Used: 25, ResetAt: now.Add(time.Hour).Format(time.RFC3339Nano)}}
		win := e.snapshot().Quotas[0].Five
		if win.ObservedTokens != 5000 {
			t.Fatalf("fetch=%q actual %+v", fetched, win)
		}
		assertNoForecast(t, win)
	}
}

func TestForecastExpiredCycleAndResetBoundary(t *testing.T) {
	e, now, q := forecastFixture()
	q.ResetAt = now.Format(time.RFC3339Nano)
	e.events = []Event{forecastEvent(now.Add(-6*time.Hour), 50000, 50000), forecastEvent(now.Add(-time.Minute), 5000, 5000), forecastEvent(now, 9000000, 9000000)}
	for _, offset := range []time.Duration{0, time.Minute, 5 * time.Minute, 6 * time.Minute} {
		win := e.windowSummaryObserved("a", now.Add(offset), now.Add(-30*time.Second), q, 5*time.Hour)
		assertNoForecast(t, win)
		if offset <= 5*time.Minute && win.ObservedTokens != 5000 {
			t.Fatalf("reset boundary counted next cycle: %+v", win)
		}
	}
}

func TestForecastMissingPricePreservesKnownTokens(t *testing.T) {
	for _, basis := range []string{"quota", "previous"} {
		for _, missingPeriod := range []string{"current", "previous"} {
			e, now, q := forecastFixture()
			e.events = []Event{forecastEvent(now.Add(-6*time.Hour), 50000, 50000), forecastEvent(now, 5000, 5000)}
			if missingPeriod == "current" {
				e.events[1].Model = "unknown"
			} else {
				e.events[0].Model = "unknown"
			}
			if basis == "previous" {
				q.Used = 0
			}
			win := e.windowSummary("a", now, q, 5*time.Hour)
			if win.EstimatedBasis != basis || win.EstimatedTotalTokens == nil {
				t.Fatalf("known tokens hidden: %+v", win)
			}
			if basis == "previous" || missingPeriod == "current" {
				if win.EstimatedTotalCost != nil || !win.MissingPrice {
					t.Fatalf("unknown price invented money: %+v", win)
				}
			}
			if missingPeriod == "current" && win.ObservedCost != nil {
				t.Fatal("unknown current price invented actual money")
			}
			if missingPeriod == "previous" && (win.ObservedCost == nil || *win.ObservedCost != .005) {
				t.Fatal("previous missing price hid current known cost")
			}
		}
	}
}

func TestPreviousForecastCostToleranceMatchesManagerPlus(t *testing.T) {
	e, now, q := forecastFixture()
	q.Used = 0
	e.prices["openai/test"] = ModelPrice{Input: 1e-6}
	e.events = []Event{forecastEvent(now.Add(-6*time.Hour), 100, 999), forecastEvent(now, 100, 1000)}
	win := e.windowSummary("a", now, q, 5*time.Hour)
	if win.EstimatedBasis != "previous" || win.EstimatedTotalCost == nil {
		t.Fatal("floating-point cost consistency epsilon must match Manager Plus")
	}
}

func TestPairedForecastSurvivesNewRequestsAndRecomputesOnRefresh(t *testing.T) {
	e, now, q := forecastFixture()
	fetched := now.Add(-2 * time.Minute)
	e.events = []Event{forecastEvent(fetched, 100000, 100000)}
	first := e.windowSummaryObserved("a", now, fetched, q, 5*time.Hour)
	e.events = append(e.events, forecastEvent(now, 10000, 10000))
	second := e.windowSummaryObserved("a", now, fetched, q, 5*time.Hour)
	if first.EstimatedTotalTokens == nil || second.EstimatedTotalTokens == nil || *first.EstimatedTotalTokens != 400000 || *second.EstimatedTotalTokens != 400000 || second.ObservedTokens != 110000 || second.EstimatedTotalCost == nil || *second.EstimatedTotalCost != .4 || second.EstimatedAt != fetched.Format(time.RFC3339Nano) || second.EstimateObservedTokens == nil || *second.EstimateObservedTokens != 100000 {
		t.Fatalf("new requests erased or inflated the paired estimate %+v", second)
	}
	q.Used = 50
	third := e.windowSummaryObserved("a", now, now, q, 5*time.Hour)
	if third.EstimatedTotalTokens == nil || *third.EstimatedTotalTokens != 220000 || third.EstimatedTotalCost == nil || *third.EstimatedTotalCost != .22 {
		t.Fatalf("refresh did not re-pair %+v", third)
	}
	e.events = append(e.events, Event{At: now, AuthID: "a", Provider: "codex", Model: "unknown", Total: 1000})
	missing := e.windowSummaryObserved("a", now, fetched, q, 5*time.Hour)
	if !missing.MissingPrice || missing.ObservedCost != nil || missing.EstimatedTotalCost == nil || *missing.EstimatedTotalCost != .2 {
		t.Fatalf("later missing price erased known paired dollars %+v", missing)
	}
}

func TestPreviousForecastSurvivesRequestsAfterObservation(t *testing.T) {
	e, now, q := forecastFixture()
	q.Used = 0
	e.events = []Event{forecastEvent(now.Add(-6*time.Hour), 50000, 50000), forecastEvent(now, 5000, 5000)}
	win := e.windowSummaryObserved("a", now, now.Add(-time.Minute), q, 5*time.Hour)
	if win.EstimatedBasis != "previous" || win.EstimatedTotalTokens == nil || *win.EstimatedTotalTokens != 50000 {
		t.Fatalf("new current request erased previous-period basis %+v", win)
	}
}
