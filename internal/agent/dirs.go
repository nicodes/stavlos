package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// Working directories (PRD §10.7): the channel has one working set, shared
// by every agent: the channel directory and what the human adds, in the dirs
// tab or by answering a boundary prompt. Roles carry none and agent_create
// grants none: one set is what a person can keep track of. A tool call that
// reaches outside the set asks first, even when policy allows the tool.
// File paths are resolved the way the tools open them (tools.ResolvePath:
// symlinks followed, nothing expanded); shell commands are inspected as
// text — absolute, ~, $HOME-style and parent-relative arguments, cd and
// redirect targets. That is string inspection, not a sandbox: it catches
// the model's ordinary behaviour, not an adversary's.

type dirEntry struct{ path, source string }

// resolveDir makes d absolute: ~ and ${env:NAME} expand, a relative path is
// taken from base, and the result is cleaned.
func resolveDir(base, d string) string {
	d = config.ExpandEnv(strings.TrimSpace(d))
	if d == "~" || strings.HasPrefix(d, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			d = filepath.Join(h, strings.TrimPrefix(d, "~"))
		}
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(base, d)
	}
	return filepath.Clean(d)
}

// dirList is the channel's working set: the channel directory, then the
// added directories in the order they came.
func (s *Channel) dirList() []dirEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]dirEntry{{s.Dir, "channel"}}, s.dirs...)
}

// dirPaths is the working set as paths.
func (s *Channel) dirPaths() []string {
	var out []string
	for _, d := range s.dirList() {
		out = append(out, d.path)
	}
	return out
}

// dirInfos is the working set for clients.
func (s *Channel) dirInfos() []protocol.DirInfo {
	var out []protocol.DirInfo
	for _, d := range s.dirList() {
		out = append(out, protocol.DirInfo{Path: d.path, Source: d.source})
	}
	return out
}

// inDirs reports whether an absolute path lies in the working set. Both
// sides are compared with symlinks resolved, so a working directory that
// is itself a link (or reached through one) still contains its files.
func (s *Channel) inDirs(p string) bool {
	p = tools.ResolvePath("", p)
	for _, d := range s.dirList() {
		dir := tools.ResolvePath("", d.path)
		if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// addDir puts a directory in the working set and logs it on agent, the one
// whose boundary prompt added it ("" for the dirs tab). A directory already
// inside the set is a no-op.
func (s *Channel) addDir(ctx context.Context, agent, dir, source string) error {
	dir = filepath.Clean(dir)
	if s.inDirs(dir) {
		return nil
	}
	if _, err := s.host.Append(ctx, event.Event{Channel: s.ID, Agent: agent, Type: event.ChannelDirAdded,
		Payload: event.MustPayload(event.DirAddedPayload{Dir: dir, Source: source})}); err != nil {
		return err
	}
	s.mu.Lock()
	s.applyDirAdded(dir, source)
	s.mu.Unlock()
	return nil
}

// applyDirAdded installs an added directory; the caller holds s.mu or is
// replaying the log.
func (s *Channel) applyDirAdded(dir, source string) {
	if dir == filepath.Clean(s.Dir) || slices.ContainsFunc(s.dirs, func(e dirEntry) bool { return e.path == dir }) {
		return
	}
	s.dirs = append(s.dirs, dirEntry{dir, source})
}

// AddDir is the human's add (an absolute path, ~, or a path relative to the
// channel directory).
func (s *Channel) AddDir(ctx context.Context, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("a directory is required")
	}
	return s.addDir(ctx, "", resolveDir(s.Dir, dir), "human")
}

// RemoveDir takes a directory out of the working set. The channel
// directory stays.
func (s *Channel) RemoveDir(ctx context.Context, dir string) error {
	dir = resolveDir(s.Dir, dir)
	if dir == filepath.Clean(s.Dir) {
		return fmt.Errorf("the channel directory cannot be removed")
	}
	if !slices.ContainsFunc(s.dirList(), func(e dirEntry) bool { return e.path == dir }) {
		return fmt.Errorf("%s is not one of the channel's directories", dir)
	}
	if _, err := s.host.Append(ctx, event.Event{Channel: s.ID, Type: event.ChannelDirRemoved, Payload: event.MustPayload(event.DirRefPayload{Dir: dir})}); err != nil {
		return err
	}
	s.mu.Lock()
	s.applyDirRemoved(dir)
	s.mu.Unlock()
	return nil
}

// applyDirRemoved forgets a directory; the caller holds s.mu or is
// replaying the log.
func (s *Channel) applyDirRemoved(dir string) {
	s.dirs = slices.DeleteFunc(s.dirs, func(e dirEntry) bool { return e.path == dir })
}

// outsideDir returns the first directory a tool call reaches outside the
// working set ("" when it stays inside), as the directory a boundary prompt
// would add: the path itself when it names a directory, else its parent.
func (a *Agent) outsideDir(sub policy.Subject) string {
	var paths []string
	switch sub.Kind {
	case policy.KindCommand:
		paths = bashPathCandidates(sub.Primary(), a.s.Dir)
	case policy.KindPath:
		for _, p := range sub.Values {
			paths = append(paths, tools.ResolvePath(a.s.Dir, p)) // as the tool will open it: no ~ or ${env:} for a model's path
		}
	case policy.KindText, policy.KindURL, policy.KindID:
	}
	for _, p := range paths {
		if p == "" || a.s.inDirs(p) {
			continue
		}
		return grantDir(p)
	}
	return ""
}

// grantDir is the directory a boundary prompt offers to add for a path:
// the git checkout containing it when there is one (the repository is the
// unit people think in, and one answer then covers every package), else
// the path itself when it is a directory, else its parent. A checkout
// rooted at the home directory does not count: that would grant everything.
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
// effort: absolute, ~ and parent-relative (../x) arguments (also after
// --flag=), $HOME/$PWD/$TMPDIR-style arguments with those variables
// expanded, cd targets and redirect targets (relative ones taken from
// base). The program of each simple command and /dev/* are skipped; a glob
// is cut at its first wildcard.
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
		if strings.HasPrefix(q, "/") || q == "~" || strings.HasPrefix(q, "~/") || q == ".." || strings.HasPrefix(q, "../") || strings.Contains(q, "/../") {
			add(q)
		}
	}
	return out
}

// expandShellVars expands the variables a path usually leans on: $HOME,
// $PWD (the working directory), $TMPDIR and $USER. Others stay as written.
func expandShellVars(s, base string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return os.Expand(s, func(name string) string {
		switch name {
		case "HOME":
			h, _ := os.UserHomeDir()
			return h
		case "PWD":
			return base
		case "TMPDIR":
			return os.TempDir()
		case "USER":
			return os.Getenv("USER")
		}
		return "$" + name
	})
}
