package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/modelsdev"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

// fakeModel is scripted: each call pops the next response; a script step
// may inspect the request. It blocks on ctx when told to.
type fakeModel struct {
	mu         sync.Mutex
	steps      []func(req model.Request) model.Response // root agent
	childSteps []func(req model.Request) model.Response // subagents (system prompt says so)
	calls      []model.Request
}

func (f *fakeModel) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	q := &f.steps
	if strings.Contains(req.System, "You are a subagent") {
		q = &f.childSteps
	}
	if len(*q) == 0 {
		f.mu.Unlock()
		return model.Response{Blocks: []model.Block{{Type: model.BlockText, Text: "done"}}, StopReason: model.StopEndTurn, Usage: model.Usage{InputTokens: 10, OutputTokens: 2}}, nil
	}
	step := (*q)[0]
	*q = (*q)[1:]
	f.mu.Unlock()
	if onDelta != nil {
		onDelta(model.Delta{Text: "…"})
	}
	if ctx.Err() != nil {
		return model.Response{}, ctx.Err()
	}
	r := step(req)
	r.Usage = model.Usage{InputTokens: 10, OutputTokens: 5}
	return r, nil
}

type fakeProvider struct{ m *fakeModel }

func (p fakeProvider) Name() string                     { return "fake" }
func (p fakeProvider) Open(string) (model.Model, error) { return p.m, nil }
func (p fakeProvider) Variants(string) []string         { return []string{"low", "high"} }

