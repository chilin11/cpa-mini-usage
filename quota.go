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
			if entry.ID != authID || (entry.Provider != "claude" && entry.Provider != "codex") {
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
	raw, err := hostRPC("host.auth.list", map[string]any{})
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
		if entry.Provider == "claude" || entry.Provider == "codex" {
			if len(jobs) == 40 {
				break
			}
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
				results <- e.refreshAccountQuota(entry)
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
		e.quotaFailure("No Claude or Codex OAuth credentials were found")
	} else if successful == 0 {
		e.quotaFailure("All OAuth quota reads failed; check the individual account messages")
	}
}

func (e *Engine) refreshAccountQuota(entry AuthEntry) bool {
	e.mu.Lock()
	e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
	e.aliases[entry.Index] = entry.ID
	e.mu.Unlock()
	authData, err := hostRPC("host.auth.get", map[string]string{"auth_index": entry.Index})
	if err != nil || authData == nil {
		authData, err = hostRPC("host.auth.get", map[string]string{"AuthIndex": entry.Index})
	}
	if err != nil {
		e.recordQuotaError(entry, "Credential could not be read")
		return false
	}
	creds := findAuthJSON(authData)
	if email := findString(creds, 0, "email", "Email"); strings.Contains(email, "@") {
		entry.Name = accountDisplayName(email)
	}
	token := findString(creds, 0, "access_token", "accessToken")
	if token == "" {
		e.recordQuotaError(entry, "OAuth access token unavailable")
		return false
	}
	if len(token) > 10000 {
		e.recordQuotaError(entry, "Invalid OAuth token")
		return false
	}
	url := "https://api.anthropic.com/api/oauth/usage"
	headers := http.Header{"Authorization": []string{"Bearer " + token}, "Accept": []string{"application/json"}, "User-Agent": []string{"claude-code/2.1.0"}, "Anthropic-Beta": []string{"oauth-2025-04-20"}}
	if entry.Provider == "codex" {
		url = "https://chatgpt.com/backend-api/wham/usage"
		headers.Set("User-Agent", "codex-cli/0.1")
		id := findString(creds, 0, "chatgpt_account_id", "chatgptAccountId", "account_id")
		if id == "" {
			id = extractJWTAccountID(findString(creds, 0, "id_token", "idToken"))
		}
		if id != "" {
			headers.Set("Chatgpt-Account-Id", id)
		}
	}
	body, err := safeProviderGET(url, headers)
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
	resetRemaining := parseManualResetRemaining(body)
	if entry.Provider == "codex" {
		resetRemaining = codexResetRemaining(body, headers, safeProviderGET)
	}
	e.mu.Lock()
	e.quotaUpdated = observedAt
	e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	e.quotas[entry.ID] = AccountQuota{Provider: entry.Provider, AuthID: entry.ID, Name: entry.Name, Five: five, Week: week, FetchedAt: observedAt.Format(time.RFC3339Nano), ManualResetRemaining: resetRemaining}
	e.rememberQuotaForecastsLocked(e.quotas[entry.ID], observedAt)
	if state, ok := e.accountRefresh[entry.ID]; ok {
		state.Error = ""
		e.accountRefresh[entry.ID] = state
	}
	e.persistQuotasLocked()
	e.mu.Unlock()
	return true
}
func (e *Engine) quotaFailure(message string) { e.mu.Lock(); e.quotaError = message; e.mu.Unlock() }
func (e *Engine) recordQuotaError(a AuthEntry, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prior := e.quotas[a.ID]
	prior.AuthID = a.ID
	prior.Provider = a.Provider
	prior.Name = a.Name
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
		entries = append(entries, AuthEntry{Index: idx, ID: id, Name: name, Provider: provider})
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
	// Explicit allow-list prevents credentials being sent to arbitrary hosts.
	if url != "https://api.anthropic.com/api/oauth/usage" && url != "https://chatgpt.com/backend-api/wham/usage" && url != codexResetCreditsURL {
		return nil, fmt.Errorf("endpoint not permitted")
	}
	request := map[string]any{"Method": "GET", "URL": url, "Headers": headers, "Header": headers}
	data, err := hostRPC("host.http.do", request)
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

const codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// GET-only details fallback used by the official Codex client. Never call /consume.
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
