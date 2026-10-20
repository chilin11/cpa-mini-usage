package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	eventDays       = 35
	quotaInterval   = 15 * time.Minute
	pricingInterval = 6 * time.Hour
)

type Event struct {
	At             time.Time `json:"at"`
	AuthID         string    `json:"auth_id"`
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	RequestedModel string    `json:"requested_model,omitempty"`
	ResponseModel  string    `json:"response_model,omitempty"`
	Input          int64     `json:"input"`
	Output         int64     `json:"output"`
	CacheRead      int64     `json:"cache_read"`
	CacheWrite     int64     `json:"cache_write"`
	Total          int64     `json:"total"`
	Failed         bool      `json:"failed"`
	LatencyMS      *float64  `json:"latency_ms,omitempty"`
	TTFTMS         *float64  `json:"ttft_ms,omitempty"`
}

// RecentEvent adds snapshot-only valuation without changing stored usage records.
type RecentEvent struct {
	Event
	Cost         *float64 `json:"cost,omitempty"`
	MissingPrice bool     `json:"missing_price"`
	ModelStatus  string   `json:"model_status"`
	UsageQuality string   `json:"usage_quality"`
}

type PriceTier struct {
	Threshold  int64    `json:"threshold"`
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

type ModelPrice struct {
	Input        float64     `json:"input"`
	Output       float64     `json:"output"`
	CacheRead    *float64    `json:"cache_read,omitempty"`
	CacheWrite   *float64    `json:"cache_write,omitempty"`
	ContextTier  *PriceTier  `json:"context_tier,omitempty"` // Legacy price-cache compatibility.
	ContextTiers []PriceTier `json:"context_tiers,omitempty"`
}

type QuotaWindow struct {
	Used    float64 `json:"used"`
	ResetAt string  `json:"reset_at,omitempty"`
	Seconds int64   `json:"seconds,omitempty"`
}

type AccountQuota struct {
	Provider             string       `json:"provider"`
	AuthID               string       `json:"auth_id"`
	Name                 string       `json:"name"`
	Five                 *QuotaWindow `json:"five_hour,omitempty"`
	Week                 *QuotaWindow `json:"seven_day,omitempty"`
	FetchedAt            string       `json:"fetched_at"`
	Error                string       `json:"error,omitempty"`
	ManualResetRemaining *int64       `json:"manual_reset_remaining,omitempty"`
	Disabled             bool         `json:"disabled,omitempty"`
}

type WindowEstimate struct {
	UsedPercent            float64  `json:"used_percent"`
	ResetAt                string   `json:"reset_at,omitempty"`
	ObservedTokens         int64    `json:"observed_tokens"`
	ObservedCost           *float64 `json:"observed_cost,omitempty"`
	EstimatedTotalTokens   *float64 `json:"estimated_total_tokens,omitempty"`
	EstimatedTotalCost     *float64 `json:"estimated_total_cost,omitempty"`
	EstimatedBasis         string   `json:"estimated_basis,omitempty"`
	EstimatedAt            string   `json:"estimated_at,omitempty"`
	EstimateObservedTokens *int64   `json:"estimate_observed_tokens,omitempty"`
	EstimateObservedCost   *float64 `json:"estimate_observed_cost,omitempty"`
	EstimateUsedPercent    *float64 `json:"estimate_used_percent,omitempty"`
	EstimateRetained       bool     `json:"estimate_retained,omitempty"`
	MissingPrice           bool     `json:"missing_price"`
	PartialHistory         bool     `json:"partial_history"`
}

type QuotaView struct {
	Provider             string          `json:"provider"`
	AuthID               string          `json:"auth_id"`
	Name                 string          `json:"name"`
	Five                 *WindowEstimate `json:"five_hour,omitempty"`
	Week                 *WindowEstimate `json:"seven_day,omitempty"`
	FetchedAt            string          `json:"fetched_at,omitempty"`
	Error                string          `json:"error,omitempty"`
	ManualResetRemaining *int64          `json:"manual_reset_remaining,omitempty"`
	Disabled             bool            `json:"disabled,omitempty"`
}

type ModelStat struct {
	Model    string   `json:"model"`
	Requests int64    `json:"requests"`
	Tokens   int64    `json:"tokens"`
	Cost     *float64 `json:"cost,omitempty"`
}

type CredentialView struct {
	AuthID        string      `json:"auth_id"`
	Name          string      `json:"name"`
	Provider      string      `json:"provider"`
	Requests      int64       `json:"requests"`
	Success       int64       `json:"success"`
	Failures      int64       `json:"failures"`
	Input         int64       `json:"input"`
	TotalInput    int64       `json:"total_input"`
	UncachedInput int64       `json:"uncached_input"`
	CacheWrite    int64       `json:"cache_write"`
	Output        int64       `json:"output"`
	Cached        int64       `json:"cached"`
	Total         int64       `json:"total"`
	Cost          *float64    `json:"cost,omitempty"`
	MissingPrice  bool        `json:"missing_price"`
	Models        []ModelStat `json:"models"`
}

type AccountRefreshState struct {
	Refreshing bool   `json:"refreshing"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Snapshot struct {
	Quotas                 []QuotaView                    `json:"quotas"`
	Credentials            []CredentialView               `json:"credentials"`
	Recent                 []RecentEvent                  `json:"recent"`
	TotalRequests          int                            `json:"total_requests"`
	PriceUpdatedAt         string                         `json:"price_updated_at,omitempty"`
	QuotaUpdatedAt         string                         `json:"quota_updated_at,omitempty"`
	QuotaRefreshing        bool                           `json:"quota_refreshing"`
	QuotaRefreshFinishedAt string                         `json:"quota_refresh_finished_at,omitempty"`
	AccountRefresh         map[string]AccountRefreshState `json:"account_refresh"`
	Persistence            PersistenceSettings            `json:"persistence"`
	StorageError           string                         `json:"storage_error,omitempty"`
	PriceError             string                         `json:"price_error,omitempty"`
	QuotaError             string                         `json:"quota_error,omitempty"`
	GeneratedAt            string                         `json:"generated_at"`
}

type Engine struct {
	mu                   sync.RWMutex
	events               []Event
	quotas               map[string]AccountQuota
	forecasts            map[string]SavedForecast
	names                map[string]string
	aliases              map[string]string
	prices               map[string]ModelPrice
	quotaUpdated         time.Time
	quotaRefreshing      bool
	quotaRefreshFinished time.Time
	accountRefresh       map[string]AccountRefreshState
	priceUpdated         time.Time
	priceError           string
	quotaError           string
	storageError         string
	storageReadFailed    bool
	persistence          PersistenceSettings
	dir                  string
	done                 chan struct{}
	stopOnce             sync.Once
	refreshMu            sync.RWMutex
	consuming            map[string]bool
	lastRefresh          time.Time
}

func newEngine() *Engine {
	dir := os.Getenv("CPA_MINI_USAGE_DATA_DIR")
	if dir == "" {
		dir = filepath.Join("data", "cpa-mini-usage")
	}
	if absolute, err := filepath.Abs(dir); err == nil {
		dir = absolute
	}
	return &Engine{quotas: make(map[string]AccountQuota), forecasts: make(map[string]SavedForecast), accountRefresh: make(map[string]AccountRefreshState), consuming: make(map[string]bool), names: make(map[string]string), aliases: make(map[string]string), prices: make(map[string]ModelPrice), dir: dir, persistence: PersistenceSettings{Enabled: true, Directory: dir}, done: make(chan struct{})}
}

func (e *Engine) start() {
	e.load()
	go func() {
		// Begin collecting immediately; there is no fixed startup delay.
		go e.refreshPrices(false)
		e.kickRefresh(false)
		tq := time.NewTicker(quotaInterval)
		tp := time.NewTicker(pricingInterval)
		tc := time.NewTicker(24 * time.Hour)
		defer tq.Stop()
		defer tp.Stop()
		defer tc.Stop()
		for {
			select {
			case <-e.done:
				return
			case <-tq.C:
				e.kickRefresh(false)
			case <-tp.C:
				go e.refreshPrices(false)
			case <-tc.C:
				e.mu.Lock()
				e.compact()
				e.mu.Unlock()
			}
		}
	}()
}
func (e *Engine) shutdown() { e.stopOnce.Do(func() { close(e.done) }) }
func (e *Engine) kickRefresh(force bool) {
	e.mu.RLock()
	recently := time.Since(e.lastRefresh) < quotaInterval
	e.mu.RUnlock()
	if !force && recently {
		return
	}
	go e.refreshQuota(force)
}

func (e *Engine) load() {
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		e.blockStorageWrites("Unable to create the data directory")
		return
	}
	e.loadPersistence()
	path := filepath.Join(e.dir, "events.jsonl")
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		cutoff := time.Now().Add(-eventDays * 24 * time.Hour)
		for scanner.Scan() {
			if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
				continue
			}
			var x Event
			if json.Unmarshal(scanner.Bytes(), &x) != nil || x.At.IsZero() {
				e.blockStorageWrites("Some historical records could not be read")
				continue
			}
			if !x.At.Before(cutoff) {
				e.events = append(e.events, x)
			}
		}
		if scanner.Err() != nil {
			e.blockStorageWrites("Some historical records could not be read")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		e.blockStorageWrites("Unable to read historical usage")
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "prices.json"))
	if err == nil {
		var v struct {
			Updated time.Time             `json:"updated"`
			Prices  map[string]ModelPrice `json:"prices"`
		}
		if json.Unmarshal(b, &v) != nil || bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			e.blockStorageWrites("Price history could not be read")
		} else if len(v.Prices) > 0 {
			e.prices = v.Prices
			e.priceUpdated = v.Updated
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		e.blockStorageWrites("Price history could not be read")
	}
	e.compact()
}

func (e *Engine) compact() {
	cutoff := time.Now().Add(-eventDays * 24 * time.Hour)
	kept := make([]Event, 0, len(e.events))
	for _, x := range e.events {
		if !x.At.Before(cutoff) {
			kept = append(kept, x)
		}
	}
	// Limit in-memory storage for a long-lived, very busy CPA instance.
	if len(kept) > 200000 {
		kept = kept[len(kept)-200000:]
	}
	e.events = kept
	if !e.persistence.Enabled || e.storageReadFailed {
		return
	}
	var buf bytes.Buffer
	for _, x := range kept {
		b, _ := json.Marshal(x)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := writeAtomic(filepath.Join(e.dir, "events.jsonl"), buf.Bytes()); err != nil {
		e.storageError = "Unable to persist request statistics"
	}
}
func writeAtomic(path string, body []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func normalizeProvider(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch {
	case strings.Contains(p, "codex") || p == "openai":
		return "codex"
	case strings.Contains(p, "claude") || strings.Contains(p, "anthropic"):
		return "claude"
	default:
		return p
	}
}
func (e *Engine) capture(data []byte) {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return
	}
	for _, k := range []string{"Record", "record", "Usage", "usage"} {
		if v, ok := m[k].(map[string]any); ok {
			m = v
			break
		}
	}
	when := time.Now().UTC()
	if s := str(m, "RequestedAt", "requested_at"); s != "" {
		if v, err := time.Parse(time.RFC3339Nano, s); err == nil {
			when = v
		}
	}
	if when.Before(time.Now().Add(-eventDays * 24 * time.Hour)) {
		return
	}
	detail, _ := getAny(m, "Detail", "detail").(map[string]any)
	provider := normalizeProvider(str(m, "Provider", "provider", "ExecutorType", "executor_type"))
	authID := str(m, "AuthID", "auth_id", "AuthIndex", "auth_index")
	if authID == "" {
		authID = "unattributed"
	}
	input := number(detail, "InputTokens", "input_tokens")
	output := number(detail, "OutputTokens", "output_tokens")
	cached := number(detail, "CacheReadTokens", "cache_read_tokens")
	if getAny(detail, "CacheReadTokens", "cache_read_tokens") == nil {
		cached = number(detail, "CachedTokens", "cached_tokens")
	}
	write := number(detail, "CacheCreationTokens", "cache_creation_tokens", "CacheWriteTokens", "cache_write_tokens")
	total := number(detail, "TotalTokens", "total_tokens")
	if total <= 0 {
		total = input + output
		if provider == "claude" {
			total += cached + write
		}
	}
	if input < 0 || output < 0 || cached < 0 || write < 0 || total < 0 {
		return
	}
	evt := Event{At: when, AuthID: authID, Provider: provider, Model: str(m, "Model", "model", "Alias", "alias"), Input: input, Output: output, CacheRead: cached, CacheWrite: write, Total: total, Failed: getAny(m, "Failed", "failed") == true}
	// Keep the existing usage model for pricing; observation names are diagnostic only.
	evt.RequestedModel = observedModelName(str(m, "Alias", "alias"))
	evt.ResponseModel = observedModelName(str(m, "ResponseModel", "response_model"))
	evt.LatencyMS = durationMilliseconds(m, "Latency", "latency")
	// CPA sends zero TTFT for requests without a measured streaming first token.
	if streamed, present := getAny(m, "Stream", "stream").(bool); !present || streamed {
		evt.TTFTMS = durationMilliseconds(m, "TTFT", "ttft")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, evt)
	if len(e.events) > 200000 {
		e.events = e.events[len(e.events)-200000:]
	}
	if !e.persistence.Enabled || e.storageReadFailed {
		return
	}
	b, _ := json.Marshal(evt)
	f, err := os.OpenFile(filepath.Join(e.dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		e.storageError = "Usage could not be written to disk"
		return
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		e.storageError = "Usage could not be written to disk"
	}
	if err = f.Close(); err != nil {
		e.storageError = "Usage could not be written to disk"
	}
}

func getAny(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}
func str(m map[string]any, keys ...string) string {
	if x, ok := getAny(m, keys...).(string); ok {
		return x
	}
	return ""
}
func number(m map[string]any, keys ...string) int64 {
	switch v := getAny(m, keys...).(type) {
	case float64:
		if finite(v) && v >= math.MinInt64 && v < math.MaxInt64 {
			return int64(v)
		}
	case json.Number:
		n, _ := v.Int64()
		return n
	case int64:
		return v
	}
	return 0
}
func f64(v any) (float64, bool)   { x, ok := v.(float64); return x, ok }
func floatPtr(v float64) *float64 { return &v }

func (e *Engine) eventCost(ev Event) (float64, bool) {
	if usageQuality(ev) != "complete" {
		return 0, false
	}
	key := normalizeModel(ev.Model)
	p, ok := e.prices[priceProvider(ev.Provider)+"/"+key]
	if !ok {
		return 0, false
	}
	contextSize := ev.Input
	if normalizeProvider(ev.Provider) == "claude" {
		contextSize += ev.CacheRead + ev.CacheWrite
	}
	var selected *PriceTier
	if tier := p.ContextTier; tier != nil && contextSize > tier.Threshold {
		selected = tier
	}
	for i := range p.ContextTiers {
		tier := &p.ContextTiers[i]
		if contextSize > tier.Threshold && (selected == nil || tier.Threshold >= selected.Threshold) {
			selected = tier
		}
	}
	if tier := selected; tier != nil {
		p.Input, p.Output = tier.Input, tier.Output
		if tier.CacheRead != nil {
			p.CacheRead = tier.CacheRead
		}
		if tier.CacheWrite != nil {
			p.CacheWrite = tier.CacheWrite
		}
	}
	cached, write, baseInput := ev.CacheRead, ev.CacheWrite, ev.Input
	if normalizeProvider(ev.Provider) != "claude" {
		// OpenAI prompt totals include cache reads AND cache writes.
		baseInput = ev.Input - cached - write
	}
	cost := float64(baseInput)*p.Input/1e6 + float64(ev.Output)*p.Output/1e6
	// A missing cache price is unknown, not a guessed discount or a free rate.
	if cached > 0 {
		if p.CacheRead == nil {
			return 0, false
		}
		cost += float64(cached) * *p.CacheRead / 1e6
	}
	if write > 0 {
		if p.CacheWrite == nil {
			return 0, false
		}
		cost += float64(write) * *p.CacheWrite / 1e6
	}
	if !finite(cost) || cost < 0 {
		return 0, false
	}
	return cost, true
}
func priceProvider(p string) string {
	if normalizeProvider(p) == "claude" {
		return "anthropic"
	}
	if normalizeProvider(p) == "codex" {
		return "openai"
	}
	return strings.ToLower(p)
}
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if idx := strings.IndexByte(m, '/'); idx >= 0 {
		m = m[idx+1:]
	}
	return m
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func roundForecastCost(v float64) float64 {
	if scaled := v * 100; finite(scaled) {
		return math.Round(scaled) / 100
	}
	return v
}

type windowUsage struct {
	tokens       int64
	cost         float64
	count        int
	missingPrice bool
	first, last  time.Time
}

func (e *Engine) usageBetween(authID string, start, end, now time.Time) windowUsage {
	var usage windowUsage
	for _, ev := range e.events {
		if e.canonicalAuth(ev.AuthID) != authID || ev.At.Before(start) || !ev.At.Before(end) || ev.At.After(now) {
			continue
		}
		usage.count++
		usage.tokens += ev.Total
		if usage.first.IsZero() || ev.At.Before(usage.first) {
			usage.first = ev.At
		}
		if ev.At.After(usage.last) {
			usage.last = ev.At
		}
		if cost, ok := e.eventCost(ev); ok {
			usage.cost += cost
		} else {
			usage.missingPrice = true
		}
	}
	return usage
}

// Direct callers assume a quota observed now; snapshots supply the real fetch time.
func (e *Engine) windowSummary(authID string, now time.Time, win *QuotaWindow, duration time.Duration) *WindowEstimate {
	return e.windowSummaryObserved(authID, now, now, win, duration)
}

func (e *Engine) windowSummaryObserved(authID string, now, observation time.Time, win *QuotaWindow, duration time.Duration) *WindowEstimate {
	if win == nil {
		return nil
	}
	fiveHour := duration == 5*time.Hour
	out := &WindowEstimate{UsedPercent: win.Used, ResetAt: win.ResetAt}
	if !finite(out.UsedPercent) {
		out.UsedPercent = 0 // Keep the snapshot JSON encodable for invalid quota data.
	}
	reset, err := time.Parse(time.RFC3339Nano, win.ResetAt)
	if err != nil {
		return out
	}
	if win.Seconds > 0 {
		if win.Seconds > math.MaxInt64/int64(time.Second) {
			return out
		}
		duration = time.Duration(win.Seconds) * time.Second
	}
	if duration <= 0 {
		return out
	}
	start := reset.Add(-duration)
	if start.After(now) || reset.Before(now.Add(-5*time.Minute)) {
		return out
	}
	current := e.usageBetween(e.canonicalAuth(authID), start, reset, now)
	out.ObservedTokens, out.MissingPrice = current.tokens, current.missingPrice
	if current.count > 0 {
		out.PartialHistory = current.first.After(start.Add(10 * time.Minute))
	}
	if !current.missingPrice && finite(current.cost) {
		out.ObservedCost = floatPtr(current.cost)
	}
	// An unused five-hour window displays zero, not adjacent history or a cap.
	if fiveHour && now.Before(reset) && win.Used == 0 && current.count == 0 {
		out.EstimatedTotalTokens, out.EstimatedTotalCost = floatPtr(0), floatPtr(0)
		out.EstimatedBasis = "unused"
		return out
	}
	// Keep actuals live, but pair the percentage only with usage observed at
	// the same quota timestamp. Later requests must not erase that estimate.
	if !now.Before(reset) || (!observation.IsZero() && (observation.Before(start) || observation.After(now))) {
		return out
	}
	basis := current
	if !observation.IsZero() {
		basis = e.usageBetween(e.canonicalAuth(authID), start, reset, observation)
	}
	if !observation.IsZero() && basis.count > 0 && finite(win.Used) && win.Used > 0 && win.Used <= 100 {
		multiplier := 100 / win.Used
		tokens := math.Max(float64(current.tokens), math.Round(float64(basis.tokens)*multiplier))
		cost := math.Max(basis.cost, roundForecastCost(basis.cost*multiplier))
		if !current.missingPrice && finite(current.cost) {
			cost = math.Max(current.cost, cost)
		}
		if finite(multiplier) && finite(tokens) && (basis.missingPrice || finite(cost)) {
			out.EstimatedTotalTokens, out.EstimatedBasis = floatPtr(tokens), "quota"
			out.EstimatedAt = observation.Format(time.RFC3339Nano)
			out.EstimateObservedTokens = &basis.tokens
			if !basis.missingPrice {
				out.EstimatedTotalCost = floatPtr(cost)
				out.EstimateObservedCost = floatPtr(basis.cost)
			}
			return out
		}
	}
	previous := e.usageBetween(e.canonicalAuth(authID), start.Add(-duration), start, now)
	// Only recorded adjacent history is evidence. Reject the entire fallback
	// if any known component is already below current actual usage.
	if previous.count == 0 || previous.tokens < current.tokens || (!previous.missingPrice && !current.missingPrice && previous.cost+1e-9 < current.cost) {
		return out
	}
	out.EstimatedTotalTokens, out.EstimatedBasis = floatPtr(float64(previous.tokens)), "previous"
	out.MissingPrice = out.MissingPrice || previous.missingPrice
	if !out.MissingPrice && finite(previous.cost) {
		out.EstimatedTotalCost = floatPtr(previous.cost)
	}
	return out
}

func (e *Engine) snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now().UTC()
	result := Snapshot{Quotas: make([]QuotaView, 0, len(e.quotas)), Credentials: []CredentialView{}, Recent: []RecentEvent{}, PriceUpdatedAt: e.priceUpdated.Format(time.RFC3339), QuotaUpdatedAt: e.quotaUpdated.Format(time.RFC3339), PriceError: e.priceError, QuotaError: e.quotaError, StorageError: e.storageError, GeneratedAt: now.Format(time.RFC3339)}
	result.QuotaRefreshing = e.quotaRefreshing
	result.Persistence = e.persistence
	if !e.quotaRefreshFinished.IsZero() {
		result.QuotaRefreshFinishedAt = e.quotaRefreshFinished.Format(time.RFC3339Nano)
	}
	result.AccountRefresh = make(map[string]AccountRefreshState, len(e.accountRefresh))
	for id, state := range e.accountRefresh {
		result.AccountRefresh[id] = state
	}
	if e.priceUpdated.IsZero() {
		result.PriceUpdatedAt = ""
	}
	if e.quotaUpdated.IsZero() {
		result.QuotaUpdatedAt = ""
	}
	forecastsChanged := e.pruneForecastsLocked(now)
	for _, q := range e.quotas {
		five, changedFive := e.resolveForecastLocked(q, "5h", now)
		week, changedWeek := e.resolveForecastLocked(q, "7d", now)
		forecastsChanged = forecastsChanged || changedFive || changedWeek
		result.Quotas = append(result.Quotas, QuotaView{Provider: q.Provider, AuthID: q.AuthID, Name: q.Name, Five: five, Week: week, FetchedAt: q.FetchedAt, Error: q.Error, ManualResetRemaining: q.ManualResetRemaining, Disabled: q.Disabled})
	}
	if forecastsChanged {
		e.persistQuotasLocked()
	}
	sort.Slice(result.Quotas, func(i, j int) bool {
		if result.Quotas[i].Provider != result.Quotas[j].Provider {
			return result.Quotas[i].Provider < result.Quotas[j].Provider
		}
		if result.Quotas[i].Name != result.Quotas[j].Name {
			return result.Quotas[i].Name < result.Quotas[j].Name
		}
		return result.Quotas[i].AuthID < result.Quotas[j].AuthID
	})
	stats := make(map[string]*CredentialView)
	modelStats := make(map[string]map[string]*ModelStat)
	cutoff := now.Add(-7 * 24 * time.Hour)
	for i := len(e.events) - 1; i >= 0; i-- {
		ev := e.events[i]
		if ev.At.Before(cutoff) || ev.At.After(now.Add(2*time.Minute)) {
			continue
		}
		result.TotalRequests++
		cost, ok := e.eventCost(ev)
		if len(result.Recent) < 60 {
			recent := RecentEvent{Event: ev, MissingPrice: !ok, UsageQuality: usageQuality(ev), ModelStatus: modelObservationStatus(ev)}
			recent.AuthID = e.canonicalAuth(ev.AuthID)
			if ok {
				recent.Cost = floatPtr(cost)
			}
			result.Recent = append(result.Recent, recent)
		}
		canonical := e.canonicalAuth(ev.AuthID)
		s := stats[canonical]
		if s == nil {
			s = &CredentialView{AuthID: canonical, Provider: ev.Provider, Name: e.names[canonical], Models: []ModelStat{}}
			if s.Name == "" {
				s.Name = shortID(ev.AuthID)
			}
			stats[canonical] = s
			modelStats[canonical] = make(map[string]*ModelStat)
		}
		s.Requests++
		if ev.Failed {
			s.Failures++
		} else {
			s.Success++
		}
		s.Input += ev.Input
		totalInput, uncachedInput := inputBreakdown(ev)
		s.TotalInput += totalInput
		s.UncachedInput += uncachedInput
		s.CacheWrite += ev.CacheWrite
		s.Output += ev.Output
		s.Cached += ev.CacheRead
		s.Total += ev.Total
		ms := modelStats[canonical][ev.Model]
		if ms == nil {
			ms = &ModelStat{Model: ev.Model}
			modelStats[canonical][ev.Model] = ms
		}
		ms.Requests++
		ms.Tokens += ev.Total
		if !ok {
			s.MissingPrice = true
			ms.Cost = nil
		} else {
			if ms.Cost != nil {
				*ms.Cost += cost
			} else if ms.Requests == 1 {
				ms.Cost = floatPtr(cost)
			}
			// Do not display partial sum as a complete amount.
			if s.Cost != nil {
				*s.Cost += cost
			} else if s.Requests == 1 {
				s.Cost = floatPtr(cost)
			}
		}
	}
	for _, s := range stats {
		if s.MissingPrice {
			s.Cost = nil
		}
		for _, m := range modelStats[s.AuthID] {
			s.Models = append(s.Models, *m)
		}
		sort.Slice(s.Models, func(i, j int) bool {
			if s.Models[i].Tokens != s.Models[j].Tokens {
				return s.Models[i].Tokens > s.Models[j].Tokens
			}
			return s.Models[i].Model < s.Models[j].Model
		})
		result.Credentials = append(result.Credentials, *s)
	}
	sort.Slice(result.Credentials, func(i, j int) bool {
		if result.Credentials[i].Total != result.Credentials[j].Total {
			return result.Credentials[i].Total > result.Credentials[j].Total
		}
		return result.Credentials[i].AuthID < result.Credentials[j].AuthID
	})
	return result
}

func shortID(id string) string {
	if len(id) <= 14 {
		return id
	}
	return id[:7] + "..." + id[len(id)-4:]
}

func (e *Engine) refreshPrices(force bool) {
	e.mu.RLock()
	last := e.priceUpdated
	e.mu.RUnlock()
	if !force && !last.IsZero() && time.Since(last) < pricingInterval {
		return
	}
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", "https://models.dev/api.json", nil)
	req.Header.Set("User-Agent", "cpa-mini-usage/0.1")
	resp, err := client.Do(req)
	if err != nil {
		e.setPriceError("Price catalog fetch failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		e.setPriceError(fmt.Sprintf("Price catalog HTTP %d", resp.StatusCode))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil || len(raw) >= 32<<20 {
		e.setPriceError("Price catalog is too large or unreadable")
		return
	}
	prices, err := decodePrices(raw)
	if err != nil || len(prices) == 0 {
		e.setPriceError("Price catalog format was not recognized")
		return
	}
	now := time.Now().UTC()
	e.mu.Lock()
	e.prices = prices
	e.priceUpdated = now
	e.priceError = ""
	if e.persistence.Enabled && !e.storageReadFailed {
		b, _ := json.Marshal(map[string]any{"updated": now, "prices": prices})
		if err := writeAtomic(filepath.Join(e.dir, "prices.json"), b); err != nil {
			e.storageError = "Price catalog could not be persisted"
		}
	}
	e.mu.Unlock()
}
func (e *Engine) setPriceError(msg string) { e.mu.Lock(); e.priceError = msg; e.mu.Unlock() }
func decodePrices(raw []byte) (map[string]ModelPrice, error) {
	var data map[string]struct {
		Models map[string]struct {
			Cost struct {
				Input      *float64 `json:"input"`
				Output     *float64 `json:"output"`
				CacheRead  *float64 `json:"cache_read"`
				CacheWrite *float64 `json:"cache_write"`
				Tiers      []struct {
					Input      *float64 `json:"input"`
					Output     *float64 `json:"output"`
					CacheRead  *float64 `json:"cache_read"`
					CacheWrite *float64 `json:"cache_write"`
					Tier       struct {
						Type string `json:"type"`
						Size int64  `json:"size"`
					} `json:"tier"`
				} `json:"tiers"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	output := make(map[string]ModelPrice)
	for _, provider := range []string{"openai", "anthropic"} {
		for id, m := range data[provider].Models {
			if m.Cost.Input == nil || m.Cost.Output == nil {
				continue
			}
			price := ModelPrice{Input: *m.Cost.Input, Output: *m.Cost.Output, CacheRead: m.Cost.CacheRead, CacheWrite: m.Cost.CacheWrite}
			for _, tier := range m.Cost.Tiers {
				if tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
					continue
				}
				resolved := PriceTier{Threshold: tier.Tier.Size, Input: price.Input, Output: price.Output, CacheRead: price.CacheRead, CacheWrite: price.CacheWrite}
				if tier.Input != nil {
					resolved.Input = *tier.Input
				}
				if tier.Output != nil {
					resolved.Output = *tier.Output
				}
				if tier.CacheRead != nil {
					resolved.CacheRead = tier.CacheRead
				}
				if tier.CacheWrite != nil {
					resolved.CacheWrite = tier.CacheWrite
				}
				price.ContextTiers = append(price.ContextTiers, resolved)
			}
			sort.SliceStable(price.ContextTiers, func(i, j int) bool { return price.ContextTiers[i].Threshold < price.ContextTiers[j].Threshold })
			if len(price.ContextTiers) > 0 {
				price.ContextTier = &price.ContextTiers[0]
			}
			output[provider+"/"+normalizeModel(id)] = price
		}
	}
	if len(output) == 0 {
		return nil, errors.New("no supported models")
	}
	return output, nil
}

func (e *Engine) canonicalAuth(id string) string {
	if target, ok := e.aliases[id]; ok {
		return target
	}
	return id
}

// Keep provider-native input fields untouched for billing and old stored events.
func inputBreakdown(ev Event) (total, uncached int64) {
	if normalizeProvider(ev.Provider) == "claude" {
		return ev.Input + ev.CacheRead + ev.CacheWrite, ev.Input
	}
	read := min(ev.CacheRead, ev.Input)
	write := min(ev.CacheWrite, ev.Input-read)
	return ev.Input, ev.Input - read - write
}
