// Package protocol defines the JSON-RPC 2.0 wire contract (PRD §9).
//
// Transport: newline-delimited JSON-RPC 2.0 over a Unix domain socket.
// Every request carries "v": 1 beside "method" (Version). Server→client
// notifications: "event", "stream", "prompt".
package protocol

import (
	"encoding/json"

	"github.com/nicodes/stavlos/internal/event"
)

// Version is the protocol version served by this daemon.
const Version = 1

// Method names.
const (
	MDaemonStatus      = "daemon.status"
	MDaemonShutdown    = "daemon.shutdown" // graceful stop; used to replace a stale build
	MDiscordStatus     = "discord.status"
	MDiscordConnect    = "discord.connect"
	MDiscordDisconnect = "discord.disconnect"
	MAttach            = "attach" // declare client name + escalation tier

	MChannelList      = "channel.list"
	MChannelCreate    = "channel.create"
	MChannelResume    = "channel.resume"
	MChannelArchive   = "channel.archive"
	MChannelRename    = "channel.rename" // give the channel another name, unique across the daemon
	MChannelSetModel  = "channel.set_model"
	MChannelSetMode   = "channel.set_mode"   // permission mode: ask | auto | yolo
	MChannelPost      = "channel.post"       // the human\'s message in the channel chat, delivered by @mention
	MChannelAddDir    = "channel.add_dir"    // put a directory in the channel\'s working set (every agent\'s)
	MChannelRemoveDir = "channel.remove_dir" // take one out (never the channel directory)
	MChannelSetDir    = "channel.set_dir"    // change an idle channel's default working directory

	MAgentTree       = "agent.tree"
	MAgentSend       = "agent.send" // Prompt / Steer / Cancel / Kill
	MAgentSpawn      = "agent.spawn"
	MAgentSetModel   = "agent.set_model"
	MAgentSetRole    = "agent.set_role"    // switch an agent's preset in place
	MAgentSetVariant = "agent.set_variant" // switch an agent's model variant (reasoning effort)
	MAgentCompact    = "agent.compact"     // summarise the agent\'s completed turns now (or at its next turn if busy)
	MVariants        = "variants"          // variant names a model offers

	MPromptList  = "prompt.list"
	MPromptClaim = "prompt.claim"
	MPromptReply = "prompt.reply"

	MTrustStatus = "trust.status"
	MTrustReply  = "trust.reply"

	MProviderList       = "provider.list"
	MProviderLoginStart = "provider.login.start" // begin a device-code login
	MProviderLoginWait  = "provider.login.wait"  // block until it completes
	MProviderDisconnect = "provider.disconnect"
	MModelList          = "model.list"

	MSubscribe   = "subscribe"
	MUnsubscribe = "unsubscribe"
	MReconcile   = "reconcile"
	MPresets     = "presets" // archetypes available to a channel

	// Notifications (server → client, no id).
	NEvent  = "event"
	NStream = "stream"
	NPrompt = "prompt"
)

// Tier is a client's escalation tier (PRD §7.4).
type Tier string

const (
	TierInteractive Tier = "interactive"
	TierFallback    Tier = "fallback"
)

// Kind is what agent.send delivers: a prompt, a steer, a cancel or a kill.
type Kind string

const (
	KindPrompt Kind = "prompt"
	KindSteer  Kind = "steer"
	KindCancel Kind = "cancel"
	KindKill   Kind = "kill"
)

// AgentState is what an agent is doing (AgentInfo.State).
type AgentState string

const (
	AgentIdle    AgentState = "idle"
	AgentRunning AgentState = "running"
	AgentBlocked AgentState = "blocked" // in a turn, waiting on a permission or a question
	AgentWaiting AgentState = "waiting" // idle, but expecting an answer from an agent or a job's exit
	AgentKilled  AgentState = "killed"
)

// Busy reports whether the agent is in a turn.
func (s AgentState) Busy() bool { return s == AgentRunning || s == AgentBlocked }

