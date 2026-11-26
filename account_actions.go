package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
const uuidHex = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

var claudeOrgPattern = regexp.MustCompile("^" + strings.ReplaceAll(uuidHex, "a-f", "a-fA-F") + "$")
var claudeConsumeURL = regexp.MustCompile(`^https://api\.anthropic\.com/api/organizations/(` + uuidHex + `)/reset_rate_limits$`)
var claudeGrantID = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

const codexResetConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

// Official Codex client outcomes (openai/codex backend-client ConsumeRateLimitResetCreditCode).
var resetConsumeCodes = map[string]bool{"reset": true, "nothing_to_reset": true, "no_credit": true, "already_redeemed": true}

// The dashboard generates one key per confirmed click; retries of that click reuse it.
var redeemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9-]{16,128}$`)

// Credit ids are opaque provider strings; accept printable ASCII without whitespace only.
var resetCreditIDPattern = regexp.MustCompile(`^[!-~]{1,256}$`)

type ResetConsumeResult struct {
	Code         string `json:"code"`
	WindowsReset int64  `json:"windows_reset"`
	codeOnly     bool   // Claude sends no window count for a non-reset outcome.
}

func (r ResetConsumeResult) MarshalJSON() ([]byte, error) {
	if r.codeOnly {
		return json.Marshal(struct {
			Code string `json:"code"`
		}{r.Code})
	}
	type wire ResetConsumeResult
	return json.Marshal(wire(r))
}

type ResetCredit struct {
	ID                string `json:"id"`
	Title             string `json:"title,omitempty"`
	Description       string `json:"description,omitempty"`
	ExpiresAt         string `json:"expires_at,omitempty"`
	Remaining         *int64 `json:"remaining,omitempty"`
	UsableNow         *bool  `json:"usable_now,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type ResetCreditList struct {
	AvailableCount    int64         `json:"available_count"`
	Credits           []ResetCredit `json:"credits"`
	ConfirmationToken string        `json:"confirmation_token,omitempty"`
}

func (e *Engine) consumeReset(authID, redeemKey, creditID, confirmToken string) (int, ResetConsumeResult, string) {
	return e.consumeResetWith(authID, redeemKey, creditID, confirmToken, hostRPC, providerPOST)
}

func (e *Engine) listResetCredits(authID string) (int, ResetCreditList, string) {
	return e.listResetCreditsWith(authID, hostRPC, safeProviderGET)
}

// Match the snapshot provider to the live credential; never cross providers.
func knownResetAccount(authID, provider string, rpc func(string, any) (any, error)) (AuthEntry, int, string) {
	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		return AuthEntry{}, 502, "host callbacks unavailable"
	}
	for _, entry := range parseAuthList(raw) {
		if entry.ID == authID && entry.Provider == provider {
			if entry.Disabled {
				return AuthEntry{}, 409, "account_disabled"
			}
			return entry, 0, ""
		}
	}
	return AuthEntry{}, 404, "account_not_found"
}

func (e *Engine) resetProvider(authID string) (string, int, string) {
	if strings.TrimSpace(authID) == "" || len(authID) > 1024 {
		return "", 400, "invalid_auth_id"
	}
	e.mu.RLock()
	quota, known := e.quotas[authID]
	e.mu.RUnlock()
	if !known {
		return "", 404, "account_not_found"
	}
	if quota.Provider != "codex" && quota.Provider != "claude" {
		return "", 400, "reset_unsupported"
	}
	return quota.Provider, 0, ""
}

