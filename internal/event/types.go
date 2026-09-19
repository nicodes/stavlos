// Package event defines the append-only event vocabulary (PRD §3.3, §4.2).
// Every state change in a channel is exactly one of these, and the channel's
// in-memory state is a fold of them (agent.apply), live and on recovery.
package event

import (
	"encoding/json"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

// Type names each event kind. The Payload's Go type is listed beside it.
type Type string

const (
	ChannelCreated    Type = "channel.created"     // ChannelCreatedPayload
	ChannelUpdated    Type = "channel.updated"     // ChannelUpdatedPayload: its name, model or permission mode changed
	ChannelArchived   Type = "channel.archived"    // (none)
	ChannelDirAdded   Type = "channel.dir_added"   // DirPayload: a directory joined the working set; Agent is the agent whose boundary prompt added it
	ChannelDirRemoved Type = "channel.dir_removed" // DirPayload
	ChatPosted        Type = "chat.posted"         // ChatPayload: the human's post in the channel chat, logged on the channel
	ChatMessage       Type = "chat.message"        // ChatPayload: an agent's message to the human, logged on the agent

	AgentSpawned   Type = "agent.spawned"   // AgentSpawnedPayload
	AgentUpdated   Type = "agent.updated"   // AgentUpdatedPayload: its role, name, model or variant changed
	AgentKilled    Type = "agent.killed"    // (none)
	AgentCancelled Type = "agent.cancelled" // (none): drop this agent's reply-debt; it stays alive

	InputQueued Type = "input.queued" // Input: something for the agent's model, waiting in its inbox
	InputTaken  Type = "input.taken"  // InputTakenPayload: the inputs a model call consumed, in order

	TurnStarted      Type = "turn.started"      // TurnPayload
	AssistantMessage Type = "assistant.message" // AssistantMessagePayload
	ToolStarted      Type = "tool.started"      // ToolStartedPayload
	ToolFinished     Type = "tool.finished"     // ToolFinishedPayload
	TurnEnded        Type = "turn.ended"        // TurnEndedPayload
	TurnAborted      Type = "turn.aborted"      // TurnPayload: the turn was open when the daemon stopped

	AskRequested  Type = "ask.requested"  // AskRequestedPayload: a permission, question or trust prompt
	AskResolved   Type = "ask.resolved"   // AskResolvedPayload
	PermitGranted Type = "permit.granted" // PermitPayload: the human allowed a call or a prefix for the channel

	JobStarted  Type = "job.started"  // JobStartedPayload: a shell command continues in the background
	JobFinished Type = "job.finished" // JobFinishedPayload
	JobStopped  Type = "job.stopped"  // JobStoppedPayload: killed before it finished

	TodoChanged Type = "todo.changed" // TodoPayload: the whole list after a change

	SheetWritten Type = "sheet.written" // SheetPayload: an agent created a sheet or replaced its content
	SheetDeleted Type = "sheet.deleted" // SheetPayload{ID}

	MCPStarted Type = "mcp.started" // MCPStartedPayload
	MCPFailed  Type = "mcp.failed"  // MCPFailedPayload
	MCPStopped Type = "mcp.stopped" // MCPRefPayload

	CompactionStarted Type = "compaction.started" // CompactionPayload{Before}
	CompactionDone    Type = "compaction.done"    // CompactionPayload: the summary replaces the history up to ToSeq
	CompactionFailed  Type = "compaction.failed"  // CompactionPayload{Before, Error}
)

// Event is one log record.
type Event struct {
	Global  int64           `json:"global"`  // total order across channels
	Seq     int64           `json:"seq"`     // per channel, contiguous from 1
	Channel string          `json:"channel"` // channel id
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

// MustPayload marshals v or panics; payloads are our own types.
func MustPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Str is a pointer to s, for the optional fields of an update.
func Str(s string) *string { return &s }

// --- channel ---

type ChannelCreatedPayload struct {
	Name  string `json:"name"` // unique across the daemon, shown as #name
	Dir   string `json:"dir"`
	Model string `json:"model,omitempty"`
	Role  string `json:"role"`           // the main agent's role
	Mode  string `json:"mode,omitempty"` // the permission mode it starts in (ask when empty)
}

// ChannelUpdatedPayload carries only what changed.
type ChannelUpdatedPayload struct {
	Dir   *string `json:"dir,omitempty"` // new default directory; clears remembered permits and resets mode to ask
	Name  *string `json:"name,omitempty"`
	Model *string `json:"model,omitempty"`
	Mode  *string `json:"mode,omitempty"`  // ask | auto | yolo
	Recap *int    `json:"recap,omitempty"` // minutes of silence after which the main agent is asked for a status report; 0 turns it off
}

// DirPayload names a working directory; Source is "human" on an add.
type DirPayload struct {
	Dir    string `json:"dir"`
	Source string `json:"source,omitempty"`
}

// ChatPayload is a message in the channel chat: the human's post (ID, the
// names it went To, and From, the client that posted it) or an agent's
// message to the human (From, the agent's name, and the Post it answers,
// "" for none).
type ChatPayload struct {
	Kind      string   `json:"kind,omitempty"` // request/response/info; empty identifies legacy events
	RequestID string   `json:"request_id,omitempty"`
	ReplyTo   []string `json:"reply_to,omitempty"`
	Posts     []string `json:"posts,omitempty"` // human posts explicitly answered together
	ID        string   `json:"id,omitempty"`
	// From is the agent's name on a message, and on a post the client that
	// sent it ("human:tui:1234", "human:discord"). A client mirroring the
	// chat elsewhere reads it to tell its own posts from everyone else's.
	From string   `json:"from,omitempty"`
	Text string   `json:"text"`
	To   []string `json:"to,omitempty"` // complete recipient names on posts and agent messages
	Post string   `json:"post,omitempty"`
}

// --- agent ---

type AgentSpawnedPayload struct {
	ID      string `json:"id"`
	Parent  string `json:"parent,omitempty"` // "" for the main agent
	Role    string `json:"role"`
	Name    string `json:"name"` // unique in the channel, never reused
	Model   string `json:"model,omitempty"`
	Variant string `json:"variant,omitempty"`
	Depth   int    `json:"depth"`
}

// AgentUpdatedPayload carries only what changed.
type AgentUpdatedPayload struct {
	Role    *string `json:"role,omitempty"`
	Name    *string `json:"name,omitempty"`
	Model   *string `json:"model,omitempty"`
	Variant *string `json:"variant,omitempty"` // "" is the provider default
}

// --- inbox ---

// InputKind says what an input is, which decides when it reaches the model
// and whether it is owed a reply.
type InputKind string

const (
	InputPrompt   InputKind = "prompt"   // the human's, queued for after the current turn
	InputSteer    InputKind = "steer"    // the human's, at the next model call; a channel post carries Post and is owed a reply
	InputRequest  InputKind = "request"  // another agent's (a message, a child's task), at the next model call; owed a reply
	InputInfo     InputKind = "info"     // another agent's, needing no reply; it never wakes the agent
	InputResponse InputKind = "response" // another agent's answer, between turns; it settles the wait on that agent
	InputJob      InputKind = "job"      // a background job's result (Job), between turns
	InputReminder InputKind = "reminder" // the harness's reminder of replies still owed (Parties)
)

// Input is one entry of an agent's inbox.
type Input struct {
	RequestID string         `json:"request_id,omitempty"`
	ReplyTo   []string       `json:"reply_to,omitempty"`
	Requests  []ReplyRequest `json:"requests,omitempty"` // per-request reminder entries
	To        []string       `json:"to,omitempty"`       // all recipient names, shared by every delivery
	ID        string         `json:"id"`
	Kind      InputKind      `json:"kind"`
	Text      string         `json:"text,omitempty"`
	From      string         `json:"from,omitempty"`      // the sending agent's id
	FromName  string         `json:"from_name,omitempty"` // its name when it sent
	Post      string         `json:"post,omitempty"`      // the channel chat post a steer delivers
	Job       string         `json:"job,omitempty"`       // kind job: the job whose result this is
	Parties   []string       `json:"parties,omitempty"`   // kind reminder: who is owed ("user" or agent ids)
	Names     []string       `json:"names,omitempty"`     // …and their names
}

// ReplyRequest identifies one response obligation. A broadcast shares an ID,
// but every receiving agent independently owes its own response.
type ReplyRequest struct {
	ID       string   `json:"id"`
	From     string   `json:"from"`
	FromName string   `json:"from_name"`
	To       []string `json:"to,omitempty"`
	Text     string   `json:"text"` // short excerpt for status and reminders
	Post     string   `json:"post,omitempty"`
}

// InputTakenPayload lists the inputs a model call consumed.
type InputTakenPayload struct {
	Turn int      `json:"turn"`
	IDs  []string `json:"ids"`
}

// --- turns ---

type TurnPayload struct {
	Turn int `json:"turn"`
}

type AssistantMessagePayload struct {
	Turn       int           `json:"turn"`
	Blocks     []model.Block `json:"blocks"`
	StopReason string        `json:"stop_reason"`
	Model      string        `json:"model"`
	Usage      model.Usage   `json:"usage"`
	CostUSD    float64       `json:"cost_usd,omitempty"`
}

// ToolStartedPayload names a call; its input is in the assistant message.
type ToolStartedPayload struct {
	Turn   int    `json:"turn"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
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

// --- asks ---

// AskRequestedPayload records a prompt to the human. Kind is permission,
// question or trust; CallID ties a permission or question to its tool call.
type AskRequestedPayload struct {
	Input          json.RawMessage `json:"input,omitempty"`
	Dir            string          `json:"dir,omitempty"`
	Prefix         string          `json:"prefix,omitempty"`
	From           string          `json:"from,omitempty"`
	Role           string          `json:"role,omitempty"`
	Questions      []Question      `json:"questions,omitempty"`
	QuestionNumber int             `json:"question_number,omitempty"`
	QuestionTotal  int             `json:"question_total,omitempty"`
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	CallID         string          `json:"call_id,omitempty"`
	Tool           string          `json:"tool,omitempty"`
	Question       string          `json:"question,omitempty"`
}

// AskOutcome is how a prompt ended.
type AskOutcome string

const (
	AskAnswered  AskOutcome = "answered"
	AskDefaulted AskOutcome = "defaulted" // nobody answered: the headless default applied
	AskWithdrawn AskOutcome = "withdrawn" // the asking turn ended first
)

type AskResolvedPayload struct {
	Reason  string           `json:"reason,omitempty"`
	Dir     string           `json:"dir,omitempty"`
	Answers []string         `json:"answers,omitempty"`
	Details []QuestionAnswer `json:"details,omitempty"`
	ID      string           `json:"id"`
	Outcome AskOutcome       `json:"outcome"`
	Answer  string           `json:"answer,omitempty"`
	By      string           `json:"by,omitempty"` // the answering client
}

// Question and QuestionAnswer preserve a question card, including the distinction
// between checked options and custom text, across clients and history replay.
type Question struct {
	Question string           `json:"question"`
	Options  []QuestionOption `json:"options"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type QuestionAnswer struct {
	Selected []int  `json:"selected,omitempty"`
	Custom   string `json:"custom,omitempty"`
}

// PermitPayload is an allow the human granted for the channel: an exact
// call (Call, one subject value) or a prefix (a command prefix, a host).
type PermitPayload struct {
	Tool   string `json:"tool"`
	Call   string `json:"call,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

// --- jobs ---

type JobStartedPayload struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

type JobFinishedPayload struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exit_code"`
	IsError  bool   `json:"is_error,omitempty"`
}

type JobStoppedPayload struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// --- todo ---

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

type TodoPayload struct {
	Items []TodoItem `json:"items"`
}

// --- MCP ---

// MCPStartedPayload lists a server's tools by their model-facing names
// (mcp__<server>__<tool>).
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

// --- compaction ---

// CompactionPayload: Before and After are estimated history tokens; a
// finished compaction's summary covers the agent's events up to ToSeq.
type CompactionPayload struct {
	FromSeq int64  `json:"from_seq,omitempty"`
	ToSeq   int64  `json:"to_seq,omitempty"`
	Summary string `json:"summary,omitempty"`
	Before  int    `json:"before,omitempty"`
	After   int    `json:"after,omitempty"`
	Error   string `json:"error,omitempty"`
}

// --- sheets ---

// SheetPayload describes a sheet, an HTML page an agent wrote for the human
// (docs/web-ui.md). The page itself is a file in the channel's sheets
// directory; the log carries what a client lists and a hash, so a viewer
// knows when to load it again.
type SheetPayload struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Author string `json:"author,omitempty"` // the name of the agent that wrote this version
	Hash   string `json:"hash,omitempty"`   // sha256 of the file, hex
	Size   int    `json:"size,omitempty"`
}
