package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/sandbox"
)

// What the sandbox hides from commands, the file tools are refused too, in
// every mode; what the channel keeps for itself under those directories (its
// sheets) stays open to it.
func TestFileToolsAreRefusedWhatIsHidden(t *testing.T) {
	s, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	cfg := s.Config()
	home, _ := os.UserHomeDir()
	for path, hidden := range map[string]bool{
		filepath.Join(home, ".ssh", "id_ed25519"):        true,
		filepath.Join(paths.DataDir(), "events.db"):      true,
		filepath.Join(paths.ConfigDir(), "stavlos.json"): true,
		filepath.Join(s.SheetDir(), "s1.html"):           false,
		filepath.Join(s.Dir(), "main.go"):                false,
		"/etc/hosts":                                     false,
	} {
		if got := s.hiddenFrom(policy.Path(path), cfg) != ""; got != hidden {
			t.Errorf("%s: hidden %v, want %v", path, got, hidden)
		}
	}
	if s.hiddenFrom(policy.Command("cat ~/.ssh/id_ed25519"), cfg) != "" {
		t.Error("a command is the sandbox's to bound, not this check's")
	}
}

// A kernel that offers no sandbox makes commands bare, unless the human
// turned the sandbox off themselves: that is a choice, not a surprise.
func TestNoSandboxIsBareOnlyWhenItWasWanted(t *testing.T) {
	prev := probeSandbox
	probeSandbox = func() (sandbox.Level, error) { return sandbox.None, nil }
	defer func() { probeSandbox = prev }()
	s, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	cfg := *s.Config()
	cfg.Sandbox.Enabled = true
	if !unsandboxed(&cfg) || sandboxLevel(&cfg) != "none" {
		t.Fatal("a wanted sandbox the kernel cannot give was not reported")
	}
	cfg.Sandbox.Enabled = false
	if unsandboxed(&cfg) || sandboxLevel(&cfg) != "off" {
		t.Fatal("a sandbox the human turned off was treated as missing")
	}
}
