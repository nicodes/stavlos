// Package evtest builds event sequences for the transcript and render
// tests: an input as the runtime logs it (queued, then taken), a tool call
// as it does (the assistant message carrying the call, then tool.started).
package evtest

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

var ids atomic.Int64

// Ev is one event of agent.
func Ev(agent string, typ event.Type, p any) event.Event {
	e := event.Event{Channel: "s1", Agent: agent, Type: typ, Time: time.Now()}
	if p != nil {
		e.Payload = event.MustPayload(p)
	}
	return e
}

// Input queues in for agent and takes it: the events a model call that
// consumed it leaves. An empty ID gets a fresh one.
func Input(agent string, in event.Input) []event.Event {
	if in.ID == "" {
		in.ID = fmt.Sprintf("i%d", ids.Add(1))
	}
	return []event.Event{Ev(agent, event.InputQueued, in), Ev(agent, event.InputTaken, event.InputTakenPayload{IDs: []string{in.ID}})}
}

// Prompt is the human's prompt to agent, taken.
func Prompt(agent, text string) []event.Event {
	return Input(agent, event.Input{Kind: event.InputPrompt, Text: text})
}

// From is another agent's message of kind to agent, taken; from is its name.
func From(agent string, kind event.InputKind, from, text string) []event.Event {
	return Input(agent, event.Input{Kind: kind, Text: text, From: "id-" + from, FromName: from})
}

// Call is agent calling tool name with input: the assistant message that
// carries the call, then tool.started.
func Call(agent, id, name, input string) []event.Event {
	if input == "" {
		input = "{}"
	}
	return []event.Event{
		Ev(agent, event.AssistantMessage, event.AssistantMessagePayload{Blocks: []model.Block{{Type: model.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}}),
		Ev(agent, event.ToolStarted, event.ToolStartedPayload{CallID: id, Name: name}),
	}
}

// Done finishes call id of tool name with output.
func Done(agent, id, name, output string) event.Event {
	return Ev(agent, event.ToolFinished, event.ToolFinishedPayload{CallID: id, Name: name, Output: output})
}

// Seq flattens events and event slices into one sequence, numbered from 1.
func Seq(parts ...any) []event.Event {
	var out []event.Event
	for _, p := range parts {
		switch v := p.(type) {
		case event.Event:
			out = append(out, v)
		case []event.Event:
			out = append(out, v...)
		default:
			panic(fmt.Sprintf("evtest.Seq: %T", p))
		}
	}
	for i := range out {
		out[i].Seq = int64(i + 1)
	}
	return out
}

// Apply feeds a sequence (as Seq takes it) to anything that applies events.
func Apply(to interface{ Apply(event.Event) }, parts ...any) {
	for _, e := range Seq(parts...) {
		to.Apply(e)
	}
}
