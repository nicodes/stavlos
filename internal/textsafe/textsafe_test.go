package textsafe

import "testing"

func TestClean(t *testing.T) {
	cases := map[string]string{
		"plain text\nwith\ttabs":                         "plain text\nwith\ttabs",
		"rm -rf ~ \x1b[2K\x1b[1Gls -la":                  "rm -rf ~ ls -la",
		"\x1b[31mred\x1b[0m":                             "red",
		"\x1b[?25l hide cursor":                          " hide cursor",
		"title\x1b]0;evil\x07 after":                     "title after",
		"title\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\": "titlelink",
		"\x1bPdcs body\x1b\\ tail":                       " tail",
		"\x1b7save\x1b8":                                 "save",
		"\x1b(Bcharset":                                  "charset",
		"c1 \x9b2Kcsi \x9d0;osc\x07 done":                "c1 csi  done",
		"bell\x07 and del\x7f and nul\x00 end":           "bell and del and nul end",
		"cr\r\nlf":                                       "cr\nlf",
		"runaway \x1b]0;no terminator\nnext line":        "runaway \nnext line",
		"unicode ünïcödé — ok":                           "unicode ünïcödé — ok",
		"\x1b":                                           "",
		"\x1b[":                                          "",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q want %q", in, got, want)
		}
	}
}

func TestVisible(t *testing.T) {
	cases := map[string]string{
		"rm -rf ~ \x1b[2K\x1b[1Gls -la": "rm -rf ~ ^[^[ls -la",
		"a\x01b\x7fc\x9b2Kd":            "a^Ab^?c<9b>d",
		"clean":                         "clean",
	}
	for in, want := range cases {
		if got := Visible(in); got != want {
			t.Errorf("Visible(%q) = %q want %q", in, got, want)
		}
	}
}
