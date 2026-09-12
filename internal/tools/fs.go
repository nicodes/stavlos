package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
)

func resolve(env *Env, p string) string {
	if p == "" {
		return env.Dir
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(env.Dir, p)
}

// relForPolicy returns the path relative to the session dir when inside it,
// so patterns like "src/**" work; absolute otherwise.
func relForPolicy(dir, p string) string {
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(dir, p)
	}
	if rel, err := filepath.Rel(dir, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, ".stavlos-cache": true, "vendor": false}

// --- read ---

type readTool struct{}

func (readTool) Def() model.ToolDef {
	return model.ToolDef{Name: "read", Description: "Read a file. Returns numbered lines. Use offset/limit for large files.",
		Schema: schema(map[string]any{
			"path":   prop("string", "File path, absolute or relative to the working directory"),
			"offset": prop("integer", "1-based first line to return (default 1)"),
			"limit":  prop("integer", "Max lines to return (default 2000)"),
		}, "path")}
}
func (readTool) PolicyArg(in json.RawMessage) string { return pathArg(in) }
func (readTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct {
		Path          string `json:"path"`
		Offset, Limit int
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	f, err := os.Open(resolve(env, a.Path))
	if err != nil {
		return errf("%v", err)
	}
	defer f.Close()
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit <= 0 {
		a.Limit = 2000
	}
	var sb strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		if n < a.Offset {
			continue
		}
		if n >= a.Offset+a.Limit {
			sb.WriteString(fmt.Sprintf("… (more lines; continue with offset=%d)\n", n))
			break
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", n, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return errf("%v", err)
	}
	if sb.Len() == 0 {
		return Result{Output: "(empty file)"}
	}
	return Result{Output: clip(sb.String(), env.MaxOutput)}
}

func pathArg(in json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = decode(in, &a)
	return a.Path
}

// --- write ---

type writeTool struct{}

func (writeTool) Def() model.ToolDef {
	return model.ToolDef{Name: "write", Description: "Create or overwrite a file with the given content. Creates parent directories.",
		Schema: schema(map[string]any{
			"path":    prop("string", "File path"),
			"content": prop("string", "Full file content"),
		}, "path", "content")}
}
func (writeTool) PolicyArg(in json.RawMessage) string { return pathArg(in) }
func (writeTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct{ Path, Content string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	p := resolve(env, a.Path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return errf("%v", err)
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path)}
}

// --- edit ---

type editTool struct{}

func (editTool) Def() model.ToolDef {
	return model.ToolDef{Name: "edit", Description: "Replace an exact string in a file. old_string must match exactly once unless replace_all is true.",
		Schema: schema(map[string]any{
			"path":        prop("string", "File path"),
			"old_string":  prop("string", "Exact text to find"),
			"new_string":  prop("string", "Replacement text"),
			"replace_all": prop("boolean", "Replace every occurrence (default false)"),
		}, "path", "old_string", "new_string")}
}
func (editTool) PolicyArg(in json.RawMessage) string { return pathArg(in) }
func (editTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	p := resolve(env, a.Path)
	b, err := os.ReadFile(p)
	if err != nil {
		return errf("%v", err)
	}
	s := string(b)
	n := strings.Count(s, a.Old)
	switch {
	case n == 0:
		return errf("old_string not found in %s", a.Path)
	case n > 1 && !a.ReplaceAll:
		return errf("old_string matches %d times in %s; make it unique or set replace_all", n, a.Path)
	}
	if a.ReplaceAll {
		s = strings.ReplaceAll(s, a.Old, a.New)
	} else {
		s = strings.Replace(s, a.Old, a.New, 1)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("edited %s (%d replacement(s))", a.Path, n)}
}

// --- glob ---

type globTool struct{}

func (globTool) Def() model.ToolDef {
	return model.ToolDef{Name: "glob", Description: "Find files by glob pattern (supports **). Returns paths relative to the working directory, newest first.",
		Schema: schema(map[string]any{
			"pattern": prop("string", "Glob such as **/*.go or src/**/test_*.py"),
			"path":    prop("string", "Directory to search (default: working directory)"),
		}, "pattern")}
}
func (globTool) PolicyArg(in json.RawMessage) string { return pathArg(in) }
func (globTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct{ Pattern, Path string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	root := resolve(env, a.Path)
	type hit struct {
		p string
		t int64
	}
	var hits []hit
	err := walk(ctx, root, func(p string, d fs.DirEntry) {
		rel, _ := filepath.Rel(root, p)
		if policy.Match(a.Pattern, rel) {
			info, _ := d.Info()
			var t int64
			if info != nil {
				t = info.ModTime().UnixNano()
			}
			hits = append(hits, hit{rel, t})
		}
	})
	if err != nil {
		return errf("%v", err)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].t > hits[j].t })
	if len(hits) == 0 {
		return Result{Output: "no matches"}
	}
	var sb strings.Builder
	for i, h := range hits {
		if i >= 500 {
			fmt.Fprintf(&sb, "… %d more\n", len(hits)-i)
			break
		}
		sb.WriteString(h.p + "\n")
	}
	return Result{Output: clip(sb.String(), env.MaxOutput)}
}

func walk(ctx context.Context, root string, fn func(p string, d fs.DirEntry)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		fn(p, d)
		return nil
	})
}