func text(s string) model.Response {
	return model.Response{Blocks: []model.Block{{Type: model.BlockText, Text: s}}, StopReason: model.StopEndTurn}
}
func call(id, name, input string) model.Response {
	return model.Response{Blocks: []model.Block{{Type: model.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}, StopReason: model.StopToolUse}
}

type harness struct {
	t      *testing.T
	d      *Daemon
	c      *rpc.Client
	fm     *fakeModel
	sock   string
	data   string
	cancel context.CancelFunc
	evs    chan event.Event
}

func newHarness(t *testing.T, data string, fm *fakeModel) *harness {
	t.Helper()
	cat, err := modelsdev.Parse([]byte(`{"fake":{"id":"fake","env":[],"models":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cat)
	if err := reg.Register(fakeProvider{fm}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d, err := New(ctx, data, reg)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(data, "s.sock")
	go d.Serve(ctx, sock)
	var c *rpc.Client
	for i := 0; i < 50; i++ {
		if c, err = rpc.Dial(sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Attach(ctx, "test", protocol.TierInteractive); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, d: d, c: c, fm: fm, sock: sock, data: data, cancel: cancel, evs: make(chan event.Event, 1000)}
	go func() {
		for n := range c.Notifications {
			if n.Method == protocol.NEvent {
				var en protocol.EventNotification
				_ = json.Unmarshal(n.Params, &en)
				h.evs <- en.Event
			}
		}
	}()
	return h
}

// recentEvents lists every session event for the dump on timeout.
func (h *harness) recentEvents() []string {
	rows, _ := h.d.Log.Sessions(context.Background())
	var out []string
	for _, r := range rows {
		evs, _ := h.d.Log.Read(context.Background(), r.ID, 1, 0)
		for _, e := range evs {
			out = append(out, fmt.Sprintf("%3d %-22s %s %s", e.Seq, e.Type, e.Agent, short(e.Payload)))
		}
	}
	return out
}

func short(b []byte) string {
	s := string(b)
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}

func (h *harness) close() {
	h.c.Close()
	h.cancel()
	h.d.Close()
}

// waitFor blocks until an event of type t for agent (or any if "") arrives.
func (h *harness) waitFor(t event.Type, agent string) event.Event {
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-h.evs:
			if e.Type == t && (agent == "" || e.Agent == agent) {
				return e
			}
		case <-deadline:
			h.t.Fatalf("timeout waiting for %s (%s)\nevents so far:\n%s", t, agent, strings.Join(h.recentEvents(), "\n"))
		}
	}
}

func setupConfig(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","policy":{"bash":{"echo*":"allow","*":"ask"},"bash_async":{"echo*":"allow"},"write":"allow"}}`), 0o644)
}

func TestEndToEnd(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		// turn 1: allowed bash, then ask-gated bash, then text
		func(model.Request) model.Response { return call("c1", "bash", `{"command":"echo hello"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1]
			if last.Blocks[0].Type != model.BlockToolResult || !strings.Contains(last.Blocks[0].Content, "hello") {
				t.Errorf("tool result not fed back: %+v", last)
			}
			return call("c2", "bash", `{"command":"touch gated"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1]
			if last.Blocks[0].IsError {
				t.Errorf("gated call should have been allowed: %+v", last.Blocks[0])
			}
			return text("turn one done")
		},
		// turn 2: spawn a child, wait for it
		func(model.Request) model.Response {
			return call("c3", "agent_create", `{"archetype":"explorer","label":"scout","task":"look around"}`)
		},
		// parent has nothing else to do; it stops and is woken by the child's result
		func(model.Request) model.Response { return text("delegated; waiting") },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1]
			if !strings.Contains(last.Blocks[len(last.Blocks)-1].Text, "found it") {
				t.Errorf("child result missing: %+v", last)
			}
			return text("child done")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.Messages[0].Blocks[0].Text, "look around") {
				t.Errorf("child task missing: %+v", req.Messages[0])
			}
			return call("k1", "agent_finish", `{"summary":"found it","status":"success"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()

	s, err := h.c.CreateSession(ctx, work, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.c.Subscribe(ctx, s.ID, 0); err != nil {
		t.Fatal(err)
	}
	agents, err := h.c.Tree(ctx, s.ID)
	if err != nil || len(agents) != 1 || agents[0].Archetype != "coder" || agents[0].Model != "fake/m1" {
		t.Fatalf("tree %v %v", agents, err)
	}
	root := agents[0].ID

	// permission prompt round trip
	// Prompts are fetched via prompt.list; the harness reader only keeps events.
	if err := h.c.Send(ctx, root, protocol.KindPrompt, "go"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.ToolCallFinished, root) // echo hello (allowed)
	h.waitFor(event.PromptRequested, root)
	ps, err := h.c.Prompts(ctx, s.ID)
	if err != nil || len(ps) != 1 || ps[0].Tool != "bash" {
		t.Fatalf("prompts %v %v", ps, err)
	}
	if err := h.c.ClaimPrompt(ctx, ps[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := h.c.ReplyPrompt(ctx, ps[0].ID, "allow"); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.TurnEnded, root)
	var te event.TurnEndedPayload
	_ = e.Decode(&te)
	if te.Reason != "end_turn" {
		t.Fatalf("turn 1 ended %+v", te)
	}
	if _, err := os.Stat(filepath.Join(work, "gated")); err != nil {
		t.Fatal("gated command did not run")
	}

	// turn 2: spawn + wait
	if err := h.c.Send(ctx, root, protocol.KindPrompt, "delegate"); err != nil {
		t.Fatal(err)
	}
	sp := h.waitFor(event.AgentSpawned, "")
	var spp event.AgentSpawnedPayload
	_ = sp.Decode(&spp)
	if spp.Parent != root || spp.Label != "scout" || spp.Model != "fake/m1" || spp.Depth != 1 {
		t.Fatalf("spawned %+v", spp)
	}
	h.waitFor(event.AgentFinished, spp.ID)
	// turn 2 may end before or after the child finishes; wait for turn 3,
	// the one started by the ChildFinished envelope.
	for te.Turn != 3 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if te.Reason != "end_turn" {
		t.Fatalf("turn 3 ended %+v", te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents) != 2 || agents[1].State != "finished" || agents[1].Summary != "found it" || agents[0].CostUSD != 0 {
		t.Fatalf("tree after: %+v", agents)
	}
	// usage was logged
	evs, _ := h.d.Log.Read(ctx, s.ID, 1, 0)
	nUsage := 0
	for _, e := range evs {
		if e.Type == event.Usage {
			nUsage++
		}
	}
	if nUsage != 7 { // 3 (turn 1) + 1 (spawn) + 1 (stop) + 1 (child) + 1 (turn 3)
		t.Fatalf("usage events %d", nUsage)
	}
	// reconcile
	rc, err := h.c.Reconcile(ctx, s.ID)
	if err != nil || rc.Seq != evs[len(evs)-1].Seq || len(rc.Agents) != 2 {
		t.Fatalf("reconcile %+v %v", rc, err)
	}
}

func TestCancelMidToolAndRecover(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	data := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c1", "bash", `{"command":"echo start; sleep 20"}`) },
	}
	h := newHarness(t, data, fm)
	ctx := context.Background()
	s, err := h.c.CreateSession(ctx, work, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	h.waitFor(event.ToolCallStarted, root)
	time.Sleep(300 * time.Millisecond)
	_ = h.c.Send(ctx, root, protocol.KindCancel, "")
	e := h.waitFor(event.ToolCallFinished, root)
	var tf event.ToolFinishedPayload
	_ = e.Decode(&tf)
	if !tf.Cancelled || !strings.Contains(tf.Output, "start") {
		t.Fatalf("cancelled call %+v", tf)
	}
	e = h.waitFor(event.TurnEnded, root)
	var te event.TurnEndedPayload
	_ = e.Decode(&te)
	if te.Reason != "cancelled" {
		t.Fatalf("%+v", te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].State != "idle" {
		t.Fatalf("state after cancel %s", agents[0].State)
	}

	// Steer while idle behaves as a prompt; the projector must have repaired
	// the cancelled call. Simulate a crash mid-turn: block the model, then
	// stop the daemon without ending the turn.
	block := make(chan struct{})
	fm.mu.Lock()
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			// history must be well-formed: tool_result for c1 present and cancelled
			found := false
			for _, m := range req.Messages {
				for _, b := range m.Blocks {
					if b.Type == model.BlockToolResult && b.ToolUseID == "c1" && b.Cancelled {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("cancelled tool_result not synthesized: %+v", req.Messages)
			}
			<-block
			return text("never")
		},
	}
	fm.mu.Unlock()
	_ = h.c.Send(ctx, root, protocol.KindSteer, "steer while idle")
	h.waitFor(event.TurnStarted, root)
	time.Sleep(200 * time.Millisecond)
	h.close()
	close(block)

	// restart on the same data dir
	fm2 := &fakeModel{}
	h2 := newHarness(t, data, fm2)
	defer h2.close()
	list, err := h2.c.Sessions(ctx, work, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions after restart: %v %v", list, err)
	}
	evs, _ := h2.d.Log.Read(ctx, s.ID, 1, 0)
	last := evs[len(evs)-1]
	if last.Type != event.TurnAborted {
		t.Fatalf("last event after restart: %s", last.Type)
	}
	agents, err = h2.c.Tree(ctx, s.ID)
	if err != nil || agents[0].State != "idle" || agents[0].Turn != 2 {
		t.Fatalf("recovered tree %+v %v", agents, err)
	}
	// and it can still run a turn with coherent history
	fm2.mu.Lock()
	fm2.steps = []func(model.Request) model.Response{func(req model.Request) model.Response {
		if len(req.Messages) < 3 {
			t.Errorf("history lost: %d messages", len(req.Messages))
		}
		return text("alive")
	}}
	fm2.mu.Unlock()
	_ = h2.c.Subscribe(ctx, s.ID, last.Seq+1) // live only; no replay of the old turns
	_ = h2.c.Send(ctx, root, protocol.KindPrompt, "still there?")
	e = h2.waitFor(event.TurnEnded, root)
	_ = e.Decode(&te)
	if te.Reason != "end_turn" {
		t.Fatalf("%+v", te)
	}
	// fork from the current offset yields a new session with the same tree
	f, err := h2.c.ForkSession(ctx, s.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	fa, err := h2.c.Tree(ctx, f.ID)
	if err != nil || len(fa) != 1 || fa[0].ID == root || fa[0].Turn != 3 {
		t.Fatalf("fork tree %+v %v", fa, err)
	}
}

func TestSpawnArmsWakeByDefault(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	release := make(chan struct{})
	fm.steps = []func(model.Request) model.Response{
		// turn 1: spawn and stop, without calling monitor
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"explorer","label":"slow","task":"a"}`)
		},
		func(model.Request) model.Response { return text("spawned, done for now") },
		// turn 2: woken by the child's finish
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks
			if !strings.Contains(last[len(last)-1].Text, "finished with status success") {
				t.Errorf("wake input: %+v", last)
			}
			return text("thanks")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			<-release
			return call("k", "agent_finish", `{"summary":"late","status":"success"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "delegate")
	h.waitFor(event.MonitorArmed, root)
	e := h.waitFor(event.TurnEnded, root)
	var te event.TurnEndedPayload
	_ = e.Decode(&te)
	if te.Turn != 1 {
		t.Fatalf("%+v", te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents) != 2 {
		t.Fatalf("child should be armed by spawn: %+v", agents)
	}
	close(release)
	e = h.waitFor(event.TurnEnded, root)
	_ = e.Decode(&te)
	if te.Turn != 2 || te.Reason != "end_turn" {
		t.Fatalf("parent was not woken: %+v", te)
	}
}

func TestBashAsyncWakes(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "bash_async", `{"command":"echo one; sleep 0.3; echo two; exit 3"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !strings.Contains(last.Content, "started job m") {
				t.Errorf("bash_async result: %+v", last)
			}
			return text("started, carrying on")
		},
		// woken by the monitor
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks
			txt := last[len(last)-1].Text
			if !strings.Contains(txt, "exited 3") || !strings.Contains(txt, "one\ntwo") {
				t.Errorf("monitor wake text: %q", txt)
			}
			return text("noted")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "run it")
	e := h.waitFor(event.MonitorStarted, root)
	var ms event.MonitorStartedPayload
	_ = e.Decode(&ms)
	if ms.Kind != "command" {
		t.Fatalf("%+v", ms)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents[0].Monitors) != 1 || agents[0].Monitors[0].Kind != "command" {
		t.Fatalf("monitors in tree: %+v", agents[0].Monitors)
	}
	e = h.waitFor(event.MonitorFired, root)
	var mf event.MonitorFiredPayload
	_ = e.Decode(&mf)
	if mf.ExitCode != 3 || !mf.IsError || !strings.Contains(mf.Output, "two") {
		t.Fatalf("%+v", mf)
	}
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if te.Reason != "end_turn" {
		t.Fatalf("%+v", te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents[0].Monitors) != 0 {
		t.Fatalf("monitor should be gone: %+v", agents[0].Monitors)
	}
}

