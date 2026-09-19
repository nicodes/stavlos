package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/testutil"
)

// The harness drives a Channel without the daemon: an in-memory log, a
// scripted model and a scripted human. Every test that pins the turn loop,
// permissions or recovery builds on it.

// fakeHost is the daemon's side of agent.Host, in memory.
type fakeHost struct {
	changed  []string // dirs whose instructions changed (ProjectChanged)
	mu       sync.Mutex
	events   []event.Event
	seq      map[string]int64
	ch       chan event.Event
	m        *fakeModel
	answer   func(ctx context.Context, p protocol.PromptInfo) escalation.Answer
	prompts  []protocol.PromptInfo
	streams  []protocol.StreamNotification
	badModel error // CheckModel/Resolve fail with this when set
	failNext error // the next Append fails with this, once
	usage    map[string]model.PlanUsage
	variants map[string][]string // model → the variants it takes; nil = low and high for every model
}

func newFakeHost(m *fakeModel) *fakeHost {
	return &fakeHost{seq: map[string]int64{}, ch: make(chan event.Event, 10000), m: m}
}

func (h *fakeHost) Append(_ context.Context, evs ...event.Event) ([]event.Event, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failNext != nil {
		err := h.failNext
		h.failNext = nil
		return nil, err
	}
	out := make([]event.Event, 0, len(evs))
	for _, e := range evs {
		h.seq[e.Channel]++
		e.Seq = h.seq[e.Channel]
		e.Global = int64(len(h.events) + 1)
		e.Time = time.Now().UTC()
		h.events = append(h.events, e)
		out = append(out, e)
		select {
		case h.ch <- e:
		default:
		}
	}
	return out, nil
}

func (h *fakeHost) Stream(n protocol.StreamNotification) {
	h.mu.Lock()
	h.streams = append(h.streams, n)
	h.mu.Unlock()
}

func (h *fakeHost) Resolve(modelID string) (model.Model, model.Info, error) {
	if h.badModel != nil {
		return nil, model.Info{}, h.badModel
	}
	if modelID == "" {
		return nil, model.Info{}, errors.New("no model")
	}
	return h.m, model.Info{ContextWindow: 200_000}, nil
}

func (h *fakeHost) CheckModel(string) error { return h.badModel }

func (h *fakeHost) ProjectChanged(dir string) {
	h.mu.Lock()
	h.changed = append(h.changed, dir)
	h.mu.Unlock()
}

// Variants is what a model takes: low and high, unless the test says what
// each model takes (a model missing from that map takes none).
func (h *fakeHost) Variants(id string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.variants != nil {
		return h.variants[id]
	}
	return []string{"low", "high"}
}

func (h *fakeHost) PlanUsage() map[string]model.PlanUsage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]model.PlanUsage{}
	for k, v := range h.usage {
		out[k] = v
	}
	return out
}

func (h *fakeHost) MarkLimited(provider string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.usage == nil {
		h.usage = map[string]model.PlanUsage{}
	}
	u := h.usage[provider]
	u.LimitedUntil = until
	h.usage[provider] = u
}

func (h *fakeHost) Prompt(ctx context.Context, p protocol.PromptInfo, opened func()) escalation.Answer {
	h.mu.Lock()
	h.prompts = append(h.prompts, p)
	f := h.answer
	h.mu.Unlock()
	if opened != nil {
		opened() // open before any answer, as the daemon's escalation does
	}
	if f == nil {
		return escalation.Answer{Value: "deny"}
	}
	return f(ctx, p)
}

// answerWith scripts the human: one answer per prompt, in order; the last
// one repeats.
func (h *fakeHost) answerWith(answers ...escalation.Answer) {
	var i int
	var mu sync.Mutex
	h.answer = func(context.Context, protocol.PromptInfo) escalation.Answer {
		mu.Lock()
		defer mu.Unlock()
		a := answers[min(i, len(answers)-1)]
		i++
		return a
	}
}

func (h *fakeHost) promptCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.prompts)
}

func (h *fakeHost) all() []event.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event.Event(nil), h.events...)
}

