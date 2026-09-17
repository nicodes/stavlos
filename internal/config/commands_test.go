package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCommandDescriptionOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.md")
	for _, tc := range []struct{ text, problem string }{
		{"---\ndescription: Run tests with coverage\n---\n\nRun the tests.\nShow failures.\n", ""},
		{"---\ndescription: Tests\nagent: build\n---\nRun tests", "unsupported command field"},
		{"---\ndescription: Tests\nmodel: provider/model\n---\nRun tests", "unsupported command field"},
		{"---\ndescription: Tests\nunknown: true\n---\nRun tests", "unsupported command field"},
		{"Run tests", "description"},
		{"---\ndescription: 123\n---\nRun tests", "description"},
		{"---\ndescription: Tests\n---\n \n", "prompt body"},
		{"---\ndescription: Tests", "unterminated"},
	} {
		if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := ReadCommand(path)
		if tc.problem != "" {
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("expected %q for %q, got %v", tc.problem, tc.text, err)
			}
			continue
		}
		if err != nil || c.Name != "coverage" || c.Description != "Run tests with coverage" || c.Body != "Run the tests.\nShow failures." {
			t.Fatalf("command: %+v %v", c, err)
		}
	}
}

func TestCommandsFollowConfigLayersAndTrust(t *testing.T) {
	global, project := t.TempDir(), t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", global)
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\ndescription: Tests\n---\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(global, "commands", "cmd.md"), "Global prompt")
	file := filepath.Join(project, ".stavlos", "commands", "cmd.md")
	write(file, "Project prompt")
	before, err := Load(project, noTrust{})
	if err != nil || !before.TrustPending || before.Commands["cmd"].Body != "Global prompt" {
		t.Fatalf("untrusted commands: %+v %v", before, err)
	}
	after, err := Load(project, allTrust{})
	if err != nil || after.Commands["cmd"].Body != "Project prompt" {
		t.Fatalf("trusted commands: %+v %v", after, err)
	}
	_, oldHash, err := ProjectHash(project)
	if err != nil {
		t.Fatal(err)
	}
	write(file, "Changed prompt")
	_, newHash, err := ProjectHash(project)
	if err != nil || newHash == oldHash {
		t.Fatal("command content did not participate in project trust")
	}
}
