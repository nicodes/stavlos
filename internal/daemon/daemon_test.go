package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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

// callCount is how many calls the model has had.
func (f *fakeModel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeModel) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	// The per-request state note (turn budget, todo list, fan-out) is moved
	// from the last message to the end of the system prompt, so steps read
	// the history as the log has it and the note where they look for it.
	var note string
	req.Messages, note = withoutNote(req.Messages)
	if note != "" {
		req.System += "\n" + note
	}
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
	// A step may block (a test holding the model mid-call); a cancelled call
	// or a stopping daemon still returns, as a real provider's would.
	done := make(chan model.Response, 1)
	go func() { done <- step(req) }()
	select {
	case r := <-done:
		r.Usage = model.Usage{InputTokens: 10, OutputTokens: 5}
		return r, nil
	case <-ctx.Done():
		return model.Response{}, ctx.Err()
	}
}

// withoutNote drops the per-request harness state note the runtime appends
// to the last user message, so steps inspect the history itself.
func withoutNote(msgs []model.Message) ([]model.Message, string) {
	if len(msgs) == 0 {
		return msgs, ""
	}
	last := msgs[len(msgs)-1]
	n := len(last.Blocks)
	if n == 0 || !strings.HasPrefix(last.Blocks[n-1].Text, "[harness state") {
		return msgs, ""
	}
	note := last.Blocks[n-1].Text
	out := append([]model.Message(nil), msgs...)
	out[len(out)-1].Blocks = last.Blocks[:n-1]
	if n == 1 {
		out = out[:len(out)-1]
	}
	return out, note
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
	if _, err := rpc.Do(ctx, c, protocol.Attach, protocol.AttachParams{Client: "test", Tier: protocol.TierInteractive}); err != nil {
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

// recentEvents lists every channel event for the dump on timeout.
func (h *harness) recentEvents() []string {
	rows, _ := h.d.Log.Channels(context.Background())
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
// waitInput waits for an input of kind queued for agent.
func (h *harness) waitInput(kind event.InputKind, agent string) event.Input {
	h.t.Helper()
	for {
		var in event.Input
		if _ = h.waitFor(event.InputQueued, agent).Decode(&in); in.Kind == kind {
			return in
		}
	}
}

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

// waitTree polls the agent tree until cond holds (or 10 s pass).
func (h *harness) waitTree(channel string, cond func([]protocol.AgentInfo) bool) []protocol.AgentInfo {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var agents []protocol.AgentInfo
	for time.Now().Before(deadline) {
		agents, _ = tree(context.Background(), h.c, channel)
		if cond(agents) {
			return agents
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("tree never settled: %+v\nevents so far:\n%s", agents, strings.Join(h.recentEvents(), "\n"))
	return nil
}

func setupConfig(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","reminders":false,"policy":{"shell":{"echo*":"allow","*":"ask"},"write":"allow"}}`), 0o644)
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
			return call("k1", "message", `{"to":"`+parentIDFromSystem(req.System)+`","text":"found it","kind":"response"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()

	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0})); err != nil {
		t.Fatal(err)
	}
	agents, err := tree(ctx, h.c, s.ID)
	if err != nil || len(agents) != 1 || agents[0].Role != "general" || agents[0].Model != "fake/m1" {
		t.Fatalf("tree %v %v", agents, err)
	}
	root := agents[0].ID

	// permission prompt round trip
	// Prompts are fetched via prompt.list; the harness reader only keeps events.
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"})); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.ToolFinished, root) // echo hello (allowed)
	h.waitFor(event.AskRequested, root)
	ps, err := prompts(ctx, h.c, s.ID)
	if err != nil || len(ps) != 1 || ps[0].Tool != "shell" {
		t.Fatalf("prompts %v %v", ps, err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: ps[0].ID})); err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: ps[0].ID, Answer: "allow"})); err != nil {
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
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "delegate"})); err != nil {
		t.Fatal(err)
	}
	sp := h.waitFor(event.AgentSpawned, "")
	var spp event.AgentSpawnedPayload
	_ = sp.Decode(&spp)
	if spp.Parent != root || spp.Name != "scout" || spp.Model != "fake/m1" || spp.Depth != 1 {
		t.Fatalf("spawned %+v", spp)
	}
	h.waitInput(event.InputResponse, root)
	// turn 2 may end before or after the child answers; wait for turn 3,
	// the one started by the response.
	for te.Turn != 3 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if te.Reason != "end_turn" {
		t.Fatalf("turn 3 ended %+v", te)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents) == 2 && agents[1].State != "idle" {
		h.waitFor(event.TurnEnded, agents[1].ID) // the child's own turn may still be closing
		agents, _ = tree(ctx, h.c, s.ID)
	}
	if len(agents) != 2 || agents[1].State != "idle" || agents[0].CostUSD != 0 {
		t.Fatalf("tree after (the child stays alive, idle): %+v", agents)
	}
	// usage was logged
	evs, _ := h.d.Log.Read(ctx, s.ID, 1, 0)
	nUsage := 0
	for _, e := range evs {
		var am event.AssistantMessagePayload
		if e.Type == event.AssistantMessage && e.Decode(&am) == nil && am.Usage.InputTokens > 0 {
			nUsage++
		}
	}
	if nUsage != 8 { // 3 (turn 1) + 1 (spawn) + 1 (stop) + 2 (child: answer, then its turn ends) + 1 (turn 3)
		t.Fatalf("usage events %d", nUsage)
	}
	// reconcile
	rc, err := rpc.Do(ctx, h.c, protocol.Reconcile, protocol.ChannelRef{Channel: s.ID})
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
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	_ = errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "auto"})) // chained commands ask under policy; auto answers inside the directory
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.ToolStarted, root)
	time.Sleep(300 * time.Millisecond)
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindCancel, Text: ""}))
	e := h.waitFor(event.ToolFinished, root)
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
	agents, _ = tree(ctx, h.c, s.ID)
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
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindSteer, Text: "steer while idle"}))
	h.waitFor(event.TurnStarted, root)
	time.Sleep(200 * time.Millisecond)
	h.close()
	close(block)

	// restart on the same data dir
	fm2 := &fakeModel{}
	h2 := newHarness(t, data, fm2)
	defer h2.close()
	list, err := channels(ctx, h2.c, work, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("channels after restart: %v %v", list, err)
	}
	evs, _ := h2.d.Log.Read(ctx, s.ID, 1, 0)
	last := evs[len(evs)-1]
	if last.Type != event.TurnAborted {
		t.Fatalf("last event after restart: %s", last.Type)
	}
	agents, err = tree(ctx, h2.c, s.ID)
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
	_ = errOf(rpc.Do(ctx, h2.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: last.Seq + 1})) // live only; no replay of the old turns
	_ = errOf(rpc.Do(ctx, h2.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "still there?"}))
	e = h2.waitFor(event.TurnEnded, root)
	_ = e.Decode(&te)
	if te.Reason != "end_turn" {
		t.Fatalf("%+v", te)
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
			return call("k", "message", `{"to":"`+parentIDFromSystem(req.System)+`","text":"late","kind":"response"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "delegate"}))
	e := h.waitFor(event.TurnEnded, root)
	var te event.TurnEndedPayload
	_ = e.Decode(&te)
	if te.Turn != 1 {
		t.Fatalf("%+v", te)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents) != 2 || agents[0].State != "waiting" || len(agents[0].Awaiting) != 1 || agents[0].Awaiting[0] != agents[1].ID {
		t.Fatalf("parent idle with a question out should read waiting on the child: %+v", agents)
	}
	if list, _ := channels(ctx, h.c, work, false); len(list) != 1 || list[0].State != "working" { // the child still runs
		t.Fatalf("channel state while a child works: %+v", list)
	}
	close(release)
	e = h.waitFor(event.TurnEnded, root)
	_ = e.Decode(&te)
	if te.Turn != 2 || te.Reason != "end_turn" {
		t.Fatalf("parent was not woken by the response: %+v", te)
	}
	// the child is still there, idle, ready for a follow-up; the parent is
	// plainly idle again now that the answer landed. The child's own turn
	// ends after its message call returns, so wait for both.
	h.waitTree(s.ID, func(agents []protocol.AgentInfo) bool {
		return len(agents) == 2 && agents[1].State == "idle" && agents[0].State == "idle"
	})
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
		// woken by the job
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks
			txt := last[len(last)-1].Text
			if !strings.Contains(txt, "exited 3") || !strings.Contains(txt, "one\ntwo") {
				t.Errorf("job wake text: %q", txt)
			}
			return text("noted")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	_ = errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "auto"}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "run it"}))
	e := h.waitFor(event.JobStarted, root)
	var ms event.JobStartedPayload
	_ = e.Decode(&ms)
	if ms.ID == "" || ms.Command == "" {
		t.Fatalf("%+v", ms)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents[0].Jobs) != 1 {
		t.Fatalf("jobs in tree: %+v", agents[0].Jobs)
	}
	e = h.waitFor(event.JobFinished, root)
	var mf event.JobFinishedPayload
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
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents[0].Jobs) != 0 {
		t.Fatalf("job should be gone: %+v", agents[0].Jobs)
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	_ = errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "auto"}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "run it"}))
	h.waitFor(event.JobStarted, root)
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents[0].Jobs) != 1 || agents[0].Jobs[0].Label != "echo early; sleep 2; echo late; exit 2" {
		t.Fatalf("jobs in tree: %+v", agents[0].Jobs)
	}
	e := h.waitFor(event.JobFinished, root)
	var mf event.JobFinishedPayload
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
	s2, _ := rpc.Do(ctx, h2.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h2.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s2.ID, From: 0}))
	_ = errOf(rpc.Do(ctx, h2.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s2.ID, Mode: "auto"}))
	agents, _ = tree(ctx, h2.c, s2.ID)
	_ = errOf(rpc.Do(ctx, h2.c, protocol.AgentSend, protocol.AgentSendParams{Agent: agents[0].ID, Kind: protocol.KindPrompt, Text: "run it"}))
	h2.waitFor(event.TurnEnded, agents[0].ID)
	evs, _ := h2.d.Log.Read(ctx, s2.ID, 1, 0)
	for _, e := range evs {
		if e.Type == event.JobStarted {
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	_ = errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "auto"}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	start := time.Now()
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.JobStopped, root)
	h.waitFor(event.TurnEnded, root)
	if time.Since(start) > 5*time.Second {
		t.Fatal("stop did not kill the command promptly")
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents[0].Jobs) != 0 {
		t.Fatalf("%+v", agents[0].Jobs)
	}
}

