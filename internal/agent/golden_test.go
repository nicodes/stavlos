package agent

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The behaviour lock for what the model is told (docs/ground-up-refactor.md
// 0.1): the system prompt, the tools it is offered, and the harness's note
// on the request, for a main agent and for a child. How an agent behaves
// follows from this text, so a refactor that changes it has changed the
// product, however clean the diff looks.
//
//	go test ./internal/agent -run TestGoldenPrompt -update

// randomID is an id the runtime makes up (NewID): a short prefix and ten hex
// digits. The text is locked, not the dice.
var randomID = regexp.MustCompile(`\b[a-z]{1,5}[0-9a-f]{10}\b`)

var updateGolden = flag.Bool("update", false, "rewrite the golden prompts in testdata/golden")

func goldenText(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s changed: the model is told something different.\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

var busyCount = regexp.MustCompile(`Agents busy in this channel: \d+ of`)

func TestGoldenPrompt(t *testing.T) {
	fm := &fakeModel{
		steps:      []step{reply(call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look around"}`)), reply(text("delegated"))},
		childSteps: []step{reply(text("looked"))},
	}
	s, h := newTestChannel(t, testConfig{}, fm)
	runTurn(t, s, h, "delegate something")
	waitUntil(t, h, func() bool { return len(fm.requests()) >= 3 })
	scrub := strings.NewReplacer(s.Dir(), "<DIR>", s.Root().ID, "<MAIN>", s.Agents()[1].ID, "<CHILD>")
	var main, child string
	for _, req := range fm.requests() {
		var names []string
		for _, d := range req.Tools {
			names = append(names, d.Name)
		}
		note := ""
		if last := req.Messages[len(req.Messages)-1].Blocks; len(last) > 0 {
			if b := last[len(last)-1]; strings.HasPrefix(b.Text, "[harness state") {
				note = b.Text
			}
		}
		// How many agents are busy when a child's note is built is one or two
		// by whether its parent's turn has ended yet: a race the prompt does
		// not care about and a golden file must not record.
		note = busyCount.ReplaceAllString(note, "Agents busy in this channel: <N> of")
		text := randomID.ReplaceAllString(scrub.Replace("## system\n"+req.System+"\n\n## tools\n"+strings.Join(names, " ")+"\n\n## note on the request\n"+note+"\n"), "<ID>")
		if strings.Contains(req.System, "You are a subagent") {
			child = text
		} else if main == "" {
			main = text
		}
	}
	goldenText(t, "prompt-main", main)
	goldenText(t, "prompt-child", child)
}
