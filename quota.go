package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type AuthEntry struct {
	Index    string
	ID       string
	Name     string
	Provider string
}

func (e *Engine) refreshQuota(force bool) {
	if !e.refreshMu.TryLock() {
		return
	}
	defer e.refreshMu.Unlock()
	e.mu.Lock()
	if !force && time.Since(e.lastRefresh) < quotaInterval {
		e.mu.Unlock()
		return
	}
	e.lastRefresh = time.Now()
	e.mu.Unlock()
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
	successful := 0
	attempted := 0
	for _, entry := range auths {
		select {
		case <-e.done:
			return
		default:
		}
		if entry.Provider != "claude" && entry.Provider != "codex" {
			continue
		}
		if attempted >= 40 {
			break
		}
		attempted++
		authData, err := hostRPC("host.auth.get", map[string]string{"auth_index": entry.Index})
		if err != nil || authData == nil {
			authData, err = hostRPC("host.auth.get", map[string]string{"AuthIndex": entry.Index})
		}
		if err != nil {
			e.recordQuotaError(entry, "Credential could not be read")
			continue
		}
		creds := findAuthJSON(authData)
		token := findString(creds, 0, "access_token", "accessToken")
		if token == "" {
			e.recordQuotaError(entry, "OAuth access token unavailable")
			continue
		}
		if len(token) > 10000 {
			e.recordQuotaError(entry, "Invalid OAuth token")
			continue
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
			continue
		}
		var five, week *QuotaWindow
		if entry.Provider == "claude" {
			five, week, err = parseClaudeQuota(body)
		} else {
			five, week, err = parseCodexQuota(body)
		}
		if err != nil {
			e.recordQuotaError(entry, "Quota response was not recognized")
			continue
		}
		e.mu.Lock()
		e.quotaUpdated = time.Now().UTC()
		e.names[entry.ID] = entry.Name
		e.names[entry.Index] = entry.Name
		e.aliases[entry.Index] = entry.ID
		e.quotas[entry.ID] = AccountQuota{Provider: entry.Provider, AuthID: entry.ID, Name: entry.Name, Five: five, Week: week, FetchedAt: time.Now().UTC().Format(time.RFC3339)}
		e.mu.Unlock()
		successful++
	}
	if attempted == 0 {
		e.quotaFailure("No Claude or Codex OAuth credentials were found")
	} else if successful == 0 {
		e.quotaFailure("All OAuth quota reads failed; check the individual account messages")
	}
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
	e.quotas[a.ID] = prior
	e.names[a.ID] = a.Name
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
		name := str(r, "name", "Name", "file_name", "fileName", "filename", "Filename", "email")
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
		name = maskCredential(name)
		entries = append(entries, AuthEntry{Index: idx, ID: id, Name: name, Provider: provider})
	}
	return entries
}
func maskCredential(s string) string {
	at := strings.IndexByte(s, '@')
	if at > 1 && at < len(s)-1 {
		s = s[:1] + "***" + s[at:]
	}
	if len(s) > 100 {
		s = s[:100]
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
	if url != "https://api.anthropic.com/api/oauth/usage" && url != "https://chatgpt.com/backend-api/wham/usage" {
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
	client := &http.Client{Timeout: 12 * time.Second}
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
