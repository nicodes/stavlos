package tui

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// The behaviour lock (docs/ground-up-refactor.md 0.1). Each scene is a whole
// frame of the TUI, as a person sees it, for a scripted state; the frames are
// kept in testdata/golden. The refactor may change everything under them and
// nothing in them: a step that changes a golden file has changed what the
// user sees, which is either a bug or a decision somebody made on purpose.
//
//	go test ./internal/tui -run TestGoldenFrames -update
//
// rewrites them, and the diff of testdata/golden is then the review.

// goldenNow is the moment every golden frame is drawn at.
var goldenNow = time.Date(2026, 9, 19, 12, 0, 0, 0, time.Local)

var updateGolden = flag.Bool("update", false, "rewrite the golden frames in testdata/golden")

func golden(t *testing.T, name, got string) {
	t.Helper()
	// trailing blanks carry nothing a person sees, and would make every
	// frame's diff noisy
	lines := strings.Split(stripANSI(got), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	got = strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "golden", name+".txt")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("the frame %q changed: what the user sees is different.\n--- want\n%s--- got\n%s", name, want, got)
	}
}

// goldenChannel is a channel with a main agent that delegated, ran a command
// that needed permission, was denied one, and answered the human; a child
// still working; a sheet; and a question waiting.
func goldenChannel(t *testing.T) Model {
	t.Helper()
	prev := clock
	clock = func() time.Time { return goldenNow }
	t.Cleanup(func() { clock = prev })
	m := newModel(context.Background(), nil, "s")
	m.width, m.height = 110, 34
	m.reconciled, m.loading, m.showTree = true, false, true
	m.superChat, m.opened = true, true // as Run starts it: the channel chat is the default view
	m.channel.Name, m.channel.Mode, m.channel.Dir = "proj", "ask", "/home/x/Work/proj"
	m.agents = []protocol.AgentInfo{
		{ID: "a", Name: "main", Role: "general", Model: "openai/gpt-5.6-terra", Variant: "high", State: "waiting", Awaiting: []string{"b"}, CostUSD: 0.20, Tokens: 1200, Context: 62_000, ContextWindow: 200_000},
		{ID: "b", Parent: "a", Depth: 1, Name: "scout", Role: "general", Model: "zai/glm-5.3", State: "running", CostUSD: 0.05, Tokens: 300},
	}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	seq := int64(0)
	ev := func(agent string, typ event.Type, payload any) event.Event {
		seq++
		e := mk(seq, agent, typ, payload)
		e.Channel = "s"                                             // the model's channel: events of another are dropped
		e.Time = goldenNow.Add(time.Duration(seq-20) * time.Minute) // a few minutes apart, all before now
		return e
	}
	feed(func(e event.Event) { m.applyEvent(e) },
		[]event.Event{
			ev("", event.ChatPosted, event.ChatPayload{ID: "p1", From: "human:tui:1", Text: "find out why the build is slow", To: []string{"main"}}),
			ev("a", event.InputQueued, event.Input{ID: "i1", Kind: event.InputSteer, Text: "find out why the build is slow", Post: "p1", RequestID: "r1"}),
			ev("a", event.TurnStarted, event.TurnPayload{Turn: 1}),
			ev("a", event.InputTaken, event.InputTakenPayload{Turn: 1, IDs: []string{"i1"}}),
			ev("a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{
				{Type: model.BlockText, Text: "I'll time the build first."},
				{Type: model.BlockToolUse, ID: "c1", Name: "shell", Input: raw(map[string]string{"command": "go build -x ./... 2>&1 | tail -5"})}}}),
			ev("a", event.ToolStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell"}),
			ev("a", event.ToolFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "real 41.2s\nlinking 38.0s\n"}),
			ev("a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{
				{Type: model.BlockToolUse, ID: "c2", Name: "shell", Input: raw(map[string]string{"command": "rm -rf ~/.cache/go-build"})}}}),
			ev("a", event.ToolStarted, event.ToolStartedPayload{Turn: 1, CallID: "c2", Name: "shell"}),
			ev("a", event.ToolFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c2", Name: "shell", Output: "Denied by policy: shell rm -rf ~/.cache/go-build", IsError: true, Denied: true}),
			ev("a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b", Parent: "a", Role: "general", Name: "scout", Model: "zai/glm-5.3", Depth: 1}),
			ev("a", event.ChatMessage, event.ChatPayload{From: "main", Kind: "response", ReplyTo: []string{"r1"}, Text: "Linking takes 38 of the 41 seconds. I've asked **scout** to look at the linker flags."}),
			ev("a", event.AgentUpdated, event.AgentUpdatedPayload{Model: event.Str("zai/glm-5.3"), Reason: "openai is at its limit until 14:10; zai: 12% of its week used with 40% of it gone"}),
			ev("a", event.SheetWritten, event.SheetPayload{ID: "s1", Title: "Build timings", Author: "main", Hash: "abc", Size: 2048}),
			ev("a", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: event.ReasonEndTurn}),
		})
	m.layout()
	m.refreshViewport()
	return m
}

