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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nicodes/stavlos/internal/agent"
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
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","policy":{"shell":{"echo*":"allow","*":"ask"},"write":"allow"}}`), 0o644)
}

func TestEndToEnd(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		// turn 1: an allowed command, then an ask-gated one, then text
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"echo hello"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1]
			if last.Blocks[0].Type != model.BlockToolResult || !strings.Contains(last.Blocks[0].Content, "hello") {
				t.Errorf("tool result not fed back: %+v", last)
			}
			return call("c2", "shell", `{"command":"touch gated"}`)
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
			return call("c3", "agent_create", `{"archetype":"general","label":"scout","task":"look around"}`)
		},
		// parent has nothing else to do; it stops and is woken by the child's answer
		func(model.Request) model.Response { return text("delegated; waiting") },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1]
			txt := last.Blocks[len(last.Blocks)-1].Text
			if !strings.Contains(txt, "found it") || !strings.Contains(txt, "scout") {
				t.Errorf("child answer missing or unattributed: %+v", last)
			}
			return text("child done")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.Messages[0].Blocks[0].Text, "look around") {
				t.Errorf("child task missing: %+v", req.Messages[0])
			}
			return call("k1", "agent_response", `{"to":"`+parentIDFromSystem(req.System)+`","text":"found it"}`)
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
	if err != nil || len(agents) != 1 || agents[0].Archetype != "general" || agents[0].Model != "fake/m1" {
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
	if err != nil || len(ps) != 1 || ps[0].Tool != "shell" {
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
	h.waitFor(event.ResponseReceived, root)
	// turn 2 may end before or after the child answers; wait for turn 3,
	// the one started by the response.
	for te.Turn != 3 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if te.Reason != "end_turn" {
		t.Fatalf("turn 3 ended %+v", te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents) == 2 && agents[1].State != "idle" {
		h.waitFor(event.TurnEnded, agents[1].ID) // the child's own turn may still be closing
		agents, _ = h.c.Tree(ctx, s.ID)
	}
	if len(agents) != 2 || agents[1].State != "idle" || agents[0].CostUSD != 0 {
		t.Fatalf("tree after (the child stays alive, idle): %+v", agents)
	}
	// usage was logged
	evs, _ := h.d.Log.Read(ctx, s.ID, 1, 0)
	nUsage := 0
	for _, e := range evs {
		if e.Type == event.Usage {
			nUsage++
		}
	}
	if nUsage != 8 { // 3 (turn 1) + 1 (spawn) + 1 (stop) + 2 (child: answer, then its turn ends) + 1 (turn 3)
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
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"echo start; sleep 20"}`) },
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

func TestChildResponseWakesParent(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	release := make(chan struct{})
	fm.steps = []func(model.Request) model.Response{
		// turn 1: spawn and stop
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"general","label":"slow","task":"a"}`)
		},
		func(model.Request) model.Response { return text("spawned, done for now") },
		// turn 2: woken by the child's answer
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks
			if !strings.Contains(last[len(last)-1].Text, "late") || !strings.Contains(last[len(last)-1].Text, "slow") {
				t.Errorf("wake input: %+v", last)
			}
			return text("thanks")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			<-release
			return call("k", "agent_response", `{"to":"`+parentIDFromSystem(req.System)+`","text":"late"}`)
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
	e := h.waitFor(event.TurnEnded, root)
	var te event.TurnEndedPayload
	_ = e.Decode(&te)
	if te.Turn != 1 {
		t.Fatalf("%+v", te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents) != 2 || agents[0].State != "waiting" || len(agents[0].Awaiting) != 1 || agents[0].Awaiting[0] != agents[1].ID {
		t.Fatalf("parent idle with a question out should read waiting on the child: %+v", agents)
	}
	close(release)
	e = h.waitFor(event.TurnEnded, root)
	_ = e.Decode(&te)
	if te.Turn != 2 || te.Reason != "end_turn" {
		t.Fatalf("parent was not woken by the response: %+v", te)
	}
	// the child is still there, idle, ready for a follow-up; the parent is
	// plainly idle again now that the answer landed
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents) != 2 || agents[1].State != "idle" || agents[0].State != "idle" {
		t.Fatalf("after answering: %+v", agents)
	}
}

func TestShellBackgroundWakes(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "shell", `{"command":"echo one; sleep 0.3; echo two; exit 3","background":true}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !strings.Contains(last.Content, "started job m") {
				t.Errorf("background shell result: %+v", last)
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

// TestShellOutlivesWaitBecomesJob: a command still running when the wait
// window closes continues as a job; the call returns the id and the output
// so far, the async tab shows the job, and its exit wakes the agent.
func TestShellOutlivesWaitBecomesJob(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "shell", `{"command":"echo early; sleep 2; echo late; exit 2","wait":1}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "still running after 1s; continuing as job m") || !strings.Contains(last.Content, "output so far:\nearly") {
				t.Errorf("handover result: %+v", last)
			}
			return text("carrying on")
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks
			txt := last[len(last)-1].Text
			if !strings.Contains(txt, "exited 2") || !strings.Contains(txt, "early\nlate") {
				t.Errorf("job wake text: %q", txt)
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
	h.waitFor(event.MonitorStarted, root)
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents[0].Monitors) != 1 || agents[0].Monitors[0].Label != "echo early; sleep 2; echo late; exit 2" {
		t.Fatalf("monitors in tree: %+v", agents[0].Monitors)
	}
	e := h.waitFor(event.MonitorFired, root)
	var mf event.MonitorFiredPayload
	_ = e.Decode(&mf)
	if mf.ExitCode != 2 || !mf.IsError || mf.Output != "early\nlate\n" {
		t.Fatalf("%+v", mf)
	}
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	// a quick command stays inline: no job, one turn
	fm2 := &fakeModel{}
	fm2.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"echo quick"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || last.Content != "quick\n" {
				t.Errorf("inline result: %+v", last)
			}
			return text("done")
		},
	}
	h2 := newHarness(t, t.TempDir(), fm2)
	defer h2.close()
	s2, _ := h2.c.CreateSession(ctx, work, "", "")
	_ = h2.c.Subscribe(ctx, s2.ID, 0)
	agents, _ = h2.c.Tree(ctx, s2.ID)
	_ = h2.c.Send(ctx, agents[0].ID, protocol.KindPrompt, "run it")
	h2.waitFor(event.TurnEnded, agents[0].ID)
	evs, _ := h2.d.Log.Read(ctx, s2.ID, 1, 0)
	for _, e := range evs {
		if e.Type == event.MonitorStarted {
			t.Fatal("a quick command must not become a job")
		}
	}
}

