package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// resolve is the filesystem path a model-supplied path means: the one the
// policy judged (env.Judged) when it judged this call, else ResolvePath now.
// A tool that resolved again would follow a link swapped in since the
// judgement; the judged path, opened through its root, refuses it instead.
func resolve(env *Env, p string) string {
	if j, ok := env.Judged[p]; ok {
		return j
	}
	return ResolvePath(env.Dir, p)
}

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
	return model.ToolDef{Name: toolname.Read, Description: "Read a text file. offset and limit are line numbers, for large files; the result says how to continue.",
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

// binaryProbe is how much of a file's head decides whether it is text.
const binaryProbe = 8 << 10

// isBinary says whether a file's head is something other than text: a NUL
// byte, or bytes that are not UTF-8 (a rune cut at the probe's end is not
// held against it).
func isBinary(head []byte) bool {
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	for len(head) > 0 && !utf8.Valid(head) {
		if len(head) < 4 || utf8.Valid(head[:len(head)-1]) {
			head = head[:len(head)-1]
			continue
		}
		return true
	}
	return false
}
func (readTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a readInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	f, err := env.openRead(resolve(env, a.Path))
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
	// A file that is not text is named, not dumped: 50 KB of an image or a
	// binary in the context says nothing and costs the same as a source
	// file (OpenCode hands images over as images; Codex resizes them).
	br := bufio.NewReaderSize(f, 1<<20)
	if head, _ := br.Peek(binaryProbe); isBinary(head) {
		size := int64(-1)
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		return errf("%s is not a text file (%s, %d bytes): read cannot show it", a.Path, http.DetectContentType(head), size)
	}
	var sb strings.Builder
	sc := bufio.NewScanner(br)
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
		// Lines carry no numbers: apply_patch anchors on the text, and a
		// number on every line of a 2,000-line read is about 5,000 tokens
		// the model never uses (Codex reads through the shell, unnumbered).
		sb.WriteString(sc.Text())
		sb.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return errf("%v", err)
	}
	if sb.Len() == 0 {
		return Result{Output: "(empty file)"}
	}
	return Result{Output: env.Clip(sb.String())}
}

func pathArg(in json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = decode(in, &a)
	return a.Path
}
