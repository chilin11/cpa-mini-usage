package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	quotaInterval   = 5 * time.Minute
	pricingInterval = 6 * time.Hour
)

type Event struct {
	At         time.Time `json:"at"`
	AuthID     string    `json:"auth_id"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Input      int64     `json:"input"`
	Output     int64     `json:"output"`
	CacheRead  int64     `json:"cache_read"`
	CacheWrite int64     `json:"cache_write"`
	Total      int64     `json:"total"`
	Failed     bool      `json:"failed"`
}

type ModelPrice struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

type QuotaWindow struct {
	Used    float64 `json:"used"`
	ResetAt string  `json:"reset_at,omitempty"`
	Seconds int64   `json:"seconds,omitempty"`
}

type AccountQuota struct {
	Provider  string       `json:"provider"`
	AuthID    string       `json:"auth_id"`
	Name      string       `json:"name"`
	Five      *QuotaWindow `json:"five_hour,omitempty"`
	Week      *QuotaWindow `json:"seven_day,omitempty"`
	FetchedAt string       `json:"fetched_at"`
	Error     string       `json:"error,omitempty"`
}

type WindowEstimate struct {
	UsedPercent          float64  `json:"used_percent"`
	ResetAt              string   `json:"reset_at,omitempty"`
	ObservedTokens       int64    `json:"observed_tokens"`
	ObservedCost         *float64 `json:"observed_cost,omitempty"`
	EstimatedTotalTokens *float64 `json:"estimated_total_tokens,omitempty"`
	EstimatedTotalCost   *float64 `json:"estimated_total_cost,omitempty"`
	MissingPrice         bool     `json:"missing_price"`
	PartialHistory       bool     `json:"partial_history"`
}

type QuotaView struct {
	Provider  string          `json:"provider"`
	AuthID    string          `json:"auth_id"`
	Name      string          `json:"name"`
	Five      *WindowEstimate `json:"five_hour,omitempty"`
	Week      *WindowEstimate `json:"seven_day,omitempty"`
	FetchedAt string          `json:"fetched_at,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type ModelStat struct {
	Model    string   `json:"model"`
	Requests int64    `json:"requests"`
	Tokens   int64    `json:"tokens"`
	Cost     *float64 `json:"cost,omitempty"`
}

type CredentialView struct {
	AuthID       string      `json:"auth_id"`
	Name         string      `json:"name"`
	Provider     string      `json:"provider"`
	Requests     int64       `json:"requests"`
	Success      int64       `json:"success"`
	Failures     int64       `json:"failures"`
	Input        int64       `json:"input"`
	Output       int64       `json:"output"`
	Cached       int64       `json:"cached"`
	Total        int64       `json:"total"`
	Cost         *float64    `json:"cost,omitempty"`
	MissingPrice bool        `json:"missing_price"`
	Models       []ModelStat `json:"models"`
}

type Snapshot struct {
	Quotas         []QuotaView      `json:"quotas"`
	Credentials    []CredentialView `json:"credentials"`
	Recent         []Event          `json:"recent"`
	TotalRequests  int              `json:"total_requests"`
	PriceUpdatedAt string           `json:"price_updated_at,omitempty"`
	QuotaUpdatedAt string           `json:"quota_updated_at,omitempty"`
	StorageError   string           `json:"storage_error,omitempty"`
	PriceError     string           `json:"price_error,omitempty"`
	QuotaError     string           `json:"quota_error,omitempty"`
	GeneratedAt    string           `json:"generated_at"`
}

type Engine struct {
	mu           sync.RWMutex
	events       []Event
	quotas       map[string]AccountQuota
	names        map[string]string
	aliases      map[string]string
	prices       map[string]ModelPrice
	quotaUpdated time.Time
	priceUpdated time.Time
	priceError   string
	quotaError   string
	storageError string
	dir          string
	done         chan struct{}
	stopOnce     sync.Once
	refreshMu    sync.Mutex
	lastRefresh  time.Time
}

