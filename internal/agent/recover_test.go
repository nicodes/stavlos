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
// everything except live-only fields (monitors, MCP, context estimates).
type snapshot struct {
	ID, Parent, Archetype, Label, Model, Variant, State string
	Depth, Turn, Queued, Tokens                         int
	CostUSD                                             float64
	LastError                                           string
	Awaiting                                            []string
	Todos                                               []event.TodoItem
	Dirs                                                []protocol.DirInfo
	Children                                            []string
	Armed                                               []string
}

func snap(s *Session) []snapshot {
	var out []snapshot
	for _, a := range s.Agents() {
		in := a.Info()
		out = append(out, snapshot{
			ID: in.ID, Parent: in.Parent, Archetype: in.Archetype, Label: in.Label, Model: in.Model, Variant: in.Variant, State: string(in.State),
			Depth: in.Depth, Turn: in.Turn, Queued: in.Queued, Tokens: in.Tokens, CostUSD: in.CostUSD, LastError: in.LastError,
			Awaiting: in.Awaiting, Todos: in.Todos, Dirs: in.Dirs, Children: a.Children(), Armed: a.armedIDs(),
		})
	}
	return out
}

// TestRecoverRoundTrip drives a session through the paths that mutate
// state, replays its log into a fresh session, and expects the same view.
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
			reply(call("k1", "todo_add", `{"text":"inspect"}`)),
			reply(call("k2", "todo_update", `{"id":"t1","status":"done"}`)),
			func(_ context.Context, req model.Request) (model.Response, error) {
				<-release
				parent := req.System[strings.Index(req.System, "created by a parent agent (id ")+len("created by a parent agent (id "):]
				parent = parent[:strings.IndexByte(parent, ')')]
				return call("k3", "message", `{"to":"`+parent+`","text":"found it"}`), nil
			},
		},
	}
	s, h := newTestSession(t, testConfig{}, fm)
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
	if err := root.AddDir(ctx, other); err != nil {
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
	waitUntil(t, h, func() bool { return child.StateOf() == StateIdle && s.Busy() == 0 })
	// Turn 4: the root messages the child by a prefix of its id and starts a
	// background job that exits; the child answers. Both leave state the
	// replay must reproduce: an expectation keyed on the resolved id (then
	// settled), a job armed then fired.
	fm.steps = []step{
		reply(call("c4", "message", `{"to":"`+child.ID[:6]+`","text":"anything else?"}`)),
		reply(call("c5", "shell", `{"command":"true","background":true}`)),
		reply(text("waiting")),
	}
	fm.childSteps = []step{reply(call("k4", "message", `{"to":"`+root.ID+`","text":"nothing else"}`))}
	_ = s.SetMode(ctx, protocol.ModeYolo)
	runTurn(t, s, h, "ask the child")
	waitUntil(t, h, func() bool {
		return root.Info().Turn >= 5 && root.StateOf() == StateIdle && child.StateOf() == StateIdle && s.Busy() == 0 && len(root.Info().Awaiting) == 0 && len(root.armedIDs()) == 0
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
	if s2.Mode() != mode || s2.Model() != modelID || s2.Live() != 2 {
		t.Fatalf("session: mode %s model %s live %d", s2.Mode(), s2.Model(), s2.Live())
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
	s, h := newTestSession(t, testConfig{}, fm)
	_ = s.SetMode(context.Background(), protocol.ModeYolo)
	root := s.Root()
	_ = root.Prompt(context.Background(), "go", "human:test")
	h.waitFor(t, event.ToolCallFinished, root.ID)
	waitUntil(t, h, func() bool { return len(fm.requests()) == 2 }) // blocked in the second model call
	s.Stop()                                                        // the daemon dies here
	evs := h.all()
	close(gate) // the orphaned turn may still log into h; evs is the log as the crash left it

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
	if !strings.HasPrefix(strings.Join(types, " "), "monitor.fired turn.aborted turn.started") {
		t.Fatalf("recovery logged %v", types)
	}
	if in := r.Info(); in.State != "idle" || len(in.Monitors) != 0 || in.LastError != "" {
		t.Fatalf("%+v", in)
	}
}

// TestRecoverArchivedAndKilled pins that archived sessions and killed
// agents come back dead, and a queued prompt restarts a live agent.
func TestRecoverArchivedAndKilled(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "agent_create", `{"archetype":"general","label":"x","task":"t"}`)),
		reply(text("ok")),
	}}
	s, h := newTestSession(t, testConfig{}, fm)
	runTurn(t, s, h, "go")
	child := s.Agents()[1]
	waitUntil(t, h, func() bool { return s.Busy() == 0 })
	if err := s.Kill(child.ID); err != nil {
		t.Fatal(err)
	}
	<-child.Done()
	s.Stop()
	_ = s.Root().Prompt(context.Background(), "after restart", "human:test")

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
	if c := s2.Agents()[1]; c.Alive() || s2.Live() != 1 {
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
	if !s3.Archived() || s3.Live() != 0 {
		t.Fatalf("archived %v live %d", s3.Archived(), s3.Live())
	}
}

// TestRecoverMissingRoleIsReadOnly: an agent whose role vanished from the
// configuration comes back read-only under its old name, never as the
// (usually most capable) root role.
func TestRecoverMissingRoleIsReadOnly(t *testing.T) {
	roles := map[string]string{"lead": "---\ndescription: Leads\nmode: primary\nspawn: [general]\n---\nYou lead.\n"}
	s, h := newTestSession(t, testConfig{json: `{"model":"fake/m1","rootAgent":"lead"}`, roles: roles}, &fakeModel{steps: []step{reply(text("hi"))}})
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
	if in.Archetype != "lead" || !strings.Contains(in.LastError, "no longer exists") {
		t.Fatalf("%+v", in)
	}
	if p := r.Preset(); strings.Join(p.Tools, ",") != "read" || len(p.Spawn) != 0 || len(p.MCP) != 0 {
		t.Fatalf("fallback preset %+v", p)
	}
	if ok, _ := s2.canSpawn(r); ok {
		t.Fatal("a read-only fallback must not spawn")
	}
}

// TestRecoverKeepsSessionAllows: an allow_prefix and an allow_always granted
// before a restart still answer after it, and a deny rule still wins.
func TestRecoverKeepsSessionAllows(t *testing.T) {
	cfgJSON := `{"model":"fake/m1","policy":{"shell":{"make test --force*":"deny","*":"ask"}}}`
	fm := &fakeModel{steps: []step{
		reply(call("c1", "shell", `{"command":"make test"}`)),
		reply(call("c2", "shell", `{"command":"echo exact"}`)),
		reply(text("ok")),
	}}
	s, h := newTestSession(t, testConfig{json: cfgJSON}, fm)
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
