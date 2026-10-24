package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Antigravity quota endpoints, in the order used by CPA's management center
// (Cli-Proxy-API-Management-Center src/utils/quota/constants.ts).
var antigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// Cloud Code rejects newer models for hub versions below 2.23.0; CPA's fallback is 2.24.0.
const antigravityUserAgent = "antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=1000335682)"

// QuotaGroup is one Antigravity shared-quota group (for example "Gemini Models").
// Only its 5-hour and weekly buckets are kept; they map onto the existing windows.
type QuotaGroup struct {
	Key   string       `json:"key"`
	Label string       `json:"label"`
	Five  *QuotaWindow `json:"five_hour,omitempty"`
	Week  *QuotaWindow `json:"seven_day,omitempty"`
}

type QuotaGroupView struct {
	Key   string          `json:"key"`
	Label string          `json:"label"`
	Five  *WindowEstimate `json:"five_hour,omitempty"`
	Week  *WindowEstimate `json:"seven_day,omitempty"`
}

// antigravityGroupKey maps an upstream group label to the model family that shares it.
func antigravityGroupKey(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "claude") || strings.Contains(l, "gpt"):
		return "claude-gpt"
	case strings.Contains(l, "gemini"):
		return "gemini"
	}
	var b strings.Builder
	for _, r := range l {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	key := strings.Trim(b.String(), "-")
	if key == "" {
		key = "group"
	}
	return "other-" + key
}

// antigravityModelGroup attributes a request to the quota group of its model family.
func antigravityModelGroup(model string) string {
	m := normalizeModel(model)
	switch {
	case strings.HasPrefix(m, "gemini"):
		return "gemini"
	case strings.HasPrefix(m, "claude") || strings.HasPrefix(m, "gpt"):
		return "claude-gpt"
	}
	return ""
}

func antigravityWindowSeconds(window string) int64 {
	switch strings.ToLower(strings.TrimSpace(window)) {
	case "5h", "five-hour", "five_hour":
		return 5 * 3600
	case "weekly", "week":
		return 7 * 24 * 3600
	}
	return 0
}

