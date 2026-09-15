// Package instructions finds the AGENTS.md files that tell agents how to
// work, the way coding-agent harnesses have converged on (agents.md): the
// user's own file for every project, the files from the repository root
// down to the working directory, and the files of subdirectories as agents
// reach into them. In each directory the first of Names present wins.
package instructions

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/paths"
)

// Names are the file names read in each directory, in order: the first one
// present is that directory's instructions (CLAUDE.md serves repositories
// written for Claude Code).
var Names = []string{"AGENTS.md", "CLAUDE.md"}

// Budget bounds the instructions put in front of an agent at once: the file
// that crosses it is cut with a note, and those after it are left out.
const Budget = 32 << 10

// maxDirs bounds the walk for nested files.
const maxDirs = 2000

// File is one instructions file.
type File struct {
	Path string // absolute
	Text string
}

// Global is the user's own instructions for every project,
// <config>/AGENTS.md; none when it is absent.
func Global() []File {
	if f, ok := read(filepath.Join(paths.ConfigDir(), "AGENTS.md")); ok {
		return []File{f}
	}
	return nil
}

// In is dir's instructions file, if it has one.
func In(dir string) (File, bool) {
	for _, n := range Names {
		if f, ok := read(filepath.Join(dir, n)); ok {
			return f, true
		}
	}
	return File{}, false
}

func read(p string) (File, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return File{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return File{}, false
	}
	return File{Path: p, Text: string(b)}, true
}

// Root is the repository root dir belongs to: the nearest directory at or
// above it holding .git, where the home directory and / do not count. A
// directory in no repository is its own root.
func Root(dir string) string {
	dir = filepath.Clean(dir)
	home, _ := os.UserHomeDir()
	for d := dir; ; d = filepath.Dir(d) {
		if d == home || d == filepath.Dir(d) {
			return dir
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
	}
}

// Chain is the instructions from dir's repository root down to dir, root
// first, so the closest comes last and wins.
func Chain(dir string) []File {
	dir = filepath.Clean(dir)
	root := Root(dir)
	var dirs []string
	for d := dir; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == root || d == filepath.Dir(d) {
			break
		}
	}
	var out []File
	for i := len(dirs) - 1; i >= 0; i-- {
		if f, ok := In(dirs[i]); ok {
			out = append(out, f)
		}
	}
	return out
}

// skipped reports whether a directory's files never count: hidden ones and
// the trees dependencies and builds fill.
func skipped(name string) bool {
	return strings.HasPrefix(name, ".") || slices.Contains([]string{"node_modules", "vendor", "target", "dist", "build", "__pycache__"}, name)
}

// Nested lists the instructions files below dir, not dir's own: skipped
// directories are not entered, and the walk stops after maxDirs
// directories.
func Nested(dir string) []string {
	var out []string
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || p == dir {
			return nil
		}
		if skipped(d.Name()) {
			return fs.SkipDir
		}
		if n++; n > maxDirs {
			return fs.SkipAll
		}
		if f, ok := In(p); ok {
			out = append(out, f.Path)
		}
		return nil
	})
	return out
}

// Between is the instructions of the directories below top down to the one
// holding p (p itself when it is a directory), top first: what an agent
// working at p follows beyond top's own. None when p is not below top, and
// a skipped directory ends the way down.
func Between(top, p string) []File {
	top, p = filepath.Clean(top), filepath.Clean(p)
	d := p
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		d = filepath.Dir(p)
	}
	rel, err := filepath.Rel(top, d)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	var out []File
	cur := top
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if skipped(part) {
			break
		}
		cur = filepath.Join(cur, part)
		if f, ok := In(cur); ok {
			out = append(out, f)
		}
	}
	return out
}

// Section is files as the system prompt carries them, general first, each
// under a heading naming it, within budget bytes.
func Section(files []File, budget int) string {
	body := render(files, budget)
	if body == "" {
		return ""
	}
	return "# Instructions (AGENTS.md)\nFollow these for the work they cover. They run from general to specific: where they conflict, a later file wins, and the human's own messages win over all of them.\n" + body
}

// Note is files as a tool result carries them, when an agent first works
// in the directories they belong to.
func Note(files []File, budget int) string {
	body := render(files, budget)
	if body == "" {
		return ""
	}
	return "[Instructions for the directories you are working in; they add to the ones you were given, and where they conflict these win]\n" + body
}

func render(files []File, budget int) string {
	var b strings.Builder
	used := 0
	for _, f := range files {
		text := strings.TrimSpace(f.Text)
		if text == "" {
			continue
		}
		b.WriteString("\n## " + display(f.Path) + "\n\n")
		if used+len(text) > budget {
			cut := strings.ToValidUTF8(text[:max(0, budget-used)], "")
			b.WriteString(cut + "\n\n[cut here: the instructions passed their 32 KiB budget, and later files are left out]\n")
			break
		}
		used += len(text)
		b.WriteString(text + "\n")
	}
	return b.String()
}

// display names a file for a heading, the home directory as ~.
func display(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
			return "~/" + rest
		}
	}
	return p
}
