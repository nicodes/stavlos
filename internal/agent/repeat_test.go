package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/protocol"
)

func readCall(id, path string) step {
	b, _ := json.Marshal(map[string]any{"path": path})
	return reply(call(id, "read", string(b)))
}

// TestRepeatedCallAsks: the third identical call in a row, allowed by the
// rules as it is, asks the human; a no reaches the model as a denial that
// says what it repeated.
func TestRepeatedCallAsks(t *testing.T) {
	fm := &fakeModel{steps: []step{readCall("c1", "go.mod"), readCall("c2", "go.mod"), readCall("c3", "go.mod"), reply(text("done"))}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1"}`}, fm)
	h.answerWith(escalation.Answer{Value: protocol.AnswerDeny, Reason: "it will not change"})
	runTurn(t, s, h, "watch go.mod")
	if n := h.promptCount(); n != 1 {
		t.Fatalf("%d prompts for three identical reads, want 1", n)
	}
	p := h.prompts[0]
	if !p.Sticky || p.Tool != "read" || !strings.Contains(p.Question, "3 times in a row") {
		t.Fatalf("repeat prompt: %+v", p)
	}
	fin := finished(h, s.Root().ID)
	if len(fin) != 3 || fin[0].Denied || fin[1].Denied || !fin[2].Denied || !strings.Contains(fin[2].Output, "3rd time in a row") || !strings.Contains(fin[2].Output, "it will not change") {
		t.Fatalf("results: %+v", fin)
	}
}

// TestRepeatedCallAllowedRunsAndCountsAgain: a yes runs the call and the
// count starts over, so the next prompt comes three calls later, not one.
func TestRepeatedCallAllowedRunsAndCountsAgain(t *testing.T) {
	var steps []step
	for i := 0; i < 6; i++ {
		steps = append(steps, readCall("c"+string(rune('1'+i)), "go.mod"))
	}
	fm := &fakeModel{steps: append(steps, reply(text("done")))}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1"}`}, fm)
	h.answerWith(escalation.Answer{Value: protocol.AnswerAllow}, escalation.Answer{Value: protocol.AnswerAllow})
	runTurn(t, s, h, "watch go.mod")
	if n := h.promptCount(); n != 2 {
		t.Fatalf("%d prompts for six identical reads, want 2 (at the 3rd and the 6th)", n)
	}
	for _, f := range finished(h, s.Root().ID) {
		if f.Denied {
			t.Fatalf("an allowed repeat was denied: %+v", f)
		}
	}
}

// TestDifferentCallsAndYoloNeverAsk: calls that differ in their arguments
// are not a loop, and yolo asks about nothing.
func TestDifferentCallsAndYoloNeverAsk(t *testing.T) {
	fm := &fakeModel{steps: []step{readCall("c1", "go.mod"), readCall("c2", "go.sum"), readCall("c3", "go.mod"), readCall("c4", "go.mod"), reply(text("done"))}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1"}`}, fm)
	runTurn(t, s, h, "read around")
	if n := h.promptCount(); n != 0 {
		t.Fatalf("%d prompts for reads that differ, want 0", n)
	}
	fm = &fakeModel{steps: []step{readCall("c1", "go.mod"), readCall("c2", "go.mod"), readCall("c3", "go.mod"), readCall("c4", "go.mod"), reply(text("done"))}}
	s, h = newTestChannel(t, testConfig{json: `{"model":"fake/m1","mode":"yolo"}`}, fm)
	runTurn(t, s, h, "watch go.mod")
	if n := h.promptCount(); n != 0 {
		t.Fatalf("%d prompts in yolo, want 0", n)
	}
}

// TestJobOutputIsClippedLikeAToolResult: a job's output wakes the agent
// bounded by compaction.maxToolOutput, with the whole kept on disk and named.
func TestJobOutputIsClippedLikeAToolResult(t *testing.T) {
	s, _ := newTestChannel(t, testConfig{json: `{"model":"fake/m1","compaction":{"maxToolOutput":"2kb"}}`}, &fakeModel{})
	out := s.Root().clipJob(strings.Repeat("x", 5000))
	if len(out) > 2048+400 || !strings.Contains(out, "the whole output, 5000 bytes, is at") {
		t.Fatalf("clipped job output (%d bytes): %.200s", len(out), out)
	}
}

// TestReadAgainIsAnsweredFromTheContext: a second read of an unchanged file
// is a one-line note naming the call that holds the content; an edit in
// between makes it a full read again.
func TestReadAgainIsAnsweredFromTheContext(t *testing.T) {
	fm := &fakeModel{steps: []step{readCall("c1", "f.txt"), readCall("c2", "f.txt"), reply(text("done"))}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1"}`}, fm)
	_ = os.WriteFile(filepath.Join(s.Dir(), "f.txt"), []byte("hello\n"), 0o644)
	runTurn(t, s, h, "read it")
	fin := finished(h, s.Root().ID)
	if len(fin) != 2 || fin[0].Output != "hello\n" || fin[1].Output != "[unchanged since your read c1: its content is still in your context]" {
		t.Fatalf("%+v", fin)
	}
	fm.steps = append(fm.steps, readCall("c3", "f.txt"), reply(text("done")))
	_ = os.WriteFile(filepath.Join(s.Dir(), "f.txt"), []byte("changed\n"), 0o644)
	runTurn(t, s, h, "read it again")
	fin = finished(h, s.Root().ID)
	if fin[len(fin)-1].Output != "changed\n" {
		t.Fatalf("an edited file was not read in full: %q", fin[len(fin)-1].Output)
	}
}
