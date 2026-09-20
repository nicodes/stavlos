package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

func TestURLStaysOnTheHostTheCredentialAlreadyGoesTo(t *testing.T) {
	for _, tc := range []struct{ provider, base, want string }{
		{"openai", "https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/backend-api/wham/usage"},
		{"zai", "https://api.z.ai/api/coding/paas/v4", "https://api.z.ai/api/monitor/usage/quota/limit"},
		{"kimi", "https://api.kimi.ai/coding/v1", "https://api.kimi.ai/coding/v1/usages"},
		{"xai", "https://api.x.ai/v1", "https://cli-chat-proxy.grok.com/v1/billing?format=credits"},
		{"xai", "http://127.0.0.1:9/v1", "http://127.0.0.1:9/v1/billing?format=credits"},
		{"fake", "https://example.com/v1", ""},
	} {
		if got := URL(tc.provider, tc.base); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.provider, tc.base, got, tc.want)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func sortWindows(u *model.PlanUsage) {
	sort.SliceStable(u.Windows, func(i, j int) bool { return span(u.Windows[i]) < span(u.Windows[j]) })
}

func TestReadings(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body string
		want                 string // minutes:percent, shortest window first
		plan                 string
		reset                bool
	}{
		{name: "chatgpt two windows", provider: "openai", plan: "pro", reset: true, want: "300:38 10080:17",
			body: `{"plan_type":"pro","rate_limit":{"allowed":true,"primary_window":{"used_percent":38,"limit_window_seconds":18000,"reset_at":1800000000},"secondary_window":{"used_percent":17,"limit_window_seconds":604800,"reset_at":1800500000}},"credits":{"has_credits":false}}`},
		{name: "chatgpt weekly only, the secondary null", provider: "openai", plan: "pro", reset: true, want: "10080:100",
			body: `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1800500000},"secondary_window":null}}`},
		{name: "zai tokens", provider: "zai", reset: true, want: "300:12 10080:40",
			body: `{"code":200,"success":true,"data":{"limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"usage":100,"currentValue":3,"percentage":3},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":40,"nextResetTime":1800500000000},{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":12,"nextResetTime":1800000000000}]}}`},
		{name: "zai credits, the shape that broke other tools", provider: "zai", reset: true, want: "300:25",
			body: `{"code":200,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":2000,"currentValue":500,"percentage":0,"nextResetTime":1800000000000}]}}`},
		{name: "kimi weekly and 5h", provider: "kimi", reset: true, want: "300:10 10080:50",
			body: `{"usage":{"limit":"1000","used":"500","resetTime":"2027-01-15T00:00:00Z"},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":200,"remaining":180,"resetTime":"2027-01-10T05:00:00Z"}}]}`},
		{name: "kimi ratio pools", provider: "kimi", reset: true, want: "300:7 43200:61",
			body: `{"usages":{"limit_5h":{"used_ratio":0.07,"reset_time":1800000000},"limit_month_total":{"used_ratio":0.61,"reset_time":1800500000}}}`},
		{name: "grok weekly", provider: "xai", reset: true, want: "10080:5",
			body: `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-07-13T02:24:00.983423+00:00","end":"2026-07-20T02:24:00.983423+00:00"},"creditUsagePercent":5,"isUnifiedBillingUser":true,"productUsage":[{"product":"Api","usagePercent":5},{"product":"GrokChat"}]}}`},
		{name: "grok, a period nothing was used in yet", provider: "xai", reset: true, want: "10080:0",
			body: `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-07-20T02:24:00Z"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := sources[tc.provider].parse([]byte(tc.body), time.Unix(1790000000, 0))
			if err != nil {
				t.Fatal(err)
			}
			sortWindows(&u)
			var got []string
			for _, w := range u.Windows {
				got = append(got, itoa(w.Minutes)+":"+itoa(int(w.UsedPercent+0.5)))
				if tc.reset && w.ResetsAt.IsZero() {
					t.Errorf("window %d has no reset time", w.Minutes)
				}
			}
			if strings.Join(got, " ") != tc.want || u.Plan != tc.plan {
				t.Fatalf("got %q plan %q, want %q plan %q", strings.Join(got, " "), u.Plan, tc.want, tc.plan)
			}
		})
	}
}

func TestFetchSendsEachProvidersCredentialItsOwnWay(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		switch {
		case strings.HasSuffix(r.URL.Path, "/limit"):
			w.Write([]byte(`{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":12}]}}`))
		case strings.HasSuffix(r.URL.Path, "/wham/usage"):
			w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`))
		case strings.HasSuffix(r.URL.Path, "/billing"):
			w.Write([]byte(`{"config":{"creditUsagePercent":9}}`))
		default:
			w.Write([]byte(`{"surprise":true}`))
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	tok := model.Token{Access: "sk-secret", AccountID: "acct"}

	if _, err := Fetch(ctx, srv.Client(), "zai", URL("zai", srv.URL+"/api/coding/paas/v4"), tok); err != nil || got.Get("Authorization") != "sk-secret" {
		t.Fatalf("zai: %v, Authorization %q (the raw key, no Bearer)", err, got.Get("Authorization"))
	}
	if u, err := Fetch(ctx, srv.Client(), "openai", URL("openai", srv.URL+"/backend-api/codex/responses"), tok); err != nil || u.Plan != "plus" || got.Get("ChatGPT-Account-Id") != "acct" || got.Get("Authorization") != "Bearer sk-secret" {
		t.Fatalf("openai: %v %+v %v", err, u, got)
	}
	if _, err := Fetch(ctx, srv.Client(), "xai", URL("xai", srv.URL+"/v1"), tok); err != nil || got.Get("x-grok-client-surface") != "grok-build" {
		t.Fatalf("xai: %v %v", err, got)
	}
	// a shape nobody has seen names its keys and never the credential
	_, err := Fetch(ctx, srv.Client(), "kimi", URL("kimi", srv.URL+"/coding/v1"), tok)
	if err == nil || !strings.Contains(err.Error(), "keys: surprise") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("kimi surprise: %v", err)
	}
	if u, err := Fetch(ctx, srv.Client(), "zai", URL("zai", srv.URL), tok); err != nil || u.Observed.IsZero() {
		t.Fatalf("observed: %+v %v", u, err)
	}
}
