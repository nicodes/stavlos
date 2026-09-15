package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/clip"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
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
	return model.ToolDef{Name: toolname.Read, Description: "Read a file. Returns numbered lines. Use offset/limit for large files.",
		Schema: schemaOf(readInput{})}
}

// readDefaultLines is what one read returns without a limit; readMaxLines
// bounds any read. Past either the tool says how to continue.
const (
	readDefaultLines = 2000
	readMaxLines     = 5000
)

type readInput struct {
	Path   string `json:"path" desc:"File path, absolute or relative to the working directory" req:"true"`
	Offset int    `json:"offset" desc:"1-based first line to return (default 1)"`
	Limit  int    `json:"limit" desc:"Max lines to return (default 2000, max 5000)"`
}

func (readTool) Subject(in json.RawMessage) policy.Subject { return policy.Path(pathArg(in)) }
func (readTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a readInput
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
	switch {
	case a.Limit <= 0:
		a.Limit = readDefaultLines
	case a.Limit > readMaxLines:
		a.Limit = readMaxLines
	}
	budget := env.MaxOutput
	if budget <= 0 {
		budget = 32 * 1024
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
		// Stop at the line limit or the byte budget: the whole file is
		// never built in memory just to be cut down afterwards.
		if n >= a.Offset+a.Limit || sb.Len() > budget {
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
	return Result{Output: clip.Middle(sb.String(), env.MaxOutput)}
}

func pathArg(in json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = decode(in, &a)
	return a.Path
}
