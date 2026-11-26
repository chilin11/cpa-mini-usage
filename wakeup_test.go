package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWakeupConfigValidationAndNormalization(t *testing.T) {
	valid, err := validateWakeupConfig(WakeupConfig{
		Enabled:           true,
		Mode:              "DAILY",
		Times:             []string{"12:05", "07:00", "07:00"},
		TimezoneOffsetMin: 480,
		Providers:         map[string]bool{"claude": true, "codex": false, "antigravity": true},
		ExcludedAuthIDs:   []string{"b", "a", "a"},
		Models:            map[string]string{"Claude": "claude-haiku-4-5-20251001", "codex": "  "},
		Message:           "  ping  ",
	})
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if valid.Mode != "daily" || len(valid.Times) != 2 || valid.Times[0] != "07:00" || valid.Times[1] != "12:05" {
		t.Fatalf("normalized times/mode unexpected: %+v", valid)
	}
	if len(valid.ExcludedAuthIDs) != 2 || valid.ExcludedAuthIDs[0] != "a" || valid.ExcludedAuthIDs[1] != "b" {
		t.Fatalf("excluded ids not sorted/deduped: %+v", valid.ExcludedAuthIDs)
	}
	if valid.Models["claude"] != "claude-haiku-4-5-20251001" || len(valid.Models) != 1 {
		t.Fatalf("models normalization unexpected: %+v", valid.Models)
	}
	if valid.Message != "ping" {
		t.Fatalf("message = %q, want ping", valid.Message)
	}

	invalidCases := []WakeupConfig{
		{Mode: "hourly", Times: []string{"07:00"}},
		{Mode: "daily", Times: nil},
		{Mode: "both", Times: []string{}},
		{Mode: "daily", Times: []string{"24:00"}},
		{Mode: "daily", Times: []string{"7:00"}},
		{Mode: "daily", Times: []string{"07:00"}, TimezoneOffsetMin: 900},
		{Mode: "daily", Times: []string{"07:00"}, Providers: map[string]bool{"unknown": true}},
		{Mode: "daily", Times: []string{"07:00"}, Models: map[string]string{"invalid": "m"}},
		{Mode: "daily", Times: []string{"07:00"}, Models: map[string]string{"claude": "bad model with space"}},
		{Mode: "daily", Times: []string{"07:00"}, Models: map[string]string{wakeupGeminiKey: "claude-sonnet-4-6"}},
		{Mode: "daily", Times: []string{"07:00"}, Models: map[string]string{wakeupGeminiKey: "gemini-3-flash/claude-sonnet-4-6"}},
		{Mode: "daily", Times: []string{"07:00"}, Message: "bad\x00ctrl"},
		{Mode: "daily", Times: []string{"07:00"}, Message: strings.Repeat("a", wakeupMaxMessageRunes+1)},
		{Enabled: true, Mode: "daily", Times: []string{"07:00"}, Providers: map[string]bool{"claude": false, "codex": false, "antigravity": false}},
	}
	for i, tc := range invalidCases {
		if _, err := validateWakeupConfig(tc); err == nil {
			t.Fatalf("case %d unexpectedly succeeded: %+v", i, tc)
		}
	}
}

func TestWakeupPersistenceSurvivesRestartAndCorruptTriggersReadOnly(t *testing.T) {
	dir := t.TempDir()
	e := newEngine()
	e.dir = dir
	e.load()
	e.quotas["claude-1"] = AccountQuota{AuthID: "claude-1", Provider: "claude", Name: "alice@example.com"}

	state, status, failure := e.setWakeupConfig(WakeupConfig{
		Enabled:           true,
		Mode:              "both",
		Times:             []string{"07:00", "12:05"},
		TimezoneOffsetMin: 480,
		Providers:         map[string]bool{"claude": true, "codex": true, "antigravity": false},
		Models:            map[string]string{"claude": "claude-haiku-4-5-20251001"},
		Message:           "hello",
	})
	if status != 200 || failure != "" || !state.Config.Enabled {
		t.Fatalf("setWakeupConfig failed: status=%d failure=%q state=%+v", status, failure, state)
	}
	state, status, failure = e.setWakeupExcluded("claude-1", true)
	if status != 200 || failure != "" || len(state.Config.ExcludedAuthIDs) != 1 || state.Config.ExcludedAuthIDs[0] != "claude-1" {
		t.Fatalf("setWakeupExcluded failed: status=%d failure=%q state=%+v", status, failure, state)
	}

	raw, err := os.ReadFile(filepath.Join(dir, wakeupConfigFileName))
	if err != nil {
		t.Fatalf("wakeup.json missing: %v", err)
	}
	info, _ := os.Stat(filepath.Join(dir, wakeupConfigFileName))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("wakeup.json permissions = %v, want 0600", info.Mode().Perm())
	}
	if strings.Contains(string(raw), "access_token") {
		t.Fatalf("wakeup.json contains secret marker")
	}

	var legacy persistedWakeup
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Config.Models["antigravity:claude-gpt"] = "claude-sonnet-4-6"
	legacy.Config.Models["antigravity"] = "gemini-3-flash"
	legacy.Config.Models[wakeupGeminiKey] = "gpt-oss-120b-medium"
	raw, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wakeupConfigFileName), raw, 0600); err != nil {
		t.Fatal(err)
	}

	restored := newEngine()
	restored.dir = dir
	restored.load()
	got := restored.snapshot().Wakeup.Config
	if restored.storageReadFailed || len(got.Models) != 2 || got.Models[wakeupGeminiKey] != "gemini-3-flash" || got.Models["claude"] != "claude-haiku-4-5-20251001" {
		t.Fatalf("legacy Google overrides were not migrated safely: %+v", got.Models)
	}
	if !got.Enabled || got.Mode != "both" || got.TimezoneOffsetMin != 480 || got.Message != "hello" || len(got.ExcludedAuthIDs) != 1 || got.ExcludedAuthIDs[0] != "claude-1" {
		t.Fatalf("restored wakeup config mismatch: %+v", got)
	}

	// Corrupt file must activate read-only storage protection.
	corruptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(corruptDir, wakeupConfigFileName), []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := newEngine()
	bad.dir = corruptDir
	bad.load()
	if !bad.persistence.ReadOnly || !bad.storageReadFailed {
		t.Fatalf("corrupt wakeup.json did not trigger read-only protection")
	}
	if _, status, failure := bad.setWakeupConfig(defaultWakeupConfig()); status != 500 || failure != "storage_write_failed" {
		t.Fatalf("expected read-only write refusal, got status=%d failure=%q", status, failure)
	}
}

func TestWakeupDailyScheduleRespectsTimezoneGraceAndNoDuplicateFire(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.load()

	// UTC+8 07:02 == UTC 23:02 previous day.
	loc := wakeupZone(480)
	at0702 := time.Date(2026, 10, 9, 7, 2, 0, 0, loc).UTC()
	e.wakeupConfig = WakeupConfig{
		Enabled:           true,
		Mode:              "daily",
		Times:             []string{"07:00", "12:00"},
		TimezoneOffsetMin: 480,
		Providers:         map[string]bool{"claude": true, "codex": true, "antigravity": true},
		Message:           "hi",
	}

	// Next slot before 07:00 should point to 07:00 local (23:00Z previous day).
	at0655 := time.Date(2026, 10, 9, 6, 55, 0, 0, loc).UTC()
	next := nextWakeupSlot(e.wakeupConfig, e.wakeupFiredSlots, at0655)
	wantNext := time.Date(2026, 10, 9, 7, 0, 0, 0, loc).UTC()
	if !next.Equal(wantNext) {
		t.Fatalf("nextWakeupSlot = %v, want %v", next, wantNext)
	}

	var mu sync.Mutex
	calls := 0
	doneCh := make(chan struct{}, 2)
	rpc := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{
				map[string]any{"id": "c1", "auth_index": "idx1", "provider": "claude", "email": "a@example.com"},
			}}, nil
		}
		return nil, nil
	}
	exec := func(entry AuthEntry, model string, msg string, _ func(string, any) (any, error)) (string, string) {
		mu.Lock()
		calls++
		mu.Unlock()
		doneCh <- struct{}{}
		return model, ""
	}

	// Trigger once at 07:02 (within 10m grace of 07:00).
	e.checkWakeupScheduleWith(at0702, rpc, exec, nil)
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled wakeup did not fire at 07:02")
	}
	waitWakeupIdle(t, e)

	// Re-checking at 07:04 must NOT fire the same slot twice.
	e.checkWakeupScheduleWith(at0702.Add(2*time.Minute), rpc, exec, nil)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("expected exactly 1 call for 07:00 slot, got %d", gotCalls)
	}

	// Next slot after 07:00 fired should advance to 12:00.
	nextAfter := nextWakeupSlot(e.wakeupConfig, e.wakeupFiredSlots, at0702.Add(3*time.Minute))
	want1200 := time.Date(2026, 10, 9, 12, 0, 0, 0, loc).UTC()
	if !nextAfter.Equal(want1200) {
		t.Fatalf("next slot after 07:00 = %v, want %v", nextAfter, want1200)
	}
}

func TestWakeupWindowActiveAndCooldownPreventsLoop(t *testing.T) {
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)

	// 1. Active window (used > 0 and reset in future).
	activeWin := &QuotaWindow{Used: 12, ResetAt: now.Add(3 * time.Hour).Format(time.RFC3339)}
	if !isFiveHourWindowActive(activeWin, wakeupSentRecord{}, now) {
		t.Fatal("expected used>0 future window to be active")
	}

	// 2. Unused window (used == 0) with no recent send -> inactive (needs wakeup).
	idleWin := &QuotaWindow{Used: 0, ResetAt: now.Add(5 * time.Hour).Format(time.RFC3339)}
	if isFiveHourWindowActive(idleWin, wakeupSentRecord{}, now) {
		t.Fatal("expected idle window without recent send to be inactive")
	}

	// 3. Even if provider rounds tiny wakeup usage to 0%, recent send within 4h50m keeps it active.
	recentSent := wakeupSentRecord{At: now.Add(-30 * time.Minute), ResetAt: now.Add(4*time.Hour + 30*time.Minute)}
	if !isFiveHourWindowActive(idleWin, recentSent, now) {
		t.Fatal("expected cooldown after recent send to prevent duplicate wakeup")
	}

	// 4. Once observed reset_at passes (and >=5m after send), window becomes inactive again!
	expiredSent := wakeupSentRecord{At: now.Add(-5 * time.Hour), ResetAt: now.Add(-1 * time.Minute)}
	if isFiveHourWindowActive(idleWin, expiredSent, now) {
		t.Fatal("expected window after reset_at to be eligible for reset wakeup again")
	}

	// 5. Reset mode automatic trigger runs for idle account and skips active account.
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	e.wakeupConfig = WakeupConfig{
		Enabled:           true,
		Mode:              "reset",
		Times:             []string{},
		TimezoneOffsetMin: 0,
		Providers:         map[string]bool{"claude": true, "codex": true, "antigravity": true},
		Message:           "hi",
	}
	e.quotas["idle-acc"] = AccountQuota{
		AuthID: "idle-acc", Provider: "codex", Name: "idle@example.com",
		FetchedAt: now.Format(time.RFC3339),
		Five:      &QuotaWindow{Used: 0, ResetAt: ""},
	}
	e.quotas["busy-acc"] = AccountQuota{
		AuthID: "busy-acc", Provider: "codex", Name: "busy@example.com",
		FetchedAt: now.Format(time.RFC3339),
		Five:      &QuotaWindow{Used: 25, ResetAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
	}
	e.quotas["errored-acc"] = AccountQuota{
		AuthID: "errored-acc", Provider: "codex", Name: "err@example.com",
		FetchedAt: now.Format(time.RFC3339),
		Error:     "Quota endpoint unavailable",
		Five:      &QuotaWindow{Used: 0},
	}
	e.quotas["stale-acc"] = AccountQuota{
		AuthID: "stale-acc", Provider: "codex", Name: "stale@example.com",
		FetchedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
		Five:      &QuotaWindow{Used: 0},
	}
	var woken []string
	var mu sync.Mutex
	rpc := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{
				map[string]any{"id": "idle-acc", "auth_index": "i1", "provider": "codex", "email": "idle@example.com"},
				map[string]any{"id": "busy-acc", "auth_index": "i2", "provider": "codex", "email": "busy@example.com"},
				map[string]any{"id": "errored-acc", "auth_index": "i3", "provider": "codex", "email": "err@example.com"},
				map[string]any{"id": "stale-acc", "auth_index": "i4", "provider": "codex", "email": "stale@example.com"},
			}}, nil
		}
		return nil, nil
	}
	exec := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		mu.Lock()
		woken = append(woken, entry.ID)
		mu.Unlock()
		return model, ""
	}
	e.checkWakeupScheduleWith(now, rpc, exec, nil)
	waitWakeupIdle(t, e)
	if len(woken) != 1 || woken[0] != "idle-acc" {
		t.Fatalf("expected only idle-acc to wake in reset mode, got %v", woken)
	}
	// Immediate second check must not re-wake idle-acc (cooldown active).
	e.checkWakeupScheduleWith(now.Add(2*time.Minute), rpc, exec, nil)
	waitWakeupIdle(t, e)
	if len(woken) != 1 {
		t.Fatalf("cooldown failed; woke again: %v", woken)
	}
}