func TestShellKillStopsJob(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "shell", `{"command":"echo start; sleep 30","background":true}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			id := strings.TrimSpace(strings.TrimPrefix(strings.Split(last.Content, ";")[0], "started job "))
			return call("c2", "shell_kill", `{"id":"`+id+`"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !strings.Contains(last.Content, "stopped job m") {
				t.Errorf("shell_kill result: %+v", last)
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
	// Only "general" ships built in; a user preset comes from agents/<name>.md.
	agentsDir := filepath.Join(os.Getenv("STAVLOS_CONFIG_DIR"), "roles")
	_ = os.MkdirAll(agentsDir, 0o755)
	os.WriteFile(filepath.Join(agentsDir, "explorer.md"), []byte("---\ndescription: Read-only investigation\ntools: [read, shell]\n---\nYou are a read-only code explorer. Do not modify anything.\n"), 0o644)
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

// TestAgentsMessageAcrossTheSession: a child messages its parent (not a
// child of the caller), the parent sees who sent it, agent_status shows
// the whole tree, and lifecycle tools are not a subagent's to use.
func TestAgentsMessageAcrossTheSession(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	var rootID string
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"ask me something"}`)
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
			return call("k1", "agent_message", `{"id":"`+pid+`","text":"which branch?"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "delivered") {
				t.Errorf("agent_message to the parent: %+v", last)
			}
			// one messaging tool for everyone: the old prompt/steer pair is gone
			for _, d := range req.Tools {
				if d.Name == "agent_steer" || d.Name == "agent_prompt" {
					t.Errorf("subagent should not be offered %s", d.Name)
				}
			}
			return call("k3", "agent_status", `{}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			flat := strings.Join(strings.Fields(last.Content), "") // the tool pretty-prints its JSON
			if last.IsError || !strings.Contains(flat, `"label":"main"`) || !strings.Contains(flat, `"you":true`) || !strings.Contains(flat, `"parent":"`+rootID+`"`) {
				t.Errorf("agent_status should list the whole tree with the caller marked: %+v", last)
			}
			return call("k4", "agent_response", `{"to":"`+rootID+`","text":"asked"}`)
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
	h.waitFor(event.ResponseReceived, rootID) // the child's agent_response, after its status check
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
			return call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look"}`)
		},
		func(model.Request) model.Response { return text("delegated") },
		func(model.Request) model.Response { return text("noted") },
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			mu.Lock()
			seen = append(seen, "child:"+req.Variant)
			mu.Unlock()
			return call("k1", "agent_response", `{"to":"`+parentIDFromSystem(req.System)+`","text":"ok"}`)
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
	h.waitFor(event.ResponseReceived, root)
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
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","policy":{"shell":{"echo*":"allow","rm*":"deny","*":"ask"}}}`), 0o644)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		// turn 1: an ask-gated command waits for a prompt
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"touch first"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError {
				t.Errorf("first call should have been allowed by yolo: %+v", last)
			}
			return text("one")
		},
		// turn 2: yolo is on, no prompt; a denied command stays denied
		func(model.Request) model.Response { return call("c2", "shell", `{"command":"touch second"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError {
				t.Errorf("second call should run without a prompt: %+v", last)
			}
			return call("c3", "shell", `{"command":"rm -rf nothing"}`)
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
	if err := h.c.SetSessionMode(ctx, s.ID, "yolo"); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.SessionModeChanged, "")
	var mp event.ModePayload
	_ = e.Decode(&mp)
	if mp.Mode != "yolo" {
		t.Fatalf("%+v", mp)
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
	if rc.Session.Mode != "yolo" {
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
			if p.Tool == "shell" && strings.Contains(string(p.Input), "second") {
				t.Fatal("no prompt should be raised in yolo")
			}
		}
	}
	// back to ask
	if err := h.c.SetSessionMode(ctx, s.ID, "ask"); err != nil {
		t.Fatal(err)
	}
	rc, _ = h.c.Reconcile(ctx, s.ID)
	if rc.Session.Mode != "ask" {
		t.Fatalf("mode should be ask: %+v", rc.Session)
	}
	if err := h.c.SetSessionMode(ctx, s.ID, "turbo"); err == nil {
		t.Fatal("an unknown mode should be rejected")
	}
}

// TestAutoMode: in auto, policy asks inside the agent's directories are
// allowed without a prompt, a call outside still asks (and switching to
// auto does not approve a waiting boundary prompt), deny still denies.
func TestAutoMode(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","policy":{"shell":{"rm*":"deny","*":"ask"}}}`), 0o644)
	work := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "f.txt"), []byte("x"), 0o644)
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		// turn 1: an inside command (ask by policy) and an outside read wait
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"touch inside"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError {
				t.Errorf("inside command should be allowed once auto is on: %+v", last)
			}
			return call("c2", "read", `{"path":"`+outside+`/f.txt"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || !strings.Contains(last.Content, "denied") {
				t.Errorf("the boundary prompt was denied by hand: %+v", last)
			}
			return call("c3", "shell", `{"command":"rm -rf nothing"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || !strings.Contains(last.Content, "Denied by policy") {
				t.Errorf("deny should hold under auto: %+v", last)
			}
			return text("one")
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
	h.waitFor(event.PromptRequested, root) // the inside command waits in ask mode
	if err := h.c.SetSessionMode(ctx, s.ID, "auto"); err != nil {
		t.Fatal(err)
	}
	// auto approves the waiting inside command; the outside read then raises
	// a boundary prompt that auto does not approve
	e := h.waitFor(event.PromptRequested, root)
	var pr event.PromptRequestedPayload
	_ = e.Decode(&pr)
	if pr.Tool != "read" || !strings.Contains(pr.Question, "outside its directories") {
		t.Fatalf("expected a boundary prompt: %+v", pr)
	}
	pending := h.d.esc.Pending(s.ID)
	if len(pending) != 1 || pending[0].Dir == "" {
		t.Fatalf("pending %+v", pending)
	}
	// switching to auto again (already auto) or asking for auto must not approve it
	_ = h.c.SetSessionMode(ctx, s.ID, "auto")
	if len(h.d.esc.Pending(s.ID)) != 1 {
		t.Fatal("auto must not approve a boundary prompt")
	}
	_ = h.c.ClaimPrompt(ctx, pending[0].ID)
	if err := h.c.ReplyPrompt(ctx, pending[0].ID, "deny"); err != nil {
		t.Fatal(err)
	}
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if _, err := os.Stat(filepath.Join(work, "inside")); err != nil {
		t.Fatal("the inside command should have run under auto")
	}
	rc, _ := h.c.Reconcile(ctx, s.ID)
	if rc.Session.Mode != "auto" {
		t.Fatalf("%+v", rc.Session)
	}
}

func TestSessionListTitles(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{func(model.Request) model.Response { return text("hi") }}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	list, err := h.c.Sessions(ctx, work, false)
	if err != nil || len(list) != 1 || list[0].Title != "" {
		t.Fatalf("fresh session should have no title: %+v %v", list, err)
	}
	agents, _ := h.c.Tree(ctx, s.ID)
	_ = h.c.Send(ctx, agents[0].ID, protocol.KindPrompt, "fix the login bug\nand add tests")
	h.waitFor(event.TurnEnded, agents[0].ID)
	list, _ = h.c.Sessions(ctx, work, false)
	if len(list) != 1 || list[0].Title != "fix the login bug" {
		t.Fatalf("title should be the first prompt's first line: %+v", list)
	}
	// a second session in the same directory lists first (newest)
	s2, _ := h.c.CreateSession(ctx, work, "", "")
	list, _ = h.c.Sessions(ctx, work, false)
	if len(list) != 2 || list[0].ID != s2.ID || list[1].Title != "fix the login bug" {
		t.Fatalf("newest first with titles: %+v", list)
	}
}

// TestRecoveredAgentWithMissingPresetFallsBack: a session whose root was
// created under a preset that no longer exists resumes as the configured
// root preset, with its full tool set.
func TestRecoveredAgentWithMissingPresetFallsBack(t *testing.T) {
	setupConfig(t)
	agentsDir := filepath.Join(os.Getenv("STAVLOS_CONFIG_DIR"), "roles")
	_ = os.MkdirAll(agentsDir, 0o755)
	presetFile := filepath.Join(agentsDir, "coder.md")
	os.WriteFile(presetFile, []byte("---\ndescription: Old coder\ntools: [read, shell]\n---\nYou are the old coder.\n"), 0o644)
	work := t.TempDir()
	data := t.TempDir()
	fm := &fakeModel{}
	h := newHarness(t, data, fm)
	ctx := context.Background()
	s, err := h.c.CreateSession(ctx, work, "", "coder")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := h.c.Tree(ctx, s.ID)
	if agents[0].Archetype != "coder" {
		t.Fatalf("root should start as coder: %+v", agents[0])
	}
	h.close()
	os.Remove(presetFile) // the preset disappears (as coder did when general replaced it)

	fm2 := &fakeModel{}
	var offered []string
	fm2.steps = []func(model.Request) model.Response{func(req model.Request) model.Response {
		for _, d := range req.Tools {
			offered = append(offered, d.Name)
		}
		if !strings.Contains(req.System, "senior software engineer") {
			t.Errorf("system prompt should be the general preset's: %.80q", req.System)
		}
		return text("ok")
	}}
	h2 := newHarness(t, data, fm2)
	defer h2.close()
	if _, err := h2.c.ResumeSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	agents, _ = h2.c.Tree(ctx, s.ID)
	if agents[0].Archetype != "general" {
		t.Fatalf("root should fall back to general: %+v", agents[0])
	}
	_ = h2.c.Subscribe(ctx, s.ID, 0)
	_ = h2.c.Send(ctx, agents[0].ID, protocol.KindPrompt, "hello")
	h2.waitFor(event.TurnEnded, agents[0].ID)
	if !strings.Contains(strings.Join(offered, " "), "agent_create") || !strings.Contains(strings.Join(offered, " "), "apply_patch") {
		t.Fatalf("the fallback should carry general's tools, got %v", offered)
	}
}

// parentIDFromSystem reads "parent agent (id X)" out of a child's system prompt.
func parentIDFromSystem(system string) string {
	i := strings.Index(system, "parent agent (id ")
	if i < 0 {
		return ""
	}
	rest := system[i+len("parent agent (id "):]
	if j := strings.IndexByte(rest, ')'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// TestOneAnswerSettlesRepeatedPrompts: a parent that prompts a child twice
// and gets one answer is not left "waiting" for a second.
func TestOneAnswerSettlesRepeatedPrompts(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	release := make(chan struct{})
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"general","label":"kid","task":"look"}`)
		},
		func(req model.Request) model.Response {
			// impatient: prompt the same child again before it answered
			out := req.Messages[len(req.Messages)-1].Blocks[0].Content // "spawned kid (general) as <id>"
			id := strings.TrimSpace(out[strings.LastIndex(out, " ")+1:])
			return call("c2", "agent_message", `{"id":"`+id+`","text":"send it now"}`)
		},
		func(model.Request) model.Response { return text("waiting") },
		func(model.Request) model.Response { return text("got it") },
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			<-release
			return call("k", "agent_response", `{"to":"`+parentIDFromSystem(req.System)+`","text":"one answer for both"}`)
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
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].State != "waiting" {
		t.Fatalf("two questions out: %+v", agents[0])
	}
	close(release)
	for te.Turn != 2 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].State != "idle" {
		t.Fatalf("one answer should settle both prompts; parent still %s", agents[0].State)
	}
}

func TestTodoListLogsProjectsAndRecovers(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	data := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "# Todo list") || !strings.Contains(req.System, "(empty)") {
				t.Errorf("system prompt should carry an empty todo section:\n%s", req.System)
			}
			return call("c1", "todo_add", `{"text":"Read the code"}`)
		},
		func(model.Request) model.Response { return call("c2", "todo_add", `{"text":"Fix the bug"}`) },
		func(model.Request) model.Response {
			return call("c3", "todo_update", `{"id":"t1","status":"in_progress"}`)
		},
		func(req model.Request) model.Response {
			// the list is projected into the system prompt at every call
			for _, want := range []string{"- t1 [in_progress] Read the code", "- t2 [pending] Fix the bug"} {
				if !strings.Contains(req.System, want) {
					t.Errorf("system prompt lacks %q:\n%s", want, req.System)
				}
			}
			return text("planned")
		},
	}
	h := newHarness(t, data, fm)
	ctx := context.Background()
	s, _ := h.c.CreateSession(ctx, work, "", "")
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	h.waitFor(event.TurnEnded, root)
	agents, _ = h.c.Tree(ctx, s.ID)
	todos := agents[0].Todos
	if len(todos) != 2 || todos[0].ID != "t1" || todos[0].Status != "in_progress" || todos[1].Text != "Fix the bug" || todos[1].Status != "pending" {
		t.Fatalf("todos %+v", todos)
	}
	evs, _ := h.d.Log.Read(ctx, s.ID, 1, 0)
	changed := 0
	for _, e := range evs {
		if e.Type == event.TodoChanged {
			changed++
		}
	}
	if changed != 3 {
		t.Fatalf("expected three todo.changed events, got %d", changed)
	}
	h.close()

	// restart: the list is replayed and ids continue past it
	fm2 := &fakeModel{}
	fm2.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c4", "todo_add", `{"text":"Run the tests"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !strings.Contains(last.Content, "added t3") {
				t.Errorf("ids should continue after recovery: %+v", last)
			}
			return text("ok")
		},
	}
	h2 := newHarness(t, data, fm2)
	defer h2.close()
	_ = h2.c.Subscribe(ctx, s.ID, evs[len(evs)-1].Seq+1) // live only; no replay of the old turn
	agents, _ = h2.c.Tree(ctx, s.ID)
	if len(agents[0].Todos) != 2 || agents[0].Todos[0].Status != "in_progress" {
		t.Fatalf("recovered todos %+v", agents[0].Todos)
	}
	_ = h2.c.Send(ctx, root, protocol.KindPrompt, "more")
	h2.waitFor(event.TurnEnded, root)
	agents, _ = h2.c.Tree(ctx, s.ID)
	if len(agents[0].Todos) != 3 || agents[0].Todos[2].ID != "t3" {
		t.Fatalf("todos after recovery %+v", agents[0].Todos)
	}
}

func TestFullAgentIDs(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look around"}`)
		},
		func(model.Request) model.Response { return text("waiting") },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0].Text
			if !strings.Contains(last, "found it") {
				t.Errorf("parent should have the child's answer: %q", last)
			}
			return text("thanks")
		},
	}
	var parent string
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			parent = parentIDFromSystem(req.System)
			last := req.Messages[len(req.Messages)-1].Blocks[0].Text
			// the task names its sender with the whole id, never a shortened one
			if !strings.Contains(last, "[message from agent main ("+parent+")]") {
				t.Errorf("task should name the parent by full id: %q (parent %s)", last, parent)
			}
			// a unique prefix still resolves, for models that shorten anyway
			return call("k1", "agent_response", `{"to":"`+parent[:8]+`","text":"found it"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "response delivered") {
				t.Errorf("prefix id should resolve: %+v", last)
			}
			return text("done")
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
	h.waitFor(event.ResponseReceived, root)
	h.waitFor(event.TurnEnded, root) // the "thanks" turn
	if len(fm.calls) < 3 {
		h.waitFor(event.TurnEnded, root)
	}
}

// TestRoles: role modes gate the root, /roles and agent_create; model and
// variant whitelists bound set_model/set_variant and decide what a child
// inherits; a subagent past max_turns answers its askers with the limit.
func TestRoles(t *testing.T) {
	setupConfig(t)
	g := os.Getenv("STAVLOS_CONFIG_DIR")
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","rootAgent":"lead"}`), 0o644)
	roles := filepath.Join(g, "roles")
	os.MkdirAll(roles, 0o755)
	os.WriteFile(filepath.Join(roles, "lead.md"), []byte("---\ndescription: Leads\nmode: primary\nmodels:\n  - id: fake/m1\n    variants: [high]\nspawn: [limited, boss, general]\n---\nYou lead.\n"), 0o644)
	os.WriteFile(filepath.Join(roles, "limited.md"), []byte("---\ndescription: Limited\nmode: subagent\nmodels: [fake/m2]\nmax_turns: 1\ntools: [read]\n---\nYou are limited.\n"), 0o644)
	os.WriteFile(filepath.Join(roles, "boss.md"), []byte("---\ndescription: Boss\nmode: primary\n---\nYou boss.\n"), 0o644)

	work := t.TempDir()
	fm := &fakeModel{}
	var childID string
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"limited","label":"kid","task":"think"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			childID = strings.TrimSpace(strings.TrimPrefix(last.Content, "spawned kid (limited) as "))
			if last.IsError || childID == "" || strings.Contains(childID, " ") {
				t.Errorf("spawn result: %+v", last)
			}
			return call("c2", "agent_create", `{"archetype":"boss","label":"b","task":"x"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || !strings.Contains(last.Content, "primary-only") {
				t.Errorf("a primary-only role must not be spawnable: %+v", last)
			}
			return call("c3", "agent_message", `{"id":"`+childID+`","text":"again"}`)
		},
		func(model.Request) model.Response { return text("sent") },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0].Text
			if !strings.Contains(last, "turn limit") {
				t.Errorf("the parent should be told about the turn limit: %q", last)
			}
			return text("noted")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "turn 1 of at most 1") {
				t.Errorf("the child should be told its turn budget:\n%s", req.System)
			}
			return text("thinking") // turn 1 passes without an answer
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, err := h.c.CreateSession(ctx, work, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	// the root runs the configured primary role on its default variant
	if agents[0].Archetype != "lead" || agents[0].Model != "fake/m1" || agents[0].Variant != "high" {
		t.Fatalf("root %+v", agents[0])
	}
	// whitelists bound the switches
	if err := h.c.SetAgentVariant(ctx, root, "low"); err == nil || !strings.Contains(err.Error(), "does not allow variant") {
		t.Fatalf("variant outside the role: %v", err)
	}
	if err := h.c.SetAgentModel(ctx, root, "fake/m2"); err == nil || !strings.Contains(err.Error(), "does not allow model") {
		t.Fatalf("model outside the role: %v", err)
	}
	if err := h.c.SetAgentRole(ctx, root, "limited"); err == nil || !strings.Contains(err.Error(), "subagent-only") {
		t.Fatalf("subagent-only role on the main agent: %v", err)
	}

	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	var sp event.AgentSpawnedPayload
	for sp.Parent == "" { // the subscription replays the root's own spawn first
		e := h.waitFor(event.AgentSpawned, "")
		_ = e.Decode(&sp)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents) != 2 || agents[1].Archetype != "limited" || agents[1].Model != "fake/m2" || agents[1].Variant != "" {
		t.Fatalf("child should start on its role's default model: %+v", agents)
	}
	if err := h.c.SetAgentRole(ctx, agents[1].ID, "boss"); err == nil || !strings.Contains(err.Error(), "primary-only") {
		t.Fatalf("primary-only role on a subagent: %v", err)
	}
	// the child's second turn is over its limit: the parent is answered
	e := h.waitFor(event.ResponseReceived, root)
	var rp event.ResponsePayload
	_ = e.Decode(&rp)
	if !strings.Contains(rp.Text, "turn limit of 1") {
		t.Fatalf("limit response: %+v", rp)
	}
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	// switching the root to a role without a whitelist keeps its model and variant
	if err := h.c.SetAgentRole(ctx, root, "general"); err != nil {
		t.Fatal(err)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].Archetype != "general" || agents[0].Model != "fake/m1" || agents[0].Variant != "high" {
		t.Fatalf("after /roles general: %+v", agents[0])
	}
}

// TestMain lets the test binary double as a stdio MCP server (one tool,
// echo) when STAVLOS_TEST_MCP_SERVER is set: the daemon under test starts
// it as an agent's server.
func TestMain(m *testing.M) {
	if os.Getenv("STAVLOS_TEST_MCP_SERVER") == "1" {
		runStubMCPServer()
		return
	}
	os.Exit(m.Run())
}

func runStubMCPServer() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "stub", Version: "1"}, nil)
	type in struct {
		Text string `json:"text" jsonschema:"text to echo back"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "Echo text back"}, func(ctx context.Context, req *mcp.CallToolRequest, args in) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text + " greeting=" + os.Getenv("GREETING")}}}, nil, nil
	})
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
}