func TestBashKillStopsJob(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "bash_async", `{"command":"echo start; sleep 30"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			id := strings.TrimSpace(strings.TrimPrefix(strings.Split(last.Content, ";")[0], "started job "))
			return call("c2", "bash_async_kill", `{"id":"`+id+`"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !strings.Contains(last.Content, "stopped job m") {
				t.Errorf("bash_async_kill result: %+v", last)
			}
			return text("killed it")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	start := time.Now()
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	h.waitFor(event.MonitorStopped, root)
	h.waitFor(event.TurnEnded, root)
	if time.Since(start) > 5*time.Second {
		t.Fatal("stop did not kill the command promptly")
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents[0].Monitors) != 0 {
		t.Fatalf("%+v", agents[0].Monitors)
	}
}

func TestSetRoleSwitchesPresetInPlace(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "senior software engineer") {
				t.Errorf("turn 1 should use the coder preset: %.80q", req.System)
			}
			return text("hi from coder")
		},
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "read-only code explorer") {
				t.Errorf("turn 2 should use the explorer preset: %.80q", req.System)
			}
			for _, d := range req.Tools {
				if d.Name == "apply_patch" || d.Name == "agent_create" {
					t.Errorf("explorer should not have %s", d.Name)
				}
			}
			return text("hi from explorer")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "one")
	h.waitFor(event.TurnEnded, root)
	if err := h.c.SetAgentRole(ctx, root, "nope"); err == nil {
		t.Fatal("unknown role accepted")
	}
	if err := h.c.SetAgentRole(ctx, root, "explorer"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.AgentRoleChanged, root)
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].Archetype != "explorer" || agents[0].Label != "main" { // the root keeps its "main" label
		t.Fatalf("tree after role change: %+v", agents[0])
	}
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "two")
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if te.Reason != "end_turn" {
		t.Fatalf("%+v", te)
	}
}

