package format

import (
	"os"
	"testing"
	"time"
)

func TestFormat(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ got, want string }{
		{Tokens(950), "950"}, {Tokens(1499), "1k"}, {Tokens(12_400), "12k"}, {Tokens(1_000_000), "1m"}, {Tokens(1_250_000), "1.2m"},
		{Who("main", "coder"), "main (coder)"}, {Who("main", ""), "main"},
		{Cost(0.5), "0.50"}, {Cost(0.1234), "0.1234"}, {Cost(0.123), "0.123"}, {Cost(2), "2.00"},
		{Elapsed(9 * time.Second), "9s"}, {Elapsed(65 * time.Second), "1m05s"}, {Elapsed(62 * time.Minute), "1h02m"},
		{ShortID("0123456789"), "01234567"}, {ShortID("abc"), "abc"},
		{Trunc("héllo", 3), "hél…"}, {Trunc("hi", 3), "hi"},
		{FirstLine("  one\ntwo"), "one…"}, {FirstLine("one "), "one"},
		{Ago(now.Add(-59*time.Second), now), "now"}, {Ago(now.Add(time.Minute), now), "now"},
		{Ago(now.Add(-time.Minute), now), "1m"}, {Ago(now.Add(-59*time.Minute), now), "59m"},
		{Ago(now.Add(-time.Hour), now), "1h"}, {Ago(now.Add(-23*time.Hour), now), "23h"},
		{Ago(now.Add(-24*time.Hour), now), "1d"}, {Ago(now.Add(-71*time.Hour), now), "2d"},
		{Ago(now.Add(-72*time.Hour), now), "Sept 14"}, {Ago(time.Date(2025, 9, 1, 8, 0, 0, 0, time.UTC), now), "Sept 1 2025"},
	} {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		if got := ShortHome(h + "/x"); got != "~/x" {
			t.Errorf("ShortHome: %q", got)
		}
	}
	if got := ShortHome("/elsewhere"); got != "/elsewhere" && got != "~/elsewhere" {
		t.Errorf("ShortHome outside home: %q", got)
	}
}
