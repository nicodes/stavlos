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

// Commands lists the commands a command line runs, best effort, in the
// form a rule should judge them: each one's words joined by single spaces,
// with leading VAR=value assignments and launcher words (sudo, env, nice,
// timeout 5, …) dropped, the program also by its base name (/bin/rm → rm),
// and the script of sh -c / bash -c expanded. A deny rule on "rm -rf *"
// thereby also speaks for "cd x && FOO=1 sudo /bin/rm  -rf /". It is a
// classifier's view, not a shell's: it may find commands that are not
// there, never fewer than a plain reading shows.
func Commands(cmd string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	var visit func(cmd string, depth int)
	visit = func(cmd string, depth int) {
		segs := []string{cmd}
		if _, ok := Words(cmd); !ok {
			segs = segments(cmd)
		}
		for _, seg := range segs {
			words, ok := Words(seg)
			if !ok {
				words = strings.Fields(seg)
			}
			words = launched(words)
			if len(words) == 0 {
				continue
			}
			add(strings.Join(words, " "))
			if base := path.Base(words[0]); base != words[0] {
				add(strings.Join(append([]string{base}, words[1:]...), " "))
			}
			if script := shellScript(words); script != "" && depth < 4 {
				visit(script, depth+1)
			}
		}
	}
	visit(cmd, 0)
	return out
}

// segments splits a line that is not one simple command at every unquoted
// control operator, parenthesis, brace word, backtick and $( — the places a
// new command may start.
func segments(cmd string) []string {
	var out []string
	var cur strings.Builder
	cut := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	rs := []rune(cmd)
	state := quoteNone
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch state {
		case quoteSingle:
			cur.WriteRune(r)
			if r == '\'' {
				state = quoteNone
			}
			continue
		case quoteDouble:
			switch {
			case r == '"':
				state = quoteNone
				cur.WriteRune(r)
			case r == '\\' && i+1 < len(rs):
				cur.WriteRune(r)
				i++
				cur.WriteRune(rs[i])
			case r == '`', r == '$' && i+1 < len(rs) && rs[i+1] == '(':
				// A substitution inside double quotes runs a command too.
				cut()
				state = quoteNone
				if r == '$' {
					i++
				}
			default:
				cur.WriteRune(r)
			}
			continue
		case quoteNone:
		}
		switch {
		case r == '\'':
			state = quoteSingle
			cur.WriteRune(r)
		case r == '"':
			state = quoteDouble
			cur.WriteRune(r)
		case r == '\\' && i+1 < len(rs):
			cur.WriteRune(r)
			i++
			cur.WriteRune(rs[i])
		case strings.ContainsRune(";&|\n\r()`", r), r == '$' && i+1 < len(rs) && rs[i+1] == '(':
			cut()
			if r == '$' {
				i++
			}
		case (r == '{' || r == '}') && (i == 0 || rs[i-1] == ' ' || rs[i-1] == '\t') && (i+1 == len(rs) || rs[i+1] == ' ' || rs[i+1] == '\t' || rs[i+1] == ';'):
			cut()
		default:
			cur.WriteRune(r)
		}
	}
	cut()
	return out
}

// launchers run the command that follows them; the value is how many
// non-flag arguments of their own they take first ("timeout 5 rm").
var launchers = map[string]int{
	"sudo": 0, "doas": 0, "env": 0, "nice": 0, "nohup": 0, "time": 0, "command": 0, "exec": 0, "builtin": 0,
	"setsid": 0, "stdbuf": 0, "ionice": 0, "xargs": 0, "chronic": 0, "unbuffer": 0, "timeout": 1, "chroot": 1,
}

// launched drops leading assignments and launcher words (with their flags
// and own arguments) from a command's words.
func launched(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		if isAssignment(w) {
			words = words[1:]
			continue
		}
		n, ok := launchers[path.Base(w)]
		if !ok {
			return words
		}
		words = words[1:]
		for len(words) > 0 && (strings.HasPrefix(words[0], "-") || isAssignment(words[0])) {
			words = words[1:]
		}
		for ; n > 0 && len(words) > 0; n-- {
			words = words[1:]
		}
	}
	return words
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range w[:eq] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// shellScript is the script a shell is asked to run with -c ("" when the
// command is not a shell given -c).
func shellScript(words []string) string {
	switch path.Base(words[0]) {
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
	default:
		return ""
	}
	for i := 1; i < len(words); i++ {
		if w := words[i]; strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "c") && i+1 < len(words) {
			return words[i+1]
		}
	}
	return ""
}
