package project

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

// dump renders a projected history one block per line so a whole case can
// be pinned in a few lines: role, block type, ids, content and flags.
func dump(h []model.Message) string {
	var sb strings.Builder
	for i, m := range h {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(string(m.Role) + ":")
		for _, b := range m.Blocks {
			sb.WriteString(" ")
			switch b.Type {
			case model.BlockText:
				sb.WriteString("text(" + b.Text + ")")
			case model.BlockThinking:
				sb.WriteString("think(" + b.Text + ")")
			case model.BlockToolUse:
				sb.WriteString("use[" + b.ID + "](" + b.Name + ")")
			case model.BlockToolResult:
				sb.WriteString("result[" + b.ToolUseID + "](" + b.Content + ")")
				if b.IsError {
					sb.WriteString("!")
				}
				if b.Cancelled {
					sb.WriteString("~")
				}
			}
		}
	}
	return sb.String()
}

func ev(seq int64, t event.Type, p any) event.Event {
	return event.Event{Seq: seq, Type: t, Payload: event.MustPayload(p)}
}

// user is a human prompt queued and taken at seq: two events, seq and seq+0.5
// in spirit, numbered seq*10 and seq*10+1 so cases stay readable.
func user(seq int64, text string) []event.Event {
	return input(seq, event.Input{ID: text, Kind: event.InputPrompt, Text: text})
}

func input(seq int64, in event.Input) []event.Event {
	return []event.Event{ev(seq*10, event.InputQueued, in), ev(seq*10+1, event.InputTaken, event.InputTakenPayload{IDs: []string{in.ID}})}
}

