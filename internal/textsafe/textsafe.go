// Package textsafe strips terminal control from text that was produced
// somewhere else — a model's reply, a tool's output, a page from the web —
// before a terminal draws it. Escape sequences are zero-width to the
// wrapping and truncation code, so without this a command could erase its
// own destructive half from the permission dialog, or retitle the window.
package textsafe

import (
	"strings"
	"unicode/utf8"
)

// Clean returns s without escape sequences (CSI, OSC, DCS, PM, APC, SOS and
// two-character ESC sequences, terminated by BEL or ST where applicable),
// C0 controls other than newline and tab, DEL, and C1 controls. Text is
// otherwise unchanged; a string with nothing to remove is returned as is.
func Clean(s string) string { return scrub(s, false) }

// Visible is Clean with each removed control shown as a caret sequence
// ("^[" for ESC, "^A" for 0x01, "^?" for DEL, "<9b>" for a C1 byte), so a
// human reading a permission dialog can see that something was hidden.
func Visible(s string) string { return scrub(s, true) }

func scrub(s string, visible bool) string {
	if !strings.ContainsFunc(s, isControl) && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 && s[i] >= 0x80 && s[i] <= 0x9f {
			r = rune(s[i]) // a raw C1 byte (not UTF-8): a control all the same
		}
		if !isControl(r) {
			b.WriteString(s[i : i+size])
			i += size
			continue
		}
		if visible {
			b.WriteString(caret(r))
		}
		i += size
		if r == 0x1b || r == 0x9b || r == 0x9d || r == 0x90 || r == 0x9e || r == 0x9f || r == 0x98 {
			i += sequenceLen(s[i:], r)
		}
	}
	return b.String()
}

// isControl reports whether r is a C0 control (other than newline and
// tab), DEL, or a C1 control.
func isControl(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20 || r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	}
	return false
}

// sequenceLen is how many bytes of s (the text after an introducer) belong
// to the escape sequence the introducer started.
func sequenceLen(s string, intro rune) int {
	if intro == 0x1b {
		if s == "" {
			return 0
		}
		switch s[0] {
		case '[':
			return 1 + csiLen(s[1:])
		case ']':
			return 1 + stringLen(s[1:])
		case 'P', '^', '_', 'X':
			return 1 + stringLen(s[1:])
		default:
			// A two-character sequence (ESC 7, ESC =, ESC ( B …): the next
			// byte, plus one more for the charset designators.
			n := 1
			if (s[0] == '(' || s[0] == ')' || s[0] == '*' || s[0] == '+' || s[0] == '#') && len(s) > 1 {
				n = 2
			}
			return n
		}
	}
	if intro == 0x9b { // C1 CSI
		return csiLen(s)
	}
	return stringLen(s) // C1 OSC, DCS, PM, APC, SOS
}

// csiLen consumes parameter and intermediate bytes (0x20–0x3f) up to and
// including the final byte (0x40–0x7e).
func csiLen(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x40 && c <= 0x7e {
			return i + 1
		}
		if c < 0x20 || c > 0x3f {
			return i // malformed: stop before the stray byte, which is then judged on its own
		}
	}
	return len(s)
}

// stringLen consumes an OSC/DCS/PM/APC/SOS body up to and including its
// terminator: BEL, ST (ESC \ or U+009C), or a bare ESC (left in place so
// the next sequence is judged on its own).
func stringLen(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 0x07:
			return i + 1
		case 0x1b:
			if i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			return i
		case 0xc2:
			if i+1 < len(s) && s[i+1] == 0x9c { // U+009C ST
				return i + 2
			}
		case 0x9c: // a raw ST byte
			return i + 1
		case '\n':
			return i // a line break ends a runaway sequence: the text after it is text
		}
	}
	return len(s)
}

func caret(r rune) string {
	switch {
	case r == 0x7f:
		return "^?"
	case r < 0x20:
		return "^" + string(rune('@'+r))
	default:
		return "<" + strings.ToLower(strings.TrimLeft(strings.ToUpper(hex(r)), "0")) + ">"
	}
}

func hex(r rune) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[(r>>4)&0xf], digits[r&0xf]})
}
