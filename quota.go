package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	allQuotaReadsFailed       = "All OAuth quota reads failed; check the individual account messages"
	quotaErrRateLimited       = "Quota endpoint rate limited"
	claudeCombinedFallbackTTL = 6 * time.Hour
	quotaRetryAfterMax        = 24 * time.Hour
)

var errOptionalResetInventoryUnavail = errors.New("optional reset inventory unavailable")

type quotaBackoffState struct {
	Until time.Time
	Count int
}

type quotaHTTPError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *quotaHTTPError) Error() string {
	return fmt.Sprintf("quota HTTP %d", e.StatusCode)
}

func providerQuotaInterval(provider string) time.Duration {
	if provider == "claude" {
		return claudeQuotaInterval
	}
	return quotaInterval
}

func quotaRateLimitBackoff(count int) time.Duration {
	switch {
	case count <= 1:
		return 30 * time.Minute
	case count == 2:
		return 60 * time.Minute
	default:
		return 120 * time.Minute
	}
}

func parseRetryAfterDuration(raw string, now time.Time) time.Duration {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		d := time.Duration(secs) * time.Second
		if d > quotaRetryAfterMax {
			return quotaRetryAfterMax
		}
		return d
	}
	if ts, err := http.ParseTime(value); err == nil && ts.After(now) {
		d := ts.Sub(now)
		if d > quotaRetryAfterMax {
			return quotaRetryAfterMax
		}
		return d
	}
	return 0
}

func headerRetryAfterFromMap(r map[string]any, now time.Time) time.Duration {
	hdrs, _ := getAny(r, "Headers", "headers", "Header", "header").(map[string]any)
	for k, v := range hdrs {
		if !strings.EqualFold(k, "Retry-After") {
			continue
		}
		switch typed := v.(type) {
		case string:
			if d := parseRetryAfterDuration(typed, now); d > 0 {
				return d
			}
		case []any:
			for _, item := range typed {
				if s, ok := item.(string); ok {
					if d := parseRetryAfterDuration(s, now); d > 0 {
						return d
					}
				}
			}
		case []string:
			for _, s := range typed {
				if d := parseRetryAfterDuration(s, now); d > 0 {
					return d
				}
			}
		}
	}
	return 0
}

func isQuotaRateLimitedErr(err error) (time.Duration, bool) {
	var httpErr *quotaHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusTooManyRequests {
		return httpErr.RetryAfter, true
	}
	return 0, false
}

func isClaudeCombinedIncompatibleErr(err error) bool {
	if errors.Is(err, errOptionalResetInventoryUnavail) {
		return true
	}
	var httpErr *quotaHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusUpgradeRequired:
			return true
		}
	}
	return false
}

func hasClaudeResetBlock(body []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return false
	}
	_, ok := m["cedar_ember"]
	return ok
}

func (e *Engine) isQuotaBackoffActive(authID string, now time.Time) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	b, ok := e.quotaBackoff[authID]
	return ok && now.Before(b.Until)
}

func (e *Engine) recordQuotaRateLimit(entry AuthEntry, retryAfter time.Duration, now time.Time) {
	e.mu.Lock()
	b := e.quotaBackoff[entry.ID]
	b.Count++
	delay := quotaRateLimitBackoff(b.Count)
	if retryAfter > delay {
		delay = retryAfter
	}
	b.Until = now.Add(delay)
	e.quotaBackoff[entry.ID] = b
	e.mu.Unlock()
	e.recordQuotaError(entry, quotaErrRateLimited)
}

func (e *Engine) shouldSkipBackgroundQuotaRefreshLocked(entry AuthEntry, now time.Time) bool {
	if b, ok := e.quotaBackoff[entry.ID]; ok && now.Before(b.Until) {
		return true
	}
	if entry.Provider == "claude" && entry.Disabled {
		return true
	}
	if interval := providerQuotaInterval(entry.Provider); interval > quotaInterval {
		if prior, ok := e.quotas[entry.ID]; ok && prior.Error == "" {
			if fetchedAt, valid := parseWindowResetTime(prior.FetchedAt); valid && now.Sub(fetchedAt) < interval {
				return true
			}
		}
	}
	return false
}

type AuthEntry struct {
	Index          string
	ID             string
	Name           string
	Provider       string
	RawProvider    string
	Disabled       bool
	DisableReason  string
	QuotaCooling   bool
	NextRetryAfter string
}

func (e *Engine) updateRoutingCooldownLocked(entry AuthEntry) {
	if entry.Disabled || !entry.QuotaCooling {
		delete(e.routingCooldown, entry.ID)
		return
	}
	var nextRetry time.Time
	if entry.NextRetryAfter != "" {
		if parsed, ok := parseWindowResetTime(entry.NextRetryAfter); ok {
			nextRetry = parsed.UTC()
		}
	}
	e.routingCooldown[entry.ID] = routingCooldownState{Cooling: true, NextRetryAfter: nextRetry}
}

func (e *Engine) kickFullRefresh() (string, bool) {
	return e.beginFullRefresh(hostRPC, e.refreshAccountQuota)
}

