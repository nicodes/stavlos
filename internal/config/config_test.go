package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/policy"
)

type allTrust struct{}

func (allTrust) Trusted(string, string) bool { return true }

type noTrust struct{}

func (noTrust) Trusted(string, string) bool { return false }

func TestLoadLayersAndTrust(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{
  // comment
  "model": "anthropic/claude-sonnet-5",
  "policy": { "shell": { "*": "allow", "git push*": "ask" }, },
}`), 0o644)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".stavlos", "roles"), 0o755)
	os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{"policy":{"shell":{"git push*":"allow","curl*":"deny"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".stavlos", "roles", "reviewer.md"), []byte("---\ndescription: reviews\nmodels: [openai/gpt-5-mini]\ntools: [read]\n---\nYou review.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use light models."), 0o644)

	e, err := Load(dir, noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if !e.TrustPending || e.TrustHash == "" || len(e.TrustFiles) != 3 {
		t.Fatalf("trust: pending=%v hash=%q files=%v", e.TrustPending, e.TrustHash, e.TrustFiles)
	}
	if _, ok := e.Presets["reviewer"]; ok {
		t.Fatal("untrusted preset loaded")
	}
	if e.AgentsMD != "" {
		t.Fatal("untrusted AGENTS.md loaded")
	}
	if e.Policy.Decide("shell", "curl x") != policy.Allow {
		t.Fatal("untrusted project policy applied")
	}

	e, err = Load(dir, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if e.TrustPending {
		t.Fatal("still pending")
	}
	if e.Model != "anthropic/claude-sonnet-5" || e.RootAgent != "general" {
		t.Fatalf("model %q root %q", e.Model, e.RootAgent)
	}
	p, ok := e.Presets["reviewer"]
	if !ok || len(p.Models) != 1 || p.Models[0].ID != "openai/gpt-5-mini" || p.Body != "You review." || p.Layer != "project" || p.Mode != ModeAll {
		t.Fatalf("preset %+v", p)
	}
	if e.AgentsMD != "Use light models." {
		t.Fatal("AGENTS.md")
	}
	if e.Policy.Decide("shell", "git push x") != policy.Ask {
		t.Fatal("project loosened git push")
	}
	if e.Policy.Decide("shell", "curl x") != policy.Deny {
		t.Fatal("project tighten lost")
	}
	if _, ok := e.Presets["general"]; !ok {
		t.Fatal("builtin missing")
	}

	// A project rule with a longer literal prefix than a global deny does
	// not shadow it: layering is the most restrictive decision, not a merge.
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"policy":{"shell":{"*":"allow","*--force*":"deny"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{"policy":{"shell":{"git*":"allow"}}}`), 0o644)
	e, err = Load(dir, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Policy.Decide("shell", "git push --force") != policy.Deny || e.Policy.Decide("shell", "git status") != policy.Allow {
		t.Fatal("project rule shadowed a global deny")
	}
}

func TestReadOnlyBashAllowedByDefault(t *testing.T) {
	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	e, err := Load(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"grep -rn foo .", "rg foo", "find . -name '*.go'", "ls -la", "git status", "git log --oneline", "cat go.mod"} {
		if v := e.Policy.Decide("shell", cmd); v != policy.Allow {
			t.Errorf("%q: %s, want allow", cmd, v)
		}
	}
	for _, cmd := range []string{"rm -rf x", "git push origin main", "go test ./...", "grepx"} {
		if v := e.Policy.Decide("shell", cmd); v == policy.Allow {
			t.Errorf("%q should not be allowed by default", cmd)
		}
	}
}