func TestSetRoleSwitchesPresetInPlace(t *testing.T) {
	setupConfig(t)
	// Only "general" ships built in; a user preset comes from agents/<name>.md.
	agentsDir := filepath.Join(os.Getenv("STAVLOS_CONFIG_DIR"), "roles")
	_ = os.MkdirAll(agentsDir, 0o755)
	os.WriteFile(filepath.Join(agentsDir, "explorer.md"), []byte("---\ndescription: Read-only investigation\ntools:\n  apply_patch: deny\n  skill: deny\n  todo: deny\n  web_fetch: deny\n  web_search: deny\n---\nYou are a read-only code explorer. Do not modify anything.\n"), 0o644)
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "one"}))
	h.waitFor(event.TurnEnded, root)
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetRole, protocol.AgentSetRoleParams{Agent: root, Role: "nope"})); err == nil {
		t.Fatal("unknown role accepted")
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetRole, protocol.AgentSetRoleParams{Agent: root, Role: "explorer"})); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.AgentUpdated, root)
	agents, _ = tree(ctx, h.c, s.ID)
	if agents[0].Role != "explorer" || agents[0].Name != "main" { // the root keeps its "main" label
		t.Fatalf("tree after role change: %+v", agents[0])
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "two"}))
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if te.Reason != "end_turn" {
		t.Fatalf("%+v", te)
	}
}

