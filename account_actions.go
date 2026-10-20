package main

import (
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
}

type ResetCredit struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

type ResetCreditList struct {
	AvailableCount int64         `json:"available_count"`
	Credits        []ResetCredit `json:"credits"`
}

func (e *Engine) consumeReset(authID, redeemKey, creditID string) (int, ResetConsumeResult, string) {
	return e.consumeResetWith(authID, redeemKey, creditID, hostRPC, providerPOST)
}

func (e *Engine) listResetCredits(authID string) (int, ResetCreditList, string) {
	return e.listResetCreditsWith(authID, hostRPC, safeProviderGET)
}

// knownCodexAccount resolves the live CPA credential for a Codex account id.
func (e *Engine) knownCodexAccount(authID string, rpc func(string, any) (any, error)) (AuthEntry, int, string) {
	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		return AuthEntry{}, 502, "host callbacks unavailable"
	}
	for _, entry := range parseAuthList(raw) {
		if entry.ID == authID && entry.Provider == "codex" {
			return entry, 0, ""
		}
	}
	return AuthEntry{}, 404, "account_not_found"
}

func (e *Engine) codexQuotaGate(authID string) (int, string) {
	if strings.TrimSpace(authID) == "" || len(authID) > 1024 {
		return 400, "invalid_auth_id"
	}
	e.mu.Lock()
	quota, known := e.quotas[authID]
	e.mu.Unlock()
	switch {
	case !known:
		return 404, "account_not_found"
	case quota.Provider != "codex":
		return 400, "reset_unsupported"
	}
	return 0, ""
}

// listResetCreditsWith reads (GET only) the account's available reset credits, ordered the way
// the official Codex TUI offers them: earliest expiry first, credits without expiry last.
func (e *Engine) listResetCreditsWith(authID string, rpc func(string, any) (any, error), get func(string, http.Header) ([]byte, error)) (int, ResetCreditList, string) {
	if status, failure := e.codexQuotaGate(authID); failure != "" {
		return status, ResetCreditList{}, failure
	}
	target, status, failure := e.knownCodexAccount(authID, rpc)
	if failure != "" {
		return status, ResetCreditList{}, failure
	}
	headers, _, failure := providerHeaders(target, rpc)
	if failure != "" {
		return 502, ResetCreditList{}, failure
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

// consumeResetWith spends one Codex rate-limit reset credit for exactly one known account.
// Only the fixed official consume URL is reachable, one request per account may be in flight,
// and the caller-supplied redeem_request_id makes a repeated confirmation idempotent upstream.
// An optional credit_id spends that specific credit, as the official Codex client does.
func (e *Engine) consumeResetWith(authID, redeemKey, creditID string, rpc func(string, any) (any, error), post func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error)) (int, ResetConsumeResult, string) {
	if strings.TrimSpace(authID) == "" || len(authID) > 1024 {
		return 400, ResetConsumeResult{}, "invalid_auth_id"
	}
	if !redeemKeyPattern.MatchString(redeemKey) {
		return 400, ResetConsumeResult{}, "invalid_redeem_request_id"
	}
	if creditID != "" && !resetCreditIDPattern.MatchString(creditID) {
		return 400, ResetConsumeResult{}, "invalid_credit_id"
	}
	if status, failure := e.codexQuotaGate(authID); failure != "" {
		return status, ResetConsumeResult{}, failure
	}
	e.mu.Lock()
	if e.consuming[authID] {
		e.mu.Unlock()
		return 409, ResetConsumeResult{}, "reset_busy"
	}
	e.consuming[authID] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.consuming, authID); e.mu.Unlock() }()

	target, status, failure := e.knownCodexAccount(authID, rpc)
	if failure != "" {
		return status, ResetConsumeResult{}, failure
	}
	headers, _, failure := providerHeaders(target, rpc)
	if failure != "" {
		return 502, ResetConsumeResult{}, failure
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

// providerPOST sends one POST through CPA's host HTTP client. There is deliberately no direct
// fallback: a network error after the provider accepted the request must not be replayed.
func providerPOST(url string, headers http.Header, body []byte, rpc func(string, any) (any, error)) (int, []byte, error) {
	if url != codexResetConsumeURL {
		return 0, nil, fmt.Errorf("endpoint not permitted")
	}
	data, err := rpc("host.http.do", map[string]any{"Method": "POST", "URL": url, "Headers": headers, "Body": body})
	if err != nil {
		return 0, nil, err
	}
	r, ok := data.(map[string]any)
	if !ok {
		return 0, nil, fmt.Errorf("invalid host response")
	}
	code := int(number(r, "StatusCode", "status_code", "statusCode"))
	raw := str(r, "Body", "body")
	decoded, decErr := base64.StdEncoding.DecodeString(raw)
	if decErr != nil {
		decoded = []byte(raw)
	}
	if len(decoded) > 64*1024 {
		return code, nil, fmt.Errorf("reset response too large")
	}
	return code, decoded, nil
}

// syncAuthStatus overlays CPA's live enabled/disabled state onto known quota accounts, so
// changes made here or in CPA's own management UI show without waiting for a quota scan.
func (e *Engine) syncAuthStatus(rpc func(string, any) (any, error)) {
	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		return
	}
	entries := parseAuthList(raw)
	if len(entries) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, entry := range entries {
		if q, ok := e.quotas[entry.ID]; ok && q.Disabled != entry.Disabled {
			q.Disabled = entry.Disabled
			e.quotas[entry.ID] = q
		}
	}
}
