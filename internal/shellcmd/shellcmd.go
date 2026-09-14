// Package shellcmd classifies shell command lines for permission decisions:
// whether a command is one simple command, which prefix of it a human may
// allow for a session, and whether such a prefix covers a later command.
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
// session would allow everything.
var wrappers = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "fish": true, "ksh": true,
	"env": true, "xargs": true, "sudo": true, "doas": true, "su": true,
	"eval": true, "exec": true, "command": true, "builtin": true, "source": true, ".": true,
	"time": true, "nohup": true, "nice": true, "ionice": true, "timeout": true, "watch": true, "setsid": true, "chroot": true, "strace": true, "ltrace": true,
	"python": true, "python2": true, "python3": true, "node": true, "deno": true, "perl": true, "ruby": true, "php": true, "lua": true, "awk": true, "gawk": true,
	"ssh": true, "script": true, "screen": true, "tmux": true,
}

// Words splits cmd into its words the way a POSIX shell would for one
// simple command: single quotes, double quotes and backslashes group, and
// the quotes are removed. It reports ok=false when cmd is not one simple
// command: an unquoted control operator (; | & newline), a redirection
// (< > >> << &> |&), a subshell or group (parentheses, braces as a word),
// or a substitution (backticks, $( …), also inside double quotes) makes
// the command more than what its first words say.
func Words(cmd string) (words []string, ok bool) {
	var cur strings.Builder
	inWord := false
	const (
		none = iota
		single
		double
	)
	state := none
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch state {
		case single:
			if r == '\'' {
				state = none
			} else {
				cur.WriteRune(r)
			}
		case double:
			switch r {
			case '"':
				state = none
			case '\\':
				// In double quotes a backslash escapes only $ " \ ` and
				// newline; before anything else it is kept.
				if i+1 < len(rs) && strings.ContainsRune("$\"\\`\n", rs[i+1]) {
					i++
					if rs[i] != '\n' {
						cur.WriteRune(rs[i])
					}
				} else {
					cur.WriteRune(r)
				}
			case '`':
				return nil, false
			case '$':
				if i+1 < len(rs) && rs[i+1] == '(' {
					return nil, false
				}
				cur.WriteRune(r)
			default:
				cur.WriteRune(r)
			}
		default:
			switch r {
			case ' ', '\t':
				flush()
			case '\n', '\r', ';', '|', '&', '<', '>', '(', ')', '`':
				return nil, false
			case '\'':
				state = single
				inWord = true
			case '"':
				state = double
				inWord = true
			case '\\':
				if i+1 < len(rs) {
					i++
					if rs[i] == '\n' {
						flush()
						continue
					}
					cur.WriteRune(rs[i])
					inWord = true
				}
			case '$':
				if i+1 < len(rs) && rs[i+1] == '(' {
					return nil, false
				}
				cur.WriteRune(r)
				inWord = true
			case '{', '}':
				// A brace as a word of its own is a command group; inside a
				// word it is brace expansion, which stays one command.
				if !inWord && (i+1 == len(rs) || rs[i+1] == ' ' || rs[i+1] == '\t') {
					return nil, false
				}
				cur.WriteRune(r)
				inWord = true
			default:
				cur.WriteRune(r)
				inWord = true
			}
		}
	}
	if state != none {
		return nil, false // unterminated quote
	}
	flush()
	return words, true
}

// Simple reports whether cmd is one simple command (see Words).
func Simple(cmd string) bool {
	w, ok := Words(cmd)
	return ok && len(w) > 0
}

// Prefix is the part of a command a human may allow for the rest of a
// session: its first word, or two words for tools like git and go ("go
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