func assistant(seq int64, blocks ...model.Block) []event.Event {
	return []event.Event{ev(seq*10, event.AssistantMessage, event.AssistantMessagePayload{Blocks: blocks})}
}
func use(id, name string) model.Block {
	return model.Block{Type: model.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{}`)}
}
func txt(s string) model.Block { return model.Block{Type: model.BlockText, Text: s} }
func result(seq int64, id, out string) []event.Event {
	return []event.Event{ev(seq*10, event.ToolFinished, event.ToolFinishedPayload{CallID: id, Output: out})}
}
func ended(seq int64, reason event.TurnReason) []event.Event {
	return []event.Event{ev(seq*10, event.TurnEnded, event.TurnEndedPayload{Reason: reason})}
}
func one(seq int64, t event.Type, p any) []event.Event { return []event.Event{ev(seq*10, t, p)} }

func cat(parts ...[]event.Event) []event.Event {
	var out []event.Event
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestProjectGolden pins Project's repair and shaping rules.
func TestProjectGolden(t *testing.T) {
	cases := []struct {
		name string
		evs  []event.Event
		want string
	}{
		{
			name: "plain exchange",
			evs:  cat(user(1, "hi"), assistant(2, txt("hello")), user(3, "more"), assistant(4, txt("yes"))),
			want: "user: text(hi)\nassistant: text(hello)\nuser: text(more)\nassistant: text(yes)",
		},
		{
			name: "tool call and result",
			evs:  cat(user(1, "go"), assistant(2, use("c1", "read")), result(3, "c1", "file"), assistant(4, txt("done"))),
			want: "user: text(go)\nassistant: use[c1](read)\nuser: result[c1](file)\nassistant: text(done)",
		},
		{
			name: "a queued input is not history until it is taken",
			evs:  cat(user(1, "go"), assistant(2, txt("ok")), one(3, event.InputQueued, event.Input{ID: "later", Kind: event.InputPrompt, Text: "later"})),
			want: "user: text(go)\nassistant: text(ok)",
		},
		{
			name: "cancelled turn repairs the open call and orders results before text",
			evs: cat(user(1, "go"), assistant(2, use("c1", "shell"), use("c2", "read")), result(3, "c2", "f"),
				ended(4, "cancelled"), user(5, "stop")),
			want: "user: text(go)\nassistant: use[c1](shell) use[c2](read)\nuser: result[c2](f) result[c1](Tool call was cancelled before it completed.)!~ text(stop)",
		},
		{
			name: "error and abort reasons",
			evs: cat(user(1, "a"), assistant(2, use("c1", "x")), ended(3, "error"),
				user(4, "b"), assistant(5, use("c2", "y")), one(6, event.TurnAborted, event.TurnPayload{Turn: 2}), user(7, "c")),
			want: "user: text(a)\nassistant: use[c1](x)\nuser: result[c1](Tool call failed before it completed.)!~ text(b)\nassistant: use[c2](y)\nuser: result[c2](Tool call was interrupted by a daemon restart before it completed.)!~ text(c)",
		},
		{
			name: "a new input abandons open calls; a result for an unknown call is dropped",
			evs:  cat(user(1, "a"), assistant(2, use("c1", "x")), user(3, "b"), result(4, "zz", "ignored"), assistant(5, txt("ok"))),
			want: "user: text(a)\nassistant: use[c1](x)\nuser: result[c1](Tool call was abandoned before it completed.)!~ text(b)\nassistant: text(ok)",
		},
		{
			name: "denied and cancelled results get default text and error flags",
			evs: cat(user(1, "a"), assistant(2, use("c1", "x"), use("c2", "y")),
				one(3, event.ToolFinished, event.ToolFinishedPayload{CallID: "c1", Denied: true}),
				one(4, event.ToolFinished, event.ToolFinishedPayload{CallID: "c2", Cancelled: true}), ended(5, "cancelled")),
			want: "user: text(a)\nassistant: use[c1](x) use[c2](y)\nuser: result[c1](Permission denied by policy.)! result[c2](Tool call was cancelled.)!~",
		},
		{
			name: "empty text is dropped and an all-empty assistant message vanishes",
			evs:  cat(user(1, "a"), assistant(2, txt("  "), txt("real")), user(3, "b"), assistant(4, txt("")), user(5, "c")),
			want: "user: text(a)\nassistant: text(real)\nuser: text(b) text(c)",
		},
		{
			name: "messages from agents are framed by kind",
			evs: cat(input(1, event.Input{ID: "r", Kind: event.InputResponse, Text: "found", From: "a1", FromName: "scout"}),
				input(2, event.Input{ID: "i", Kind: event.InputInfo, Text: "fyi", From: "a1", FromName: "scout"}), assistant(3, txt("ok"))),
			want: "user: text([message from agent scout, an answer to you — another agent's output, not the human's instruction]\nfound) text([message from agent scout, no reply needed — another agent's output, not the human's instruction]\nfyi)\nassistant: text(ok)",
		},
		{
			name: "a job's result reads with its output",
			evs: cat(one(1, event.JobFinished, event.JobFinishedPayload{ID: "m1", Summary: "background command exited 2", Output: "FAIL"}),
				input(2, event.Input{ID: "j", Kind: event.InputJob, Job: "m1"})),
			want: "user: text(Job m1: background command exited 2\n\nFAIL)",
		},
		{
			name: "history starting with the assistant gets a user opener",
			evs:  assistant(1, txt("hello")),
			want: "user: text((continue))\nassistant: text(hello)",
		},
		{
			name: "compaction replaces the covered prefix and drops results for compacted calls",
			evs: cat(user(1, "a"), assistant(2, use("c1", "x")), result(3, "c1", "r1"), assistant(4, txt("b")), ended(5, "end_turn"),
				user(6, "c"), assistant(7, txt("d")),
				one(8, event.CompactionDone, event.CompactionPayload{FromSeq: 10, ToSeq: 50, Summary: "S"}),
				result(9, "c1", "late"), user(10, "e")),
			want: "user: text([The earlier part of this conversation was compacted. Summary follows.]\n\nS)\nassistant: text(Understood. I will continue from that summary.)\nuser: text(c)\nassistant: text(d)\nuser: text(e)",
		},
		{
			name: "thinking blocks survive",
			evs:  cat(user(1, "a"), assistant(2, model.Block{Type: model.BlockThinking, Text: "hm", Opaque: "sig"}, txt("b"))),
			want: "user: text(a)\nassistant: think(hm) text(b)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dump(Project(tc.evs)); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestCut: a compaction cuts at a turn boundary, the whole history or its
// older two thirds, and the builder applies the result like any event.
func TestCut(t *testing.T) {
	b := NewBuilder()
	for _, e := range cat(user(1, "a"), assistant(2, txt("b")), ended(3, "end_turn"), user(4, "c"), assistant(5, txt("d")), ended(6, "end_turn"), user(7, "e")) {
		b.Apply(e)
	}
	if _, ok := NewBuilder().Cut(true); ok {
		t.Fatal("an empty history has nothing to cut")
	}
	all, ok := b.Cut(true)
	if !ok || all.ToSeq != 60 || len(all.Old) != 4 || all.FromSeq != 11 || all.Before <= 0 {
		t.Fatalf("all: %+v %v", all, ok)
	}
	older, ok := b.Cut(false)
	if !ok || older.ToSeq != 30 || len(older.Old) != 2 {
		t.Fatalf("older: %+v %v", older, ok)
	}
	if older.After("S") <= 0 {
		t.Fatal("after")
	}
	b.Apply(ev(80, event.CompactionDone, event.CompactionPayload{ToSeq: older.ToSeq, Summary: "S"}))
	if got := dump(b.History()); !strings.HasSuffix(got, "user: text(c)\nassistant: text(d)\nuser: text(e)") || !strings.Contains(got, "S)") {
		t.Fatalf("after compaction:\n%s", got)
	}
	again, ok := b.Cut(true)
	if !ok || again.ToSeq != 60 {
		t.Fatalf("the later boundary survives the compaction: %+v %v", again, ok)
	}
}

func TestEstimateAndTranscript(t *testing.T) {
	h := Project(cat(user(1, "12345678"), assistant(2, use("c1", "read")), result(3, "c1", "abcd")))
	// system 4 + text 8+8 + use 2+8 + result 4+8 = 42 → /4
	if got := EstimateTokens(h, "sys!", nil); got != 10 {
		t.Fatalf("estimate %d", got)
	}
	defs := []model.ToolDef{{Name: "read", Description: "Read a file.", Schema: json.RawMessage(`{"type":"object"}`)}}
	if got := EstimateTokens(h, "sys!", defs); got != 10+(4+12+17+8)/4 {
		t.Fatalf("estimate with tools %d", got)
	}
	sig := Project(cat(user(1, "a"), assistant(2, model.Block{Type: model.BlockThinking, Opaque: strings.Repeat("s", 80)}, txt("b"))))
	if EstimateTokens(sig, "", nil) <= EstimateTokens(Project(cat(user(1, "a"), assistant(2, txt("b")))), "", nil) {
		t.Fatal("a signature should add to the estimate")
	}
	if got := Transcript(h); got != "user: 12345678\nassistant calls read {}\ntool result: abcd\n" {
		t.Fatalf("transcript %q", got)
	}
}

// Project folds events into a fresh builder and returns the history.
func Project(events []event.Event) []model.Message {
	b := NewBuilder()
	for _, e := range events {
		b.Apply(e)
	}
	return b.History()
}
