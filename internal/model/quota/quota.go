// Package quota asks a subscription how much of its plan is used
// (docs/plan-usage.md). ChatGPT reports that on every model call; Z.ai, Kimi
// and Grok report it nowhere but a usage endpoint of their own, so reading
// it means one small GET with the credential the model calls already use.
// The endpoints and the shapes they answer with are the ones
// slkiser/opencode-quota reads (MIT), which in turn follow each vendor's own
// client: Z.ai's glm-plan-usage plugin, Moonshot's kimi-cli, the Codex CLI
// and the Grok CLI. None is a documented API, and three have changed shape
// under other tools already, so every parser takes what it recognises and
// ignores the rest.
package quota

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

// Window lengths, in minutes.
const (
	FiveHours = 5 * 60
	Day       = 24 * 60
	Week      = 7 * Day
	Month     = 30 * Day
)

// xaiBilling is the Grok CLI's billing endpoint: the one source of a
// SuperGrok plan's usage, and the only one here on a host the model calls do
// not already reach.
const xaiBilling = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

const userAgent = "stavlos"

// source is how one provider reports plan usage: where, given the base URL
// its model calls go to; how the credential is presented; and how the reply
// reads. A provider is described here once; adding one is adding an entry.
type source struct {
	url   func(base *url.URL) string
	auth  func(h http.Header, tok model.Token)
	parse func(body []byte, now time.Time) (model.PlanUsage, error)
}

func bearer(h http.Header, tok model.Token) { h.Set("Authorization", "Bearer "+tok.Access) }

// onHost keeps the base URL's scheme and host and replaces its path: the
// credential goes only where it already goes.
func onHost(path string) func(*url.URL) string {
	return func(base *url.URL) string {
		u := *base
		u.Path, u.RawQuery = path, ""
		return u.String()
	}
}

var sources = map[string]source{
	"openai": {
		// …/backend-api/codex/responses → …/backend-api/wham/usage
		url: func(base *url.URL) string {
			i := strings.Index(base.Path, "/codex/")
			if i < 0 {
				return ""
			}
			return onHost(base.Path[:i] + "/wham/usage")(base)
		},
		auth: func(h http.Header, tok model.Token) {
			bearer(h, tok)
			h.Set("originator", "stavlos")
			if tok.AccountID != "" {
				h.Set("ChatGPT-Account-Id", tok.AccountID)
			}
		},
		parse: parseOpenAI,
	},
	"zai": {
		url: onHost("/api/monitor/usage/quota/limit"),
		// the key as it is, the way Z.ai's own plugin sends it
		auth:  func(h http.Header, tok model.Token) { h.Set("Authorization", tok.Access) },
		parse: parseZai,
	},
	"kimi": {
		url:   func(base *url.URL) string { return onHost(strings.TrimRight(base.Path, "/") + "/usages")(base) },
		auth:  bearer,
		parse: parseKimi,
	},
	"xai": {
		// The one source on a host the model calls do not reach: the Grok
		// CLI's billing endpoint. A test server stands in for both hosts.
		url: func(base *url.URL) string {
			if base.Host != "api.x.ai" {
				u := *base
				u.Path, u.RawQuery = "/v1/billing", "format=credits"
				return u.String()
			}
			return xaiBilling
		},
		auth: func(h http.Header, tok model.Token) {
			bearer(h, tok)
			h.Set("x-grok-client-surface", "grok-build") // the endpoint answers the Grok CLI
			h.Set("x-grok-client-version", "1.0.0")
		},
		parse: parseXai,
	},
}

// URL is where provider reports plan usage, given the base URL (or, for
// ChatGPT, the endpoint) its model calls go to; "" for a provider that has
// no such place.
func URL(provider, base string) string {
	src, ok := sources[provider]
	u, err := url.Parse(base)
	if !ok || err != nil || u.Host == "" {
		return ""
	}
	return src.url(u)
}

