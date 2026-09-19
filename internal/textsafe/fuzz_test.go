package textsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzClean: nothing a model, a tool or a web page sends can reach the
// terminal as a control sequence or a direction override, cleaning twice
// changes nothing more, and the result is valid UTF-8.
func FuzzClean(f *testing.F) {
	for _, s := range []string{"plain", "\x1b[31mred\x1b[0m", "\x1b]0;title\x07", "a\u202eb", "\x9b31m", "tab\there\n", "\xff\xfe", "\u2066x\u2069", "\r\x00\x7f"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for name, clean := range map[string]func(string) string{"Clean": Clean, "Visible": Visible} {
			out := clean(s)
			if !utf8.ValidString(out) {
				t.Fatalf("%s(%q) is not valid UTF-8: %q", name, s, out)
			}
			if i := strings.IndexFunc(out, func(r rune) bool {
				return r == 0x1b || r == 0x07 || r == 0 || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
			}); i >= 0 {
				t.Fatalf("%s(%q) let a control through at %d: %q", name, s, i, out)
			}
			if again := clean(out); again != out {
				t.Fatalf("%s is not idempotent: %q then %q", name, out, again)
			}
		}
	})
}
