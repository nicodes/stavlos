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
	SessionYoloChanged  Type = "session.yolo_changed"  // YoloPayload (legacy: replayed as mode yolo/ask; new logs carry SessionModeChanged)
	SessionModeChanged  Type = "session.mode_changed"  // ModePayload: the session's permission mode (ask | auto | yolo)
	ChatPosted          Type = "chat.posted"           // ChatPayload: the human\'s message in the session chat, logged on the session, and the agents it went to

	AgentSpawned        Type = "agent.spawned"         // AgentSpawnedPayload
	AgentFinished       Type = "agent.finished"        // AgentFinishedPayload (legacy: agents no longer finish; kept for old logs)
	ResponseReceived    Type = "agent.response"        // ResponsePayload: an answer from another agent, logged on the recipient
	MessageToUser       Type = "agent.message_to_user" // ChatPayload: a message tool call addressed to the human, logged on the sender
	ReminderQueued      Type = "agent.reminder_queued" // RepliesPayload: a turn ended owing replies; one reminder starts the next turn
	ReplyMissing        Type = "agent.reply_missing"   // RepliesPayload: a turn ended still owing replies it was reminded of
	AgentKilled         Type = "agent.killed"          // AgentRefPayload
	AgentModelChanged   Type = "agent.model_changed"   // ModelChangedPayload
	AgentRoleChanged    Type = "agent.role_changed"    // RoleChangedPayload: the agent's preset was switched
	AgentVariantChanged Type = "agent.variant_changed" // VariantChangedPayload: model variant (reasoning effort) switched
	AgentDirAdded       Type = "agent.dir_added"       // DirAddedPayload: a directory joined the agent\'s working set (a grant at creation, or the human\'s answer)
	AgentDirRemoved     Type = "agent.dir_removed"     // DirRefPayload: the human took a directory out of the agent\'s working set

	MonitorArmed    Type = "monitor.armed"    // MonitorPayload: wake armed for these ids (children or monitors)
	MonitorDisarmed Type = "monitor.disarmed" // MonitorPayload
	MonitorStarted  Type = "monitor.started"  // MonitorStartedPayload: a general monitor (command, watch, timer) began
	MonitorFired    Type = "monitor.fired"    // MonitorFiredPayload: it completed / detected a change / elapsed
	MonitorStopped  Type = "monitor.stopped"  // MonitorRefPayload: stopped before firing (unmonitor stop, kill, restart)

	TodoChanged Type = "todo.changed" // TodoPayload: the agent\'s todo list after a change (a full snapshot)

	MCPStarted Type = "mcp.started" // MCPStartedPayload: an agent\'s MCP server is connected and its tools listed
	MCPFailed  Type = "mcp.failed"  // MCPFailedPayload: it could not be started or was lost
	MCPStopped Type = "mcp.stopped" // MCPRefPayload: stopped (role change, kill)

	PromptQueued  Type = "prompt.queued"  // TextPayload
	SteerReceived Type = "steer.received" // TextPayload
	NoteQueued    Type = "note.queued"    // TextPayload: a message that needs no reply, for the recipient\'s next step; it never wakes the agent

	TurnStarted      Type = "turn.started"      // TurnPayload
	UserMessage      Type = "user.message"      // UserMessagePayload
	AssistantMessage Type = "assistant.message" // AssistantMessagePayload
	ToolCallStarted  Type = "tool.started"      // ToolStartedPayload
	ToolCallFinished Type = "tool.finished"     // ToolFinishedPayload
	TurnEnded        Type = "turn.ended"        // TurnEndedPayload
	TurnAborted      Type = "turn.aborted"      // TurnPayload (daemon restart, PRD §4.3)

	PromptRequested Type = "prompt.requested" // PromptRequestedPayload (permission/question)
	PromptEscalated Type = "prompt.escalated" // PromptRefPayload: unclaimed past the claim timeout, shown to the fallback tier too
	PromptClaimed   Type = "prompt.claimed"   // PromptRefPayload
	PromptAnswered  Type = "prompt.answered"  // PromptAnsweredPayload
	PromptWithdrawn Type = "prompt.withdrawn" // PromptRefPayload
	PermitGranted   Type = "permit.granted"   // PermitPayload: the human allowed a call or a prefix for the rest of the session
	PromptDefaulted Type = "prompt.defaulted" // PromptAnsweredPayload

	Usage     Type = "usage"     // UsagePayload (PRD §4.4)
	Compacted Type = "compacted" // CompactedPayload

	CompactionStarted Type = "compaction.started" // CompactionPayload: the summariser is running
	CompactionFailed  Type = "compaction.failed"  // CompactionPayload with Error
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

// ModePayload records the session's permission mode: ask (every policy
// ask prompts), auto (asks are allowed inside the agent's working
// directories, the boundary still asks), yolo (everything a policy would
// ask about is allowed, boundary included). Deny rules hold in every mode.
type ModePayload struct {
	Mode string `json:"mode"`
}

// YoloPayload records the session's yolo switch: while on, every tool
// call a policy would ask about is allowed without a prompt.
type YoloPayload struct {
	On bool `json:"on"`
}

// VariantChangedPayload records a model-variant switch; "" is the
// provider default.
type VariantChangedPayload struct {
	Variant string `json:"variant"`
}

type AgentSpawnedPayload struct {
	ID        string   `json:"id"`
	Parent    string   `json:"parent,omitempty"` // empty for the root
	Archetype string   `json:"archetype"`
	Label     string   `json:"label"`
	Model     string   `json:"model"` // resolved provider/model-id
	Task      string   `json:"task,omitempty"`
	Depth     int      `json:"depth"`
	Dirs      []string `json:"dirs,omitempty"` // directories the creator granted, absolute
}

