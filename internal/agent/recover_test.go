package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// snapshot is the part of an agent's view that recovery must reproduce:
// everything except live-only fields (jobs, MCP, context estimates).
type snapshot struct {
	ID, Parent, Archetype, Label, Model, Variant, State string
	Depth, Turn, Queued, Tokens                         int
	CostUSD                                             float64
	LastError                                           string
	Awaiting, Due                                       []string
	Todos                                               []event.TodoItem
	Dirs                                                []protocol.DirInfo
	Children                                            []string
	Inbox                                               []string
	LastPost                                            string
	Nudges                                              int
}

func snap(s *Channel) []snapshot {
	var out []snapshot
	for _, a := range s.Agents() {
		in := a.Info()
		s.mu.Lock()
		st := a.state()
		var inbox []string
		for _, i := range st.inbox {
			inbox = append(inbox, string(i.Kind)+":"+i.Text)
		}
		out = append(out, snapshot{
			ID: in.ID, Parent: in.Parent, Archetype: in.Role, Label: in.Name, Model: in.Model, Variant: in.Variant, State: string(in.State),
			Depth: in.Depth, Turn: in.Turn, Queued: in.Queued, Tokens: in.Tokens, CostUSD: in.CostUSD, LastError: in.LastError,
			Awaiting: in.Awaiting, Due: in.Due, Todos: in.Todos, Dirs: s.dirInfosLocked(), Children: append([]string(nil), st.children...),
			Inbox: inbox, LastPost: st.lastPost, Nudges: st.nudges,
		})
		s.mu.Unlock()
	}
	return out
}

