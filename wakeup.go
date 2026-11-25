package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	wakeupSlotGrace         = 10 * time.Minute
	wakeupCooldown          = 4*time.Hour + 50*time.Minute
	wakeupFailedBackoff     = 15 * time.Minute
	wakeupCheckInterval     = 30 * time.Second
	wakeupMaxTimes          = 12
	wakeupMaxExcluded       = 2000
	wakeupMaxMessageRunes   = 128
	wakeupMaxModelBytes     = 128
	wakeupMaxFiredSlots     = 48
	wakeupMaxSentRecords    = 2048
	wakeupDefaultMessage    = "hi"
	wakeupDefaultMaxTokens  = 16
	wakeupConfigFileName    = "wakeup.json"
	wakeupReasonWindow      = "window_active"
	wakeupReasonDisabled    = "disabled"
	wakeupReasonExcluded    = "excluded"
	wakeupReasonProviderOff = "provider_disabled"
	wakeupReasonExhausted   = "quota_exhausted"
	wakeupErrHostUnavail    = "host_list_unavailable"
	wakeupErrHostUnsupp     = "host_unsupported"
	wakeupErrModelUnavail   = "model_unavailable"
	wakeupErrRateLimited    = "rate_limited"
	wakeupErrAuthFailed     = "auth_failed"
	wakeupErrRequestFailed  = "request_failed"
	wakeupErrAccountDis     = "account_disabled"
	wakeupErrAccountMiss    = "account_not_found"
)

