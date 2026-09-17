package transcript

import (
	"reflect"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tui/transcript/evtest"
)

func TestExplicitResponsesDoNotClearOtherPosts(t *testing.T) {
	tr := NewChat()
	post := func(id string, to ...string) {
		tr.Apply(evtest.Ev("", event.ChatPosted, event.ChatPayload{ID: id, RequestID: id, Kind: "request", To: to, Text: id}))
	}
	reply := func(kind string, ids ...string) {
		tr.Apply(evtest.Ev("a", event.ChatMessage, event.ChatPayload{From: "main", Kind: kind, ReplyTo: ids, Text: "update"}))
	}
	post("p1", "main", "scout")
	post("p2", "main")
	reply("info")
	reply("") // neither info nor an old unlinked response settles new requests
	if !reflect.DeepEqual(tr.Waiting(), []string{"main", "scout"}) {
		t.Fatal(tr.Waiting())
	}
	reply("response", "p2")
	if !reflect.DeepEqual(tr.Waiting(), []string{"main", "scout"}) {
		t.Fatal("one answer cleared another request")
	}
	reply("response", "p1")
	if !reflect.DeepEqual(tr.Waiting(), []string{"scout"}) {
		t.Fatal("broadcast answer cleared another agent")
	}
	tr.Apply(evtest.Ev("b", event.ChatMessage, event.ChatPayload{From: "scout", Kind: "response", ReplyTo: []string{"p1"}, Text: "done"}))
	if len(tr.Waiting()) != 0 {
		t.Fatal(tr.Waiting())
	}
}

func TestMessageWaitColorUsesRequestIDs(t *testing.T) {
	tr := NewTranscript()
	request := func(call, request string) int {
		evtest.Apply(tr, evtest.Call("a", call, "message", `{"to":["scout"],"text":"work"}`))
		tr.Apply(evtest.Ev("a", event.ToolFinished, event.ToolFinishedPayload{CallID: call, Name: "message", Output: "request delivered to scout; request_id: " + request}))
		return tr.Items() - 1
	}
	first, second := request("c1", "r1"), request("c2", "r2")
	reply := func(id string, refs []string) {
		tr.Apply(evtest.Ev("a", event.InputQueued, event.Input{ID: id, Kind: event.InputResponse, From: "b", FromName: "scout", ReplyTo: refs, Text: "answer"}))
		tr.Apply(evtest.Ev("a", event.InputTaken, event.InputTakenPayload{IDs: []string{id}}))
	}
	reply("legacy", nil)
	if tr.Item(first)[0].Tone != ToneWorking || tr.Item(second)[0].Tone != ToneWorking {
		t.Fatal("unlinked response settled tracked requests")
	}
	reply("i2", []string{"r2"})
	if tr.Item(first)[0].Tone != ToneWorking || tr.Item(second)[0].Tone != ToneNone {
		t.Fatal("response updated the wrong message")
	}
	reply("i1", []string{"r1"})
	if tr.Item(first)[0].Tone != ToneNone {
		t.Fatal("explicit response did not settle its message")
	}
}