func (e *Engine) beginFullRefresh(rpc func(string, any) (any, error), read func(AuthEntry) bool) (string, bool) {
	if !e.refreshMu.TryLock() {
		return "", false
	}
	e.mu.Lock()
	previous := ""
	if !e.quotaRefreshFinished.IsZero() {
		previous = e.quotaRefreshFinished.Format(time.RFC3339Nano)
	}
	e.quotaRefreshing = true
	e.mu.Unlock()
	go func() { defer e.refreshMu.Unlock(); e.refreshQuotaWith(true, rpc, read) }()
	return previous, true
}

// Account refreshes share the provider-read lock, but never advance the full-scan clock.
func (e *Engine) kickAccountRefresh(authID string) (int, string, string) {
	return e.beginAccountRefresh(authID, hostRPC, e.refreshAccountQuota)
}

func (e *Engine) beginAccountRefresh(authID string, rpc func(string, any) (any, error), read func(AuthEntry) bool) (int, string, string) {
	if strings.TrimSpace(authID) == "" || len(authID) > 1024 {
		return 400, "", "invalid_auth_id"
	}
	if !e.refreshMu.TryRLock() {
		return 409, "", "refresh_busy"
	}
	e.mu.Lock()
	if _, known := e.quotas[authID]; !known {
		e.mu.Unlock()
		e.refreshMu.RUnlock()
		return 404, "", "account_not_found"
	}
	state := e.accountRefresh[authID]
	if state.Refreshing {
		e.mu.Unlock()
		e.refreshMu.RUnlock()
		return 409, "", "refresh_busy"
	}
	previous := state.FinishedAt
	e.accountRefresh[authID] = AccountRefreshState{Refreshing: true, FinishedAt: previous}
	e.mu.Unlock()
	go func() {
		message := ""
		succeeded := false
		defer func() {
			e.mu.Lock()
			if succeeded && e.quotaError == allQuotaReadsFailed {
				e.quotaError = ""
			}
			e.accountRefresh[authID] = AccountRefreshState{FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: message}
			e.refreshMu.RUnlock()
			e.mu.Unlock()
		}()
		raw, err := rpc("host.auth.list", map[string]any{})
		if err != nil {
			message = "CPA credential callback is unavailable"
			return
		}
		for _, entry := range parseAuthList(raw) {
			if entry.ID != authID || !quotaProvider(entry.Provider) {
				continue
			}
			succeeded = read(entry)
			if !succeeded {
				e.mu.RLock()
				message = e.quotas[authID].Error
				e.mu.RUnlock()
				if message == "" {
					message = "Quota endpoint unavailable"
				}
			}
			return
		}
		message = "account_not_found"
	}()
	return 202, previous, ""
}

func (e *Engine) refreshQuota(force bool) {
	if !e.refreshMu.TryLock() {
		return
	}
	defer e.refreshMu.Unlock()
	e.refreshQuotaLocked(force)
}

// Caller holds refreshMu exclusively for the whole batch, including all provider reads.
func (e *Engine) refreshQuotaLocked(force bool) {
	e.refreshQuotaWith(force, hostRPC, e.refreshAccountQuota)
}

// Injectable boundaries keep full-scan coverage/concurrency regression-testable.
func (e *Engine) refreshQuotaWith(force bool, rpc func(string, any) (any, error), read func(AuthEntry) bool) {
	e.mu.Lock()
	if !force && time.Since(e.lastRefresh) < quotaInterval {
		e.mu.Unlock()
		return
	}
	e.lastRefresh = time.Now()
	e.quotaRefreshing = true
	// Consume only discoveries made before this fresh host.auth.list call. A new
	// account observed while this batch runs stays pending for the next sync.
	e.authDiscoveryPending = false
	e.authListStarted++
	listVersion := e.authListStarted
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.quotaRefreshing = false
		e.quotaRefreshFinished = time.Now().UTC()
		e.mu.Unlock()
	}()
	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		e.quotaFailure("CPA credential callback is unavailable")
		return
	}
	auths := parseAuthList(raw)
	live, complete := completeAuthIDs(raw, auths)
	if len(auths) == 0 && !complete {
		e.quotaFailure("CPA returned no OAuth credential records")
		return
	}
	e.mu.Lock()
	if listVersion >= e.authListApplied {
		e.authListApplied = listVersion
		// The full batch already owns refreshMu exclusively.
		if complete && e.pruneQuotasLocked(live) {
			e.persistQuotasLocked()
		}
		for _, entry := range auths {
			if quotaProvider(entry.Provider) {
				e.updateRoutingCooldownLocked(entry)
			}
		}
	} else {
		// A newer sync completed during the list RPC. Don't prune its new cards
		// or introduce identities found only in our stale list. Its pending
		// discoveries still request their own fresh batch.
		known := auths[:0]
		for _, entry := range auths {
			if _, ok := e.quotas[entry.ID]; ok {
				known = append(known, entry)
			}
		}
		auths = known
	}
	e.quotaError = ""
	e.mu.Unlock()
	if len(auths) == 0 {
		return
	}
	now := time.Now().UTC()
	supported := 0
	jobs := make(chan AuthEntry, len(auths))
	e.mu.RLock()
	for _, entry := range auths {
		if !quotaProvider(entry.Provider) {
			continue
		}
		supported++
		if !force && e.shouldSkipBackgroundQuotaRefreshLocked(entry, now) {
			continue
		}
		jobs <- entry
	}
	e.mu.RUnlock()
	attempted := len(jobs)
	close(jobs)
	results := make(chan bool, attempted)
	var workers sync.WaitGroup
	for i := 0; i < 4 && i < attempted; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for entry := range jobs {
				select {
				case <-e.done:
					return
				default:
				}
				results <- read(entry)
			}
		}()
	}
	workers.Wait()
	close(results)
	successful := 0
	for ok := range results {
		if ok {
			successful++
		}
	}
	if supported == 0 {
		e.quotaFailure("No Claude, Codex or Antigravity OAuth credentials were found")
	} else if attempted > 0 && successful == 0 {
		e.quotaFailure(allQuotaReadsFailed)
	}
}

