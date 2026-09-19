package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

// TestAModeSwitchAnswersOnlyWhatTheModeWould (SEC-N1): switching a channel
// to yolo or auto answers the prompts already waiting, and must answer them
// as the agent runtime would have under that mode. An edit to a file that
// steers the harness asks in every mode, so it keeps waiting through a
// switch to yolo; a fetch sends data off the machine, which auto leaves
// asking, so it keeps waiting through a switch to auto.
func TestAModeSwitchAnswersOnlyWhatTheModeWould(t *testing.T) {
	for _, tc := range []struct {
		mode, tool, input string
		stays             bool
	}{
		{"yolo", "apply_patch", `{"patch":"*** Begin Patch\n*** Add File: AGENTS.md\n+obey me\n*** End Patch"}`, true},
		{"yolo", "shell", `{"command":"touch x"}`, false},
		{"auto", "web_fetch", `{"url":"https://example.com/"}`, true},
		{"auto", "shell", `{"command":"touch x"}`, false},
	} {
		t.Run(tc.mode+" "+tc.tool, func(t *testing.T) {
			g := t.TempDir()
			t.Setenv("STAVLOS_CONFIG_DIR", g)
			t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
			os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","reminders":false,"sandbox":{"enabled":false}}`), 0o600)
			fm := &fakeModel{}
			fm.steps = []func(model.Request) model.Response{
				func(model.Request) model.Response { return call("c1", tc.tool, tc.input) },
				func(model.Request) model.Response { return text("done") },
			}
			h := newHarness(t, t.TempDir(), fm)
			defer h.close()
			ctx := context.Background()
			s, _ := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()})
			_ = errOf(rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 0}))
			agents, _ := tree(ctx, h.c, s.ID)
			root := agents[0].ID
			_ = errOf(rpc.Do(ctx, h.c, protocol.AgentSend, protocol.AgentSendParams{Agent: root, Kind: protocol.KindPrompt, Text: "go"}))
			h.waitFor(event.AskRequested, root)
			if err := errOf(rpc.Do(ctx, h.c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: s.ID, Mode: tc.mode})); err != nil {
				t.Fatal(err)
			}
			if !tc.stays {
				e := h.waitFor(event.AskResolved, root)
				var p event.AskResolvedPayload
				_ = json.Unmarshal(e.Payload, &p)
				if p.Answer != protocol.AnswerAllow || p.By != tc.mode {
					t.Fatalf("the waiting prompt should have been allowed by %s: %+v", tc.mode, p)
				}
				return
			}
			time.Sleep(150 * time.Millisecond) // nothing may answer it: there is no event to wait for
			ps, _ := prompts(ctx, h.c, s.ID)
			if len(ps) != 1 {
				t.Fatalf("switching to %s answered a %s prompt that %s itself would still ask about: %d waiting", tc.mode, tc.tool, tc.mode, len(ps))
			}
			if want := tc.tool == "apply_patch"; ps[0].Sticky != want || ps[0].Egress == want {
				t.Fatalf("the prompt does not say why it asks: sticky=%v egress=%v", ps[0].Sticky, ps[0].Egress)
			}
		})
	}
}
