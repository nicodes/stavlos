package quota

import (
	"testing"
	"time"
)

// FuzzParsers: a provider's usage reply is undocumented and has changed
// shape before. Whatever arrives, no parser panics, and every window it
// reports is a percentage.
func FuzzParsers(f *testing.F) {
	for _, s := range []string{
		`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":38,"limit_window_seconds":18000,"reset_at":1800000000},"secondary_window":null}}`,
		`{"code":200,"data":{"limits":[{"type":"CREDIT_LIMIT","unit":3,"usage":2000,"currentValue":500,"nextResetTime":1.8e12}]}}`,
		`{"usage":{"limit":"1000","used":"500","resetTime":"2027-01-15T00:00:00Z"},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":200,"remaining":180}}]}`,
		`{"usages":{"limit_5h":{"used_ratio":0.07,"reset_time":1800000000}}}`,
		`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-07-20T02:24:00Z"},"creditUsagePercent":5}}`,
		`{}`, `[]`, `null`, `{"data":null}`, `{"limits":[null,1,"x"]}`, `{"usage":{"limit":0,"used":1}}`, `{"usage":{"limit":-5,"used":1e308}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		for name, src := range sources {
			u, err := src.parse(body, time.Unix(1_790_000_000, 0))
			if err != nil {
				continue
			}
			for _, w := range u.Windows {
				if !(w.UsedPercent >= 0 && w.UsedPercent <= 100) || w.Minutes < 0 {
					t.Fatalf("%s: window %+v from %q", name, w, body)
				}
			}
		}
	})
}