func quotaProvider(p string) bool { return p == "claude" || p == "codex" || p == "antigravity" }

func (e *Engine) refreshAccountQuota(entry AuthEntry) bool {
	return e.refreshAccountQuotaWith(entry, hostRPC, safeProviderGET)
}

func (e *Engine) refreshAccountQuotaWith(entry AuthEntry, rpc func(string, any) (any, error), get func(string, http.Header) ([]byte, error)) bool {
	e.mu.Lock()
	e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
	e.aliases[entry.Index] = entry.ID
	e.updateRoutingCooldownLocked(entry)
	e.mu.Unlock()
	headers, creds, email, failure := providerCredential(entry, rpc)
	if email != "" {
		entry.Name = email
	}
	if failure != "" {
		e.recordQuotaError(entry, failure)
		return false
	}
	if entry.Provider == "antigravity" {
		return e.refreshAntigravityQuota(entry, headers, creds, antigravityPOST)
	}
	now := time.Now().UTC()
	var five, week *QuotaWindow
	var resetRemaining *int64
	var resetDiagnostic string
	var observedAt time.Time
	if entry.Provider == "claude" {
		e.mu.RLock()
		combinedBlocked := now.Before(e.claudeCombinedBlockedUntil)
		e.mu.RUnlock()
		usePlain := combinedBlocked
		if !combinedBlocked {
			body, err := get(claudeResetInventoryURL, claudeResetHeaders(headers))
			if err != nil {
				if retryAfter, rateLimited := isQuotaRateLimitedErr(err); rateLimited {
					e.recordQuotaRateLimit(entry, retryAfter, now)
					return false
				}
				if isClaudeCombinedIncompatibleErr(err) {
					e.mu.Lock()
					e.claudeCombinedBlockedUntil = now.Add(claudeCombinedFallbackTTL)
					e.mu.Unlock()
					usePlain = true
				} else {
					e.recordQuotaError(entry, "Quota endpoint unavailable")
					return false
				}
			} else {
				five, week, err = parseClaudeQuota(body)
				if err != nil {
					if hasClaudeResetBlock(body) {
						e.mu.Lock()
						e.claudeCombinedBlockedUntil = now.Add(claudeCombinedFallbackTTL)
						e.mu.Unlock()
						usePlain = true
					} else {
						e.recordQuotaError(entry, "Quota response was not recognized")
						return false
					}
				} else {
					observedAt = time.Now().UTC()
					resetRemaining, resetDiagnostic = parseClaudeResetRemaining(body, observedAt)
				}
			}
		}
		if usePlain {
			body, err := get(claudeUsageURL, headers)
			if err != nil {
				if retryAfter, rateLimited := isQuotaRateLimitedErr(err); rateLimited {
					e.recordQuotaRateLimit(entry, retryAfter, now)
					return false
				}
				e.recordQuotaError(entry, "Quota endpoint unavailable")
				return false
			}
			five, week, err = parseClaudeQuota(body)
			if err != nil {
				e.recordQuotaError(entry, "Quota response was not recognized")
				return false
			}
			observedAt = time.Now().UTC()
			resetRemaining, resetDiagnostic = nil, manualResetRequestFailed
		}
	} else {
		body, err := get("https://chatgpt.com/backend-api/wham/usage", headers)
		if err != nil {
			if retryAfter, rateLimited := isQuotaRateLimitedErr(err); rateLimited {
				e.recordQuotaRateLimit(entry, retryAfter, now)
				return false
			}
			e.recordQuotaError(entry, "Quota endpoint unavailable")
			return false
		}
		five, week, err = parseCodexQuota(body)
		if err != nil {
			e.recordQuotaError(entry, "Quota response was not recognized")
			return false
		}
		observedAt = time.Now().UTC()
		resetRemaining = codexResetRemaining(body, headers, get)
	}
	e.mu.Lock()
	delete(e.quotaBackoff, entry.ID)
	e.quotaUpdated = observedAt
	e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
	prior, hadPrior := e.quotas[entry.ID]
	reconcileCooldown := shouldAutoClearResetCooldown(entry, hadPrior, prior, five, week, resetRemaining, observedAt)
	e.rememberQuotaForecastsLocked(prior, observedAt)
	e.quotas[entry.ID] = AccountQuota{Provider: entry.Provider, AuthID: entry.ID, Name: entry.Name, Five: five, Week: week, FetchedAt: observedAt.Format(time.RFC3339Nano), ManualResetRemaining: resetRemaining, ManualResetDiagnostic: resetDiagnostic, Disabled: entry.Disabled, DisableReason: sanitizeDisableReason(entry.Disabled, entry.DisableReason)}
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	e.updateWakeupObservedResetLocked(e.quotas[entry.ID])
	if state, ok := e.accountRefresh[entry.ID]; ok {
		state.Error = ""
		e.accountRefresh[entry.ID] = state
	}
	e.persistQuotasLocked()
	e.mu.Unlock()
	if reconcileCooldown {
		if cleared, _ := clearRoutingCooldown(entry.ID, entry.Provider, entry.Index, rpc); cleared {
			e.mu.Lock()
			delete(e.routingCooldown, entry.ID)
			delete(e.wakeupLastFail, entry.ID)
			e.mu.Unlock()
		}
	}
	return true
}

