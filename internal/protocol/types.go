// Package protocol defines the JSON-RPC 2.0 wire contract (PRD §9).
//
// Transport: newline-delimited JSON-RPC 2.0 over a Unix domain socket.
// Every request carries "v": 1 beside "method" (Version). Server→client
// notifications: "event", "stream", "prompt".
package protocol

import (
	"time"
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
	MWebStatus         = "web.status"
	MWebEnable         = "web.enable"     // start the loopback listener; the result carries a sign-in URL to open
	MWebDisable        = "web.disable"    // stop it and sign every browser out
	MWebOpen           = "web.open"       // a fresh sign-in URL for a listener already running
	MSandboxStatus     = "sandbox.status" // what bounds commands on this machine, and what is missing
	MSandboxSet        = "sandbox.set"    // turn the command sandbox on or off (the global setting)
	MAttach            = "attach"         // declare client name + escalation tier

	MChannelList      = "channel.list"
	MChannelCreate    = "channel.create"
	MChannelResume    = "channel.resume"
	MChannelArchive   = "channel.archive"
	MChannelRename    = "channel.rename" // give the channel another name, unique across the daemon
	MChannelSetModel  = "channel.set_model"
	MChannelSetMode   = "channel.set_mode"   // permission mode: ask | auto | yolo
	MChannelSetRecap  = "channel.set_recap"  // minutes of silence before the main agent is asked for a status report; 0 off
	MChannelPost      = "channel.post"       // the human\'s message in the channel chat, delivered by @mention
	MChannelAddDir    = "channel.add_dir"    // put a directory in the channel\'s working set (every agent\'s)
	MChannelRemoveDir = "channel.remove_dir" // take one out (never the channel directory)
	MChannelSetDir    = "channel.set_dir"    // change an idle channel's default working directory

	MAgentTree       = "agent.tree"
	MAgentSend       = "agent.send" // Prompt / Steer / Cancel / Kill
	MAgentSpawn      = "agent.spawn"
	MAgentSetModel   = "agent.set_model"
	MAgentSetRole    = "agent.set_role"    // switch an agent's role in place
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
	MProviderLoginKey   = "provider.login.key"   // hand a pasted API key to a login waiting for one
	MProviderDisconnect = "provider.disconnect"
	MModelList          = "model.list"

	MSheetList = "sheet.list" // the channel's sheets, oldest first

	MSubscribe   = "subscribe"
	MUnsubscribe = "unsubscribe"
	MReconcile   = "reconcile"
	MRoles       = "roles" // archetypes available to a channel

	MUsageSeries = "usage.series" // tokens and cost over time: the system's, a channel's or an agent's
	MPlanUsage   = "plan.usage"   // the signed-in subscriptions' plan usage, as last observed
	MCacheUsage  = "usage.cache"  // how much of recent model calls came from the providers' prompt caches
	MPlanSeries  = "plan.series"  // a subscription's plan usage over time, as it was observed

	// Notifications (server → client, no id).
	NEvent  = "event"
	NStream = "stream"
	NPrompt = "prompt"
	// NChanged says that something a client shows beside its channels is no
	// longer what it was last told, so it asks again once instead of asking
	// every few seconds for ever.
	NChanged = "changed"
)

// ChangedNotification names what changed: one of the Changed* values.
type ChangedNotification struct {
	What string `json:"what"`
}

const (
	ChangedWeb     = "web"
	ChangedDiscord = "discord"
	ChangedPlan    = "plan" // a subscription's plan usage, or the limit it is known to be at
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
	Version  int    `json:"version"`
	Build    string `json:"build"` // buildid.ID() of the daemon binary
	PID      int    `json:"pid"`
	DataDir  string `json:"data_dir"`
	Channels int    `json:"channels"`
	Agents   int    `json:"agents"`
	// Working is how many agents are in a turn or have a job running: what a
	// restart of the daemon would cut short.
	Working   int      `json:"working"`
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

// WebStatus describes the daemon's browser listener. OpenURL carries a
// one-time code in its fragment and is only set by web.enable and web.open.
type WebStatus struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	OpenURL string `json:"open_url,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SheetInfo is one sheet of a channel: an HTML page an agent wrote, shown by
// the web UI in a sandboxed frame.
type SheetInfo struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Author  string    `json:"author"` // the agent that wrote the current version
	Hash    string    `json:"hash"`
	Size    int       `json:"size"`
	Updated time.Time `json:"updated"`
}

type SheetListResult struct {
	Sheets []SheetInfo `json:"sheets"`
}

type SubscribeParams struct {
	Channel string `json:"channel"`
	From    int64  `json:"from"` // first per-channel seq to deliver (0 = from start)
	// Tail, for a subscription from the start, delivers only the channel's
	// last Tail events: what a client needs to draw a chat that opens at its
	// end. The agents and prompts come from reconcile, not from history. 0
	// delivers everything.
	Tail int `json:"tail,omitempty"`
}

// ReconcileResult is the authoritative snapshot (PRD §9).
type ReconcileResult struct {
	Channel ChannelInfo  `json:"channel"`
	Agents  []AgentInfo  `json:"agents"`
	Prompts []PromptInfo `json:"prompts"`
	Seq     int64        `json:"seq"`
}