var (
	wakeupTimePattern  = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)
	wakeupModelPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@()-]{1,128}$`)
)

var wakeupAllowedModelKeys = map[string]bool{
	"claude":                 true,
	"codex":                  true,
	"antigravity":            true,
	"antigravity:gemini":     true,
	"antigravity:claude-gpt": true,
}

type WakeupConfig struct {
	Enabled           bool              `json:"enabled"`
	Mode              string            `json:"mode"` // "daily" | "reset" | "both"
	Cron              string            `json:"cron,omitempty"`
	Times             []string          `json:"times,omitempty"`
	TimezoneOffsetMin int               `json:"timezone_offset_min"`
	SkipActiveWindow  bool              `json:"skip_active_window"`
	Providers         map[string]bool   `json:"providers"`
	ExcludedAuthIDs   []string          `json:"excluded_auth_ids,omitempty"`
	Models            map[string]string `json:"models,omitempty"`
	Message           string            `json:"message,omitempty"`
}

type WakeupGroupResult struct {
	Group  string `json:"group"`
	Status string `json:"status"` // "sent" | "skipped" | "failed"
	Reason string `json:"reason,omitempty"`
	Model  string `json:"model,omitempty"`
	At     string `json:"at"`
}

type WakeupAccountResult struct {
	AuthID   string              `json:"auth_id"`
	Provider string              `json:"provider"`
	Name     string              `json:"name,omitempty"`
	Status   string              `json:"status"` // "sent" | "skipped" | "failed"
	Reason   string              `json:"reason,omitempty"`
	Model    string              `json:"model,omitempty"`
	At       string              `json:"at"`
	Groups   []WakeupGroupResult `json:"groups,omitempty"`
}

type WakeupState struct {
	Config            WakeupConfig                   `json:"config"`
	ConfigPath        string                         `json:"config_path,omitempty"`
	Persisted         bool                           `json:"persisted"`
	Running           bool                           `json:"running"`
	RunningAuthIDs    []string                       `json:"running_auth_ids,omitempty"`
	LastRunAt         string                         `json:"last_run_at,omitempty"`
	LastRunTrigger    string                         `json:"last_run_trigger,omitempty"`
	LastRunFinished   string                         `json:"last_run_finished_at,omitempty"`
	NextScheduledAt   string                         `json:"next_scheduled_at,omitempty"`
	NextScheduledRuns []string                       `json:"next_scheduled_runs,omitempty"`
	Results           map[string]WakeupAccountResult `json:"results,omitempty"`
	LastError         string                         `json:"last_error,omitempty"`
}

type wakeupSentRecord struct {
	At      time.Time `json:"at"`
	ResetAt time.Time `json:"reset_at,omitempty"`
}

type persistedWakeup struct {
	Config          WakeupConfig                   `json:"config"`
	FiredSlots      map[string]string              `json:"fired_slots,omitempty"`
	LastSent        map[string]wakeupSentRecord    `json:"last_sent,omitempty"`
	LastRunAt       string                         `json:"last_run_at,omitempty"`
	LastRunTrigger  string                         `json:"last_run_trigger,omitempty"`
	LastRunFinished string                         `json:"last_run_finished_at,omitempty"`
	Results         map[string]WakeupAccountResult `json:"results,omitempty"`
}

type wakeupRunAccepted struct {
	LastRunFinished string
	AccountResultAt string
}

func defaultWakeupConfig() WakeupConfig {
	_, offsetSec := time.Now().Zone()
	offsetMin := offsetSec / 60
	if offsetMin < -720 || offsetMin > 840 {
		offsetMin = 0
	}
	return WakeupConfig{
		Enabled:           false,
		Mode:              "daily",
		Cron:              wakeupDefaultCron,
		Times:             []string{"06:00", "11:00", "16:00", "21:00"},
		TimezoneOffsetMin: offsetMin,
		SkipActiveWindow:  true,
		Providers: map[string]bool{
			"claude":      true,
			"codex":       true,
			"antigravity": true,
		},
		Models:  map[string]string{},
		Message: wakeupDefaultMessage,
	}
}

func cloneWakeupConfig(cfg WakeupConfig) WakeupConfig {
	out := cfg
	if len(cfg.Times) > 0 {
		out.Times = append([]string(nil), cfg.Times...)
	} else {
		out.Times = []string{}
	}
	out.Providers = map[string]bool{
		"claude":      cfg.Providers["claude"],
		"codex":       cfg.Providers["codex"],
		"antigravity": cfg.Providers["antigravity"],
	}
	if len(cfg.ExcludedAuthIDs) > 0 {
		out.ExcludedAuthIDs = append([]string(nil), cfg.ExcludedAuthIDs...)
	} else {
		out.ExcludedAuthIDs = nil
	}
	out.Models = make(map[string]string, len(cfg.Models))
	for k, v := range cfg.Models {
		if v != "" {
			out.Models[k] = v
		}
	}
	return out
}

func validateWakeupConfig(raw WakeupConfig) (WakeupConfig, error) {
	mode := strings.ToLower(strings.TrimSpace(raw.Mode))
	if mode == "" {
		mode = "daily"
	}
	switch mode {
	case "daily", "reset", "both":
	default:
		return WakeupConfig{}, errors.New("invalid wakeup mode")
	}
	if raw.TimezoneOffsetMin < -720 || raw.TimezoneOffsetMin > 840 {
		return WakeupConfig{}, errors.New("invalid timezone offset")
	}
	cronExpr := strings.Join(strings.Fields(raw.Cron), " ")
	if cronExpr != "" {
		if _, err := parseCron(cronExpr); err != nil {
			return WakeupConfig{}, err
		}
	}
	if len(raw.Times) > wakeupMaxTimes {
		return WakeupConfig{}, errors.New("too many wakeup times")
	}
	seenTimes := make(map[string]bool, len(raw.Times))
	times := make([]string, 0, len(raw.Times))
	for _, value := range raw.Times {
		trimmed := strings.TrimSpace(value)
		if !wakeupTimePattern.MatchString(trimmed) {
			return WakeupConfig{}, errors.New("invalid wakeup time")
		}
		if !seenTimes[trimmed] {
			seenTimes[trimmed] = true
			times = append(times, trimmed)
		}
	}
	sort.Strings(times)
	if (mode == "daily" || mode == "both") && cronExpr == "" && len(times) == 0 {
		return WakeupConfig{}, errors.New("cron expression or wakeup times required")
	}
	providers := map[string]bool{
		"claude":      true,
		"codex":       true,
		"antigravity": true,
	}
	if raw.Providers != nil {
		for k := range raw.Providers {
			if !quotaProvider(k) {
				return WakeupConfig{}, errors.New("invalid wakeup provider")
			}
		}
		for _, p := range []string{"claude", "codex", "antigravity"} {
			providers[p] = raw.Providers[p]
		}
	}
	if raw.Enabled && !providers["claude"] && !providers["codex"] && !providers["antigravity"] {
		return WakeupConfig{}, errors.New("at least one provider required when wakeup is enabled")
	}
	if len(raw.ExcludedAuthIDs) > wakeupMaxExcluded {
		return WakeupConfig{}, errors.New("too many excluded accounts")
	}
	seenExcluded := make(map[string]bool, len(raw.ExcludedAuthIDs))
	var excluded []string
	for _, id := range raw.ExcludedAuthIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" || len(trimmed) > 1024 {
			return WakeupConfig{}, errors.New("invalid excluded auth id")
		}
		if !seenExcluded[trimmed] {
			seenExcluded[trimmed] = true
			excluded = append(excluded, trimmed)
		}
	}
	sort.Strings(excluded)

	models := make(map[string]string)
	for k, v := range raw.Models {
		key := strings.ToLower(strings.TrimSpace(k))
		if !wakeupAllowedModelKeys[key] {
			return WakeupConfig{}, errors.New("invalid wakeup model key")
		}
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		if len(val) > wakeupMaxModelBytes || !wakeupModelPattern.MatchString(val) {
			return WakeupConfig{}, errors.New("invalid wakeup model name")
		}
		models[key] = val
	}

	msg := strings.TrimSpace(raw.Message)
	if msg == "" {
		msg = wakeupDefaultMessage
	}
	if !utf8.ValidString(msg) || utf8.RuneCountInString(msg) > wakeupMaxMessageRunes {
		return WakeupConfig{}, errors.New("invalid wakeup message")
	}
	for _, r := range msg {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return WakeupConfig{}, errors.New("invalid wakeup message control character")
		}
	}

	return WakeupConfig{
		Enabled:           raw.Enabled,
		Mode:              mode,
		Cron:              cronExpr,
		Times:             times,
		TimezoneOffsetMin: raw.TimezoneOffsetMin,
		SkipActiveWindow:  raw.SkipActiveWindow,
		Providers:         providers,
		ExcludedAuthIDs:   excluded,
		Models:            models,
		Message:           msg,
	}, nil
}

func (e *Engine) loadWakeupPersistence() {
	body, err := os.ReadFile(filepath.Join(e.dir, wakeupConfigFileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			e.blockStorageWrites("Wakeup settings could not be read")
		}
		return
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		e.blockStorageWrites("Wakeup settings could not be read")
		return
	}
	var saved persistedWakeup
	if json.Unmarshal(body, &saved) != nil {
		e.blockStorageWrites("Wakeup settings could not be read")
		return
	}
	cfg, err := validateWakeupConfig(saved.Config)
	if err != nil {
		e.blockStorageWrites("Wakeup settings could not be read")
		return
	}
	e.wakeupConfig = cfg
	e.wakeupPersisted = true
	for k, ts := range saved.FiredSlots {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.wakeupFiredSlots[k] = parsed
		}
	}
	for k, rec := range saved.LastSent {
		if !rec.At.IsZero() && len(e.wakeupLastSent) < wakeupMaxSentRecords {
			e.wakeupLastSent[k] = rec
		}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, saved.LastRunAt); err == nil {
		e.wakeupLastRunAt = parsed.UTC()
	}
	if parsed, err := time.Parse(time.RFC3339Nano, saved.LastRunFinished); err == nil {
		e.wakeupLastRunFinished = parsed.UTC()
	}
	switch saved.LastRunTrigger {
	case "schedule", "reset", "manual", "test", "account":
		e.wakeupLastRunTrigger = saved.LastRunTrigger
	}
	for id, res := range saved.Results {
		if id == "" || len(e.wakeupResults) >= wakeupMaxSentRecords {
			continue
		}
		if res.Status != "sent" && res.Status != "skipped" && res.Status != "exhausted" && res.Status != "failed" {
			continue
		}
		res.AuthID = id
		res.Reason = sanitizeWakeupReason(res.Reason)
		if res.Model != "" && !wakeupModelPattern.MatchString(res.Model) {
			res.Model = ""
		}
		if len(res.Groups) > 0 {
			cleanGroups := make([]WakeupGroupResult, 0, len(res.Groups))
			for _, g := range res.Groups {
				if g.Status != "sent" && g.Status != "skipped" && g.Status != "exhausted" && g.Status != "failed" {
					continue
				}
				g.Reason = sanitizeWakeupReason(g.Reason)
				if g.Model != "" && !wakeupModelPattern.MatchString(g.Model) {
					g.Model = ""
				}
				cleanGroups = append(cleanGroups, g)
			}
			res.Groups = cleanGroups
		}
		e.wakeupResults[id] = res
	}
}

func sanitizeWakeupReason(reason string) string {
	switch reason {
	case "", wakeupReasonWindow, wakeupReasonDisabled, wakeupReasonExcluded, wakeupReasonProviderOff, wakeupReasonExhausted,
		wakeupErrHostUnavail, wakeupErrHostUnsupp, wakeupErrModelUnavail, wakeupErrRateLimited,
		wakeupErrAuthFailed, wakeupErrRequestFailed, wakeupErrAccountDis, wakeupErrAccountMiss:
		return reason
	default:
		return wakeupErrRequestFailed
	}
}

func (e *Engine) writeWakeupLocked(now time.Time) error {
	if e.storageReadFailed {
		return errors.New("storage is read-only after an incomplete load")
	}
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	e.pruneWakeupTrackingLocked(now)
	fired := make(map[string]string, len(e.wakeupFiredSlots))
	for k, t := range e.wakeupFiredSlots {
		fired[k] = t.UTC().Format(time.RFC3339Nano)
	}
	lastSent := make(map[string]wakeupSentRecord, len(e.wakeupLastSent))
	for k, rec := range e.wakeupLastSent {
		lastSent[k] = rec
	}
	persisted := persistedWakeup{
		Config:     e.wakeupConfig,
		FiredSlots: fired,
		LastSent:   lastSent,
	}
	if e.persistence.Enabled {
		if !e.wakeupLastRunAt.IsZero() {
			persisted.LastRunAt = e.wakeupLastRunAt.UTC().Format(time.RFC3339Nano)
		}
		if !e.wakeupLastRunFinished.IsZero() {
			persisted.LastRunFinished = e.wakeupLastRunFinished.UTC().Format(time.RFC3339Nano)
		}
		persisted.LastRunTrigger = e.wakeupLastRunTrigger
		if len(e.wakeupResults) > 0 {
			resCopy := make(map[string]WakeupAccountResult, len(e.wakeupResults))
			for k, v := range e.wakeupResults {
				resCopy[k] = v
			}
			persisted.Results = resCopy
		}
	}
	body, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(e.dir, wakeupConfigFileName), body); err != nil {
		return err
	}
	e.wakeupPersisted = true
	return nil
}

func (e *Engine) persistWakeupTrackingLocked(now time.Time) {
	if e.storageReadFailed {
		return
	}
	if !e.persistence.Enabled && !e.wakeupConfig.Enabled {
		return
	}
	_ = e.writeWakeupLocked(now)
}

func (e *Engine) pruneWakeupTrackingLocked(now time.Time) {
	slotCutoff := now.Add(-48 * time.Hour)
	for k, t := range e.wakeupFiredSlots {
		if t.Before(slotCutoff) {
			delete(e.wakeupFiredSlots, k)
		}
	}
	if len(e.wakeupFiredSlots) > wakeupMaxFiredSlots {
		type slotItem struct {
			k string
			t time.Time
		}
		items := make([]slotItem, 0, len(e.wakeupFiredSlots))
		for k, t := range e.wakeupFiredSlots {
			items = append(items, slotItem{k, t})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
		next := make(map[string]time.Time, wakeupMaxFiredSlots)
		for i := 0; i < wakeupMaxFiredSlots && i < len(items); i++ {
			next[items[i].k] = items[i].t
		}
		e.wakeupFiredSlots = next
	}
	sentCutoff := now.Add(-14 * 24 * time.Hour)
	for k, rec := range e.wakeupLastSent {
		if rec.At.Before(sentCutoff) {
			delete(e.wakeupLastSent, k)
		}
	}
	if len(e.wakeupLastSent) > wakeupMaxSentRecords {
		type sentItem struct {
			k string
			t time.Time
		}
		items := make([]sentItem, 0, len(e.wakeupLastSent))
		for k, rec := range e.wakeupLastSent {
			items = append(items, sentItem{k, rec.At})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
		next := make(map[string]wakeupSentRecord, wakeupMaxSentRecords)
		for i := 0; i < wakeupMaxSentRecords && i < len(items); i++ {
			next[items[i].k] = e.wakeupLastSent[items[i].k]
		}
		e.wakeupLastSent = next
	}
}

func wakeupSlotKey(day string, hhmm string, offsetMin int) string {
	return fmt.Sprintf("%s@%s@%+d", day, hhmm, offsetMin)
}

func parseHHMM(hhmm string) (int, int, bool) {
	if !wakeupTimePattern.MatchString(hhmm) {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(hhmm[:2])
	m, err2 := strconv.Atoi(hhmm[3:5])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return h, m, true
}

func wakeupZone(offsetMin int) *time.Location {
	return time.FixedZone("wakeup", offsetMin*60)
}

// Mark already-past recent slots as fired when configuration changes so
// enabling a schedule does not immediately fire an already-elapsed slot.
func (e *Engine) markPastSlotsFiredLocked(cfg WakeupConfig, now time.Time) {
	if cfg.Mode != "daily" && cfg.Mode != "both" {
		return
	}
	if strings.TrimSpace(cfg.Cron) != "" {
		if sched, err := parseCron(cfg.Cron); err == nil {
			sched.markRecentFired(now, wakeupSlotGrace, e.wakeupFiredSlots, cfg.TimezoneOffsetMin)
		}
		return
	}
	loc := wakeupZone(cfg.TimezoneOffsetMin)
	localNow := now.In(loc)
	day := localNow.Format("2006-01-02")
	for _, hhmm := range cfg.Times {
		h, m, ok := parseHHMM(hhmm)
		if !ok {
			continue
		}
		slot := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), h, m, 0, 0, loc)
		if !localNow.Before(slot) {
			e.wakeupFiredSlots[wakeupSlotKey(day, hhmm, cfg.TimezoneOffsetMin)] = now.UTC()
		}
	}
}

func nextWakeupRuns(cfg WakeupConfig, fired map[string]time.Time, now time.Time, count int) []time.Time {
	if !cfg.Enabled || (cfg.Mode != "daily" && cfg.Mode != "both") || count <= 0 {
		return nil
	}
	if strings.TrimSpace(cfg.Cron) != "" {
		sched, err := parseCron(cfg.Cron)
		if err != nil {
			return nil
		}
		return sched.nextRuns(now, count, fired, cfg.TimezoneOffsetMin)
	}
	if len(cfg.Times) == 0 {
		return nil
	}
	loc := wakeupZone(cfg.TimezoneOffsetMin)
	localNow := now.In(loc)
	out := make([]time.Time, 0, count)
	for dayOffset := 0; dayOffset <= 7 && len(out) < count; dayOffset++ {
		base := time.Date(localNow.Year(), localNow.Month(), localNow.Day()+dayOffset, 0, 0, 0, 0, loc)
		day := base.Format("2006-01-02")
		for _, hhmm := range cfg.Times {
			h, m, ok := parseHHMM(hhmm)
			if !ok {
				continue
			}
			slot := time.Date(base.Year(), base.Month(), base.Day(), h, m, 0, 0, loc)
			key := wakeupSlotKey(day, hhmm, cfg.TimezoneOffsetMin)
			if _, already := fired[key]; already {
				continue
			}
			if slot.After(localNow) || (!localNow.Before(slot) && localNow.Sub(slot) < wakeupSlotGrace) {
				out = append(out, slot.UTC())
				if len(out) >= count {
					break
				}
			}
		}
	}
	return out
}

func nextWakeupSlot(cfg WakeupConfig, fired map[string]time.Time, now time.Time) time.Time {
	runs := nextWakeupRuns(cfg, fired, now, 1)
	if len(runs) == 0 {
		return time.Time{}
	}
	return runs[0]
}

func (e *Engine) setWakeupConfig(raw WakeupConfig) (WakeupState, int, string) {
	cfg, err := validateWakeupConfig(raw)
	if err != nil {
		return WakeupState{}, 400, "invalid_wakeup_config"
	}
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.storageReadFailed {
		return e.wakeupStateLocked(now), 500, "storage_write_failed"
	}
	prev := e.wakeupConfig
	prevSlots := make(map[string]time.Time, len(e.wakeupFiredSlots))
	for k, v := range e.wakeupFiredSlots {
		prevSlots[k] = v
	}
	e.wakeupConfig = cfg
	e.markPastSlotsFiredLocked(cfg, now)
	if err := e.writeWakeupLocked(now); err != nil {
		e.wakeupConfig = prev
		e.wakeupFiredSlots = prevSlots
		e.storageError = "Wakeup settings could not be persisted"
		return e.wakeupStateLocked(now), 500, "storage_write_failed"
	}
	e.storageError = ""
	return e.wakeupStateLocked(now), 200, ""
}

func (e *Engine) setWakeupExcluded(authID string, excluded bool) (WakeupState, int, string) {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(authID) > 1024 {
		return WakeupState{}, 400, "invalid_auth_id"
	}
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.storageReadFailed {
		return e.wakeupStateLocked(now), 500, "storage_write_failed"
	}
	if _, known := e.quotas[authID]; !known {
		return WakeupState{}, 404, "account_not_found"
	}
	next := cloneWakeupConfig(e.wakeupConfig)
	set := make(map[string]bool, len(next.ExcludedAuthIDs)+1)
	for _, id := range next.ExcludedAuthIDs {
		set[id] = true
	}
	if excluded {
		set[authID] = true
	} else {
		delete(set, authID)
	}
	if len(set) > wakeupMaxExcluded {
		return WakeupState{}, 400, "invalid_wakeup_config"
	}
	list := make([]string, 0, len(set))
	for id := range set {
		list = append(list, id)
	}
	sort.Strings(list)
	next.ExcludedAuthIDs = list

	prev := e.wakeupConfig
	e.wakeupConfig = next
	if err := e.writeWakeupLocked(now); err != nil {
		e.wakeupConfig = prev
		e.storageError = "Wakeup settings could not be persisted"
		return e.wakeupStateLocked(now), 500, "storage_write_failed"
	}
	e.storageError = ""
	return e.wakeupStateLocked(now), 200, ""
}

func (e *Engine) wakeupStateLocked(now time.Time) WakeupState {
	state := WakeupState{
		Config:         cloneWakeupConfig(e.wakeupConfig),
		ConfigPath:     filepath.Join(e.dir, wakeupConfigFileName),
		Persisted:      e.wakeupPersisted,
		Running:        e.wakeupRunning,
		LastRunTrigger: e.wakeupLastRunTrigger,
		LastError:      e.wakeupLastError,
	}
	if len(e.wakeupRunningAuths) > 0 {
		ids := make([]string, 0, len(e.wakeupRunningAuths))
		for id := range e.wakeupRunningAuths {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		state.RunningAuthIDs = ids
	}
	if !e.wakeupLastRunAt.IsZero() {
		state.LastRunAt = e.wakeupLastRunAt.UTC().Format(time.RFC3339Nano)
	}
	if !e.wakeupLastRunFinished.IsZero() {
		state.LastRunFinished = e.wakeupLastRunFinished.UTC().Format(time.RFC3339Nano)
	}
	if runs := nextWakeupRuns(e.wakeupConfig, e.wakeupFiredSlots, now, 5); len(runs) > 0 {
		state.NextScheduledAt = runs[0].Format(time.RFC3339)
		formatted := make([]string, len(runs))
		for i, run := range runs {
			formatted[i] = run.Format(time.RFC3339)
		}
		state.NextScheduledRuns = formatted
	}
	if len(e.wakeupResults) > 0 {
		results := make(map[string]WakeupAccountResult, len(e.wakeupResults))
		for id, res := range e.wakeupResults {
			copyRes := res
			if len(res.Groups) > 0 {
				copyRes.Groups = append([]WakeupGroupResult(nil), res.Groups...)
			}
			results[id] = copyRes
		}
		state.Results = results
	}
	return state
}

func wakeupSentKey(authID, group string) string {
	if group == "" {
		return authID
	}
	return authID + "#" + group
}

func parseWindowResetTime(resetAt string) (time.Time, bool) {
	if strings.TrimSpace(resetAt) == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, resetAt); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, resetAt); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// isFiveHourWindowActive reports whether a 5-hour quota window is already running
// (observed utilization > 0 before reset, or recently awakened within the same window).
func isFiveHourWindowActive(win *QuotaWindow, sent wakeupSentRecord, now time.Time) bool {
	if win != nil && win.Used > 0 {
		if resetAt, ok := parseWindowResetTime(win.ResetAt); ok {
			if resetAt.After(now) {
				return true
			}
		} else {
			return true
		}
	}
	if !sent.At.IsZero() {
		if !sent.ResetAt.IsZero() && !now.Before(sent.ResetAt) && now.Sub(sent.At) >= 5*time.Minute {
			return false
		}
		if now.Sub(sent.At) < wakeupCooldown {
			return true
		}
	}
	return false
}

func (e *Engine) updateWakeupObservedResetLocked(q AccountQuota) {
	if q.Provider == "antigravity" {
		for _, g := range q.Groups {
			if g.Five == nil {
				continue
			}
			key := wakeupSentKey(q.AuthID, g.Key)
			rec, ok := e.wakeupLastSent[key]
			if !ok {
				continue
			}
			if resetAt, valid := parseWindowResetTime(g.Five.ResetAt); valid && resetAt.After(rec.At) {
				rec.ResetAt = resetAt
				e.wakeupLastSent[key] = rec
			}
		}
		return
	}
	if q.Five == nil {
		return
	}
	key := wakeupSentKey(q.AuthID, "")
	rec, ok := e.wakeupLastSent[key]
	if !ok {
		return
	}
	if resetAt, valid := parseWindowResetTime(q.Five.ResetAt); valid && resetAt.After(rec.At) {
		rec.ResetAt = resetAt
		e.wakeupLastSent[key] = rec
	}
}

func isWakeupExcluded(cfg WakeupConfig, authID string) bool {
	for _, id := range cfg.ExcludedAuthIDs {
		if id == authID {
			return true
		}
	}
	return false
}

func (e *Engine) checkWakeupSchedule() {
	e.checkWakeupScheduleWith(time.Now().UTC(), hostRPC, executeWakeupModel, e.refreshAccountQuota)
}

func (e *Engine) checkWakeupScheduleWith(
	now time.Time,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
	refresh func(AuthEntry) bool,
) {
	e.mu.Lock()
	cfg := cloneWakeupConfig(e.wakeupConfig)
	if !cfg.Enabled || e.wakeupRunning {
		e.mu.Unlock()
		return
	}

	// 1. Check scheduled cron expression (or legacy fixed daily times).
	var dueSlotKey string
	if cfg.Mode == "daily" || cfg.Mode == "both" {
		if strings.TrimSpace(cfg.Cron) != "" {
			if sched, err := parseCron(cfg.Cron); err == nil {
				if key, _, ok := sched.dueSlot(now, wakeupSlotGrace, e.wakeupFiredSlots, cfg.TimezoneOffsetMin); ok {
					dueSlotKey = key
					e.wakeupFiredSlots[key] = now.UTC()
				}
			}
		} else {
			loc := wakeupZone(cfg.TimezoneOffsetMin)
			localNow := now.In(loc)
			day := localNow.Format("2006-01-02")
			for _, hhmm := range cfg.Times {
				h, m, ok := parseHHMM(hhmm)
				if !ok {
					continue
				}
				slot := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), h, m, 0, 0, loc)
				key := wakeupSlotKey(day, hhmm, cfg.TimezoneOffsetMin)
				if _, fired := e.wakeupFiredSlots[key]; fired {
					continue
				}
				if !localNow.Before(slot) && localNow.Sub(slot) < wakeupSlotGrace {
					dueSlotKey = key
					e.wakeupFiredSlots[key] = now.UTC()
					break
				}
			}
		}
	}
	if dueSlotKey != "" {
		e.wakeupRunning = true
		e.wakeupLastRunAt = now.UTC()
		e.wakeupLastRunTrigger = "schedule"
		e.wakeupLastError = ""
		e.persistWakeupTrackingLocked(now)
		e.mu.Unlock()
		go e.runWakeupBatchLockedClaimed(cfg, false, "schedule", dueSlotKey, now, rpc, exec, refresh)
		return
	}

	// 2. Check reset-triggered automatic wakeup for accounts whose observed 5h window is idle.
	var resetTargets []string
	if cfg.Mode == "reset" || cfg.Mode == "both" {
		for id, q := range e.quotas {
			if q.Disabled || q.FetchedAt == "" || !quotaProvider(q.Provider) || !cfg.Providers[q.Provider] || isWakeupExcluded(cfg, id) || e.wakeupRunningAuths[id] {
				continue
			}
			if failAt, ok := e.wakeupLastFail[id]; ok && now.Sub(failAt) < wakeupFailedBackoff {
				continue
			}
			if e.accountNeedsResetWakeupLocked(q, now) {
				resetTargets = append(resetTargets, id)
			}
		}
		sort.Strings(resetTargets)
	}
	if len(resetTargets) > 0 {
		e.wakeupRunning = true
		e.wakeupLastRunAt = now.UTC()
		e.wakeupLastRunTrigger = "reset"
		e.wakeupLastError = ""
		e.mu.Unlock()
		go e.runWakeupSubsetClaimed(cfg, resetTargets, false, "reset", "", now, rpc, exec, refresh)
		return
	}
	e.mu.Unlock()
}

func (e *Engine) accountNeedsResetWakeupLocked(q AccountQuota, now time.Time) bool {
	if q.Error != "" {
		return false
	}
	fetchedAt, ok := parseWindowResetTime(q.FetchedAt)
	if !ok || now.Sub(fetchedAt) > 2*quotaInterval {
		return false
	}
	if q.Provider == "antigravity" {
		if len(q.Groups) == 0 {
			return false
		}
		for _, g := range q.Groups {
			if g.Five == nil {
				continue
			}
			sent := e.wakeupLastSent[wakeupSentKey(q.AuthID, g.Key)]
			if !isFiveHourWindowActive(g.Five, sent, now) {
				return true
			}
		}
		return false
	}
	if q.Five == nil {
		return false
	}
	sent := e.wakeupLastSent[wakeupSentKey(q.AuthID, "")]
	return !isFiveHourWindowActive(q.Five, sent, now)
}

func (e *Engine) kickWakeupRun(authID string, force bool) (int, wakeupRunAccepted, string) {
	return e.kickWakeupRunWith(authID, force, time.Now().UTC(), hostRPC, executeWakeupModel, e.refreshAccountQuota)
}

func (e *Engine) kickWakeupRunWith(
	authID string,
	force bool,
	now time.Time,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
	refresh func(AuthEntry) bool,
) (int, wakeupRunAccepted, string) {
	authID = strings.TrimSpace(authID)
	if len(authID) > 1024 {
		return 400, wakeupRunAccepted{}, "invalid_auth_id"
	}
	e.mu.Lock()
	cfg := cloneWakeupConfig(e.wakeupConfig)
	accepted := wakeupRunAccepted{}
	if !e.wakeupLastRunFinished.IsZero() {
		accepted.LastRunFinished = e.wakeupLastRunFinished.UTC().Format(time.RFC3339Nano)
	}
	if authID != "" {
		q, known := e.quotas[authID]
		if !known {
			e.mu.Unlock()
			return 404, wakeupRunAccepted{}, wakeupErrAccountMiss
		}
		if !quotaProvider(q.Provider) {
			e.mu.Unlock()
			return 400, wakeupRunAccepted{}, "wakeup_unsupported"
		}
		if q.Disabled {
			e.mu.Unlock()
			return 409, wakeupRunAccepted{}, wakeupErrAccountDis
		}
		if e.wakeupRunning || e.wakeupRunningAuths[authID] {
			e.mu.Unlock()
			return 409, wakeupRunAccepted{}, "wakeup_busy"
		}
		accepted.AccountResultAt = e.wakeupResults[authID].At
		e.wakeupRunningAuths[authID] = true
		e.wakeupLastRunAt = now.UTC()
		e.wakeupLastRunTrigger = "account"
		e.mu.Unlock()
		go e.runSingleAccountWakeupClaimed(authID, cfg, force, now, rpc, exec, refresh)
		return 202, accepted, ""
	}
	if e.wakeupRunning || len(e.wakeupRunningAuths) > 0 {
		e.mu.Unlock()
		return 409, wakeupRunAccepted{}, "wakeup_busy"
	}
	trigger := "manual"
	if force {
		trigger = "test"
	}
	e.wakeupRunning = true
	e.wakeupLastRunAt = now.UTC()
	e.wakeupLastRunTrigger = trigger
	e.wakeupLastError = ""
	e.mu.Unlock()
	go e.runWakeupBatchLockedClaimed(cfg, force, trigger, "", now, rpc, exec, refresh)
	return 202, accepted, ""
}

func (e *Engine) runSingleAccountWakeupClaimed(
	authID string,
	cfg WakeupConfig,
	force bool,
	now time.Time,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
	refresh func(AuthEntry) bool,
) {
	var refreshedEntry *AuthEntry
	defer func() {
		// Refresh quota BEFORE signaling completion so polling callers immediately
		// observe the locked 5-hour reset timestamp on the card.
		if refreshedEntry != nil && refresh != nil {
			e.refreshAfterWakeup(*refreshedEntry, refresh)
		}
		e.mu.Lock()
		delete(e.wakeupRunningAuths, authID)
		e.wakeupLastRunFinished = time.Now().UTC()
		e.persistWakeupTrackingLocked(now)
		e.mu.Unlock()
	}()

	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		e.recordWakeupResult(WakeupAccountResult{
			AuthID: authID,
			Status: "failed",
			Reason: wakeupErrHostUnavail,
			At:     now.UTC().Format(time.RFC3339Nano),
		}, true)
		return
	}
	var target *AuthEntry
	for _, entry := range parseAuthList(raw) {
		if entry.ID == authID && quotaProvider(entry.Provider) {
			copyEntry := entry
			target = &copyEntry
			break
		}
	}
	if target == nil {
		e.recordWakeupResult(WakeupAccountResult{
			AuthID: authID,
			Status: "failed",
			Reason: wakeupErrAccountMiss,
			At:     now.UTC().Format(time.RFC3339Nano),
		}, true)
		return
	}
	if target.Disabled {
		e.recordWakeupResult(WakeupAccountResult{
			AuthID:   target.ID,
			Provider: target.Provider,
			Name:     target.Name,
			Status:   "skipped",
			Reason:   wakeupReasonDisabled,
			At:       now.UTC().Format(time.RFC3339Nano),
		}, false)
		return
	}
	res := e.wakeupEntry(*target, cfg, force, now, rpc, exec)
	e.recordWakeupResult(res, res.Status == "failed")
	if res.Status == "sent" {
		refreshedEntry = target
	}
}

func (e *Engine) runWakeupBatchLockedClaimed(
	cfg WakeupConfig,
	force bool,
	trigger string,
	slotKey string,
	now time.Time,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
	refresh func(AuthEntry) bool,
) {
	e.runWakeupSubsetClaimed(cfg, nil, force, trigger, slotKey, now, rpc, exec, refresh)
}

func (e *Engine) runWakeupSubsetClaimed(
	cfg WakeupConfig,
	onlyAuthIDs []string,
	force bool,
	trigger string,
	slotKey string,
	now time.Time,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
	refresh func(AuthEntry) bool,
) {
	var sentEntries []AuthEntry
	var sentMu sync.Mutex
	rollbackSlot := false
	defer func() {
		if len(sentEntries) > 0 && refresh != nil {
			refreshJobs := make(chan AuthEntry, len(sentEntries))
			for _, entry := range sentEntries {
				refreshJobs <- entry
			}
			close(refreshJobs)
			n := 4
			if len(sentEntries) < n {
				n = len(sentEntries)
			}
			var rwg sync.WaitGroup
			for i := 0; i < n; i++ {
				rwg.Add(1)
				go func() {
					defer rwg.Done()
					for entry := range refreshJobs {
						select {
						case <-e.done:
							return
						default:
						}
						e.refreshAfterWakeup(entry, refresh)
					}
				}()
			}
			rwg.Wait()
		}
		e.mu.Lock()
		if rollbackSlot && slotKey != "" {
			// Roll back the slot if CPA was still starting up so the next tick inside
			// the grace window retries instead of silently losing the scheduled run.
			delete(e.wakeupFiredSlots, slotKey)
		}
		e.wakeupRunning = false
		e.wakeupLastRunFinished = time.Now().UTC()
		e.persistWakeupTrackingLocked(now)
		e.mu.Unlock()
	}()

	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		rollbackSlot = trigger == "schedule"
		e.mu.Lock()
		e.wakeupLastError = wakeupErrHostUnavail
		e.mu.Unlock()
		return
	}
	entries := parseAuthList(raw)
	if len(entries) == 0 {
		if _, complete := completeAuthIDs(raw, entries); !complete && trigger == "schedule" {
			rollbackSlot = true
			return
		}
	}
	if len(onlyAuthIDs) == 0 {
		// Full batch replaces prior batch results so summary counts match this run.
		e.mu.Lock()
		e.wakeupResults = make(map[string]WakeupAccountResult)
		e.mu.Unlock()
	}
	only := make(map[string]bool, len(onlyAuthIDs))
	for _, id := range onlyAuthIDs {
		only[id] = true
	}
	jobs := make(chan AuthEntry, len(entries))
	for _, entry := range entries {
		if !quotaProvider(entry.Provider) {
			continue
		}
		if len(only) > 0 && !only[entry.ID] {
			continue
		}
		if entry.Disabled {
			if trigger != "reset" {
				e.recordWakeupResult(WakeupAccountResult{
					AuthID:   entry.ID,
					Provider: entry.Provider,
					Name:     entry.Name,
					Status:   "skipped",
					Reason:   wakeupReasonDisabled,
					At:       now.UTC().Format(time.RFC3339Nano),
				}, false)
			}
			continue
		}
		if !cfg.Providers[entry.Provider] {
			if trigger != "reset" {
				e.recordWakeupResult(WakeupAccountResult{
					AuthID:   entry.ID,
					Provider: entry.Provider,
					Name:     entry.Name,
					Status:   "skipped",
					Reason:   wakeupReasonProviderOff,
					At:       now.UTC().Format(time.RFC3339Nano),
				}, false)
			}
			continue
		}
		if isWakeupExcluded(cfg, entry.ID) {
			if trigger != "reset" {
				e.recordWakeupResult(WakeupAccountResult{
					AuthID:   entry.ID,
					Provider: entry.Provider,
					Name:     entry.Name,
					Status:   "skipped",
					Reason:   wakeupReasonExcluded,
					At:       now.UTC().Format(time.RFC3339Nano),
				}, false)
			}
			continue
		}
		jobs <- entry
	}
	close(jobs)

	workerCount := 4
	if len(entries) < workerCount {
		workerCount = len(entries)
	}
	if workerCount == 0 {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range jobs {
				select {
				case <-e.done:
					return
				default:
				}
				e.mu.Lock()
				if e.wakeupRunningAuths[entry.ID] {
					e.mu.Unlock()
					continue
				}
				e.wakeupRunningAuths[entry.ID] = true
				e.mu.Unlock()

				res := e.wakeupEntry(entry, cfg, force, now, rpc, exec)

				e.mu.Lock()
				delete(e.wakeupRunningAuths, entry.ID)
				e.mu.Unlock()

				e.recordWakeupResult(res, res.Status == "failed")
				if res.Status == "sent" {
					sentMu.Lock()
					sentEntries = append(sentEntries, entry)
					sentMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
}

func (e *Engine) recordWakeupResult(res WakeupAccountResult, failed bool) {
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	if res.At == "" {
		res.At = now.Format(time.RFC3339Nano)
	}
	if prev, ok := e.wakeupResults[res.AuthID]; ok && prev.At != "" && prev.At == res.At {
		if parsed, err := time.Parse(time.RFC3339Nano, prev.At); err == nil {
			res.At = parsed.Add(time.Nanosecond).UTC().Format(time.RFC3339Nano)
		}
	}
	if res.Name == "" {
		if q, ok := e.quotas[res.AuthID]; ok {
			res.Name = q.Name
			if res.Provider == "" {
				res.Provider = q.Provider
			}
		} else if name := e.names[res.AuthID]; name != "" {
			res.Name = name
		}
	}
	e.wakeupResults[res.AuthID] = res
	if failed || res.Status == "exhausted" {
		e.wakeupLastFail[res.AuthID] = now
	} else if res.Status == "sent" {
		delete(e.wakeupLastFail, res.AuthID)
	}
}

func (e *Engine) refreshAfterWakeup(entry AuthEntry, refresh func(AuthEntry) bool) {
	if !e.refreshMu.TryRLock() {
		return
	}
	defer e.refreshMu.RUnlock()
	if refresh(entry) {
		e.mu.Lock()
		if q, ok := e.quotas[entry.ID]; ok {
			e.updateWakeupObservedResetLocked(q)
			e.persistWakeupTrackingLocked(time.Now().UTC())
		}
		e.mu.Unlock()
	}
}

func isQuotaWindowExhausted(win *QuotaWindow, now time.Time) bool {
	if win == nil || win.Used < 100 {
		return false
	}
	if resetAt, ok := parseWindowResetTime(win.ResetAt); ok {
		return resetAt.After(now)
	}
	return true
}

func (e *Engine) wakeupEntry(
	entry AuthEntry,
	cfg WakeupConfig,
	force bool,
	now time.Time,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
) WakeupAccountResult {
	ts := now.UTC().Format(time.RFC3339Nano)
	if entry.Provider == "antigravity" {
		return e.wakeupAntigravityEntry(entry, cfg, force, now, rpc, exec)
	}
	e.mu.RLock()
	quota, hasQuota := e.quotas[entry.ID]
	sent := e.wakeupLastSent[wakeupSentKey(entry.ID, "")]
	candidates := e.wakeupCandidateModelsLocked(entry, "", cfg)
	e.mu.RUnlock()

	if hasQuota && (isQuotaWindowExhausted(quota.Five, now) || isQuotaWindowExhausted(quota.Week, now)) {
		return WakeupAccountResult{
			AuthID:   entry.ID,
			Provider: entry.Provider,
			Name:     entry.Name,
			Status:   "exhausted",
			Reason:   wakeupReasonExhausted,
			At:       ts,
		}
	}
	if !force && cfg.SkipActiveWindow {
		var five *QuotaWindow
		if hasQuota {
			five = quota.Five
		}
		if isFiveHourWindowActive(five, sent, now) {
			return WakeupAccountResult{
				AuthID:   entry.ID,
				Provider: entry.Provider,
				Name:     entry.Name,
				Status:   "skipped",
				Reason:   wakeupReasonWindow,
				At:       ts,
			}
		}
	}
	usedModel, failure := tryWakeupModels(entry, candidates, cfg.Message, rpc, exec)
	if failure != "" {
		status := "failed"
		if failure == wakeupErrRateLimited {
			status = "exhausted"
		} else if entry.QuotaCooling && failure == wakeupErrModelUnavail {
			// CPA skips cooling/quota-exhausted credentials during pinned auth selection and returns auth_not_found.
			status = "exhausted"
			failure = wakeupReasonExhausted
			usedModel = ""
		}
		return WakeupAccountResult{
			AuthID:   entry.ID,
			Provider: entry.Provider,
			Name:     entry.Name,
			Status:   status,
			Reason:   failure,
			Model:    usedModel,
			At:       ts,
		}
	}
	e.mu.Lock()
	e.wakeupLastSent[wakeupSentKey(entry.ID, "")] = wakeupSentRecord{At: now.UTC()}
	e.mu.Unlock()
	return WakeupAccountResult{
		AuthID:   entry.ID,
		Provider: entry.Provider,
		Name:     entry.Name,
		Status:   "sent",
		Model:    usedModel,
		At:       ts,
	}
}

func (e *Engine) wakeupAntigravityEntry(
	entry AuthEntry,
	cfg WakeupConfig,
	force bool,
	now time.Time,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
) WakeupAccountResult {
	ts := now.UTC().Format(time.RFC3339Nano)
	type groupTarget struct {
		key  string
		five *QuotaWindow
		week *QuotaWindow
	}
	e.mu.RLock()
	quota, hasQuota := e.quotas[entry.ID]
	var targets []groupTarget
	if hasQuota && len(quota.Groups) > 0 {
		for _, g := range quota.Groups {
			if g.Key == "gemini" || g.Key == "claude-gpt" {
				targets = append(targets, groupTarget{key: g.Key, five: g.Five, week: g.Week})
			}
		}
	}
	if len(targets) == 0 {
		targets = []groupTarget{{key: "gemini"}, {key: "claude-gpt"}}
	}
	e.mu.RUnlock()

	results := make([]WakeupGroupResult, 0, len(targets))
	sentCount, skippedCount, exhaustedCount, failedCount := 0, 0, 0, 0
	var firstSentModel, firstReason, firstExhaustedReason string
	for _, target := range targets {
		e.mu.RLock()
		sent := e.wakeupLastSent[wakeupSentKey(entry.ID, target.key)]
		candidates := e.wakeupCandidateModelsLocked(entry, target.key, cfg)
		e.mu.RUnlock()

		if isQuotaWindowExhausted(target.five, now) || isQuotaWindowExhausted(target.week, now) {
			exhaustedCount++
			if firstExhaustedReason == "" {
				firstExhaustedReason = wakeupReasonExhausted
			}
			results = append(results, WakeupGroupResult{
				Group:  target.key,
				Status: "exhausted",
				Reason: wakeupReasonExhausted,
				At:     ts,
			})
			continue
		}
		if !force && cfg.SkipActiveWindow && isFiveHourWindowActive(target.five, sent, now) {
			skippedCount++
			results = append(results, WakeupGroupResult{
				Group:  target.key,
				Status: "skipped",
				Reason: wakeupReasonWindow,
				At:     ts,
			})
			continue
		}
		usedModel, failure := tryWakeupModels(entry, candidates, cfg.Message, rpc, exec)
		if failure != "" {
			gStatus := "failed"
			if failure == wakeupErrRateLimited {
				gStatus = "exhausted"
				exhaustedCount++
				if firstExhaustedReason == "" {
					firstExhaustedReason = failure
				}
			} else if entry.QuotaCooling && failure == wakeupErrModelUnavail {
				gStatus = "exhausted"
				failure = wakeupReasonExhausted
				usedModel = ""
				exhaustedCount++
				if firstExhaustedReason == "" {
					firstExhaustedReason = failure
				}
			} else {
				failedCount++
				if firstReason == "" {
					firstReason = failure
				}
			}
			results = append(results, WakeupGroupResult{
				Group:  target.key,
				Status: gStatus,
				Reason: failure,
				Model:  usedModel,
				At:     ts,
			})
			if failure == wakeupErrHostUnsupp || failure == wakeupErrAuthFailed || failure == wakeupErrAccountDis {
				break
			}
			continue
		}
		sentCount++
		if firstSentModel == "" {
			firstSentModel = usedModel
		}
		e.mu.Lock()
		e.wakeupLastSent[wakeupSentKey(entry.ID, target.key)] = wakeupSentRecord{At: now.UTC()}
		e.mu.Unlock()
		results = append(results, WakeupGroupResult{
			Group:  target.key,
			Status: "sent",
			Model:  usedModel,
			At:     ts,
		})
	}

	out := WakeupAccountResult{
		AuthID:   entry.ID,
		Provider: entry.Provider,
		Name:     entry.Name,
		At:       ts,
		Groups:   results,
	}
	switch {
	case sentCount > 0:
		out.Status = "sent"
		out.Model = firstSentModel
		if firstReason != "" {
			out.Reason = firstReason
		} else if firstExhaustedReason != "" {
			out.Reason = firstExhaustedReason
		}
	case failedCount > 0:
		out.Status = "failed"
		out.Reason = firstReason
	case exhaustedCount > 0:
		out.Status = "exhausted"
		out.Reason = firstExhaustedReason
	default:
		out.Status = "skipped"
		out.Reason = wakeupReasonWindow
	}
	return out
}

func isNonChatWakeupModel(model string) bool {
	l := strings.ToLower(model)
	for _, token := range []string{"image", "video", "tts", "speech", "audio", "whisper", "embedding", "auto-review", "reserve"} {
		if strings.Contains(l, token) {
			return true
		}
	}
	return false
}

func isHeavyWakeupModel(model string) bool {
	l := strings.ToLower(model)
	for _, token := range []string{"astra", "opus", "fable", "xhigh", "thinking", "-pro", "pro-", "sol", "terra", "research", "max"} {
		if strings.Contains(l, token) {
			return true
		}
	}
	return false
}

func isLightWakeupModel(model string) bool {
	if isNonChatWakeupModel(model) || isHeavyWakeupModel(model) {
		return false
	}
	l := strings.ToLower(model)
	for _, token := range []string{"haiku", "nano", "mini", "flash-lite", "lite", "spark", "flash", "luna", "fast"} {
		if strings.Contains(l, token) {
			return true
		}
	}
	return false
}

func (e *Engine) wakeupCandidateModelsLocked(entry AuthEntry, group string, cfg WakeupConfig) []string {
	// Explicit user-configured model takes sole priority so we never silently bill
	// a different/heavier model than the user requested.
	if entry.Provider == "antigravity" {
		if m := strings.TrimSpace(cfg.Models["antigravity:"+group]); m != "" {
			return []string{m}
		}
		if m := strings.TrimSpace(cfg.Models["antigravity"]); m != "" {
			if group == "" || antigravityModelGroup(m) == group {
				return []string{m}
			}
		}
	} else if m := strings.TrimSpace(cfg.Models[entry.Provider]); m != "" {
		return []string{m}
	}

	seen := make(map[string]bool)
	var models []string
	addModel := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" || len(m) > wakeupMaxModelBytes || !wakeupModelPattern.MatchString(m) || seen[m] {
			return
		}
		seen[m] = true
		models = append(models, m)
	}

	// Prefer true lightweight models first; avoid heavy flagship models (e.g. astra/opus) by default.
	switch entry.Provider {
	case "claude":
		for _, m := range []string{
			"claude-haiku-4-5-20251001",
			"claude-haiku-4-5",
			"claude-3-5-haiku-20241022",
			"claude-sonnet-4-6",
			"claude-sonnet-4-5-20250929",
		} {
			addModel(m)
		}
	case "codex":
		for _, m := range []string{
			"gpt-5.1-codex-mini",
			"gpt-5.4-mini",
			"gpt-5.3-codex-spark",
			"gpt-5.4-nano",
			"gpt-5-mini",
			"gpt-5.6-luna",
			"gpt-6-luna",
			"gpt-5.5",
			"gpt-5.2-codex",
			"gpt-5.3-codex",
			"gpt-5.4",
		} {
			addModel(m)
		}
	case "antigravity":
		if group == "claude-gpt" {
			for _, m := range []string{
				"gpt-oss-120b-medium",
				"claude-sonnet-4-6",
			} {
				addModel(m)
			}
		} else {
			for _, m := range []string{
				"gemini-2.5-flash-lite",
				"gemini-3.1-flash-lite",
				"gemini-3.5-flash-lite",
				"gemini-3-flash",
				"gemini-2.5-flash",
			} {
				addModel(m)
			}
		}
	}

	// Append recently succeeded lightweight observed models for this exact credential,
	// keeping non-chat models out and placing heavy flagship models last as fallback only.
	canonical := e.canonicalAuth(entry.ID)
	var lightObserved, normalObserved, heavyObserved []string
	obsSeen := make(map[string]bool)
	for i := len(e.events) - 1; i >= 0 && len(obsSeen) < 16; i-- {
		ev := e.events[i]
		if ev.Failed || e.canonicalAuth(ev.AuthID) != canonical || normalizeProvider(ev.Provider) != entry.Provider {
			continue
		}
		m := strings.TrimSpace(ev.Model)
		if m == "" || obsSeen[m] || isNonChatWakeupModel(m) {
			continue
		}
		if entry.Provider == "antigravity" && group != "" && antigravityModelGroup(m) != group {
			continue
		}
		obsSeen[m] = true
		switch {
		case isLightWakeupModel(m):
			lightObserved = append(lightObserved, m)
		case isHeavyWakeupModel(m):
			heavyObserved = append(heavyObserved, m)
		default:
			normalObserved = append(normalObserved, m)
		}
	}
	merged := make([]string, 0, len(lightObserved)+len(models)+len(normalObserved)+len(heavyObserved))
	mergedSeen := make(map[string]bool)
	appendUnique := func(list []string) {
		for _, m := range list {
			if m != "" && wakeupModelPattern.MatchString(m) && !mergedSeen[m] {
				mergedSeen[m] = true
				merged = append(merged, m)
			}
		}
	}
	appendUnique(lightObserved)
	appendUnique(models)
	appendUnique(normalObserved)
	appendUnique(heavyObserved)
	return merged
}

func tryWakeupModels(
	entry AuthEntry,
	candidates []string,
	message string,
	rpc func(string, any) (any, error),
	exec func(AuthEntry, string, string, func(string, any) (any, error)) (string, string),
) (string, string) {
	if len(candidates) == 0 {
		return "", wakeupErrModelUnavail
	}
	lastModel := ""
	lastErr := wakeupErrModelUnavail
	for _, model := range candidates {
		usedModel, failure := exec(entry, model, message, rpc)
		if failure == "" {
			return usedModel, ""
		}
		lastModel = model
		lastErr = failure
		// Only try the next candidate when this specific model is unavailable/unsupported.
		if failure != wakeupErrModelUnavail {
			return lastModel, lastErr
		}
	}
	return lastModel, lastErr
}

// executeWakeupModel sends one minimal chat request pinned to entry.ID via CPA's
// native host.model.execute callback so OAuth token refresh, provider headers,
// cloaking, and credential-scoped proxies are all handled by CPA itself.
func executeWakeupModel(entry AuthEntry, model string, message string, rpc func(string, any) (any, error)) (string, string) {
	prompt := strings.TrimSpace(message)
	if prompt == "" {
		prompt = wakeupDefaultMessage
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": wakeupDefaultMaxTokens,
		"stream":     false,
	})
	if err != nil {
		return model, wakeupErrRequestFailed
	}
	forcedProvider := strings.ToLower(strings.TrimSpace(entry.RawProvider))
	if forcedProvider == "" {
		forcedProvider = entry.Provider
	}
	req := map[string]any{
		"entry_protocol":  "openai",
		"exit_protocol":   "openai",
		"model":           model,
		"stream":          false,
		"body":            payload,
		"forced_provider": forcedProvider,
		"auth_id":         entry.ID,
	}
	data, err := rpc("host.model.execute", req)
	if err != nil {
		return model, classifyWakeupError(err)
	}
	if r, ok := data.(map[string]any); ok {
		code := int(number(r, "StatusCode", "status_code", "statusCode"))
		if code >= 200 && code < 300 {
			return model, ""
		}
		if code > 0 {
			return model, classifyWakeupStatus(code, "")
		}
	}
	return model, wakeupErrRequestFailed
}

func classifyWakeupError(err error) string {
	var rpcErr *hostRPCError
	if errors.As(err, &rpcErr) {
		detail := strings.ToLower(rpcErr.Detail)
		if strings.Contains(detail, "unsupported host callback") || strings.Contains(detail, "host model executor is unavailable") {
			return wakeupErrHostUnsupp
		}
		if strings.Contains(detail, "disabled") {
			return wakeupErrAccountDis
		}
		if rpcErr.StatusCode > 0 {
			return classifyWakeupStatus(rpcErr.StatusCode, detail)
		}
		if strings.Contains(detail, "429") || strings.Contains(detail, "rate limit") || strings.Contains(detail, "rate_limit") || strings.Contains(detail, "quota") || strings.Contains(detail, "cooldown") || strings.Contains(detail, "exhausted") || strings.Contains(detail, "usage limit") {
			return wakeupErrRateLimited
		}
		if strings.Contains(detail, "auth_not_found") || strings.Contains(detail, "model") || strings.Contains(detail, "not found") || strings.Contains(detail, "unsupported") {
			return wakeupErrModelUnavail
		}
		return wakeupErrRequestFailed
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unavailable") || strings.Contains(msg, "unsupported") {
		return wakeupErrHostUnsupp
	}
	return wakeupErrRequestFailed
}

func classifyWakeupStatus(status int, detail string) string {
	switch status {
	case 401, 403:
		return wakeupErrAuthFailed
	case 429:
		return wakeupErrRateLimited
	case 400, 404, 422:
		if strings.Contains(detail, "disabled") {
			return wakeupErrAccountDis
		}
		return wakeupErrModelUnavail
	default:
		if strings.Contains(detail, "rate limit") || strings.Contains(detail, "rate_limit") || strings.Contains(detail, "quota") || strings.Contains(detail, "cooldown") || strings.Contains(detail, "usage limit") {
			return wakeupErrRateLimited
		}
		if strings.Contains(detail, "auth_not_found") || strings.Contains(detail, "no auth available") || strings.Contains(detail, "unknown model") || strings.Contains(detail, "model_not_found") || strings.Contains(detail, "does not support") {
			return wakeupErrModelUnavail
		}
		return wakeupErrRequestFailed
	}
}
