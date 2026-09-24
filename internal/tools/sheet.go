package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// Sheets is implemented by the agent runtime: the channel's sheets, HTML
// pages its agents write for the human (docs/web-ui.md). Every agent of the
// channel shares them; last write wins and the log says who wrote.
type Sheets interface {
	// Write creates a sheet (id "") or replaces one's content, and returns it.
	Write(id, title, html string) (SheetRef, error)
	List() []SheetRef
	Delete(id string) error
}

// SheetRef is a sheet as a tool reports it.
type SheetRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author"`
	Path   string `json:"path"` // the file: read and apply_patch edit it like any other
	Size   int    `json:"size"`
}

type sheetTool struct{}

type sheetInput struct {
	Action string `json:"action" desc:"write (create a sheet, or replace one's content when id is given), list, or delete" req:"true"`
	ID     string `json:"id" desc:"The sheet's id (s1, s2, …): required for delete, and for write when replacing"`
	Title  string `json:"title" desc:"A short title, shown on the sheet's tab; required when creating"`
	HTML   string `json:"html" desc:"write: the page, as a complete HTML document or a body fragment"`
}

func (sheetTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Sheet, Description: "An HTML page for the human, shown as a tab beside the chat in the web UI: a report, a comparison table, a diagram, a small interactive tool, when a page says it better than chat text. action write creates a sheet (give a title) or replaces the content of the sheet whose id you pass; list and delete manage them. A sheet is a file the result names: edit it with read and apply_patch rather than rewriting it. The page runs in a sandboxed frame with no network, so inline every script, style and image (data: URI or SVG) and use vanilla JavaScript. daisyUI's component classes (btn, card, badge, alert, stats, tabs, table, modal…) on a dark theme and the common Tailwind CSS 4 utilities (flex/grid with sm:/md:/lg:, spacing, typography, colours, borders) are already loaded: prefer them, and put anything unusual, such as an arbitrary value, in a <style> block. Then tell the human the title with message.",
		Schema: schemaOf(sheetInput{})}
}

func (sheetTool) Subject(in json.RawMessage) policy.Subject {
	var a sheetInput
	_ = decode(in, &a)
	return policy.Text(strings.TrimSpace(a.Action + " " + a.ID + " " + a.Title))
}

func (sheetTool) Run(_ context.Context, in json.RawMessage, env *Env) Result {
	if env.Sheets == nil {
		return errf("sheets are not available to this agent")
	}
	var a sheetInput
	if err := decode(in, &a); err != nil {
		return errf("%v", err)
	}
	switch a.Action {
	case "write":
		if strings.TrimSpace(a.HTML) == "" {
			return errf("write needs html")
		}
		if a.ID == "" && strings.TrimSpace(a.Title) == "" {
			return errf("a new sheet needs a title")
		}
		ref, err := env.Sheets.Write(a.ID, strings.TrimSpace(a.Title), a.HTML)
		if err != nil {
			return errf("%v", err)
		}
		verb := "created"
		if a.ID != "" {
			verb = "replaced"
		}
		return Result{Output: fmt.Sprintf("Sheet %s %s: %q (%d bytes), shown as a tab in the web UI.\nFile: %s (edit it with read and apply_patch)", ref.ID, verb, ref.Title, ref.Size, ref.Path)}
	case "list":
		refs := env.Sheets.List()
		if len(refs) == 0 {
			return Result{Output: "This channel has no sheets."}
		}
		var sb strings.Builder
		for _, r := range refs {
			fmt.Fprintf(&sb, "%s  %q  by %s  %d bytes  %s\n", r.ID, r.Title, r.Author, r.Size, r.Path)
		}
		return Result{Output: strings.TrimRight(sb.String(), "\n")}
	case "delete":
		if a.ID == "" {
			return errf("delete needs an id")
		}
		if err := env.Sheets.Delete(a.ID); err != nil {
			return errf("%v", err)
		}
		return Result{Output: "Sheet " + a.ID + " deleted."}
	}
	return errf("action must be write, list or delete")
}
