package main

import (
	"encoding/json"
	"errors"
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
		SkipActiveWindow:  true,
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
		SkipActiveWindow:  true,
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
		SkipActiveWindow:  true,
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
		SkipActiveWindow:  true,
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
	res := e.wakeupEntry(AuthEntry{ID: "c1", Provider: "claude"}, e.wakeupConfig, true, time.Now().UTC(), nil, exec)
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
	res = e.wakeupEntry(AuthEntry{ID: "c1", Provider: "claude"}, e.wakeupConfig, true, time.Now().UTC(), nil, exec429)
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
		SkipActiveWindow:  true,
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
		SkipActiveWindow:  true,
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
	res := e.wakeupEntry(AuthEntry{ID: "codex-full", Provider: "codex"}, e.wakeupConfig, true, now, nil, execShouldNotRun)
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
	res = e.wakeupEntry(entries[0], e.wakeupConfig, true, now, nil, execAuthNotFound)
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