// ChannelState rolls a channel's agents up (ChannelInfo.State): working
// while any agent is in a turn, waiting while any expects an answer, idle
// otherwise.
type ChannelState string

const (
	ChannelIdle    ChannelState = "idle"
	ChannelWaiting ChannelState = "waiting"
	ChannelWorking ChannelState = "working"
)

// RollUp is the channel state for a set of agent states.
func RollUp(states []AgentState) ChannelState {
	out := ChannelIdle
	for _, st := range states {
		switch {
		case st.Busy():
			return ChannelWorking
		case st == AgentWaiting:
			out = ChannelWaiting
		}
	}
	return out
}

// PromptKind says what a prompt asks (PromptInfo.Kind).
type PromptKind string

const (
	PromptPermission PromptKind = "permission" // may this tool call run
	PromptQuestion   PromptKind = "question"   // an ask_user batch
	PromptTrust      PromptKind = "trust"      // trust a project's configuration
)

// PromptAction is what happened to a prompt (PromptNotification.Action).
type PromptAction string

const (
	ActionRequested PromptAction = "requested"
	ActionEscalated PromptAction = "escalated" // unclaimed past the claim timeout: shown to the fallback tier too
	ActionClaimed   PromptAction = "claimed"
	ActionAnswered  PromptAction = "answered"
	ActionWithdrawn PromptAction = "withdrawn" // the asking turn ended first
	ActionDefaulted PromptAction = "defaulted" // nobody answered: the headless default applied
)

// Answer values on prompt.reply. The field stays a string because a
// question's reply may carry text; these are the fixed ones.
const (
	AnswerAllow       = "allow"
	AnswerDeny        = "deny"
	AnswerAllowAlways = "allow_always" // this exact call, for the channel
	AnswerAllowPrefix = "allow_prefix" // every call the prompt's prefix covers, for the channel
	AnswerAnswered    = "answered"     // a question batch: the answers are in Answers
)

// MCPState is an agent's MCP server's state (MCPInfo.State).
type MCPState string

const (
	MCPPending   MCPState = "pending" // listed by the role, not started yet
	MCPStarting  MCPState = "starting"
	MCPConnected MCPState = "connected"
	MCPFailed    MCPState = "failed"
	MCPStopped   MCPState = "stopped"
)

// --- JSON-RPC framing ---

type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	V       int              `json:"v"` // protocol version (Version); a daemon refuses a version it does not serve
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
	// Notification fields (when ID is nil).
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
	ErrVersion        = -32000 // unsupported protocol version
	ErrNotFound       = -32001
	ErrConflict       = -32002 // e.g. prompt already claimed, late answer
	ErrTrust          = -32003 // project layer pending trust
	ErrForbidden      = -32004 // the caller may not use the daemon (a process the daemon runs)
)

// --- params / results ---

type AttachParams struct {
	Client string `json:"client"` // human-facing name, e.g. "tui:pid"
	Tier   Tier   `json:"tier"`
}
type AttachResult struct {
	ClientID string `json:"client_id"`
	Version  int    `json:"version"`
}

type DaemonStatusResult struct {
	Version   int      `json:"version"`
	Build     string   `json:"build"` // buildid.ID() of the daemon binary
	PID       int      `json:"pid"`
	DataDir   string   `json:"data_dir"`
	Channels  int      `json:"channels"`
	Agents    int      `json:"agents"`
	Providers []string `json:"providers"`
}

// DiscordStatus describes the daemon-managed integration, without credentials.
type DiscordStatus struct {
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"` // reconnect on daemon startup
	State      string `json:"state"`   // disconnected, connecting, connected, reconnecting, stopping, error
	Bot        string `json:"bot,omitempty"`
	Guild      string `json:"guild,omitempty"`
	GuildName  string `json:"guild_name,omitempty"`
	Channels   int    `json:"channels"`
	Error      string `json:"error,omitempty"`
	ConfigPath string `json:"config_path"`
}

