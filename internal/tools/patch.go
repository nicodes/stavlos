package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	plan, err := stagePatch(ops, env)
	if err != nil {
		return errf("patch: %v", err)
	}
	if err := plan.write(); err != nil {
		return errf("patch: %v", err)
	}
	return Result{Output: patchSummary(ops)}
}

// A patch is staged in memory against an overlay of the files it touches,
// keyed by resolved path: each section sees the result of the ones before
// it, so two updates to one file chain, a delete and an add of one path
// replace it, and an update after a move edits the moved file. Nothing is
// written until every section has applied.

// stagedFile is one path's state in the overlay.
type stagedFile struct {
	path    string
	exists  bool   // after the patch
	content string // after the patch, when exists
	mode    os.FileMode
	had     bool   // the file existed before the patch
	prev    []byte // its content then, for rollback
}

// overlay is the patch's view of the filesystem.
type overlay struct {
	files map[string]*stagedFile
	order []string // first-touch order, for writing
	env   *Env
}

// file is a path's state, read from disk on first touch.
func (o *overlay) file(abs, shown string) (*stagedFile, error) {
	if f, ok := o.files[abs]; ok {
		return f, nil
	}
	f := &stagedFile{path: abs, mode: 0o644}
	switch r, err := o.env.openRead(abs); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		if strings.HasSuffix(err.Error(), " is a directory") {
			return nil, fmt.Errorf("%s is a directory", shown)
		}
		return nil, fmt.Errorf("%s: %v", shown, err)
	default:
		b, rerr := io.ReadAll(r)
		fi, serr := r.Stat()
		r.Close()
		if rerr != nil || serr != nil {
			return nil, fmt.Errorf("%s: %v", shown, errors.Join(rerr, serr))
		}
		f.exists, f.had, f.content, f.prev, f.mode = true, true, string(b), b, fi.Mode().Perm()
	}
	o.files[abs] = f
	o.order = append(o.order, abs)
	return f, nil
}

// stagePatch applies every section to the overlay, so a failure leaves
// nothing half-applied.
func stagePatch(ops []patchOp, env *Env) (*overlay, error) {
	o := &overlay{files: map[string]*stagedFile{}, env: env}
	for _, op := range ops {
		f, err := o.file(resolve(env, op.path), op.path)
		if err != nil {
			return nil, err
		}
		switch op.kind {
		case "add":
			if f.exists {
				return nil, fmt.Errorf("%s already exists (delete it first in the same patch to replace it)", op.path)
			}
			f.exists, f.content = true, strings.Join(op.added, "\n")+"\n"
		case "delete":
			if !f.exists {
				return nil, fmt.Errorf("%s: no such file", op.path)
			}
			f.exists = false
		case "update":
			if err := o.update(op, f, env); err != nil {
				return nil, err
			}
		}
	}
	return o, nil
}

// update applies an update section's hunks, and its move, to the overlay.
func (o *overlay) update(op patchOp, f *stagedFile, env *Env) error {
	if !f.exists {
		return fmt.Errorf("%s: no such file", op.path)
	}
	out, err := applyHunks(f.content, op.hunks)
	if err != nil {
		return fmt.Errorf("%s: %v", op.path, err)
	}
	if op.moveTo == "" {
		f.content = out
		return nil
	}
	to, err := o.file(resolve(env, op.moveTo), op.moveTo)
	if err != nil {
		return err
	}
	if to.exists {
		return fmt.Errorf("cannot move %s to %s: it already exists", op.path, op.moveTo)
	}
	f.exists = false
	to.exists, to.content, to.mode = true, out, f.mode
	return nil
}

// write puts the overlay on disk: each changed file through a temporary
// file renamed into place, so a reader never sees a half-written file, and
// the deletions last, so a failed write never costs a file. A failed write
// rolls back the writes before it.
func (o *overlay) write() error {
	var done []*stagedFile
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			if f := done[i]; f.had {
				_ = writeAtomic(o.env, f.path, f.prev, f.mode)
			} else {
				_ = o.env.remove(f.path)
			}
		}
	}
	for _, abs := range o.order {
		f := o.files[abs]
		if !f.exists || f.had && f.content == string(f.prev) {
			continue
		}
		if err := writeAtomic(o.env, f.path, []byte(f.content), f.mode); err != nil {
			rollback()
			return err
		}
		done = append(done, f)
	}
	for _, abs := range o.order {
		if f := o.files[abs]; f.had && !f.exists {
			if err := o.env.remove(f.path); err != nil {
				return fmt.Errorf("%v (the other changes were applied)", err)
			}
		}
	}
	return nil
}