func newEngine() *Engine {
	dir := os.Getenv("CPA_MINI_USAGE_DATA_DIR")
	if dir == "" {
		dir = filepath.Join("data", "cpa-mini-usage")
	}
	return &Engine{quotas: make(map[string]AccountQuota), names: make(map[string]string), aliases: make(map[string]string), prices: make(map[string]ModelPrice), dir: dir, done: make(chan struct{})}
}

func (e *Engine) start() {
	e.load()
	go func() {
		time.Sleep(2 * time.Second)
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
		e.storageError = "Unable to create the data directory"
		return
	}
	path := filepath.Join(e.dir, "events.jsonl")
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		cutoff := time.Now().Add(-eventDays * 24 * time.Hour)
		for scanner.Scan() {
			var x Event
			if json.Unmarshal(scanner.Bytes(), &x) == nil && !x.At.Before(cutoff) {
				e.events = append(e.events, x)
			}
		}
		if scanner.Err() != nil {
			e.storageError = "Some historical records could not be read"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		e.storageError = "Unable to read historical usage"
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "prices.json"))
	if err == nil {
		var v struct {
			Updated time.Time             `json:"updated"`
			Prices  map[string]ModelPrice `json:"prices"`
		}
		if json.Unmarshal(b, &v) == nil && len(v.Prices) > 0 {
			e.prices = v.Prices
			e.priceUpdated = v.Updated
		}
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0600); err != nil {
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
	cached := number(detail, "CachedTokens", "cached_tokens", "CacheReadTokens", "cache_read_tokens")
	write := number(detail, "CacheCreationTokens", "cache_creation_tokens", "CacheWriteTokens", "cache_write_tokens")
	total := number(detail, "TotalTokens", "total_tokens")
	if total <= 0 {
		total = input + output + write
	}
	if input < 0 || output < 0 || cached < 0 || write < 0 || total < 0 {
		return
	}
	evt := Event{At: when, AuthID: authID, Provider: provider, Model: str(m, "Model", "model", "Alias", "alias"), Input: input, Output: output, CacheRead: cached, CacheWrite: write, Total: total, Failed: getAny(m, "Failed", "failed") == true}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, evt)
	if len(e.events) > 200000 {
		e.events = e.events[len(e.events)-200000:]
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
		if v > 0 {
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
	key := normalizeModel(ev.Model)
	p, ok := e.prices[priceProvider(ev.Provider)+"/"+key]
	if !ok {
		return 0, false
	}
	cached := ev.CacheRead
	if cached > ev.Input {
		cached = ev.Input
	}
	baseInput := ev.Input - cached
	cost := float64(baseInput)*p.Input/1e6 + float64(ev.Output)*p.Output/1e6
	if cached > 0 {
		if p.CacheRead != nil {
			cost += float64(cached) * (*p.CacheRead) / 1e6
		} else {
			cost += float64(cached) * p.Input / 1e6
		}
	}
	if ev.CacheWrite > 0 {
		if p.CacheWrite != nil {
			cost += float64(ev.CacheWrite) * (*p.CacheWrite) / 1e6
		} else {
			cost += float64(ev.CacheWrite) * p.Input / 1e6
		}
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

func (e *Engine) windowSummary(authID string, now time.Time, win *QuotaWindow, duration time.Duration) *WindowEstimate {
	if win == nil {
		return nil
	}
	out := &WindowEstimate{UsedPercent: win.Used, ResetAt: win.ResetAt}
	reset, err := time.Parse(time.RFC3339Nano, win.ResetAt)
	if err != nil {
		return out
	}
	if win.Seconds > 0 {
		duration = time.Duration(win.Seconds) * time.Second
	}
	if duration <= 0 {
		return out
	}
	since := reset.Add(-duration)
	if since.After(now) || reset.Before(now.Add(-5*time.Minute)) {
		return out
	}
	observedFirst := time.Time{}
	totalCost := 0.0
	for _, x := range e.events {
		if e.canonicalAuth(x.AuthID) != authID || x.At.Before(since) || x.At.After(now) {
			continue
		}
		if observedFirst.IsZero() || x.At.Before(observedFirst) {
			observedFirst = x.At
		}
		out.ObservedTokens += x.Total
		if c, ok := e.eventCost(x); ok {
			totalCost += c
		} else {
			out.MissingPrice = true
		}
	}
	if !observedFirst.IsZero() {
		// We cannot verify if usage before the first captured request was observed.
		out.PartialHistory = observedFirst.After(since.Add(10 * time.Minute))
		if !out.MissingPrice {
			out.ObservedCost = floatPtr(totalCost)
		}
		ratio := win.Used / 100
		if ratio >= 0.05 && ratio <= 1 && out.ObservedTokens > 0 {
			out.EstimatedTotalTokens = floatPtr(float64(out.ObservedTokens) / ratio)
			if !out.MissingPrice {
				out.EstimatedTotalCost = floatPtr(totalCost / ratio)
			}
		}
	}
	return out
}

func (e *Engine) snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	now := time.Now().UTC()
	result := Snapshot{Quotas: make([]QuotaView, 0, len(e.quotas)), Credentials: []CredentialView{}, Recent: []Event{}, PriceUpdatedAt: e.priceUpdated.Format(time.RFC3339), QuotaUpdatedAt: e.quotaUpdated.Format(time.RFC3339), PriceError: e.priceError, QuotaError: e.quotaError, StorageError: e.storageError, GeneratedAt: now.Format(time.RFC3339)}
	if e.priceUpdated.IsZero() {
		result.PriceUpdatedAt = ""
	}
	if e.quotaUpdated.IsZero() {
		result.QuotaUpdatedAt = ""
	}
	for _, q := range e.quotas {
		result.Quotas = append(result.Quotas, QuotaView{Provider: q.Provider, AuthID: q.AuthID, Name: q.Name, Five: e.windowSummary(q.AuthID, now, q.Five, 5*time.Hour), Week: e.windowSummary(q.AuthID, now, q.Week, 7*24*time.Hour), FetchedAt: q.FetchedAt, Error: q.Error})
	}
	sort.Slice(result.Quotas, func(i, j int) bool {
		if result.Quotas[i].Provider != result.Quotas[j].Provider {
			return result.Quotas[i].Provider < result.Quotas[j].Provider
		}
		return result.Quotas[i].Name < result.Quotas[j].Name
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
		if len(result.Recent) < 60 {
			result.Recent = append(result.Recent, ev)
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
		cost, ok := e.eventCost(ev)
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
		sort.Slice(s.Models, func(i, j int) bool { return s.Models[i].Tokens > s.Models[j].Tokens })
		result.Credentials = append(result.Credentials, *s)
	}
	sort.Slice(result.Credentials, func(i, j int) bool { return result.Credentials[i].Total > result.Credentials[j].Total })
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
	b, _ := json.Marshal(map[string]any{"updated": now, "prices": prices})
	if err := writeAtomic(filepath.Join(e.dir, "prices.json"), b); err != nil {
		e.storageError = "Price catalog could not be persisted"
	}
	e.mu.Unlock()
}
func (e *Engine) setPriceError(msg string) { e.mu.Lock(); e.priceError = msg; e.mu.Unlock() }
func decodePrices(raw []byte) (map[string]ModelPrice, error) {
	var data map[string]struct {
		Models map[string]struct {
			Cost struct {
				Input      float64  `json:"input"`
				Output     float64  `json:"output"`
				CacheRead  *float64 `json:"cache_read"`
				CacheWrite *float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	output := make(map[string]ModelPrice)
	for _, provider := range []string{"openai", "anthropic"} {
		x := data[provider]
		for id, m := range x.Models {
			if m.Cost.Input == 0 && m.Cost.Output == 0 {
				continue
			}
			output[provider+"/"+normalizeModel(id)] = ModelPrice{Input: m.Cost.Input, Output: m.Cost.Output, CacheRead: m.Cost.CacheRead, CacheWrite: m.Cost.CacheWrite}
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