type ChannelInfo struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"` // unique across the daemon, shown as #name
	Dir          string       `json:"dir"`
	DirError     string       `json:"dir_error,omitempty"` // directory unavailable; history remains accessible
	Model        string       `json:"model"`
	RootAgent    string       `json:"root_agent"`
	Created      string       `json:"created"`
	Archived     bool         `json:"archived"`
	Seq          int64        `json:"seq"` // latest per-channel sequence
	Live         int          `json:"live_agents"`
	CostUSD      float64      `json:"cost_usd"`
	Tokens       int          `json:"tokens"` // input + output tokens every agent of the channel has used
	TrustPending bool         `json:"trust_pending"`
	Mode         string       `json:"mode"`                  // permission mode: ask | auto | yolo
	State        ChannelState `json:"state,omitempty"`       // working (an agent runs) | waiting (one expects an answer) | idle; "" for a channel not in memory
	Title        string       `json:"title,omitempty"`       // the first human prompt, for pickers
	Dirs         []DirInfo    `json:"dirs,omitempty"`        // the working directories every agent shares, the channel directory first
	Permissions  int          `json:"permissions,omitempty"` // permission and trust prompts waiting on the human (channel.list)
	Questions    int          `json:"questions,omitempty"`   // questions waiting on the human (channel.list)
}

type ChannelListParams struct {
	Dir             string `json:"dir,omitempty"` // filter
	IncludeArchived bool   `json:"include_archived,omitempty"`
}
type ChannelListResult struct {
	Channels []ChannelInfo `json:"channels"`
}

type ChannelCreateParams struct {
	Name      string `json:"name,omitempty"` // "" = the directory's base name; otherwise normalised and refused when taken
	Dir       string `json:"dir"`
	Model     string `json:"model,omitempty"`      // overrides config
	RootAgent string `json:"root_agent,omitempty"` // archetype; overrides config
}
type ChannelRef struct {
	Channel string `json:"channel"`
}

// ChannelRenameParams names a channel's new name: normalised like an agent's,
// refused when another channel has it.
type ChannelRenameParams struct {
	Channel string `json:"channel"`
	Name    string `json:"name"`
}
type ChannelSetModelParams struct {
	Channel string `json:"channel"`
	Model   string `json:"model"`
}
type ChannelSetModeParams struct {
	Channel string `json:"channel"`
	Mode    string `json:"mode"` // ask | auto | yolo
}

// ChannelPostParams is a message to the channel chat: it reaches every
// agent it @mentions as a steer, or the root agent when it mentions none.
type ChannelPostParams struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// ChannelPostResult names the agents the message was delivered to.
type ChannelPostResult struct {
	To []string `json:"to"`
}

// ChannelDirParams names a directory to add to or remove from the
// channel's working set.
type ChannelDirParams struct {
	Channel string `json:"channel"`
	Dir     string `json:"dir"` // absolute, ~ or relative to the channel directory
}

// Permission modes.
const (
	ModeAsk  = "ask"
	ModeAuto = "auto"
	ModeYolo = "yolo"
)

// ModeSummary says in a phrase what a permission mode does, for pickers,
// status lines and the chat.
func ModeSummary(mode string) string {
	switch mode {
	case ModeAuto:
		return "allows inside the channel's directories, denies outside them"
	case ModeYolo:
		return "every permission is approved, directories included"
	}
	return "every permission is asked"
}

