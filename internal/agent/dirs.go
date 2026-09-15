package agent

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/proc"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// Working directories (PRD §10.7): the channel has one working set, shared
// by every agent: the channel directory and what the human adds, in the dirs
// tab or by answering a boundary prompt. A tool call that reaches outside
// the set asks first, even when policy allows the tool. File paths are
// resolved the way the tools open them (tools.ResolvePath); shell commands
// are inspected as text, and the sandbox enforces the boundary for writes.

type dirEntry struct{ path, source string }

// resolveDir makes d absolute: ~, ~user and ${env:NAME} expand, a relative
// path is taken from base, and the result is cleaned.
func resolveDir(base, d string) string {
	d = config.ExpandEnv(strings.TrimSpace(d))
	if rest, ok := strings.CutPrefix(d, "~"); ok {
		name, tail, _ := strings.Cut(rest, "/")
		if h := homeOf(name); h != "" {
			d = filepath.Join(h, tail)
		}
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(base, d)
	}
	return filepath.Clean(d)
}

// dirPaths is the working set: the channel directory, then the added
// directories in the order they came.
func (c *Channel) dirPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirPathsLocked()
}

func (c *Channel) dirPathsLocked() []string {
	out := []string{c.Dir}
	for _, d := range c.st.dirs {
		out = append(out, d.path)
	}
	return out
}

func (c *Channel) dirInfosLocked() []protocol.DirInfo {
	out := []protocol.DirInfo{{Path: c.Dir, Source: "channel"}}
	for _, d := range c.st.dirs {
		out = append(out, protocol.DirInfo{Path: d.path, Source: d.source})
	}
	return out
}

// inDirs reports whether an absolute path lies in dirs. Both sides are
// compared with symlinks resolved, so a working directory that is itself a
// link (or reached through one) still contains its files.
func inDirs(dirs []string, p string) bool {
	p = tools.ResolvePath("", p)
	for _, d := range dirs {
		dir := tools.ResolvePath("", d)
		if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// addDir puts a directory in the working set, logged on agent (the one
// whose boundary prompt added it, "" for the dirs tab). A directory already
// inside the set is a no-op.
func (c *Channel) addDir(ctx context.Context, agent, dir, source string) error {
	dir = filepath.Clean(dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	if inDirs(c.dirPathsLocked(), dir) {
		return nil
	}
	_, err := c.commitLocked(ctx, c.event(agent, event.ChannelDirAdded, event.DirPayload{Dir: dir, Source: source}))
	return err
}

// AddDir is the human's add (an absolute path, ~, or a path relative to the
// channel directory).
func (c *Channel) AddDir(ctx context.Context, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("a directory is required")
	}
	return c.addDir(ctx, "", resolveDir(c.Dir, dir), "human")
}

// RemoveDir takes a directory out of the working set. The channel
// directory stays.
func (c *Channel) RemoveDir(ctx context.Context, dir string) error {
	dir = resolveDir(c.Dir, dir)
	if dir == filepath.Clean(c.Dir) {
		return fmt.Errorf("the channel directory cannot be removed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.ContainsFunc(c.st.dirs, func(e dirEntry) bool { return e.path == dir }) {
		return fmt.Errorf("%s is not one of the channel's directories", dir)
	}
	_, err := c.commitLocked(ctx, c.event("", event.ChannelDirRemoved, event.DirPayload{Dir: dir}))
	return err
}

// outsideDir returns the first directory a tool call reaches outside dirs
// ("" when it stays inside), as a boundary prompt would offer to add it.
func outsideDir(sub policy.Subject, base string, dirs []string) string {
	var paths []string
	switch sub.Kind {
	case policy.KindCommand:
		paths = bashPathCandidates(sub.Primary(), base)
	case policy.KindPath:
		for _, p := range sub.Values {
			paths = append(paths, tools.ResolvePath(base, p)) // as the tool will open it: no ~ or ${env:} for a model's path
		}
	case policy.KindText, policy.KindURL, policy.KindID:
	}
	for _, p := range paths {
		if p != "" && !inDirs(dirs, p) {
			return grantDir(p)
		}
	}
	return ""
}

// grantDir is the directory a boundary prompt offers to add for a path:
// the git checkout containing it when there is one (the repository is the
// unit people think in), else the path itself when it is a directory, else
// its parent. A checkout rooted at the home directory does not count.
func grantDir(p string) string {
	base := p
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		base = filepath.Dir(p)
	}
	home, _ := os.UserHomeDir()
	for d := base; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			if d == home || d == "/" {
				return base
			}
			return d
		}
		if d == "/" || d == filepath.Dir(d) {
			return base
		}
	}
}

