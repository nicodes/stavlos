package protocol

import (
	"github.com/nicodes/stavlos/internal/event"
)

// Agents: the tree, and what is sent to one.

type AgentInfo struct {
	Nudges          int                  `json:"nudges,omitempty"`      // consecutive empty reminder-only turns
	NudgeLimit      int                  `json:"nudge_limit,omitempty"` // empty-reminder-turn seatbelt; 0 when reminders are off
	PendingReplies  []event.ReplyRequest `json:"pending_replies,omitempty"`
	AwaitingReplies []event.ReplyRequest `json:"awaiting_replies,omitempty"`
	ID              string               `json:"id"`
	Channel         string               `json:"channel"`
	Parent          string               `json:"parent,omitempty"`
	Role            string               `json:"role"`
	Name            string               `json:"name"`
	Model           string               `json:"model"`
	Variant         string               `json:"variant,omitempty"` // model variant (reasoning effort); "" = default
	Depth           int                  `json:"depth"`
	State           AgentState           `json:"state"` // idle | running | waiting | blocked | finished | killed
	Turn            int                  `json:"turn"`
	Queued          int                  `json:"queued"` // prompts waiting
	CostUSD         float64              `json:"cost_usd"`
	Tokens          int                  `json:"tokens"`                   // input+output total
	Context         int                  `json:"context,omitempty"`        // estimated tokens the next model call carries (what compaction measures)
	ContextWindow   int                  `json:"context_window,omitempty"` // the model\'s window; 0 when unknown
	LastError       string               `json:"last_error,omitempty"`     // error that ended the most recent turn, if any
	Jobs            []JobInfo            `json:"jobs,omitempty"`           // its background jobs still running (not children)
	Todos           []event.TodoItem     `json:"todos,omitempty"`          // this agent\'s todo list, in creation order
	MCP             []MCPInfo            `json:"mcp,omitempty"`            // this agent\'s MCP servers (the ones its role lists), with state
	Awaiting        []string             `json:"awaiting,omitempty"`       // ids of the agents whose answer this one is waiting for (a child's task, a message)
	Due             []string             `json:"due,omitempty"`            // unique request senders; PendingReplies lists each individual obligation
}
type AgentTreeParams struct {
	Channel string `json:"channel"`
}
type AgentTreeResult struct {
	Agents []AgentInfo `json:"agents"` // pre-order; root first
}

type AgentSendParams struct {
	Agent string `json:"agent"`
	Kind  Kind   `json:"kind"`
	Text  string `json:"text,omitempty"`
}

type AgentSpawnParams struct {
	Parent string `json:"parent"`
	Role   string `json:"role"`
	Name   string `json:"name"`
	Task   string `json:"task"`
	Model  string `json:"model,omitempty"`
}
type AgentSpawnResult struct {
	ID string `json:"id"`
}
type AgentSetModelParams struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
}
type AgentSetRoleParams struct {
	Agent string `json:"agent"`
	Role  string `json:"role"` // preset name
}
type AgentCompactParams struct {
	Agent string `json:"agent"`
}
type AgentCompactResult struct {
	Status string `json:"status"` // compacted | queued (the agent is mid-turn; it compacts before its next model call)
}
type AgentSetVariantParams struct {
	Agent   string `json:"agent"`
	Variant string `json:"variant"` // "" = provider default
}
type VariantsParams struct {
	Model string `json:"model"` // provider/id
}
type VariantsResult struct {
	Variants []string `json:"variants"`
}
