package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

// TestSheetsAreFilesTheLogKnows writes a sheet with the tool, edits it with
// apply_patch like any file (inside the working set, so nothing asks), and
// recovers the channel: the list, the author and the hash all come from the
// log, the page from the file.
func TestSheetsAreFilesTheLogKnows(t *testing.T) {
	t.Setenv("STAVLOS_DATA_DIR", t.TempDir())
	var sheetFilePath string
	fm := &fakeModel{steps: []step{
		reply(call("c1", "sheet", `{"action":"write","title":"Findings","html":"<h1 class=\"text-2xl\">Findings</h1>\n<p>old</p>\n"}`)),
		func(_ context.Context, req model.Request) (model.Response, error) {
			out := lastUserText(req) // the tool result names the file
			sheetFilePath = strings.Fields(out[strings.Index(out, "File: ")+6:])[0]
			patch, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Update File: " + sheetFilePath + "\n@@\n-<p>old</p>\n+<p>new</p>\n*** End Patch"})
			return call("c2", "apply_patch", string(patch)), nil
		},
		reply(call("c3", "sheet", `{"action":"write","title":"Second","html":"<p>two</p>"}`)),
		reply(call("c4", "sheet", `{"action":"delete","id":"s2"}`)),
		reply(call("c5", "sheet", `{"action":"write","id":"s9","html":"<p>nope</p>"}`)),
		reply(text("done")),
	}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","policy":{"apply_patch":"allow"}}`}, fm)
	runTurn(t, s, h, "make a sheet")

	if h.promptCount() != 0 {
		t.Fatalf("a sheet edit asked the human: %+v", h.prompts)
	}
	sheets := s.Sheets()
	if len(sheets) != 1 || sheets[0].ID != "s1" || sheets[0].Title != "Findings" || sheets[0].Author != "main" {
		t.Fatalf("sheets: %+v", sheets)
	}
	if filepath.Dir(sheetFilePath) != s.SheetDir() || strings.HasPrefix(sheetFilePath, s.Dir()) {
		t.Fatalf("the sheet lives in %s, want it under %s and outside the repository", sheetFilePath, s.SheetDir())
	}
	info, page, err := s.Sheet("s1")
	if err != nil || !strings.Contains(string(page), "<p>new</p>") || info.Size != len(page) {
		t.Fatalf("page after the patch: %v %q %+v", err, page, info)
	}
	written := h.ofType(event.SheetWritten, "")
	if len(written) != 3 { // s1 created, s1 patched, s2 created
		t.Fatalf("sheet.written events: %d\n%s", len(written), h.dump())
	}
	var first, patched event.SheetPayload
	_ = written[0].Decode(&first)
	_ = written[1].Decode(&patched)
	if first.Hash == patched.Hash || patched.ID != "s1" || strings.Contains(string(written[0].Payload), "Findings</h1>") {
		t.Fatalf("the patch must change the hash and the log must not carry the page: %s %s", written[0].Payload, written[1].Payload)
	}
	if fin := finished(h, s.Root().ID); !fin[len(fin)-1].IsError || !strings.Contains(fin[len(fin)-1].Output, "no sheet") {
		t.Fatalf("replacing an unknown sheet: %+v", fin[len(fin)-1])
	}
	if _, err := os.Stat(filepath.Join(s.SheetDir(), "s2.html")); !os.IsNotExist(err) {
		t.Fatalf("the deleted sheet's file is still there: %v", err)
	}

	// recovery: the same list from the log; ids keep counting past s2
	s.Stop()
	r, err := Recover(context.Background(), newFakeHost(fm), s.ID, s.Dir(), time.Now(), s.Config(), h.all())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	got := r.Sheets()
	if len(got) != 1 || got[0].Hash != patched.Hash || got[0].Title != "Findings" {
		t.Fatalf("recovered sheets: %+v", got)
	}
	ref, err := sheetsAPI{a: r.Root()}.Write("", "Third", "<p>3</p>")
	if err != nil || ref.ID != "s3" {
		t.Fatalf("ids are never reused: %+v %v", ref, err)
	}
}
