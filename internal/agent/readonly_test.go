package agent

import (
	"encoding/json"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/sandbox"
)

func TestReadOnlyRoleCannotUsePermissiveModeToWrite(t *testing.T) {
	c, _ := newTestChannel(t, testConfig{json: `{"model":"fake/m1","mode":"yolo","sandbox":{"enabled":false}}`}, &fakeModel{})
	a := c.Root()
	rv := a.role()
	rv.def.ReadOnly = true
	for _, name := range []string{"patch", "sheet", "shell"} {
		tool, ok := c.tools[name]
		if !ok {
			t.Fatalf("missing tool %s", name)
		}
		block := model.Block{Name: name, Input: json.RawMessage(`{"command":"echo changed > file","patch":"","action":"create"}`)}
		if got := a.decide(block, tool, rv, c.Config()).verb; got != policy.Deny {
			t.Fatalf("read-only %s admitted in yolo with sandbox off: %s", name, got)
		}
	}
	cfg := *c.Config()
	cfg.Sandbox.Enabled = true
	ProbeSandbox = func() (sandbox.Level, error) { return sandbox.Landlock, nil }
	block := model.Block{Name: "shell", Input: json.RawMessage(`{"command":"echo changed > file"}`)}
	if got := a.decide(block, c.tools["shell"], rv, &cfg).verb; got != policy.Deny {
		t.Fatalf("limited sandbox admitted read-only shell: %s", got)
	}
}