// TestAgentsMessageAcrossTheChannel: a child messages its parent, which is
// waiting on it, so the message is the answer; the parent sees who sent it,
// agent_status shows the whole tree, and a second message to the parent, no
// longer waiting, is a new message (a steer) rather than an answer.
func TestAgentsMessageAcrossTheChannel(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	var rootID string
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"ask me something"}`)
		},
		func(model.Request) model.Response { return text("delegated") },
		// woken by the child's answer: the model sees the sender
		func(req model.Request) model.Response {
			seen := false
			for _, m := range req.Messages {
				for _, b := range m.Blocks {
					seen = seen || (strings.HasPrefix(b.Text, "[message from agent scout") && strings.Contains(b.Text, "which branch?"))
				}
			}
			if !seen {
				t.Errorf("parent should see the child's message with its sender: %+v", req.Messages[len(req.Messages)-1])
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
			return call("k1", "message", `{"to":"`+pid+`","text":"which branch?","kind":"response"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || last.Content != "response delivered to main" {
				t.Errorf("message to the waiting parent should be an answer: %+v", last)
			}
			// one messaging tool for everyone: the older tools are gone
			for _, d := range req.Tools {
				if d.Name == "agent_message" || d.Name == "agent_response" {
					t.Errorf("subagent should not be offered %s", d.Name)
				}
			}
			return call("k3", "agent_status", `{}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			flat := strings.Join(strings.Fields(last.Content), "") // the tool pretty-prints its JSON
			if last.IsError || !strings.Contains(flat, `"name":"main"`) || !strings.Contains(flat, `"you":true`) || !strings.Contains(flat, `"parent":"`+rootID+`"`) {
				t.Errorf("agent_status should list the whole tree with the caller marked: %+v", last)
			}
			return call("k4", "message", `{"to":"main","text":"asked"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	rootID = agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: rootID, Kind: protocol.KindPrompt, Text: "delegate"}))

	// The parent's logged answer (it carries the sender's name), the end of
	// its turn 2 and the child's second message (after its status check)
	// race each other, so one loop watches for all three: a separate wait
	// for the answer would throw away a steer that landed first.
	var um event.Input
	var turn2, responded bool
	deadline := time.After(10 * time.Second)
	for um.FromName == "" || !turn2 || !responded {
		select {
		case e := <-h.evs:
			var in event.Input
			if e.Type == event.InputQueued {
				_ = e.Decode(&in)
			}
			switch {
			case e.Type == event.InputQueued && e.Agent == rootID && in.Kind == event.InputResponse && um.FromName == "":
				um = in // the answer, not the child's later message
			case e.Type == event.TurnEnded && e.Agent == rootID:
				var te event.TurnEndedPayload
				if _ = e.Decode(&te); te.Turn == 2 {
					if te.Reason != "end_turn" {
						t.Fatalf("turn 2: %+v", te)
					}
					turn2 = true
				}
			case e.Type == event.InputQueued && e.Agent == rootID && in.Kind == event.InputRequest:
				responded = true
			}
		case <-deadline:
			t.Fatalf("answer %+v, turn 2 ended %v, second message received %v\nevents so far:\n%s", um, turn2, responded, strings.Join(h.recentEvents(), "\n"))
		}
	}
	if um.FromName != "scout" || um.Text != "which branch?" || um.Kind != event.InputResponse {
		t.Fatalf("parent's message: %+v", um)
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
			return call("k1", "message", `{"to":"`+parentIDFromSystem(req.System)+`","text":"ok","kind":"response"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID

	if vs, err := func() ([]string, error) {
		r, err := rpc.Do(ctx, h.c, protocol.Variants, protocol.VariantsParams{Model: "fake/m1"})
		return r.Variants, err
	}(); err != nil || len(vs) != 2 || vs[1] != "high" {
		t.Fatalf("variants: %v %v", vs, err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetVariant, protocol.AgentSetVariantParams{Agent: root, Variant: "extreme"})); err == nil {
		t.Fatal("unknown variant should be rejected")
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetVariant, protocol.AgentSetVariantParams{Agent: root, Variant: "high"})); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.AgentUpdated, root)
	var vp event.AgentUpdatedPayload
	_ = e.Decode(&vp)
	if vp.Variant == nil || *vp.Variant != "high" {
		t.Fatalf("%+v", vp)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if agents[0].Variant != "high" {
		t.Fatalf("tree variant: %+v", agents[0])
	}

	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	sp := h.waitFor(event.AgentSpawned, "")
	var spp event.AgentSpawnedPayload
	_ = sp.Decode(&spp)
	h.waitInput(event.InputResponse, root)
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
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents) != 2 || agents[1].Variant != "high" {
		t.Fatalf("child variant: %+v", agents)
	}
	// back to the default
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetVariant, protocol.AgentSetVariantParams{Agent: root, Variant: ""})); err != nil {
		t.Fatal(err)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if agents[0].Variant != "" {
		t.Fatalf("reset: %+v", agents[0])
	}
}

// TestYolo: with the channel in yolo, ask-gated calls run without a prompt,
// a prompt already waiting is approved when yolo turns on, and a deny rule
// still denies.
func TestYolo(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","reminders":false,"policy":{"shell":{"echo*":"allow","rm*":"deny","*":"ask"}}}`), 0o644)
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID

	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.AskRequested, root)
	if ps, _ := prompts(ctx, h.c, s.ID); len(ps) != 1 {
		t.Fatalf("one prompt should be waiting: %+v", ps)
	}
	// yolo on: the waiting prompt is approved and logged
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "yolo"})); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.ChannelUpdated, "")
	var mp event.ChannelUpdatedPayload
	_ = e.Decode(&mp)
	if mp.Mode == nil || *mp.Mode != "yolo" {
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
	if ps, _ := prompts(ctx, h.c, s.ID); len(ps) != 0 {
		t.Fatalf("prompt queue should be drained: %+v", ps)
	}
	rc, _ := rpc.Do(ctx, h.c, protocol.Reconcile, protocol.ChannelRef{Channel: s.ID})
	if rc.Channel.Mode != "yolo" {
		t.Fatalf("channel info should show yolo: %+v", rc.Channel)
	}

	// turn 2 runs with no prompt at all
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "again"}))
	for te.Turn != 2 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if _, err := os.Stat(filepath.Join(work, "second")); err != nil {
		t.Fatal("second command should have run")
	}
	evs, _ := h.d.Log.Read(ctx, s.ID, 1, 0)
	asks := 0
	for _, ev := range evs {
		if ev.Type == event.AskRequested {
			asks++
		}
	}
	if asks != 1 {
		t.Fatalf("only turn 1's prompt should have been raised, not one in yolo: %d", asks)
	}
	// back to ask
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "ask"})); err != nil {
		t.Fatal(err)
	}
	rc, _ = rpc.Do(ctx, h.c, protocol.Reconcile, protocol.ChannelRef{Channel: s.ID})
	if rc.Channel.Mode != "ask" {
		t.Fatalf("mode should be ask: %+v", rc.Channel)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "turbo"})); err == nil {
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
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","reminders":false,"policy":{"shell":{"rm*":"deny","*":"ask"}}}`), 0o644)
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
			if !last.IsError || !strings.Contains(last.Content, "auto mode") {
				t.Errorf("auto denies a call outside the directories: %+v", last)
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.AskRequested, root) // the inside command waits in ask mode
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "auto"})); err != nil {
		t.Fatal(err)
	}
	// auto approves the waiting inside command, then denies the outside read
	// without asking
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	if _, err := os.Stat(filepath.Join(work, "inside")); err != nil {
		t.Fatal("the inside command should have run under auto")
	}
	if n := len(h.d.esc.Pending(s.ID)); n != 0 {
		t.Fatalf("auto should leave no boundary prompt waiting: %d", n)
	}
	rc, _ := rpc.Do(ctx, h.c, protocol.Reconcile, protocol.ChannelRef{Channel: s.ID})
	if rc.Channel.Mode != "auto" {
		t.Fatalf("%+v", rc.Channel)
	}
}

func TestChannelListTitles(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{func(model.Request) model.Response { return text("hi") }}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	list, err := channels(ctx, h.c, work, false)
	if err != nil || len(list) != 1 || list[0].Title != "" {
		t.Fatalf("fresh channel should have no title: %+v %v", list, err)
	}
	agents, _ := tree(ctx, h.c, s.ID)
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: agents[0].ID, Kind: protocol.KindPrompt, Text: "fix the login bug\nand add tests"}))
	h.waitFor(event.TurnEnded, agents[0].ID)
	list, _ = channels(ctx, h.c, work, false)
	if len(list) != 1 || list[0].Title != "fix the login bug" {
		t.Fatalf("title should be the first prompt's first line: %+v", list)
	}
	// a second channel in the same directory lists first (newest)
	s2, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	list, _ = channels(ctx, h.c, work, false)
	if len(list) != 2 || list[0].ID != s2.ID || list[1].Title != "fix the login bug" {
		t.Fatalf("newest first with titles: %+v", list)
	}
}

