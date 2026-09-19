package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// Sheets are HTML pages the channel's agents write for the human, shown by
// the web UI (docs/web-ui.md). A sheet is a file in the channel's sheets
// directory, outside every repository; the log records that it was written
// (id, title, author, hash), never the page, so replay rebuilds the list and
// a viewer knows when to load a page again. Any agent of the channel may
// write, replace or delete any sheet: last write wins, and the log says who.

const (
	maxSheets    = 50      // per channel, so a looping agent cannot write thousands
	maxSheetSize = 2 << 20 // bytes of HTML
)

// sheetState is what the log says about one sheet.
type sheetState struct {
	id, title, author, hash string
	size                    int
	updated                 time.Time
}

var sheetFile = regexp.MustCompile(`^(s[0-9]+)\.html$`)

// SheetDir is where the channel's sheets live. Native tools treat it as part
// of the working set, so read and apply_patch edit a sheet like any file;
// commands do not: the sandbox hides the harness's data directory.
func (c *Channel) SheetDir() string { return filepath.Join(paths.DataDir(), "sheets", c.ID) }

func sheetPath(dir, id string) string { return filepath.Join(dir, id+".html") }

// applySheet folds sheet.written and sheet.deleted.
func (cs *channelState) applySheet(e event.Event) {
	var p event.SheetPayload
	if e.Decode(&p) != nil || p.ID == "" {
		return
	}
	if e.Type == event.SheetDeleted {
		delete(cs.sheets, p.ID)
		return
	}
	sh := cs.sheets[p.ID]
	if sh == nil {
		sh = &sheetState{id: p.ID}
		cs.sheets[p.ID] = sh
		if n, err := strconv.Atoi(strings.TrimPrefix(p.ID, "s")); err == nil && n > cs.sheetSeq {
			cs.sheetSeq = n // ids keep counting past every sheet there has been
		}
	}
	if p.Title != "" {
		sh.title = p.Title
	}
	sh.author, sh.hash, sh.size, sh.updated = p.Author, p.Hash, p.Size, e.Time
}

// Sheets lists the channel's sheets, oldest first.
func (c *Channel) Sheets() []protocol.SheetInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sheetsLocked()
}

func (c *Channel) sheetsLocked() []protocol.SheetInfo {
	out := make([]protocol.SheetInfo, 0, len(c.st.sheets))
	for n := 1; n <= c.st.sheetSeq; n++ {
		if sh := c.st.sheets["s"+strconv.Itoa(n)]; sh != nil {
			out = append(out, protocol.SheetInfo{ID: sh.id, Title: sh.title, Author: sh.author, Hash: sh.hash, Size: sh.size, Updated: sh.updated})
		}
	}
	return out
}

// Sheet is one sheet and its page, for the web UI to serve.
func (c *Channel) Sheet(id string) (protocol.SheetInfo, []byte, error) {
	for _, sh := range c.Sheets() {
		if sh.ID == id {
			b, err := os.ReadFile(sheetPath(c.SheetDir(), id))
			return sh, b, err
		}
	}
	return protocol.SheetInfo{}, nil, fmt.Errorf("no sheet %q", id)
}

type sheetsAPI struct{ a *Agent }

func (s sheetsAPI) ref(sh protocol.SheetInfo) tools.SheetRef {
	return tools.SheetRef{ID: sh.ID, Title: sh.Title, Author: sh.Author, Size: sh.Size, Path: sheetPath(s.a.c.SheetDir(), sh.ID)}
}

func (s sheetsAPI) List() []tools.SheetRef {
	var out []tools.SheetRef
	for _, sh := range s.a.c.Sheets() {
		out = append(out, s.ref(sh))
	}
	return out
}

func (s sheetsAPI) Write(id, title, html string) (tools.SheetRef, error) {
	if len(html) > maxSheetSize {
		return tools.SheetRef{}, fmt.Errorf("the page is %d bytes; a sheet holds at most %d", len(html), maxSheetSize)
	}
	c := s.a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" {
		if len(c.st.sheets) >= maxSheets {
			return tools.SheetRef{}, fmt.Errorf("this channel already has %d sheets: replace or delete one", maxSheets)
		}
		id = "s" + strconv.Itoa(c.st.sheetSeq+1)
	} else if c.st.sheets[id] == nil {
		return tools.SheetRef{}, fmt.Errorf("no sheet %q: leave id empty to create one", id)
	}
	dir := c.SheetDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tools.SheetRef{}, err
	}
	if err := writeFileAtomic(sheetPath(dir, id), []byte(html)); err != nil {
		return tools.SheetRef{}, err
	}
	if err := c.sheetWrittenLocked(s.a, id, title, []byte(html)); err != nil {
		return tools.SheetRef{}, err
	}
	for _, sh := range c.sheetsLocked() {
		if sh.ID == id {
			return s.ref(sh), nil
		}
	}
	return tools.SheetRef{}, errors.New("the sheet was not recorded")
}

func (s sheetsAPI) Delete(id string) error {
	c := s.a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.sheets[id] == nil {
		return fmt.Errorf("no sheet %q", id)
	}
	if err := os.Remove(sheetPath(c.SheetDir(), id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := c.commitLocked(context.Background(), c.event(s.a.ID, event.SheetDeleted, event.SheetPayload{ID: id}))
	return err
}

func (c *Channel) sheetWrittenLocked(a *Agent, id, title string, page []byte) error {
	sum := sha256.Sum256(page)
	_, err := c.commitLocked(context.Background(), c.event(a.ID, event.SheetWritten,
		event.SheetPayload{ID: id, Title: title, Author: a.state().name, Hash: hex.EncodeToString(sum[:]), Size: len(page)}))
	return err
}

// sheetsPatched records what an apply_patch did to the sheets it touched, so
// an edit made with the ordinary file tools reaches the viewers like one
// made with the sheet tool. A file that is no known sheet is left alone:
// sheets are created by the sheet tool, which numbers and caps them.
func (a *Agent) sheetsPatched(sub policy.Subject) {
	if sub.Kind != policy.KindPath {
		return
	}
	c := a.c
	dir := tools.ResolvePath("", c.SheetDir())
	for _, v := range sub.Values {
		p := tools.ResolvePath(c.Dir(), v)
		m := sheetFile.FindStringSubmatch(filepath.Base(p))
		if m == nil || filepath.Dir(p) != dir {
			continue
		}
		page, err := os.ReadFile(p)
		c.mu.Lock()
		switch {
		case c.st.sheets[m[1]] == nil:
		case errors.Is(err, os.ErrNotExist):
			_, _ = c.commitLocked(context.Background(), c.event(a.ID, event.SheetDeleted, event.SheetPayload{ID: m[1]}))
		case err == nil:
			_ = c.sheetWrittenLocked(a, m[1], "", page)
		}
		c.mu.Unlock()
	}
}

func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".sheet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Chmod(0o600)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
