package protocol

// The command sandbox, as a client shows it: two states a person chooses
// between (on, off) and one the machine imposes (limited, or none).

// SandboxStatus is what bounds the commands agents run.
type SandboxStatus struct {
	Enabled bool `json:"enabled"` // the global setting; a trusted project may say otherwise for its channels
	// Level is what this machine can enforce: "full", "limited" (writes and
	// the network are bounded, nothing is hidden) or "none".
	Level   string   `json:"level"`
	Missing []string `json:"missing,omitempty"` // what a full sandbox would stop and this one does not
	Why     string   `json:"why,omitempty"`     // what the system said
	Fix     string   `json:"fix,omitempty"`     // a command that usually mends it
}

type SandboxSetParams struct {
	Enabled bool `json:"enabled"`
}