// TestRecoveredAgentWithMissingPresetFallsBack: a channel whose root was
// created under a preset that no longer exists resumes as the configured
// root preset, with its full tool set.
func TestRecoveredAgentWithMissingPresetFallsBack(t *testing.T) {
	setupConfig(t)
	agentsDir := filepath.Join(os.Getenv("STAVLOS_CONFIG_DIR"), "roles")
	_ = os.MkdirAll(agentsDir, 0o755)
	presetFile := filepath.Join(agentsDir, "coder.md")
	os.WriteFile(presetFile, []byte("---\ndescription: Old coder\ntools:\n  apply_patch: deny\n  skill: deny\n  todo: deny\n  web_fetch: deny\n  web_search: deny\n---\nYou are the old coder.\n"), 0o644)
	work := t.TempDir()
	data := t.TempDir()
	fm := &fakeModel{}
	h := newHarness(t, data, fm)
	ctx := context.Background()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: "coder"})
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := tree(ctx, h.c, s.ID)
	if agents[0].Role != "coder" {
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
		if !strings.Contains(req.System, "role's definition is gone") {
			t.Errorf("system prompt should say the role is gone: %.80q", req.System)
		}
		return text("ok")
	}}
	h2 := newHarness(t, data, fm2)
	defer h2.close()
	if _, err := rpc.Do(ctx, h2.c, protocol.ChannelResume, protocol.ChannelRef{Channel: s.ID}); err != nil {
		t.Fatal(err)
	}
	agents, _ = tree(ctx, h2.c, s.ID)
	// Recovery never widens: the agent keeps its archetype name, runs
	// read-only, and says why.
	if agents[0].Role != "coder" || !strings.Contains(agents[0].LastError, "no longer exists") {
		t.Fatalf("root should come back read-only under its old name: %+v", agents[0])
	}
	_ = errOf(rpc.Do(ctx, h2.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	_ = errOf(rpc.Do(ctx, h2.c, protocol.AgentSend, protocol.AgentSendParams{Agent: agents[0].ID, Kind: protocol.KindPrompt, Text: "hello"}))
	h2.waitFor(event.TurnEnded, agents[0].ID)
	got := " " + strings.Join(offered, " ") + " "
	for _, gone := range []string{" shell ", " apply_patch ", " agent_create "} {
		if strings.Contains(got, gone) {
			t.Fatalf("the fallback must not offer %s, got %v", strings.TrimSpace(gone), offered)
		}
	}
	if !strings.Contains(got, " read ") {
		t.Fatalf("the fallback should still read, got %v", offered)
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
		func(model.Request) model.Response {
			// impatient: message the same child again before it answered
			return call("c2", "message", `{"to":"kid","text":"send it now"}`)
		},
		func(model.Request) model.Response { return text("waiting") },
		func(model.Request) model.Response { return text("got it") },
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			<-release
			return call("k", "message", `{"to":"`+parentIDFromSystem(req.System)+`","text":"one answer for both","kind":"response"}`)
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "delegate"}))
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if agents[0].State != "waiting" {
		t.Fatalf("two questions out: %+v", agents[0])
	}
	close(release)
	for te.Turn != 2 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	agents, _ = tree(ctx, h.c, s.ID)
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
			// the list comes with every request
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.TurnEnded, root)
	agents, _ = tree(ctx, h.c, s.ID)
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
	_ = errOf(rpc.Do(ctx, h2.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: evs[len(evs)-1].Seq + 1})) // live only; no replay of the old turn
	agents, _ = tree(ctx, h2.c, s.ID)
	if len(agents[0].Todos) != 2 || agents[0].Todos[0].Status != "in_progress" {
		t.Fatalf("recovered todos %+v", agents[0].Todos)
	}
	_ = errOf(rpc.Do(ctx, h2.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "more"}))
	h2.waitFor(event.TurnEnded, root)
	agents, _ = tree(ctx, h2.c, s.ID)
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
			// the task names its sender by its name, which addresses it
			if !strings.Contains(last, "[message from agent main — ") {
				t.Errorf("task should name the parent: %q", last)
			}
			// a unique id prefix still resolves, for models that use ids
			return call("k1", "message", `{"to":"`+parent[:8]+`","text":"found it","kind":"response"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || last.Content != "response delivered to main" {
				t.Errorf("prefix id should resolve: %+v", last)
			}
			return text("done")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitInput(event.InputResponse, root)
	h.waitFor(event.TurnEnded, root) // the "thanks" turn
	if fm.callCount() < 3 {
		h.waitFor(event.TurnEnded, root)
	}
}

// TestRoles: role modes gate the root, /roles and agent_create; model and
// variant whitelists bound set_model/set_variant and decide what a child
// inherits; a subagent past max_turns answers its askers with the limit.
func TestRoles(t *testing.T) {
	setupConfig(t)
	g := os.Getenv("STAVLOS_CONFIG_DIR")
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","reminders":false,"rootAgent":"lead"}`), 0o644)
	roles := filepath.Join(g, "roles")
	os.MkdirAll(roles, 0o755)
	os.WriteFile(filepath.Join(roles, "lead.md"), []byte("---\ndescription: Leads\nmode: primary\nmodels:\n  - id: fake/m1\n    variants: [high]\nspawn: [limited, boss, general]\n---\nYou lead.\n"), 0o644)
	os.WriteFile(filepath.Join(roles, "limited.md"), []byte("---\ndescription: Limited\nmode: subagent\nmodels: [fake/m2]\nmax_turns: 1\ntools:\n  shell: deny\n  apply_patch: deny\n  skill: deny\n  todo: deny\n  web_fetch: deny\n  web_search: deny\n---\nYou are limited.\n"), 0o644)
	os.WriteFile(filepath.Join(roles, "boss.md"), []byte("---\ndescription: Boss\nmode: primary\n---\nYou boss.\n"), 0o644)

	work := t.TempDir()
	// The root's second message must reach the child after its first model
	// call: one that lands before it is taken into turn 1, and the turn over
	// the limit this test waits for never comes.
	kidCalled := make(chan struct{})
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent_create", `{"archetype":"limited","label":"kid","task":"think"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.HasPrefix(last.Content, "created kid (limited), id ") {
				t.Errorf("spawn result: %+v", last)
			}
			return call("c2", "agent_create", `{"archetype":"boss","label":"b","task":"x"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if !last.IsError || !strings.Contains(last.Content, "primary-only") {
				t.Errorf("a primary-only role must not be spawnable: %+v", last)
			}
			select {
			case <-kidCalled:
			case <-time.After(10 * time.Second):
				t.Error("the child never made its first model call")
			}
			return call("c3", "message", `{"to":"kid","text":"again"}`) // by name
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
			close(kidCalled)
			if !strings.Contains(req.System, "turn 1 of at most 1") {
				t.Errorf("the child should be told its turn budget:\n%s", req.System)
			}
			return text("thinking") // turn 1 passes without an answer
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	// the root runs the configured primary role on its default variant
	if agents[0].Role != "lead" || agents[0].Model != "fake/m1" || agents[0].Variant != "high" {
		t.Fatalf("root %+v", agents[0])
	}
	// whitelists bound the switches
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetVariant, protocol.AgentSetVariantParams{Agent: root, Variant: "low"})); err == nil || !strings.Contains(err.Error(), "does not allow variant") {
		t.Fatalf("variant outside the role: %v", err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetModel, protocol.AgentSetModelParams{Agent: root, Model: "fake/m2"})); err == nil || !strings.Contains(err.Error(), "does not allow model") {
		t.Fatalf("model outside the role: %v", err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetRole, protocol.AgentSetRoleParams{Agent: root, Role: "limited"})); err == nil || !strings.Contains(err.Error(), "subagent-only") {
		t.Fatalf("subagent-only role on the main agent: %v", err)
	}

	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	var sp event.AgentSpawnedPayload
	for sp.Parent == "" { // the subscription replays the root's own spawn first
		e := h.waitFor(event.AgentSpawned, "")
		_ = e.Decode(&sp)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents) != 2 || agents[1].Role != "limited" || agents[1].Model != "fake/m2" || agents[1].Variant != "" {
		t.Fatalf("child should start on its role's default model: %+v", agents)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetRole, protocol.AgentSetRoleParams{Agent: agents[1].ID, Role: "boss"})); err == nil || !strings.Contains(err.Error(), "primary-only") {
		t.Fatalf("primary-only role on a subagent: %v", err)
	}
	// the child's second turn is over its limit: the parent is answered
	rp := h.waitInput(event.InputResponse, root)
	if !strings.Contains(rp.Text, "turn limit of 1") {
		t.Fatalf("limit response: %+v", rp)
	}
	var te event.TurnEndedPayload
	for te.Turn != 2 {
		e := h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	// switching the root to a role without a whitelist keeps its model and variant
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetRole, protocol.AgentSetRoleParams{Agent: root, Role: "general"})); err != nil {
		t.Fatal(err)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if agents[0].Role != "general" || agents[0].Model != "fake/m1" || agents[0].Variant != "high" {
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
	if sock := os.Getenv("STAVLOS_TEST_DIAL"); sock != "" {
		dialAndReport(sock)
		return
	}
	os.Exit(m.Run())
}

// dialAndReport is a child process of the test (so of its daemon) calling
// daemon.status; it prints the error code it got.
func dialAndReport(sock string) {
	c, err := rpc.Dial(sock)
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer c.Close()
	_, err = rpc.Do(context.Background(), c, protocol.DaemonStatus, protocol.None{})
	var pe *protocol.Error
	if errors.As(err, &pe) {
		fmt.Println("code", pe.Code)
		return
	}
	fmt.Println("err", err)
}

// TestProcessesTheDaemonRunsAreRefused: a process descended from the daemon
// (what an agent's shell command is) gets ErrForbidden for every request,
// while the daemon's real clients are served.
func TestProcessesTheDaemonRunsAreRefused(t *testing.T) {
	setupConfig(t)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.cancel()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "STAVLOS_TEST_DIAL="+h.sock)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), fmt.Sprint("code ", protocol.ErrForbidden)) {
		t.Fatalf("child: %v %s", err, out)
	}
	if _, err := rpc.Do(context.Background(), h.c, protocol.DaemonStatus, protocol.None{}); err != nil {
		t.Fatalf("the harness's own client was refused: %v", err)
	}
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
	cfg := fmt.Sprintf(`{"model":"fake/m1","reminders":false,"rootAgent":"mcpuser","mcp":{"echo":{"command":%q,"env":{"STAVLOS_TEST_MCP_SERVER":"1","GREETING":"${env:STAVLOS_TEST_GREETING}"}}},"policy":{"mcp__echo__*":"allow"}}`, exe)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(cfg), 0o644)
	os.MkdirAll(filepath.Join(g, "roles"), 0o755)
	os.WriteFile(filepath.Join(g, "roles", "mcpuser.md"), []byte("---\ndescription: Uses MCP\nmcp: [echo, missing]\ntools:\n  shell: deny\n  apply_patch: deny\n  skill: deny\n  todo: deny\n  web_fetch: deny\n  web_search: deny\n---\nYou use tools.\n"), 0o644)

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
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	// before the first turn the servers are listed but pending
	if len(agents[0].MCP) != 2 || agents[0].MCP[0].Name != "echo" || agents[0].MCP[0].State != "pending" {
		t.Fatalf("pending servers %+v", agents[0].MCP)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
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
	agents, _ = tree(ctx, h.c, s.ID)
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
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "one more"}))
	h.waitFor(event.TurnEnded, root)
	h.waitFor(event.MCPStopped, root)
	agents, _ = tree(ctx, h.c, s.ID)
	for _, m := range agents[0].MCP {
		if m.Name == "echo" && m.State != "pending" {
			t.Fatalf("idle stop should leave the server pending for the next turn: %+v", m)
		}
	}
	agent.MCPIdleAfter = 10 * time.Minute
	// a role without MCP: the next turn stops the server
	if err := errOf(rpc.Do(ctx, h.c, protocol.AgentSetRole, protocol.AgentSetRoleParams{Agent: root, Role: "general"})); err != nil {
		t.Fatal(err)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "again"}))
	h.waitFor(event.TurnEnded, root)
	agents, _ = tree(ctx, h.c, s.ID)
	if len(agents[0].MCP) != 0 {
		t.Fatalf("servers should be gone after the role change: %+v", agents[0].MCP)
	}
}

// TestWorkingDirectories: the channel has one working set, shared by every
// agent. Reads inside the channel directory and a directory the human added
// run without a boundary prompt; a path outside asks (naming the directory)
// even though read is allowed, and "allow_always" adds it to the channel,
// so a child created afterwards reads there without asking; the human edits
// the set; it survives a restart.
func TestWorkingDirectories(t *testing.T) {
	setupConfig(t)
	g := os.Getenv("STAVLOS_CONFIG_DIR")
	work := t.TempDir()
	shared := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(shared, "lib.txt"), []byte("lib"), 0o644)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(work, "in.txt"), []byte("in"), 0o644)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","reminders":false,"rootAgent":"lead"}`), 0o644)
	os.MkdirAll(filepath.Join(g, "roles"), 0o755)
	os.WriteFile(filepath.Join(g, "roles", "lead.md"), []byte("---\ndescription: Leads\nspawn: [general]\n---\nYou lead.\n"), 0o644)

	data := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "working directories, shared by every agent: "+work+", "+shared) {
				t.Errorf("system prompt should list the channel's directories:\n%s", req.System)
			}
			return call("c1", "read", `{"path":"in.txt"}`) // inside the channel dir
		},
		func(model.Request) model.Response { return call("c2", "read", `{"path":"`+shared+`/lib.txt"}`) }, // inside an added dir
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "lib") {
				t.Errorf("added dir read: %+v", last)
			}
			return call("c3", "read", `{"path":"`+outside+`/secret.txt"}`) // outside: asks
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1].Blocks[0]
			if last.IsError || !strings.Contains(last.Content, "s") {
				t.Errorf("outside read after allow_always: %+v", last)
			}
			return call("c4", "agent_create", `{"archetype":"general","label":"kid","task":"read the secret"}`)
		},
		func(req model.Request) model.Response {
			if last := req.Messages[len(req.Messages)-1].Blocks[0]; last.IsError {
				t.Errorf("agent_create: %+v", last)
			}
			return text("done")
		},
	}
	fm.childSteps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("k1", "read", `{"path":"`+outside+`/secret.txt"}`) }, // the channel's set: no prompt
		func(req model.Request) model.Response {
			if last := req.Messages[len(req.Messages)-1].Blocks[0]; last.IsError || !strings.Contains(last.Content, "s") {
				t.Errorf("the child reads inside the shared set: %+v", last)
			}
			return text("child idle")
		},
	}
	h := newHarness(t, data, fm)
	ctx := context.Background()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	if len(s.Dirs) != 1 || s.Dirs[0].Path != work || s.Dirs[0].Source != "channel" {
		t.Fatalf("dirs %+v", s.Dirs)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelAddDir, protocol.ChannelDirParams{Channel: s.ID, Dir: shared})); err != nil {
		t.Fatal(err)
	}
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	// the only prompt is the boundary one, and it names the directory
	e := h.waitFor(event.AskRequested, root)
	var pr event.AskRequestedPayload
	_ = e.Decode(&pr)
	if pr.Tool != "read" || !strings.Contains(pr.Question, "outside the channel's directories") || !strings.Contains(pr.Question, outside) {
		t.Fatalf("boundary prompt %+v", pr)
	}
	pending := h.pending(s.ID)
	if len(pending) != 1 || pending[0].Dir != outside {
		t.Fatalf("pending %+v", pending)
	}
	if list, _ := channels(ctx, h.c, work, false); len(list) != 1 || list[0].Permissions != 1 || list[0].Questions != 0 {
		t.Fatalf("the channel list counts the waiting permission: %+v", list)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: pending[0].ID})); err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: pending[0].ID, Answer: "allow_always"})); err != nil {
		t.Fatal(err)
	}
	e = h.waitFor(event.ChannelDirAdded, root)
	var dp event.DirPayload
	_ = e.Decode(&dp)
	if dp.Dir != outside || dp.Source != "human" {
		t.Fatalf("dir added %+v", dp)
	}
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
	// the child reads the added directory without a prompt of its own
	deadline := time.Now().Add(10 * time.Second)
	var kid protocol.AgentInfo
	for kid.Turn < 1 || kid.State != protocol.AgentIdle {
		if time.Now().After(deadline) {
			t.Fatalf("the child should finish its read without a prompt: %+v, pending %+v", kid, h.d.esc.Pending(s.ID))
		}
		time.Sleep(20 * time.Millisecond)
		agents, _ = tree(ctx, h.c, s.ID)
		for _, a := range agents {
			if a.Name == "kid" {
				kid = a
			}
		}
	}
	// the human edits the set: add, remove (the channel directory refuses),
	// and a relative path inside the channel directory is already covered
	extra := t.TempDir()
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelAddDir, protocol.ChannelDirParams{Channel: s.ID, Dir: extra})); err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelAddDir, protocol.ChannelDirParams{Channel: s.ID, Dir: "sub/dir"})); err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelRemoveDir, protocol.ChannelDirParams{Channel: s.ID, Dir: shared})); err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelRemoveDir, protocol.ChannelDirParams{Channel: s.ID, Dir: work})); err == nil || !strings.Contains(err.Error(), "channel directory") {
		t.Fatalf("removing the channel directory: %v", err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelRemoveDir, protocol.ChannelDirParams{Channel: s.ID, Dir: "/never/there"})); err == nil {
		t.Fatal("removing an unknown directory should fail")
	}
	dirsOf := func(c *rpc.Client) string {
		ss, _ := channels(ctx, c, work, false)
		for _, x := range ss {
			if x.ID == s.ID {
				var out []string
				for _, d := range x.Dirs {
					out = append(out, d.Path+":"+d.Source)
				}
				return strings.Join(out, " ")
			}
		}
		return ""
	}
	want := work + ":channel " + outside + ":human " + extra + ":human"
	if got := dirsOf(h.c); got != want {
		t.Fatalf("edited dirs: %s", got)
	}
	h.close()

	// restart: the set comes back
	h2 := newHarness(t, data, &fakeModel{})
	defer h2.close()
	_, _ = tree(ctx, h2.c, s.ID)
	if got := dirsOf(h2.c); got != want {
		t.Fatalf("recovered dirs: %s", got)
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.AskRequested, root)
	pending := h.pending(s.ID)
	if len(pending) != 1 || pending[0].Dir != filepath.Join(outside, "sub") {
		t.Fatalf("offered dir %+v", pending)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: pending[0].ID}))
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: pending[0].ID, Answer: "allow_always", Dir: outside})); err != nil {
		t.Fatal(err)
	}
	e := h.waitFor(event.ChannelDirAdded, root)
	var dp event.DirPayload
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.AskRequested, root)
	p := h.pending(s.ID)[0]
	_ = errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID}))
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerDeny, Reason: "use apply_patch instead"})); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.AskRequested, root)
	p = h.pending(s.ID)[0]
	_ = errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID}))
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerDeny, Reason: ""})); err != nil {
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.AskRequested, root)
	p := h.pending(s.ID)[0]
	_ = errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID}))
	if p.Prefix != "touch" {
		t.Fatalf("prompt prefix %q", p.Prefix)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerAllowPrefix})); err != nil {
		t.Fatal(err)
	}
	// the second echo runs without a prompt; the chained one asks
	h.waitFor(event.AskRequested, root)
	p = h.pending(s.ID)[0]
	if !strings.Contains(string(p.Input), "touch three; touch four") {
		t.Fatalf("second prompt should be the chained command: %s", p.Input)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID}))
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerDeny, Reason: ""})); err != nil {
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

// TestManualCompact: /compact on an idle agent summarises every completed
// turn with the model and logs a Compacted event; the next turn's request
// starts from the summary. Mid-turn it is queued and runs before the next
// model call.
// TestWebSearchAlwaysOffered: web_search is offered with or without a
// configured backend (the keyless Exa fallback covers the latter), and a
// configured key is read from the environment.
func TestWebSearchAlwaysOffered(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	offered := func(req model.Request) bool {
		for _, d := range req.Tools {
			if d.Name == "web_search" {
				return true
			}
		}
		return false
	}
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !offered(req) || !strings.Contains(req.System, "web_search returns titles") {
				t.Error("web_search should be offered without a backend (keyless fallback)")
			}
			return text("ok")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: agents[0].ID, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.TurnEnded, agents[0].ID)

	t.Setenv("STAVLOS_TEST_SEARCH_KEY", "k-123")
	setupConfig(t)
	g := os.Getenv("STAVLOS_CONFIG_DIR")
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","reminders":false,"search":{"provider":"Brave","apiKey":"${env:STAVLOS_TEST_SEARCH_KEY}"}}`), 0o644)
	fm2 := &fakeModel{}
	fm2.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !offered(req) {
				t.Error("web_search should be offered with a backend configured")
			}
			return text("ok")
		},
	}
	h2 := newHarness(t, t.TempDir(), fm2)
	defer h2.close()
	s2, _ := rpc.Do(ctx, h2.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h2.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s2.ID, From: 0}))
	agents, _ = tree(ctx, h2.c, s2.ID)
	_ = errOf(rpc.Do(ctx, h2.c, protocol.AgentSend, protocol.AgentSendParams{Agent: agents[0].ID, Kind: protocol.KindPrompt, Text: "go"}))
	h2.waitFor(event.TurnEnded, agents[0].ID)
}

