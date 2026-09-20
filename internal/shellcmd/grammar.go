package shellcmd

import (
	"path"
	"strings"
)

// What follows is the part of the shell's grammar a classifier needs so
// that a rule written about a program ("deny rm -rf *") still speaks for
// that program when the line wraps it: in a compound command's keywords,
// behind a launcher and its flags, in a string handed to eval or a shell,
// or behind a variable set on the same line.

// keywords are reserved words that stand before a command without being
// one: "then rm -rf /" runs rm.
var keywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"while": true, "until": true, "do": true, "done": true,
	"!": true, "{": true, "}": true, "time": true, "coproc": true,
}

// launcher describes a program that runs the command that follows it.
type launcher struct {
	args  int    // non-flag arguments of its own it takes first ("timeout 5 rm")
	flags string // its short flags that take a value as the next word ("sudo -u root rm")
}

var launchers = map[string]launcher{
	"sudo":     {flags: "ughpCDRTUr"},
	"doas":     {flags: "uC"},
	"env":      {flags: "uCS"},
	"nice":     {flags: "n"},
	"ionice":   {flags: "cnpPu"},
	"timeout":  {args: 1, flags: "ks"},
	"chroot":   {args: 1},
	"stdbuf":   {flags: "ioe"},
	"xargs":    {flags: "IndPLEsa"},
	"nohup":    {},
	"command":  {},
	"exec":     {flags: "a"},
	"builtin":  {},
	"setsid":   {},
	"chronic":  {},
	"unbuffer": {},
}

// launched drops what stands before the program: keywords, leading
// assignments, and launcher words with their flags and own arguments.
func launched(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		if keywords[w] || isAssignment(w) {
			words = words[1:]
			continue
		}
		l, ok := launchers[path.Base(w)]
		if !ok {
			return words
		}
		words = l.skip(words[1:])
	}
	return words
}

// skip drops a launcher's flags, their values, and its own arguments.
func (l launcher) skip(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		if !isAssignment(w) && (len(w) < 2 || w[0] != '-') { // env A=b cmd
			break
		}
		words = words[1:]
		// "-u root" takes the next word; "-uroot" carries its value.
		if len(w) == 2 && w != "--" && strings.ContainsRune(l.flags, rune(w[1])) && len(words) > 0 {
			words = words[1:]
		}
	}
	for n := l.args; n > 0 && len(words) > 0; n-- {
		words = words[1:]
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

// scripts are the strings a command hands to a shell to run: the argument
// of sh -c, everything after eval, and a here-string fed to a shell.
func scripts(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	switch path.Base(words[0]) {
	case "eval":
		return []string{strings.Join(words[1:], " ")}
	case "sh", "bash", "zsh", "dash", "ksh", "fish":
	default:
		return nil
	}
	var out []string
	for i := 1; i+1 < len(words); i++ {
		w := words[i]
		switch {
		case w == "<<<": // read as fields, since a redirection is not a simple command
			out = append(out, strings.Trim(strings.Join(words[i+1:], " "), "\"'"))
		case strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "c"):
			out = append(out, words[i+1])
		}
	}
	return out
}

// bind records the literal assignments at the head of a command
// ("a=rm; $a -rf /"), and expand writes them back into a later one. A
// value that is not a plain literal is not recorded: the word that uses it
// stays as written and matches nothing, which is the honest answer.
func bind(vars map[string]string, words []string) {
	for _, w := range words {
		if !isAssignment(w) {
			return
		}
		name, value, _ := strings.Cut(w, "=")
		if strings.ContainsAny(value, "$`") {
			delete(vars, name)
			continue
		}
		vars[name] = value
	}
}

func expand(vars map[string]string, words []string) []string {
	if len(vars) == 0 {
		return words
	}
	var out []string
	for _, w := range words {
		if !strings.Contains(w, "$") {
			out = append(out, w)
			continue
		}
		for name, value := range vars {
			w = strings.ReplaceAll(w, "${"+name+"}", value)
		}
		w = expandBare(vars, w)
		// An unquoted expansion splits into words: a="rm -rf"; $a /
		out = append(out, strings.Fields(w)...)
	}
	return out
}

// expandBare replaces $name, taking the longest name the shell would.
func expandBare(vars map[string]string, w string) string {
	var b strings.Builder
	for i := 0; i < len(w); i++ {
		if w[i] != '$' {
			b.WriteByte(w[i])
			continue
		}
		j := i + 1
		for j < len(w) && (w[j] == '_' || w[j] >= 'A' && w[j] <= 'Z' || w[j] >= 'a' && w[j] <= 'z' || j > i+1 && w[j] >= '0' && w[j] <= '9') {
			j++
		}
		if value, ok := vars[w[i+1:j]]; ok {
			b.WriteString(value)
			i = j - 1
			continue
		}
		b.WriteByte(w[i])
	}
	return b.String()
}
