package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
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