// TestMCPServersPerAgent: a role's MCP servers start with the agent's first
// turn, their tools are offered as mcp__<server>__<tool> and run, a server
// that is not defined is reported as failed, env references expand, the
// tree shows every server's state, and a role change stops servers the
// new role does not list.
func TestMCPServersPerAgent(t *testing.T) {
	setupConfig(t)
	t.Setenv("STAVLOS_TEST_GREETING", "hello")
	g := os.Getenv("STAVLOS_CONFIG_DIR")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"model":"fake/m1","rootAgent":"mcpuser","mcp":{"echo":{"command":%q,"env":{"STAVLOS_TEST_MCP_SERVER":"1","GREETING":"${env:STAVLOS_TEST_GREETING}"}}},"policy":{"mcp__echo__*":"allow"}}`, exe)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(cfg), 0o644)
	os.MkdirAll(filepath.Join(g, "roles"), 0o755)
	os.WriteFile(filepath.Join(g, "roles", "mcpuser.md"), []byte("---\ndescription: Uses MCP\nmcp: [echo, missing]\ntools: [read]\n---\nYou use tools.\n"), 0o644)

	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			found := false
			for _, d := range req.Tools {
				if d.Name == "mcp__echo__echo" {
					found = true
					if !strings.Contains(d.Description, "[echo]") || !strings.Contains(string(d.Schema), "text") {
						t.Errorf("tool def %+v", d)
					}
				}
			}
			if !found || !strings.Contains(req.System, "# MCP tools") {
				t.Errorf("the echo tool should be offered: tools=%d system has MCP section=%v", len(req.Tools), strings.Contains(req.System, "# MCP tools"))
			}
			return call("c1", "mcp__echo__echo", `{"text":"hi"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || last.Content != "echo: hi greeting=hello" {
				t.Errorf("mcp tool result: %+v", last)
			}
			return text("done")
		},
		func(model.Request) model.Response { return text("switched") },
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, err := h.c.CreateSession(ctx, work, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.c.Subscribe(ctx, s.ID, 0)
	agents, _ := h.c.Tree(ctx, s.ID)
	root := agents[0].ID
	// before the first turn the servers are listed but pending
	if len(agents[0].MCP) != 2 || agents[0].MCP[0].Name != "echo" || agents[0].MCP[0].State != "pending" {
		t.Fatalf("pending servers %+v", agents[0].MCP)
	}
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	e := h.waitFor(event.MCPStarted, root)
	var sp event.MCPStartedPayload
	_ = e.Decode(&sp)
	if sp.Server != "echo" || len(sp.Tools) != 1 || sp.Tools[0] != "mcp__echo__echo" {
		t.Fatalf("mcp.started %+v", sp)
	}
	e = h.waitFor(event.MCPFailed, root)
	var fp event.MCPFailedPayload
	_ = e.Decode(&fp)
	if fp.Server != "missing" || !strings.Contains(fp.Error, "not defined") {
		t.Fatalf("mcp.failed %+v", fp)
	}
	h.waitFor(event.TurnEnded, root)
	agents, _ = h.c.Tree(ctx, s.ID)
	byName := map[string]protocol.MCPInfo{}
	for _, m := range agents[0].MCP {
		byName[m.Name] = m
	}
	if byName["echo"].State != "connected" || len(byName["echo"].Tools) != 1 || byName["missing"].State != "failed" {
		t.Fatalf("tree mcp %+v", agents[0].MCP)
	}
	// an idle agent's servers stop after MCPIdleAfter and start again at
	// its next turn (agents are never killed, so this is what keeps an idle
	// child cheap)
	agent.MCPIdleAfter = 150 * time.Millisecond
	defer func() { agent.MCPIdleAfter = 10 * time.Minute }()
	fm.mu.Lock()
	fm.steps = append(fm.steps, func(model.Request) model.Response { return text("idle now") })
	fm.mu.Unlock()
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "one more")
	h.waitFor(event.TurnEnded, root)
	h.waitFor(event.MCPStopped, root)
	agents, _ = h.c.Tree(ctx, s.ID)
	for _, m := range agents[0].MCP {
		if m.Name == "echo" && m.State != "pending" {
			t.Fatalf("idle stop should leave the server pending for the next turn: %+v", m)
		}
	}
	agent.MCPIdleAfter = 10 * time.Minute
	// a role without MCP: the next turn stops the server
	if err := h.c.SetAgentRole(ctx, root, "general"); err != nil {
		t.Fatal(err)
	}
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "again")
	h.waitFor(event.TurnEnded, root)
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents[0].MCP) != 0 {
		t.Fatalf("servers should be gone after the role change: %+v", agents[0].MCP)
	}
}

