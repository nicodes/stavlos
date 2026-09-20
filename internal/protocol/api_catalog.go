package protocol

import (
	"github.com/nicodes/stavlos/internal/event"
)

// Roles, providers, models and usage: what there is to choose from and what it cost.

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
	Method       string `json:"method"` // "browser": open URL, no code; "device": URL + code; "apikey": paste a key
	URL          string `json:"url"`
	Code         string `json:"code"`
	Instructions string `json:"instructions"`
	ExpiresIn    int    `json:"expires_in"` // seconds
}
type LoginWaitParams struct {
	ID string `json:"id"`
}

// LoginKeyParams delivers the key an "apikey" login is waiting for. The
// waiting provider.login.wait then returns as any other login would.
type LoginKeyParams struct {
	ID  string `json:"id"`
	Key string `json:"key"`
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
