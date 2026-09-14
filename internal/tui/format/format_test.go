package format

import (
	"os"
	"testing"
	"time"
)

func TestFormat(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{Tokens(950), "950"}, {Tokens(1499), "1k"}, {Tokens(12_400), "12k"}, {Tokens(1_000_000), "1m"}, {Tokens(1_250_000), "1.2m"},
		{Cost(0.5), "0.50"}, {Cost(0.1234), "0.1234"}, {Cost(0.123), "0.123"}, {Cost(2), "2.00"},
		{Elapsed(9 * time.Second), "9s"}, {Elapsed(65 * time.Second), "1m05s"}, {Elapsed(62 * time.Minute), "1h02m"},
		{ShortID("0123456789"), "01234567"}, {ShortID("abc"), "abc"},
		{Trunc("héllo", 3), "hél…"}, {Trunc("hi", 3), "hi"},
		{FirstLine("  one\ntwo"), "one…"}, {FirstLine("one "), "one"},
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