// TestWorkingDirectories: reads and commands inside the session directory
// and the role's dirs run without a boundary prompt; a path outside asks
// (naming the directory) even though read is allowed, and "allow_always"
// adds that directory to the agent; a child can be granted only
// directories its parent has; grants and additions survive a restart.
func TestWorkingDirectories(t *testing.T) {
	setupConfig(t)
	g := os.Getenv("STAVLOS_CONFIG_DIR")
	work := t.TempDir()
	shared := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(shared, "lib.txt"), []byte("lib"), 0o644)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(work, "in.txt"), []byte("in"), 0o644)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","rootAgent":"lead"}`), 0o644)
	os.MkdirAll(filepath.Join(g, "roles"), 0o755)
	os.WriteFile(filepath.Join(g, "roles", "lead.md"), []byte(fmt.Sprintf("---\ndescription: Leads\ndirs: [%q]\nspawn: [general]\n---\nYou lead.\n", shared)), 0o644)

	data := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "Your working directories: "+work+", "+shared) {
				t.Errorf("system prompt should list the directories:\n%s", req.System)
			}
			return call("c1", "read", `{"path":"in.txt"}`) // inside the session dir
		},
		func(model.Request) model.Response { return call("c2", "read", `{"path":"`+shared+`/lib.txt"}`) }, // inside a role dir
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "lib") {
				t.Errorf("role dir read: %+v", last)
			}
			return call("c3", "read", `{"path":"`+outside+`/secret.txt"}`) // outside: asks
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "s") {
				t.Errorf("outside read after allow_always: %+v", last)
			}
			return call("c4", "read", `{"path":"`+outside+`/secret.txt"}`) // now inside: no prompt
		},
		func(model.Request) model.Response {
			// a grant inside the parent's set works; one outside is refused
			return call("c5", "agent_create", `{"archetype":"general","label":"kid","task":"x","dirs":["`+shared+`"]}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError {
				t.Errorf("grant inside the parent's set: %+v", last)
			}
			return call("c6", "agent_create", `{"archetype":"general","label":"kid2","task":"x","dirs":["/nowhere/else"]}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || !strings.Contains(last.Content, "not inside your directories") {
				t.Errorf("grant outside the parent's set should fail: %+v", last)
			}
			return text("done")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response { return text("child idle") },
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
	if len(agents[0].Dirs) != 2 || agents[0].Dirs[0].Path != work || agents[0].Dirs[0].Source != "session" || agents[0].Dirs[1].Path != shared || agents[0].Dirs[1].Source != "role" {
		t.Fatalf("dirs %+v", agents[0].Dirs)
	}
	_ = h.c.Send(ctx, root, protocol.KindPrompt, "go")
	// the only prompt is the boundary one, and it names the directory
	e := h.waitFor(event.PromptRequested, root)
	var pr event.PromptRequestedPayload
	_ = e.Decode(&pr)
	if pr.Tool != "read" || !strings.Contains(pr.Question, "outside its directories") || !strings.Contains(pr.Question, outside) {
		t.Fatalf("boundary prompt %+v", pr)
	}
	pending := h.d.esc.Pending(s.ID)
	if len(pending) != 1 || pending[0].Dir != outside {
		t.Fatalf("pending %+v", pending)
	}
	if err := h.c.ClaimPrompt(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := h.c.ReplyPrompt(ctx, pending[0].ID, "allow_always"); err != nil {
		t.Fatal(err)
	}
	e = h.waitFor(event.AgentDirAdded, root)
	var dp event.DirAddedPayload
	_ = e.Decode(&dp)
	if dp.Dir != outside || dp.Source != "human" {
		t.Fatalf("dir added %+v", dp)
	}
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if len(agents[0].Dirs) != 3 || agents[0].Dirs[2].Path != outside || agents[0].Dirs[2].Source != "human" {
		t.Fatalf("dirs after the answer %+v", agents[0].Dirs)
	}
	var kid protocol.AgentInfo
	for _, a := range agents {
		if a.Label == "kid" {
			kid = a
		}
	}
	if kid.ID == "" || len(kid.Dirs) != 2 || kid.Dirs[1].Path != shared || kid.Dirs[1].Source != "grant" {
		t.Fatalf("child dirs %+v", kid.Dirs)
	}
	// the human edits the set: add, remove (a role directory hides, the
	// session directory refuses), and a relative path resolves
	extra := t.TempDir()
	if err := h.c.AddAgentDir(ctx, root, extra); err != nil {
		t.Fatal(err)
	}
	if err := h.c.AddAgentDir(ctx, root, "sub/dir"); err != nil { // inside the session dir: already covered, a no-op
		t.Fatal(err)
	}
	if err := h.c.RemoveAgentDir(ctx, root, shared); err != nil {
		t.Fatal(err)
	}
	if err := h.c.RemoveAgentDir(ctx, root, work); err == nil || !strings.Contains(err.Error(), "session directory") {
		t.Fatalf("removing the session directory: %v", err)
	}
	if err := h.c.RemoveAgentDir(ctx, root, "/never/there"); err == nil {
		t.Fatal("removing an unknown directory should fail")
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	paths := func(ds []protocol.DirInfo) string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Path+":"+d.Source)
		}
		return strings.Join(out, " ")
	}
	if got := paths(agents[0].Dirs); got != work+":session "+outside+":human "+extra+":human" {
		t.Fatalf("edited dirs: %s", got)
	}
	h.close()

	// restart: grants, additions and removals come back
	h2 := newHarness(t, data, &fakeModel{})
	defer h2.close()
	agents, _ = h2.c.Tree(ctx, s.ID)
	if got := paths(agents[0].Dirs); got != work+":session "+outside+":human "+extra+":human" {
		t.Fatalf("recovered root dirs: %s", got)
	}
	for _, a := range agents {
		if a.Label == "kid" && (len(a.Dirs) != 2 || a.Dirs[1].Path != shared) {
			t.Fatalf("recovered child dirs %+v", a.Dirs)
		}
	}
}