type AgentInfo struct {
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

// PromptInfo is a pending permission/question/trust prompt.
type PromptInfo struct {
	ID             string          `json:"id"`
	Channel        string          `json:"channel"`
	Agent          string          `json:"agent,omitempty"`
	From           string          `json:"from,omitempty"`         // the asking agent\'s name, for a client that does not hold its channel\'s tree
	Role           string          `json:"role,omitempty"`         // the asking agent's role, including prompts from other channels
	ChannelName    string          `json:"channel_name,omitempty"` // the channel\'s name, likewise
	Kind           PromptKind      `json:"kind"`                   // permission | question | trust
	Tool           string          `json:"tool,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	Question       string          `json:"question,omitempty"`
	Options        []string        `json:"options,omitempty"`
	ClaimedBy      string          `json:"claimed_by,omitempty"`
	Escalated      bool            `json:"escalated"` // visible to fallback tier; questions are visible immediately
	Created        string          `json:"created"`
	Dir            string          `json:"dir,omitempty"`             // a boundary prompt: the call reaches outside the channel's directories; "allow_always" adds this one
	Prefix         string          `json:"prefix,omitempty"`          // what "allow_prefix" would remember for this call (a command prefix, a host); "" when the call has none
	Questions      []Question      `json:"questions,omitempty"`       // one question per prompt; legacy servers may send batches
	QuestionNumber int             `json:"question_number,omitempty"` // one-based position in the tool call's sequence
	QuestionTotal  int             `json:"question_total,omitempty"`
}

// QuestionPosition is the display position of a question. Older multi-question
// prompts keep their local numbering; new prompts each contain one question.
func (p PromptInfo) QuestionPosition(index int) (int, int) {
	if len(p.Questions) == 1 && p.QuestionNumber > 0 && p.QuestionTotal >= p.QuestionNumber {
		return p.QuestionNumber, p.QuestionTotal
	}
	return index + 1, len(p.Questions)
}

// Question is an ask_user question: a checklist. Options are
// always present; the human may pick any number of them and add a typed
// answer of their own, all joined with ", " in the answer.
type Question = event.Question
type QuestionOption = event.QuestionOption
type QuestionAnswer = event.QuestionAnswer
type PromptListParams struct {
	Channel string `json:"channel,omitempty"`
}
type PromptListResult struct {
	Prompts []PromptInfo `json:"prompts"`
}
type PromptClaimParams struct {
	ID string `json:"id"`
}
type PromptReplyParams struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`           // allow | deny | allow_always | text
	Dir    string `json:"dir,omitempty"`    // boundary prompt + allow_always: add this directory instead of the offered one
	Reason string `json:"reason,omitempty"` // deny: an optional note the agent sees in its tool result
	// Answers contains the current question's answer (legacy batches have one entry per question)
	// (a picked label, several joined with ", ", or typed text).
	Answers []string         `json:"answers,omitempty"`
	Details []QuestionAnswer `json:"details,omitempty"`
}

type TrustStatusParams struct {
	Dir string `json:"dir"`
}
type TrustStatusResult struct {
	Dir     string   `json:"dir"`
	Pending bool     `json:"pending"`
	Hash    string   `json:"hash,omitempty"`
	Files   []string `json:"files,omitempty"` // what would be trusted
}
type TrustReplyParams struct {
	Dir   string `json:"dir"`
	Hash  string `json:"hash"`
	Trust bool   `json:"trust"`
}

type SubscribeParams struct {
	Channel string `json:"channel"`
	From    int64  `json:"from"` // first per-channel seq to deliver (0 = from start)
}

// ReconcileResult is the authoritative snapshot (PRD §9).
type ReconcileResult struct {
	Channel ChannelInfo  `json:"channel"`
	Agents  []AgentInfo  `json:"agents"`
	Prompts []PromptInfo `json:"prompts"`
	Seq     int64        `json:"seq"`
}

type PresetsParams struct {
	Channel string `json:"channel"`
}
type PresetInfo struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Type        string      `json:"type"`             // primary | subagent | all
	Models      []ModelSpec `json:"models,omitempty"` // whitelist, first is the default; empty = any
	Spawn       []string    `json:"spawn,omitempty"`
	Color       string      `json:"color,omitempty"`
	MaxTurns    int         `json:"max_turns,omitempty"`
}

// ModelSpec is one whitelist entry: a model id (glob allowed) and the
// variants allowed for it (empty = any the provider offers).
type ModelSpec struct {
	ID       string   `json:"id"`
	Variants []string `json:"variants,omitempty"`
}
type PresetsResult struct {
	Presets []PresetInfo `json:"presets"`
}

