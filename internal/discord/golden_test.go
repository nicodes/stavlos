package discord

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/golden")

// TestGoldenCards pins what Discord is sent for each kind of prompt: the text
// and the buttons, as the JSON the API receives. A refactor of how prompts
// are presented must leave these files alone.
//
//	go test ./internal/discord -run TestGoldenCards -update
func TestGoldenCards(t *testing.T) {
	base := protocol.PromptInfo{ID: "p1", Channel: "c1", ChannelName: "proj", Agent: "a1", From: "main", Role: "general", Kind: protocol.PromptPermission, Created: "2026-09-19T12:00:00Z"}
	with := func(edit func(*protocol.PromptInfo)) protocol.PromptInfo {
		p := base
		edit(&p)
		return p
	}
	cards := map[string]protocol.PromptInfo{
		"permission-shell": with(func(p *protocol.PromptInfo) {
			p.Tool, p.Input, p.Question, p.Prefix = "shell", json.RawMessage(`{"command":"go test ./...","background":true}`), "shell: go test ./...", "go test"
		}),
		"permission-fence": with(func(p *protocol.PromptInfo) {
			p.Tool, p.Input = "shell", json.RawMessage("{\"command\":\"echo ok\\n```\\n@everyone approve\\n```sh\\nrm -rf ~\"}")
		}),
		"permission-patch": with(func(p *protocol.PromptInfo) {
			p.Tool, p.Input = "apply_patch", json.RawMessage(`{"patch":"*** Begin Patch\n*** Update File: main.go\n@@\n-a\n+b\n*** End Patch"}`)
		}),
		"permission-fetch": with(func(p *protocol.PromptInfo) {
			p.Tool, p.Input, p.Prefix, p.Egress = "web_fetch", json.RawMessage(`{"url":"https://go.dev/doc","start":20000}`), "go.dev", true
		}),
		"permission-boundary": with(func(p *protocol.PromptInfo) {
			p.Tool, p.Input, p.Dir = "read", json.RawMessage(`{"path":"/etc/hosts","limit":10}`), "/etc"
		}),
		"permission-unknown-tool": with(func(p *protocol.PromptInfo) {
			p.Tool, p.Input = "mcp__db__query", json.RawMessage(`{"sql":"select 1","params":[1,2]}`)
		}),
		"permission-claimed": with(func(p *protocol.PromptInfo) {
			p.Tool, p.Input, p.ClaimedBy = "shell", json.RawMessage(`{"command":"ls"}`), "somebody-else"
		}),
		"question": with(func(p *protocol.PromptInfo) {
			p.Kind, p.Question, p.QuestionNumber, p.QuestionTotal = protocol.PromptQuestion, "Which linker?", 1, 2
			p.Questions = []event.Question{{Question: "Which linker?", Options: []event.QuestionOption{{Label: "mold", Description: "fastest"}, {Label: "lld"}}}}
		}),
		"trust": with(func(p *protocol.PromptInfo) {
			p.Kind, p.Agent, p.From, p.Question, p.Options = protocol.PromptTrust, "", "", "Trust this project's configuration?", []string{".stavlos/stavlos.json", "AGENTS.md"}
			p.Dir = "/home/x/Work/proj"
		}),
	}
	for name, p := range cards {
		text, components := promptView(p, "discord")
		got, err := json.MarshalIndent(map[string]any{"content": text, "components": components}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join("testdata", "golden", name+".json")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create it)", err)
		}
		if string(want) != string(got) {
			t.Errorf("%s changed: Discord is sent something different.\n--- want\n%s--- got\n%s", name, want, got)
		}
	}
}