func TestWakeupModelExecutionPinningFallbackAndAntigravityGroups(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	now := time.Now().UTC()
	e.quotas["claude-1"] = AccountQuota{AuthID: "claude-1", Provider: "claude", Name: "c@example.com"}
	e.quotas["ag-1"] = AccountQuota{
		AuthID: "ag-1", Provider: "antigravity", Name: "g@example.com",
		Groups: []QuotaGroup{
			{Key: "gemini", Label: "Gemini Models", Five: &QuotaWindow{Used: 0}},
			{Key: "claude-gpt", Label: "Claude / GPT Models", Five: &QuotaWindow{Used: 0}},
		},
	}

	var requests []map[string]any
	var mu sync.Mutex
	catalogReads := 0
	windowReset := now.Add(5 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	rpc := func(method string, arg any) (any, error) {
		switch method {
		case "host.auth.list":
			return map[string]any{"files": []any{
				map[string]any{"id": "claude-1", "auth_index": "i1", "provider": "claude", "email": "c@example.com"},
				map[string]any{"id": "ag-1", "auth_index": "i2", "provider": "antigravity", "email": "g@example.com"},
				map[string]any{"id": "dis-1", "auth_index": "i3", "provider": "codex", "email": "d@example.com", "disabled": true},
			}}, nil
		case "host.auth.models":
			catalogReads++
			if arg.(map[string]string)["auth_index"] != "i2" {
				t.Errorf("model catalog not pinned: %v", arg)
			}
			return map[string]any{"credentials": []any{map[string]any{
				"id": "ag-1", "auth_index": "i2", "provider": "antigravity",
				"models": []any{"gemini-nonchat-image", "gemini-4-hydrogen-xhigh", "gemini-3.5-flash-lite", "claude-sonnet-4-6", "gpt-oss-120b-medium"},
			}}}, nil
		case "host.model.execute":
			req := arg.(map[string]any)
			mu.Lock()
			requests = append(requests, req)
			mu.Unlock()
			// Reject first Claude default candidate to verify fallback to next candidate.
			if req["auth_id"] == "claude-1" && req["model"] == "claude-haiku-4-5-20251001" {
				return nil, &hostRPCError{Method: method, StatusCode: 404, Detail: "auth_not_found for model"}
			}
			return map[string]any{"status_code": float64(200), "body": "e30="}, nil
		}
		return nil, nil
	}

	refreshed := make(map[string]bool)
	var refMu sync.Mutex
	refresh := func(entry AuthEntry) bool {
		refMu.Lock()
		refreshed[entry.ID] = true
		refMu.Unlock()
		if entry.ID == "ag-1" {
			e.mu.Lock()
			q := e.quotas[entry.ID]
			q.FetchedAt = time.Now().UTC().Format(time.RFC3339Nano)
			q.Groups[0].Five = &QuotaWindow{Used: 0.1, ResetAt: windowReset}
			e.quotas[entry.ID] = q
			e.mu.Unlock()
		}
		return true
	}

	// Seed a stale result from an earlier run to confirm full batch clears stale entries.
	e.wakeupResults["removed-old"] = WakeupAccountResult{AuthID: "removed-old", Provider: "codex", Status: "failed", Reason: wakeupErrRequestFailed, At: now.Add(-time.Hour).Format(time.RFC3339Nano)}

	status, _, failure := e.kickWakeupRunWith("", false, now, rpc, executeWakeupModel, refresh)
	if status != 202 || failure != "" {
		t.Fatalf("kickWakeupRunWith status=%d failure=%q", status, failure)
	}
	waitWakeupIdle(t, e)

	// Refresh must already be finished before waitWakeupIdle returns (no extra sleep).
	refMu.Lock()
	bothRefreshed := refreshed["claude-1"] && refreshed["ag-1"]
	refMu.Unlock()
	if !bothRefreshed {
		t.Fatalf("expected post-wakeup quota refresh to complete before wakeup signals finished: %+v", refreshed)
	}

	snap := e.snapshot().Wakeup
	if _, stale := snap.Results["removed-old"]; stale {
		t.Fatalf("full wakeup batch should clear stale results from prior runs: %+v", snap.Results)
	}
	if snap.Results["claude-1"].Status != "sent" || snap.Results["claude-1"].Model != "claude-haiku-4-5" {
		t.Fatalf("claude fallback unexpected: %+v", snap.Results["claude-1"])
	}

	// Persisted results survive engine restart while sanitizing untrusted status/reason fields.
	restored := newEngine()
	restored.dir = e.dir
	restored.load()
	if got := restored.snapshot().Wakeup.Results["claude-1"]; got.Status != "sent" || got.Model != "claude-haiku-4-5" {
		t.Fatalf("restored wakeup result mismatch: %+v", got)
	}
	if snap.Results["dis-1"].Status != "skipped" || snap.Results["dis-1"].Reason != wakeupReasonDisabled {
		t.Fatalf("disabled account should be skipped: %+v", snap.Results["dis-1"])
	}
	agRes := snap.Results["ag-1"]
	if agRes.Status != "sent" || len(agRes.Groups) != 1 {
		t.Fatalf("antigravity must wake only the Gemini group: %+v", agRes)
	}
	if catalogReads != 1 || agRes.Groups[0].Group != "gemini" || agRes.Groups[0].Status != "sent" || agRes.Groups[0].Model != "gemini-3.5-flash-lite" || agRes.Groups[0].WindowResetAt != windowReset {
		t.Fatalf("antigravity Gemini wakeup unexpected (catalog reads=%d): %+v", catalogReads, agRes.Groups)
	}
	if e.accountNeedsResetWakeupLocked(e.quotas["ag-1"], time.Now()) {
		t.Fatal("an idle Antigravity Claude/GPT window must not trigger auto-wakeup")
	}
	entry := AuthEntry{ID: "ag-1", Provider: "antigravity"}
	if model, reason := executeWakeupModel(entry, "claude-sonnet-4-6", "hi", func(string, any) (any, error) {
		t.Fatal("Antigravity Claude must not reach any RPC")
		return nil, nil
	}); model != "" || reason != wakeupErrModelUnavail {
		t.Fatalf("non-Gemini guard returned %q/%q", model, reason)
	}
	if got := restored.snapshot().Wakeup.Results["ag-1"].Groups; len(got) == 0 || got[0].WindowResetAt != windowReset {
		t.Fatalf("confirmed reset time was not restored: %+v", got)
	}

	// Verify pinned wire contract sent to host.model.execute.
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("expected two standalone Claude attempts and one Antigravity Gemini attempt, got %d", len(requests))
	}
	for _, r := range requests {
		if r["entry_protocol"] != "openai" || r["exit_protocol"] != "openai" || r["stream"] != false {
			t.Fatalf("unexpected protocol fields: %+v", r)
		}
		if r["auth_id"] == "" || r["forced_provider"] == "" {
			t.Fatalf("missing pinned auth_id/forced_provider: %+v", r)
		}
		var body map[string]any
		if err := json.Unmarshal(r["body"].([]byte), &body); err != nil {
			t.Fatalf("invalid body json: %v", err)
		}
		if r["auth_id"] == "ag-1" {
			if body["max_tokens"] != nil || body["reasoning_effort"] != "minimal" || r["forced_provider"] != "antigravity" {
				t.Fatalf("unexpected Antigravity request: %+v %+v", r, body)
			}
		} else if body["max_tokens"] != float64(wakeupDefaultMaxTokens) {
			t.Fatalf("unexpected max_tokens: %+v", body)
		}
	}
}

func TestWakeupGemini4UsesLowEffortAndIsNotLightweight(t *testing.T) {
	const model = "gemini-4-argon-high"
	if isLightWakeupModel(model) || !isLightWakeupModel("gemini-3.5-flash-lite") {
		t.Fatal("Gemini family name must not make every Gemini model lightweight")
	}
	entry := AuthEntry{ID: "ag-on", Provider: "antigravity", RawProvider: "antigravity"}
	_, reason := executeWakeupModel(entry, model, "hi", func(method string, arg any) (any, error) {
		request := arg.(map[string]any)
		var body map[string]any
		if err := json.Unmarshal(request["body"].([]byte), &body); err != nil {
			t.Fatal(err)
		}
		if method != "host.model.execute" || request["auth_id"] != "ag-on" || request["model"] != model || body["reasoning_effort"] != "low" || body["max_tokens"] != nil {
			t.Fatalf("unexpected Gemini 4 wakeup: %s %+v %+v", method, request, body)
		}
		return map[string]any{"status_code": float64(200)}, nil
	})
	if reason != "" {
		t.Fatalf("Gemini 4 wakeup failed: %s", reason)
	}
}

func TestWakeupStopsOnRateLimitAuthAndExplicitModel(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	e.quotas["c1"] = AccountQuota{AuthID: "c1", Provider: "claude", Name: "c1"}
	e.wakeupConfig.Models = map[string]string{"claude": "claude-custom-model"}

	attempts := 0
	exec := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		attempts++
		return model, wakeupErrModelUnavail
	}
	res := e.wakeupEntry(AuthEntry{ID: "c1", Provider: "claude"}, e.wakeupConfig, "test", "", time.Now().UTC(), wakeupSentRecord{}, nil, exec)
	if attempts != 1 || res.Status != "failed" || res.Reason != wakeupErrModelUnavail {
		t.Fatalf("explicit user model must not silently fall back: attempts=%d res=%+v", attempts, res)
	}

	// Rate limit (429) must stop immediately and report status=exhausted (not generic failed).
	e.wakeupConfig.Models = map[string]string{}
	attempts = 0
	exec429 := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		attempts++
		return model, wakeupErrRateLimited
	}
	res = e.wakeupEntry(AuthEntry{ID: "c1", Provider: "claude"}, e.wakeupConfig, "test", "", time.Now().UTC(), wakeupSentRecord{}, nil, exec429)
	if attempts != 1 || res.Status != "exhausted" || res.Reason != wakeupErrRateLimited {
		t.Fatalf("429 must stop immediately as exhausted: attempts=%d res=%+v", attempts, res)
	}

	// Verify error classification never leaks raw secret detail.
	if got := classifyWakeupError(&hostRPCError{Method: "host.model.execute", Detail: "unsupported host callback host.model.execute"}); got != wakeupErrHostUnsupp {
		t.Fatalf("got %q, want %q", got, wakeupErrHostUnsupp)
	}
	if got := classifyWakeupError(&hostRPCError{Method: "host.model.execute", StatusCode: 401, Detail: "Bearer sk-ant-secret"}); got != wakeupErrAuthFailed {
		t.Fatalf("got %q, want %q", got, wakeupErrAuthFailed)
	}
}

