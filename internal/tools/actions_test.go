package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestMergedToolSurfaceAndInvalidActions(t *testing.T) {
	set := Builtin()
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{"agent", "ask", "channel", "glob", "grep", "message", "patch", "read", "sheet", "shell", "skill", "todo", "web"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tools %v", names)
	}
	for _, tc := range []struct{ name, input string }{
		{"agent", `{}`}, {"agent", `{"action":"create","label":"child"}`}, {"agent", `{"action":"cancel"}`}, {"agent", `{"action":"delete"}`},
		{"web", `{}`}, {"web", `{"action":"fetch"}`}, {"web", `{"action":"search"}`}, {"web", `{"action":"delete"}`},
		{"shell", `{"action":"kill"}`}, {"shell", `{"action":"delete","command":"echo should-not-run"}`},
	} {
		r := set[tc.name].Run(context.Background(), json.RawMessage(tc.input), &Env{})
		if !r.IsError {
			t.Errorf("%s %s accepted: %+v", tc.name, tc.input, r)
		}
	}
	var schema struct {
		Properties map[string]struct{ Enum []string }
	}
	if err := json.Unmarshal(AgentDef(false).Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(schema.Properties["action"].Enum, []string{"status"}) {
		t.Fatalf("nondelegating actions: %+v", schema)
	}
}
