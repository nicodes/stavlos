package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMarkdownPartsIsOneLenientSplitter: the role and command readers
// (frontmatter) and the config editor (markdownParts) split a file the
// same way: a byte order mark and CRLF line ends are accepted, and either
// "---" line may carry trailing blanks.
func TestMarkdownPartsIsOneLenientSplitter(t *testing.T) {
	for _, tc := range []struct{ in, meta, body, problem string }{
		{"---\ndescription: A\n---\nBody\n", "description: A", "Body\n", ""},
		{"\uFEFF---  \r\ndescription: A\r\n--- \r\nBody\r\n", "description: A", "Body\n", ""},
		{"---\ndescription: A\n---", "description: A", "", ""},
		{"---\ndescription: A\n---\n", "description: A", "", ""},
		{"Body only\n", "", "Body only\n", ""},
		{"--- not frontmatter\n", "", "--- not frontmatter\n", ""},
		{"---\ndescription: A\n", "", "", "unterminated frontmatter"},
	} {
		meta, body, err := markdownParts(tc.in)
		if tc.problem != "" {
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Errorf("%q: %v", tc.in, err)
			}
			continue
		}
		if err != nil || meta != tc.meta || body != tc.body {
			t.Errorf("%q: %q %q %v", tc.in, meta, body, err)
		}
	}
	// Both call sites see the lenient rule: a role with trailing blanks
	// and CRLF reads, and the editor's field write keeps such a body.
	dir := t.TempDir()
	role := filepath.Join(dir, "lead.md")
	if err := os.WriteFile(role, []byte("\uFEFF--- \r\ndescription: Leads\r\n---\t\r\nYou lead.\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadRole(role); err != nil || r.Description != "Leads" || r.Body != "You lead." {
		t.Fatalf("role: %+v %v", r, err)
	}
	out, err := EditConfigField("commands/x.md", "--- \r\ndescription: Old\r\n--- \r\nBody\r\n", []string{"description"}, json.RawMessage(`"New"`))
	if err != nil || out != "---\ndescription: \"New\"\n---\nBody\n" {
		t.Fatalf("editor: %q %v", out, err)
	}
}

// TestRoleLoopRemoved: a role's loop: key, never read, is refused like the
// other retired keys rather than silently kept.
func TestRoleLoopRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lead.md")
	if err := os.WriteFile(path, []byte("---\ndescription: Leads\nloop: default\n---\nYou lead.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRole(path); err == nil || !strings.Contains(err.Error(), "loop: was removed") {
		t.Fatalf("loop: %v", err)
	}
	for _, f := range EditorFields("agents/lead.md", "---\ndescription: Leads\n---\n") {
		if f.Path[0] == "loop" {
			t.Fatal("the editor still offers loop")
		}
	}
}
