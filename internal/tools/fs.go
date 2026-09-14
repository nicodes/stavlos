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

func resolve(env *Env, p string) string { return ResolvePath(env.Dir, p) }

// ResolvePath is the one way a model-supplied path becomes a filesystem
// path: absolute against root, cleaned, and with symlinks resolved on the
// part of it that exists, so a link inside the working directory that
// points outside is judged — and opened — by where it leads. Nothing is
// expanded: "~" and "$HOME" are the shell's business, not a path's. The
// boundary check and the tools use the same function, so they cannot
// disagree about which file a call touches.
func ResolvePath(root, p string) string {
	if p == "" {
		p = root
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return real
			}
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // nothing of it exists yet
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
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
