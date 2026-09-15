package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// grep and glob are the read-only search tools. They take no flags from
// the model and run no shell, so unlike a shell search they cannot be
// turned into running something (find -exec, rg --pre), and their paths
// are judged like read's. They use ripgrep when it is installed, with an
// argument list built here (so .gitignore is honoured), and a walk of the
// tree otherwise.

// rgBinary is ripgrep's path, "" when it is not installed (tests clear it
// to exercise the walk).
var rgBinary, _ = exec.LookPath("rg")

const (
	grepDefaultLimit = 200
	grepMaxLimit     = 2000
	globDefaultLimit = 500
	globMaxLimit     = 5000
	grepMaxLine      = 500     // characters of a matching line shown
	walkMaxFileSize  = 4 << 20 // larger files are skipped by the walk
)

// --- grep ---

type grepTool struct{}

type grepInput struct {
	Pattern    string `json:"pattern" desc:"Regular expression to search file contents for" req:"true"`
	Path       string `json:"path" desc:"File or directory to search, absolute or relative to the working directory (default: the working directory)"`
	Glob       string `json:"glob" desc:"Only search files matching this glob, such as *.go or src/**/*.ts"`
	IgnoreCase bool   `json:"ignore_case" desc:"Match regardless of case"`
	Limit      int    `json:"limit" desc:"Maximum matching lines to return (default 200, max 2000)"`
}

func (grepTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Grep, Description: "Search file contents with a regular expression. Returns matching lines as path:line:text, skipping hidden, binary and git-ignored files. Use it rather than grep or rg in shell: it never needs the human's approval inside the working directories.",
		Schema: schemaOf(grepInput{})}
}

func (grepTool) Subject(in json.RawMessage) policy.Subject {
	var a grepInput
	_ = decode(in, &a)
	return policy.Path(searchPath(a.Path))
}

func (grepTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a grepInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil || a.Pattern == "" {
		return errf("pattern %q is not a regular expression: %v", a.Pattern, err)
	}
	limit := clampLimit(a.Limit, grepDefaultLimit, grepMaxLimit)
	var lines []string
	var more bool
	if rgBinary != "" {
		args := []string{"--line-number", "--no-heading", "--with-filename", "--max-columns", fmt.Sprint(grepMaxLine), "--max-columns-preview"}
		if a.IgnoreCase {
			args = append(args, "--ignore-case")
		}
		if a.Glob != "" {
			args = append(args, "--glob", a.Glob)
		}
		lines, more, err = runRG(ctx, env.Dir, append(args, "--regexp", a.Pattern), a.Path, limit)
	} else {
		lines, more, err = walkGrep(ctx, env, a, re, limit)
	}
	if err != nil {
		return errf("%v", err)
	}
	return matchList(lines, more, limit, "matching lines", env.MaxOutput)
}

// walkGrep is grep without ripgrep.
func walkGrep(ctx context.Context, env *Env, a grepInput, re *regexp.Regexp, limit int) ([]string, bool, error) {
	match := globMatcher(a.Glob)
	var lines []string
	more := false
	err := walkFiles(ctx, env.Dir, a.Path, func(abs, shown string) bool {
		if !match(shown) {
			return true
		}
		f, err := os.Open(abs)
		if err != nil {
			return true
		}
		defer f.Close()
		head := make([]byte, 8000)
		n, _ := io.ReadFull(f, head)
		if strings.IndexByte(string(head[:n]), 0) >= 0 {
			return true // binary
		}
		_, _ = f.Seek(0, io.SeekStart)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), walkMaxFileSize)
		for no := 1; sc.Scan(); no++ {
			if re.MatchString(sc.Text()) {
				if len(lines) == limit {
					more = true
					return false
				}
				lines = append(lines, fmt.Sprintf("%s:%d:%s", shown, no, cutRunes(sc.Text(), grepMaxLine)))
			}
		}
		return true
	})
	return lines, more, err
}

// --- glob ---

type globTool struct{}

type globInput struct {
	Pattern string `json:"pattern" desc:"Glob to match file paths against: * stays within a directory, ** crosses directories, and a pattern without a slash matches file names at any depth (*.go, src/**/*_test.go)" req:"true"`
	Path    string `json:"path" desc:"Directory to search, absolute or relative to the working directory (default: the working directory)"`
	Limit   int    `json:"limit" desc:"Maximum paths to return (default 500, max 5000)"`
}