func isWindowExhaustedWithFutureReset(win *QuotaWindow, now time.Time) bool {
	if win == nil || win.Used < 100 {
		return false
	}
	resetAt, ok := parseWindowResetTime(win.ResetAt)
	return ok && resetAt.After(now)
}

// shouldAutoClearResetCooldown reconciles stale CPA routing cooldown when a fresh official
// quota read confirms an account reset (e.g. a reset card spent outside this plugin) while
// CPA still holds a routing cooldown. It requires positive reset evidence so short-term
// RPM/529/403 or model-specific cooldowns are never cleared accidentally.
func shouldAutoClearResetCooldown(entry AuthEntry, hadPrior bool, prior AccountQuota, five, week *QuotaWindow, resetRemaining *int64, observedAt time.Time) bool {
	if entry.Disabled || !entry.QuotaCooling || (entry.Provider != "codex" && entry.Provider != "claude") {
		return false
	}
	if !hadPrior || (five == nil && week == nil) {
		return false
	}
	if isQuotaWindowExhausted(five, observedAt) || isQuotaWindowExhausted(week, observedAt) {
		return false
	}
	if prior.ManualResetRemaining != nil && resetRemaining != nil && *resetRemaining < *prior.ManualResetRemaining {
		return true
	}
	if isWindowExhaustedWithFutureReset(prior.Five, observedAt) || isWindowExhaustedWithFutureReset(prior.Week, observedAt) {
		return true
	}
	return false
}

func (e *Engine) refreshAntigravityQuota(entry AuthEntry, headers http.Header, creds map[string]any, post func(string, http.Header, []byte) (int, []byte, error)) bool {
	projectID := antigravityProjectID(creds)
	if projectID == "" {
		e.recordQuotaError(entry, "Antigravity project ID unavailable")
		return false
	}
	groups, failure := fetchAntigravityQuota(headers, projectID, post)
	if failure != "" {
		e.recordQuotaError(entry, failure)
		return false
	}
	observedAt := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.quotaBackoff, entry.ID)
	e.quotaUpdated = observedAt
	e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	e.quotas[entry.ID] = AccountQuota{Provider: entry.Provider, AuthID: entry.ID, Name: entry.Name, Groups: groups, FetchedAt: observedAt.Format(time.RFC3339Nano), Disabled: entry.Disabled, DisableReason: sanitizeDisableReason(entry.Disabled, entry.DisableReason)}
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	e.updateWakeupObservedResetLocked(e.quotas[entry.ID])
	if state, ok := e.accountRefresh[entry.ID]; ok {
		state.Error = ""
		e.accountRefresh[entry.ID] = state
	}
	e.persistQuotasLocked()
	return true
}

// providerHeaders reads one CPA credential and builds the fixed OAuth headers for its provider.
// The access token never leaves the plugin; failures are stable, secret-free messages.
func providerHeaders(entry AuthEntry, rpc func(string, any) (any, error)) (http.Header, string, string) {
	headers, _, email, failure := providerCredential(entry, rpc)
	return headers, email, failure
}

func providerCredential(entry AuthEntry, rpc func(string, any) (any, error)) (http.Header, map[string]any, string, string) {
	authData, err := rpc("host.auth.get", map[string]string{"auth_index": entry.Index})
	if err != nil || authData == nil {
		authData, err = rpc("host.auth.get", map[string]string{"AuthIndex": entry.Index})
	}
	if err != nil {
		return nil, nil, "", "Credential could not be read"
	}
	creds := findAuthJSON(authData)
	email := ""
	if value := findString(creds, 0, "email", "Email"); strings.Contains(value, "@") {
		email = accountDisplayName(value)
	}
	token := findString(creds, 0, "access_token", "accessToken")
	if token == "" {
		return nil, creds, email, "OAuth access token unavailable"
	}
	if len(token) > 10000 {
		return nil, creds, email, "Invalid OAuth token"
	}
	headers := http.Header{"Authorization": []string{"Bearer " + token}, "Accept": []string{"application/json"}, "User-Agent": []string{"claude-code/2.1.0"}, "Anthropic-Beta": []string{"oauth-2025-04-20"}}
	if entry.Provider == "antigravity" {
		headers = http.Header{"Authorization": []string{"Bearer " + token}, "Accept": []string{"application/json"}, "Content-Type": []string{"application/json"}, "User-Agent": []string{antigravityUserAgent}}
	}
	if entry.Provider == "codex" {
		headers.Set("User-Agent", "codex-cli/0.1")
		id := findString(creds, 0, "chatgpt_account_id", "chatgptAccountId", "account_id")
		if id == "" {
			id = extractJWTAccountID(findString(creds, 0, "id_token", "idToken"))
		}
		if id != "" {
			headers.Set("Chatgpt-Account-Id", id)
		}
	}
	return headers, creds, email, ""
}

