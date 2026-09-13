// Package protocol defines the JSON-RPC 2.0 wire contract (PRD §9).
//
// Transport: newline-delimited JSON-RPC 2.0 over a Unix domain socket.
// Every request carries "v": 1 in params (Version). Server→client
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
	MDaemonStatus   = "daemon.status"
	MDaemonShutdown = "daemon.shutdown" // graceful stop; used to replace a stale build
	MAttach         = "attach"          // declare client name + escalation tier

	MSessionList     = "session.list"
	MSessionCreate   = "session.create"
	MSessionResume   = "session.resume"
	MSessionFork     = "session.fork"
	MSessionArchive  = "session.archive"
	MSessionSetModel = "session.set_model"
	MSessionSetYolo  = "session.set_yolo" // auto-approve permission prompts session-wide

	MAgentTree       = "agent.tree"
	MAgentSend       = "agent.send" // Prompt / Steer / Cancel / Kill
	MAgentSpawn      = "agent.spawn"
	MAgentSetModel   = "agent.set_model"
	MAgentSetRole    = "agent.set_role"    // switch an agent's preset in place
	MAgentSetVariant = "agent.set_variant" // switch an agent's model variant (reasoning effort)
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
	MPresets     = "presets" // archetypes available to a session

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

// Envelope kinds for agent.send.
type Kind string

const (
	KindPrompt Kind = "prompt"
	KindSteer  Kind = "steer"
	KindCancel Kind = "cancel"
	KindKill   Kind = "kill"
)

// --- JSON-RPC framing ---

type Request struct {
	JSONRPC string           `json:"jsonrpc"`
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
)

// --- params / results ---

type AttachParams struct {
	V      int    `json:"v"`
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
	Sessions  int      `json:"sessions"`
	Agents    int      `json:"agents"`
	Providers []string `json:"providers"`
}

type SessionInfo struct {
	ID           string  `json:"id"`
	Dir          string  `json:"dir"`
	Model        string  `json:"model"`
	RootAgent    string  `json:"root_agent"`
	Created      string  `json:"created"`
	Archived     bool    `json:"archived"`
	Seq          int64   `json:"seq"` // latest per-session sequence
	Live         int     `json:"live_agents"`
	CostUSD      float64 `json:"cost_usd"`
	TrustPending bool    `json:"trust_pending"`
	Yolo         bool    `json:"yolo,omitempty"` // permission prompts are auto-approved session-wide
}

type SessionListParams struct {
	V               int    `json:"v"`
	Dir             string `json:"dir,omitempty"` // filter
	IncludeArchived bool   `json:"include_archived,omitempty"`
}
type SessionListResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionCreateParams struct {
	V         int    `json:"v"`
	Dir       string `json:"dir"`
	Model     string `json:"model,omitempty"`      // overrides config
	RootAgent string `json:"root_agent,omitempty"` // archetype; overrides config
}
type SessionRef struct {
	V  int    `json:"v"`
	ID string `json:"id"`
}
type SessionForkParams struct {
	V   int    `json:"v"`
	ID  string `json:"id"`
	Seq int64  `json:"seq"` // fork point (inclusive)
}
type SessionSetModelParams struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Model string `json:"model"`
}
type SessionSetYoloParams struct {
	V  int    `json:"v"`
	ID string `json:"id"`
	On bool   `json:"on"`
}

type AgentInfo struct {
	ID        string        `json:"id"`
	Session   string        `json:"session"`
	Parent    string        `json:"parent,omitempty"`
	Archetype string        `json:"archetype"`
	Label     string        `json:"label"`
	Model     string        `json:"model"`
	Variant   string        `json:"variant,omitempty"` // model variant (reasoning effort); "" = default
	Depth     int           `json:"depth"`
	State     string        `json:"state"` // idle | running | waiting | blocked | finished | killed
	Turn      int           `json:"turn"`
	Queued    int           `json:"queued"` // prompts waiting
	CostUSD   float64       `json:"cost_usd"`
	Tokens    int           `json:"tokens"`               // input+output total
	Summary   string        `json:"summary,omitempty"`    // finish summary
	Status    string        `json:"status,omitempty"`     // finish status
	LastError string        `json:"last_error,omitempty"` // error that ended the most recent turn, if any
	Monitors  []MonitorInfo `json:"monitors,omitempty"`   // this agent's general monitors (not children)
	Monitored bool          `json:"monitored"`            // parent armed a wake for this child
}
type AgentTreeParams struct {
	V       int    `json:"v"`
	Session string `json:"session"`
}
type AgentTreeResult struct {
	Agents []AgentInfo `json:"agents"` // pre-order; root first
}

type AgentSendParams struct {
	V     int    `json:"v"`
	Agent string `json:"agent"`
	Kind  Kind   `json:"kind"`
	Text  string `json:"text,omitempty"`
}

