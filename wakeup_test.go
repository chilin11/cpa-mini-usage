package main

import (
	"encoding/json"
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

	restored := newEngine()
	restored.dir = dir
	restored.load()
	got := restored.snapshot().Wakeup.Config
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
			{Key: "claude-gpt", Label: "Claude / GPT Models", Five: &QuotaWindow{Used: 15, ResetAt: now.Add(2 * time.Hour).Format(time.RFC3339)}},
		},
	}

	var requests []map[string]any
	var mu sync.Mutex
	rpc := func(method string, arg any) (any, error) {
		switch method {
		case "host.auth.list":
			return map[string]any{"files": []any{
				map[string]any{"id": "claude-1", "auth_index": "i1", "provider": "claude", "email": "c@example.com"},
				map[string]any{"id": "ag-1", "auth_index": "i2", "provider": "antigravity", "email": "g@example.com"},
				map[string]any{"id": "dis-1", "auth_index": "i3", "provider": "codex", "email": "d@example.com", "disabled": true},
			}}, nil
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
	if snap.Results["claude-1"].Status != "sent" || snap.Results["claude-1"].Model != "claude-3-5-haiku-20241022" {
		t.Fatalf("claude fallback unexpected: %+v", snap.Results["claude-1"])
	}

	// Persisted results survive engine restart while sanitizing untrusted status/reason fields.
	restored := newEngine()
	restored.dir = e.dir
	restored.load()
	if got := restored.snapshot().Wakeup.Results["claude-1"]; got.Status != "sent" || got.Model != "claude-3-5-haiku-20241022" {
		t.Fatalf("restored wakeup result mismatch: %+v", got)
	}
	if snap.Results["dis-1"].Status != "skipped" || snap.Results["dis-1"].Reason != wakeupReasonDisabled {
		t.Fatalf("disabled account should be skipped: %+v", snap.Results["dis-1"])
	}
	agRes := snap.Results["ag-1"]
	if agRes.Status != "sent" || len(agRes.Groups) != 2 {
		t.Fatalf("antigravity result unexpected: %+v", agRes)
	}
	if agRes.Groups[0].Group != "gemini" || agRes.Groups[0].Status != "sent" || agRes.Groups[1].Group != "claude-gpt" || agRes.Groups[1].Status != "skipped" {
		t.Fatalf("antigravity per-group wakeup unexpected: %+v", agRes.Groups)
	}

	// Verify pinned wire contract sent to host.model.execute.
	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 3 {
		t.Fatalf("expected at least 3 model execute calls, got %d", len(requests))
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
		if body["max_tokens"] != float64(wakeupDefaultMaxTokens) {
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

	// Rate limit must stop immediately without trying other candidates.
	e.wakeupConfig.Models = map[string]string{}
	attempts = 0
	exec429 := func(entry AuthEntry, model string, _ string, _ func(string, any) (any, error)) (string, string) {
		attempts++
		return model, wakeupErrRateLimited
	}
	res = e.wakeupEntry(AuthEntry{ID: "c1", Provider: "claude"}, e.wakeupConfig, true, time.Now().UTC(), nil, exec429)
	if attempts != 1 || res.Reason != wakeupErrRateLimited {
		t.Fatalf("429 must stop immediately: attempts=%d res=%+v", attempts, res)
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
	if code, _ := call("/wakeup/run", `{"auth_id":"dis","force":true}`); code != 409 {
		t.Fatalf("disabled account run status=%d, want 409", code)
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
