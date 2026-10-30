package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nicodes/stavlos/internal/paths"
)

func sharedSkillFixture(t *testing.T, root, name, body string) {
	t.Helper()
	file := filepath.Join(root, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("---\nname: "+name+"\ndescription: fixture\n---\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSharedSkillLayersRespectTrustAndPrecedence(t *testing.T) {
	global, shared, project := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", global)
	t.Setenv("STAVLOS_SKILLS_DIR", shared)
	sharedSkillFixture(t, shared, "global-only", "user shared")
	sharedSkillFixture(t, shared, "scope", "user shared")
	sharedSkillFixture(t, filepath.Join(global, "skills"), "scope", "user native")
	sharedSkillFixture(t, paths.ProjectSharedSkillsDir(project), "scope", "project shared")
	sharedSkillFixture(t, paths.ProjectSharedSkillsDir(project), "project-only", "project shared")

	untrusted, err := Load(project, noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if untrusted.ProjectTrusted || !untrusted.TrustPending || untrusted.Skills["scope"].Body != "user native" {
		t.Fatalf("untrusted project changed global skills: %+v", untrusted.Skills)
	}
	if _, exists := untrusted.Skills["project-only"]; exists {
		t.Fatal("untrusted shared skill loaded")
	}
	trusted, err := Load(project, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if !trusted.ProjectTrusted || trusted.Skills["scope"].Body != "project shared" || trusted.Skills["global-only"].Body != "user shared" {
		t.Fatalf("shared project did not override global definitions: %+v", trusted.Skills)
	}
	if trusted.Skills["scope"].Dir != filepath.Join(paths.ProjectSharedSkillsDir(project), "scope") {
		t.Fatal("skill resource directory does not identify the shared package")
	}
	sharedSkillFixture(t, filepath.Join(project, ".stavlos", "skills"), "scope", "project native")
	trusted, err = Load(project, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if trusted.Skills["scope"].Body != "project native" {
		t.Fatal("native project override lost")
	}
}

type exactSharedTrust string

func (h exactSharedTrust) Trusted(_ string, hash string) bool { return string(h) == hash }

func TestSharedSkillBodyAndResourcesInvalidateTrustAndUseSnapshotBytes(t *testing.T) {
	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	t.Setenv("STAVLOS_SKILLS_DIR", t.TempDir())
	project := t.TempDir()
	root := paths.ProjectSharedSkillsDir(project)
	sharedSkillFixture(t, root, "deploy", "reviewed body")
	resource := filepath.Join(root, "deploy", "scripts", "deploy.sh")
	if err := os.MkdirAll(filepath.Dir(resource), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte("reviewed resource"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := TakeSnapshot(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ path, value string }{
		{filepath.Join(root, "deploy", "SKILL.md"), "---\nname: deploy\ndescription: fixture\n---\nchanged body"},
		{resource, "changed resource"},
	} {
		before, err := TakeSnapshot(project)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(change.path, []byte(change.value), 0o644); err != nil {
			t.Fatal(err)
		}
		next, err := TakeSnapshot(project)
		if err != nil {
			t.Fatal(err)
		}
		if next.Hash == before.Hash {
			t.Fatal("shared skill change was not trust-gated")
		}
		e, err := Load(project, exactSharedTrust(before.Hash))
		if err != nil {
			t.Fatal(err)
		}
		if !e.TrustPending || e.ProjectTrusted {
			t.Fatal("stale trust accepted")
		}
		if _, exists := e.Skills["deploy"]; exists {
			t.Fatal("changed skill loaded with stale trust")
		}
	}
	e := &Effective{Skills: map[string]Skill{}}
	if err := e.loadSkills(snap, root); err != nil {
		t.Fatal(err)
	}
	if e.Skills["deploy"].Body != "reviewed body" {
		t.Fatal("loader reread changed skill bytes")
	}
	b, err := snap.ReadFile(resource)
	if err != nil || string(b) != "reviewed resource" {
		t.Fatal("snapshot did not retain reviewed resources")
	}
}

func TestSharedSkillSnapshotRefusesUnsafeResourceTypes(t *testing.T) {
	for _, kind := range []string{"fifo", "directory-link", "parent-link", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			project := t.TempDir()
			root := paths.ProjectSharedSkillsDir(project)
			sharedSkillFixture(t, root, "deploy", "fixture")
			path := filepath.Join(root, "deploy", "resource")
			switch kind {
			case "fifo":
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "parent-link":
				parent := filepath.Dir(root)
				original := filepath.Join(project, "original-agents")
				if err := os.Rename(parent, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original, parent); err != nil {
					t.Fatal(err)
				}
			case "directory-link":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(snapshotMaxFile + 1)
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("fixture: %v %v", err, closeErr)
				}
			}
			_, err := TakeSnapshot(project)
			if err == nil {
				t.Fatal("unsafe shared resource accepted")
			}
			if kind != "oversized" && kind != "parent-link" && !strings.Contains(err.Error(), "not a regular file") {
				t.Fatal(err)
			}
		})
	}
}