func TestWakeupManagementRoutes(t *testing.T) {
	old := tracker
	tracker = newEngine()
	tracker.dir = t.TempDir()
	tracker.quotas["c1"] = AccountQuota{AuthID: "c1", Provider: "claude", Name: "alice@example.com"}
	tracker.quotas["dis"] = AccountQuota{AuthID: "dis", Provider: "codex", Name: "off@example.com", Disabled: true}
	defer func() { tracker = old }()

	// Verify routes are registered.
	var reg struct {
		OK     bool `json:"ok"`
		Result struct {
			Routes []struct {
				Method string
				Path   string
			}
		}
	}
	if err := json.Unmarshal(dispatch("management.register", nil), &reg); err != nil || !reg.OK {
		t.Fatalf("management.register failed: %v", err)
	}
	wantRoutes := map[string]bool{
		"POST /plugins/" + pluginID + "/wakeup/config":  false,
		"POST /plugins/" + pluginID + "/wakeup/exclude": false,
		"POST /plugins/" + pluginID + "/wakeup/run":     false,
	}
	for _, r := range reg.Result.Routes {
		key := r.Method + " " + r.Path
		if _, exists := wantRoutes[key]; exists {
			wantRoutes[key] = true
		}
	}
	for k, seen := range wantRoutes {
		if !seen {
			t.Fatalf("missing registered route %s", k)
		}
	}

	call := func(path, payload string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/v0/management/plugins/" + pluginID + path, "Body": []byte(payload)})
		var resp struct {
			OK     bool `json:"ok"`
			Result struct {
				StatusCode int
				Body       []byte
			}
		}
		if err := json.Unmarshal(dispatch("management.handle", body), &resp); err != nil || !resp.OK {
			t.Fatalf("dispatch %s failed: %v", path, err)
		}
		var decoded map[string]any
		_ = json.Unmarshal(resp.Result.Body, &decoded)
		return resp.Result.StatusCode, decoded
	}

	if code, _ := call("/wakeup/config", `{"mode":"daily","times":[]}`); code != 400 {
		t.Fatalf("empty daily times status = %d, want 400", code)
	}
	if code, body := call("/wakeup/config", `{"enabled":true,"mode":"daily","times":["07:30"],"timezone_offset_min":480,"skip_active_window":true,"providers":{"claude":true,"codex":true,"antigravity":true},"message":"hi"}`); code != 200 || body["wakeup"] == nil {
		t.Fatalf("valid config status=%d body=%+v", code, body)
	}
	if code, _ := call("/wakeup/exclude", `{"auth_id":"unknown","excluded":true}`); code != 404 {
		t.Fatalf("unknown exclude status=%d, want 404", code)
	}
	if code, _ := call("/wakeup/exclude", `{"auth_id":"c1","excluded":true}`); code != 200 {
		t.Fatalf("valid exclude status=%d, want 200", code)
	}
	if code, body := call("/wakeup/run", `{"auth_id":"dis","force":true}`); code != 409 || body["error"] != wakeupErrAccountDis {
		t.Fatalf("disabled account run status=%d body=%+v, want 409 %s", code, body, wakeupErrAccountDis)
	}
	if code, _ := call("/wakeup/run", `{"auth_id":"unknown","force":true}`); code != 404 {
		t.Fatalf("unknown account run status=%d, want 404", code)
	}
}

func waitWakeupIdle(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		busy := e.wakeupRunning || len(e.wakeupRunningAuths) > 0
		e.mu.RUnlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("wakeup run did not finish in time")
}

func TestWakeupCronParsingScheduleAndWeekdays(t *testing.T) {
	// 1. Validate 5-field cron parsing, including 6/5 and invalid dates like Feb 31.
	sched, err := parseCron("5 6/5 * * *")
	if err != nil {
		t.Fatalf("parseCron(5 6/5 * * *) failed: %v", err)
	}
	loc := wakeupZone(480) // UTC+8
	base := time.Date(2026, 11, 25, 5, 50, 0, 0, loc).UTC()
	runs := sched.nextRuns(base, 4, nil, 480)
	if len(runs) != 4 {
		t.Fatalf("got %d runs, want 4", len(runs))
	}
	wantHours := []int{6, 11, 16, 21}
	for i, run := range runs {
		lt := run.In(loc)
		if lt.Hour() != wantHours[i] || lt.Minute() != 5 {
			t.Fatalf("run[%d] = %v, want %02d:05", i, lt, wantHours[i])
		}
	}

	// Weekday-only cron skips Saturday/Sunday and jumps to Monday 06:00.
	weekdaySched, err := parseCron("0 6,11,16,21 * * 1-5")
	if err != nil {
		t.Fatalf("parseCron weekday failed: %v", err)
	}
	// 2026-11-28 is Saturday.
	satMorning := time.Date(2026, 11, 28, 5, 0, 0, 0, loc).UTC()
	nextWeekday := weekdaySched.nextRuns(satMorning, 2, nil, 480)
	if len(nextWeekday) != 2 {
		t.Fatalf("weekday runs = %v", nextWeekday)
	}
	if first := nextWeekday[0].In(loc); first.Weekday() != time.Monday || first.Day() != 30 || first.Hour() != 6 || first.Minute() != 0 {
		t.Fatalf("first weekday run = %v, want Mon Nov 30 06:00", first)
	}

	for _, bad := range []string{"", "* * *", "0 6 * * * *", "60 6 * * *", "0 24 * * *", "0 6/0 * * *", "0 0 31 2 *", "0 10-5 * * *"} {
		if _, err := parseCron(bad); err == nil {
			t.Fatalf("expected invalid cron %q to fail", bad)
		}
	}

	// 2. End-to-end Cron schedule trigger inside grace window with no duplicate firing and 5 upcoming runs.
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	e.wakeupConfig = WakeupConfig{
		Enabled:           true,
		Mode:              "daily",
		Cron:              "0 6,11,16,21 * * *",
		TimezoneOffsetMin: 480,
		Providers:         map[string]bool{"claude": true, "codex": true, "antigravity": true},
		Message:           "hi",
	}

	var mu sync.Mutex
	calls := 0
	rpc := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{
				map[string]any{"id": "c1", "auth_index": "i1", "provider": "claude", "email": "a@example.com"},
			}}, nil
		}
		return nil, nil
	}
	exec := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		mu.Lock()
		calls++
		mu.Unlock()
		return model, ""
	}

	at0602 := time.Date(2026, 11, 25, 6, 2, 0, 0, loc).UTC()
	e.checkWakeupScheduleWith(at0602, rpc, exec, nil)
	waitWakeupIdle(t, e)
	// Second check 2 minutes later must not re-fire 06:00 slot.
	e.checkWakeupScheduleWith(at0602.Add(2*time.Minute), rpc, exec, nil)
	waitWakeupIdle(t, e)
	if calls != 1 {
		t.Fatalf("expected 1 cron execution for 06:00 slot, got %d", calls)
	}

	e.mu.RLock()
	upcoming := nextWakeupRuns(e.wakeupConfig, e.wakeupFiredSlots, at0602.Add(3*time.Minute), 5)
	e.mu.RUnlock()
	if len(upcoming) != 5 || upcoming[0].In(loc).Hour() != 11 || upcoming[1].In(loc).Hour() != 16 || upcoming[2].In(loc).Hour() != 21 || upcoming[3].In(loc).Hour() != 6 {
		t.Fatalf("unexpected upcoming runs after 06:00: %v", upcoming)
	}
}

func TestWakeupSurvivesCPAUpgradeAndRestartWithoutUI(t *testing.T) {
	dir := t.TempDir()
	loc := wakeupZone(480)

	// 1. User configures and saves scheduled wakeup before a CPA upgrade.
	before := newEngine()
	before.dir = dir
	before.load()
	if before.snapshot().Wakeup.Persisted {
		t.Fatal("fresh install without wakeup.json should report persisted=false")
	}
	state, status, failure := before.setWakeupConfig(WakeupConfig{
		Enabled:           true,
		Mode:              "daily",
		Cron:              "0 6,11,16,21 * * *",
		TimezoneOffsetMin: 480,
		Providers:         map[string]bool{"claude": true, "codex": true, "antigravity": true},
		Models:            map[string]string{"claude": "claude-haiku-4-5-20251001"},
		Message:           "hi",
	})
	if status != 200 || failure != "" || !state.Persisted || state.ConfigPath != filepath.Join(dir, wakeupConfigFileName) {
		t.Fatalf("expected persisted wakeup state, got status=%d failure=%q state=%+v", status, failure, state)
	}
	before.shutdown()

	// 2. Simulate CPA upgrade/restart: brand new Engine pointing at the same persistent data dir,
	// with no browser/UI requests ever made.
	after := newEngine()
	after.dir = dir
	after.load()
	restored := after.snapshot().Wakeup
	if !restored.Persisted || !restored.Config.Enabled || restored.Config.Cron != "0 6,11,16,21 * * *" || restored.Config.Models["claude"] != "claude-haiku-4-5-20251001" {
		t.Fatalf("wakeup config did not survive CPA restart: %+v", restored)
	}

	// 3. Simulate transient host startup unavailability at 06:00: slot must roll back, not be lost.
	at0601 := time.Date(2026, 11, 26, 6, 1, 0, 0, loc).UTC()
	unavailableRPC := func(method string, _ any) (any, error) {
		return nil, &hostRPCError{Method: method, Detail: "host starting up"}
	}
	calls := 0
	exec := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		calls++
		return model, ""
	}
	after.checkWakeupScheduleWith(at0601, unavailableRPC, exec, nil)
	waitWakeupIdle(t, after)
	if calls != 0 {
		t.Fatalf("expected 0 calls while host unavailable, got %d", calls)
	}

	// 4. Once CPA finishes loading auth records 1 minute later (still inside grace window), the
	// rolled-back 06:00 slot automatically retries and executes.
	readyRPC := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{
				map[string]any{"id": "c1", "auth_index": "i1", "provider": "claude", "email": "a@example.com"},
			}}, nil
		}
		return nil, nil
	}
	after.checkWakeupScheduleWith(at0601.Add(time.Minute), readyRPC, exec, nil)
	waitWakeupIdle(t, after)
	if calls != 1 {
		t.Fatalf("expected rolled-back slot to fire once CPA became ready, got %d", calls)
	}
}