// TestRecoverRoundTrip drives a channel through the paths that mutate
// state, replays its log into a fresh channel, and expects the same view.
// This is the guard for keeping the live path and Recover in step.
func TestRecoverRoundTrip(t *testing.T) {
	other := t.TempDir()
	release := make(chan struct{})
	fm := &fakeModel{
		steps: []step{
			reply(call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look"}`)),
			reply(text("delegated")),
			reply(text("thanks")), // turn 2, woken by the answer
			reply(text("third")),  // turn 3, after the human's follow-up
		},
		childSteps: []step{
			reply(call("k1", "todo", `{"add":["inspect"]}`)),
			reply(call("k2", "todo", `{"update":[{"id":"t1","status":"done"}]}`)),
			func(_ context.Context, req model.Request) (model.Response, error) {
				<-release
				parent := req.System[strings.Index(req.System, "created by a parent agent (id ")+len("created by a parent agent (id "):]
				parent = parent[:strings.IndexByte(parent, ')')]
				return call("k3", "message", `{"to":"`+parent+`","text":"found it","kind":"response"}`), nil
			},
		},
	}
	s, h := newTestChannel(t, testConfig{}, fm)
	ctx := context.Background()
	root := s.Root()
	if err := s.SetMode(ctx, protocol.ModeAuto); err != nil {
		t.Fatal(err)
	}
	runTurn(t, s, h, "delegate")
	child := s.Agents()[1]
	waitUntil(t, h, func() bool {
		return child.Info().Turn == 1 && len(child.Info().Todos) == 1 && child.Info().Todos[0].Status == "done"
	})
	if err := s.AddDir(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := root.SetVariant(ctx, "high"); err != nil {
		t.Fatal(err)
	}
	if err := child.SetModel(ctx, "fake/m2"); err != nil {
		t.Fatal(err)
	}
	close(release)
	h.waitFor(t, event.TurnEnded, root.ID) // turn 2
	runTurn(t, s, h, "follow up")          // turn 3
	waitUntil(t, h, func() bool { return stateOf(child) == StateIdle && busy(s) == 0 })
	// Turn 4: the root messages the child by a prefix of its id and starts a
	// background job that exits; the child answers. Both leave state the
	// replay must reproduce: an expectation keyed on the resolved id (then
	// settled), a job armed then fired.
	fm.steps = []step{
		reply(call("c4", "message", `{"to":"`+child.ID[:6]+`","text":"anything else?"}`)),
		reply(call("c5", "shell", `{"command":"true","background":true}`)),
		reply(text("waiting")),
	}
	fm.childSteps = []step{reply(call("k4", "message", `{"to":"`+root.ID+`","text":"nothing else","kind":"response"}`))}
	_ = s.SetMode(ctx, protocol.ModeYolo)
	runTurn(t, s, h, "ask the child")
	waitUntil(t, h, func() bool {
		return root.Info().Turn >= 5 && stateOf(root) == StateIdle && stateOf(child) == StateIdle && busy(s) == 0 && len(root.Info().Awaiting) == 0 && len(root.Info().Jobs) == 0
	})
	s.Stop()
	before := snap(s)
	mode, modelID := s.Mode(), s.Model()

	h2 := newFakeHost(fm)
	s2, err := Recover(ctx, h2, s.ID, s.Dir, s.Created, s.Config(), h.all())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	after := snap(s2)
	if !reflect.DeepEqual(before, after) {
		bj, _ := json.MarshalIndent(before, "", " ")
		aj, _ := json.MarshalIndent(after, "", " ")
		t.Fatalf("recovered view differs\nlive:\n%s\nrecovered:\n%s\nlog:\n%s", bj, aj, h.dump())
	}
	if s2.Mode() != mode || s2.Model() != modelID || live(s2) != 2 {
		t.Fatalf("channel: mode %s model %s live %d", s2.Mode(), s2.Model(), live(s2))
	}
	if len(h2.all()) != 0 {
		t.Fatalf("recovery of a clean log should log nothing, got\n%s", h2.dump())
	}
}

// TestRecoverAbortsOpenTurns pins the restart path: a turn that was running
// gets TurnAborted, the agent comes back idle, and a job that was running
// is reported lost.
func TestRecoverAbortsOpenTurns(t *testing.T) {
	gate := make(chan struct{})
	fm := &fakeModel{steps: []step{
		reply(call("c1", "shell", `{"command":"sleep 30","background":true}`)),
		func(context.Context, model.Request) (model.Response, error) { <-gate; return text("never"), nil },
	}}
	s, h := newTestChannel(t, testConfig{}, fm)
	_ = s.SetMode(context.Background(), protocol.ModeYolo)
	root := s.Root()
	_ = root.Prompt(context.Background(), "go", "human:test")
	h.waitFor(t, event.ToolFinished, root.ID)
	waitUntil(t, h, func() bool { return len(fm.requests()) == 2 }) // blocked in the second model call
	// The daemon dies here: once the channel is stopped nothing more is
	// logged, so the log is the crash's; then the blocked call may return.
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	waitUntil(t, h, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.stopped })
	evs := h.all()
	close(gate)
	<-stopped

	// The lost job wakes the recovered agent at once: its next turn opens
	// with the loss report.
	fm.steps = []step{func(_ context.Context, req model.Request) (model.Response, error) {
		if !strings.Contains(lastUserText(req), "lost in a daemon restart") {
			return text(""), errors.New("no loss report: " + lastUserText(req))
		}
		return text("ok"), nil
	}}
	h2 := newFakeHost(fm)
	s2, err := Recover(context.Background(), h2, s.ID, s.Dir, s.Created, s.Config(), evs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	r := s2.Root()
	e := h2.waitFor(t, event.TurnEnded, r.ID)
	var end event.TurnEndedPayload
	_ = e.Decode(&end)
	if end.Turn != 2 || end.Reason != "end_turn" {
		t.Fatalf("%+v", end)
	}
	var types []string
	for _, e := range h2.all() {
		types = append(types, string(e.Type))
	}
	if !strings.HasPrefix(strings.Join(types, " "), "turn.aborted job.finished input.queued turn.started") {
		t.Fatalf("recovery logged %v", types)
	}
	if in := r.Info(); in.State != "idle" || len(in.Jobs) != 0 || in.LastError != "" {
		t.Fatalf("%+v", in)
	}
}

// TestRecoverArchivedAndKilled pins that archived channels and killed
// agents come back dead, and a queued prompt restarts a live agent.
func TestRecoverArchivedAndKilled(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "agent_create", `{"archetype":"general","label":"x","task":"t"}`)),
		reply(text("ok")),
	}}
	s, h := newTestChannel(t, testConfig{}, fm)
	runTurn(t, s, h, "go")
	child := s.Agents()[1]
	waitUntil(t, h, func() bool { return busy(s) == 0 })
	if err := s.Kill(child.ID); err != nil {
		t.Fatal(err)
	}
	<-child.ctx.Done()
	s.Stop()
	// A prompt that reached the log as the daemon stopped.
	_, _ = h.Append(context.Background(), event.Event{Channel: s.ID, Agent: s.Root().ID, Type: event.InputQueued,
		Payload: event.MustPayload(event.Input{ID: "late", Kind: event.InputPrompt, Text: "after restart"})})

	fm.steps = []step{reply(text("resumed"))}
	h2 := newFakeHost(fm)
	s2, err := Recover(context.Background(), h2, s.ID, s.Dir, s.Created, s.Config(), h.all())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	e := h2.waitFor(t, event.TurnEnded, s2.Root().ID)
	var end event.TurnEndedPayload
	_ = e.Decode(&end)
	if end.Turn != 2 {
		t.Fatalf("queued prompt did not run: %+v", end)
	}
	if c := s2.Agents()[1]; c.Alive() || live(s2) != 1 {
		t.Fatalf("killed child came back alive")
	}

	if err := s2.Archive(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := append(h.all(), h2.all()...)
	s3, err := Recover(context.Background(), newFakeHost(fm), s.ID, s.Dir, s.Created, s.Config(), all)
	if err != nil {
		t.Fatal(err)
	}
	if !s3.Archived() || live(s3) != 0 {
		t.Fatalf("archived %v live %d", s3.Archived(), live(s3))
	}
}

