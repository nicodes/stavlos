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

func user(seq int64, text string) event.Event {
	return ev(seq, event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: text})
}
func assistant(seq int64, blocks ...model.Block) event.Event {
	return ev(seq, event.AssistantMessage, event.AssistantMessagePayload{Blocks: blocks})
}
func use(id, name string) model.Block {
	return model.Block{Type: model.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{}`)}
}
func txt(s string) model.Block { return model.Block{Type: model.BlockText, Text: s} }
func result(seq int64, id, out string) event.Event {
	return ev(seq, event.ToolCallFinished, event.ToolFinishedPayload{CallID: id, Output: out})
}
func ended(seq int64, reason string) event.Event {
	return ev(seq, event.TurnEnded, event.TurnEndedPayload{Reason: reason})
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
			evs:  []event.Event{user(1, "hi"), assistant(2, txt("hello")), user(3, "more"), assistant(4, txt("yes"))},
			want: "user: text(hi)\nassistant: text(hello)\nuser: text(more)\nassistant: text(yes)",
		},
		{
			name: "tool call and result",
			evs:  []event.Event{user(1, "go"), assistant(2, use("c1", "read")), result(3, "c1", "file"), assistant(4, txt("done"))},
			want: "user: text(go)\nassistant: use[c1](read)\nuser: result[c1](file)\nassistant: text(done)",
		},
		{
			name: "dangling call at the end reads as still running",
			evs:  []event.Event{user(1, "go"), assistant(2, use("c1", "shell"))},
			want: "user: text(go)\nassistant: use[c1](shell)\nuser: result[c1](Tool call is still running before it completed.)!~",
		},
		{
			name: "cancelled turn repairs the open call and orders results before text",
			evs: []event.Event{user(1, "go"), assistant(2, use("c1", "shell"), use("c2", "read")), result(3, "c2", "f"),
				ended(4, "cancelled"), user(5, "stop")},
			want: "user: text(go)\nassistant: use[c1](shell) use[c2](read)\nuser: result[c2](f) result[c1](Tool call was cancelled before it completed.)!~ text(stop)",
		},
		{
			name: "error and abort reasons",
			evs: []event.Event{user(1, "a"), assistant(2, use("c1", "x")), ended(3, "error"),
				user(4, "b"), assistant(5, use("c2", "y")), ev(6, event.TurnAborted, event.TurnPayload{Turn: 2}), user(7, "c")},
			want: "user: text(a)\nassistant: use[c1](x)\nuser: result[c1](Tool call failed before it completed.)!~ text(b)\nassistant: use[c2](y)\nuser: result[c2](Tool call was interrupted by a daemon restart before it completed.)!~ text(c)",
		},
		{
			name: "a new message abandons open calls; a result for an unknown call is dropped",
			evs:  []event.Event{user(1, "a"), assistant(2, use("c1", "x")), user(3, "b"), result(4, "zz", "ignored"), assistant(5, txt("ok"))},
			want: "user: text(a)\nassistant: use[c1](x)\nuser: result[c1](Tool call was abandoned before it completed.)!~ text(b)\nassistant: text(ok)",
		},
		{
			name: "denied and cancelled results get default text and error flags",
			evs: []event.Event{user(1, "a"), assistant(2, use("c1", "x"), use("c2", "y")),
				ev(3, event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Denied: true}),
				ev(4, event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c2", Cancelled: true}), ended(5, "cancelled")},
			want: "user: text(a)\nassistant: use[c1](x) use[c2](y)\nuser: result[c1](Permission denied by policy.)! result[c2](Tool call was cancelled.)!~",
		},
		{
			name: "empty text is dropped and an all-empty assistant message vanishes",
			evs:  []event.Event{user(1, "a"), assistant(2, txt("  "), txt("real")), user(3, "b"), assistant(4, txt("")), user(5, "c")},
			want: "user: text(a)\nassistant: text(real)\nuser: text(b) text(c)",
		},
		{
			name: "messages from agents are labelled",
			evs: []event.Event{ev(1, event.UserMessage, event.UserMessagePayload{Kind: "agent_response", Text: "found", From: "scout (a1)"}),
				assistant(2, txt("ok"))},
			want: "user: text([message from agent scout (a1)]\nfound)\nassistant: text(ok)",
		},
		{
			name: "history starting with the assistant gets a user opener",
			evs:  []event.Event{assistant(1, txt("hello"))},
			want: "user: text((continue))\nassistant: text(hello)",
		},
		{
			name: "compaction replaces the covered prefix and drops results for compacted calls",
			evs: []event.Event{user(1, "a"), assistant(2, use("c1", "x")), result(3, "c1", "r1"), assistant(4, txt("b")), ended(5, "end_turn"),
				user(6, "c"), assistant(7, txt("d")),
				ev(8, event.Compacted, event.CompactedPayload{FromSeq: 1, ToSeq: 5, Summary: "S"}),
				result(9, "c1", "late"), user(10, "e")},
			want: "user: text([The earlier part of this conversation was compacted. Summary follows.]\n\nS)\nassistant: text(Understood. I will continue from that summary.)\nuser: text(c)\nassistant: text(d)\nuser: text(e)",
		},
		{
			name: "thinking blocks survive",
			evs:  []event.Event{user(1, "a"), assistant(2, model.Block{Type: model.BlockThinking, Text: "hm", Signature: "sig"}, txt("b"))},
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

func TestEstimateAndTranscript(t *testing.T) {
	h := Project([]event.Event{user(1, "12345678"), assistant(2, use("c1", "read")), result(3, "c1", "abcd")})
	// system 4 + text 8+8 + use 2+8 + result 4+8 = 42 → /4
	if got := EstimateTokens(h, "sys!"); got != 10 {
		t.Fatalf("estimate %d", got)
	}
	want := "user: 12345678\nassistant calls read {}\ntool result: abcd\n"
	if got := Transcript(h); got != want {
		t.Fatalf("transcript %q", got)
	}
}
