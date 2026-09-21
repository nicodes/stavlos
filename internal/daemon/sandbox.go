package daemon

import (
	"fmt"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/sandbox"
)

// sandboxStatus is the global setting and what this machine can enforce.
func sandboxStatus() (protocol.SandboxStatus, error) {
	cfg, err := config.LoadGlobal()
	if err != nil {
		return protocol.SandboxStatus{}, err
	}
	r := sandbox.Explain()
	return protocol.SandboxStatus{Enabled: cfg.Sandbox.Enabled, Level: sandboxLevelName(r.Level), Missing: r.Missing, Why: r.Why, Fix: r.Fix}, nil
}

// sandboxLevelName is a level as people are told it. "Landlock" is how the
// limited level is built, not something anybody should have to know.
func sandboxLevelName(l sandbox.Level) string {
	if l == sandbox.Landlock {
		return "limited"
	}
	return l.String()
}

// SetSandbox turns the command sandbox on or off for this user and has every
// channel load its configuration again, so the next command runs under it.
// Only the owner may call it (its route says so): an agent cannot, and the
// file tools are refused the configuration directory besides.
func (d *Daemon) SetSandbox(enabled bool) (protocol.SandboxStatus, error) {
	d.editorMu.Lock()
	defer d.editorMu.Unlock()
	if err := config.SetGlobalSandbox(enabled); err != nil {
		return protocol.SandboxStatus{}, fmt.Errorf("saving the setting: %w", err)
	}
	d.reloadEditedConfig(protocol.ConfigScope{Scope: "system"}, "")
	return sandboxStatus()
}
