package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxHistoryRecords = 200000
const historyCursorTTL = 15 * time.Minute

// Start is inclusive, End exclusive. The browser supplies local midnight instants
// including their actual UTC offsets (days around DST need not have 24 hours).
type historyQuery struct {
	Start, End            time.Time
	Provider, ModelStatus string
	Limit                 int
	Cursor                string
}

type HistoryRetention struct {
	Days            int        `json:"days"`
	MaxRecords      int        `json:"max_records"`
	RetainedRecords int        `json:"retained_records"`
	OldestAt        *time.Time `json:"oldest_at,omitempty"`
	NewestAt        *time.Time `json:"newest_at,omitempty"`
}

type HistoryPage struct {
	Events      []RecentEvent    `json:"events"`
	Total       int              `json:"total"`
	Offset      int              `json:"offset"`
	Limit       int              `json:"limit"`
	Cursor      string           `json:"cursor"`
	NextCursor  string           `json:"next_cursor,omitempty"`
	GeneratedAt time.Time        `json:"generated_at"`
	Retention   HistoryRetention `json:"retention"`
}

// Signed and query-bound: clients cannot select arbitrary offsets, change filters
// between pages or reuse a cursor across process restarts. Appends are excluded by
// MaxSequence; removals invalidate the cursor instead of silently skipping rows.
type historyCursor struct {
	Query       string    `json:"q"`
	Offset      int       `json:"o"`
	MaxSequence uint64    `json:"s"`
	Revision    uint64    `json:"r"`
	AsOf        time.Time `json:"t"`
}

func parseHistoryQuery(values url.Values) (historyQuery, error) {
	q := historyQuery{Limit: 20}
	bad := errors.New("invalid_history_query")
	for key, values := range values {
		if len(values) != 1 || len(values[0]) > 2048 {
			return q, bad
		}
		switch key {
		case "start", "end", "provider", "model_status", "limit", "cursor":
		default:
			return q, bad
		}
	}
	var err error
	q.Start, err = time.Parse(time.RFC3339Nano, values.Get("start"))
	if err != nil {
		return q, bad
	}
	q.End, err = time.Parse(time.RFC3339Nano, values.Get("end"))
	if err != nil {
		return q, bad
	}
	if !q.Start.Before(q.End) || q.End.Sub(q.Start) > (eventDays+2)*24*time.Hour {
		return q, bad
	}
	q.Provider = values.Get("provider")
	// Provider ids may be custom CPA executors; accept bounded identifiers, not a
	// hard-coded list which would make old/custom provider records inaccessible.
	if len(q.Provider) > 64 || strings.IndexFunc(q.Provider, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) >= 0 {
		return q, bad
	}
	q.ModelStatus = values.Get("model_status")
	switch q.ModelStatus {
	case "", "same", "variant", "different", "unknown":
	default:
		return q, bad
	}
	if s := values.Get("limit"); s != "" {
		q.Limit, err = strconv.Atoi(s)
		if err != nil || q.Limit < 1 || q.Limit > 60 {
			return q, bad
		}
	}
	q.Cursor = values.Get("cursor")
	return q, nil
}

func (q historyQuery) fingerprint() string {
	body, _ := json.Marshal([]any{q.Start.UTC(), q.End.UTC(), q.Provider, q.ModelStatus, q.Limit})
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (e *Engine) signHistoryCursor(c historyCursor) string {
	body, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, e.historyKey[:])
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (e *Engine) decodeHistoryCursor(token string, c *historyCursor) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(token) > 2048 {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, e.historyKey[:])
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil)) && json.Unmarshal(body, c) == nil
}