// Fetch reads provider's plan usage from endpoint (see URL) with the
// credential its model calls use.
func Fetch(ctx context.Context, c *http.Client, provider, endpoint string, tok model.Token) (model.PlanUsage, error) {
	src, ok := sources[provider]
	if !ok || endpoint == "" {
		return model.PlanUsage{}, fmt.Errorf("%s reports no plan usage", provider)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.PlanUsage{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	src.auth(req.Header, tok)
	resp, err := c.Do(req)
	if err != nil {
		return model.PlanUsage{}, fmt.Errorf("%s plan usage: %w", provider, redact(err, tok.Access))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return model.PlanUsage{}, fmt.Errorf("%s plan usage: %w", provider, err)
	}
	if resp.StatusCode/100 != 2 {
		return model.PlanUsage{}, fmt.Errorf("%s plan usage: status %d", provider, resp.StatusCode)
	}
	now := time.Now()
	u, err := src.parse(body, now)
	if err != nil {
		return model.PlanUsage{}, fmt.Errorf("%s plan usage: %w", provider, err)
	}
	if len(u.Windows) == 0 {
		return model.PlanUsage{}, fmt.Errorf("%s plan usage: no window in the reply (keys: %s)", provider, topKeys(body))
	}
	sort.SliceStable(u.Windows, func(i, j int) bool { return span(u.Windows[i]) < span(u.Windows[j]) })
	u.Observed = now
	return u, nil
}

// span orders windows shortest first, a window of unknown length last.
func span(w model.UsageWindow) int {
	if w.Minutes <= 0 {
		return int(^uint(0) >> 1)
	}
	return w.Minutes
}

func redact(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}

func topKeys(body []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func clamp(p float64) float64 { return min(max(p, 0), 100) }

// --- ChatGPT: GET /backend-api/wham/usage, the Codex CLI's account/rateLimits/read ---

func parseOpenAI(body []byte, _ time.Time) (model.PlanUsage, error) {
	type window struct {
		UsedPercent *float64 `json:"used_percent"`
		Seconds     int      `json:"limit_window_seconds"`
		ResetAt     int64    `json:"reset_at"`
	}
	var p struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return model.PlanUsage{}, err
	}
	u := model.PlanUsage{Plan: p.PlanType}
	if p.RateLimit == nil {
		return u, nil
	}
	for _, w := range []*window{p.RateLimit.Primary, p.RateLimit.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		uw := model.UsageWindow{UsedPercent: clamp(*w.UsedPercent), Minutes: w.Seconds / 60}
		if w.ResetAt > 0 {
			uw.ResetsAt = time.Unix(w.ResetAt, 0)
		}
		u.Windows = append(u.Windows, uw)
	}
	return u, nil
}

// --- Z.ai: GET /api/monitor/usage/quota/limit ---

// parseZai reads the token quota windows. The window is told by unit (3 is
// the five-hour one, 6 the weekly), never by type alone: TOKENS_LIMIT became
// CREDIT_LIMIT for some plans, which broke the tools that matched on it.
// TIME_LIMIT is the monthly count of MCP tool calls, not a model allowance.
func parseZai(body []byte, _ time.Time) (model.PlanUsage, error) {
	type limit struct {
		Type         string   `json:"type"`
		Unit         int      `json:"unit"`
		Usage        *float64 `json:"usage"`        // the allowance
		CurrentValue *float64 `json:"currentValue"` // what is used of it
		Percentage   *float64 `json:"percentage"`
		NextReset    float64  `json:"nextResetTime"` // epoch milliseconds
	}
	var p struct {
		Code    *int   `json:"code"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
		Data    *struct {
			Limits []limit `json:"limits"`
		} `json:"data"`
		Limits []limit `json:"limits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return model.PlanUsage{}, err
	}
	if (p.Success != nil && !*p.Success) || (p.Code != nil && *p.Code >= 400) {
		return model.PlanUsage{}, errors.New(cmp.Or(p.Msg, "the API refused"))
	}
	limits := p.Limits
	if p.Data != nil && p.Data.Limits != nil {
		limits = p.Data.Limits
	}
	var u model.PlanUsage
	for _, l := range limits {
		if l.Type != "TOKENS_LIMIT" && l.Type != "CREDIT_LIMIT" {
			continue
		}
		w := model.UsageWindow{}
		switch l.Unit {
		case 3:
			w.Minutes = FiveHours
		case 6:
			w.Minutes = Week
		default:
			continue
		}
		switch {
		case l.Type == "CREDIT_LIMIT" && l.Usage != nil && *l.Usage > 0 && l.CurrentValue != nil && *l.CurrentValue >= 0:
			w.UsedPercent = clamp(*l.CurrentValue / *l.Usage * 100)
		case l.Percentage != nil:
			w.UsedPercent = clamp(*l.Percentage)
		default:
			continue
		}
		if l.NextReset > 0 {
			w.ResetsAt = time.UnixMilli(int64(l.NextReset))
		}
		u.Windows = append(u.Windows, w)
	}
	return u, nil
}

// --- Kimi For Coding: GET <base>/usages, kimi-cli's /usage ---

// parseKimi reads both shapes the endpoint has answered with: a weekly
// "usage" plus "limits" windows of {limit, used | remaining}, and, for some
// accounts, "usages" of {used_ratio, reset_time} pools.
func parseKimi(body []byte, now time.Time) (model.PlanUsage, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return model.PlanUsage{}, err
	}
	if data, ok := root["data"].(map[string]any); ok {
		for _, k := range []string{"usage", "limits", "usages"} {
			if v, ok := data[k]; ok {
				root[k] = v
			}
		}
	}
	var u model.PlanUsage
	if usage, ok := root["usage"].(map[string]any); ok {
		if w, ok := kimiWindow(usage, Week, now); ok {
			u.Windows = append(u.Windows, w)
		}
	}
	if limits, ok := root["limits"].([]any); ok {
		for _, it := range limits {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			detail, ok := item["detail"].(map[string]any)
			if !ok {
				detail = item
			}
			win, _ := item["window"].(map[string]any)
			if w, ok := kimiWindow(detail, kimiMinutes(win, item, detail), now); ok {
				u.Windows = append(u.Windows, w)
			}
		}
	}
	if pools, ok := root["usages"].(map[string]any); ok {
		for key, minutes := range map[string]int{"limit_5h": FiveHours, "limit_week": Week, "limit_weekly": Week, "limit_month_total": Month} {
			pool, ok := pools[key].(map[string]any)
			if !ok {
				continue
			}
			ratio, ok := number(pool["used_ratio"])
			if !ok {
				continue
			}
			u.Windows = append(u.Windows, model.UsageWindow{UsedPercent: clamp(ratio * 100), Minutes: minutes, ResetsAt: kimiReset(pool, now)})
		}
	}
	return u, nil
}

func kimiWindow(d map[string]any, minutes int, now time.Time) (model.UsageWindow, bool) {
	limit, hasLimit := number(d["limit"])
	used, hasUsed := number(d["used"])
	if !hasUsed {
		if remaining, ok := number(d["remaining"]); ok && hasLimit {
			used, hasUsed = limit-remaining, true
		}
	}
	if !hasLimit || !hasUsed || limit <= 0 {
		return model.UsageWindow{}, false
	}
	return model.UsageWindow{UsedPercent: clamp(used / limit * 100), Minutes: minutes, ResetsAt: kimiReset(d, now)}, true
}

// kimiMinutes is a window's length: duration in its timeUnit (…MINUTE,
// …HOUR, …DAY; seconds otherwise), 0 when it says none.
func kimiMinutes(sources ...map[string]any) int {
	for _, s := range sources {
		d, ok := number(s["duration"])
		if !ok || d <= 0 {
			continue
		}
		unit := strings.ToUpper(fmt.Sprint(s["timeUnit"]))
		switch {
		case strings.Contains(unit, "MINUTE"):
			return int(d)
		case strings.Contains(unit, "HOUR"):
			return int(d * 60)
		case strings.Contains(unit, "DAY"):
			return int(d * Day)
		}
		return int(d / 60)
	}
	return 0
}

func kimiReset(d map[string]any, now time.Time) time.Time {
	for _, k := range []string{"reset_at", "resetAt", "reset_time", "resetTime"} {
		if s, ok := d[k].(string); ok {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
				return t
			}
		}
		if n, ok := number(d[k]); ok && n > 0 { // epoch seconds, or milliseconds
			if n > 1e12 {
				return time.UnixMilli(int64(n))
			}
			return time.Unix(int64(n), 0)
		}
	}
	for _, k := range []string{"reset_in", "resetIn", "ttl"} {
		if n, ok := number(d[k]); ok && n > 0 {
			return now.Add(time.Duration(n * float64(time.Second)))
		}
	}
	return time.Time{}
}