func (e *Engine) quotaFailure(message string) { e.mu.Lock(); e.quotaError = message; e.mu.Unlock() }
func (e *Engine) recordQuotaError(a AuthEntry, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prior := e.quotas[a.ID]
	prior.AuthID = a.ID
	prior.Provider = a.Provider
	prior.Name = a.Name
	prior.Disabled = a.Disabled
	prior.DisableReason = sanitizeDisableReason(a.Disabled, a.DisableReason)
	prior.Error = message
	if state, ok := e.accountRefresh[a.ID]; ok && !state.Refreshing {
		state.Error = message
		e.accountRefresh[a.ID] = state
	}
	e.quotas[a.ID] = prior
	e.names[a.ID] = a.Name
	e.persistQuotasLocked()
}

// A legacy/extended wire envelope can provide identities but cannot prove absence.
type discoveryOnlyAuthList struct{ result any }

func parseAuthList(v any) []AuthEntry {
	if compat, ok := v.(discoveryOnlyAuthList); ok {
		v = compat.result
	}
	records := []any{}
	switch x := v.(type) {
	case []any:
		records = x
	case map[string]any:
		for _, k := range []string{"files", "Files", "auths", "Auths", "records", "Records", "credentials", "Credentials", "items", "Items"} {
			if arr, ok := x[k].([]any); ok {
				records = arr
				break
			}
		}
	}
	var entries []AuthEntry
	for _, raw := range records {
		r, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		rawProvider := strings.ToLower(strings.TrimSpace(str(r, "provider", "Provider", "type", "Type", "auth_type", "AuthType")))
		provider := normalizeProvider(rawProvider)
		name := str(r, "email", "Email")
		if name == "" {
			name = str(r, "name", "Name", "file_name", "fileName", "filename", "Filename")
		}
		if provider == "" {
			provider = normalizeProvider(name)
		}
		idx := str(r, "auth_index", "authIndex", "AuthIndex", "index", "Index")
		if idx == "" {
			continue
		}
		id := str(r, "id", "ID", "auth_id", "authId", "AuthID")
		if id == "" {
			id = idx
		}
		if name == "" {
			name = shortID(id)
		}
		name = accountDisplayName(name)
		disabled, _ := getAny(r, "disabled", "Disabled").(bool)
		if strings.EqualFold(str(r, "status", "Status"), "disabled") {
			disabled = true
		}
		statusMsg := str(r, "status_message", "StatusMessage")
		disableReason := sanitizeDisableReason(disabled, statusMsg)
		now := time.Now().UTC()
		unavail, _ := getAny(r, "unavailable", "Unavailable").(bool)
		var retryAt time.Time
		hasRetry := false
		if retryStr := str(r, "next_retry_after", "NextRetryAfter"); retryStr != "" {
			if parsed, ok := parseWindowResetTime(retryStr); ok {
				retryAt, hasRetry = parsed.UTC(), true
			}
		}
		cooling := false
		nextRetryAfter := ""
		if !disabled {
			if hasRetry {
				if retryAt.After(now) {
					cooling = true
					nextRetryAfter = retryAt.Format(time.RFC3339)
				}
			} else {
				if unavail && !isAuthFailureStatus(statusMsg) {
					cooling = true
				}
				if isQuotaCooldownStatus(statusMsg) {
					cooling = true
				}
			}
		}
		entries = append(entries, AuthEntry{Index: idx, ID: id, Name: name, Provider: provider, RawProvider: rawProvider, Disabled: disabled, DisableReason: disableReason, QuotaCooling: cooling, NextRetryAfter: nextRetryAfter})
	}
	return entries
}

func isQuotaCooldownStatus(statusMsg string) bool {
	msg := strings.ToLower(strings.TrimSpace(statusMsg))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota") ||
		strings.Contains(msg, "rate limit") || strings.Contains(msg, "rate_limit") ||
		strings.Contains(msg, "cooldown") || strings.Contains(msg, "exhausted") ||
		strings.Contains(msg, "usage limit")
}

func isAuthFailureStatus(statusMsg string) bool {
	msg := strings.ToLower(strings.TrimSpace(statusMsg))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "refresh_token_reused") || strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "sign in") || strings.Contains(msg, "sign-in") ||
		strings.Contains(msg, "401")
}

// The native, unpaginated list response contains *only* files. Keep alternate
// envelopes, future pagination metadata and disk-only/partial records useful for
// discovery, but never infer absence from them. Compare every identity, including
// unsupported providers, so that filtering quota jobs cannot imply deletion.
func completeAuthIDs(raw any, entries []AuthEntry) (map[string]bool, bool) {
	envelope, ok := raw.(map[string]any)
	files, array := envelope["files"].([]any)
	if !ok || len(envelope) != 1 || !array || files == nil || len(entries) != len(files) {
		return nil, false
	}
	ids, indices := make(map[string]bool, len(files)), make(map[string]bool, len(files))
	for i, file := range files {
		row, _ := file.(map[string]any)
		id, _ := row["id"].(string)
		idx, _ := row["auth_index"].(string)
		if id == "" || idx == "" || strings.TrimSpace(id) != id || strings.TrimSpace(idx) != idx ||
			id != entries[i].ID || idx != entries[i].Index || ids[id] || indices[idx] {
			return nil, false
		}
		// Do not authorize pruning on provider/name inference or conflicting fields.
		provider := ""
		for _, key := range []string{"provider", "type"} {
			if value, present := row[key]; present {
				p, ok := value.(string)
				if !ok || strings.TrimSpace(p) == "" || (provider != "" && provider != p) {
					return nil, false
				}
				provider = p
			}
		}
		if provider == "" || normalizeProvider(provider) != entries[i].Provider {
			return nil, false
		}
		for _, key := range []string{"name", "email", "status"} {
			if v, exists := row[key]; exists {
				if _, ok := v.(string); !ok {
					return nil, false
				}
			}
		}
		if v, exists := row["disabled"]; exists {
			if _, ok := v.(bool); !ok {
				return nil, false
			}
		}
		ids[id], indices[idx] = true, true
	}
	return ids, true
}