// Read-only and bounded to the retained 200k events. Only page rows are valued.
// No host/OAuth RPC, filesystem IO or forecast mutation is performed here.
func (e *Engine) history(q historyQuery) (HistoryPage, int, string) {
	e.historyKeyOnce.Do(func() { _, e.historyKeyErr = rand.Read(e.historyKey[:]) })
	if e.historyKeyErr != nil {
		return HistoryPage{}, 500, "history_unavailable"
	}
	now := time.Now().UTC()
	e.mu.RLock()
	defer e.mu.RUnlock()
	c := historyCursor{Query: q.fingerprint(), MaxSequence: e.eventSequence, Revision: e.historyRevision, AsOf: now}
	if q.Cursor != "" {
		if !e.decodeHistoryCursor(q.Cursor, &c) || c.Query != q.fingerprint() || c.Offset < 0 || c.Offset > maxHistoryRecords {
			return HistoryPage{}, 400, "invalid_history_cursor"
		}
		if now.Sub(c.AsOf) > historyCursorTTL || c.AsOf.After(now) || c.Revision != e.historyRevision {
			return HistoryPage{}, 409, "history_cursor_expired"
		}
	}
	page := HistoryPage{Events: []RecentEvent{}, Offset: c.Offset, Limit: q.Limit, GeneratedAt: c.AsOf, Retention: HistoryRetention{Days: eventDays, MaxRecords: maxHistoryRecords}}
	cutoff := c.AsOf.Add(-eventDays * 24 * time.Hour)
	matches := make([]*Event, 0)
	for i := range e.events {
		ev := &e.events[i]
		// Time-based expiry can remove eligibility even before disk compaction.
		// Reject the anchored page rather than shifting its offset or exposing an
		// event which has just aged past the retention boundary.
		if q.Cursor != "" && ev.sequence <= c.MaxSequence && !ev.At.Before(cutoff) && ev.At.Before(now.Add(-eventDays*24*time.Hour)) {
			return HistoryPage{}, 409, "history_cursor_expired"
		}
		if ev.sequence > c.MaxSequence || ev.At.Before(cutoff) || ev.At.After(c.AsOf) {
			continue
		}
		page.Retention.RetainedRecords++
		if page.Retention.OldestAt == nil || ev.At.Before(*page.Retention.OldestAt) {
			page.Retention.OldestAt = &ev.At
		}
		if page.Retention.NewestAt == nil || ev.At.After(*page.Retention.NewestAt) {
			page.Retention.NewestAt = &ev.At
		}
		if ev.At.Before(q.Start) || !ev.At.Before(q.End) || q.Provider != "" && ev.Provider != q.Provider || q.ModelStatus != "" && modelObservationStatus(*ev) != q.ModelStatus {
			continue
		}
		matches = append(matches, ev)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].At.Equal(matches[j].At) {
			return matches[i].sequence > matches[j].sequence
		}
		return matches[i].At.After(matches[j].At)
	})
	page.Total = len(matches)
	if c.Offset > len(matches) {
		return HistoryPage{}, 409, "history_cursor_expired"
	}
	end := c.Offset + q.Limit
	if end > len(matches) {
		end = len(matches)
	}
	for _, ev := range matches[c.Offset:end] {
		cost, ok := e.eventCost(*ev)
		page.Events = append(page.Events, e.recentEventLocked(*ev, cost, ok))
	}
	page.Cursor = e.signHistoryCursor(c) // Replay this page, including page one, at the same anchor.
	if end < len(matches) {
		c.Offset = end
		page.NextCursor = e.signHistoryCursor(c)
	}
	// Do not let returned pointers alias the mutable backing event slice.
	if page.Retention.OldestAt != nil {
		v := *page.Retention.OldestAt
		page.Retention.OldestAt = &v
	}
	if page.Retention.NewestAt != nil {
		v := *page.Retention.NewestAt
		page.Retention.NewestAt = &v
	}
	return page, 200, ""
}

func (e *Engine) recentEventLocked(ev Event, cost float64, ok bool) RecentEvent {
	recent := RecentEvent{Event: ev, MissingPrice: !ok, UsageQuality: usageQuality(ev), ModelStatus: modelObservationStatus(ev)}
	recent.AuthID = e.canonicalAuth(ev.AuthID)
	recent.CredentialName = e.names[recent.AuthID]
	recent.Output = billableOutput(ev) // Never rewrite raw persisted token counts.
	if ok {
		recent.Cost = floatPtr(cost)
	}
	return recent
}

func observedReasoningEffort(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}
