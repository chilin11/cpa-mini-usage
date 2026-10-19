package main

import "math"

// usageQuality validates only facts available in the legacy CPA plugin callback.
// TotalTokens stays authoritative for display; it is never charged a second time.
func usageQuality(ev Event) string {
	if ev.Input < 0 || ev.Output < 0 || ev.CacheRead < 0 || ev.CacheWrite < 0 || ev.Total < 0 {
		return "inconsistent"
	}
	input := ev.Input
	if normalizeProvider(ev.Provider) == "claude" {
		for _, n := range []int64{ev.CacheRead, ev.CacheWrite} {
			if n > math.MaxInt64-input {
				return "inconsistent"
			}
			input += n
		}
	} else if ev.CacheRead > input || ev.CacheWrite > input-ev.CacheRead {
		return "inconsistent"
	}
	if ev.Output > math.MaxInt64-input {
		return "inconsistent"
	}
	if input+ev.Output == 0 && ev.Total > 0 {
		return "unclassified"
	}
	return "complete"
}