// --- grep ---

type grepTool struct{}

func (grepTool) Def() model.ToolDef {
	return model.ToolDef{Name: "grep", Description: "Search file contents with a regular expression. Returns path:line:text matches.",
		Schema: schema(map[string]any{
			"pattern":     prop("string", "Go/RE2 regular expression"),
			"path":        prop("string", "Directory or file to search (default: working directory)"),
			"glob":        prop("string", "Only search files matching this glob, e.g. *.go"),
			"ignore_case": prop("boolean", "Case-insensitive"),
			"max":         prop("integer", "Max matches (default 200)"),
		}, "pattern")}
}
func (grepTool) PolicyArg(in json.RawMessage) string { return pathArg(in) }
func (grepTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct {
		Pattern, Path, Glob string
		IgnoreCase          bool `json:"ignore_case"`
		Max                 int
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	pat := a.Pattern
	if a.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return errf("bad regexp: %v", err)
	}
	if a.Max <= 0 {
		a.Max = 200
	}
	root := resolve(env, a.Path)
	var sb strings.Builder
	count := 0
	search := func(p string) {
		if count >= a.Max {
			return
		}
		f, err := os.Open(p)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		ln := 0
		for sc.Scan() {
			ln++
			line := sc.Text()
			if ln == 1 && strings.IndexByte(line, 0) >= 0 {
				return // binary
			}
			if re.MatchString(line) {
				rel, _ := filepath.Rel(env.Dir, p)
				if strings.HasPrefix(rel, "..") {
					rel = p
				}
				if len(line) > 300 {
					line = line[:300] + "…"
				}
				fmt.Fprintf(&sb, "%s:%d:%s\n", rel, ln, line)
				count++
				if count >= a.Max {
					return
				}
			}
		}
	}
	if st, err := os.Stat(root); err == nil && !st.IsDir() {
		search(root)
	} else {
		err := walk(ctx, root, func(p string, d fs.DirEntry) {
			if a.Glob != "" {
				if ok := policy.Match(a.Glob, d.Name()); !ok {
					rel, _ := filepath.Rel(root, p)
					if !policy.Match(a.Glob, rel) {
						return
					}
				}
			}
			search(p)
		})
		if err != nil {
			return errf("%v", err)
		}
	}
	if count == 0 {
		return Result{Output: "no matches"}
	}
	if count >= a.Max {
		fmt.Fprintf(&sb, "… stopped at %d matches\n", a.Max)
	}
	return Result{Output: clip(sb.String(), env.MaxOutput)}
}
