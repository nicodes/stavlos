package render

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/transcript"
	"github.com/nicodes/stavlos/internal/tui/transcript/evtest"
)

// TestRenderMatchesRenderAll: the cached render is exactly the uncached
// render of All(), for every cursor position and option mix, as the
// transcript goes through streaming, nesting, prompts and compaction, with
// one cache carried across all of it.
func TestRenderMatchesRenderAll(t *testing.T) {
	markCursorForTest(t)
	tr := transcript.NewTranscript()
	cache := &Cache{}
	check := func(stage string) {
		t.Helper()
		items := tr.Items()
		for _, base := range []Options{
			{Width: 80},
			{Width: 36, Details: true},
			{Width: 80, NoFold: true},
			{Width: 60, Working: true, Spinner: "*", Verb: "Galloping", Stats: "(1s)"},
			{Width: 80, CompactFrame: 3},
		} {
			for cur := -1; cur <= items; cur++ {
				o := base
				if cur >= 0 {
					o.Focused, o.Cursor = true, cur
					o.Expanded = map[int]bool{cur: cur%2 == 0}
				}
				want, wantRows := Lines(tr.All(), o)
				got, gotRows := Transcript(tr, cache, o)
				if got != want || !reflect.DeepEqual(gotRows, wantRows) {
					t.Fatalf("%s, opts %+v:\ngot:\n%s\nwant:\n%s\nrows %v vs %v", stage, o, got, want, gotRows, wantRows)
				}
			}
		}
	}
	check("empty")
	tr.Apply(mk(1, "a", event.TurnStarted, event.TurnPayload{Turn: 1}))
	evtest.Apply(tr, evtest.Prompt("a", "build **it**\nplease"))
	evtest.Apply(tr, evtest.Call("a", "c1", "shell", `{"command":"make"}`))
	check("running call")
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, ToolName: "shell", Text: "compiling\nlinking\n"})
	check("stream under the call")
	tr.Apply(mk(4, "a", event.AskRequested, event.AskRequestedPayload{ID: "p1", Kind: "permission", Tool: "shell"}))
	tr.Apply(mk(5, "a", event.AskResolved, event.AskResolvedPayload{ID: "p1", Outcome: event.AskAnswered, Answer: "allow"}))
	tr.Apply(mk(6, "a", event.ToolFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "1\n2\n3\n4\n5\n6"}))
	check("finished call")
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "Here is a long answer that wraps across the narrow width more than once"})
	check("text stream")
	tr.Apply(mk(7, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "# Done\nall good"}}}))
	tr.Apply(mk(8, "a", event.CompactionStarted, event.CompactionPayload{Before: 1000}))
	check("compacting")
	tr.Apply(mk(9, "a", event.CompactionDone, event.CompactionPayload{Summary: "S", Before: 1000, After: 100}))
	tr.Apply(mk(10, "a", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: event.ReasonCancelled}))
	check("ended")
}

// TestRenderReusesUnchangedItems: a spinner frame or a streamed token
// renders no committed item again; moving the cursor renders the two items
// it leaves and enters; a finished call renders only its own item.
func TestRenderReusesUnchangedItems(t *testing.T) {
	tr := transcript.NewTranscript()
	cache := &Cache{}
	evtest.Apply(tr, evtest.Prompt("a", "hi"))
	evtest.Apply(tr, evtest.Call("a", "c1", "shell", `{"command":"ls"}`))
	tr.Apply(mk(3, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "working on it"}}}))
	o := Options{Width: 80, Focused: true, Cursor: 0}
	misses := func(step string, want int, o Options) {
		t.Helper()
		before := cache.misses
		Transcript(tr, cache, o)
		if got := cache.misses - before; got != want {
			t.Fatalf("%s: rendered %d items, want %d", step, got, want)
		}
	}
	misses("first render", 3, o)
	misses("same options", 0, o)
	o.Working, o.Spinner = true, "⠙"
	misses("spinner frame", 0, o)
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "more " + strings.Repeat("x", 10)})
	misses("streamed token", 0, o)
	o.Cursor = 1
	misses("cursor moved", 2, o)
	tr.Apply(mk(4, "a", event.ToolFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "a"}))
	misses("call finished", 1, o)
	o.Width = 60
	misses("resized", 3, o)
}