type AgentSpawnParams struct {
	V         int    `json:"v"`
	Parent    string `json:"parent"`
	Archetype string `json:"archetype"`
	Label     string `json:"label"`
	Task      string `json:"task"`
	Model     string `json:"model,omitempty"`
}
type AgentSpawnResult struct {
	ID string `json:"id"`
}
type AgentSetModelParams struct {
	V     int    `json:"v"`
	Agent string `json:"agent"`
	Model string `json:"model"`
}
type AgentSetRoleParams struct {
	V     int    `json:"v"`
	Agent string `json:"agent"`
	Role  string `json:"role"` // preset name
}
type AgentSetVariantParams struct {
	V       int    `json:"v"`
	Agent   string `json:"agent"`
	Variant string `json:"variant"` // "" = provider default
}
type VariantsParams struct {
	V     int    `json:"v"`
	Model string `json:"model"` // provider/id
}
type VariantsResult struct {
	Variants []string `json:"variants"`
}

// PromptInfo is a pending permission/question/trust prompt.
type PromptInfo struct {
	ID        string          `json:"id"`
	Session   string          `json:"session"`
	Agent     string          `json:"agent,omitempty"`
	Kind      string          `json:"kind"` // permission | question | trust
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Question  string          `json:"question,omitempty"`
	Options   []string        `json:"options,omitempty"`
	ClaimedBy string          `json:"claimed_by,omitempty"`
	Escalated bool            `json:"escalated"` // visible to fallback tier
	Created   string          `json:"created"`
}
type PromptListParams struct {
	V       int    `json:"v"`
	Session string `json:"session,omitempty"`
}
type PromptListResult struct {
	Prompts []PromptInfo `json:"prompts"`
}
type PromptClaimParams struct {
	V  int    `json:"v"`
	ID string `json:"id"`
}
type PromptReplyParams struct {
	V      int    `json:"v"`
	ID     string `json:"id"`
	Answer string `json:"answer"` // allow | deny | allow_always | text
}

type TrustStatusParams struct {
	V   int    `json:"v"`
	Dir string `json:"dir"`
}
type TrustStatusResult struct {
	Dir     string   `json:"dir"`
	Pending bool     `json:"pending"`
	Hash    string   `json:"hash,omitempty"`
	Files   []string `json:"files,omitempty"` // what would be trusted
}
type TrustReplyParams struct {
	V     int    `json:"v"`
	Dir   string `json:"dir"`
	Hash  string `json:"hash"`
	Trust bool   `json:"trust"`
}

type SubscribeParams struct {
	V       int    `json:"v"`
	Session string `json:"session"`
	From    int64  `json:"from"` // first per-session seq to deliver (0 = from start)
}

// ReconcileResult is the authoritative snapshot (PRD §9).
type ReconcileResult struct {
	Session SessionInfo  `json:"session"`
	Agents  []AgentInfo  `json:"agents"`
	Prompts []PromptInfo `json:"prompts"`
	Seq     int64        `json:"seq"`
}

type PresetsParams struct {
	V       int    `json:"v"`
	Session string `json:"session"`
}
type PresetInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Model       string   `json:"model,omitempty"`
	Spawn       []string `json:"spawn,omitempty"`
}
type PresetsResult struct {
	Presets []PresetInfo `json:"presets"`
}

// ProviderInfo describes one provider known to the daemon (PRD §8).
type ProviderInfo struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Connected bool          `json:"connected"`
	Source    string        `json:"source,omitempty"`  // "auth.json" | "env" | "local"
	Via       string        `json:"via,omitempty"`     // file path or env var name
	Env       []string      `json:"env,omitempty"`     // env vars the provider reads
	Hint      string        `json:"hint,omitempty"`    // where to get a key
	Kind      string        `json:"kind"`              // "anthropic" | "openai-compatible" | "local" | "unsupported"
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
	V        int    `json:"v"`
	Provider string `json:"provider"`
	Method   string `json:"method,omitempty"` // "" = the provider's default
}
type ProviderListParams struct {
	V int `json:"v"`
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
	V  int    `json:"v"`
	ID string `json:"id"`
}
type ProviderRef struct {
	V        int    `json:"v"`
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
	V        int    `json:"v"`
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
	Session  string `json:"session"`
	Agent    string `json:"agent"`
	Turn     int    `json:"turn"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
}

// PromptNotification announces, updates, or withdraws a prompt.
// Action: "requested" | "claimed" | "answered" | "withdrawn" | "defaulted" | "escalated"
type PromptNotification struct {
	Action string     `json:"action"`
	Prompt PromptInfo `json:"prompt"`
}

// MonitorInfo is a general monitor owned by an agent: a background command,
// a file watch, or a timer. Children are not monitors; they are agents.
type MonitorInfo struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Kind      string `json:"kind"`  // command | watch | timer
	Label     string `json:"label"` // human-facing
	Spec      string `json:"spec"`  // command line / path / duration
	State     string `json:"state"` // running | fired | stopped | lost
	Started   string `json:"started"`
	Progress  string `json:"progress,omitempty"` // e.g. "42 lines", "3m left"
	Monitored bool   `json:"monitored"`          // wake armed
}
