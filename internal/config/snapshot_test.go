package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type trustAll struct{}

func (trustAll) Trusted(string, string) bool { return true }

// What is loaded is what was hashed: the loaders of a project layer read the
// snapshot, never the disk a second time.
func TestAProjectIsLoadedFromTheBytesThatWereHashed(t *testing.T) {
	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	write := func(rel, text string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".stavlos/agents/reviewer.md", "---\ndescription: reviews\n---\nhashed body")
	write(".stavlos/skills/deploy/SKILL.md", "---\ndescription: deploys\n---\nhashed skill")
	write(".stavlos/commands/ship.md", "---\ndescription: ships\n---\nhashed command")
	write("AGENTS.md", "hashed instructions")

	snap, err := TakeSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	// the repository changes after the hash was taken
	write(".stavlos/agents/reviewer.md", "---\ndescription: reviews\n---\nSWAPPED")
	write(".stavlos/skills/deploy/SKILL.md", "---\ndescription: deploys\n---\nSWAPPED")
	write("AGENTS.md", "SWAPPED")

	e := &Effective{Presets: map[string]Preset{}, Skills: map[string]Skill{}}
	pdir := filepath.Join(dir, ".stavlos")
	if err := e.loadPresets(snap, filepath.Join(pdir, "agents"), "project"); err != nil {
		t.Fatal(err)
	}
	if err := e.loadSkills(snap, filepath.Join(pdir, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := e.loadCommands(snap, filepath.Join(pdir, "commands")); err != nil {
		t.Fatal(err)
	}
	if got := e.Presets["reviewer"].Body; got != "hashed body" {
		t.Errorf("role body %q", got)
	}
	if got := e.Skills["deploy"].Body; got != "hashed skill" {
		t.Errorf("skill body %q", got)
	}
	if got := e.Commands["ship"].Body; got != "hashed command" {
		t.Errorf("command body %q", got)
	}
	if len(snap.chain) != 1 || snap.chain[0].Text != "hashed instructions" {
		t.Errorf("instructions %+v", snap.chain)
	}

	// and a whole Load agrees with the hash it reports
	full, err := Load(dir, trustAll{})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, _ := ProjectHash(dir)
	if full.TrustHash != hash || !full.ProjectTrusted || full.Presets["reviewer"].Body != "SWAPPED" {
		t.Errorf("load: hash %q vs %q, trusted %v, body %q", full.TrustHash, hash, full.ProjectTrusted, full.Presets["reviewer"].Body)
	}
}

// A clone cannot stall or exhaust the daemon before anybody is asked
// anything: a FIFO is refused, not read for ever, and size is bounded.
func TestASnapshotRefusesWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".stavlos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, ".stavlos", "stavlos.json"), 0o644); err != nil {
		t.Skip(err)
	}
	if _, err := TakeSnapshot(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a FIFO as configuration: %v", err)
	}
}