func TestGoldenFrames(t *testing.T) {
	now := goldenNow
	t.Run("channel chat with the nav", func(t *testing.T) {
		m := goldenChannel(t)
		golden(t, "channel-chat", m.View())
	})
	t.Run("an agent's own chat", func(t *testing.T) {
		m := goldenChannel(t)
		m.superChat, m.selected = false, 0
		m.layout()
		m.refreshViewport()
		golden(t, "agent-chat", m.View())
	})
	t.Run("the nav's sections", func(t *testing.T) {
		m := goldenChannel(t)
		m.onWeb(webMsg{action: "status", status: protocol.WebStatus{Enabled: true, URL: "http://127.0.0.1:4999/"}})
		m.onDiscord(discordMsg{epoch: m.discordEpoch, status: protocol.DiscordStatus{Configured: true, Enabled: true, State: "connected"}})
		m.plans = []protocol.PlanUsageInfo{
			{Provider: "openai", Name: "ChatGPT", Observed: now, Windows: []protocol.UsageWindowInfo{{UsedPercent: 38, Minutes: 300, ResetsAt: now.Add(2 * time.Hour)}, {UsedPercent: 100, Minutes: 10080, ResetsAt: now.Add(72 * time.Hour)}}},
			{Provider: "xai", Name: "Grok", Observed: now, Windows: []protocol.UsageWindowInfo{{UsedPercent: 71, Minutes: 10080, ResetsAt: now.Add(24 * time.Hour)}}},
		}
		m.channel.TrustFiles = 3
		m.cache = protocol.CacheUsageResult{Fresh: 40, Cached: 960, Providers: []protocol.CacheProviderUsage{{Provider: "xai", Fresh: 30_000_000, Cached: 970_000_000}, {Provider: "openai", Fresh: 9_800_000, Cached: 200_000}}}
		m.layout()
		golden(t, "nav-sections", strings.Join(m.sidebarHeader(sidebarWidth-1), "\n"))
	})
	t.Run("the web UI's controls", func(t *testing.T) {
		m := goldenChannel(t)
		m.onWeb(webMsg{action: "status", status: protocol.WebStatus{Enabled: true, URL: "http://127.0.0.1:4999/"}})
		m.openWeb("")
		m.layout()
		m.refreshViewport()
		golden(t, "overlay-web", m.View())
	})
	t.Run("the command palette", func(t *testing.T) {
		m := goldenChannel(t)
		m.setFocus(focusInput)
		for _, r := range "/mo" {
			m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		m.layout()
		m.refreshViewport()
		golden(t, "palette", m.View())
	})
	// Every dialog and card, so that a change to how the screen is built (the
	// sub-models, the keymap) is held to what each of them looked like.
	draw := func(t *testing.T, name string, prepare func(m *Model)) {
		t.Run(name, func(t *testing.T) {
			m := goldenChannel(t)
			prepare(&m)
			m.layout()
			m.refreshViewport()
			golden(t, name, m.View())
		})
	}
	permission := protocol.PromptInfo{ID: "p1", Channel: "s", Agent: "a", From: "main", Kind: protocol.PromptPermission, Tool: "shell",
		Input: json.RawMessage(`{"command":"go test ./..."}`), Question: "shell: go test ./...", Prefix: "go test", Created: now.Format(time.RFC3339)}
	draw(t, "card-permission", func(m *Model) { m.upsertPrompt(permission) })
	draw(t, "card-boundary", func(m *Model) {
		p := permission
		p.ID, p.Prefix, p.Dir, p.Tool, p.Input = "p2", "", "/etc", "read", json.RawMessage(`{"path":"/etc/hosts"}`)
		p.Question = "read: /etc/hosts"
		m.upsertPrompt(p)
	})
	draw(t, "card-control-file", func(m *Model) {
		p := permission
		p.ID, p.Prefix, p.Sticky, p.Tool, p.Input = "p3", "", true, "apply_patch", json.RawMessage(`{"patch":"*** Begin Patch\n*** Update File: AGENTS.md\n@@\n-a\n+b\n*** End Patch"}`)
		p.Question = "apply_patch: AGENTS.md"
		m.upsertPrompt(p)
	})
	draw(t, "card-question", func(m *Model) {
		m.upsertPrompt(protocol.PromptInfo{ID: "q1", Channel: "s", Agent: "a", From: "main", Kind: protocol.PromptQuestion, Question: "Which linker?", Created: now.Format(time.RFC3339),
			QuestionNumber: 1, QuestionTotal: 2, Questions: []event.Question{{Question: "Which linker?", Options: []event.QuestionOption{{Label: "mold", Description: "fastest"}, {Label: "lld"}}}}})
	})
	draw(t, "card-trust", func(m *Model) {
		m.upsertPrompt(protocol.PromptInfo{ID: "t1", Channel: "s", Kind: protocol.PromptTrust, Question: "Trust this project's configuration?", Dir: "", Options: []string{".stavlos/stavlos.json", "AGENTS.md"}, Created: now.Format(time.RFC3339)})
	})
	draw(t, "dialog-mode", func(m *Model) { m.openMode() })
	draw(t, "dialog-discord", func(m *Model) {
		m.openDiscord("")
		m.onDiscord(discordMsg{epoch: m.discordEpoch, status: protocol.DiscordStatus{Configured: true, Enabled: true, State: "connected", Bot: "stavlos", GuildName: "home", Channels: 3, ConfigPath: "/home/x/.config/stavlos/stavlos.json"}})
	})
	draw(t, "dialog-todo", func(m *Model) {
		m.agents[0].Todos = []event.TodoItem{{ID: "t1", Text: "Time the build", Status: "done"}, {ID: "t2", Text: "Try another linker", Status: "in_progress"}, {ID: "t3", Text: "Report", Status: "pending"}}
		m.superChat = false
		m.openTab(focusTodo)
	})
	draw(t, "dialog-async", func(m *Model) {
		m.superChat = false
		m.openTab(focusAsync)
	})
	draw(t, "dialog-dirs", func(m *Model) { m.openTab(focusDirs) })
	draw(t, "key-bar", func(m *Model) { m.command("/help") })
	draw(t, "loading", func(m *Model) { m.loading = true })

	t.Run("a narrow terminal hides the nav", func(t *testing.T) {
		m := goldenChannel(t)
		m.width, m.height = 70, 24
		m.layout()
		m.refreshViewport()
		golden(t, "narrow", m.View())
	})
}
