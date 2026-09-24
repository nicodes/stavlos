package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	s, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	ProbeSandbox = func() (sandbox.Level, error) { return sandbox.None, nil }
	cfg := *s.Config()
	cfg.Sandbox.Enabled = true
	if !unsandboxed(&cfg) || sandboxLevel(&cfg) != "none" {
		t.Fatal("a wanted sandbox the kernel cannot give was not reported")
	}
	cfg.Sandbox.Enabled = false
	if unsandboxed(&cfg) || sandboxLevel(&cfg) != "off" {
		t.Fatal("a sandbox the human turned off was treated as missing")
	}
	// The limited level hides nothing: a command can have a session service
	// start a process for it outside the sandbox, so it is bare too, and
	// the prompt says why.
	ProbeSandbox = func() (sandbox.Level, error) { return sandbox.Landlock, nil }
	cfg.Sandbox.Enabled = true
	if !unsandboxed(&cfg) || sandboxLevel(&cfg) != "limited" || !strings.Contains(bareWhy(&cfg), "hides nothing") {
		t.Fatalf("limited: unsandboxed=%v level=%s why=%q", unsandboxed(&cfg), sandboxLevel(&cfg), bareWhy(&cfg))
	}
	ProbeSandbox = func() (sandbox.Level, error) { return sandbox.Full, nil }
	if unsandboxed(&cfg) {
		t.Fatal("a full sandbox was treated as bare")
	}
}

// TestHiddenPathsCoverOtherToolsAndBrowsers: what a command could read and
// send out in one step is hidden, not only the classic key stores.
func TestHiddenPathsCoverOtherToolsAndBrowsers(t *testing.T) {
	for _, want := range []string{".claude", ".codex", ".mozilla", ".config/google-chrome", ".bash_history", ".pgpass", ".vault-token"} {
		if !slices.Contains(sandboxHiddenHome, want) {
			t.Errorf("%s is not hidden", want)
		}
	}
}