// TestAgentsMessageAcrossTheSession: a child prompts its parent (not a
// child of the caller), the parent sees who sent it, agent_status shows
// the whole tree, and steer/lifecycle tools are not a subagent's to use.
func TestAgentsMessageAcrossTheSession(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	var rootID string
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"explorer","label":"scout","task":"ask me something"}`)
		},
		func(model.Request) model.Response { return text("delegated") },
		// woken by the child's prompt: the model sees the sender
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0].Text
			if !strings.HasPrefix(last, "[message from agent scout (") || !strings.Contains(last, "which branch?") {
				t.Errorf("parent saw: %q", last)
			}
			return text("main")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			// the system prompt names the parent; message it
			i := strings.Index(req.System, "parent agent (id ")
			if i < 0 {
				t.Errorf("child system prompt lacks the parent id: %q", req.System)
				return text("no parent")
			}
			pid := req.System[i+len("parent agent (id "):]
			pid = pid[:strings.Index(pid, ")")]
			if pid != rootID {
				t.Errorf("parent id %q, want %q", pid, rootID)
			}
			return call("k1", "agent_prompt", `{"id":"`+pid+`","text":"which branch?"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "queued") {
				t.Errorf("agent_prompt to the parent: %+v", last)
			}
			// a subagent is not offered steer, kill, cancel or result at all
			for _, d := range req.Tools {
				switch d.Name {
				case "agent_steer", "agent_kill", "agent_cancel", "agent_result":
					t.Errorf("subagent should not be offered %s", d.Name)
				}
			}
			return call("k2", "agent_steer", `{"id":"`+rootID+`","text":"stop"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError {
				t.Errorf("agent_steer from a subagent should be refused: %+v", last)
			}
			return call("k3", "agent_status", `{}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, `"label":"main"`) || !strings.Contains(last.Content, `"you":true`) || !strings.Contains(last.Content, `"parent":"`+rootID+`"`) {
				t.Errorf("agent_status should list the whole tree with the caller marked: %+v", last)
			}
			return call("k4", "agent_finish", `{"summary":"asked","status":"success"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	rootID = agents[0].ID
	_ = h.c.Send(ctx, rootID, protocol.KindPrompt, "delegate")

	// the parent's logged message carries the sender
	var um event.UserMessagePayload
	for um.From == "" {
		e := h.waitFor(event.UserMessage, rootID)
		_ = e.Decode(&um)
	}
	if !strings.HasPrefix(um.From, "scout (") || um.Text != "which branch?" || um.Kind != "prompt" {
		t.Fatalf("parent's message: %+v", um)
	}
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e := h.waitFor(event.TurnEnded, rootID)
		_ = e.Decode(&te)
	}
	if te.Reason != "end_turn" {
		t.Fatalf("turn 2: %+v", te)
	}
}

func TestVariants(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	var seen []string
	var mu sync.Mutex
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			mu.Lock()
			seen = append(seen, req.Variant)
			mu.Unlock()
			return call("c1", "agent_create", `{"archetype":"explorer","label":"scout","task":"look"}`)
		},
		func(model.Request) model.Response { return text("delegated") },
		func(model.Request) model.Response { return text("noted") },
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			mu.Lock()
			seen = append(seen, "child:"+req.Variant)
			mu.Unlock()
			return call("k1", "agent_finish", `{"summary":"ok","status":"success"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID

	if vs, err := h.c.Variants(ctx, "fake/m1"); err != nil || len(vs) != 2 || vs[1] != "high" {
		t.Fatalf("variants: %v %v", vs, err)
	}
	if err := h.c.SetAgentVariant(ctx, root, "extreme"); err == nil {
		t.Fatal("unknown variant should be rejected")
	}
	if err := h.c.SetAgentVariant(ctx, root, "high"); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.AgentVariantChanged, root)
	var vp event.VariantChangedPayload
	_ = e.Decode(&vp)
	if vp.Variant != "high" {
		t.Fatalf("%+v", vp)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].Variant != "high" {
		t.Fatalf("tree variant: %+v", agents[0])
	}

	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	sp := h.waitFor(event.AgentSpawned, "")
	var spp event.AgentSpawnedPayload
	_ = sp.Decode(&spp)
	h.waitFor(event.AgentFinished, spp.ID)
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "high" || seen[1] != "child:high" {
		t.Fatalf("variants seen by the model: %v", seen)
	}
	// the child (same model) inherited the variant and it shows in the tree
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents) != 2 || agents[1].Variant != "high" {
		t.Fatalf("child variant: %+v", agents)
	}
	// back to the default
	if err := h.c.SetAgentVariant(ctx, root, ""); err != nil {
		t.Fatal(err)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].Variant != "" {
		t.Fatalf("reset: %+v", agents[0])
	}
}