// TestRecoverMissingRoleIsReadOnly: an agent whose role vanished from the
// configuration comes back read-only under its old name, never as the
// (usually most capable) root role.
func TestRecoverMissingRoleIsReadOnly(t *testing.T) {
	roles := map[string]string{"lead": "---\ndescription: Leads\ntype: primary\nspawn: [general]\n---\nYou lead.\n"}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","rootAgent":"lead"}`, roles: roles}, &fakeModel{steps: []step{reply(text("hi"))}})
	runTurn(t, s, h, "go")
	s.Stop()
	cfg, _ := loadTestConfig(t, testConfig{}) // the lead role is gone
	s2, err := Recover(context.Background(), newFakeHost(&fakeModel{}), s.ID, s.Dir, s.Created, cfg, h.all())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	r := s2.Root()
	in := r.Info()
	if in.Role != "lead" || !strings.Contains(in.LastError, "no longer exists") {
		t.Fatalf("%+v", in)
	}
	if p := r.role().preset; strings.Join(p.Tools, ",") != "read" || len(p.Spawn) != 0 || len(p.MCP) != 0 {
		t.Fatalf("fallback preset %+v", p)
	}
	s2.mu.Lock()
	ok, _ := s2.canSpawnLocked(r.state())
	s2.mu.Unlock()
	if ok {
		t.Fatal("a read-only fallback must not spawn")
	}
}

// TestRecoverKeepsChannelAllows: an allow_prefix and an allow_always granted
// before a restart still answer after it, and a deny rule still wins.
func TestRecoverKeepsChannelAllows(t *testing.T) {
	cfgJSON := `{"model":"fake/m1","policy":{"shell":{"make test --force*":"deny","*":"ask"}}}`
	fm := &fakeModel{steps: []step{
		reply(call("c1", "shell", `{"command":"make test"}`)),
		reply(call("c2", "shell", `{"command":"echo exact"}`)),
		reply(text("ok")),
	}}
	s, h := newTestChannel(t, testConfig{json: cfgJSON}, fm)
	h.answerWith(escalation.Answer{Value: "allow_prefix"}, escalation.Answer{Value: "allow_always"})
	runTurn(t, s, h, "go")
	if n := h.promptCount(); n != 2 {
		t.Fatalf("prompts before restart: %d", n)
	}
	s.Stop()

	fm.steps = []step{
		reply(call("d1", "shell", `{"command":"make test -j4"}`)),
		reply(call("d2", "shell", `{"command":"echo exact"}`)),
		reply(call("d3", "shell", `{"command":"make test --force"}`)),
		reply(call("d4", "shell", `{"command":"make build"}`)),
		reply(text("ok")),
	}
	h2 := newFakeHost(fm)
	h2.answerWith(escalation.Answer{Value: "deny"})
	s2, err := Recover(context.Background(), h2, s.ID, s.Dir, s.Created, s.Config(), h.all())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	runTurn(t, s2, h2, "again")
	if n := h2.promptCount(); n != 1 {
		t.Fatalf("prompts after restart: %d (only make build should ask)", n)
	}
	fin := finished(h2, s2.Root().ID)
	if len(fin) != 4 || fin[0].Denied || fin[1].Denied || !fin[2].Denied || !strings.Contains(fin[2].Output, "Denied by policy") || !fin[3].Denied {
		t.Fatalf("%+v", fin)
	}
}