func TestWakeupExhaustedDetectionAndLightModelOrdering(t *testing.T) {
	now := time.Now().UTC()
	e := newEngine()
	e.dir = t.TempDir()
	e.load()

	// 1. Account with observed 100% five-hour usage reports exhausted even under force=true test,
	// without issuing a model call or misreporting model_unavailable.
	e.quotas["codex-full"] = AccountQuota{
		AuthID: "codex-full", Provider: "codex", Name: "full@example.com",
		Five: &QuotaWindow{Used: 100, ResetAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
	}
	called := false
	execShouldNotRun := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		called = true
		return model, ""
	}
	res := e.wakeupEntry(AuthEntry{ID: "codex-full", Provider: "codex"}, e.wakeupConfig, "test", "", now, wakeupSentRecord{}, nil, execShouldNotRun)
	if called || res.Status != "exhausted" || res.Reason != wakeupReasonExhausted {
		t.Fatalf("expected 100%% used account to report exhausted without calling model: called=%v res=%+v", called, res)
	}

	// 2. Account marked cooling/unavailable by CPA host.auth.list whose pinned execute returns auth_not_found
	// reports exhausted instead of model_unavailable.
	rawList := map[string]any{"files": []any{
		map[string]any{
			"id":               "codex-cool",
			"auth_index":       "i-cool",
			"provider":         "codex",
			"email":            "cool@example.com",
			"unavailable":      true,
			"next_retry_after": now.Add(30 * time.Minute).Format(time.RFC3339),
		},
	}}
	entries := parseAuthList(rawList)
	if len(entries) != 1 || !entries[0].QuotaCooling {
		t.Fatalf("expected parseAuthList to detect CPA cooling state: %+v", entries)
	}
	execAuthNotFound := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		return model, wakeupErrModelUnavail
	}
	res = e.wakeupEntry(entries[0], e.wakeupConfig, "test", "", now, wakeupSentRecord{}, nil, execAuthNotFound)
	if res.Status != "exhausted" || res.Reason != wakeupReasonExhausted {
		t.Fatalf("expected cooling account auth_not_found to report exhausted, got %+v", res)
	}

	// 3. Lightweight model ordering never prefers heavy flagship models (e.g. astra) or non-chat models.
	e.events = []Event{
		{At: now, AuthID: "codex-a", Provider: "codex", Model: "gpt-6-astra"},
		{At: now, AuthID: "codex-a", Provider: "codex", Model: "codex-auto-review"},
		{At: now, AuthID: "codex-a", Provider: "codex", Model: "gpt-image-2"},
	}
	e.mu.RLock()
	candidates := e.wakeupCandidateModelsLocked(AuthEntry{ID: "codex-a", Provider: "codex"}, e.wakeupConfig)
	e.mu.RUnlock()
	if len(candidates) == 0 || candidates[0] != "gpt-5.1-codex-mini" {
		t.Fatalf("expected lightweight mini model first, got %v", candidates)
	}
	for _, m := range candidates {
		if m == "codex-auto-review" || m == "gpt-image-2" {
			t.Fatalf("non-chat model %q must not be in wakeup candidates: %v", m, candidates)
		}
	}
	if candidates[len(candidates)-1] != "gpt-6-astra" {
		t.Fatalf("heavy flagship model gpt-6-astra should only appear last as fallback: %v", candidates)
	}
}

func TestWakeupAntigravityTOSViolationAndDisableReason(t *testing.T) {
	// 1. Error classification distinguishes TOS_VIOLATION, real auth_disabled, and unrelated "disabled" text.
	if got := classifyWakeupError(&hostRPCError{
		Method:     "host.model.execute",
		Code:       "tos_violation",
		StatusCode: 403,
		Detail:     "Your account has been disabled due to TOS_VIOLATION",
	}); got != wakeupErrTOSViolation {
		t.Fatalf("TOS_VIOLATION classified as %q, want %q", got, wakeupErrTOSViolation)
	}
	if got := classifyWakeupError(&hostRPCError{
		Method:     "host.model.execute",
		Code:       "auth_disabled",
		StatusCode: 409,
		Detail:     "credential is disabled",
	}); got != wakeupErrAccountDis {
		t.Fatalf("auth_disabled classified as %q, want %q", got, wakeupErrAccountDis)
	}
	if got := classifyWakeupError(&hostRPCError{
		Method:     "host.model.execute",
		StatusCode: 403,
		Detail:     "Cloud Code API has not been used in project 123 or it is disabled",
	}); got != wakeupErrAuthFailed {
		t.Fatalf("generic API disabled 403 classified as %q, want %q", got, wakeupErrAuthFailed)
	}
	if got := sanitizeWakeupReason("account_disabled"); got != wakeupErrAccountDis {
		t.Fatalf("legacy account_disabled should migrate to %q, got %q", wakeupErrAccountDis, got)
	}

	// 2. Antigravity wakeup stops immediately on TOS_VIOLATION without hammering the second group,
	// and syncs CPA's newly auto-disabled status + allow-listed disable_reason back into the snapshot.
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	now := time.Now().UTC()
	e.quotas["ag-1"] = AccountQuota{
		AuthID:   "ag-1",
		Provider: "antigravity",
		Name:     "google@example.com",
		Groups: []QuotaGroup{
			{Key: "gemini", Label: "Gemini"},
			{Key: "claude-gpt", Label: "Claude/GPT"},
		},
	}

	disabledByCPA := false
	execCalls := 0
	rpc := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			row := map[string]any{
				"id":         "ag-1",
				"auth_index": "i-ag",
				"provider":   "antigravity",
				"email":      "google@example.com",
				"disabled":   disabledByCPA,
			}
			if disabledByCPA {
				row["status"] = "disabled"
				row["status_message"] = "tos_violation"
			} else {
				// Non-allowlisted status messages must never leak into disable_reason.
				row["status_message"] = "secret-internal-token-text"
			}
			return map[string]any{"files": []any{row}}, nil
		}
		if method == "host.auth.models" {
			return nil, &hostRPCError{Method: method, Detail: "unsupported host callback host.auth.models"}
		}
		return nil, errors.New("unexpected rpc " + method)
	}
	exec := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		execCalls++
		disabledByCPA = true
		return model, wakeupErrTOSViolation
	}

	code, _, msg := e.kickWakeupRunWith("ag-1", true, now, rpc, exec, nil)
	if code != 202 || msg != "" {
		t.Fatalf("kickWakeupRunWith status=%d msg=%q", code, msg)
	}
	waitWakeupIdle(t, e)

	if execCalls != 1 {
		t.Fatalf("Antigravity TOS_VIOLATION must stop after first group, got %d exec calls", execCalls)
	}
	snap := e.snapshot()
	res := snap.Wakeup.Results["ag-1"]
	if res.Status != "failed" || res.Reason != wakeupErrTOSViolation {
		t.Fatalf("unexpected wakeup result after TOS_VIOLATION: %+v", res)
	}
	var found *QuotaView
	for i := range snap.Quotas {
		if snap.Quotas[i].AuthID == "ag-1" {
			found = &snap.Quotas[i]
			break
		}
	}
	if found == nil || !found.Disabled || found.DisableReason != "tos_violation" {
		t.Fatalf("expected snapshot to reflect CPA auto-disable TOS reason, got %+v", found)
	}
	if got := sanitizeDisableReason(true, "some raw upstream secret error"); got != "" {
		t.Fatalf("non-allowlisted disable reason must be stripped, got %q", got)
	}
}

func TestWakeupVerifiesWindowAndRetriesAfterMiss(t *testing.T) {
	at := time.Date(2026, 11, 26, 19, 0, 14, 0, time.UTC)
	// Reset margin: a window that "reset" seconds ago may still be the old one.
	win := &QuotaWindow{Used: 40, ResetAt: at.Add(-10 * time.Second).Format(time.RFC3339)}
	if !isFiveHourWindowActive(win, wakeupSentRecord{}, at) {
		t.Fatal("window inside the reset margin must stay active")
	}
	if isFiveHourWindowActive(win, wakeupSentRecord{}, at.Add(25*time.Second)) {
		t.Fatal("window past the reset margin must be inactive")
	}

	sent := wakeupSentRecord{At: at}
	if rec, reset, existing := verifyWakeupSentRecord("codex", sent, &QuotaWindow{Used: 1, ResetAt: at.Add(5 * time.Hour).Format(time.RFC3339)}, at.Add(time.Second)); !rec.RetryAt.IsZero() || rec.ResetAt.IsZero() || rec.Misses != 0 || existing || !reset.Equal(at.Add(5*time.Hour)) {
		t.Fatalf("opened codex window must be confirmed: rec=%+v existing=%v", rec, existing)
	}
	// Codex window started 1h earlier (4h01m remaining) must be recognized as already running, not newly opened.
	hourOld := &QuotaWindow{Used: 1, ResetAt: at.Add(4*time.Hour + time.Minute).Format(time.RFC3339)}
	if rec, reset, existing := verifyWakeupSentRecord("codex", sent, hourOld, at.Add(time.Second)); !existing || !rec.RetryAt.IsZero() || !rec.ResetAt.Equal(at.Add(4*time.Hour+time.Minute)) || !reset.Equal(at.Add(4*time.Hour+time.Minute)) {
		t.Fatalf("1h-old codex window must be marked existing without queuing retry: rec=%+v existing=%v", rec, existing)
	}
	// Claude aligns a new window to the top of the clock hour (e.g. 09:03:12 -> 13:59:59).
	claudeAt := time.Date(2026, 11, 26, 9, 3, 12, 0, time.UTC)
	claudeNew := time.Date(2026, 11, 26, 13, 59, 59, 0, time.UTC)
	if rec, reset, existing := verifyWakeupSentRecord("claude", wakeupSentRecord{At: claudeAt}, &QuotaWindow{Used: 1, ResetAt: claudeNew.Format(time.RFC3339)}, claudeAt.Add(time.Second)); existing || !rec.RetryAt.IsZero() || !reset.Equal(claudeNew) {
		t.Fatalf("claude top-of-hour new window must be confirmed: rec=%+v existing=%v", rec, existing)
	}
	claudePrev := time.Date(2026, 11, 26, 12, 59, 59, 0, time.UTC)
	if rec, reset, existing := verifyWakeupSentRecord("claude", wakeupSentRecord{At: claudeAt}, &QuotaWindow{Used: 1, ResetAt: claudePrev.Format(time.RFC3339)}, claudeAt.Add(time.Second)); !existing || !rec.RetryAt.IsZero() || !reset.Equal(claudePrev) {
		t.Fatalf("claude prior-hour window must be marked existing without queuing retry: rec=%+v existing=%v", rec, existing)
	}
	// Landed in the expiring window (resets 6s after the send): 1st miss retries right past the reset margin.
	old := &QuotaWindow{Used: 40, ResetAt: at.Add(6 * time.Second).Format(time.RFC3339)}
	rec, reset, existing := verifyWakeupSentRecord("codex", sent, old, at.Add(2*time.Second))
	if rec.Misses != 1 || existing || !reset.IsZero() || !rec.RetryAt.Equal(at.Add(6*time.Second+wakeupResetMargin)) {
		t.Fatalf("first miss on expiring window must retry right past reset margin: rec=%+v existing=%v", rec, existing)
	}
	// Second miss backs off so providers are not spammed.
	rec2, _, _ := verifyWakeupSentRecord("codex", rec, old, at.Add(40*time.Second))
	if rec2.Misses != 2 || !rec2.RetryAt.Equal(at.Add(40*time.Second+5*time.Minute)) {
		t.Fatalf("second miss must back off 5m: rec=%+v", rec2)
	}
	// Sent in the middle of a running window (2h remaining): record reset, do NOT queue off-Cron retry.
	long := &QuotaWindow{Used: 40, ResetAt: at.Add(2 * time.Hour).Format(time.RFC3339)}
	if rec, reset, existing := verifyWakeupSentRecord("codex", wakeupSentRecord{At: at, Misses: wakeupMaxQuickRetries}, long, at.Add(time.Second)); !existing || !rec.RetryAt.IsZero() || rec.Misses != 0 || !rec.ResetAt.Equal(at.Add(2*time.Hour)) || !reset.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("running window must record reset without queuing an off-schedule retry: rec=%+v existing=%v", rec, existing)
	}
	if rec, _, _ := verifyWakeupSentRecord("codex", wakeupSentRecord{At: at, Misses: wakeupMaxQuickRetries}, old, at); !rec.RetryAt.IsZero() {
		t.Fatalf("quick retries must be bounded: %+v", rec)
	}
	if rec, _, _ := verifyWakeupSentRecord("codex", sent, &QuotaWindow{Used: 3}, at); rec != sent {
		t.Fatalf("usage without a reset time must keep the record: %+v", rec)
	}

	// Daily mode: send even though cached quota says a window is active, then retry once
	// the fresh read shows the wakeup landed in the expiring window.
	start := time.Now().UTC()
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	e.wakeupConfig = WakeupConfig{
		Enabled: true, Mode: "daily", Cron: fmt.Sprintf("0 %d * * *", (start.Hour()+12)%24),
		Providers: map[string]bool{"claude": true, "codex": true, "antigravity": true}, Message: "hi",
	}
	e.quotas["cx"] = AccountQuota{AuthID: "cx", Provider: "codex", Name: "cx@example.com", FetchedAt: start.Format(time.RFC3339Nano),
		Five: &QuotaWindow{Used: 30, ResetAt: start.Add(10 * time.Second).Format(time.RFC3339)}}
	rpc := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{map[string]any{"id": "cx", "auth_index": "i1", "provider": "codex", "email": "cx@example.com"}}}, nil
		}
		return nil, nil
	}
	var mu sync.Mutex
	sends := 0
	exec := func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		mu.Lock()
		sends++
		mu.Unlock()
		return model, ""
	}
	refresh := func(entry AuthEntry) bool {
		mu.Lock()
		n := sends
		mu.Unlock()
		five := &QuotaWindow{Used: 30, ResetAt: start.Add(10 * time.Second).Format(time.RFC3339)}
		if n >= 2 {
			five = &QuotaWindow{Used: 1, ResetAt: time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339)}
		}
		e.mu.Lock()
		q := e.quotas[entry.ID]
		q.FetchedAt, q.Five = time.Now().UTC().Format(time.RFC3339Nano), five
		e.quotas[entry.ID] = q
		e.mu.Unlock()
		return true
	}
	if status, _, failure := e.kickWakeupRunWith("", false, start, rpc, exec, refresh); status != 202 || failure != "" {
		t.Fatalf("kick status=%d failure=%q", status, failure)
	}
	waitWakeupIdle(t, e)
	if res := e.snapshot().Wakeup.Results["cx"]; sends != 1 || res.Status != "sent" || res.RetryAt == "" {
		t.Fatalf("expected a blind send with a queued retry, sends=%d result=%+v", sends, res)
	}
	e.checkWakeupScheduleWith(start.Add(30*time.Second), rpc, exec, refresh)
	waitWakeupIdle(t, e)
	if sends != 1 {
		t.Fatalf("retry fired before it was due: sends=%d", sends)
	}
	e.checkWakeupScheduleWith(start.Add(3*time.Minute), rpc, exec, refresh)
	waitWakeupIdle(t, e)
	if res := e.snapshot().Wakeup.Results["cx"]; sends != 2 || res.RetryAt != "" || res.WindowResetAt == "" {
		t.Fatalf("expected one verified retry, sends=%d result=%+v", sends, res)
	}
	e.checkWakeupScheduleWith(start.Add(6*time.Minute), rpc, exec, refresh)
	waitWakeupIdle(t, e)
	if sends != 2 {
		t.Fatalf("verified window must stop retries: sends=%d", sends)
	}
}