func TestManualCompact(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return text("first answer") },
		func(model.Request) model.Response { return text("second answer") },
		// the compaction request itself
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "summarise") || !strings.Contains(req.Messages[0].Blocks[0].Text, "second answer") {
				t.Errorf("compaction request: %+v", req)
			}
			return text("SUMMARY: two answers given")
		},
		// turn 3 starts from the summary
		func(req model.Request) model.Response {
			if len(req.Messages) < 3 || !strings.Contains(req.Messages[0].Blocks[0].Text, "SUMMARY: two answers given") || strings.Contains(req.Messages[0].Blocks[0].Text, "first answer") {
				t.Errorf("turn after compaction should start from the summary: %+v", req.Messages)
			}
			return text("third answer")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	if _, err := func() (string, error) {
		r, err := rpc.Do(ctx, h.c, protocol.AgentCompact, protocol.AgentCompactParams{Agent: root})
		return r.Status, err
	}(); err == nil {
		t.Fatal("nothing to compact before any turn")
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "one"}))
	h.waitFor(event.TurnEnded, root)
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "two"}))
	h.waitFor(event.TurnEnded, root)
	if agents, _ = tree(ctx, h.c, s.ID); agents[0].Context <= 0 {
		t.Fatalf("the tree should carry the context estimate after a turn: %+v", agents[0])
	}
	status, err := func() (string, error) {
		r, err := rpc.Do(ctx, h.c, protocol.AgentCompact, protocol.AgentCompactParams{Agent: root})
		return r.Status, err
	}()
	if err != nil || status != "compacted" {
		t.Fatalf("compact: %q %v", status, err)
	}
	e := h.waitFor(event.CompactionDone, root)
	var cp event.CompactionPayload
	_ = e.Decode(&cp)
	if !strings.Contains(cp.Summary, "SUMMARY") || cp.ToSeq >= e.Seq || cp.FromSeq == 0 || cp.Before <= 0 || cp.After <= 0 {
		t.Fatalf("compacted payload: %+v", cp)
	}
	evs, _ := h.d.Log.Read(ctx, s.ID, 1, 0)
	started := false
	for _, ev := range evs {
		if ev.Type == event.CompactionStarted && ev.Agent == root {
			started = true
		}
	}
	if !started {
		t.Fatal("compaction.started should precede compacted")
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "three"}))
	h.waitFor(event.TurnEnded, root)
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
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	e := h.waitFor(event.AskRequested, root)
	var pr event.AskRequestedPayload
	_ = e.Decode(&pr)
	if pr.Kind != "question" || pr.Tool != "ask_user" || !strings.Contains(pr.Question, "Which backend?") {
		t.Fatalf("prompt %+v", pr)
	}
	agents, _ = tree(ctx, h.c, s.ID)
	if agents[0].State != "blocked" {
		t.Fatalf("an asking agent is blocked: %+v", agents[0].State)
	}
	p := h.pending(s.ID)[0]
	if len(p.Questions) != 2 || p.Questions[0].Options[0].Label != "Postgres" {
		t.Fatalf("pending %+v", p)
	}
	_ = errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID}))
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerAnswered, Answers: []string{"Postgres", "stavlos"}})); err != nil {
		t.Fatal(err)
	}
	// the second question is cancelled instead of answered
	h.waitFor(event.AskRequested, root)
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindCancel, Text: ""}))
	for {
		var r event.AskResolvedPayload
		if _ = h.waitFor(event.AskResolved, root).Decode(&r); r.Outcome == event.AskWithdrawn {
			break
		}
	}
	var te event.TurnEndedPayload
	for te.Turn != 1 {
		e = h.waitFor(event.TurnEnded, root)
		_ = e.Decode(&te)
	}
}

