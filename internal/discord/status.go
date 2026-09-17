package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/protocol"
)

func applicationCommands() []*dg.ApplicationCommand {
	return []*dg.ApplicationCommand{
		{Name: "status", Description: "Show this channel's Stavlos agents", Options: []*dg.ApplicationCommandOption{
			{Type: dg.ApplicationCommandOptionSubCommand, Name: "active", Description: "Show non-idle agents"},
			{Type: dg.ApplicationCommandOptionSubCommand, Name: "all", Description: "Show all agents, including idle agents"},
		}},
		{Name: "cancel", Description: "Cancel an agent's current turn", Options: []*dg.ApplicationCommandOption{
			{Type: dg.ApplicationCommandOptionString, Name: "agent", Description: "Agent name (default main)"},
		}},
	}
}

const statusPageSize = 8

func statusPage(i *dg.Interaction) (all bool, page int, ok bool) {
	if i.Type != dg.InteractionMessageComponent {
		return
	}
	parts := strings.Split(i.MessageComponentData().CustomID, "|")
	if len(parts) != 3 || parts[0] != "sv-status" || parts[1] != "true" && parts[1] != "false" {
		return
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 0 {
		return
	}
	return parts[1] == "true", n, true
}

func (w *worker) status(ctx context.Context, all bool, page int) (string, []dg.MessageComponent, error) {
	r, err := call(ctx, w.link.rpc, protocol.AgentTree, protocol.AgentTreeParams{Channel: w.id})
	if err != nil {
		return "", nil, err
	}
	p, err := call(ctx, w.link.rpc, protocol.PromptList, protocol.PromptListParams{Channel: w.id})
	if err != nil {
		return "", nil, err
	}
	w.agents = r.Agents
	name := w.info.Name
	if name == "" {
		name = w.id
	}
	text, controls := statusView(name, r.Agents, p.Prompts, w.id, all, page)
	return text, controls, nil
}

func statusView(name string, agents []protocol.AgentInfo, prompts []protocol.PromptInfo, channel string, all bool, page int) (string, []dg.MessageComponent) {
	var shown []protocol.AgentInfo
	cost := 0.0
	for _, a := range agents {
		cost += a.CostUSD
		if all || a.State != protocol.AgentIdle {
			shown = append(shown, a)
		}
	}
	summary := fmt.Sprintf("**%d non-idle** · %d total · **$%.2f** spent", len(shown), len(agents), cost)
	if all {
		summary = fmt.Sprintf("**%d agents** · **$%.2f** spent", len(agents), cost)
	}
	lines := []string{"**#" + statusText(name, 100) + " · Status**", summary}
	permissions, questions, trust := 0, 0, 0
	for _, p := range prompts {
		if p.Channel != channel {
			continue
		}
		switch p.Kind {
		case protocol.PromptPermission:
			permissions++
		case protocol.PromptQuestion:
			questions++
		case protocol.PromptTrust:
			trust++
		}
	}
	if permissions+questions+trust > 0 {
		var waiting []string
		if permissions > 0 {
			waiting = append(waiting, statusCount(permissions, "permission"))
		}
		if questions > 0 {
			waiting = append(waiting, statusCount(questions, "question"))
		}
		if trust > 0 {
			waiting = append(waiting, statusCount(trust, "trust prompt"))
		}
		lines = append(lines, "🙋 **Needs you:** "+strings.Join(waiting, " · "))
	}
	if len(shown) == 0 {
		message := "💤 All agents are idle."
		if len(agents) == 0 {
			message = "No agents in this channel yet."
		}
		lines = append(lines, "", message)
		if !all && len(agents) > 0 {
			lines = append(lines, "Use `/status all` to see them.")
		}
		return strings.Join(lines, "\n"), nil
	}
	pages := (len(shown) + statusPageSize - 1) / statusPageSize
	page = min(max(0, page), pages-1)
	start, end := page*statusPageSize, min(len(shown), (page+1)*statusPageSize)
	for _, a := range shown[start:end] {
		mark, state := statusState(a.State)
		name := a.Name
		if name == "" {
			name = a.ID
		}
		line := mark + " **@" + statusText(name, 40) + "** · " + state
		if a.Role != "" {
			line += " · " + statusText(a.Role, 32)
		}
		lines = append(lines, "", line)
		if detail := statusDetail(a); detail != "" {
			lines = append(lines, "  ↳ "+statusText(detail, 100))
		}
	}
	var controls []dg.MessageComponent
	if pages > 1 {
		lines = append(lines, "", fmt.Sprintf("Page %d/%d · agents %d–%d of %d", page+1, pages, start+1, end, len(shown)))
		button := func(label string, target int, disabled bool) dg.MessageComponent {
			return dg.Button{CustomID: fmt.Sprintf("sv-status|%t|%d", all, max(0, target)), Label: label, Style: dg.SecondaryButton, Disabled: disabled}
		}
		controls = []dg.MessageComponent{row(button("Previous", page-1, page == 0), button("Next", page+1, page+1 == pages))}
	}
	if !all && len(shown) < len(agents) {
		lines = append(lines, "", "`/status all` includes idle agents.")
	}
	return strings.Join(lines, "\n"), controls
}

func statusCount(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}

func statusText(text string, limit int) string {
	text = escapeMarkdown(strings.Join(strings.Fields(text), " "))
	return strings.TrimRight(clip(text, limit), "\\")
}

func statusState(state protocol.AgentState) (string, string) {
	switch state {
	case protocol.AgentRunning:
		return "⚙️", "Working"
	case protocol.AgentBlocked:
		return "🙋", "Needs input"
	case protocol.AgentWaiting:
		return "⏳", "Waiting"
	case protocol.AgentIdle:
		return "💤", "Idle"
	case protocol.AgentKilled:
		return "⛔", "Stopped"
	default:
		return "❔", "Unknown"
	}
}

func statusDetail(a protocol.AgentInfo) string {
	if a.LastError != "" {
		return "Error: " + a.LastError
	}
	if len(a.PendingReplies) > 0 {
		return statusCount(len(a.PendingReplies), "request") + " awaiting an explicit response"
	}
	for _, todo := range a.Todos {
		if todo.Status == "in_progress" {
			return todo.Text
		}
	}
	if len(a.Awaiting) > 0 {
		return "Waiting for " + statusCount(len(a.Awaiting), "agent")
	}
	return ""
}
