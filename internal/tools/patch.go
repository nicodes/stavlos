package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// apply_patch: the Codex CLI patch grammar. No line numbers; hunks are
// anchored on context lines, so models can produce them reliably.
//
//	*** Begin Patch
//	*** Add File: path          (every following line starts with "+")
//	*** Delete File: path
//	*** Update File: path
//	*** Move to: newpath        (optional, right after Update File)
//	@@ optional anchor line     (a line of the file the hunk sits under)
//	 context line
//	-removed line
//	+added line
//	*** End of File             (the hunk's context reaches EOF)
//	*** End Patch

const patchDescription = `Create, modify, delete, or move files with a patch in this exact format:

*** Begin Patch
*** Add File: relative/path.txt
+every line of the new file, each prefixed with +
*** Update File: relative/other.go
@@ a line from the file that identifies where the change is (optional)
 unchanged context line (leading space)
-line to remove
+line to add
*** Delete File: relative/old.txt
*** End Patch

Rules: paths are relative to the working directory. Update hunks must include enough unchanged context (usually 3 lines above and below) to locate the change uniquely; there are no line numbers. Use "*** Move to: newpath" right after an Update File header to rename. Several files may appear in one patch; every change is checked before any file is written, then each file is written in one step and a failure midway is rolled back. A move keeps the file's permissions and refuses to overwrite an existing file. To replace a whole file, delete and re-add it in the same patch.`

type patchTool struct{}

func (patchTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.ApplyPatch, Description: patchDescription,
		Schema: schemaOf(patchInput{})}
}

type patchInput struct {
	Patch string `json:"patch" desc:"The full patch text, from *** Begin Patch to *** End Patch" req:"true"`
}

// Subject lists every path the patch touches (a move counts both ends), so
// the policy judges each one and the most restrictive decision wins.
func (patchTool) Subject(in json.RawMessage) policy.Subject {
	var a patchInput
	if decode(in, &a) != nil {
		return policy.Path()
	}
	ops, err := parsePatch(a.Patch)
	if err != nil {
		return policy.Path()
	}
	var out []string
	for _, op := range ops {
		out = append(out, op.path)
		if op.moveTo != "" {
			out = append(out, op.moveTo)
		}
	}
	return policy.Path(out...)
}

func (patchTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a patchInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	ops, err := parsePatch(a.Patch)
	if err != nil {
		return errf("patch: %v", err)
	}
	if len(ops) == 0 {
		return errf("patch: no file sections")
	}
	// Stage every change first so a failure leaves nothing half-applied:
	// every file is read and every hunk applied in memory before a byte is
	// written.
	type staged struct {
		path    string
		content string
		mode    os.FileMode
		prev    []byte // what was there (nil for a new file), for rollback
		delete  bool
	}
	var plan []staged
	for _, op := range ops {
		abs := resolve(env, op.path)
		switch op.kind {
		case "add":
			if _, err := os.Lstat(abs); err == nil {
				return errf("patch: %s already exists (delete it first to replace it)", op.path)
			}
			plan = append(plan, staged{path: abs, content: strings.Join(op.added, "\n") + "\n", mode: 0o644})
		case "delete":
			fi, err := os.Lstat(abs)
			if err != nil {
				return errf("patch: %s: %v", op.path, err)
			}
			if fi.IsDir() {
				return errf("patch: %s is a directory", op.path)
			}
			plan = append(plan, staged{path: abs, delete: true})
		case "update":
			fi, err := os.Stat(abs)
			if err != nil {
				return errf("patch: %s: %v", op.path, err)
			}
			b, err := os.ReadFile(abs)
			if err != nil {
				return errf("patch: %s: %v", op.path, err)
			}
			out, err := applyHunks(string(b), op.hunks)
			if err != nil {
				return errf("patch: %s: %v", op.path, err)
			}
			if op.moveTo != "" {
				to := resolve(env, op.moveTo)
				if _, err := os.Lstat(to); err == nil {
					return errf("patch: cannot move %s to %s: it already exists", op.path, op.moveTo)
				}
				plan = append(plan, staged{path: abs, delete: true})
				plan = append(plan, staged{path: to, content: out, mode: fi.Mode().Perm()})
			} else {
				plan = append(plan, staged{path: abs, content: out, mode: fi.Mode().Perm(), prev: b})
			}
		}
	}
	// Writes first, each through a temporary file renamed into place so a
	// reader never sees a half-written file; deletions last, so a failed
	// write never costs a file. A failure rolls the writes back.
	var done []staged
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			st := done[i]
			if st.prev == nil {
				_ = os.Remove(st.path)
			} else {
				_ = os.WriteFile(st.path, st.prev, st.mode)
			}
		}
	}
	for _, st := range plan {
		if st.delete {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
			rollback()
			return errf("patch: %v", err)
		}
		tmp := st.path + ".stavlos-tmp"
		if err := os.WriteFile(tmp, []byte(st.content), st.mode); err != nil {
			rollback()
			return errf("patch: %v", err)
		}
		if err := os.Rename(tmp, st.path); err != nil {
			_ = os.Remove(tmp)
			rollback()
			return errf("patch: %v", err)
		}
		done = append(done, st)
	}
	for _, st := range plan {
		if st.delete {
			if err := os.Remove(st.path); err != nil {
				return errf("patch: %v (the other changes were applied)", err)
			}
		}
	}
	var sb strings.Builder
	for _, op := range ops {
		switch {
		case op.kind == "update" && op.moveTo != "":
			fmt.Fprintf(&sb, "updated %s → %s\n", op.path, op.moveTo)
		case op.kind == "update":
			fmt.Fprintf(&sb, "updated %s (%d hunk(s))\n", op.path, len(op.hunks))
		case op.kind == "add":
			fmt.Fprintf(&sb, "added %s (%d lines)\n", op.path, len(op.added))
		case op.kind == "delete":
			fmt.Fprintf(&sb, "deleted %s\n", op.path)
		}
	}
	return Result{Output: strings.TrimRight(sb.String(), "\n")}
}