// listResetCreditsWith reads (GET only) the account's available reset credits, ordered the way
// the official Codex TUI offers them: earliest expiry first, credits without expiry last.
func (e *Engine) listResetCreditsWith(authID string, rpc func(string, any) (any, error), get func(string, http.Header) ([]byte, error)) (int, ResetCreditList, string) {
	provider, status, failure := e.resetProvider(authID)
	if failure != "" {
		return status, ResetCreditList{}, failure
	}
	target, status, failure := knownResetAccount(authID, provider, rpc)
	if failure != "" {
		return status, ResetCreditList{}, failure
	}
	headers, _, failure := providerHeaders(target, rpc)
	if failure != "" {
		return 502, ResetCreditList{}, failure
	}
	if provider == "claude" {
		return e.listClaudeResetCredits(authID, claudeResetHeaders(headers), rpc)
	}
	body, err := get(codexResetCreditsURL, headers)
	if err != nil {
		return 502, ResetCreditList{}, "reset credits unavailable"
	}
	list, ok := parseResetCredits(body)
	if !ok {
		return 502, ResetCreditList{}, "reset credits invalid"
	}
	e.mu.Lock()
	if q, known := e.quotas[authID]; known {
		count := list.AvailableCount
		q.ManualResetRemaining = &count
		e.quotas[authID] = q
	}
	e.mu.Unlock()
	return 200, list, ""
}