// Caller holds mu and exclusive refreshMu: no obsolete quota/error/refresh-state
// writes can finish after a deletion. Names, index aliases, usage and forecasts
// remain available for historical requests.
func (e *Engine) pruneQuotasLocked(live map[string]bool) bool {
	removed := false
	for id := range e.quotas {
		if !live[id] {
			delete(e.quotas, id)
			delete(e.quotaBackoff, id)
			delete(e.routingCooldown, id)
			e.removeResetBindingsLocked(id)
			delete(e.accountRefresh, id)
			delete(e.wakeupResults, id)
			delete(e.wakeupLastFail, id)
			removed = true
		}
	}
	if len(e.quotas) == 0 {
		e.authDiscoveryPending = false
		e.quotaError = ""
	}
	return removed
}

func accountDisplayName(s string) string {
	s = strings.TrimSpace(s)
	if slash := strings.LastIndexAny(s, "/\\"); slash >= 0 {
		s = s[slash+1:]
	}
	if strings.HasSuffix(strings.ToLower(s), ".json") {
		s = s[:len(s)-5]
	}
	return s
}
func findAuthJSON(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"JSON", "json", "content", "Content", "storage_json", "storageJSON", "StorageJSON", "data", "credential", "Credential", "auth"} {
			value := x[key]
			if value == nil {
				continue
			}
			switch nested := value.(type) {
			case map[string]any:
				if len(nested) > 0 {
					return findAuthJSON(nested)
				}
			case string:
				var m map[string]any
				if json.Unmarshal([]byte(nested), &m) == nil {
					return m
				}
				if body, err := base64.StdEncoding.DecodeString(nested); err == nil {
					if json.Unmarshal(body, &m) == nil {
						return m
					}
				}
			}
		}
		return x
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(x), &m) == nil {
			return m
		}
	}
	return nil
}
func findString(m map[string]any, depth int, names ...string) string {
	if m == nil || depth > 5 {
		return ""
	}
	for _, name := range names {
		if value, ok := m[name].(string); ok && value != "" {
			return value
		}
	}
	for _, k := range []string{"claudeAiOauth", "oauth", "OAuth", "tokens", "token", "auth", "credentials", "metadata", "Metadata", "attributes", "Attributes"} {
		if nested, ok := m[k].(map[string]any); ok {
			if value := findString(nested, depth+1, names...); value != "" {
				return value
			}
		}
	}
	return ""
}
func extractJWTAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	if s := str(m, "chatgpt_account_id"); s != "" {
		return s
	}
	if nested, ok := m["https://api.openai.com/auth"].(map[string]any); ok {
		return str(nested, "chatgpt_account_id")
	}
	return ""
}

func safeProviderGET(url string, headers http.Header) ([]byte, error) {
	return safeProviderGETWithRPC(url, headers, hostRPC)
}