func TestWakeupBothModeRestsBeforeCronSlot(t *testing.T) {
	e := newEngine()
	e.dir = t.TempDir()
	e.load()
	e.wakeupConfig = WakeupConfig{
		Enabled:           true,
		Mode:              "both",
		Cron:              "0 6,11,16,21 * * *",
		TimezoneOffsetMin: -480,
		Providers:         map[string]bool{"claude": true, "codex": true, "antigravity": true},
		Message:           "hi",
	}
	loc := wakeupZone(-480)
	var mu sync.Mutex
	sends := 0
	rpc := func(method string, _ any) (any, error) {
		if method == "host.auth.list" {
			return map[string]any{"files": []any{
				map[string]any{"id": "cx", "auth_index": "i1", "provider": "codex", "email": "cx@example.com"},
			}}, nil
		}
		return nil, nil
	}
	exec := func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		mu.Lock()
		sends++
		mu.Unlock()
		return model, ""
	}
	setIdleAt := func(ts time.Time) {
		e.mu.Lock()
		e.quotas["cx"] = AccountQuota{
			AuthID: "cx", Provider: "codex", Name: "cx@example.com",
			FetchedAt: ts.UTC().Format(time.RFC3339Nano),
			Five:      &QuotaWindow{Used: 0},
		}
		e.mu.Unlock()
	}

	// Overnight 02:01 and 05:01 PST: next Cron is 06:00 (< 5h05m away) -> must rest so 06:00 aligns.
	for _, h := range []int{2, 5} {
		ts := time.Date(2026, 11, 26, h, 1, 0, 0, loc)
		setIdleAt(ts)
		e.checkWakeupScheduleWith(ts, rpc, exec, nil)
		waitWakeupIdle(t, e)
		if sends != 0 {
			t.Fatalf("both mode must not auto-wake at %02d:01 before 06:00 Cron slot (sends=%d)", h, sends)
		}
	}
	// At 06:00 PST, the Cron slot fires cleanly.
	at0600 := time.Date(2026, 11, 26, 6, 0, 12, 0, loc)
	setIdleAt(at0600)
	e.checkWakeupScheduleWith(at0600, rpc, exec, nil)
	waitWakeupIdle(t, e)
	if sends != 1 {
		t.Fatalf("expected 06:00 Cron slot to fire, sends=%d", sends)
	}

	// With a once-daily 06:00 Cron + auto-reset, 11:01 PST has 18h59m until next Cron (>= 5h05m) -> auto-wakes.
	e.mu.Lock()
	e.wakeupConfig.Cron = "0 6 * * *"
	e.wakeupLastSent["cx"] = wakeupSentRecord{At: at0600.UTC(), ResetAt: at0600.Add(5 * time.Hour).UTC()}
	e.mu.Unlock()
	at1101 := time.Date(2026, 11, 26, 11, 1, 0, 0, loc)
	setIdleAt(at1101)
	e.checkWakeupScheduleWith(at1101, rpc, exec, nil)
	waitWakeupIdle(t, e)
	if sends != 2 {
		t.Fatalf("both mode should auto-wake when next Cron is >= 5h05m away, sends=%d", sends)
	}
}

func TestWakeupPersistenceMigratesLegacyRetriesTogether(t *testing.T) {
	at := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)
	cases := []struct {
		name, provider               string
		status, resetSide            string // resetSide is account or Gemini; empty for v0.1.44 or a quick retry.
		delay, groupOffset           time.Duration
		migrate, modern, staleResult bool
		zeroMisses                   bool
	}{
		{name: "v45-short", provider: "codex", delay: 20*time.Minute + wakeupResetMargin, resetSide: "account", migrate: true},
		{name: "v46-long", provider: "claude", delay: 2*time.Hour + wakeupResetMargin, resetSide: "account", migrate: true},
		{name: "v46-Gemini-group-only", provider: "antigravity", delay: 6*time.Minute + wakeupResetMargin, resetSide: "gemini", migrate: true, zeroMisses: true},
		{name: "v46-Gemini-group-formula-tolerance", provider: "antigravity", delay: 7 * time.Minute, resetSide: "gemini", groupOffset: 6*time.Minute + 28*time.Second, migrate: true, zeroMisses: true},
		{name: "v46-Gemini-late-verification", provider: "antigravity", delay: 6*time.Minute + 40*time.Second, resetSide: "gemini", groupOffset: 5*time.Minute + 10*time.Second, zeroMisses: true},
		{name: "v44-Gemini-existing-window-with-miss-count", provider: "antigravity", delay: 20*time.Minute + wakeupResetMargin, resetSide: "gemini", migrate: true},
		{name: "v46-Gemini-observed-expiring-window", provider: "antigravity", delay: 2 * time.Minute, resetSide: "gemini", groupOffset: 10 * time.Second},
		{name: "v46-Gemini-observed-at-five-minute-boundary", provider: "antigravity", delay: 5*time.Minute + wakeupResetMargin, resetSide: "gemini", groupOffset: 5 * time.Minute},
		{name: "v46-Gemini-account-only", provider: "antigravity", delay: 20*time.Minute + wakeupResetMargin, resetSide: "account", migrate: true},
		{name: "v44-Codex", provider: "codex", delay: 16*time.Minute + wakeupResetMargin, migrate: true},
		{name: "v44-Gemini", provider: "antigravity", delay: 2*time.Hour + wakeupResetMargin, migrate: true},
		{name: "quick-2m", provider: "codex", delay: 2 * time.Minute},
		{name: "quick-5m", provider: "antigravity", delay: 5 * time.Minute},
		{name: "quick-10m", provider: "claude", delay: 10 * time.Minute},
		{name: "new-late-verification", provider: "codex", delay: 20 * time.Minute, modern: true},
		{name: "new-exhausted", provider: "antigravity", status: "exhausted", delay: 40 * time.Second, modern: true},
		{name: "stale-result-keep-later-quick-retry", provider: "codex", delay: 20*time.Minute + wakeupResetMargin, resetSide: "account", migrate: true, staleResult: true},
	}
	cfg := defaultWakeupConfig()
	cfg.Enabled, cfg.Mode = true, "reset"
	saved := persistedWakeup{
		Config: cfg, LastSent: make(map[string]wakeupSentRecord), Results: make(map[string]WakeupAccountResult),
	}
	for _, tc := range cases {
		retry := at.Add(tc.delay)
		res := WakeupAccountResult{Provider: tc.provider, Status: tc.status, At: at.Format(time.RFC3339Nano), RetryAt: retry.Format(time.RFC3339Nano)}
		if res.Status == "" {
			res.Status = "sent"
		}
		reset := retry.Add(-wakeupResetMargin).Format(time.RFC3339Nano)
		if tc.resetSide == "account" {
			res.WindowResetAt = reset
		}
		if tc.provider == "antigravity" {
			g := WakeupGroupResult{Group: "gemini", Status: res.Status, At: res.At}
			if tc.resetSide == "gemini" {
				g.WindowResetAt = reset
				if tc.groupOffset != 0 {
					g.WindowResetAt = at.Add(tc.groupOffset).Format(time.RFC3339Nano)
				}
			}
			res.Groups = []WakeupGroupResult{g}
		}
		rec := wakeupSentRecord{At: at, RetryAt: retry, Misses: 1}
		if tc.zeroMisses {
			rec.Misses = 0
		}
		if tc.staleResult {
			rec.At, rec.RetryAt = at.Add(time.Minute), at.Add(3*time.Minute)
		}
		if tc.modern {
			rec.RetrySource, rec.RetrySlot, rec.RetryUntil = "schedule", "slot", at.Add(30*time.Minute)
		}
		saved.Results[tc.name] = res
		key := tc.name
		if tc.provider == "antigravity" {
			key += "#gemini"
		}
		saved.LastSent[key] = rec
	}
	saved.LastSent["without-results"] = wakeupSentRecord{At: at, RetryAt: at.Add(45*time.Minute + wakeupResetMargin), Misses: 1}
	saved.LastSent["without-results-zero-misses"] = wakeupSentRecord{At: at, RetryAt: at.Add(8*time.Minute + wakeupResetMargin)}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine()
	e.dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(e.dir, wakeupConfigFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	e.load()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := wakeupRecordKey(tc.name, tc.provider)
			rec, res := e.wakeupLastSent[key], e.wakeupResults[tc.name]
			want := saved.Results[tc.name]
			if res.AuthID != tc.name || res.Status != want.Status {
				t.Fatalf("state not reloaded: %+v", res)
			}
			if tc.migrate {
				reset := at.Add(tc.delay - wakeupResetMargin)
				if tc.groupOffset != 0 {
					reset = at.Add(tc.groupOffset)
				}
				if res.RetryAt != "" || !res.WindowExisting || res.WindowResetAt != reset.Format(time.RFC3339Nano) {
					t.Fatalf("existing window result not migrated: %+v", res)
				}
				if !tc.staleResult && (!rec.RetryAt.IsZero() || !rec.ResetAt.Equal(reset) || rec.Misses != 0 || !rec.legacyRetry()) {
					t.Fatalf("existing window send record not migrated: %+v", rec)
				}
			} else if res.RetryAt != want.RetryAt || res.WindowExisting || res.WindowResetAt != "" {
				t.Fatalf("quick retry result changed: %+v", res)
			}
			if !tc.migrate || tc.staleResult {
				wantRec := saved.LastSent[key]
				wantRec.RetryUntil = wantRec.At.Add(wakeupQuickRetryWindow)
				if rec != wantRec {
					t.Fatalf("quick retry record=%+v, want %+v", rec, wantRec)
				}
			}
			if tc.provider == "antigravity" && (len(res.Groups) != 1 || res.Groups[0].WindowResetAt != res.WindowResetAt) {
				t.Fatalf("Gemini reset differs between account and group: %+v", res)
			}
		})
	}
	if rec := e.wakeupLastSent["without-results"]; !rec.RetryAt.IsZero() || !rec.ResetAt.Equal(at.Add(45*time.Minute)) {
		t.Fatalf("standalone legacy record was not migrated: %+v", rec)
	}
	if rec := e.wakeupLastSent["without-results-zero-misses"]; !rec.RetryAt.IsZero() || !rec.ResetAt.Equal(at.Add(8*time.Minute)) {
		t.Fatalf("standalone zero-miss record was not migrated: %+v", rec)
	}
}

