package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every key the TUI acts on is a binding in keys.go. A handler that compares
// the key's text instead ("case \"k\":") works, and is invisible to anything
// that reads the bindings: a legend, a remap, a search for who uses ctrl+d.
func TestKeysAreReadThroughTheirBindings(t *testing.T) {
	raw := regexp.MustCompile(`msg\.String\(\)|\bkey (==|!=) "`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "keys.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if raw.MatchString(line) && !strings.Contains(line, "// not a binding:") {
				t.Errorf("%s:%d compares a key's text; add a binding to keys.go and use key.Matches:\n\t%s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