// TestTrustReplyChecksTheHash: trust.reply names a directory and the hash
// the client was shown; the daemon recomputes the hash from disk, so a
// stale or invented one trusts nothing, and the directory is normalised.
func TestTrustReplyChecksTheHash(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	_ = os.MkdirAll(filepath.Join(work, ".stavlos"), 0o755)
	os.WriteFile(filepath.Join(work, ".stavlos", "stavlos.json"), []byte(`{"policy":{"shell":{"curl*":"deny"}}}`), 0o644)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	st, err := rpc.Do(ctx, h.c, protocol.TrustStatus, protocol.TrustStatusParams{Dir: work})
	if err != nil || !st.Pending || st.Hash == "" {
		t.Fatalf("status %+v %v", st, err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.TrustReply, protocol.TrustReplyParams{Dir: work, Hash: "stale", Trust: true})); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a stale hash should be refused: %v", err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.TrustReply, protocol.TrustReplyParams{Dir: t.TempDir(), Hash: st.Hash, Trust: true})); err == nil {
		t.Fatal("a directory without project config should be refused")
	}
	if st2, _ := rpc.Do(ctx, h.c, protocol.TrustStatus, protocol.TrustStatusParams{Dir: work}); !st2.Pending {
		t.Fatal("nothing should be trusted yet")
	}
	// The right hash, through an unnormalised path, trusts the directory.
	if err := errOf(rpc.Do(ctx, h.c, protocol.TrustReply, protocol.TrustReplyParams{Dir: work + "/./", Hash: st.Hash, Trust: true})); err != nil {
		t.Fatal(err)
	}
	if st3, _ := rpc.Do(ctx, h.c, protocol.TrustStatus, protocol.TrustStatusParams{Dir: work}); st3.Pending {
		t.Fatal("should be trusted now")
	}
}

