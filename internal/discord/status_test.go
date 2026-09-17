package discord

import (
	"context"
	"fmt"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestStatusDefaultsToNonIdleWithReadableStates(t *testing.T) {
	agents := []protocol.AgentInfo{
		{Name: "main", State: protocol.AgentRunning, Role: "general", CostUSD: 1, Todos: []event.TodoItem{{Text: "Review the patch", Status: event.TodoInProgress}}},
		{Name: "build", State: protocol.AgentBlocked},
		{Name: "review", State: protocol.AgentWaiting},
		{Name: "old", State: protocol.AgentKilled},
		{Name: "resting", State: protocol.AgentIdle, CostUSD: 4},
	}
	prompts := []protocol.PromptInfo{{Channel: "here", Kind: protocol.PromptPermission}, {Channel: "here", Kind: protocol.PromptQuestion}, {Channel: "elsewhere", Kind: protocol.PromptTrust}}
	text, controls := statusView("repo", agents, prompts, "here", false, 0)
	for _, want := range []string{"**#repo · Status**", "**4 non-idle**", "**$5.00** spent", "1 permission · 1 question", "⚙️ **@main** · Working", "🙋 **@build** · Needs input", "⏳ **@review** · Waiting", "⛔ **@old** · Stopped", "↳ Review the patch", "/status all"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "@resting") || strings.Contains(text, "trust prompt") || strings.Contains(text, "turn 0") || len(controls) != 0 {
		t.Fatalf("status leaked clutter/hidden agents:\n%s", text)
	}
	all, _ := statusView("repo", agents, nil, "here", true, 0)
	if !strings.Contains(all, "💤 **@resting** · Idle") || !strings.Contains(all, "**5 agents**") {
		t.Fatal("all view did not include idle agents")
	}
	empty, _ := statusView("repo", agents[4:], nil, "here", false, 0)
	if !strings.Contains(empty, "All agents are idle") || strings.Contains(empty, "**@resting**") {
		t.Fatal("all-idle state not explained")
	}
}

func TestStatusAllArgumentAndPaginationStayScoped(t *testing.T) {
	var agents []protocol.AgentInfo
	for i := range 12 {
		agents = append(agents, protocol.AgentInfo{Name: fmt.Sprintf("agent-%02d", i), State: protocol.AgentIdle})
	}
	b, w, api := fixture(t, func(_ context.Context, method string, p, out any) error {
		switch method {
		case protocol.MAgentTree:
			if p.(protocol.AgentTreeParams).Channel != "channel" {
				t.Fatal("status escaped the mapped channel")
			}
			return result(out, protocol.AgentTreeResult{Agents: agents})
		case protocol.MPromptList:
			if p.(protocol.PromptListParams).Channel != "channel" {
				t.Fatal("prompt count escaped the mapped channel")
			}
			return result(out, protocol.PromptListResult{})
		default:
			t.Fatalf("unexpected method %s", method)
		}
		return nil
	})
	i := interaction("", "")
	i.Type = dg.InteractionApplicationCommand
	i.Data = dg.ApplicationCommandInteractionData{Name: "status", Options: []*dg.ApplicationCommandInteractionDataOption{{Name: "active", Type: dg.ApplicationCommandOptionSubCommand}}}
	text, _, err := w.command(context.Background(), i)
	if err != nil || strings.Contains(text, "**@agent") {
		t.Fatalf("default filter: %s %v", text, err)
	}
	i.Data = dg.ApplicationCommandInteractionData{Name: "status", Options: []*dg.ApplicationCommandInteractionDataOption{{Name: "all", Type: dg.ApplicationCommandOptionSubCommand}}}
	text, controls, err := w.command(context.Background(), i)
	if err != nil || !strings.Contains(text, "**@agent-00**") || strings.Contains(text, "**@agent-08**") || len(controls) != 1 {
		t.Fatalf("first page: %s %v", text, err)
	}
	next := controls[0].(dg.ActionsRow).Components[1].(dg.Button)
	i.Type = dg.InteractionMessageComponent
	i.Data = dg.MessageComponentInteractionData{CustomID: next.CustomID}
	b.Interaction(&dg.InteractionCreate{Interaction: i})
	if api.responses[len(api.responses)-1].Type != dg.InteractionResponseDeferredMessageUpdate {
		t.Fatal("paging created a new response")
	}
	if err := w.execute(<-w.queue); err != nil {
		t.Fatal(err)
	}
	if len(api.replies) != 1 || !strings.Contains(api.replies[0], "**@agent-08**") || !strings.Contains(api.replies[0], "Page 2/2") {
		t.Fatalf("next page: %v", api.replies)
	}
	commands := applicationCommands()
	if commands[0].Name != "status" || len(commands[0].Options) != 2 {
		t.Fatal("status subcommands missing")
	}
	for i, name := range []string{"active", "all"} {
		option := commands[0].Options[i]
		if option.Name != name || option.Type != dg.ApplicationCommandOptionSubCommand {
			t.Fatal("status subcommand not registered")
		}
	}
}

func TestStatusLongListsFitWithoutTruncatingAgentRows(t *testing.T) {
	var agents []protocol.AgentInfo
	for i := range 32 {
		agents = append(agents, protocol.AgentInfo{Name: fmt.Sprintf("agent-%02d-", i) + strings.Repeat("x", 30), Role: strings.Repeat("*role_", 30), State: protocol.AgentRunning, Todos: []event.TodoItem{{Text: strings.Repeat("🌲 detail ", 100), Status: event.TodoInProgress}}})
	}
	for page := range 4 {
		text, _ := statusView(strings.Repeat("repo", 30), agents, []protocol.PromptInfo{{Channel: "here", Kind: protocol.PromptPermission}}, "here", false, page)
		if units(text) > 2000 {
			t.Fatalf("page exceeds Discord limit: %d", units(text))
		}
		for i := page * statusPageSize; i < (page+1)*statusPageSize; i++ {
			if !strings.Contains(text, fmt.Sprintf("**@agent-%02d-", i)) {
				t.Fatalf("missing agent %d from page %d", i, page)
			}
		}
	}
}