// ProviderInfo describes one provider known to the daemon (PRD §8).
type ProviderInfo struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Connected bool          `json:"connected"`
	Kind      string        `json:"kind"`              // "subscription" | "plugin"
	Priority  int           `json:"priority"`          // lower sorts first
	Models    int           `json:"models"`            // catalog model count
	Label     string        `json:"label,omitempty"`   // subscription label, e.g. "ChatGPT Plus/Pro subscription"
	Account   string        `json:"account,omitempty"` // email or account id once connected
	Methods   []LoginMethod `json:"methods,omitempty"` // sign-in methods, default first
}

// LoginMethod is one way to sign in to a provider.
type LoginMethod struct {
	ID    string `json:"id"`    // "browser" | "device"
	Label string `json:"label"` // e.g. "ChatGPT Plus/Pro (browser)"
}

type LoginStartParams struct {
	Provider string `json:"provider"`
	Method   string `json:"method,omitempty"` // "" = the provider's default
}
type ProviderListParams struct {
}
type ProviderListResult struct {
	AuthPath  string         `json:"auth_path"`
	Providers []ProviderInfo `json:"providers"`
}

// LoginStartResult describes a device-code login in progress (RFC 8628
// style): the user opens URL and enters Code; the client then calls
// provider.login.wait with ID.
type LoginStartResult struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	Method       string `json:"method"` // "browser": open URL, no code; "device": URL + code
	URL          string `json:"url"`
	Code         string `json:"code"`
	Instructions string `json:"instructions"`
	ExpiresIn    int    `json:"expires_in"` // seconds
}
type LoginWaitParams struct {
	ID string `json:"id"`
}
type ProviderRef struct {
	Provider string `json:"provider"`
}

// ModelInfo is a catalog model.
type ModelInfo struct {
	ID          string  `json:"id"` // full "provider/model-id"
	Provider    string  `json:"provider"`
	Name        string  `json:"name"`
	Context     int     `json:"context,omitempty"`
	InputPrice  float64 `json:"input_price,omitempty"`  // USD per 1M
	OutputPrice float64 `json:"output_price,omitempty"` // USD per 1M
}
type ModelListParams struct {
	Provider string `json:"provider,omitempty"` // filter; empty = all connected providers
	All      bool   `json:"all,omitempty"`      // include unconnected providers
}
type ModelListResult struct {
	Models []ModelInfo `json:"models"`
}

// --- notifications ---

// EventNotification wraps a log event.
type EventNotification struct {
	Event event.Event `json:"event"`
}

// StreamNotification is transient streaming output; not logged.
type StreamNotification struct {
	Channel  string `json:"channel"`
	Agent    string `json:"agent"`
	Turn     int    `json:"turn"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Reset    bool   `json:"reset,omitempty"` // the model call is being retried: drop what streamed for this turn
}

// PromptNotification announces, updates, or withdraws a prompt.
// Action: "requested" | "claimed" | "answered" | "withdrawn" | "defaulted" | "escalated"
type PromptNotification struct {
	Action PromptAction `json:"action"`
	Prompt PromptInfo   `json:"prompt"`
}

// DirInfo is one of the channel's working directories and where it came
// from: channel (the channel directory) | human.
type DirInfo struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// MCPInfo is one MCP server an agent's role lists. State: pending (not
// started yet: it starts at the agent's next turn) | starting | connected |
// failed | stopped.
type MCPInfo struct {
	Name    string   `json:"name"`
	State   MCPState `json:"state"`
	Error   string   `json:"error,omitempty"`
	Tools   []string `json:"tools,omitempty"` // model-facing names
	Started string   `json:"started,omitempty"`
}

// JobInfo is one of an agent's background jobs: a shell command still
// running (one that outlived the shell tool's wait, or was started in the
// background). Children are not jobs; they are agents.
type JobInfo struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	Label    string `json:"label"` // human-facing
	Spec     string `json:"spec"`  // the command line
	Started  string `json:"started"`
	Progress string `json:"progress,omitempty"` // e.g. "42 lines"
}