// ofType lists the events of one type for one agent ("" = any).
func (h *fakeHost) ofType(t event.Type, agent string) []event.Event {
	var out []event.Event
	for _, e := range h.all() {
		if e.Type == t && (agent == "" || e.Agent == agent) {
			out = append(out, e)
		}
	}
	return out
}

// waitFor blocks until an event of type t for agent (or any if "") arrives
// on the live channel. Events already delivered are not replayed: call it
// before the action, or use waitUntil.
func (h *fakeHost) waitFor(t *testing.T, typ event.Type, agent string) event.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-h.ch:
			if e.Type == typ && (agent == "" || e.Agent == agent) {
				return e
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s (%s)\n%s", typ, agent, h.dump())
		}
	}
}

// waitTurnEnd waits for the TurnEnded event of a specific turn, skipping
// earlier turns still in the channel.
func (h *fakeHost) waitTurnEnd(t *testing.T, agent string, turn int) event.TurnEndedPayload {
	t.Helper()
	for {
		e := h.waitFor(t, event.TurnEnded, agent)
		var p event.TurnEndedPayload
		_ = e.Decode(&p)
		if p.Turn >= turn {
			return p
		}
	}
}

// waitUntil polls cond every 10 ms for up to 10 s.
func waitUntil(t *testing.T, h *fakeHost, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition never held\n%s", h.dump())
}

func (h *fakeHost) dump() string {
	var sb strings.Builder
	for _, e := range h.all() {
		p := string(e.Payload)
		if len(p) > 120 {
			p = p[:120] + "…"
		}
		fmt.Fprintf(&sb, "%3d %-22s %-12s %s\n", e.Seq, e.Type, e.Agent, p)
	}
	return sb.String()
}

// fakeModel is scripted: each call pops the next step. Steps for the root
// and for subagents (whose system prompt says so) are separate queues; an
// empty queue answers "done".
type fakeModel struct {
	mu         sync.Mutex
	steps      []step
	childSteps []step
	calls      []model.Request
}

type step func(ctx context.Context, req model.Request) (model.Response, error)

func (f *fakeModel) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	q := &f.steps
	if strings.Contains(req.System, "You are a subagent") {
		q = &f.childSteps
	}
	var s step
	if len(*q) > 0 {
		s = (*q)[0]
		*q = (*q)[1:]
	}
	f.mu.Unlock()
	if s == nil {
		return text("done"), nil
	}
	if onDelta != nil {
		onDelta(model.Delta{Text: "…"})
	}
	r, err := s(ctx, req)
	if err != nil {
		return r, err
	}
	if r.Usage == (model.Usage{}) {
		r.Usage = model.Usage{InputTokens: 10, OutputTokens: 5}
	}
	return testutil.BindReplies(req, r), nil
}

