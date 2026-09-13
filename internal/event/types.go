// Package event defines the append-only event vocabulary (PRD §3.3, §4.2).
// Every state change in a session is exactly one of these.
package event

import (
	"encoding/json"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

// Type names each event kind. The Payload's Go type is listed beside it.
type Type string

const (
	SessionCreated      Type = "session.created"       // SessionCreatedPayload
	SessionArchived     Type = "session.archived"      // (none)
	SessionModelChanged Type = "session.model_changed" // ModelChangedPayload

	AgentSpawned      Type = "agent.spawned"       // AgentSpawnedPayload
	AgentFinished     Type = "agent.finished"      // AgentFinishedPayload
	AgentKilled       Type = "agent.killed"        // AgentRefPayload
	AgentModelChanged Type = "agent.model_changed" // ModelChangedPayload
	AgentRoleChanged  Type = "agent.role_changed"  // RoleChangedPayload: the agent's preset was switched

	MonitorArmed    Type = "monitor.armed"    // MonitorPayload: wake armed for these ids (children or monitors)
	MonitorDisarmed Type = "monitor.disarmed" // MonitorPayload
	MonitorStarted  Type = "monitor.started"  // MonitorStartedPayload: a general monitor (command, watch, timer) began
	MonitorFired    Type = "monitor.fired"    // MonitorFiredPayload: it completed / detected a change / elapsed
	MonitorStopped  Type = "monitor.stopped"  // MonitorRefPayload: stopped before firing (unmonitor stop, kill, restart)

	PromptQueued  Type = "prompt.queued"  // TextPayload
	SteerReceived Type = "steer.received" // TextPayload

	TurnStarted      Type = "turn.started"      // TurnPayload
	UserMessage      Type = "user.message"      // UserMessagePayload
	AssistantMessage Type = "assistant.message" // AssistantMessagePayload
	ToolCallStarted  Type = "tool.started"      // ToolStartedPayload
	ToolCallFinished Type = "tool.finished"     // ToolFinishedPayload
	TurnEnded        Type = "turn.ended"        // TurnEndedPayload
	TurnAborted      Type = "turn.aborted"      // TurnPayload (daemon restart, PRD §4.3)

	PromptRequested Type = "prompt.requested" // PromptRequestedPayload (permission/question)
	PromptClaimed   Type = "prompt.claimed"   // PromptRefPayload
	PromptAnswered  Type = "prompt.answered"  // PromptAnsweredPayload
	PromptWithdrawn Type = "prompt.withdrawn" // PromptRefPayload
	PromptDefaulted Type = "prompt.defaulted" // PromptAnsweredPayload

	Usage     Type = "usage"     // UsagePayload (PRD §4.4)
	Compacted Type = "compacted" // CompactedPayload
)

// Event is one log record.
type Event struct {
	Global  int64           `json:"global"`  // total order across sessions
	Seq     int64           `json:"seq"`     // per-session, contiguous from 1
	Session string          `json:"session"` // session id
	Agent   string          `json:"agent,omitempty"`
	Type    Type            `json:"type"`
	Time    time.Time       `json:"time"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Decode unmarshals the payload into v.
func (e Event) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// --- payloads ---

type SessionCreatedPayload struct {
	Dir        string `json:"dir"`
	Model      string `json:"model"`
	RootAgent  string `json:"root_agent"` // archetype
	ForkedFrom string `json:"forked_from,omitempty"`
	ForkSeq    int64  `json:"fork_seq,omitempty"`
}

type ModelChangedPayload struct {
	Model string `json:"model"`
}

// RoleChangedPayload records a preset switch; Label is the agent's label
// afterwards (it follows the role when it was the old role's name).
type RoleChangedPayload struct {
	Role  string `json:"role"`
	Label string `json:"label"`
}

type AgentSpawnedPayload struct {
	ID        string `json:"id"`
	Parent    string `json:"parent,omitempty"` // empty for the root
	Archetype string `json:"archetype"`
	Label     string `json:"label"`
	Model     string `json:"model"` // resolved provider/model-id
	Task      string `json:"task,omitempty"`
	Depth     int    `json:"depth"`
}

type AgentFinishedPayload struct {
	Summary   string     `json:"summary"`
	Status    string     `json:"status"` // success | failure | partial
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

type Artifact struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type AgentRefPayload struct {
	ID string `json:"id"`
}

// Source says who wrote an envelope: "human:<client>" or "agent:<id>".
type TextPayload struct {
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}

// MonitorPayload lists child ids whose finish wakes (or no longer wakes) the agent.
type MonitorPayload struct {
	IDs []string `json:"ids"`
}

// MonitorStartedPayload describes a general monitor. Kind: "command" |
// "watch" | "timer". Spec is kind-specific: the command line, the watched
// path (plus glob), or the duration in seconds.
type MonitorStartedPayload struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"`
	Label   string  `json:"label"`
	Spec    string  `json:"spec"`
	Glob    string  `json:"glob,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

type MonitorFiredPayload struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Summary  string `json:"summary"`          // one line
	Output   string `json:"output,omitempty"` // command output / changed paths
	ExitCode int    `json:"exit_code,omitempty"`
	IsError  bool   `json:"is_error,omitempty"`
}

type MonitorRefPayload struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

type TurnPayload struct {
	Turn int `json:"turn"`
}

// UserMessagePayload is the model-visible input to a model call. Kind is
// "prompt" (one or more coalesced prompts), "steer", or "child_finished".
type UserMessagePayload struct {
	Turn int    `json:"turn"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type AssistantMessagePayload struct {
	Turn       int           `json:"turn"`
	Blocks     []model.Block `json:"blocks"`
	StopReason string        `json:"stop_reason"`
	Model      string        `json:"model"`
}

type ToolStartedPayload struct {
	Turn   int             `json:"turn"`
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
}

type ToolFinishedPayload struct {
	Turn      int    `json:"turn"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Output    string `json:"output"`
	IsError   bool   `json:"is_error,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Denied    bool   `json:"denied,omitempty"`
}

// TurnEndedPayload.Reason: "end_turn" | "cancelled" | "error" | "finished" | "max_tokens"
type TurnEndedPayload struct {
	Turn   int    `json:"turn"`
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

// PromptRequestedPayload.Kind: "permission" | "question" | "trust"
type PromptRequestedPayload struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Tool     string          `json:"tool,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Question string          `json:"question,omitempty"`
	Options  []string        `json:"options,omitempty"`
}

type PromptRefPayload struct {
	ID     string `json:"id"`
	Client string `json:"client,omitempty"`
}

type PromptAnsweredPayload struct {
	ID     string `json:"id"`
	Answer string `json:"answer"` // "allow" | "deny" | free text for questions
	Client string `json:"client,omitempty"`
}

type UsagePayload struct {
	Turn    int         `json:"turn"`
	Model   string      `json:"model"`
	Usage   model.Usage `json:"usage"`
	CostUSD float64     `json:"cost_usd"`
}

type CompactedPayload struct {
	FromSeq int64  `json:"from_seq"`
	ToSeq   int64  `json:"to_seq"`
	Summary string `json:"summary"`
}

// MustPayload marshals v or panics; payloads are our own types.
func MustPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