func TestWakeupReloadsLegacyRetryOriginAndDispatchesIt(t *testing.T) {
	slotAt := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	ts := func(t time.Time) string { return t.Format(time.RFC3339Nano) }
	firedAt := func(at time.Time) string { return fmt.Sprintf(`{%q:%q}`, cronSlotKey(at, 0), ts(at)) }
	for _, tc := range []struct {
		name, wantSource, lastTrigger                     string
		fired, mode, cron                                 string
		sendOffset, lastRunOffset, resetDelay, retryDelay time.Duration
		existing                                          bool
	}{
		{name: "fired-slot", wantSource: "schedule", lastTrigger: "manual", lastRunOffset: 2 * time.Second,
			fired: fmt.Sprintf(`{%q:%q,%q:%q,%q:%q}`, cronSlotKey(slotAt, 0), ts(slotAt), "older", ts(slotAt.Add(-5*time.Minute)), "late", ts(slotAt.Add(2*time.Second)))},
		{name: "last-run-schedule", wantSource: "schedule", lastTrigger: "schedule", fired: firedAt(slotAt)},
		{name: "no-evidence", lastTrigger: "schedule"},
		{name: "manual-wins-over-slot", wantSource: "manual", lastTrigger: "manual", lastRunOffset: -time.Second, fired: firedAt(slotAt)},
		{name: "test", wantSource: "test", lastTrigger: "test"},
		{name: "account", wantSource: "account", lastTrigger: "account", lastRunOffset: time.Second},
		{name: "manual-outside-batch", lastTrigger: "manual", lastRunOffset: -2 * time.Second},
		{name: "reset-after-cron", wantSource: "reset", lastTrigger: "reset", fired: firedAt(slotAt),
			mode: "both", cron: "0 11,16 * * *", sendOffset: 10 * time.Minute},
		{name: "no-batch-evidence-after-cron", lastTrigger: "manual", lastRunOffset: -2 * time.Second, fired: firedAt(slotAt),
			mode: "both", cron: "0 11,16 * * *", sendOffset: 10 * time.Minute},
		{name: "v44-existing-window-does-not-retry", lastTrigger: "schedule", fired: firedAt(slotAt.Add(10 * time.Hour)),
			cron: "0 21 * * *", sendOffset: 10 * time.Hour, resetDelay: 20 * time.Minute, retryDelay: 20*time.Minute + 30*time.Second, existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := slotAt.Add(tc.sendOffset)
			resetAt, due := at.Add(tc.resetDelay), at.Add(tc.retryDelay)
			if tc.resetDelay == 0 {
				resetAt = at.Add(10 * time.Second)
			}
			if tc.retryDelay == 0 {
				due = at.Add(2 * time.Minute)
			}
			fired, mode, cron := tc.fired, tc.mode, tc.cron
			if mode == "" {
				mode = "daily"
			}
			if cron == "" {
				cron = "0 11 * * *"
			}
			if fired == "" {
				// Keep the next Cron slot away from the retry tick so a new run
				// cannot mask a missing retry dispatch.
				fired, cron = "{}", "0 12 * * *"
			}
			body := fmt.Sprintf(`{
				"config":{"enabled":true,"mode":%q,"cron":%q,"timezone_offset_min":0,
					"providers":{"antigravity":true},"models":{"antigravity:gemini":"gemini-3.5-flash-lite"}},
				"fired_slots":%s,"last_run_at":%q,"last_run_trigger":%q,
				"last_sent":{"ag#gemini":{"at":%q,"retry_at":%q,"misses":1}},
				"results":{"ag":{"provider":"antigravity","status":"sent","at":%q,"retry_at":%q,
					"groups":[{"group":"gemini","status":"sent","at":%q,"window_reset_at":%q}]}}
			}`, mode, cron, fired, ts(at.Add(tc.lastRunOffset)), tc.lastTrigger, ts(at), ts(due), ts(at), ts(due), ts(at), ts(resetAt))
			e := newEngine()
			e.dir = t.TempDir()
			if err := os.WriteFile(filepath.Join(e.dir, wakeupConfigFileName), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			e.load()
			rec, res := e.wakeupLastSent["ag#gemini"], e.wakeupResults["ag"]
			wantSlot, wantRetry, wantReset, wantAdvertised := "", "", "", ""
			wantCalls := 0
			if tc.wantSource != "" {
				wantRetry = ts(due)
				if tc.wantSource != "reset" {
					wantAdvertised, wantCalls = wantRetry, 1
				}
			}
			if tc.wantSource == "schedule" {
				wantSlot = cronSlotKey(at, 0)
			}
			if tc.existing {
				wantReset = ts(resetAt)
				if !rec.ResetAt.Equal(resetAt) {
					t.Fatalf("existing window reset was not retained: %+v", rec)
				}
			}
			if rec.RetrySource != tc.wantSource || rec.RetrySlot != wantSlot || (!rec.RetryAt.IsZero()) != (wantRetry != "") {
				t.Fatalf("incorrect recovered retry: %+v", rec)
			}
			if res.RetryAt != wantRetry || res.WindowResetAt != wantReset || len(res.Groups) != 1 || res.Groups[0].WindowResetAt != wantReset || res.WindowExisting != tc.existing {
				t.Fatalf("incorrect migrated result: %+v", res)
			}
			if advertised := e.wakeupStateLocked(due).Results["ag"].RetryAt; advertised != wantAdvertised {
				t.Fatalf("advertised retry=%q, want %q", advertised, wantAdvertised)
			}
			e.quotas["ag"] = AccountQuota{AuthID: "ag", Provider: "antigravity", FetchedAt: ts(due),
				Groups: []QuotaGroup{{Key: "gemini", Five: &QuotaWindow{Used: 5, ResetAt: ts(resetAt)}}}}
			rpc := func(method string, _ any) (any, error) {
				if method != "host.auth.list" {
					t.Errorf("unexpected RPC: %s", method)
				}
				return map[string]any{"files": []any{map[string]any{"id": "ag", "auth_index": "i1", "provider": "antigravity"}}}, nil
			}
			calls := 0
			exec := func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
				calls++
				return model, ""
			}
			e.checkWakeupScheduleWith(due, rpc, exec, nil)
			waitWakeupIdle(t, e)
			e.checkWakeupScheduleWith(due.Add(30*time.Second), rpc, exec, nil)
			waitWakeupIdle(t, e)
			if calls != wantCalls {
				t.Fatalf("dispatched %d legacy retries, want %d", calls, wantCalls)
			}
		})
	}
}

func TestWakeupQuickRetryKeepsOriginalSlotAndDeadline(t *testing.T) {
	at := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)
	const slot = "2026-11-26|21:00|-480"
	first := newWakeupSentRecord(wakeupSentRecord{}, at, "schedule", slot)
	first, _, _ = verifyWakeupSentRecord("codex", first, &QuotaWindow{}, at)
	if !first.RetryAt.Equal(at.Add(2*time.Minute)) || !first.RetryUntil.Equal(at.Add(30*time.Minute)) {
		t.Fatalf("initial retry not bounded by original send: %+v", first)
	}
	followed := newWakeupSentRecord(first, at.Add(20*time.Minute), "reset", "")
	if followed.RetrySource != "schedule" || followed.RetrySlot != slot || !followed.RetryUntil.Equal(first.RetryUntil) || followed.Misses != 1 {
		t.Fatalf("follow-up lost original retry metadata: %+v", followed)
	}
	followed, _, _ = verifyWakeupSentRecord("codex", followed, &QuotaWindow{}, at.Add(25*time.Minute))
	if !followed.RetryAt.Equal(first.RetryUntil) || followed.Misses != 2 {
		t.Fatalf("retry exactly at deadline should be retained: %+v", followed)
	}
	next := newWakeupSentRecord(followed, followed.RetryAt, "reset", "")
	outOfTime, _, _ := verifyWakeupSentRecord("codex", next, &QuotaWindow{}, next.At)
	if !outOfTime.RetryAt.IsZero() || !outOfTime.legacyRetry() {
		t.Fatalf("retry past original deadline was queued: %+v", outOfTime)
	}
	followed.Misses = wakeupMaxQuickRetries
	outOfTries, _, _ := verifyWakeupSentRecord("codex", followed, &QuotaWindow{}, at)
	if !outOfTries.RetryAt.IsZero() || !outOfTries.legacyRetry() {
		t.Fatalf("exhausted retry retained retry metadata: %+v", outOfTries)
	}
	for _, delay := range []time.Duration{time.Hour, 5 * time.Hour} {
		reset := followed.At.Add(delay)
		confirmed, _, _ := verifyWakeupSentRecord("codex", followed, &QuotaWindow{Used: 1, ResetAt: reset.Format(time.RFC3339)}, followed.At)
		if !confirmed.RetryAt.IsZero() || !confirmed.legacyRetry() || !confirmed.ResetAt.Equal(reset) || confirmed.Misses != 0 {
			t.Fatalf("confirmation did not clear retry state: %+v", confirmed)
		}
	}
	fresh := newWakeupSentRecord(first, followed.At, "schedule", "new-slot")
	if fresh.RetrySlot != "new-slot" || !fresh.RetryUntil.Equal(followed.At.Add(30*time.Minute)) || fresh.Misses != 0 {
		t.Fatalf("new schedule reused an old retry chain: %+v", fresh)
	}
	tooLate := first
	tooLate.RetryAt = at.Add(time.Hour)
	if isFiveHourWindowActive(nil, tooLate, at.Add(31*time.Minute)) {
		t.Fatal("expired retry must not keep an idle window active")
	}
	e := newEngine()
	e.wakeupConfig.Enabled = true
	retryAt := first.RetryAt.Format(time.RFC3339Nano)
	e.wakeupLastSent["ag#gemini"] = first
	e.wakeupResults["ag"] = WakeupAccountResult{Provider: "antigravity", Status: "exhausted", RetryAt: retryAt}
	if got := e.wakeupStateLocked(first.RetryUntil).Results["ag"].RetryAt; got != retryAt {
		t.Fatalf("live retry missing from snapshot: %q", got)
	}
	if got := e.wakeupStateLocked(first.RetryUntil.Add(time.Nanosecond)).Results["ag"].RetryAt; got != "" {
		t.Fatalf("snapshot advertised an expired retry: %q", got)
	}
	first.RetryAt = first.RetryAt.Add(time.Minute)
	e.wakeupLastSent["ag#gemini"] = first
	if got := e.wakeupStateLocked(at).Results["ag"].RetryAt; got != "" {
		t.Fatalf("snapshot advertised a different queued retry: %q", got)
	}
	delete(e.wakeupLastSent, "ag#gemini")
	if got := e.wakeupStateLocked(at).Results["ag"].RetryAt; got != "" {
		t.Fatalf("snapshot advertised a missing queued retry: %q", got)
	}
	if e.wakeupResults["ag"].RetryAt != retryAt {
		t.Fatal("snapshot must not modify persisted tracking")
	}
}