// DirAddedPayload: Source is "grant" (from the creating agent) or "human"
// (the answer to a boundary prompt). Role directories are not logged: they
// follow the role.
type DirAddedPayload struct {
	Dir    string `json:"dir"`
	Source string `json:"source"`
}

type DirRefPayload struct {
	Dir string `json:"dir"`
}

// ResponsePayload is an answer delivered to this agent (a message from an
// agent it was waiting on): who sent it and what it said. It is consumed by the recipient's next turn as a
// user message of kind "agent_response".
type ResponsePayload struct {
	From      string `json:"from"`
	FromLabel string `json:"from_label,omitempty"`
	Text      string `json:"text"`
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
	Post   string `json:"post,omitempty"` // the session chat post a steer delivers
}

// MonitorPayload lists child ids whose finish wakes (or no longer wakes) the agent.
type MonitorPayload struct {
	IDs []string `json:"ids"`
}

// TodoStatus is where a todo item stands.
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoDone       TodoStatus = "done"
	TodoCancelled  TodoStatus = "cancelled"
)

// TodoItem is one entry of an agent's todo list.
type TodoItem struct {
	ID     string     `json:"id"`
	Text   string     `json:"text"`
	Status TodoStatus `json:"status"`
}

// TodoPayload is the whole todo list after a change; replaying the last
// one restores the list.
type TodoPayload struct {
	Items []TodoItem `json:"items"`
}

// MCPStartedPayload lists the tools an agent's MCP server offers, by their
// model-facing names (mcp__<server>__<tool>).
type MCPStartedPayload struct {
	Server string   `json:"server"`
	Tools  []string `json:"tools"`
}

type MCPFailedPayload struct {
	Server string `json:"server"`
	Error  string `json:"error"`
}

type MCPRefPayload struct {
	Server string `json:"server"`
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

// MessageKind says where a user message came from.
type MessageKind string

const (
	MsgPrompt        MessageKind = "prompt"         // one or more coalesced prompts (a steer to an idle agent reads as one)
	MsgSteer         MessageKind = "steer"          // delivered mid-turn at a model-call boundary
	MsgAgentResponse MessageKind = "agent_response" // another agent's answer, from the mailbox
	MsgMonitorFired  MessageKind = "monitor_fired"  // a background job's exit, from the mailbox
	MsgReminder      MessageKind = "reminder"       // the harness's one reminder of replies still owed
	MsgNote          MessageKind = "note"           // a message from another agent that needs no reply (message no_reply)
)

// UserMessagePayload is the model-visible input to a model call.
type UserMessagePayload struct {
	Turn int         `json:"turn"`
	Kind MessageKind `json:"kind"`
	Text string      `json:"text"`
	// From names the sending agent ("scout") when a prompt, steer or answer
	// came from another agent in the session; empty for humans. FromID is
	// its id (logs from before it carry the name only).
	From   string `json:"from,omitempty"`
	FromID string `json:"from_id,omitempty"`
	Post   string `json:"post,omitempty"` // the session chat post this input delivers
}

// ChatPayload is a message in the session chat: the human's post (To: the
// names of the agents it was delivered to) or an agent's message to the
// human (From: the agent's name).
type ChatPayload struct {
	ID   string   `json:"id,omitempty"` // a post's id
	From string   `json:"from,omitempty"`
	Text string   `json:"text"`
	To   []string `json:"to,omitempty"`
	Post string   `json:"post,omitempty"` // a message to the human: the post it answers
}

// RepliesPayload names the parties a turn ended owing a reply: "user" or
// agent ids, with the names shown for them.
type RepliesPayload struct {
	Parties []string `json:"parties"`
	Names   []string `json:"names"`
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

// TurnReason says why a turn ended.
type TurnReason string

const (
	ReasonEndTurn   TurnReason = "end_turn"
	ReasonCancelled TurnReason = "cancelled"
	ReasonError     TurnReason = "error" // Error carries the message
	ReasonMaxTokens TurnReason = "max_tokens"
)

type TurnEndedPayload struct {
	Turn   int        `json:"turn"`
	Reason TurnReason `json:"reason"`
	Error  string     `json:"error,omitempty"`
}

// PromptRequestedPayload.Kind: "permission" | "question" | "trust"
type PromptRequestedPayload struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Question  string          `json:"question,omitempty"`
	Options   []string        `json:"options,omitempty"`
	Questions json.RawMessage `json:"questions,omitempty"` // kind question: the protocol.Question batch, as JSON
}

// PermitPayload is an allow the human granted for the session: either an
// exact call (Call, the policy subject it matched) or a prefix (a command
// prefix, a host) covering every call of the tool it fits. Replayed on
// recovery, so a restart does not ask again.
type PermitPayload struct {
	Tool   string `json:"tool"`
	Call   string `json:"call,omitempty"`
	Prefix string `json:"prefix,omitempty"`
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
	Before  int    `json:"before,omitempty"` // estimated history tokens before and after (clients show "84k → 12k")
	After   int    `json:"after,omitempty"`
}

// CompactionPayload marks the start or failure of a compaction.
type CompactionPayload struct {
	Before int    `json:"before,omitempty"` // estimated history tokens going in
	Error  string `json:"error,omitempty"`
}

// MustPayload marshals v or panics; payloads are our own types.
func MustPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
