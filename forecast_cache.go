package main

import (
	"reflect"
	"time"
)

// Only one active-cycle estimate per account/window; never store credentials.
type SavedForecast struct {
	ResetAt  time.Time      `json:"reset_at"`
	Seconds  int64          `json:"seconds"`
	Estimate WindowEstimate `json:"estimate"`
}

func forecastValue(v *float64) bool { return v != nil && finite(*v) && *v >= 0 }

// Caller holds e.mu. Cache is separate from live usage and official percentages.
func (e *Engine) resolveForecastLocked(q AccountQuota, period string, now time.Time) (*WindowEstimate, bool) {
	win, duration := q.Five, 5*time.Hour
	if period == "7d" {
		win, duration = q.Week, 7*24*time.Hour
	}
	observation, _ := time.Parse(time.RFC3339Nano, q.FetchedAt)
	out := e.windowSummaryObserved(q.AuthID, now, observation, win, duration)
	if e.forecasts == nil {
		e.forecasts = make(map[string]SavedForecast)
	}
	key := q.Provider + "/" + e.canonicalAuth(q.AuthID) + "/" + period
	saved, exists := e.forecasts[key]
	if win == nil {
		return out, false
	}
	reset, err := time.Parse(time.RFC3339Nano, win.ResetAt)
	seconds := int64(duration / time.Second)
	if win.Seconds > 0 {
		seconds = win.Seconds
	}
	active := err == nil && now.Before(reset) && seconds > 0 && seconds <= int64((35*24*time.Hour)/time.Second) && !reset.Add(-time.Duration(seconds)*time.Second).After(now)
	changed := false
	if exists && (!active || !saved.ResetAt.Equal(reset) || saved.Seconds != seconds) {
		delete(e.forecasts, key)
		exists = false
		changed = true
	}
	if !active || out == nil {
		return out, changed
	}
	if out.EstimatedBasis == "quota" {
		out.EstimateUsedPercent = floatPtr(win.Used)
	}
	validTokens, validCost := forecastValue(out.EstimatedTotalTokens), forecastValue(out.EstimatedTotalCost)
	if (validTokens || validCost) && (out.EstimatedBasis == "quota" || out.EstimatedBasis == "previous") && !(exists && (out.EstimatedBasis == "previous" || (!validCost && forecastValue(saved.Estimate.EstimatedTotalCost)))) {
		fresh := WindowEstimate{EstimatedTotalTokens: out.EstimatedTotalTokens, EstimatedTotalCost: out.EstimatedTotalCost, EstimatedBasis: out.EstimatedBasis, EstimatedAt: out.EstimatedAt, EstimateObservedTokens: out.EstimateObservedTokens, EstimateObservedCost: out.EstimateObservedCost, EstimateUsedPercent: out.EstimateUsedPercent}
		candidate := SavedForecast{ResetAt: reset, Seconds: seconds, Estimate: fresh}
		if !exists || !reflect.DeepEqual(saved, candidate) {
			e.forecasts[key] = candidate
			changed = true
		}
	} else if exists {
		prior := saved.Estimate
		if forecastValue(prior.EstimatedTotalTokens) {
			out.EstimatedTotalTokens = floatPtr(*prior.EstimatedTotalTokens)
		}
		if forecastValue(prior.EstimatedTotalCost) {
			out.EstimatedTotalCost = floatPtr(*prior.EstimatedTotalCost)
		}
		out.EstimatedBasis = prior.EstimatedBasis
		out.EstimatedAt = prior.EstimatedAt
		out.EstimateObservedTokens = prior.EstimateObservedTokens
		out.EstimateObservedCost = prior.EstimateObservedCost
		out.EstimateUsedPercent = prior.EstimateUsedPercent
		out.EstimateRetained = true
	}
	// A retained total is an estimate, but cannot be less than known usage.
	if forecastValue(out.EstimatedTotalTokens) && *out.EstimatedTotalTokens < float64(out.ObservedTokens) {
		out.EstimatedTotalTokens = floatPtr(float64(out.ObservedTokens))
	}
	if forecastValue(out.EstimatedTotalCost) && forecastValue(out.ObservedCost) && *out.EstimatedTotalCost < *out.ObservedCost {
		out.EstimatedTotalCost = floatPtr(*out.ObservedCost)
	}
	return out, changed
}

func (e *Engine) pruneForecastsLocked(now time.Time) bool {
	changed := false
	for key, saved := range e.forecasts {
		if !now.Before(saved.ResetAt) {
			delete(e.forecasts, key)
			changed = true
		}
	}
	return changed
}

func (e *Engine) rememberQuotaForecastsLocked(q AccountQuota, now time.Time) {
	if q.AuthID == "" {
		return
	}
	e.resolveForecastLocked(q, "5h", now)
	e.resolveForecastLocked(q, "7d", now)
}