// TestOneDaemonPerDataDir: the data directory is locked for the daemon's
// life; a second daemon on it fails at New, and Close releases it.
func TestOneDaemonPerDataDir(t *testing.T) {
	setupConfig(t)
	data := t.TempDir()
	cat, _ := modelsdev.Parse([]byte(`{"fake":{"id":"fake","env":[],"models":{}}}`))
	ctx := context.Background()
	d1, err := New(ctx, data, registry.New(cat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, data, registry.New(cat)); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second daemon: %v", err)
	}
	d1.Close()
	d2, err := New(ctx, data, registry.New(cat))
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	d2.Close()
}

// collect drains a client's notifications into a list of events, until
// stop is closed.
func collect(c *rpc.Client, stop <-chan struct{}) (func() []event.Event, func()) {
	var mu sync.Mutex
	var got []event.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case n := <-c.Notifications:
				if n.Method == protocol.NEvent {
					var en protocol.EventNotification
					if json.Unmarshal(n.Params, &en) == nil {
						mu.Lock()
						got = append(got, en.Event)
						mu.Unlock()
					}
				}
			case <-stop:
				return
			}
		}
	}()
	return func() []event.Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]event.Event(nil), got...)
		}, func() {
			<-done
		}
}

func attached(t *testing.T, sock string) *rpc.Client {
	t.Helper()
	c, err := rpc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.Do(context.Background(), c, protocol.Attach, protocol.AttachParams{Client: "extra", Tier: protocol.TierInteractive}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestSlowClientDoesNotStallTheDaemon: a client that stops reading fills
// its socket; the daemon keeps appending, keeps delivering to the others,
// and drops the stalled one.
func TestSlowClientDoesNotStallTheDaemon(t *testing.T) {
	setupConfig(t)
	old := writeTimeout
	writeTimeout = 300 * time.Millisecond
	defer func() { writeTimeout = old }()
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir(), Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	// The stalled client is a raw connection that attaches, subscribes and
	// never reads: its socket buffer fills after a few events.
	stalled, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if _, err := stalled.Write([]byte(`{"jsonrpc":"2.0","v":1,"id":1,"method":"attach","params":{"client":"stalled","tier":"interactive"}}` + "\n" +
		`{"jsonrpc":"2.0","v":1,"id":2,"method":"subscribe","params":{"channel":"` + s.ID + `","from":1}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	waitSubs := time.Now().Add(5 * time.Second)
	for len(h.d.clientList()) < 2 && time.Now().Before(waitSubs) {
		time.Sleep(10 * time.Millisecond)
	}
	good := attached(t, h.sock)
	stop := make(chan struct{})
	events, wait := collect(good, stop)
	if err := errOf(rpc.Do(ctx, good, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0})); err != nil {
		t.Fatal(err)
	}
	// 600 events of 8 KB overflow the stalled socket many times over.
	// Before the outbound queue the appends blocked for good on the first
	// full socket; now they finish.
	payload := event.MustPayload(event.Input{Text: strings.Repeat("x", 8000)})
	var last event.Event
	appended := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < 600 && err == nil; i++ {
			var out []event.Event
			if out, err = h.d.Append(ctx, event.Event{Channel: s.ID, Type: "test.noise", Payload: payload}); err == nil {
				last = out[0]
			}
		}
		appended <- err
	}()
	select {
	case err := <-appended:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("appends never finished: a stalled client blocked the daemon")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		evs := events()
		if len(evs) > 0 && evs[len(evs)-1].Seq == last.Seq {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if evs := events(); len(evs) == 0 || evs[len(evs)-1].Seq != last.Seq {
		t.Fatalf("the reading client did not get everything: %d events", len(evs))
	}
	for time.Now().Before(deadline) && len(h.d.clientList()) > 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(h.d.clientList()); n != 2 {
		t.Fatalf("the stalled client should have been dropped: %d clients", n)
	}
	close(stop)
	wait()
}

// TestSubscribeHandoverIsContiguous: subscribing while events are being
// appended delivers every seq exactly once, in order.
func TestSubscribeHandoverIsContiguous(t *testing.T) {
	setupConfig(t)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir(), Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 5; round++ {
		c := attached(t, h.sock)
		stop := make(chan struct{})
		events, wait := collect(c, stop)
		appended := make(chan int64, 1)
		go func() {
			var last event.Event
			for i := 0; i < 400; i++ {
				if out, err := h.d.Append(ctx, event.Event{Channel: s.ID, Type: "test.noise"}); err == nil {
					last = out[0]
				}
			}
			appended <- last.Seq
		}()
		time.Sleep(time.Duration(round) * time.Millisecond)
		if err := errOf(rpc.Do(ctx, c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0})); err != nil {
			t.Fatal(err)
		}
		final := <-appended
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			evs := events()
			if len(evs) > 0 && evs[len(evs)-1].Seq >= final {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(stop)
		wait()
		evs := events()
		for i, e := range evs {
			if e.Seq != int64(i+1) {
				t.Fatalf("round %d: event %d has seq %d (gap or duplicate)", round, i, e.Seq)
			}
		}
		if len(evs) == 0 || evs[len(evs)-1].Seq != final {
			t.Fatalf("round %d: got %d events, last appended %d", round, len(evs), final)
		}
	}
}

// TestWireErrors pins the envelope version check and the error codes the
// handler table maps to.
func TestWireErrors(t *testing.T) {
	setupConfig(t)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	raw, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rd := bufio.NewReader(raw)
	ask := func(line string) protocol.Response {
		t.Helper()
		if _, err := raw.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		b, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var r protocol.Response
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		return r
	}
	cases := []struct {
		line string
		code int
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"daemon.status"}`, protocol.ErrVersion},
		{`{"jsonrpc":"2.0","id":2,"v":1,"method":"no.such"}`, protocol.ErrMethodNotFound},
		{`{"jsonrpc":"2.0","id":3,"v":1,"method":"channel.resume","params":{"channel":7}}`, protocol.ErrInvalidParams},
		{`{"jsonrpc":"2.0","id":4,"v":1,"method":"channel.resume","params":{"channel":"s-missing"}}`, protocol.ErrNotFound},
		{`{"jsonrpc":"2.0","id":5,"v":1,"method":"agent.send","params":{"agent":"a-missing","kind":"prompt"}}`, protocol.ErrNotFound},
		{`{"jsonrpc":"2.0","id":6,"v":1,"method":"prompt.reply","params":{"id":"p-missing","answer":"allow"}}`, protocol.ErrConflict},
		{`{"jsonrpc":"2.0","id":7,"v":1,"method":"channel.create","params":{"dir":"/definitely/not/here"}}`, protocol.ErrInvalidParams},
	}
	for _, c := range cases {
		r := ask(c.line)
		if r.Error == nil || r.Error.Code != c.code {
			t.Errorf("%s: got %+v, want code %d", c.line, r.Error, c.code)
		}
	}
	if r := ask(`{"jsonrpc":"2.0","id":8,"v":1,"method":"daemon.status"}`); r.Error != nil || len(r.Result) == 0 {
		t.Fatalf("status with the version: %+v", r)
	}
	if r := ask(`{"jsonrpc":"2.0","id":9,"v":1,"method":"channel.resume","params":{"channel":"s-missing"}}`); r.Error == nil || r.Error.Message != `channel "s-missing" not found` {
		t.Fatalf("message: %+v", r.Error)
	}
}

// TestChannelPost: a chat message with no mention reaches the root, is
// logged once on the channel with who it went to, and a mention of no agent
// is refused.
func TestChannelPost(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	to, err := func() ([]string, error) {
		r, err := rpc.Do(ctx, h.c, protocol.ChannelPost, protocol.ChannelPostParams{Channel: s.ID, Text: "hello there"})
		return r.To, err
	}()
	if err != nil || len(to) != 1 || to[0] != "main" {
		t.Fatalf("to %v err %v", to, err)
	}
	e := h.waitFor(event.ChatPosted, "")
	var p event.ChatPayload
	if _ = e.Decode(&p); p.Text != "hello there" || e.Agent != "" || len(p.To) != 1 {
		t.Fatalf("chat.posted %+v on %q", p, e.Agent)
	}
	h.waitFor(event.TurnEnded, root)
	if _, err := func() ([]string, error) {
		r, err := rpc.Do(ctx, h.c, protocol.ChannelPost, protocol.ChannelPostParams{Channel: s.ID, Text: "@nobody hi"})
		return r.To, err
	}(); err == nil || !strings.Contains(err.Error(), "@nobody") {
		t.Fatalf("an unknown mention should be refused: %v", err)
	}
}

// TestAutoDeniesAWaitingBoundaryPrompt: a boundary prompt waiting in ask
// mode is denied, with the auto mode note, when the channel switches to
// auto.
func TestAutoDeniesAWaitingBoundaryPrompt(t *testing.T) {
	setupConfig(t)
	work, outside := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "f.txt"), []byte("secret"), 0o644)
	told := make(chan string, 1)
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c1", "read", `{"path":"`+outside+`/f.txt"}`) },
		func(req model.Request) model.Response {
			told <- req.Messages[len(req.Messages)-1].Blocks[0].Content
			return text("done")
		},
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work, Model: "", RootAgent: ""})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: agents[0].ID, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.AskRequested, agents[0].ID) // the outside read waits in ask mode
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: "auto"})); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-told:
		if !strings.Contains(got, "auto mode") || strings.Contains(got, "secret") {
			t.Fatalf("the waiting boundary prompt should be denied by auto: %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was never told")
	}
}