func parseResetCredits(body []byte) (ResetCreditList, bool) {
	var response struct {
		AvailableCount json.RawMessage `json:"available_count"`
		Credits        []struct {
			ID          string  `json:"id"`
			Status      string  `json:"status"`
			ExpiresAt   *string `json:"expires_at"`
			Title       *string `json:"title"`
			Description *string `json:"description"`
		} `json:"credits"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.AvailableCount) == 0 {
		return ResetCreditList{}, false
	}
	wrapped, _ := json.Marshal(map[string]any{"rate_limit_reset_credits": map[string]any{"available_count": response.AvailableCount}})
	count := parseManualResetRemaining(wrapped)
	if count == nil {
		return ResetCreditList{}, false
	}
	type ranked struct {
		credit  ResetCredit
		expires int64
	}
	var available []ranked
	for _, item := range response.Credits {
		if item.Status != "available" {
			continue
		}
		if !resetCreditIDPattern.MatchString(item.ID) {
			return ResetCreditList{}, false
		}
		credit := ResetCredit{ID: item.ID, Title: shortText(item.Title), Description: shortText(item.Description)}
		expires := int64(1<<63 - 1)
		if item.ExpiresAt != nil {
			at, err := time.Parse(time.RFC3339, *item.ExpiresAt)
			if err != nil {
				return ResetCreditList{}, false // Same as the official client: never guess an order.
			}
			expires = at.Unix()
			credit.ExpiresAt = at.UTC().Format(time.RFC3339)
		}
		available = append(available, ranked{credit, expires})
	}
	sort.SliceStable(available, func(i, j int) bool { return available[i].expires < available[j].expires })
	list := ResetCreditList{AvailableCount: *count, Credits: []ResetCredit{}}
	for _, item := range available {
		if int64(len(list.Credits)) >= *count {
			break
		}
		list.Credits = append(list.Credits, item.credit)
	}
	return list, true
}

type claudeGrant struct {
	claudeGrantBalance
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Clears          []string `json:"clears"`
	GivesBackWeekly bool     `json:"gives_back_weekly"`
	UsableNow       bool     `json:"usable_now"`
	Reason          string   `json:"unavailable_reason"`
	Blocking        []string `json:"blocking"`
	RequiresLimit   *bool    `json:"use_requires_limit"`
}

func (g claudeGrant) description() (string, string) {
	fiveOnly, weekOnly := len(g.Clears) > 0, len(g.Clears) > 0
	for _, window := range g.Clears {
		if window != "five_hour" {
			fiveOnly = false
		}
		switch window {
		case "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_overage_included":
		default:
			weekOnly = false
		}
	}
	switch {
	case g.GivesBackWeekly:
		return "Fable reset", "Refills your Fable allowance and gives back what Fable used on your weekly limit."
	case g.Type == "session" || fiveOnly:
		return "5-hour reset", "Refills your 5-hour limit."
	case g.Type == "weekly" || weekOnly:
		return "Weekly reset", "Refills your weekly limit."
	default:
		return "Full reset", "Refills your 5-hour and weekly limits."
	}
}

func (g claudeGrant) unavailableReason(cooldown, atLimit bool) string {
	if g.UsableNow {
		return ""
	}
	switch g.Reason {
	case "cooldown", "blocked_five_hour", "blocked_seven_day", "blocked_opus", "blocked_sonnet", "blocked_other", "cannot_clear", "needs_limit", "unavailable":
		return g.Reason
	}
	if cooldown {
		return "cooldown"
	}
	if len(g.Blocking) > 0 {
		switch g.Blocking[0] {
		case "five_hour":
			return "blocked_five_hour"
		case "seven_day":
			return "blocked_seven_day"
		case "seven_day_opus":
			return "blocked_opus"
		case "seven_day_sonnet":
			return "blocked_sonnet"
		default:
			return "blocked_other"
		}
	}
	if (g.RequiresLimit == nil || *g.RequiresLimit) && !atLimit {
		return "needs_limit"
	}
	return "unavailable"
}

func parseClaudeResetCredits(body []byte, now time.Time) (ResetCreditList, string) {
	inventory, diag := decodeClaudeInventory(body)
	switch diag {
	case "":
	case manualResetUnavailable:
		return ResetCreditList{}, "reset credits unavailable"
	case manualResetCLIVersion, manualResetSurface, manualResetNotEligible:
		return ResetCreditList{}, "claude_reset_" + diag
	default:
		return ResetCreditList{}, "reset credits invalid"
	}
	var meta struct {
		Next     string     `json:"next_grant_id"`
		AtLimit  bool       `json:"at_limit"`
		Cooldown *time.Time `json:"cooldown_until"`
	}
	if json.Unmarshal(inventory.block, &meta) != nil || (meta.Next != "" && !claudeGrantID.MatchString(meta.Next)) {
		return ResetCreditList{}, "reset credits invalid"
	}
	seen := make(map[string]bool)
	type ranked struct {
		credit  ResetCredit
		expires *time.Time
	}
	var rows []ranked
	list := ResetCreditList{Credits: []ResetCredit{}}
	for _, raw := range inventory.Grants {
		var g claudeGrant
		if json.Unmarshal(raw, &g) != nil || !claudeGrantID.MatchString(g.ID) || seen[g.ID] {
			return ResetCreditList{}, "reset credits invalid"
		}
		seen[g.ID] = true
		left, end, ok := g.active(now)
		if !ok || left > 50-list.AvailableCount {
			return ResetCreditList{}, "reset credits invalid"
		}
		if left == 0 {
			continue
		}
		list.AvailableCount += left
		credit := ResetCredit{ID: g.ID, Remaining: &left, UsableNow: &g.UsableNow, UnavailableReason: g.unavailableReason(meta.Cooldown != nil && meta.Cooldown.After(now), meta.AtLimit)}
		credit.Title, credit.Description = g.description()
		if end != nil {
			credit.ExpiresAt = end.UTC().Format(time.RFC3339Nano)
		}
		rows = append(rows, ranked{credit, end})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.expires == nil && b.expires != nil {
			return false
		}
		if a.expires != nil && b.expires == nil {
			return true
		}
		if a.expires != nil && !a.expires.Equal(*b.expires) {
			return a.expires.Before(*b.expires)
		}
		if (a.credit.ID == meta.Next) != (b.credit.ID == meta.Next) {
			return a.credit.ID == meta.Next
		}
		return *a.credit.UsableNow && !*b.credit.UsableNow
	})
	for _, row := range rows {
		list.Credits = append(list.Credits, row.credit)
	}
	return list, ""
}

func claudeResetStatus(status int, fallback string) (int, string) {
	switch status {
	case 429:
		return 429, "reset_rate_limited"
	case 401, 403:
		return 422, "reset_auth_failed"
	default:
		return 502, fallback
	}
}

const resetBindingTTL = 30 * time.Minute
const resetBindingLimit = 256

var confirmationTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

type resetBinding struct {
	authID, org   string
	usableCredits map[string]bool
	createdAt     time.Time
}

func (e *Engine) removeResetBindingsLocked(authID string) {
	for token, b := range e.resetBindings {
		if b.authID == authID {
			delete(e.resetBindings, token)
		}
	}
}

func (e *Engine) storeResetBindingLocked(token string, b resetBinding) {
	oldest := ""
	for key, previous := range e.resetBindings {
		if b.createdAt.Sub(previous.createdAt) >= resetBindingTTL {
			delete(e.resetBindings, key)
			continue
		}
		if oldest == "" || previous.createdAt.Before(e.resetBindings[oldest].createdAt) {
			oldest = key
		}
	}
	if len(e.resetBindings) >= resetBindingLimit {
		delete(e.resetBindings, oldest)
	}
	e.resetBindings[token] = b
}

func (e *Engine) boundOrgLocked(token, authID, creditID string) string {
	b, ok := e.resetBindings[token]
	if !ok {
		return ""
	}
	if time.Since(b.createdAt) >= resetBindingTTL {
		delete(e.resetBindings, token)
		return ""
	}
	if b.authID != authID || !b.usableCredits[creditID] {
		return ""
	}
	return b.org
}

func (e *Engine) listClaudeResetCredits(authID string, headers http.Header, rpc func(string, any) (any, error)) (int, ResetCreditList, string) {
	resp, err := resetHostHTTP("GET", claudeResetInventoryURL, headers, nil, rpc)
	if err != nil || resp.status < 200 || resp.status >= 300 {
		status, msg := claudeResetStatus(resp.status, "reset credits unavailable")
		return status, ResetCreditList{}, msg
	}
	list, failure := parseClaudeResetCredits(resp.body, time.Now())
	if failure != "" {
		return 502, ResetCreditList{}, failure
	}
	usable := make(map[string]bool)
	for _, credit := range list.Credits {
		if credit.UsableNow != nil && *credit.UsableNow {
			usable[credit.ID] = true
		}
	}
	org := resp.org
	if len(usable) > 0 {
		if org == "" {
			var status int
			org, status, failure = claudeProfileOrg(headers, rpc)
			if failure != "" {
				return status, ResetCreditList{}, failure
			}
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return 502, ResetCreditList{}, "reset credits unavailable"
		}
		list.ConfirmationToken = base64.RawURLEncoding.EncodeToString(nonce[:])
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	q, ok := e.quotas[authID]
	if !ok || q.Provider != "claude" {
		return 409, ResetCreditList{}, "reset_account_changed"
	}
	if token := list.ConfirmationToken; token != "" {
		if _, exists := e.resetBindings[token]; exists {
			return 502, ResetCreditList{}, "reset credits unavailable"
		}
		e.storeResetBindingLocked(token, resetBinding{authID: authID, org: org, usableCredits: usable, createdAt: time.Now()})
	}
	count := list.AvailableCount
	q.ManualResetRemaining = &count
	q.ManualResetDiagnostic = ""
	e.quotas[authID] = q
	return 200, list, ""
}

func shortText(value *string) string {
	if value == nil {
		return ""
	}
	text := strings.TrimSpace(*value)
	if utf8.RuneCountInString(text) > 200 {
		text = string([]rune(text)[:200])
	}
	return text
}

// Serialize credential/profile lookup and redemption per account. The caller reuses
// the same key for retries; Claude requires an explicit grant, while Codex does not.
func (e *Engine) consumeResetWith(authID, redeemKey, creditID, confirmToken string, rpc func(string, any) (any, error), post func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error)) (int, ResetConsumeResult, string) {
	if strings.TrimSpace(authID) == "" || len(authID) > 1024 {
		return 400, ResetConsumeResult{}, "invalid_auth_id"
	}
	if !redeemKeyPattern.MatchString(redeemKey) {
		return 400, ResetConsumeResult{}, "invalid_redeem_request_id"
	}
	if creditID != "" && !resetCreditIDPattern.MatchString(creditID) {
		return 400, ResetConsumeResult{}, "invalid_credit_id"
	}
	provider, status, failure := e.resetProvider(authID)
	if failure != "" {
		return status, ResetConsumeResult{}, failure
	}
	if provider == "claude" {
		if !claudeGrantID.MatchString(creditID) {
			return 400, ResetConsumeResult{}, "invalid_credit_id"
		}
		if len(redeemKey) > 64 {
			return 400, ResetConsumeResult{}, "invalid_redeem_request_id"
		}
		if !confirmationTokenPattern.MatchString(confirmToken) {
			return 400, ResetConsumeResult{}, "invalid_confirmation_token"
		}
	}
	e.mu.Lock()
	if provider == "claude" && e.boundOrgLocked(confirmToken, authID, creditID) == "" {
		e.mu.Unlock()
		return 409, ResetConsumeResult{}, "reset_account_changed"
	}
	if e.consuming[authID] {
		e.mu.Unlock()
		return 409, ResetConsumeResult{}, "reset_busy"
	}
	e.consuming[authID] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.consuming, authID); e.mu.Unlock() }()

	target, status, failure := knownResetAccount(authID, provider, rpc)
	if failure != "" {
		return status, ResetConsumeResult{}, failure
	}
	headers, _, failure := providerHeaders(target, rpc)
	if failure != "" {
		return 502, ResetConsumeResult{}, failure
	}
	if provider == "claude" {
		return e.consumeClaudeReset(authID, redeemKey, creditID, confirmToken, claudeResetHeaders(headers), rpc, post)
	}
	headers.Set("Content-Type", "application/json")
	request := map[string]string{"redeem_request_id": redeemKey}
	if creditID != "" {
		request["credit_id"] = creditID
	}
	body, _ := json.Marshal(request)
	status, response, err := post(codexResetConsumeURL, headers, body, rpc)
	if err != nil {
		return 502, ResetConsumeResult{}, "reset endpoint unavailable"
	}
	if status < 200 || status >= 300 {
		return 502, ResetConsumeResult{}, fmt.Sprintf("reset HTTP %d", status)
	}
	var result ResetConsumeResult
	if json.Unmarshal(response, &result) != nil || !resetConsumeCodes[result.Code] || result.WindowsReset < 0 {
		return 502, ResetConsumeResult{}, "reset response invalid"
	}
	// no_credit for one chosen credit says nothing about the others; the follow-up refresh does.
	if result.Code == "reset" || (result.Code == "no_credit" && creditID == "") {
		// The count is stale either way; the dashboard follows with a quota refresh.
		e.mu.Lock()
		if q, ok := e.quotas[authID]; ok && q.ManualResetRemaining != nil {
			next := *q.ManualResetRemaining - 1
			if result.Code == "no_credit" || next < 0 {
				next = 0
			}
			q.ManualResetRemaining = &next
			e.quotas[authID] = q
		}
		e.mu.Unlock()
	}
	return 200, result, ""
}

func claudeProfileOrg(headers http.Header, rpc func(string, any) (any, error)) (string, int, string) {
	resp, err := resetHostHTTP("GET", claudeProfileURL, headers, nil, rpc)
	if err != nil || resp.status < 200 || resp.status >= 300 {
		status, msg := claudeResetStatus(resp.status, "reset organization unavailable")
		return "", status, msg
	}
	var profile struct {
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if json.Unmarshal(resp.body, &profile) != nil || !claudeOrgPattern.MatchString(profile.Organization.UUID) {
		return "", 502, "reset organization unavailable"
	}
	return strings.ToLower(profile.Organization.UUID), 200, ""
}

func (e *Engine) consumeClaudeReset(authID, key, creditID, confirmToken string, headers http.Header, rpc func(string, any) (any, error), post func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error)) (int, ResetConsumeResult, string) {
	org, status, msg := claudeProfileOrg(headers, rpc)
	if msg != "" {
		return status, ResetConsumeResult{}, msg
	}
	e.mu.Lock()
	if e.quotas[authID].Provider != "claude" || e.boundOrgLocked(confirmToken, authID, creditID) != org {
		delete(e.resetBindings, confirmToken)
		e.mu.Unlock()
		return 409, ResetConsumeResult{}, "reset_account_changed"
	}
	e.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": creditID, "request_id": key})
	status, response, err := post("https://api.anthropic.com/api/organizations/"+org+"/reset_rate_limits", headers, body, rpc)
	if err != nil {
		return 502, ResetConsumeResult{}, "reset_unconfirmed"
	}
	if status < 200 || status >= 300 {
		status, msg := claudeResetStatus(status, fmt.Sprintf("reset HTTP %d", status))
		return status, ResetConsumeResult{}, msg
	}
	var result struct {
		Result  string   `json:"result"`
		Reason  string   `json:"reason"`
		Cleared []string `json:"cleared"`
	}
	if json.Unmarshal(response, &result) != nil {
		return 502, ResetConsumeResult{}, "reset response invalid"
	}
	code := ""
	switch result.Result {
	case "reset":
		code = "reset"
	case "already_used":
		code = "already_redeemed"
	case "not_limited":
		code = "nothing_to_reset"
	case "cooldown":
		code = "cooldown"
	case "ineligible":
		code = "no_credit"
		if result.Reason == "not_next_grant" {
			code = "not_next_grant"
		}
	case "unavailable":
		return 502, ResetConsumeResult{}, "reset_unconfirmed"
	default:
		return 502, ResetConsumeResult{}, "reset response invalid"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, bound := e.resetBindings[confirmToken]; bound && code == "reset" {
		if q, ok := e.quotas[authID]; ok && q.Provider == "claude" && q.ManualResetRemaining != nil {
			var next int64
			if *q.ManualResetRemaining > 0 {
				next = *q.ManualResetRemaining - 1
			}
			q.ManualResetRemaining = &next
			e.quotas[authID] = q
		}
	}
	delete(e.resetBindings, confirmToken)
	return 200, ResetConsumeResult{Code: code, codeOnly: code != "reset", WindowsReset: int64(len(result.Cleared))}, ""
}

// providerPOST sends one POST through CPA's host HTTP client. There is deliberately no direct
// fallback: a network error after the provider accepted the request must not be replayed.
func providerPOST(url string, headers http.Header, body []byte, rpc func(string, any) (any, error)) (int, []byte, error) {
	resp, err := resetHostHTTP("POST", url, headers, body, rpc)
	if err == nil && resp.status >= 200 && resp.status < 300 && resp.orgPresent && url != codexResetConsumeURL {
		if resp.org != claudeConsumeURL.FindStringSubmatch(url)[1] {
			return resp.status, nil, fmt.Errorf("unconfirmed organization")
		}
	}
	return resp.status, resp.body, err
}

type resetHostResponse struct {
	status     int
	body       []byte
	org        string
	orgPresent bool
}

// Only these exact method/URL pairs may receive the bearer token. No direct HTTP,
// redirects implemented by the plugin, or replay after an ambiguous host failure.
func resetHostHTTP(method, url string, headers http.Header, body []byte, rpc func(string, any) (any, error)) (resetHostResponse, error) {
	var resp resetHostResponse
	request := map[string]any{"Method": method, "URL": url, "Headers": headers, "Header": headers}
	limit := 512 * 1024
	switch {
	case method == "GET" && (url == claudeResetInventoryURL || url == claudeProfileURL):
	case method == "POST" && (url == codexResetConsumeURL || claudeConsumeURL.MatchString(url)):
		request["Body"] = body
		limit = 64 * 1024
	default:
		return resp, fmt.Errorf("endpoint not permitted")
	}
	data, err := rpc("host.http.do", request)
	r, ok := data.(map[string]any)
	if err != nil || !ok {
		return resp, fmt.Errorf("host request failed")
	}
	resp.status = int(number(r, "StatusCode", "status_code", "statusCode"))
	if resp.status < 100 || resp.status > 599 {
		return resp, fmt.Errorf("invalid host response")
	}
	// Never decode or forward an upstream error body.
	if resp.status < 200 || resp.status >= 300 {
		return resp, nil
	}
	if url != codexResetConsumeURL {
		resp.org, resp.orgPresent = resetOrgHeader(getAny(r, "Headers", "headers", "Header", "header"))
	}
	raw := str(r, "Body", "body")
	if len(raw) > base64.StdEncoding.EncodedLen(limit) {
		return resp, fmt.Errorf("reset response too large")
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		decoded = []byte(raw)
	}
	if len(decoded) > limit {
		return resp, fmt.Errorf("reset response too large")
	}
	resp.body = decoded
	return resp, nil
}

// Header names are case-insensitive. An empty, malformed or ambiguous value is
// distinguishable from an absent header, so it cannot confirm a successful POST.
func resetOrgHeader(headers any) (string, bool) {
	if headers == nil {
		return "", false
	}
	wire, err := json.Marshal(headers)
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(wire, &fields) != nil {
		return "", true
	}
	var org string
	present := false
	for key, raw := range fields {
		if !strings.EqualFold(key, "Anthropic-Organization-Id") {
			continue
		}
		if present {
			return "", true
		}
		present = true
		if json.Unmarshal(raw, &org) != nil {
			var values []string
			if json.Unmarshal(raw, &values) != nil || len(values) != 1 {
				return "", true
			}
			org = values[0]
		}
	}
	if !claudeOrgPattern.MatchString(org) {
		return "", present
	}
	return strings.ToLower(org), true
}

// syncAuthStatus reconciles live CPA identities as well as enabled/disabled state.
// New accounts are visible before their first quota read. The caller requests a full
// refresh when true; a busy refresh lock leaves the discovery pending for a later sync.
func (e *Engine) syncAuthStatus(rpc func(string, any) (any, error)) bool {
	e.mu.Lock()
	e.authListStarted++
	listVersion := e.authListStarted
	e.mu.Unlock()
	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		return false
	}
	entries := parseAuthList(raw)
	live, complete := completeAuthIDs(raw, entries)
	if len(entries) == 0 && !complete {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if listVersion < e.authListApplied {
		return e.authDiscoveryPending
	}
	e.authListApplied = listVersion
	changed := false
	// Never wait here with mu held. Busy account/full writers may still have
	// old results; discover additions now, and prune on a fresh idle sync.
	if complete && e.refreshMu.TryLock() {
		changed = e.pruneQuotasLocked(live)
		e.refreshMu.Unlock()
	}
	for _, entry := range entries {
		if !quotaProvider(entry.Provider) {
			continue
		}
		q, known := e.quotas[entry.ID]
		if !known {
			q = AccountQuota{AuthID: entry.ID, Provider: entry.Provider, Name: entry.Name, Error: "Quota has not been fetched; refresh pending"}
			e.names[entry.ID], e.names[entry.Index] = entry.Name, entry.Name
			e.aliases[entry.Index] = entry.ID
			e.authDiscoveryPending = true
			changed = true
		}
		q.Disabled = entry.Disabled
		q.DisableReason = sanitizeDisableReason(entry.Disabled, entry.DisableReason)
		e.quotas[entry.ID] = q
	}
	if changed {
		e.persistQuotasLocked()
	}
	return e.authDiscoveryPending
}
