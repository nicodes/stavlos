// Package shellcmd classifies shell command lines for permission decisions:
// whether a command is one simple command, which prefix of it a human may
// allow for a channel, and whether such a prefix covers a later command.
//
// This is a classifier, not a shell. It errs towards "not simple": anything
// that could chain, redirect, substitute or hand control to an interpreter
// is refused, and a refused command is one the human sees whole in the
// permission dialog and answers for itself.
package shellcmd

import (
	"path"
	"strings"
)

// twoWordTools are commands whose first word says little on its own: "go"
// covers build, test and run alike, so their prefix takes the subcommand too.
var twoWordTools = map[string]bool{
	"git": true, "go": true, "npm": true, "npx": true, "cargo": true, "make": true,
	"docker": true, "kubectl": true, "pip": true, "pip3": true, "yarn": true, "pnpm": true, "bun": true,
}

// wrappers run whatever follows them: allowing "bash" or "env" for a
// channel would allow everything.
var wrappers = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "fish": true, "ksh": true,
	"env": true, "xargs": true, "sudo": true, "doas": true, "su": true,
	"eval": true, "exec": true, "command": true, "builtin": true, "source": true, ".": true,
	"time": true, "nohup": true, "nice": true, "ionice": true, "timeout": true, "watch": true, "setsid": true, "chroot": true, "strace": true, "ltrace": true,
	"python": true, "python2": true, "python3": true, "node": true, "deno": true, "perl": true, "ruby": true, "php": true, "lua": true, "awk": true, "gawk": true,
	"ssh": true, "script": true, "screen": true, "tmux": true,
}

// Words splits cmd into its words when it is one simple command: single
// quotes, double quotes and backslashes group, and the quotes are removed.
// It reports ok=false when cmd is not one simple command: an unquoted
// control operator (; | & newline), a redirection (< > >> << &> |&), a
// subshell or group (parentheses, braces as a word), or a substitution
// (backticks, $( …), also inside double quotes) makes the command more
// than what its first words say.
func Words(cmd string) (words []string, ok bool) {
	w := wordScanner{rs: []rune(cmd)}
	for ; w.i < len(w.rs); w.i++ {
		switch w.state {
		case quoteSingle:
			w.single()
			ok = true
		case quoteDouble:
			ok = w.double()
		default:
			ok = w.bare()
		}
		if !ok {
			return nil, false
		}
	}
	if w.state != quoteNone {
		return nil, false // unterminated quote
	}
	w.flush()
	return w.words, true
}

type quoteState int

const (
	quoteNone quoteState = iota
	quoteSingle
	quoteDouble
)

// wordScanner walks a command line rune by rune for Words.
type wordScanner struct {
	rs     []rune
	i      int
	state  quoteState
	cur    strings.Builder
	inWord bool
	words  []string
}

func (w *wordScanner) flush() {
	if w.inWord {
		w.words = append(w.words, w.cur.String())
		w.cur.Reset()
		w.inWord = false
	}
}

// nextIs reports whether the rune after the current one is r.
func (w *wordScanner) nextIs(r rune) bool { return w.i+1 < len(w.rs) && w.rs[w.i+1] == r }

// single takes a rune inside single quotes.
func (w *wordScanner) single() {
	if r := w.rs[w.i]; r == '\'' {
		w.state = quoteNone
	} else {
		w.cur.WriteRune(r)
	}
}

// double takes a rune inside double quotes; false when it starts a
// substitution.
func (w *wordScanner) double() bool {
	r := w.rs[w.i]
	switch r {
	case '"':
		w.state = quoteNone
	case '\\':
		// In double quotes a backslash escapes only $ " \ ` and newline;
		// before anything else it is kept.
		if w.i+1 < len(w.rs) && strings.ContainsRune("$\"\\`\n", w.rs[w.i+1]) {
			w.i++
			if w.rs[w.i] != '\n' {
				w.cur.WriteRune(w.rs[w.i])
			}
		} else {
			w.cur.WriteRune(r)
		}
	case '`':
		return false
	case '$':
		if w.nextIs('(') {
			return false
		}
		w.cur.WriteRune(r)
	default:
		w.cur.WriteRune(r)
	}
	return true
}

// bare takes an unquoted rune; false when it makes the line more than one
// simple command.
func (w *wordScanner) bare() bool {
	r := w.rs[w.i]
	switch r {
	case ' ', '\t':
		w.flush()
		return true
	case '\n', '\r', ';', '|', '&', '<', '>', '(', ')', '`':
		return false
	case '\'':
		w.state = quoteSingle
	case '"':
		w.state = quoteDouble
	case '\\':
		if w.i+1 >= len(w.rs) {
			return true
		}
		w.i++
		if w.rs[w.i] == '\n' {
			w.flush()
			return true
		}
		w.cur.WriteRune(w.rs[w.i])
	case '$':
		if w.nextIs('(') {
			return false
		}
		w.cur.WriteRune(r)
	case '{', '}':
		// A brace as a word of its own is a command group; inside a word it
		// is brace expansion, which stays one command.
		if !w.inWord && (w.i+1 == len(w.rs) || w.rs[w.i+1] == ' ' || w.rs[w.i+1] == '\t') {
			return false
		}
		w.cur.WriteRune(r)
	default:
		w.cur.WriteRune(r)
	}
	w.inWord = true
	return true
}

// Simple reports whether cmd is one simple command (see Words).
func Simple(cmd string) bool {
	w, ok := Words(cmd)
	return ok && len(w) > 0
}

// Prefix is the part of a command a human may allow for the rest of a
// channel: its first word, or two words for tools like git and go ("go
// test") when the second is a subcommand rather than a flag. It is "" when
// no prefix would mean what it says: a compound command, an environment
// assignment, a wrapper or interpreter (bash, env, sudo, python…), or a
// two-word tool whose second word is a flag ("go -C x test" is not "go").
func Prefix(cmd string) string {
	w, ok := Words(cmd)
	if !ok || len(w) == 0 {
		return ""
	}
	first := w[0]
	if first == "" || strings.Contains(first, "=") || first == "!" {
		return ""
	}
	if wrappers[path.Base(first)] {
		return ""
	}
	if twoWordTools[first] {
		if len(w) < 2 || strings.HasPrefix(w[1], "-") || w[1] == "" {
			return ""
		}
		return first + " " + w[1]
	}
	return first
}

// Covers reports whether an allowed prefix covers cmd: cmd is one simple
// command whose first words are the prefix's, word for word.
func Covers(prefix, cmd string) bool {
	pw := strings.Fields(prefix)
	if len(pw) == 0 {
		return false
	}
	cw, ok := Words(cmd)
	if !ok || len(cw) < len(pw) {
		return false
	}
	for i, p := range pw {
		if cw[i] != p {
			return false
		}
	}
	return true
}