func TestDefaultEscalationAnswerTimeout(t *testing.T) {
	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	e, err := Load(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Escalation.AnswerTimeout != 3*time.Minute {
		t.Fatalf("default answer timeout = %s, want 3m", e.Escalation.AnswerTimeout)
	}
}

func TestReadRoleFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name+".md")
		os.WriteFile(path, []byte(body), 0o644)
		return path
	}
	p, err := ReadPreset(write("reviewer", `---
description: Reviews a diff
mode: subagent
models:
  - id: openai/gpt-5.1-codex
    variants: [medium, high]
  - id: openai/gpt-5.1-codex-mini
  - xai/grok-4-fast
tools:
  shell:
    "git push*": deny
    "*": ask
  read: allow
  todo:
spawn: [explorer]
max_turns: 20
color: cyan
dirs: [../shared, ~/notes]
---
You review.
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "reviewer" || p.Mode != ModeSubagent || p.MaxTurns != 20 || p.Color != "cyan" || p.Body != "You review." || strings.Join(p.Dirs, ",") != "../shared,~/notes" {
		t.Fatalf("%+v", p)
	}
	if len(p.Models) != 3 || p.Models[0].ID != "openai/gpt-5.1-codex" || len(p.Models[0].Variants) != 2 || p.Models[2].ID != "xai/grok-4-fast" || p.Models[2].Variants != nil {
		t.Fatalf("models %+v", p.Models)
	}
	if strings.Join(p.Tools, ",") != "shell,read,todo" {
		t.Fatalf("tools %v", p.Tools)
	}
	pol := p.PresetPolicy()
	if pol.Decide("shell", "git push origin") != policy.Deny || pol.Decide("shell", "ls") != policy.Ask || pol.Decide("read", "x") != policy.Allow {
		t.Fatalf("rules %+v", pol.Rules())
	}
	// whitelist helpers
	if !p.AllowsModel("openai/gpt-5.1-codex") || p.AllowsModel("openai/gpt-4") || p.DefaultModel() != "openai/gpt-5.1-codex" {
		t.Fatal("model whitelist")
	}
	if !p.AllowsVariant("openai/gpt-5.1-codex", "high") || p.AllowsVariant("openai/gpt-5.1-codex", "low") || p.AllowsVariant("openai/gpt-5.1-codex", "") || p.DefaultVariant("openai/gpt-5.1-codex") != "medium" {
		t.Fatal("variant whitelist")
	}
	if !p.AllowsVariant("xai/grok-4-fast", "anything") || p.DefaultVariant("xai/grok-4-fast") != "" {
		t.Fatal("a model without variants allows any")
	}
	if !p.CanBeSubagent() || p.CanBePrimary() {
		t.Fatal("mode")
	}
	// a glob entry admits its models but is no default
	g, _ := ReadPreset(write("any", "---\ndescription: d\nmodels: [openai/*]\n---\nbody"))
	if !g.AllowsModel("openai/gpt-5") || g.AllowsModel("xai/grok") || g.DefaultModel() != "" {
		t.Fatalf("glob %+v", g.Models)
	}
	// minimal: description and body; everything else inherits
	m, err := ReadPreset(write("explainer", "---\ndescription: Explains code\n---\nYou explain."))
	if err != nil || m.Mode != ModeAll || len(m.Models) != 0 || strings.Join(m.Tools, ",") != strings.Join(DefaultTools, ",") || m.PresetPolicy().Rules() != nil {
		t.Fatalf("minimal %+v %v", m, err)
	}
	// errors name the problem
	for name, body := range map[string]string{
		"nodesc":   "---\nmode: all\n---\nx",
		"badmode":  "---\ndescription: d\nmode: sometimes\n---\nx",
		"badcolor": "---\ndescription: d\ncolor: teal\n---\nx",
		"oldmodel": "---\ndescription: d\nmodel: openai/gpt-5\n---\nx",
		"oldpol":   "---\ndescription: d\npolicy:\n  shell: deny\n---\nx",
		"hidden":   "---\ndescription: d\nhidden: true\n---\nx",
		"badverb":  "---\ndescription: d\ntools:\n  shell: maybe\n---\nx",
		"badtools": "---\ndescription: d\ntools: 3\n---\nx",
	} {
		if _, err := ReadPreset(write(name, body)); err == nil {
			t.Errorf("%s should fail to parse", name)
		}
	}
	if _, err := ReadPreset(write("oldmodel", "---\ndescription: d\nmodel: x\n---\nx")); err == nil || !strings.Contains(err.Error(), "models:") {
		t.Fatalf("the model: error should point at models: %v", err)
	}
}

func TestRoleRulesOnlyTighten(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	os.MkdirAll(filepath.Join(g, "roles"), 0o755)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1"}`), 0o644)
	// read is allowed by default: a role may not turn it into allow-everything for shell
	os.WriteFile(filepath.Join(g, "roles", "loose.md"), []byte("---\ndescription: d\ntools:\n  shell:\n    \"rm *\": allow\n---\nx"), 0o644)
	if _, err := Load(t.TempDir(), noTrust{}); err == nil || !strings.Contains(err.Error(), "loosens") {
		t.Fatalf("a loosening role rule should be a config error: %v", err)
	}
	os.WriteFile(filepath.Join(g, "roles", "loose.md"), []byte("---\ndescription: d\ntools:\n  shell:\n    \"rm *\": deny\n  read: ask\n---\nx"), 0o644)
	e, err := Load(t.TempDir(), noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Presets["loose"].Mode != ModeAll {
		t.Fatalf("%+v", e.Presets["loose"])
	}
}

