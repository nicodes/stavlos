package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// Working directories (PRD §10.7): every agent has the session directory;
// its role adds dirs:; its creator may grant directories from its own set at
// agent_create; the human may add one by answering a boundary prompt. A
// tool call that reaches outside the set asks first, even when policy
// allows the tool. This is string inspection of paths and commands, not a
// sandbox: it catches the model's ordinary behaviour, not an adversary's.

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

// dirListLocked is the agent's working set: the session directory, the
// role's directories, then grants and human additions. The caller holds
// a.mu.
func (a *Agent) dirListLocked() []dirEntry {
	out := []dirEntry{{a.s.Dir, "session"}}
	seen := map[string]bool{a.s.Dir: true}
	add := func(e dirEntry) {
		if !seen[e.path] {
			seen[e.path] = true
			out = append(out, e)
		}
	}
	for _, d := range a.preset.Dirs {
		if e := (dirEntry{resolveDir(a.s.Dir, d), "role"}); !a.removedDirs[e.path] {
			add(e)
		}
	}
	for _, e := range a.extraDirs {
		if !a.removedDirs[e.path] {
			add(e)
		}
	}
	return out
}

func (a *Agent) dirList() []dirEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dirListLocked()
}

// dirPaths is the working set as paths.
func (a *Agent) dirPaths() []string {
	var out []string
	for _, d := range a.dirList() {
		out = append(out, d.path)
	}
	return out
}

// inDirs reports whether an absolute path lies in the working set.
func (a *Agent) inDirs(p string) bool {
	p = filepath.Clean(p)
	for _, d := range a.dirList() {
		if p == d.path || strings.HasPrefix(p, d.path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// addDir puts a directory in the working set and logs it. A directory the
// human had removed comes back.
func (a *Agent) addDir(ctx context.Context, dir, source string) error {
	dir = filepath.Clean(dir)
	if a.inDirs(dir) {
		return nil
	}
	if _, err := a.record(ctx, event.AgentDirAdded, event.DirAddedPayload{Dir: dir, Source: source}); err != nil {
		return err
	}
	a.mu.Lock()
	a.applyDirAdded(dir, source)
	a.mu.Unlock()
	return nil
}

// applyDirAdded installs an added directory; the caller holds a.mu.
func (a *Agent) applyDirAdded(dir, source string) {
	delete(a.removedDirs, dir)
	a.extraDirs = append(a.extraDirs, dirEntry{dir, source})
}

// AddDir is the human's add (an absolute path, ~, or a path relative to the
// session directory).
func (a *Agent) AddDir(ctx context.Context, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("a directory is required")
	}
	return a.addDir(ctx, resolveDir(a.s.Dir, dir), "human")
}

// RemoveDir takes a directory out of the working set. The session
// directory stays; a role directory is hidden until added back.
func (a *Agent) RemoveDir(ctx context.Context, dir string) error {
	dir = resolveDir(a.s.Dir, dir)
	if dir == filepath.Clean(a.s.Dir) {
		return fmt.Errorf("the session directory cannot be removed")
	}
	found := false
	for _, d := range a.dirList() {
		if d.path == dir {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%s is not one of the agent's directories", dir)
	}
	if _, err := a.record(ctx, event.AgentDirRemoved, event.DirRefPayload{Dir: dir}); err != nil {
		return err
	}
	a.mu.Lock()
	a.applyDirRemoved(dir)
	a.mu.Unlock()
	return nil
}

// applyDirRemoved forgets a directory; the caller holds a.mu.
func (a *Agent) applyDirRemoved(dir string) {
	if a.removedDirs == nil {
		a.removedDirs = map[string]bool{}
	}
	a.removedDirs[dir] = true
	kept := a.extraDirs[:0]
	for _, e := range a.extraDirs {
		if e.path != dir {
			kept = append(kept, e)
		}
	}
	a.extraDirs = kept
}

// outsideDir returns the first directory a tool call reaches outside the
// working set ("" when it stays inside), as the directory a boundary prompt
// would add: the path itself when it names a directory, else its parent.
func (a *Agent) outsideDir(name string, input json.RawMessage, t tools.Tool) string {
	var paths []string
	switch name {
	case "bash", "bash_async":
		var in struct{ Command string }
		_ = json.Unmarshal(input, &in)
		paths = bashPathCandidates(in.Command, a.s.Dir)
	case "read", "apply_patch":
		if ma, ok := t.(tools.MultiArg); ok {
			paths = ma.PolicyArgs(input)
		} else {
			paths = []string{t.PolicyArg(input)}
		}
		for i, p := range paths {
			paths[i] = resolveDir(a.s.Dir, p)
		}
	}
	for _, p := range paths {
		if p == "" || a.inDirs(p) {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
		return filepath.Dir(p)
	}
	return ""
}

// bashPathCandidates picks the paths a shell command line may touch, best
// effort: absolute and ~ arguments (also after --flag=), cd targets and
// redirect targets (relative ones taken from base). The program of each
// simple command and /dev/* are skipped; a glob is cut at its first
// wildcard.
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
		q := strings.Trim(val, "\"'")
		if strings.HasPrefix(q, "/") || q == "~" || strings.HasPrefix(q, "~/") {
			add(q)
		}
	}
	return out
}