// parseAntigravityQuota reads retrieveUserQuotaSummary. Buckets without a valid
// remainingFraction stay unknown rather than being treated as full or empty.
func parseAntigravityQuota(body []byte) ([]QuotaGroup, error) {
	var payload struct {
		Groups []struct {
			DisplayName  string `json:"displayName"`
			DisplayName2 string `json:"display_name"`
			Buckets      []struct {
				Window             string `json:"window"`
				ResetTime          string `json:"resetTime"`
				ResetTime2         string `json:"reset_time"`
				RemainingFraction  any    `json:"remainingFraction"`
				RemainingFraction2 any    `json:"remaining_fraction"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var groups []QuotaGroup
	seen := map[string]bool{}
	for i, g := range payload.Groups {
		label := strings.TrimSpace(firstNonEmpty(g.DisplayName, g.DisplayName2))
		if label == "" {
			label = fmt.Sprintf("Quota Group %d", i+1)
		}
		group := QuotaGroup{Key: antigravityGroupKey(label), Label: label}
		for _, b := range g.Buckets {
			seconds := antigravityWindowSeconds(b.Window)
			fraction, ok := quotaFraction(b.RemainingFraction)
			if !ok {
				fraction, ok = quotaFraction(b.RemainingFraction2)
			}
			if seconds == 0 || !ok {
				continue
			}
			used := math.Round((1-fraction)*100*1e6) / 1e6
			win := &QuotaWindow{Used: used, ResetAt: strings.TrimSpace(firstNonEmpty(b.ResetTime, b.ResetTime2)), Seconds: seconds}
			if seconds == 5*3600 {
				group.Five = win
			} else {
				group.Week = win
			}
		}
		if (group.Five == nil && group.Week == nil) || seen[group.Key] {
			continue
		}
		seen[group.Key] = true
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("quota groups missing")
	}
	return groups, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func quotaFraction(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		s := strings.TrimSpace(x)
		percent := strings.HasSuffix(s, "%")
		s = strings.TrimSuffix(s, "%")
		if _, err := fmt.Sscanf(s, "%g", &f); err != nil || s == "" {
			return 0, false
		}
		if percent {
			f /= 100
		}
	default:
		return 0, false
	}
	if !finite(f) || f < 0 || f > 1 {
		return 0, false
	}
	return f, true
}

func antigravityProjectID(creds map[string]any) string {
	if id := findString(creds, 0, "project_id", "projectId"); id != "" {
		return id
	}
	for _, k := range []string{"installed", "web"} {
		if nested, ok := creds[k].(map[string]any); ok {
			if id := findString(nested, 0, "project_id", "projectId"); id != "" {
				return id
			}
		}
	}
	return ""
}

// fetchAntigravityQuota tries each fixed endpoint like CPA's management center.
// A 2xx response whose groups cannot be parsed falls through to the next endpoint.
func fetchAntigravityQuota(headers http.Header, projectID string, post func(string, http.Header, []byte) (int, []byte, error)) ([]QuotaGroup, string) {
	body, _ := json.Marshal(map[string]string{"project": projectID})
	failure := "Quota endpoint unavailable"
	for _, url := range antigravityQuotaURLs {
		status, response, err := post(url, headers, body)
		if err != nil {
			continue
		}
		if status == 401 {
			failure = "OAuth access token expired; CPA refreshes it automatically"
			continue
		}
		if status < 200 || status >= 300 {
			failure = fmt.Sprintf("quota HTTP %d", status)
			continue
		}
		groups, perr := parseAntigravityQuota(response)
		if perr != nil {
			failure = "Quota response was not recognized"
			continue
		}
		return groups, ""
	}
	return nil, failure
}

func isAntigravityQuotaURL(url string) bool {
	for _, allowed := range antigravityQuotaURLs {
		if url == allowed {
			return true
		}
	}
	return false
}

// antigravityPOST is a read-only quota POST: through CPA's host HTTP client first,
// with the same fixed-URL direct fallback as the GET quota reads for older hosts.
func antigravityPOST(url string, headers http.Header, body []byte) (int, []byte, error) {
	if !isAntigravityQuotaURL(url) {
		return 0, nil, fmt.Errorf("endpoint not permitted")
	}
	if data, err := hostRPC("host.http.do", map[string]any{"Method": "POST", "URL": url, "Headers": headers, "Body": body}); err == nil {
		if r, ok := data.(map[string]any); ok {
			code := int(number(r, "StatusCode", "status_code", "statusCode"))
			raw := str(r, "Body", "body")
			decoded, decErr := base64.StdEncoding.DecodeString(raw)
			if decErr != nil {
				decoded = []byte(raw)
			}
			if len(decoded) > 512*1024 {
				return code, nil, fmt.Errorf("quota response too large")
			}
			return code, decoded, nil
		}
	}
	client := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("quota endpoint failed")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024+1))
	if err != nil || len(out) > 512*1024 {
		return resp.StatusCode, nil, fmt.Errorf("quota response invalid")
	}
	return resp.StatusCode, out, nil
}

// --- Accounting (sub2api Antigravity semantics) ---
//
// Antigravity reports Gemini-style usageMetadata for every model. CPA forwards
// promptTokenCount(+toolUse) as InputTokens (cache reads included),
// candidatesTokenCount as OutputTokens and thoughtsTokenCount separately as
// ReasoningTokens. sub2api bills input = prompt - cached (floored at 0),
// output = candidates + thoughts, cache read = cached.

// billableOutput is output including reasoning. Codex/Claude output already
// includes reasoning. Antigravity events recorded before Reasoning was stored
// recover it from totalTokenCount, which equals prompt + candidates + thoughts.
func billableOutput(ev Event) int64 {
	if normalizeProvider(ev.Provider) != "antigravity" {
		return ev.Output
	}
	if ev.Reasoning > 0 && ev.Output <= math.MaxInt64-ev.Reasoning {
		return ev.Output + ev.Reasoning
	}
	if ev.Total > ev.Input && ev.Input >= 0 && ev.Total-ev.Input > ev.Output {
		return ev.Total - ev.Input
	}
	return ev.Output
}

var antigravityTierSuffixes = []string{"-thinking", "-high", "-medium", "-low", "-minimal", "-tiered"}

// antigravityPriceKeys follows sub2api's lookup: exact model id first, then the
// base model with the thinking-tier suffix removed (the tier selects reasoning
// depth, not a token rate). "-preview" covers catalogs that list only the preview id.
func antigravityPriceKeys(model string) []string {
	m := normalizeModel(model)
	catalog := ""
	switch {
	case strings.HasPrefix(m, "gemini"):
		catalog = "google"
	case strings.HasPrefix(m, "claude"):
		catalog = "anthropic"
	case strings.HasPrefix(m, "gpt"):
		catalog = "openai"
	default:
		return nil
	}
	names := []string{m}
	base := m
	for changed := true; changed; {
		changed = false
		for _, suffix := range antigravityTierSuffixes {
			if strings.HasSuffix(base, suffix) && len(base) > len(suffix) {
				base = strings.TrimSuffix(base, suffix)
				changed = true
			}
		}
	}
	if base != m {
		names = append(names, base)
	}
	if catalog == "google" && !strings.HasSuffix(base, "-preview") {
		names = append(names, base+"-preview")
	}
	keys := make([]string, len(names))
	for i, n := range names {
		keys[i] = catalog + "/" + n
	}
	return keys
}
