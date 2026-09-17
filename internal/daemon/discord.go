package daemon

import "github.com/nicodes/stavlos/internal/protocol"

// DiscordService is the background integration supplied by the executable.
// Keeping this seam here avoids making the agent daemon depend on its client
// implementation and permits lifecycle tests without Discord credentials.
type DiscordService interface {
	Start()
	Status() protocol.DiscordStatus
	Connect() (protocol.DiscordStatus, error)
	Disconnect() (protocol.DiscordStatus, error)
	Close()
}