func TestWakeupQuickRetryCannotBypassCronRestAfterExpiryOrFromReset(t *testing.T) {
	now := time.Date(2026, 11, 27, 4, 0, 0, 0, wakeupZone(-480))
	for _, tc := range []struct {
		name, mode, source string
		age                time.Duration
		wantSend           bool
	}{
		{name: "schedule-follow-up", mode: "both", source: "schedule", age: 5 * time.Minute, wantSend: true},
		{name: "missed-overnight", mode: "both", source: "schedule", age: 7 * time.Hour},
		{name: "old-no-deadline", mode: "both", age: 7 * time.Hour},
		{name: "live-legacy-source-rests", mode: "both", age: 5 * time.Minute},
		{name: "unknown-source-rests", mode: "both", source: "unknown", age: 5 * time.Minute},
		{name: "legacy-source-can-run-when-auto-reset-allowed", mode: "reset", age: 5 * time.Minute, wantSend: true},
		{name: "ordinary-reset-rests", mode: "both", source: "reset", age: 5 * time.Minute},
		{name: "reset-only-does-not-rest", mode: "reset", source: "reset", age: 5 * time.Minute, wantSend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			e.wakeupConfig = WakeupConfig{Enabled: true, Mode: tc.mode, Cron: "0 6,11,16,21 * * *", TimezoneOffsetMin: -480,
				Providers: map[string]bool{"codex": true}}
			e.quotas["cx"] = AccountQuota{AuthID: "cx", Provider: "codex", FetchedAt: now.Format(time.RFC3339), Five: &QuotaWindow{}}
			at := now.Add(-tc.age)
			rec := wakeupSentRecord{At: at, RetryAt: at.Add(2 * time.Minute), Misses: 1, RetrySource: tc.source}
			if tc.source != "" {
				rec.RetryUntil = at.Add(30 * time.Minute)
			}
			e.wakeupLastSent["cx"] = rec
			e.wakeupResults["cx"] = WakeupAccountResult{Provider: "codex", Status: "sent", RetryAt: rec.RetryAt.Format(time.RFC3339Nano)}
			if advertised := e.wakeupStateLocked(now).Results["cx"].RetryAt; (advertised != "") != tc.wantSend {
				t.Fatalf("snapshot retry_at=%q, want retry available=%v", advertised, tc.wantSend)
			}
			sent := false
			e.checkWakeupScheduleWith(now, func(string, any) (any, error) {
				return map[string]any{"files": []any{map[string]any{"id": "cx", "auth_index": "i1", "provider": "codex"}}}, nil
			}, func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
				sent = true
				return model, ""
			}, nil)
			waitWakeupIdle(t, e)
			if sent != tc.wantSend {
				t.Fatalf("sent=%v, want %v (retry=%+v)", sent, tc.wantSend, rec)
			}
		})
	}
}

func TestWakeupFailedFollowUpConsumesQueuedRetry(t *testing.T) {
	for _, tc := range []struct {
		mode, provider, failure, status string
	}{
		{mode: "daily", provider: "codex", failure: wakeupErrRateLimited, status: "exhausted"},
		{mode: "reset", provider: "antigravity", failure: wakeupErrAuthFailed, status: "failed"},
	} {
		t.Run(tc.mode+"/"+tc.provider+"/"+tc.failure, func(t *testing.T) {
			due := time.Now().UTC()
			key := wakeupRecordKey("acct", tc.provider)
			retry := wakeupSentRecord{
				At: due.Add(-2 * time.Minute), RetryAt: due, Misses: 1, RetrySource: "schedule", RetrySlot: "slot",
			}
			if tc.mode == "reset" {
				// Model 11:00 send, 11:05 observed reset and 11:05:30 claim.
				retry.At, retry.ResetAt = due.Add(-5*time.Minute-30*time.Second), due.Add(-30*time.Second)
				retry.RetrySource, retry.RetrySlot = "reset", ""
			}
			firstAt := retry.At
			retry.RetryUntil = firstAt.Add(30 * time.Minute)
			e := newEngine()
			e.dir = t.TempDir()
			e.wakeupConfig = WakeupConfig{
				Enabled: true, Mode: tc.mode, Cron: fmt.Sprintf("0 %d * * *", (due.Hour()+12)%24),
				Providers: map[string]bool{tc.provider: true}, Models: map[string]string{wakeupGeminiKey: "gemini-3.5-flash-lite"},
			}
			e.wakeupLastSent[key] = retry
			e.wakeupResults["acct"] = WakeupAccountResult{AuthID: "acct", Provider: tc.provider, Status: "sent",
				At: firstAt.Format(time.RFC3339Nano), RetryAt: due.Format(time.RFC3339Nano)}
			q := AccountQuota{AuthID: "acct", Provider: tc.provider, FetchedAt: due.Format(time.RFC3339Nano), Five: &QuotaWindow{}}
			if tc.provider == "antigravity" {
				q.Groups = []QuotaGroup{{Key: "gemini", Five: &QuotaWindow{}}}
			}
			e.quotas["acct"] = q
			rpc := func(_ string, _ any) (any, error) {
				return map[string]any{"files": []any{map[string]any{"id": "acct", "auth_index": "i1", "provider": tc.provider}}}, nil
			}
			if err := e.writeWakeupLocked(firstAt); err != nil {
				t.Fatal(err)
			}
			calls := 0
			var reloaded *Engine
			var claimAt time.Time
			exec := func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
				calls++
				e.mu.RLock()
				claimed := e.wakeupLastSent[key]
				claimAt = claimed.At
				if claimAt.Before(due) || !claimed.ResetAt.IsZero() || !claimed.RetryAt.IsZero() {
					t.Errorf("claim time and consumed retry must be recorded before model I/O: %+v", claimed)
				}
				if pending := e.wakeupResults["acct"].RetryAt; pending != "" {
					t.Errorf("stored result promised another attempt while the consumed retry ran: %s", pending)
				}
				e.mu.RUnlock()
				// Simulate a restart using only the disk state visible before the model
				// returns and the original batch can persist any final result.
				reloaded = newEngine()
				reloaded.dir = e.dir
				reloaded.load()
				return model, tc.failure
			}
			e.checkWakeupScheduleWith(due, rpc, exec, nil)
			waitWakeupIdle(t, e)
			if res := e.wakeupResults["acct"]; calls != 1 || res.Status != tc.status || res.Reason != tc.failure || res.RetryAt != "" {
				t.Fatalf("failed follow-up must not requeue: calls=%d result=%+v", calls, res)
			}
			if rec := reloaded.wakeupLastSent[key]; !rec.At.Equal(claimAt) || !rec.ResetAt.IsZero() || !rec.RetryAt.IsZero() {
				t.Fatalf("consumption was not persisted at model dispatch: %+v", rec)
			}
			if res := reloaded.wakeupResults["acct"]; res.Status != "sent" || res.RetryAt != "" {
				t.Fatalf("pre-dispatch write did not clear the historical result retry: %+v", res)
			}
			if reloaded.wakeupConfig.Mode != tc.mode {
				t.Fatalf("reload used %q mode, want %q", reloaded.wakeupConfig.Mode, tc.mode)
			}
			reloaded.quotas["acct"] = q
			replays := 0
			// Quota is still fresh and idle; stale reset evidence must not bypass cooldown.
			reloaded.checkWakeupScheduleWith(claimAt.Add(6*time.Minute), rpc, func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
				replays++
				return model, ""
			}, nil)
			waitWakeupIdle(t, reloaded)
			if replays != 0 {
				t.Fatalf("reloaded engine replayed a consumed retry: calls=%d", replays)
			}
			failedAt := e.wakeupLastFail["acct"]
			afterBackoff := failedAt.Add(15*time.Minute + time.Second)
			if failedAt.IsZero() || !afterBackoff.Before(retry.RetryUntil) {
				t.Fatalf("test must reach the end of failure backoff before the original deadline: failedAt=%v retry=%+v", failedAt, retry)
			}
			e.checkWakeupScheduleWith(afterBackoff, rpc, exec, nil)
			waitWakeupIdle(t, e)
			if calls != 1 {
				t.Fatalf("failed follow-up fired again after backoff: calls=%d", calls)
			}
		})
	}
}

func TestWakeupDoesNotDispatchRetryWhenClaimCannotPersist(t *testing.T) {
	due := time.Date(2026, 11, 26, 11, 2, 0, 0, time.UTC)
	for _, readOnly := range []bool{true, false} {
		name := "write-error"
		if readOnly {
			name = "read-only"
		}
		t.Run(name, func(t *testing.T) {
			e := newEngine()
			e.dir = t.TempDir()
			if readOnly {
				e.blockStorageWrites("test partial load")
			} else if err := os.Mkdir(filepath.Join(e.dir, wakeupConfigFileName), 0700); err != nil {
				t.Fatal(err)
			}
			e.wakeupConfig = WakeupConfig{Enabled: true, Mode: "daily", Cron: "0 12 * * *", Providers: map[string]bool{"codex": true}}
			rec := wakeupSentRecord{At: due.Add(-2 * time.Minute), RetryAt: due, RetrySource: "schedule", Misses: 1, RetryUntil: due.Add(28 * time.Minute)}
			e.wakeupLastSent["cx"] = rec
			retryAt := due.Format(time.RFC3339)
			e.wakeupResults["cx"] = WakeupAccountResult{Provider: "codex", Status: "sent", RetryAt: retryAt}
			e.quotas["cx"] = AccountQuota{Provider: "codex", AuthID: "cx", FetchedAt: retryAt, Five: &QuotaWindow{}}
			calls := 0
			e.checkWakeupScheduleWith(due, func(_ string, _ any) (any, error) {
				return map[string]any{"files": []any{map[string]any{"id": "cx", "auth_index": "i1", "provider": "codex"}}}, nil
			}, func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
				calls++
				return model, ""
			}, nil)
			waitWakeupIdle(t, e)
			if calls != 0 {
				t.Fatalf("dispatched %d retries without a successful claim write", calls)
			}
			if e.wakeupLastSent["cx"] != rec || e.wakeupResults["cx"].RetryAt != retryAt {
				t.Fatal("failed claim did not restore both queue and result")
			}
		})
	}
}

