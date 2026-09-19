package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

// TestReplayDoesNotCostAClientItsConnection (#39): replaying a channel with
// more events than the client's queue holds keeps that queue full for as
// long as the replay runs, because history waits for room. A reply or a
// prompt sent meanwhile must wait its turn too, not find the queue full and
// have the client dropped as "not reading" while it is reading as fast as it
// can.
func TestReplayDoesNotCostAClientItsConnection(t *testing.T) {
	old := outQueue
	outQueue = 64
	defer func() { outQueue = old }()
	setupConfig(t)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const history = 2000
	var last int64
	for i := 0; i < history; i += 100 {
		batch := make([]event.Event, 100)
		for j := range batch {
			batch[j] = event.Event{Channel: s.ID, Type: "test.noise", Payload: json.RawMessage(`{"pad":"` + fmt.Sprintf("%0200d", i+j) + `"}`)}
		}
		out, err := h.d.Append(ctx, batch...)
		if err != nil {
			t.Fatal(err)
		}
		last = out[len(out)-1].Seq
	}

	nc, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	send := func(id int, method string, params any) {
		p, _ := json.Marshal(params)
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "v": protocol.Version, "id": id, "method": method, "params": json.RawMessage(p)})
		_, _ = nc.Write(append(b, '\n')) // a dropped connection shows below, as replies that never come
	}
	send(1, protocol.MAttach, protocol.AttachParams{Client: "slowish", Tier: protocol.TierInteractive})
	send(2, protocol.MSubscribe, protocol.SubscribeParams{Channel: s.ID, From: 0})
	// what a TUI does while its channel replays: it keeps asking things
	const asks = 20
	go func() {
		for i := 0; i < asks; i++ {
			send(100+i, protocol.MDaemonStatus, struct{}{})
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// a reader that is reading, only not infinitely fast
	_ = nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	replies, events := 0, int64(0)
	for (replies < asks+2 || events < last) && sc.Scan() {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  *protocol.Error `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			break // a line cut short: the connection was closed under us
		}
		switch {
		case msg.ID != nil:
			if msg.Error != nil {
				t.Fatalf("request %d: %+v", *msg.ID, msg.Error)
			}
			replies++
		case msg.Method == protocol.NEvent:
			var en protocol.EventNotification
			_ = json.Unmarshal(msg.Params, &en)
			if en.Event.Seq != events+1 {
				t.Fatalf("event %d arrived after %d: history out of order", en.Event.Seq, events)
			}
			events = en.Event.Seq
		}
		if events%10 == 0 {
			time.Sleep(time.Millisecond) // a TUI parsing and drawing: slower than SQLite reads history
		}
	}
	if replies < asks+2 || events < last {
		t.Fatalf("the client was dropped mid-replay: %d of %d replies, %d of %d events (%v)", replies, asks+2, events, last, sc.Err())
	}
}

// TestADroppedClientsClaimIsReleased (#38): a client that claimed a prompt
// and went away holds it for nobody. The next client, which may be the same
// human's TUI reconnecting under a new id, can claim and answer at once, not
// after the two-minute claim expiry.
func TestADroppedClientsClaimIsReleased(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	fm := &fakeModel{}
	fm.steps = []func(model.Request) model.Response{
		func(model.Request) model.Response { return call("c1", "shell", `{"command":"ls -la"}`) },
		func(model.Request) model.Response { return text("ok") },
	}
	h := newHarness(t, t.TempDir(), fm)
	defer h.close()
	ctx := context.Background()
	s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work})
	_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
	agents, _ := tree(ctx, h.c, s.ID)
	root := agents[0].ID
	_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
	h.waitFor(event.AskRequested, root)
	p := h.pending(s.ID)[0]

	first := attached(t, h.sock)
	if err := errOf(rpc.Do(ctx, first, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID})); err != nil {
		t.Fatal(err)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID})); err == nil {
		t.Fatal("two clients hold one claim")
	}
	first.Close() // the TUI is gone, however it went

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := errOf(rpc.Do(ctx, h.c, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID}))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the prompt is still held by a client that is gone: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := errOf(rpc.Do(ctx, h.c, protocol.PromptReply, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerAllow})); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.TurnEnded, root)
}