// --- Grok: GET cli-chat-proxy.grok.com/v1/billing?format=credits ---

// parseXai reads the one window a SuperGrok plan has: the share of the
// current period's credits used.
func parseXai(body []byte, _ time.Time) (model.PlanUsage, error) {
	var p struct {
		Config *struct {
			Period *struct {
				Type string `json:"type"`
				End  string `json:"end"`
			} `json:"currentPeriod"`
			UsagePercent *float64 `json:"creditUsagePercent"`
			BillingEnd   string   `json:"billingPeriodEnd"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return model.PlanUsage{}, err
	}
	if p.Config == nil || (p.Config.Period == nil && p.Config.UsagePercent == nil) {
		return model.PlanUsage{}, nil
	}
	w := model.UsageWindow{} // a period with no percentage yet has used nothing
	if p.Config.UsagePercent != nil {
		w.UsedPercent = clamp(*p.Config.UsagePercent)
	}
	end := p.Config.BillingEnd
	if per := p.Config.Period; per != nil {
		end = cmp.Or(per.End, end)
		switch kind := strings.ToUpper(per.Type); {
		case strings.Contains(kind, "WEEK"):
			w.Minutes = Week
		case strings.Contains(kind, "MONTH"):
			w.Minutes = Month
		case strings.Contains(kind, "DAY"):
			w.Minutes = Day
		}
	}
	if t, err := time.Parse(time.RFC3339, end); err == nil {
		w.ResetsAt = t
	}
	return model.PlanUsage{Windows: []model.UsageWindow{w}}, nil
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}