func TestWakeupScheduleDefersUntilExhaustedWindowResets(t *testing.T) {
	slot := time.Date(2026, 11, 26, 11, 0, 0, 0, wakeupZone(-480))
	for _, tc := range []struct {
		name, provider          string
		sendOffset, resetOffset time.Duration
	}{
		{name: "codex-before-reset", provider: "codex", resetOffset: 10 * time.Second},
		{name: "claude-after-reset-within-margin", provider: "claude", sendOffset: 5 * time.Second, resetOffset: -time.Second},
		{name: "gemini-after-reset-within-margin", provider: "antigravity", sendOffset: 5 * time.Second, resetOffset: -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := tc.provider
			start, reset := slot.Add(tc.sendOffset), slot.Add(tc.resetOffset)
			retry := reset.Add(30 * time.Second)
			e := newEngine()
			e.dir = t.TempDir()
			e.wakeupConfig = WakeupConfig{Enabled: true, Mode: "both", Cron: "0 6,11,16,21 * * *", TimezoneOffsetMin: -480,
				Providers: map[string]bool{provider: true}, Models: map[string]string{wakeupGeminiKey: "gemini-3.5-flash-lite"}}
			five, week := &QuotaWindow{Used: 100, ResetAt: reset.Format(time.RFC3339)}, &QuotaWindow{Used: 55}
			q := AccountQuota{AuthID: "acct", Provider: provider, FetchedAt: start.Format(time.RFC3339), Five: five, Week: week}
			key := "acct"
			if provider == "antigravity" {
				key = "acct#gemini"
				q.Groups = []QuotaGroup{{Key: "gemini", Five: five, Week: week}}
			}
			e.quotas["acct"] = q
			e.wakeupLastFail["acct"] = start.Add(-time.Minute)
			rpc := func(method string, _ any) (any, error) {
				if method != "host.auth.list" {
					t.Errorf("unexpected RPC: %s", method)
					return nil, errors.New("unexpected RPC")
				}
				return map[string]any{"files": []any{map[string]any{"id": "acct", "auth_index": "i1", "provider": provider}}}, nil
			}
			sends, reads := 0, 0
			exec := func(_ AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
				sends++
				return model, ""
			}
			refresh := func(AuthEntry) bool { reads++; return false }
			e.checkWakeupScheduleWith(start, rpc, exec, refresh)
			waitWakeupIdle(t, e)
			res, rec := e.wakeupResults["acct"], e.wakeupLastSent[key]
			if sends != 0 || reads != 0 || res.Status != "exhausted" || res.Reason != wakeupReasonExhausted || res.RetryAt != retry.UTC().Format(time.RFC3339Nano) {
				t.Fatalf("slot must queue, not send or read quota: sends=%d reads=%d result=%+v", sends, reads, res)
			}
			if rec.RetrySource != "schedule" || rec.RetrySlot != cronSlotKey(start, -480) || rec.Misses != 1 || !rec.RetryAt.Equal(retry) || !rec.RetryUntil.Equal(rec.At.Add(30*time.Minute)) {
				t.Fatalf("incorrect scheduled follow-up: %+v", rec)
			}
			if _, failed := e.wakeupLastFail["acct"]; failed {
				t.Fatal("queued reset must clear rather than set the 15-minute backoff")
			}
			e.checkWakeupScheduleWith(retry.Add(-time.Second), rpc, exec, refresh)
			waitWakeupIdle(t, e)
			if sends != 0 || reads != 0 {
				t.Fatalf("retry fired before reset margin: sends=%d reads=%d", sends, reads)
			}
			e.checkWakeupScheduleWith(retry, rpc, exec, refresh)
			waitWakeupIdle(t, e)
			if res := e.wakeupResults["acct"]; sends != 1 || reads != 1 || res.Status != "sent" || res.RetryAt != "" {
				t.Fatalf("expected one scheduled follow-up during Cron rest: sends=%d reads=%d result=%+v", sends, reads, res)
			}
			followed := e.wakeupLastSent[key]
			if followed.RetrySource != rec.RetrySource || followed.RetrySlot != rec.RetrySlot || !followed.RetryUntil.Equal(rec.RetryUntil) || followed.Misses != rec.Misses {
				t.Fatalf("follow-up did not retain the scheduled chain: %+v", followed)
			}
		})
	}
}

func TestWakeupScheduleOnlyDefersNearValidFiveHourReset(t *testing.T) {
	at := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, trigger, reset string
		used, week           float64
		wantRetry            bool
	}{
		{"at-5m", "schedule", at.Add(5 * time.Minute).Format(time.RFC3339Nano), 100, 99, true},
		{"beyond-5m", "schedule", at.Add(5*time.Minute + time.Nanosecond).Format(time.RFC3339Nano), 100, 99, false},
		{"at-reset", "schedule", at.Format(time.RFC3339Nano), 100, 99, true},
		{"inside-reset-margin", "schedule", at.Add(-wakeupResetMargin + time.Nanosecond).Format(time.RFC3339Nano), 100, 99, true},
		{"at-end-of-reset-margin", "schedule", at.Add(-wakeupResetMargin).Format(time.RFC3339Nano), 100, 99, false},
		{"reset-unknown", "schedule", "invalid", 100, 99, false},
		{"weekly-exhausted", "schedule", at.Add(time.Minute).Format(time.RFC3339Nano), 100, 100, false},
		{"five-not-exhausted", "schedule", at.Add(time.Minute).Format(time.RFC3339Nano), 99, 0, false},
		{"not-a-slot", "reset", at.Add(time.Minute).Format(time.RFC3339Nano), 100, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine()
			retry := e.deferScheduledWakeup("acc", tc.trigger, "slot", &QuotaWindow{Used: tc.used, ResetAt: tc.reset}, &QuotaWindow{Used: tc.week}, at)
			if _, queued := e.wakeupLastSent["acc"]; queued != tc.wantRetry || (retry != "") != tc.wantRetry {
				t.Fatalf("queued=%v retry=%q, want queue=%v", queued, retry, tc.wantRetry)
			}
		})
	}
}

func TestWakeupBatchUsesIndividualSendTimes(t *testing.T) {
	at := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	e := newEngine()
	e.dir = t.TempDir()
	cfg := defaultWakeupConfig()
	const lag = 30 * time.Millisecond
	files := []any{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("c%d", i)
		files = append(files, map[string]any{"id": id, "auth_index": id, "provider": "codex"})
	}
	e.quotas["c4"] = AccountQuota{AuthID: "c4", Provider: "codex",
		Five: &QuotaWindow{Used: 100, ResetAt: at.Add(-wakeupResetMargin + lag/2).Format(time.RFC3339Nano)}}
	rpc := func(string, any) (any, error) { return map[string]any{"files": files}, nil }
	exec := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		if entry.ID != "c4" {
			time.Sleep(lag)
		}
		return model, ""
	}
	refresh := func(entry AuthEntry) bool {
		if entry.ID != "c4" {
			return false
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		q := e.quotas[entry.ID]
		q.FetchedAt = time.Now().UTC().Format(time.RFC3339Nano)
		q.Five = &QuotaWindow{Used: 1, ResetAt: at.Add(wakeupOpenedMinLeft).Format(time.RFC3339Nano)}
		e.quotas[entry.ID] = q
		return true
	}
	e.runWakeupBatchLockedClaimed(cfg, "schedule", "slot", at, rpc, exec, refresh)
	last, res := e.wakeupLastSent["c4"], e.wakeupResults["c4"]
	if last.At.Before(at.Add(lag)) || last.At.Sub(at) >= time.Minute || res.At != last.At.Format(time.RFC3339Nano) {
		t.Fatalf("late worker didn't record its own synthetic send time: rec=%+v result=%+v", last, res)
	}
	for id, first := range e.wakeupLastSent {
		if id != "c4" && last.At.Sub(first.At) >= lag {
			if res.Status != "sent" || res.RetryAt != "" || !res.WindowExisting || res.WindowResetAt == "" {
				t.Fatalf("reset margin should have elapsed before late send; post-send read joins an old window: %+v", res)
			}
			return
		}
	}
	t.Fatalf("the queued fifth account did not record a later send than the first wave: %+v", e.wakeupLastSent)
}

func TestWakeupGeminiUsesSameConfirmationForAccountAndGroup(t *testing.T) {
	at := time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                       string
		used                       float64
		reset                      string
		confirmed, existing, retry bool
	}{
		{name: "zero-usage-fresh-reset", reset: at.Add(5 * time.Hour).Format(time.RFC3339), retry: true},
		{name: "zero-usage-existing-reset", reset: at.Add(time.Hour).Format(time.RFC3339), retry: true},
		{name: "negative-usage", used: -1, reset: at.Add(time.Hour).Format(time.RFC3339), retry: true},
		{name: "used-invalid-reset", used: 1, reset: "invalid"},
		{name: "new-window", used: 1, reset: at.Add(5 * time.Hour).Format(time.RFC3339), confirmed: true},
		{name: "existing-window", used: 1, reset: at.Add(time.Hour).Format(time.RFC3339), confirmed: true, existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine()
			entry := AuthEntry{ID: "ag", Provider: "antigravity"}
			e.wakeupLastSent["ag#gemini"] = newWakeupSentRecord(wakeupSentRecord{}, at, "schedule", "slot")
			staleReset := at.Add(4 * time.Hour).Format(time.RFC3339)
			before := WakeupAccountResult{AuthID: "ag", Provider: "antigravity", Status: "sent", WindowResetAt: staleReset, WindowExisting: true,
				Groups: []WakeupGroupResult{{Group: "gemini", Status: "sent", WindowResetAt: staleReset}}}
			e.wakeupResults["ag"] = before
			e.refreshAfterWakeup(entry, func(entry AuthEntry) bool {
				e.mu.Lock()
				defer e.mu.Unlock()
				e.quotas[entry.ID] = AccountQuota{AuthID: entry.ID, Provider: entry.Provider, FetchedAt: time.Now().UTC().Format(time.RFC3339Nano),
					Groups: []QuotaGroup{{Key: "gemini", Five: &QuotaWindow{Used: tc.used, ResetAt: tc.reset}}}}
				return true
			}, func() time.Time { return at.Add(time.Second) })
			res := e.wakeupResults["ag"]
			wantReset := ""
			if tc.confirmed {
				wantReset = tc.reset
			}
			if res.WindowResetAt != wantReset || res.Groups[0].WindowResetAt != wantReset || res.WindowExisting != tc.existing || (res.RetryAt != "") != tc.retry {
				t.Fatalf("invalid Gemini confirmation: %+v", res)
			}
			if before.Groups[0].WindowResetAt != staleReset {
				t.Fatal("verification mutated the previously published group slice")
			}
			if tc.retry && res.RetryAt != at.Add(time.Second+2*time.Minute).Format(time.RFC3339Nano) {
				t.Fatalf("unconfirmed reset must not control the retry time: %+v", res)
			}
		})
	}
}