// TestYolo: with the session in yolo, ask-gated calls run without a prompt,
// a prompt already waiting is approved when yolo turns on, and a deny rule
// still denies.
func TestYolo(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","policy":{"bash":{"echo*":"allow","rm*":"deny","*":"ask"}}}`), 0o644)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		// turn 1: an ask-gated command waits for a prompt
		func(model.Request) model.Response { return call("c1", "bash", `{"command":"touch first"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError {
				t.Errorf("first call should have been allowed by yolo: %+v", last)
			}
			return text("one")
		},
		// turn 2: yolo is on, no prompt; a denied command stays denied
		func(model.Request) model.Response { return call("c2", "bash", `{"command":"touch second"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError {
				t.Errorf("second call should run without a prompt: %+v", last)
			}
			return call("c3", "bash", `{"command":"rm -rf nothing"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || !strings.Contains(last.Content, "Denied by policy") {
				t.Errorf("deny rule should still deny under yolo: %+v", last)
			}
			return text("two")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID

	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	h.waitFor(event.PromptRequested, root)
	if ps, _ := h.c.Prompts(ctx, s.ID); len(ps) != 1 {
		t.Fatalf("one prompt should be waiting: %+v", ps)
	}
	// yolo on: the waiting prompt is approved and logged
	if err := h.c.SetSessionYolo(ctx, s.ID, true); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.SessionYoloChanged, "")
	var yp event.YoloPayload
	_ = e.Decode(&yp)
	if !yp.On {
		t.Fatalf("%+v", yp)
	}
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if _, err := os.Stat(filepath.Join(work, "first")); err != nil {
		t.Fatal("the waiting command should have run once yolo turned on")
	}
	if ps, _ := h.c.Prompts(ctx, s.ID); len(ps) != 0 {
		t.Fatalf("prompt queue should be drained: %+v", ps)
	}
	rc, _ := h.c.Reconcile(ctx, s.ID)
	if !rc.Session.Yolo {
		t.Fatalf("session info should show yolo: %+v", rc.Session)
	}

	// turn 2 runs with no prompt at all
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "again")
	for te.Turn != 2 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if _, err := os.Stat(filepath.Join(work, "second")); err != nil {
		t.Fatal("second command should have run")
	}
	evs, _ := h.d.Log.Read(ctx, s.ID, 1, 0)
	for _, ev := range evs {
		if ev.Type == event.PromptRequested {
			var p event.PromptRequestedPayload
			_ = ev.Decode(&p)
			if p.Tool == "bash" && strings.Contains(string(p.Input), "second") {
				t.Fatal("no prompt should be raised in yolo")
			}
		}
	}
	// off again
	if err := h.c.SetSessionYolo(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	rc, _ = h.c.Reconcile(ctx, s.ID)
	if rc.Session.Yolo {
		t.Fatalf("yolo should be off: %+v", rc.Session)
	}
}