// TestBoundaryPromptEditedDir: allow_always with an edited directory adds
// that directory rather than the offered one.
func TestBoundaryPromptEditedDir(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	outside := t.TempDir()
	os.MkdirAll(filepath.Join(outside, "sub"), 0o755)
	os.WriteFile(filepath.Join(outside, "sub", "f.txt"), []byte("f"), 0o644)
	os.WriteFile(filepath.Join(outside, "other.txt"), []byte("o"), 0o644)
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c1", "read", `{"path":"`+outside+`/sub/f.txt"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError {
				t.Errorf("first read: %+v", last)
			}
			return call("c2", "read", `{"path":"`+outside+`/other.txt"}`) // covered by the edited (wider) directory: no prompt
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "o") {
				t.Errorf("second read should run without a prompt: %+v", last)
			}
			return text("done")
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
	pending := h.d.esc.Pending(s.ID)
	if len(pending) != 1 || pending[0].Dir != filepath.Join(outside, "sub") {
		t.Fatalf("offered dir %+v", pending)
	}
	_ = h.c.ClaimPrompt(ctx, pending[0].ID)
	if err := h.c.ReplyPromptDir(ctx, pending[0].ID, "allow_always", outside); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.AgentDirAdded, root)
	var dp event.DirAddedPayload
	_ = e.Decode(&dp)
	if dp.Dir != outside {
		t.Fatalf("the edited directory should be added: %+v", dp)
	}
	h.waitFor(event.TurnEnded, root)
	if n := len(h.d.esc.Pending(s.ID)); n != 0 {
		t.Fatalf("no further prompt expected, %d pending", n)
	}
}