func safeProviderGETWithRPC(url string, headers http.Header, rpc func(string, any) (any, error)) ([]byte, error) {
	// Exact URLs only: optional inventory must not broaden the credential allow-list.
	if url != claudeUsageURL && url != claudeResetInventoryURL && url != "https://chatgpt.com/backend-api/wham/usage" && url != codexResetCreditsURL {
		return nil, fmt.Errorf("endpoint not permitted")
	}
	request := map[string]any{"Method": "GET", "URL": url, "Headers": headers, "Header": headers}
	data, err := rpc("host.http.do", request)
	if err == nil {
		if r, ok := data.(map[string]any); ok {
			code := number(r, "StatusCode", "status_code", "statusCode")
			if code >= 200 && code < 300 {
				raw := str(r, "Body", "body")
				if decoded, decErr := base64.StdEncoding.DecodeString(raw); decErr == nil {
					if len(decoded) > 512*1024 {
						return nil, fmt.Errorf("quota response too large")
					}
					return decoded, nil
				}
				if len(raw) > 512*1024 {
					return nil, fmt.Errorf("quota response too large")
				}
				return []byte(raw), nil
			}
			return nil, &quotaHTTPError{StatusCode: int(code), RetryAfter: headerRetryAfterFromMap(r, time.Now().UTC())}
		}
	}
	// Optional inventory is a single host GET, with no direct-HTTP retry even if the
	// callback fails ambiguously. Legacy ordinary usage keeps its existing fallback.
	if url == claudeResetInventoryURL {
		return nil, errOptionalResetInventoryUnavail
	}
	// Legacy hosts may not implement host.http.do; fixed HTTPS fallback.
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota endpoint failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &quotaHTTPError{StatusCode: resp.StatusCode, RetryAfter: parseRetryAfterDuration(resp.Header.Get("Retry-After"), time.Now().UTC())}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024+1))
	if err != nil || len(body) > 512*1024 {
		return nil, fmt.Errorf("quota response invalid")
	}
	return body, nil
}
func parseClaudeQuota(body []byte) (*QuotaWindow, *QuotaWindow, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, err
	}
	five := parseClaudeWindow(m["five_hour"])
	week := parseClaudeWindow(m["seven_day"])
	if five == nil && week == nil {
		return nil, nil, fmt.Errorf("missing quota windows")
	}
	return five, week, nil
}
func parseClaudeWindow(v any) *QuotaWindow {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	util, valid := f64(getAny(m, "utilization", "used_percent"))
	if !valid {
		return nil
	}
	if util < 0 || util > 100 {
		return nil
	}
	return &QuotaWindow{Used: util, ResetAt: str(m, "resets_at", "reset_at")}
}
func parseCodexQuota(body []byte) (*QuotaWindow, *QuotaWindow, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, err
	}
	rate, ok := getAny(m, "rate_limit", "rateLimit").(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("missing rate_limit")
	}
	primary := parseCodexWindow(getAny(rate, "primary_window", "primaryWindow"))
	secondary := parseCodexWindow(getAny(rate, "secondary_window", "secondaryWindow"))
	five, week := (*QuotaWindow)(nil), (*QuotaWindow)(nil)
	for _, w := range []*QuotaWindow{primary, secondary} {
		if w == nil {
			continue
		}
		if w.Seconds >= 4*3600 && w.Seconds <= 6*3600 {
			five = w
		}
		if w.Seconds >= 6*24*3600 && w.Seconds <= 8*24*3600 {
			week = w
		}
	}
	if primary != nil && five == nil && primary.Seconds == 0 {
		five = primary
	}
	if secondary != nil && week == nil && secondary.Seconds == 0 {
		week = secondary
	}
	if five == nil && week == nil {
		return nil, nil, fmt.Errorf("quota windows missing")
	}
	return five, week, nil
}
func parseCodexWindow(v any) *QuotaWindow {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	util, valid := f64(getAny(m, "used_percent", "usedPercent"))
	if !valid || util < 0 || util > 100 {
		return nil
	}
	seconds := number(m, "limit_window_seconds", "limitWindowSeconds")
	reset := str(m, "reset_at", "resetAt")
	if reset == "" {
		if unix := number(m, "reset_at", "resetAt"); unix > 0 {
			reset = time.Unix(unix, 0).UTC().Format(time.RFC3339)
		}
	}
	if reset == "" {
		if after := number(m, "reset_after_seconds", "resetAfterSeconds"); after > 0 {
			reset = time.Now().Add(time.Duration(after) * time.Second).UTC().Format(time.RFC3339)
		}
	}
	return &QuotaWindow{Used: util, ResetAt: reset, Seconds: seconds}
}

// Only explicit remaining-count fields qualify. No inference from credits,
// reset timestamps, totals, or used counts; unsupported responses stay unknown.
func parseManualResetRemaining(body []byte) *int64 {
	var response map[string]any
	if json.Unmarshal(body, &response) != nil {
		return nil
	}
	count := func(value any) *int64 {
		n, ok := value.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9007199254740991 || math.Trunc(n) != n {
			return nil
		}
		v := int64(n)
		return &v
	}
	// Official Codex usage response (openai/codex backend-client/src/types.rs).
	if credits, ok := response["rate_limit_reset_credits"].(map[string]any); ok {
		if value, present := credits["available_count"]; present {
			return count(value)
		}
	}
	for _, key := range []string{"manual_reset_remaining", "manual_resets_remaining", "remaining_manual_resets"} {
		if value, present := response[key]; present {
			return count(value)
		}
	}
	for _, key := range []string{"manual_reset", "manual_resets", "reset_bank"} {
		if bank, ok := response[key].(map[string]any); ok {
			for _, field := range []string{"remaining", "remaining_count", "remaining_resets"} {
				if value, present := bank[field]; present {
					return count(value)
				}
			}
		}
	}
	return nil
}

