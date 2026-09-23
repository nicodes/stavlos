package protocol

// Channels: what a client is told of one, and what it may ask of it.

type ChannelInfo struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"` // unique across the daemon, shown as #name
	Dir          string  `json:"dir"`
	DirError     string  `json:"dir_error,omitempty"` // directory unavailable; history remains accessible
	Model        string  `json:"model"`
	RootAgent    string  `json:"root_agent"`
	Created      string  `json:"created"`
	Archived     bool    `json:"archived"`
	Seq          int64   `json:"seq"` // latest per-channel sequence
	Live         int     `json:"live_agents"`
	CostUSD      float64 `json:"cost_usd"`
	Tokens       int     `json:"tokens"` // input + output tokens every agent of the channel has used
	TrustPending bool    `json:"trust_pending"`
	// Sandbox is what bounds this channel's commands: "full", "limited"
	// (writes and network bounded, nothing hidden), "none" (the kernel offers
	// nothing: every command asks) or "off" (turned off in stavlos.json).
	Sandbox     string       `json:"sandbox,omitempty"`
	TrustFiles  int          `json:"trust_files,omitempty"` // files the project layer's trust hash covers; 0 when the directory has no project configuration
	Mode        string       `json:"mode"`                  // permission mode: ask | auto | yolo
	Recap       int          `json:"recap,omitempty"`       // minutes of silence before a recap is asked for; 0 is off
	State       ChannelState `json:"state,omitempty"`       // working (an agent runs) | waiting (one expects an answer) | idle; "" for a channel not in memory
	Title       string       `json:"title,omitempty"`       // the first human prompt, for pickers
	Dirs        []DirInfo    `json:"dirs,omitempty"`        // the working directories every agent shares, the channel directory first
	Permissions int          `json:"permissions,omitempty"` // permission and trust prompts waiting on the human (channel.list)
	Questions   int          `json:"questions,omitempty"`   // questions waiting on the human (channel.list)
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

// ChannelSetRecapParams sets a channel's recap timer: after this many
// minutes without the human hearing from it, and with work done since the
// last one, its main agent is asked for a status report. 0 turns it off.
type ChannelSetRecapParams struct {
	Channel string `json:"channel"`
	Minutes int    `json:"minutes"`
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