func (f *fakeModel) requests() []model.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Request(nil), f.calls...)
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Response builders.
func text(s string) model.Response {
	return model.Response{Blocks: []model.Block{{Type: model.BlockText, Text: s}}, StopReason: model.StopEndTurn}
}
func call(id, name, input string) model.Response {
	return model.Response{Blocks: []model.Block{{Type: model.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}, StopReason: model.StopToolUse}
}
func reply(r model.Response) step {
	return func(context.Context, model.Request) (model.Response, error) { return r, nil }
}
func fail(err error) step {
	return func(context.Context, model.Request) (model.Response, error) { return model.Response{}, err }
}

// lastUserText is the text of the last block of the last message the model
// was given.
func lastUserText(req model.Request) string {
	if len(req.Messages) == 0 {
		return ""
	}
	bs := req.Messages[len(req.Messages)-1].Blocks
	if len(bs) == 0 {
		return ""
	}
	b := bs[len(bs)-1]
	if strings.HasPrefix(b.Text, "[harness state") && len(bs) > 1 {
		b = bs[len(bs)-2] // the per-request state note rides after the last input
	}
	return b.Text + b.Content
}

// testConfig writes a global stavlos.json (and optional role files) into a
// fresh config dir and loads the effective config for a fresh work dir.
type testConfig struct {
	json      string
	reminders bool              // remind agents that end a turn owing a reply (off in tests unless set)
	roles     map[string]string // name → role file body
}

func loadTestConfig(t *testing.T, tc testConfig) (*config.Effective, string) {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", cfgDir)
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	if tc.json == "" {
		tc.json = `{"model":"fake/m1"}`
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "stavlos.json"), []byte(tc.json), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(tc.roles) > 0 {
		roles := filepath.Join(cfgDir, "agents")
		if err := os.MkdirAll(roles, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range tc.roles {
			if err := os.WriteFile(filepath.Join(roles, name+".md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	work := t.TempDir()
	cfg, err := config.Load(work, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Reminders = tc.reminders
	return cfg, work
}

// newTestChannel starts a channel on a fake host; the root agent is idle.
func newTestChannel(t *testing.T, tc testConfig, fm *fakeModel) (*Channel, *fakeHost) {
	t.Helper()
	cfg, work := loadTestConfig(t, tc)
	h := newFakeHost(fm)
	s := New(h, "s1", work, cfg, "", "")
	if err := s.Start(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s, h
}

// runTurn prompts the root and waits for the turn to end.
func runTurn(t *testing.T, s *Channel, h *fakeHost, prompt string) event.TurnEndedPayload {
	t.Helper()
	root := s.Root()
	if err := root.Prompt(context.Background(), prompt, "human:test"); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(t, event.TurnEnded, root.ID)
	var p event.TurnEndedPayload
	_ = e.Decode(&p)
	return p
}

// finished decodes every tool.finished event of an agent.
func finished(h *fakeHost, agent string) []event.ToolFinishedPayload {
	var out []event.ToolFinishedPayload
	for _, e := range h.ofType(event.ToolFinished, agent) {
		var p event.ToolFinishedPayload
		_ = e.Decode(&p)
		out = append(out, p)
	}
	return out
}

// takenInput is an input a model call took, with the turn that took it.
type takenInput struct {
	event.Input
	Turn int
}

// userMessages lists the inputs an agent's model calls took, in order.
func userMessages(h *fakeHost, agent string) []takenInput { return takenIn(h.all(), agent) }

// takenIn is userMessages over a log (one that spans a restart, say).
func takenIn(evs []event.Event, agent string) []takenInput {
	queued := map[string]event.Input{}
	var out []takenInput
	for _, e := range evs {
		if e.Agent != agent {
			continue
		}
		switch e.Type {
		case event.InputQueued:
			var in event.Input
			_ = e.Decode(&in)
			queued[in.ID] = in
		case event.InputTaken:
			var p event.InputTakenPayload
			_ = e.Decode(&p)
			for _, id := range p.IDs {
				out = append(out, takenInput{queued[id], p.Turn})
			}
		}
	}
	return out
}

// Agent states, for comparisons.
const (
	StateIdle    = protocol.AgentIdle
	StateRunning = protocol.AgentRunning
)

func stateOf(a *Agent) protocol.AgentState { return a.Info().State }

// busy counts the channel's agents in a turn or about to start one.
func busy(s *Channel) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyLocked()
}

func live(s *Channel) int { return s.Info().Live }

// resolve finds an agent the way message recipients are found.
func resolve(s *Channel, ref string) (*Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.resolveLocked(ref)
	if !ok {
		return nil, false
	}
	return s.agents[st.id], true
}

// inputsOf lists the inputs of one kind queued for an agent.
func inputsOf(h *fakeHost, kind event.InputKind, agent string) []event.Input {
	var out []event.Input
	for _, e := range h.ofType(event.InputQueued, agent) {
		var in event.Input
		_ = e.Decode(&in)
		if in.Kind == kind {
			out = append(out, in)
		}
	}
	return out
}

// reminders lists the names each reminder queued for an agent names.
func reminders(h *fakeHost, agent string) [][]string {
	var out [][]string
	for _, in := range inputsOf(h, event.InputReminder, agent) {
		out = append(out, in.Names)
	}
	return out
}