// --- grammar ---

type patchOp struct {
	kind   string // add | delete | update
	path   string
	moveTo string
	added  []string // add
	hunks  []hunk   // update
}

type hunk struct {
	anchor string // optional "@@ …" line
	lines  []hunkLine
	eof    bool
}

type hunkLine struct {
	op   byte // ' ', '-', '+'
	text string
}

func parsePatch(text string) ([]patchOp, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	// tolerate leading/trailing whitespace lines and a missing Begin marker
	start, end := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if start < 0 && t == "*** Begin Patch" {
			start = i
		}
		if t == "*** End Patch" {
			end = i
		}
	}
	if start < 0 {
		return nil, errors.New("missing \"*** Begin Patch\"")
	}
	if end < 0 || end < start {
		return nil, errors.New("missing \"*** End Patch\"")
	}
	body := lines[start+1 : end]
	var ops []patchOp
	var cur *patchOp
	var h *hunk
	flushHunk := func() {
		if cur != nil && h != nil && len(h.lines) > 0 {
			cur.hunks = append(cur.hunks, *h)
		}
		h = nil
	}
	for _, l := range body {
		switch {
		case strings.HasPrefix(l, "*** Add File: "):
			flushHunk()
			ops = append(ops, patchOp{kind: "add", path: strings.TrimSpace(strings.TrimPrefix(l, "*** Add File: "))})
			cur = &ops[len(ops)-1]
		case strings.HasPrefix(l, "*** Delete File: "):
			flushHunk()
			ops = append(ops, patchOp{kind: "delete", path: strings.TrimSpace(strings.TrimPrefix(l, "*** Delete File: "))})
			cur = &ops[len(ops)-1]
		case strings.HasPrefix(l, "*** Update File: "):
			flushHunk()
			ops = append(ops, patchOp{kind: "update", path: strings.TrimSpace(strings.TrimPrefix(l, "*** Update File: "))})
			cur = &ops[len(ops)-1]
		case strings.HasPrefix(l, "*** Move to: "):
			if cur == nil || cur.kind != "update" {
				return nil, errors.New("\"*** Move to:\" must follow an Update File header")
			}
			cur.moveTo = strings.TrimSpace(strings.TrimPrefix(l, "*** Move to: "))
		case strings.TrimSpace(l) == "*** End of File":
			if h != nil {
				h.eof = true
			}
		case strings.HasPrefix(l, "@@"):
			if cur == nil || cur.kind != "update" {
				return nil, errors.New("hunk outside an Update File section")
			}
			flushHunk()
			h = &hunk{anchor: strings.TrimSpace(strings.TrimPrefix(l, "@@"))}
		default:
			if cur == nil {
				if strings.TrimSpace(l) == "" {
					continue
				}
				return nil, fmt.Errorf("unexpected line before any file header: %q", l)
			}
			switch cur.kind {
			case "add":
				if strings.HasPrefix(l, "+") {
					cur.added = append(cur.added, l[1:])
				} else if strings.TrimSpace(l) != "" {
					return nil, fmt.Errorf("add file %s: line must start with '+': %q", cur.path, l)
				}
			case "update":
				if h == nil {
					h = &hunk{}
				}
				if l == "" {
					h.lines = append(h.lines, hunkLine{' ', ""})
					continue
				}
				switch l[0] {
				case ' ', '-', '+':
					h.lines = append(h.lines, hunkLine{l[0], l[1:]})
				default:
					// lenient: treat an unprefixed line as context
					h.lines = append(h.lines, hunkLine{' ', l})
				}
			case "delete":
				if strings.TrimSpace(l) != "" {
					return nil, fmt.Errorf("delete file %s: unexpected content", cur.path)
				}
			}
		}
	}
	flushHunk()
	for i := range ops {
		if ops[i].kind == "update" && len(ops[i].hunks) == 0 && ops[i].moveTo == "" {
			return nil, fmt.Errorf("update file %s has no hunks", ops[i].path)
		}
	}
	return ops, nil
}

