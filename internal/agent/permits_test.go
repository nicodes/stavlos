package agent

import (
	"encoding/json"
	"testing"

	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestPrefixFor(t *testing.T) {
	if prefixFor(policy.KindURL, "https://Docs.Go.dev/pkg/x?y") != "docs.go.dev" || prefixFor(policy.KindURL, "pkg.go.dev/net") != "pkg.go.dev" || prefixFor(policy.KindPath, "/x") != "" || prefixFor(policy.KindCommand, "go test ./x") != "go test" || prefixFor(policy.KindCommand, "go test; rm x") != "" || prefixFor(policy.KindText, "x") != "" || prefixFor(policy.KindID, "a1") != "" {
		t.Fatal("prefixFor")
	}
	if !prefixCovers(policy.KindURL, "docs.go.dev", "https://docs.go.dev/other") || prefixCovers(policy.KindURL, "docs.go.dev", "https://evil.docs.go.dev/") || prefixCovers(policy.KindURL, "", "https://x/") || !prefixCovers(policy.KindCommand, "go test", "go test ./...") || prefixCovers(policy.KindCommand, "go test", "go test; rm x") || prefixCovers(policy.KindPath, "/", "/x") {
		t.Fatal("prefixCovers")
	}
	var p permits
	p.apply(event.PermitPayload{Tool: "shell", Prefix: "go test"})
	p.apply(event.PermitPayload{Tool: "shell", Prefix: "go test"}) // once
	p.apply(event.PermitPayload{Tool: "read", Call: "/etc/hosts"})
	if len(p.prefixes["shell"]) != 1 || !p.covers("shell", policy.Command("go test ./...")) || p.covers("shell", policy.Command("go build")) || !p.covers("read", policy.Path("/etc/hosts")) || p.covers("read", policy.Path("/etc/passwd")) {
		t.Fatal("permits")
	}
	// A patch is covered only when every path it touches is.
	p.apply(event.PermitPayload{Tool: "apply_patch", Call: "a.go"})
	if !p.covers("apply_patch", policy.Path("a.go")) || p.covers("apply_patch", policy.Path("a.go", "../../.bashrc")) || p.covers("apply_patch", policy.Path()) {
		t.Fatal("a permit for one path covered another")
	}
}

// TestAlwaysAllowNeverCoversAControlFile (1C.2): an edit to a file that
// steers the harness asks every time. "Allow for this channel" on one such
// edit must not become a standing licence to rewrite it: the second edit of
// the same file asks again.
func TestAlwaysAllowNeverCoversAControlFile(t *testing.T) {
	patch := func(text string) string {
		b, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Add File: AGENTS.md\n+" + text + "\n*** End Patch"})
		return string(b)
	}
	update := func() string {
		b, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Update File: AGENTS.md\n@@\n-first\n+second\n*** End Patch"})
		return string(b)
	}
	fm := &fakeModel{steps: []step{
		reply(call("c1", "apply_patch", patch("first"))),
		reply(call("c2", "apply_patch", update())),
		reply(text("done")),
	}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","policy":{"apply_patch":"allow"}}`}, fm)
	h.answerWith(escalation.Answer{Value: protocol.AnswerAllowAlways})
	runTurn(t, s, h, "edit the instructions twice")
	if n := h.promptCount(); n != 2 {
		t.Fatalf("%d prompts for two edits of AGENTS.md: a remembered allow answered a control-file ask", n)
	}
	for _, p := range h.prompts {
		if !p.Sticky {
			t.Fatalf("a control-file prompt is not marked sticky: %+v", p)
		}
	}
}