// TestChannelNames: a channel is named after its directory, unique across
// the daemon (a second one gets -2); a rename is normalised and refused when
// it leaves nothing or another channel has the name; names survive a
// restart.
func TestChannelNames(t *testing.T) {
	setupConfig(t)
	data := t.TempDir()
	h := newHarness(t, data, &fakeModel{})
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "Proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: dir, Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	b, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: dir, Model: "", RootAgent: ""})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "proj" || b.Name != "proj-2" {
		t.Fatalf("names %q %q", a.Name, b.Name)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelRename, protocol.ChannelRenameParams{Channel: b.ID, Name: "#Docs Site"})); err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelRename, protocol.ChannelRenameParams{Channel: a.ID, Name: "docs-site"})); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("a taken name: %v", err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelRename, protocol.ChannelRenameParams{Channel: a.ID, Name: "!!"})); err == nil {
		t.Fatal("a name with nothing left should be refused")
	}
	// a channel created under a chosen name keeps it, normalised; a taken
	// one is refused before anything starts
	if n, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: dir, Name: "Site Ops"}); err != nil || n.Name != "site-ops" {
		t.Fatalf("named create: %+v %v", n, err)
	}
	if _, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: dir, Name: "#proj"}); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("a taken name on create: %v", err)
	}
	names := func(c *rpc.Client) map[string]bool {
		ss, _ := channels(ctx, c, dir, false)
		out := map[string]bool{}
		for _, s := range ss {
			out[s.Name] = true
		}
		return out
	}
	if got := names(h.c); len(got) != 3 || !got["proj"] || !got["docs-site"] || !got["site-ops"] {
		t.Fatalf("names %v", got)
	}
	h.close()
	h2 := newHarness(t, data, &fakeModel{})
	defer h2.close()
	if got := names(h2.c); len(got) != 3 || !got["proj"] || !got["docs-site"] || !got["site-ops"] {
		t.Fatalf("names after a restart %v", got)
	}
}

// errOf is the error of a call whose result the test does not need.
func errOf[R any](_ R, err error) error { return err }

func tree(ctx context.Context, c *rpc.Client, channel string) ([]protocol.AgentInfo, error) {
	r, err := rpc.Do(ctx, c, protocol.AgentTree, protocol.AgentTreeParams{Channel: channel})
	return r.Agents, err
}

func channels(ctx context.Context, c *rpc.Client, dir string, archived bool) ([]protocol.ChannelInfo, error) {
	r, err := rpc.Do(ctx, c, protocol.ChannelList, protocol.ChannelListParams{Dir: dir, IncludeArchived: archived})
	return r.Channels, err
}

func prompts(ctx context.Context, c *rpc.Client, channel string) ([]protocol.PromptInfo, error) {
	r, err := rpc.Do(ctx, c, protocol.PromptList, protocol.PromptListParams{Channel: channel})
	return r.Prompts, err
}

// pending lists a channel's open prompts, waiting (up to ten seconds) until
// there is one: an agent logs ask.requested before the prompt reaches the
// escalation manager, so an event alone does not mean the prompt is listed.
func (h *harness) pending(channel string) []protocol.PromptInfo {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if p := h.d.esc.Pending(channel); len(p) > 0 || time.Now().After(deadline) {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
}
