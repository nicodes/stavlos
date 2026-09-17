package discord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestPermissionSubjectsAreReadableAndComplete(t *testing.T) {
	for _, tc := range []struct{ tool, input, want string }{
		{"apply_patch", `{"patch":"*** Begin Patch\n*** Update File: AGENTS.md\n@@\n-old\n+new\n*** End Patch"}`, "```diff\n*** Begin Patch\n*** Update File: AGENTS.md\n@@\n-old\n+new\n*** End Patch\n```"},
		{"web_fetch", `{"url":"https://example.com/docs?a=1&b=2","start":100}`, "```text\nhttps://example.com/docs?a=1&b=2\nStart: 100\n```"},
		{"web_search", `{"query":"how does this work?","n":5}`, "```text\nhow does this work?\nResults: 5\n```"},
		{"read", `{"path":"/outside/file.txt","offset":10,"limit":20}`, "```text\n/outside/file.txt\nFirst line: 10\nLine limit: 20\n```"},
		{"grep", `{"pattern":"log.*Error","path":"src","glob":"*.go","ignore_case":true,"limit":50}`, "```text\nPattern: log.*Error\nPath: src\nFile glob: *.go\nIgnore case: true\nMatch limit: 50\n```"},
		{"glob", `{"pattern":"**/*.go","path":"/outside","limit":20}`, "```text\nPattern: **/*.go\nPath: /outside\nPath limit: 20\n```"},
		{"skill", `{"name":"review"}`, "```text\nSkill: review\n```"},
		{"agent_cancel", `{"id":"scout"}`, "```text\nTarget: scout\n```"},
		{"shell_kill", `{"id":"job-1"}`, "```text\nTarget: job-1\n```"},
		{"mcp__github__create_issue", `{"title":"Fix it","id":9007199254740993}`, "```json\n{\n  \"title\": \"Fix it\",\n  \"id\": 9007199254740993\n}\n```"},
		{"other_tool", `{"nested":{"enabled":true}}`, "```json\n{\n  \"nested\": {\n    \"enabled\": true\n  }\n}\n```"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			p := protocol.PromptInfo{ID: "p", Kind: protocol.PromptPermission, Tool: tc.tool, Input: json.RawMessage(tc.input), Dir: "/outside"}
			if body := permissionSubject(p); body != tc.want {
				t.Fatalf("subject:\n%s\nwant:\n%s", body, tc.want)
			}
			text, controls := promptView(p, "client")
			if !strings.Contains(text, "** /outside\n"+tc.want) || strings.Contains(text, "Directory:") || strings.Contains(text, "Permission:") || len(controls) != 1 || len(controls[0].(dg.ActionsRow).Components) != 4 {
				t.Fatalf("card: %s", text)
			}
			titles := map[string]string{"apply_patch": "Apply patch", "web_fetch": "Web fetch", "web_search": "Web search", "mcp__github__create_issue": "Github · Create issue"}
			if title := titles[tc.tool]; title != "" && !strings.HasPrefix(text, "❗ **"+title+"**") {
				t.Fatalf("heading should use a readable tool name: %s", text)
			}
			answer := recordedPermissionResult(p, event.AskResolvedPayload{Outcome: event.AskAnswered, Answer: protocol.AnswerAllow})
			head, _, _ := strings.Cut(answer, "\n")
			if !strings.Contains(answer, tc.want) || !strings.HasSuffix(head, " ✔️ **Allowed once**") || !strings.HasSuffix(answer, "\n```") {
				t.Fatalf("result lost its readable subject: %s", answer)
			}
		})
	}
}

func TestPermissionFormattingKeepsUnexpectedArguments(t *testing.T) {
	p := protocol.PromptInfo{Tool: "web_fetch", Input: json.RawMessage(`{"url":"https://example.com","new_setting":{"id":9007199254740993}}`)}
	text := permissionSubject(p)
	if !strings.Contains(text, "```text\nhttps://example.com\n```") || !strings.Contains(text, "Additional arguments:\n```json") || !strings.Contains(text, "9007199254740993") {
		t.Fatalf("new arguments hidden or rounded: %s", text)
	}
	for _, raw := range []string{`{"url":42}`, `{"other":"value"}`, `null`, `{broken`} {
		p.Input = json.RawMessage(raw)
		if body := permissionSubject(p); !strings.HasPrefix(body, "```") || !strings.HasSuffix(body, "\n```") {
			t.Fatalf("fallback is not readable code: %s", body)
		}
	}
}

func TestTrustPromptUsesConsistentHeadingAndFileList(t *testing.T) {
	p := protocol.PromptInfo{ID: "trust-id", Kind: protocol.PromptTrust, Input: json.RawMessage(`{"dir":"/project","hash":"opaque-hash","files":["stavlos.json","agents/scout.md"]}`)}
	text, controls := promptView(p, "client")
	if !strings.HasPrefix(text, "❗ **Project configuration trust** · /project\n") || !strings.Contains(text, "```text\nstavlos.json\nagents/scout.md\n```") || !strings.Contains(text, "MCP servers, policy") || strings.Contains(text, "opaque-hash") || strings.Contains(text, "Prompt:") {
		t.Fatalf("trust prompt: %s", text)
	}
	if len(controls) != 1 || len(controls[0].(dg.ActionsRow).Components) != 2 {
		t.Fatal("trust decision controls changed")
	}
}

func TestLongPatchPermissionRetainsBoundedEditableCard(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: README.md\n@@\n" + strings.Repeat("+a long added line\n", 500) + "*** End Patch"
	input, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	p := protocol.PromptInfo{ID: "p", Channel: "channel", Kind: protocol.PromptPermission, From: "main", Tool: "apply_patch", Input: input, Escalated: true}
	b, w, api := fixture(t, func(_ context.Context, _ string, _, out any) error {
		return result(out, protocol.PromptListResult{Prompts: []protocol.PromptInfo{p}})
	})
	if err := w.refreshPrompts(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := b.store.snapshot()[p.ID].Message
	if err := w.updateQuestionCard(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range api.snapshot() {
		if units(m.Text) > 2000 {
			t.Fatal("permission fragment exceeded Discord's limit")
		}
		if m.ID == id {
			found = true
			if len(m.Components) == 0 || !strings.Contains(m.Text, "*** End Patch") || !strings.HasSuffix(m.Text, "\n```") {
				t.Fatalf("editable patch tail lost context: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("permission message missing")
	}
}
