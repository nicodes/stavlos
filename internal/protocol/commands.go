package protocol

const (
	MCommandList = "command.list"
	MCommandRun  = "command.run"
)

var (
	CommandList = Method[ChannelRef, CommandListResult]{MCommandList}
	CommandRun  = Method[CommandRunParams, None]{MCommandRun}
)

type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type CommandListResult struct {
	Commands []CommandInfo `json:"commands"`
}

type CommandRunParams struct {
	Channel string `json:"channel"`
	Name    string `json:"name"`
	Agent   string `json:"agent,omitempty"` // omitted: post to the channel's root
}