const (
	claudeUsageURL          = "https://api.anthropic.com/api/oauth/usage"
	claudeResetInventoryURL = claudeUsageURL + "?cedar_ember=1"
	// Pinned CLI contract, not an eligibility or availability guarantee.
	claudeResetInventoryUserAgent = "claude-cli/2.1.331 (external, cli)"
	codexResetCreditsURL          = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

const (
	manualResetRequestFailed   = "request_failed"
	manualResetCLIVersion      = "cli_version"
	manualResetSurface         = "surface"
	manualResetNotEligible     = "not_eligible"
	manualResetUnavailable     = "unavailable"
	manualResetInvalidResponse = "invalid_response"
)

func sanitizeManualResetDiagnostic(provider string, remaining *int64, diagnostic string) string {
	if provider != "claude" || remaining != nil {
		return ""
	}
	switch diagnostic {
	case manualResetRequestFailed, manualResetCLIVersion, manualResetSurface,
		manualResetNotEligible, manualResetUnavailable, manualResetInvalidResponse:
		return diagnostic
	default:
		return manualResetUnavailable
	}
}

func claudeResetHeaders(headers http.Header) http.Header {
	inventoryHeaders := headers.Clone()
	if inventoryHeaders == nil {
		inventoryHeaders = make(http.Header)
	}
	inventoryHeaders.Set("User-Agent", claudeResetInventoryUserAgent)
	inventoryHeaders.Set("Anthropic-Beta", "oauth-2025-04-20")
	inventoryHeaders.Set("Content-Type", "application/json")
	inventoryHeaders.Set("Cache-Control", "no-store")
	inventoryHeaders.Set("Accept", "application/json")
	return inventoryHeaders
}

func claudeResetRemaining(headers http.Header, get func(string, http.Header) ([]byte, error)) (*int64, string) {
	body, err := get(claudeResetInventoryURL, claudeResetHeaders(headers))
	if err != nil {
		return nil, manualResetRequestFailed
	}
	return parseClaudeResetRemaining(body, time.Now().UTC())
}

// Count saved inventory, NOT immediate redeemability (usable_now), automatic
// window resets, or extra-usage money. Fail closed on any malformed grant rather
// than reporting a partial total, especially a fabricated zero.
func parseClaudeResetRemaining(body []byte, now time.Time) (*int64, string) {
	inventory, diagnostic := decodeClaudeInventory(body)
	if diagnostic != "" {
		return nil, diagnostic
	}
	var count int64
	for _, raw := range inventory.Grants {
		var g claudeGrantBalance
		if json.Unmarshal(raw, &g) != nil {
			return nil, manualResetInvalidResponse
		}
		left, _, ok := g.active(now)
		if !ok || left > 50-count {
			return nil, manualResetInvalidResponse
		}
		count += left
	}
	return &count, ""
}

type claudeInventory struct {
	Grants []json.RawMessage
	block  json.RawMessage // Decoded further only for an explicit list request.
}

func decodeClaudeInventory(body []byte) (*claudeInventory, string) {
	var response map[string]json.RawMessage
	if json.Unmarshal(body, &response) != nil || response == nil {
		return nil, manualResetInvalidResponse
	}
	raw, present := response["cedar_ember"]
	if !present || string(raw) == "null" {
		return nil, manualResetUnavailable
	}
	var inventory struct {
		Eligible         *bool           `json:"eligible"`
		IneligibleReason json.RawMessage `json:"ineligible_reason"`
		Grants           json.RawMessage `json:"grants"`
	}
	if json.Unmarshal(raw, &inventory) != nil || inventory.Eligible == nil {
		return nil, manualResetInvalidResponse
	}
	if !*inventory.Eligible {
		var reason string
		// Only exact known reasons are exposed. Unknown/malformed reasons are
		// deliberately collapsed; never retain upstream text or credential data.
		_ = json.Unmarshal(inventory.IneligibleReason, &reason)
		switch reason {
		case "cli_version":
			return nil, manualResetCLIVersion
		case "surface":
			return nil, manualResetSurface
		default:
			return nil, manualResetNotEligible
		}
	}
	var grants []json.RawMessage
	if json.Unmarshal(inventory.Grants, &grants) != nil || grants == nil || len(grants) > 200 {
		return nil, manualResetInvalidResponse
	}
	return &claudeInventory{grants, raw}, ""
}

type claudeGrantBalance struct {
	Left     *int64  `json:"resets_left"`
	Total    *int64  `json:"resets_total"`
	Paused   *bool   `json:"paused"`
	StartsAt *string `json:"starts_at"`
	EndsAt   *string `json:"ends_at"`
}

func (g claudeGrantBalance) active(now time.Time) (int64, *time.Time, bool) {
	if g.Left == nil || *g.Left < 0 || g.Paused == nil || (g.Total != nil && *g.Total < *g.Left) {
		return 0, nil, false
	}
	start, startOK := parseClaudeResetBound(g.StartsAt)
	end, endOK := parseClaudeResetBound(g.EndsAt)
	if !startOK || !endOK || (start != nil && end != nil && !end.After(*start)) {
		return 0, nil, false
	}
	if *g.Paused || (start != nil && now.Before(*start)) || (end != nil && !now.Before(*end)) {
		return 0, nil, true
	}
	return *g.Left, end, true
}

func parseClaudeResetBound(value *string) (*time.Time, bool) {
	if value == nil {
		return nil, true // Missing/null bounds are open; unreadable ones are not.
	}
	bound, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return nil, false
	}
	return &bound, true
}

// GET-only details fallback used by the official Codex client. Spending a credit is only
// done by the explicit, confirmed consume-reset route in account_actions.go.
func codexResetRemaining(usage []byte, headers http.Header, get func(string, http.Header) ([]byte, error)) *int64 {
	if count := parseManualResetRemaining(usage); count != nil {
		return count
	}
	details, err := get(codexResetCreditsURL, headers)
	if err != nil {
		return nil
	} // Missing optional reset data must not break quotas.
	var response map[string]json.RawMessage
	if json.Unmarshal(details, &response) != nil {
		return nil
	}
	value, present := response["available_count"]
	if !present {
		return nil
	}
	// Reuse strict integer validation without accepting ordinary usage credits.
	wrapped, _ := json.Marshal(map[string]any{"rate_limit_reset_credits": map[string]any{"available_count": value}})
	return parseManualResetRemaining(wrapped)
}