// The repository's own example role must keep parsing: it is what the
// docs point users at.
func TestExampleCoderRoleParses(t *testing.T) {
	p, err := ReadPreset(filepath.Join("..", "..", ".stavlos", "roles", "coder.md"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "coder" || p.Mode != ModeAll || p.Color != "green" || len(p.Models) != 3 || p.DefaultVariant("openai/gpt-5.1-codex") != "medium" || strings.Join(p.Spawn, ",") != "general" {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.Tools, ",") != "shell,read,apply_patch,skill,todo,web_search,web_fetch" || p.PresetPolicy().Decide("shell", "git push origin main") != policy.Deny || p.PresetPolicy().Decide("web_fetch", "https://x.slack.com/y") != policy.Deny {
		t.Fatalf("tools %v rules %+v", p.Tools, p.PresetPolicy().Rules())
	}
}

// TestLoadGlobalReadsNoDirectory: the daemon's own configuration comes from
// the defaults and the global layer alone. It used to be Load(os.TempDir()),
// which applied /tmp/.stavlos/stavlos.local.json as a trusted layer.
func TestLoadGlobalReadsNoDirectory(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1","escalation":{"answerTimeout":"7s"}}`), 0o644)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	os.MkdirAll(filepath.Join(tmp, ".stavlos"), 0o755)
	os.WriteFile(filepath.Join(tmp, ".stavlos", "stavlos.local.json"), []byte(`{"escalation":{"default":"allow","answerTimeout":"1s"},"policy":{"shell":"allow"}}`), 0o644)
	e, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	if e.Dir != "" || e.Model != "fake/m1" || e.Escalation.Default != policy.Deny || e.Escalation.AnswerTimeout != 7*time.Second || e.Policy.Decide("shell", "rm x") != policy.Ask {
		t.Fatalf("%+v", e)
	}
	// Load on that directory does apply the local layer: the two are distinct.
	l, err := Load(tmp, noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Escalation.Default != policy.Allow || l.Policy.Decide("shell", "rm x") != policy.Allow {
		t.Fatalf("%+v", l)
	}
}

// TestConfigValidation: a setting that cannot be applied is an error at
// load, never a silent default — a typo in a deny rule must not disarm it.
func TestConfigValidation(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	cases := map[string]string{
		`{"polciy":{"shell":"deny"}}`:                        `unknown field "polciy"`,
		`{"policy":{"shell":"dney"}}`:                        `policy.shell: "dney" is not a verb`,
		`{"policy":{"shell":{"rm *":"never"}}}`:              `policy.shell."rm *": "never" is not a verb`,
		`{"policy":{"shell":{"rm *":1}}}`:                    `policy.shell."rm *": want a verb`,
		`{"policy":{"shell":["deny"]}}`:                      `policy.shell: want a verb or a {pattern: verb} object`,
		`{"escalation":{"default":"maybe"}}`:                 `escalation.default "maybe"`,
		`{"escalation":{"answerTimeout":"soon"}}`:            `escalation.answerTimeout "soon"`,
		`{"compaction":{"threshold":1.5}}`:                   `compaction.threshold 1.5`,
		`{"compaction":{"maxToolOutput":"lots"}}`:            `compaction.maxToolOutput`,
		`{"search":{"provider":"bing","apiKey":"${env:X}"}}`: `search.provider "bing"`,
	}
	for cfg, want := range cases {
		os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(cfg), 0o644)
		_, err := LoadGlobal()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error containing %q", cfg, err, want)
		}
	}
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{
  // comments and trailing commas are fine
  "$schema": "x",
  "escalation": {"claimTimeout": "10s", "default": "allow"},
  "compaction": {"threshold": 0.5, "maxToolOutput": "64kb"},
  "search": {"provider": "Brave", "apiKey": "${env:STAVLOS_TEST_KEY}"},
  "policy": {"shell": {"rm *": "deny"}, "read": "allow"},
}`), 0o644)
	t.Setenv("STAVLOS_TEST_KEY", "k")
	e, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	if e.Escalation.ClaimTimeout != 10*time.Second || e.Escalation.Default != policy.Allow || e.Compaction.Threshold != 0.5 || e.Compaction.MaxToolOutput != 64*1024 || e.Search.Provider != "brave" || e.Search.APIKey != "k" || e.Policy.Decide("shell", "rm -rf x") != policy.Deny {
		t.Fatalf("%+v", e)
	}
	if n, err := parseSize("1mb"); err != nil || n != 1<<20 {
		t.Fatalf("parseSize %d %v", n, err)
	}
	if _, err := parseSize("0"); err == nil {
		t.Fatal("zero size accepted")
	}
}

// TestWebSearchAsksUntilConfigured: with no search backend the tool asks
// (its fallback is a third party); with one it is allowed; an explicit rule
// wins either way.
func TestWebSearchAsksUntilConfigured(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("STAVLOS_TEST_KEY", "k")
	for cfg, want := range map[string]policy.Verb{
		`{}`: policy.Ask,
		`{"search":{"provider":"brave","apiKey":"${env:STAVLOS_TEST_KEY}"}}`:                                policy.Allow,
		`{"search":{"provider":"brave","apiKey":"${env:STAVLOS_TEST_KEY}"},"policy":{"web_search":"deny"}}`: policy.Deny,
		`{"policy":{"web_search":"allow"}}`:                                                                 policy.Allow,
	} {
		os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(cfg), 0o644)
		e, err := LoadGlobal()
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Policy.Decide("web_search", "anything"); got != want {
			t.Errorf("%s: web_search %s want %s", cfg, got, want)
		}
	}
}
