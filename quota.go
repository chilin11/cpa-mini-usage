package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

type AuthEntry struct {
	Index    string
	ID       string
	Name     string
	Provider string
	Disabled bool
}

func (e *Engine) kickFullRefresh() (string, bool) {
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
	go func() { defer e.refreshMu.Unlock(); e.refreshQuotaLocked(true) }()
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
		defer func() {
			e.mu.Lock()
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
			if !read(entry) {
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
	if len(auths) == 0 {
		e.quotaFailure("CPA returned no OAuth credential records")
		return
	}
	e.mu.Lock()
	e.quotaError = ""
	e.mu.Unlock()
	jobs := make(chan AuthEntry, len(auths))
	for _, entry := range auths {
		if quotaProvider(entry.Provider) {
			jobs <- entry
		}
	}
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
	if attempted == 0 {
		e.quotaFailure("No Claude, Codex or Antigravity OAuth credentials were found")
	} else if successful == 0 {
		e.quotaFailure("All OAuth quota reads failed; check the individual account messages")
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
	url := claudeUsageURL
	if entry.Provider == "codex" {
		url = "https://chatgpt.com/backend-api/wham/usage"
	}
	body, err := get(url, headers)
	if err != nil {
		e.recordQuotaError(entry, "Quota endpoint unavailable")
		return false
	}
	var five, week *QuotaWindow
	if entry.Provider == "claude" {
		five, week, err = parseClaudeQuota(body)
	} else {
		five, week, err = parseCodexQuota(body)
	}
	if err != nil {
		e.recordQuotaError(entry, "Quota response was not recognized")
		return false
	}
	observedAt := time.Now().UTC()
	var resetRemaining *int64
	var resetDiagnostic string
	if entry.Provider == "claude" {
		resetRemaining, resetDiagnostic = claudeResetRemaining(headers, get)
	} else if entry.Provider == "codex" {
		resetRemaining = codexResetRemaining(body, headers, get)
	}
	e.mu.Lock()
	e.quotaUpdated = observedAt
	e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	e.quotas[entry.ID] = AccountQuota{Provider: entry.Provider, AuthID: entry.ID, Name: entry.Name, Five: five, Week: week, FetchedAt: observedAt.Format(time.RFC3339Nano), ManualResetRemaining: resetRemaining, ManualResetDiagnostic: resetDiagnostic, Disabled: entry.Disabled}
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	if state, ok := e.accountRefresh[entry.ID]; ok {
		state.Error = ""
		e.accountRefresh[entry.ID] = state
	}
	e.persistQuotasLocked()
	e.mu.Unlock()
	return true
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
	e.quotaUpdated = observedAt
	e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	e.quotas[entry.ID] = AccountQuota{Provider: entry.Provider, AuthID: entry.ID, Name: entry.Name, Groups: groups, FetchedAt: observedAt.Format(time.RFC3339Nano), Disabled: entry.Disabled}
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
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
	prior.Error = message
	if state, ok := e.accountRefresh[a.ID]; ok && !state.Refreshing {
		state.Error = message
		e.accountRefresh[a.ID] = state
	}
	e.quotas[a.ID] = prior
	e.names[a.ID] = a.Name
	e.persistQuotasLocked()
}

func parseAuthList(v any) []AuthEntry {
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
		provider := normalizeProvider(str(r, "provider", "Provider", "type", "Type", "auth_type", "AuthType"))
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
		entries = append(entries, AuthEntry{Index: idx, ID: id, Name: name, Provider: provider, Disabled: disabled})
	}
	return entries
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
			return nil, fmt.Errorf("quota HTTP %d", code)
		}
	}
	// Optional inventory is a single host GET, with no direct-HTTP retry even if the
	// callback fails ambiguously. Legacy ordinary usage keeps its existing fallback.
	if url == claudeResetInventoryURL {
		return nil, fmt.Errorf("optional reset inventory unavailable")
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
		return nil, fmt.Errorf("quota HTTP %d", resp.StatusCode)
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
	// Public successful inventory read: pingdotgg/t3code#34226. This pinned
	// version can age out; it is not an eligibility or availability guarantee.
	claudeResetInventoryUserAgent = "claude-cli/2.1.321 (external, cli)"
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

// CodexBar's ClaudeOAuthUsageFetcher/ClaudeRateLimitResetCredits contract at
// 7447bc77c2497323d9c41e40164ce705533493c8, with a publicly tested newer CLI
// identity from https://github.com/pingdotgg/t3code/issues/34226. This optional,
// read-only request never replaces usage or sends a redemption handle anywhere.
func claudeResetRemaining(headers http.Header, get func(string, http.Header) ([]byte, error)) (*int64, string) {
	inventoryHeaders := headers.Clone()
	if inventoryHeaders == nil {
		inventoryHeaders = make(http.Header)
	}
	inventoryHeaders.Set("User-Agent", claudeResetInventoryUserAgent)
	inventoryHeaders.Set("Anthropic-Beta", "oauth-2025-04-20")
	inventoryHeaders.Set("Content-Type", "application/json")
	inventoryHeaders.Set("Cache-Control", "no-store")
	body, err := get(claudeResetInventoryURL, inventoryHeaders)
	if err != nil {
		return nil, manualResetRequestFailed
	}
	return parseClaudeResetRemaining(body, time.Now().UTC())
}

// Count saved inventory, NOT immediate redeemability (usable_now), automatic
// window resets, or extra-usage money. Fail closed on any malformed grant rather
// than reporting a partial total, especially a fabricated zero.
func parseClaudeResetRemaining(body []byte, now time.Time) (*int64, string) {
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
	var count int64
	for _, raw := range grants {
		var grant struct {
			Left     *int64  `json:"resets_left"`
			Total    *int64  `json:"resets_total"`
			Paused   *bool   `json:"paused"`
			StartsAt *string `json:"starts_at"`
			EndsAt   *string `json:"ends_at"`
		}
		if json.Unmarshal(raw, &grant) != nil || grant.Left == nil || grant.Paused == nil || *grant.Left < 0 {
			return nil, manualResetInvalidResponse
		}
		if grant.Total != nil && *grant.Total < *grant.Left {
			return nil, manualResetInvalidResponse
		}
		startsAt, startOK := parseClaudeResetBound(grant.StartsAt)
		endsAt, endOK := parseClaudeResetBound(grant.EndsAt)
		if !startOK || !endOK || (startsAt != nil && endsAt != nil && !endsAt.After(*startsAt)) {
			return nil, manualResetInvalidResponse
		}
		if *grant.Paused || (startsAt != nil && now.Before(*startsAt)) || (endsAt != nil && !now.Before(*endsAt)) {
			continue
		}
		// CodexBar's plausibility limits, without allocating one item per reset.
		if *grant.Left > 50-count {
			return nil, manualResetInvalidResponse
		}
		count += *grant.Left
	}
	return &count, ""
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
