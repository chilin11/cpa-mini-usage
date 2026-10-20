package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const codexResetConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

// Official Codex client outcomes (openai/codex backend-client ConsumeRateLimitResetCreditCode).
var resetConsumeCodes = map[string]bool{"reset": true, "nothing_to_reset": true, "no_credit": true, "already_redeemed": true}

// The dashboard generates one key per confirmed click; retries of that click reuse it.
var redeemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9-]{16,128}$`)

type ResetConsumeResult struct {
	Code         string `json:"code"`
	WindowsReset int64  `json:"windows_reset"`
}

func (e *Engine) consumeReset(authID, redeemKey string) (int, ResetConsumeResult, string) {
	return e.consumeResetWith(authID, redeemKey, hostRPC, providerPOST)
}

// consumeResetWith spends one Codex rate-limit reset credit for exactly one known account.
// Only the fixed official consume URL is reachable, one request per account may be in flight,
// and the caller-supplied redeem_request_id makes a repeated confirmation idempotent upstream.
func (e *Engine) consumeResetWith(authID, redeemKey string, rpc func(string, any) (any, error), post func(string, http.Header, []byte, func(string, any) (any, error)) (int, []byte, error)) (int, ResetConsumeResult, string) {
	if strings.TrimSpace(authID) == "" || len(authID) > 1024 {
		return 400, ResetConsumeResult{}, "invalid_auth_id"
	}
	if !redeemKeyPattern.MatchString(redeemKey) {
		return 400, ResetConsumeResult{}, "invalid_redeem_request_id"
	}
	e.mu.Lock()
	quota, known := e.quotas[authID]
	switch {
	case !known:
		e.mu.Unlock()
		return 404, ResetConsumeResult{}, "account_not_found"
	case quota.Provider != "codex":
		e.mu.Unlock()
		return 400, ResetConsumeResult{}, "reset_unsupported"
	case e.consuming[authID]:
		e.mu.Unlock()
		return 409, ResetConsumeResult{}, "reset_busy"
	}
	e.consuming[authID] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.consuming, authID); e.mu.Unlock() }()

	raw, err := rpc("host.auth.list", map[string]any{})
	if err != nil {
		return 502, ResetConsumeResult{}, "host callbacks unavailable"
	}
	var target *AuthEntry
	for _, entry := range parseAuthList(raw) {
		if entry.ID == authID && entry.Provider == "codex" {
			entry := entry
			target = &entry
			break
		}
	}
	if target == nil {
		return 404, ResetConsumeResult{}, "account_not_found"
	}
	headers, _, failure := providerHeaders(*target, rpc)
	if failure != "" {
		return 502, ResetConsumeResult{}, failure
	}
	headers.Set("Content-Type", "application/json")
	body, _ := json.Marshal(map[string]string{"redeem_request_id": redeemKey})
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
	if result.Code == "reset" || result.Code == "no_credit" {
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