// bashPathCandidates picks the paths a shell command line may touch, best
// effort: absolute, ~ and ~user, and parent-relative (../x) arguments (also
// after --flag=), variables expanded as the command's shell would see them
// (an unset one expands to nothing, so "$X/etc" is "/etc"), relative
// arguments that lead outside through a symlink, cd targets and redirect
// targets (relative ones taken from base). The program of each simple
// command and /dev/* are skipped; a glob is cut at its first wildcard. The
// sandbox is the boundary for writes; this is what makes a command ask.
func bashPathCandidates(cmd, base string) []string {
	var out []string
	add := func(p string) {
		p = strings.Trim(p, "\"'")
		if i := strings.IndexAny(p, "*?["); i >= 0 {
			p = p[:i]
			if j := strings.LastIndex(p, "/"); j >= 0 {
				p = p[:j]
			}
		}
		if p == "" || strings.HasPrefix(p, "/dev/") || p == "/dev" || p == "-" {
			return
		}
		out = append(out, resolveDir(base, p))
	}
	// Separators become tokens of their own, so "cd x; ls" splits cleanly.
	sep := strings.NewReplacer("&&", " && ", "||", " || ", ";", " ; ", "|", " | ")
	toks := strings.Fields(sep.Replace(cmd))
	startOfCommand := true
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		switch {
		case tok == "|", tok == "||", tok == "&&", tok == ";", tok == "&":
			startOfCommand = true
			continue
		case tok == ">", tok == ">>", tok == "<", tok == "2>", tok == "&>", tok == "2>>":
			if i+1 < len(toks) {
				add(toks[i+1])
				i++
			}
			continue
		}
		if k := strings.TrimLeft(tok, "12&"); strings.HasPrefix(k, ">") || strings.HasPrefix(k, "<") {
			add(strings.TrimLeft(k, "><"))
			continue
		}
		if startOfCommand {
			startOfCommand = false
			if tok == "cd" && i+1 < len(toks) {
				add(toks[i+1])
				i++
			}
			continue // the program itself (/usr/bin/env, ./script.sh…) is not a data path
		}
		val := tok
		if eq := strings.IndexByte(tok, '='); eq >= 0 && strings.HasPrefix(tok, "-") {
			val = tok[eq+1:]
		}
		q := expandShellVars(strings.Trim(val, "\"'"), base)
		switch {
		case strings.HasPrefix(q, "/"), strings.HasPrefix(q, "~"), q == "..", strings.HasPrefix(q, "../"), strings.Contains(q, "/../"):
			add(q)
		case q != "" && !strings.HasPrefix(q, "-"):
			// A relative argument that resolves outside the working
			// directory went through a symlink: judge where it leads.
			root := tools.ResolvePath(base, "")
			if real := tools.ResolvePath(base, q); real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
				add(real)
			}
		}
	}
	return out
}

// homeOf is the home directory of the named user ("" for the current
// one), "" when there is no such user.
func homeOf(name string) string {
	if name == "" {
		h, _ := os.UserHomeDir()
		return h
	}
	if u, err := user.Lookup(name); err == nil {
		return u.HomeDir
	}
	return ""
}

// expandShellVars expands variables the way the command's shell will: $PWD
// is the working directory and the rest come from the environment child
// processes get, where a scrubbed or unset variable is empty.
func expandShellVars(s, base string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	env := map[string]string{}
	for _, kv := range proc.Env(nil) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return os.Expand(s, func(name string) string {
		if name == "PWD" {
			return base
		}
		return env[name]
	})
}
