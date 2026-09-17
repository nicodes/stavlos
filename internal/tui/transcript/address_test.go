package transcript

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/transcript/evtest"
)

func TestAddressingUsesTheViewsPerspective(t *testing.T) {
	for _, c := range []struct {
		from         string
		to           []string
		viewer, want string
	}{
		{"user", []string{"main", "scout"}, "user", "@main @scout"},
		{"main", []string{"user"}, "user", "@main:"},
		{"main", []string{"user", "scout"}, "user", "@main: @scout"},
		{"main", []string{"scout", "reader"}, "main", "@scout @reader"},
		{"scout", []string{"main", "reader"}, "main", "@scout: @main @reader"},
		{"user", []string{"main", "scout"}, "main", "@user: @main @scout"},
	} {
		if got, _ := messageAddress(c.from, c.to, c.viewer); got != c.want {
			t.Fatalf("%s / %v in %s: %s", c.from, c.to, c.viewer, got)
		}
	}
}

func TestMultiRecipientCallKeepsWaitingUntilAllReplies(t *testing.T) {
	tr := NewTranscript()
	tr.Apply(evtest.Ev("a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Name: "main"}))
	evtest.Apply(tr, evtest.Call("a", "call", "message", `{"to":["scout","reader","user"],"text":"review this"}`))
	tr.Apply(evtest.Ev("a", event.ToolFinished, event.ToolFinishedPayload{CallID: "call", Name: "message", Output: "request delivered to scout, reader, user; responses wake you"}))
	var item int
	for _, l := range tr.All() {
		if l.Tool == "message" {
			item = l.Item
			if l.Text != "@scout @reader @user review this" || strings.Contains(l.Text, "@main") {
				t.Fatalf("outgoing address: %+v", l)
			}
		}
	}
	check := func(want Tone) {
		t.Helper()
		if got := tr.Item(item)[0].Tone; got != want {
			t.Fatalf("message tone %v, want %v", got, want)
		}
	}
	check(ToneWorking)
	tr.answered("scout")
	check(ToneWorking)
	tr.answered("reader")
	check(ToneNone)
	in := InputLines(event.Input{Kind: event.InputRequest, FromName: "main", To: []string{"scout", "reader", "user"}, Text: "review this"}, "scout")
	if in[1].Text != "@main: @scout @reader @user review this" {
		t.Fatalf("incoming address: %+v", in)
	}
	p := protocol.PromptInfo{From: "main", Questions: []protocol.Question{{Question: "Which?"}}}
	if QuestionHeader(p, 0, true).Text != "@main: Which?" || QuestionHeader(p, 0, false).Text != "@user Which?" {
		t.Fatal("prompt addressing ignored perspective")
	}
}