// writeAtomic writes a file through a temporary file created beside it
// (O_EXCL, a random name: a link planted at a guessable name is never
// followed) and renamed into place. Beneath a root every step goes through
// the root, so a directory on the way swapped for a link out of the
// working set fails the write instead of landing it elsewhere.
func writeAtomic(env *Env, path string, content []byte, mode os.FileMode) error {
	root, rel, in, err := env.at(path)
	if err != nil {
		return err
	}
	if !in {
		return writeAtomicPlain(path, content, mode)
	}
	defer root.Close()
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".stavlos-"+hex.EncodeToString(rnd[:]))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Chmod(tmp, mode)
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
	}
	return err
}

// writeAtomicPlain is writeAtomic for a path under no root.
func writeAtomicPlain(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".stavlos-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(content)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// patchSummary is the result text: one line per file section.
func patchSummary(ops []patchOp) string {
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
	return strings.TrimRight(sb.String(), "\n")
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
	body, err := patchBody(strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"))
	if err != nil {
		return nil, err
	}
	var p patchParser
	for _, l := range body {
		if err := p.line(l); err != nil {
			return nil, err
		}
	}
	p.flushHunk()
	for _, op := range p.ops {
		if op.path == "" {
			return nil, fmt.Errorf("a %s section names no file", op.kind)
		}
	}
	for i := range p.ops {
		if p.ops[i].kind == "update" && len(p.ops[i].hunks) == 0 && p.ops[i].moveTo == "" {
			return nil, fmt.Errorf("update file %s has no hunks", p.ops[i].path)
		}
	}
	return p.ops, nil
}

// patchBody is the lines between the Begin and End markers, which may carry
// surrounding whitespace.
func patchBody(lines []string) ([]string, error) {
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
	return lines[start+1 : end], nil
}

// patchParser collects file sections and hunks line by line.
type patchParser struct {
	ops []patchOp
	cur *patchOp // the open section: the last of ops
	h   *hunk    // the open hunk of cur
}

func (p *patchParser) flushHunk() {
	if p.cur != nil && p.h != nil && len(p.h.lines) > 0 {
		p.cur.hunks = append(p.cur.hunks, *p.h)
	}
	p.h = nil
}

// open starts a file section (closing the open hunk first: cur must not
// outlive the append).
func (p *patchParser) open(kind, path string) {
	p.flushHunk()
	p.ops = append(p.ops, patchOp{kind: kind, path: strings.TrimSpace(path)})
	p.cur = &p.ops[len(p.ops)-1]
}

// line takes one line of the patch body.
func (p *patchParser) line(l string) error {
	switch {
	case strings.HasPrefix(l, "*** Add File: "):
		p.open("add", strings.TrimPrefix(l, "*** Add File: "))
	case strings.HasPrefix(l, "*** Delete File: "):
		p.open("delete", strings.TrimPrefix(l, "*** Delete File: "))
	case strings.HasPrefix(l, "*** Update File: "):
		p.open("update", strings.TrimPrefix(l, "*** Update File: "))
	case strings.HasPrefix(l, "*** Move to: "):
		if p.cur == nil || p.cur.kind != "update" {
			return errors.New("\"*** Move to:\" must follow an Update File header")
		}
		p.cur.moveTo = strings.TrimSpace(strings.TrimPrefix(l, "*** Move to: "))
	case strings.TrimSpace(l) == "*** End of File":
		if p.h != nil {
			p.h.eof = true
		}
	case strings.HasPrefix(l, "@@"):
		if p.cur == nil || p.cur.kind != "update" {
			return errors.New("hunk outside an Update File section")
		}
		p.flushHunk()
		p.h = &hunk{anchor: strings.TrimSpace(strings.TrimPrefix(l, "@@"))}
	default:
		return p.content(l)
	}
	return nil
}

// content takes a line inside a file section.
func (p *patchParser) content(l string) error {
	if p.cur == nil {
		if strings.TrimSpace(l) == "" {
			return nil
		}
		return fmt.Errorf("unexpected line before any file header: %q", l)
	}
	switch p.cur.kind {
	case "add":
		if strings.HasPrefix(l, "+") {
			p.cur.added = append(p.cur.added, l[1:])
		} else if strings.TrimSpace(l) != "" {
			return fmt.Errorf("add file %s: line must start with '+': %q", p.cur.path, l)
		}
	case "update":
		if p.h == nil {
			p.h = &hunk{}
		}
		switch {
		case l == "":
			p.h.lines = append(p.h.lines, hunkLine{' ', ""})
		case l[0] == ' ' || l[0] == '-' || l[0] == '+':
			p.h.lines = append(p.h.lines, hunkLine{l[0], l[1:]})
		default:
			// lenient: treat an unprefixed line as context
			p.h.lines = append(p.h.lines, hunkLine{' ', l})
		}
	case "delete":
		if strings.TrimSpace(l) != "" {
			return fmt.Errorf("delete file %s: unexpected content", p.cur.path)
		}
	}
	return nil
}

// --- applying ---

// applyHunks applies hunks in order to content. Each hunk's context and
// removed lines must match a unique run of lines (exact, then ignoring
// trailing whitespace, then ignoring all surrounding whitespace); the run
// is replaced by the context and added lines.
func applyHunks(content string, hunks []hunk) (string, error) {
	// A CRLF file is patched as LF and written back as CRLF: the model
	// writes hunks without carriage returns, and the file keeps its endings.
	crlf := strings.Contains(content, "\r\n")
	if crlf {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	hadTrailingNL := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}
	searchFrom := 0
	for hi, h := range hunks {
		old, new := h.oldNew()
		at, err := h.locate(lines, old, searchFrom)
		if err != nil {
			return "", fmt.Errorf("hunk %d: %v", hi+1, err)
		}
		if len(old) > 0 {
			new = h.replacement(lines, old, at)
		}
		lines = splice(lines, at, at+len(old), new)
		searchFrom = at + len(new)
	}
	out := strings.Join(lines, "\n")
	if hadTrailingNL || out != "" {
		out += "\n"
	}
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out, nil
}

// oldNew is the run of lines a hunk expects to find and the run it leaves,
// without the trailing empty context line models often add.
func (h hunk) oldNew() (old, new []string) {
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
	for len(old) > 0 && old[len(old)-1] == "" && len(new) > 0 && new[len(new)-1] == "" {
		old, new = old[:len(old)-1], new[:len(new)-1]
	}
	return old, new
}

// locate is where a hunk applies. A pure insertion goes after its anchor,
// or at the end of the file when it has none; an anchor that is not there
// is an error, never a silent append. Otherwise the old run is searched
// from the anchor (or from), then from the top, and an End of File hunk
// prefers the run at the end.
func (h hunk) locate(lines, old []string, from int) (int, error) {
	anchor := -1
	if h.anchor != "" {
		anchor = findLine(lines, h.anchor, from)
	}
	if len(old) == 0 {
		switch {
		case h.anchor == "":
			return len(lines), nil
		case anchor < 0:
			return 0, fmt.Errorf("anchor not found: %s", h.anchor)
		}
		return anchor + 1, nil
	}
	if anchor >= 0 {
		from = anchor
	}
	at := findRun(lines, old, from)
	if at < 0 && from > 0 {
		at = findRun(lines, old, 0)
	}
	if at < 0 {
		return 0, fmt.Errorf("context not found:\n%s", strings.Join(old, "\n"))
	}
	if h.eof && at+len(old) != len(lines) {
		if alt := findRunFrom(lines, old, len(lines)-len(old)); alt >= 0 {
			at = alt
		}
	}
	return at, nil
}

// replacement rebuilds a hunk's new run with the file's own text for its
// context lines, so a whitespace-lenient match never rewrites them.
func (h hunk) replacement(lines, old []string, at int) []string {
	var out []string
	oi := 0
	for _, l := range h.lines {
		switch l.op {
		case ' ':
			if oi < len(old) {
				out = append(out, lines[at+oi])
			}
			oi++
		case '-':
			oi++
		case '+':
			out = append(out, l.text)
		}
	}
	return out
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
