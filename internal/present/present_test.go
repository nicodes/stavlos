package present

import (
	"slices"
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