// TestDenyReasonReachesTheAgent: a deny with a reason shows up in the tool
// result the model reads; without one the plain message stays.
func TestDenyReasonReachesTheAgent(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"touch a"}`) },
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || last.Content != "Permission denied by the user: use apply_patch instead" {
				t.Errorf("deny with reason: %+v", last)
			}
			return call("c2", "shell", `{"command":"touch b"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || last.Content != "Permission denied by the user." {
				t.Errorf("deny without reason: %+v", last)
			}
			return text("ok")
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
	p := h.d.esc.Pending(s.ID)[0]
	_ = h.c.ClaimPrompt(ctx, p.ID)
	if err := h.c.DenyPrompt(ctx, p.ID, "use apply_patch instead"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.PromptRequested, root)
	p = h.d.esc.Pending(s.ID)[0]
	_ = h.c.ClaimPrompt(ctx, p.ID)
	if err := h.c.DenyPrompt(ctx, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.TurnEnded, root)
}

// TestAllowPrefix: "allow_prefix" runs the call and approves every later
// simple command of the tool starting with the prefix; a chained command
// with the same start asks again.
func TestAllowPrefix(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"touch one"}`) },
		func(req model.Request) model.Response {
			if last := req.Messages[len(req.Messages)-1].Blocks[0]; last.IsError {
				t.Errorf("first touch: %+v", last)
			}
			return call("c2", "shell", `{"command":"touch two words"}`)
		},
		func(req model.Request) model.Response {
			if last := req.Messages[len(req.Messages)-1].Blocks[0]; last.IsError {
				t.Errorf("covered touch should run without asking: %+v", last)
			}
			return call("c3", "shell", `{"command":"touch three; touch four"}`)
		},
		func(req model.Request) model.Response {
			if last := req.Messages[len(req.Messages)-1].Blocks[0]; !last.IsError || !strings.Contains(last.Content, "denied") {
				t.Errorf("chained command should have asked (and been denied): %+v", last)
			}
			return text("ok")
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
	p := h.d.esc.Pending(s.ID)[0]
	_ = h.c.ClaimPrompt(ctx, p.ID)
	if err := h.c.AllowPromptPrefix(ctx, p.ID, "touch"); err != nil {
		t.Fatal(err)
	}
	// the second echo runs without a prompt; the chained one asks
	h.waitFor(event.PromptRequested, root)
	p = h.d.esc.Pending(s.ID)[0]
	if !strings.Contains(string(p.Input), "touch three; touch four") {
		t.Fatalf("second prompt should be the chained command: %s", p.Input)
	}
	_ = h.c.ClaimPrompt(ctx, p.ID)
	if err := h.c.DenyPrompt(ctx, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.TurnEnded, root)
	for _, f := range []string{"one", "two", "words"} {
		if _, err := os.Stat(filepath.Join(work, f)); err != nil {
			t.Errorf("%s should exist: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "three")); err == nil {
		t.Error("the chained command was denied and must not have run")
	}
}

// TestAskUser: ask_user raises one question prompt for the batch, the
// answers come back as "header: answer" lines, and a cancel withdraws it.
func TestAskUser(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			has := false
			for _, d := range req.Tools {
				if d.Name == "ask_user" {
					has = true
				}
			}
			if !has || !strings.Contains(req.System, "# Asking the human") {
				t.Errorf("ask_user should be offered to every agent")
			}
			return call("c1", "ask_user", `{"questions":[{"question":"Which backend?","options":[{"label":"Postgres","description":"in use"},{"label":"SQLite"}]},{"question":"Call it?","options":[{"label":"stavlos-api"}]}]}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || last.Content != "Which backend? → Postgres\nCall it? → stavlos" {
				t.Errorf("answers: %+v", last)
			}
			return call("c2", "ask_user", `{"questions":[{"question":"Sure?","options":[{"label":"yes"}]}]}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || !strings.Contains(last.Content, "withdrawn") {
				t.Errorf("a cancelled question should come back withdrawn: %+v", last)
			}
			return text("ok")
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
	e := h.waitFor(event.PromptRequested, root)
	var pr event.PromptRequestedPayload
	_ = e.Decode(&pr)
	if pr.Kind != "question" || pr.Tool != "ask_user" || !strings.Contains(string(pr.Questions), "Postgres") {
		t.Fatalf("prompt %+v", pr)
	}
	agents, _ = h.c.Tree(ctx, s.ID)
	if agents[0].State != "blocked" {
		t.Fatalf("an asking agent is blocked: %+v", agents[0].State)
	}
	p := h.d.esc.Pending(s.ID)[0]
	if len(p.Questions) != 2 || p.Questions[0].Options[0].Label != "Postgres" {
		t.Fatalf("pending %+v", p)
	}
	_ = h.c.ClaimPrompt(ctx, p.ID)
	if err := h.c.AnswerQuestions(ctx, p.ID, []string{"Postgres", "stavlos"}); err != nil {
		t.Fatal(err)
	}
	// the second question is cancelled instead of answered
	h.waitFor(event.PromptRequested, root)
	_ = h.c.Send(ctx, root, protocol.KindCancel, "")
	h.waitFor(event.PromptWithdrawn, root)
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
}
