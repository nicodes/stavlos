package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSharedSkillsAreReadOnlyAndChangesAreReported(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("STAVLOS_SKILLS_DIR", shared)
	c, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	cfg := *c.Config()
	cfg.Sandbox.Enabled = true
	project := filepath.Join(c.Dir(), ".agents")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := c.sandboxSpec(&cfg)
	if !slices.Contains(spec.ReadOnly, project) || !slices.Contains(spec.ReadOnly, shared) || !slices.Contains(spec.Readable, shared) {
		t.Fatalf("shared library is not protected/readable: %+v", spec)
	}
	before := c.stampControl(&cfg)
	for _, root := range []string{project, shared} {
		if err := os.WriteFile(filepath.Join(root, "resource"), []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	note := c.controlNote(before, &cfg)
	if !strings.Contains(note, project) || !strings.Contains(note, shared) {
		t.Fatalf("changed skill control paths went unreported: %s", note)
	}
}
