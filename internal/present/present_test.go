package present

import (
	"flag"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
)

func TestAgentChangesSayWhyTheHarnessMovedAModel(t *testing.T) {
	got := AgentChanges(event.AgentUpdatedPayload{Model: event.Str("zai/glm-5.3"), Variant: event.Str(""), Reason: "openai is at its limit until 14:10"})
	if len(got) != 2 || got[0].String() != "model → zai/glm-5.3 · openai is at its limit until 14:10" || got[1].String() != "variant → default" {
		t.Fatalf("%+v", got)
	}
	if AgentChanges(event.AgentUpdatedPayload{}) != nil {
		t.Fatal("nothing changed, and something was said")
	}
}

func TestAddressing(t *testing.T) {
	for _, c := range []struct {
		to   []string
		text string
		want string
	}{
		{nil, "hi", "hi"},
		{[]string{"scout"}, "", "@scout"},
		{[]string{"scout", "lint"}, "look", "@scout @lint look"},
		{UnlessOnly([]string{"main"}, "main"), "hi", "hi"},
		{UnlessOnly([]string{"main", "scout"}, "main"), "hi", "@main @scout hi"},
		{Without([]string{"user", "scout"}, "user"), "hi", "@scout hi"},
		{Without([]string{"user"}, "user"), "hi", "hi"},
	} {
		if got := Addressed(c.to, c.text); got != c.want {
			t.Errorf("Addressed(%v, %q) = %q, want %q", c.to, c.text, got, c.want)
		}
	}
	to := []string{"user", "scout"}
	if Without(to, "user"); !slices.Equal(to, []string{"user", "scout"}) {
		t.Fatal("Without changed its argument")
	}
}

func TestEveryToolWithAPrimaryArgumentIsKnown(t *testing.T) {
	for tool, key := range PrimaryArgs() {
		if key == "" || PrimaryArg(tool) != key {
			t.Errorf("%s: %q", tool, key)
		}
	}
}

var update = flag.Bool("update", false, "rewrite the browser's copy of the tool table")

// The browser's copy of which argument stands for a tool call is written from
// the table here, so its one-line summaries are the terminal's and Discord's.
//
//	go test ./internal/present -update
func TestTheBrowsersToolTableIsCurrent(t *testing.T) {
	const path = "../../web/src/core/tools.gen.ts"
	args := PrimaryArgs()
	var b strings.Builder
	b.WriteString("// Code generated from internal/present; DO NOT EDIT.\n// go test ./internal/present -update\n\nexport const primaryArg: Record<string, string> = {\n")
	for _, tool := range slices.Sorted(maps.Keys(args)) {
		b.WriteString("  " + tool + ": " + strconv.Quote(args[tool]) + ",\n")
	}
	b.WriteString("};\n")
	if *update {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if have, err := os.ReadFile(path); err != nil || string(have) != b.String() {
		t.Fatalf("%s is stale (%v): run with -update, then rebuild the web client", path, err)
	}
}