func (globTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Glob, Description: "List files whose paths match a glob, skipping hidden and git-ignored files. Use it rather than find or ls in shell to discover files: it never needs the human's approval inside the working directories.",
		Schema: schemaOf(globInput{})}
}

func (globTool) Subject(in json.RawMessage) policy.Subject {
	var a globInput
	_ = decode(in, &a)
	return policy.Path(searchPath(a.Path))
}

func (globTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a globInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return errf("a pattern is required")
	}
	limit := clampLimit(a.Limit, globDefaultLimit, globMaxLimit)
	var paths []string
	var more bool
	var err error
	if rgBinary != "" {
		paths, more, err = runRG(ctx, env.Dir, []string{"--files", "--glob", a.Pattern}, a.Path, limit)
	} else {
		match := globMatcher(a.Pattern)
		err = walkFiles(ctx, env.Dir, a.Path, func(_, shown string) bool {
			if match(shown) {
				if len(paths) == limit {
					more = true
					return false
				}
				paths = append(paths, shown)
			}
			return true
		})
	}
	if err != nil {
		return errf("%v", err)
	}
	sort.Strings(paths)
	return matchList(paths, more, limit, "paths", env.MaxOutput)
}

// --- shared ---

// searchPath is a search's subject: its path, or the working directory.
func searchPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return "."
	}
	return p
}

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	return min(n, max)
}

func matchList(lines []string, more bool, limit int, what string, maxOutput int) Result {
	if len(lines) == 0 {
		return Result{Output: "no matches"}
	}
	out := strings.Join(lines, "\n")
	if more {
		out += fmt.Sprintf("\n… (stopped at %d %s; narrow the pattern or the path, or raise limit)", limit, what)
	}
	return Result{Output: clip(out, maxOutput)}
}

// runRG runs ripgrep in dir with the given arguments on path ("" for dir
// itself) and returns up to limit lines of its output. Every argument is
// built by the caller from fields it validated: nothing the model wrote
// can become a flag, since the pattern follows --regexp or --glob and the
// path follows --.
func runRG(ctx context.Context, dir string, args []string, path string, limit int) ([]string, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args = append([]string{"--no-config", "--color", "never"}, args...)
	if path != "" {
		args = append(args, "--", path)
	}
	cmd := exec.CommandContext(ctx, rgBinary, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	stderr := &cappedBuffer{max: 2048}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	var lines []string
	more := false
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		if len(lines) == limit {
			more = true
			cancel()
			break
		}
		lines = append(lines, strings.TrimPrefix(sc.Text(), "./"))
	}
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case more, err == nil:
		return lines, more, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1 && len(lines) == 0:
		return nil, false, nil // no matches
	case stderr.sb.Len() > 0:
		return lines, false, errors.New(strings.TrimSpace(stderr.sb.String()))
	}
	return lines, false, err
}

// walkFiles visits the regular files under path (resolved from root; a
// file visits itself), skipping hidden entries and node_modules, and
// files over walkMaxFileSize, in lexical order. shown is the path as the
// result prints it: relative to root when inside it. visit returns false
// to stop.
func walkFiles(ctx context.Context, root, path string, visit func(abs, shown string) bool) error {
	start := ResolvePath(root, path)
	if _, err := os.Stat(start); err != nil {
		return err
	}
	stop := errors.New("stop")
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: skip
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name := d.Name()
		if p != start && (strings.HasPrefix(name, ".") || name == "node_modules") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > walkMaxFileSize {
			return nil
		}
		shown := p
		if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			shown = rel
		}
		if !visit(p, shown) {
			return stop
		}
		return nil
	})
	if errors.Is(err, stop) {
		return nil
	}
	return err
}

// globMatcher matches a shown path against a glob the way ripgrep's --glob
// does: a pattern without a slash matches the file name at any depth, one
// with a slash the whole path, "*" staying within a directory.
func globMatcher(pattern string) func(string) bool {
	if pattern == "" {
		return func(string) bool { return true }
	}
	re := policy.Glob(strings.TrimPrefix(pattern, "/"), true)
	if !strings.Contains(pattern, "/") {
		return func(p string) bool { return re.MatchString(filepath.Base(p)) }
	}
	return re.MatchString
}

// cappedBuffer keeps the first max bytes written to it.
type cappedBuffer struct {
	sb  strings.Builder
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.sb.Len(); room > 0 {
		c.sb.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