// --- applying ---

// applyHunks applies hunks in order to content. Each hunk's context and
// removed lines must match a unique run of lines (exact, then ignoring
// trailing whitespace, then ignoring all surrounding whitespace); the run
// is replaced by the context and added lines.
func applyHunks(content string, hunks []hunk) (string, error) {
	hadTrailingNL := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}
	searchFrom := 0
	for hi, h := range hunks {
		var old, new []string
		for _, l := range h.lines {
			switch l.op {
			case ' ':
				old = append(old, l.text)
				new = append(new, l.text)
			case '-':
				old = append(old, l.text)
			case '+':
				new = append(new, l.text)
			}
		}
		// drop a trailing empty context line the model often adds
		for len(old) > 0 && old[len(old)-1] == "" && len(new) > 0 && new[len(new)-1] == "" {
			old, new = old[:len(old)-1], new[:len(new)-1]
		}
		if len(old) == 0 {
			// pure insertion: after the anchor, else at EOF
			at := len(lines)
			if h.anchor != "" {
				if i := findLine(lines, h.anchor, searchFrom); i >= 0 {
					at = i + 1
				}
			}
			lines = splice(lines, at, at, new)
			searchFrom = at + len(new)
			continue
		}
		from := searchFrom
		if h.anchor != "" {
			if i := findLine(lines, h.anchor, searchFrom); i >= 0 {
				from = i
			}
		}
		at := findRun(lines, old, from)
		if at < 0 && from > 0 {
			at = findRun(lines, old, 0)
		}
		if at < 0 {
			return "", fmt.Errorf("hunk %d: context not found:\n%s", hi+1, strings.Join(old, "\n"))
		}
		if h.eof && at+len(old) != len(lines) {
			if alt := findRunFrom(lines, old, len(lines)-len(old)); alt >= 0 {
				at = alt
			}
		}
		// Rebuild the replacement using the file's own text for context
		// lines, so a whitespace-lenient match never rewrites them.
		new = new[:0]
		oi := 0
		for _, l := range h.lines {
			switch l.op {
			case ' ':
				if oi < len(old) {
					new = append(new, lines[at+oi])
				}
				oi++
			case '-':
				oi++
			case '+':
				new = append(new, l.text)
			}
		}
		lines = splice(lines, at, at+len(old), new)
		searchFrom = at + len(new)
	}
	out := strings.Join(lines, "\n")
	if hadTrailingNL || out != "" {
		out += "\n"
	}
	return out, nil
}

func splice(lines []string, from, to int, repl []string) []string {
	out := make([]string, 0, len(lines)-(to-from)+len(repl))
	out = append(out, lines[:from]...)
	out = append(out, repl...)
	out = append(out, lines[to:]...)
	return out
}

func findLine(lines []string, want string, from int) int {
	for pass := 0; pass < 3; pass++ {
		for i := from; i < len(lines); i++ {
			if eq(lines[i], want, pass) {
				return i
			}
		}
	}
	return -1
}

// findRun returns the index where old matches lines contiguously, starting
// the search at from, trying stricter matching first. Ambiguous (two exact
// matches) runs are still accepted at the first occurrence after from.
func findRun(lines, old []string, from int) int {
	for pass := 0; pass < 3; pass++ {
		for i := from; i+len(old) <= len(lines); i++ {
			if runEq(lines, old, i, pass) {
				return i
			}
		}
	}
	return -1
}

func findRunFrom(lines, old []string, i int) int {
	if i < 0 {
		return -1
	}
	for pass := 0; pass < 3; pass++ {
		if runEq(lines, old, i, pass) {
			return i
		}
	}
	return -1
}

func runEq(lines, old []string, at, pass int) bool {
	for j := range old {
		if !eq(lines[at+j], old[j], pass) {
			return false
		}
	}
	return true
}

func eq(a, b string, pass int) bool {
	switch pass {
	case 0:
		return a == b
	case 1:
		return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t")
	}
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}
