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
	os.MkdirAll(filepath.Join(dir, ".stavlos", "agents"), 0o755)
	os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{"policy":{"shell":{"git push*":"allow","curl*":"deny"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".stavlos", "agents", "reviewer.md"), []byte("---\ndescription: reviews\nmodels: [openai/gpt-5-mini]\ntools:\n  apply_patch: deny\n---\nYou review.\n"), 0o644)
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
	if len(e.Instructions) != 0 || e.ProjectTrusted {
		t.Fatal("untrusted AGENTS.md loaded")
	}
	if verb(e.Policy, "shell", "curl x") != policy.Allow {
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
	if !ok || len(p.Models) != 1 || p.Models[0].ID != "openai/gpt-5-mini" || p.Body != "You review." || p.Layer != "project" || p.Type != TypeAll {
		t.Fatalf("preset %+v", p)
	}
	if len(e.Instructions) != 1 || e.Instructions[0].Text != "Use light models." || !e.ProjectTrusted || len(e.InstructionFiles) != 1 {
		t.Fatalf("AGENTS.md: %+v %v", e.Instructions, e.InstructionFiles)
	}
	if verb(e.Policy, "shell", "git push x") != policy.Allow {
		t.Fatal("the project's git push rule replaces the global one")
	}
	if verb(e.Policy, "shell", "curl x") != policy.Deny {
		t.Fatal("project tighten lost")
	}
	if _, ok := e.Presets["general"]; !ok {
		t.Fatal("builtin missing")
	}

	// Project and global rules merge as if they were one file: the most
	// specific pattern wins, so a project's git* allow is more specific than
	// a global *--force* deny.
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"policy":{"shell":{"*":"allow","*--force*":"deny"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{"policy":{"shell":{"git*":"allow"}}}`), 0o644)
	e, err = Load(dir, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if got := verb(e.Policy, "shell", "git push --force"); got != policy.Allow || verb(e.Policy, "shell", "git status") != policy.Allow || verb(e.Policy, "shell", "rm --force x") != policy.Deny {
		t.Fatalf("project rules merge with the global ones, the most specific pattern winning: git push --force is %s", got)
	}
}

// TestShellAsksByDefault: no shell command is allowed by default, not even
// a "read-only" one (find -exec and rg --pre run programs); searching is
// grep and glob, allowed like read.
func TestShellAsksByDefault(t *testing.T) {
	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	e, err := Load(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"ls", "grep -rn foo .", "find . -exec sh -c 'id' \\;", "rg --pre sh x .", "cat ~/.ssh/id_rsa", "git status"} {
		if v := verb(e.Policy, "shell", cmd); v != policy.Ask {
			t.Errorf("%q: %s, want ask", cmd, v)
		}
	}
	for _, tool := range []string{"grep", "glob", "read"} {
		if v := verb(e.Policy, tool, "src"); v != policy.Allow {
			t.Errorf("%s: %s, want allow", tool, v)
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
type: subagent
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
  web_fetch: deny
spawn: [explorer]
max_turns: 20
color: cyan
---
You review.
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "reviewer" || p.Type != TypeSubagent || p.MaxTurns != 20 || p.Color != "cyan" || p.Body != "You review." {
		t.Fatalf("%+v", p)
	}
	if len(p.Models) != 3 || p.Models[0].ID != "openai/gpt-5.1-codex" || len(p.Models[0].Variants) != 2 || p.Models[2].ID != "xai/grok-4-fast" || p.Models[2].Variants != nil {
		t.Fatalf("models %+v", p.Models)
	}
	if strings.Join(p.Tools, ",") != "shell,read,grep,glob,apply_patch,skill,todo,web_search,sheet" { // everything but the removed web_fetch
		t.Fatalf("tools %v", p.Tools)
	}
	pol := p.PresetPolicy()
	if verb(pol, "shell", "git push origin") != policy.Deny || verb(pol, "shell", "ls") != policy.Ask || verb(pol, "read", "x") != policy.Allow {
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
	if err != nil || m.Type != TypeAll || len(m.Models) != 0 || strings.Join(m.Tools, ",") != strings.Join(RoleTools, ",") || m.PresetPolicy().Rules() != nil {
		t.Fatalf("minimal %+v %v", m, err)
	}
	// errors name the problem
	for name, body := range map[string]string{
		"nodesc":   "---\ntype: all\n---\nx",
		"badmode":  "---\ndescription: d\ntype: sometimes\n---\nx",
		"badcolor": "---\ndescription: d\ncolor: teal\n---\nx",
		"oldmodel": "---\ndescription: d\nmodel: openai/gpt-5\n---\nx",
		"oldpol":   "---\ndescription: d\npolicy:\n  shell: deny\n---\nx",
		"hidden":   "---\ndescription: d\nhidden: true\n---\nx",
		"badverb":  "---\ndescription: d\ntools:\n  shell: maybe\n---\nx",
		"badtools": "---\ndescription: d\ntools: 3\n---\nx",
		"oldlist":  "---\ndescription: d\ntools: [read]\n---\nx",
		"nomsg":    "---\ndescription: d\ntools:\n  message: deny\n---\nx",
		"nokill":   "---\ndescription: d\ntools:\n  shell_kill: deny\n---\nx",
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
	os.MkdirAll(filepath.Join(g, "agents"), 0o755)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"model":"fake/m1"}`), 0o644)
	// read is allowed by default: a role may not turn it into allow-everything for shell
	os.WriteFile(filepath.Join(g, "agents", "loose.md"), []byte("---\ndescription: d\ntools:\n  shell:\n    \"rm *\": allow\n---\nx"), 0o644)
	if _, err := Load(t.TempDir(), noTrust{}); err == nil || !strings.Contains(err.Error(), "loosens") {
		t.Fatalf("a loosening role rule should be a config error: %v", err)
	}
	os.WriteFile(filepath.Join(g, "agents", "loose.md"), []byte("---\ndescription: d\ntools:\n  shell:\n    \"rm *\": deny\n  read: ask\n---\nx"), 0o644)
	e, err := Load(t.TempDir(), noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Presets["loose"].Type != TypeAll {
		t.Fatalf("%+v", e.Presets["loose"])
	}
}

// The repository's own example role must keep parsing: it is what the
// docs point users at.
func TestExampleCoderRoleParses(t *testing.T) {
	p, err := ReadPreset(filepath.Join("..", "..", ".stavlos", "agents", "coder.md"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "coder" || p.Type != TypeAll || p.Color != "green" || len(p.Models) != 3 || p.DefaultVariant("openai/gpt-5.1-codex") != "medium" || strings.Join(p.Spawn, ",") != "general" {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.Tools, ",") != "shell,read,grep,glob,apply_patch,skill,todo,web_fetch,sheet" || verb(p.PresetPolicy(), "shell", "git push origin main") != policy.Deny || verb(p.PresetPolicy(), "web_fetch", "https://x.slack.com/y") != policy.Deny {
		t.Fatalf("tools %v rules %+v", p.Tools, p.PresetPolicy().Rules())
	}
}

// TestLoadGlobalReadsNoDirectory: the daemon's own configuration comes from
// the defaults and the global layer alone, and a directory's
// stavlos.local.json is the repository's: trust-gated like the rest.
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
	if e.Dir != "" || e.Model != "fake/m1" || e.Escalation.Default != policy.Deny || e.Escalation.AnswerTimeout != 7*time.Second || verb(e.Policy, "shell", "rm x") != policy.Ask {
		t.Fatalf("%+v", e)
	}
	l, err := Load(tmp, noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if !l.TrustPending || strings.Join(l.TrustFiles, ",") != ".stavlos/stavlos.local.json" || l.Escalation.Default != policy.Deny || verb(l.Policy, "shell", "rm x") != policy.Ask {
		t.Fatalf("an untrusted local file applied: %+v", l)
	}
}

// TestRepositoryLayersTakePrecedence: once trusted, stavlos.json and
// stavlos.local.json set anything the global file can, local over project
// over global: a rule for the same pattern replaces the global one, and env,
// search, plugins, an allow escalation default, raised limits and the
// sandbox all apply. An untrusted project sets nothing.
func TestRepositoryLayersTakePrecedence(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"policy":{"shell":{"rm *":"deny","ls*":"allow"}},"limits":{"maxAgents":6}}`), 0o644)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".stavlos"), 0o755)
	os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{"limits":{"maxAgents":20},"env":{"pass":["GITHUB_TOKEN"]},"search":{"provider":"brave"},"plugins":["x"],"escalation":{"default":"allow"},"sandbox":{"enabled":false}}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.local.json"), []byte(`{"policy":{"shell":{"rm *":"allow","ls -la":"deny"}},"limits":{"maxAgents":30}}`), 0o644)
	e, err := Load(dir, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if verb(e.Policy, "shell", "rm x") != policy.Allow || verb(e.Policy, "shell", "ls -la") != policy.Deny || verb(e.Policy, "shell", "ls") != policy.Allow {
		t.Fatal("the local layer's rules take precedence")
	}
	if e.Limits.MaxAgents != 30 || !contains(e.PassEnv, "GITHUB_TOKEN") || e.Search.Provider != "brave" || !contains(e.Plugins, "x") || e.Escalation.Default != policy.Allow || e.Sandbox.Enabled {
		t.Fatalf("a project sets what the global file can: limits %+v env %v search %q plugins %v default %s sandbox %v", e.Limits, e.PassEnv, e.Search.Provider, e.Plugins, e.Escalation.Default, e.Sandbox.Enabled)
	}
	if verb(e.Policy, "web_search", "q") != policy.Allow {
		t.Fatal("a project's search backend allows web_search")
	}
	e, err = Load(dir, noTrust{})
	if err != nil || e.Limits.MaxAgents != 6 || !e.Sandbox.Enabled || e.Search.Provider != "" {
		t.Fatalf("an untrusted project sets nothing: %v %+v", err, e.Limits)
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
	if e.Escalation.ClaimTimeout != 10*time.Second || e.Escalation.Default != policy.Allow || e.Compaction.Threshold != 0.5 || e.Compaction.MaxToolOutput != 64*1024 || e.Search.Provider != "brave" || e.Search.APIKey != "k" || verb(e.Policy, "shell", "rm -rf x") != policy.Deny {
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
		if got := verb(e.Policy, "web_search", "anything"); got != want {
			t.Errorf("%s: web_search %s want %s", cfg, got, want)
		}
	}
}

// TestRoleToolsAreRemovedNotListed: a role offers every tool but the ones a
// bare deny removes; a deny under patterns, or on a tool it keeps anyway,
// stays a rule.
func TestRoleToolsAreRemovedNotListed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviewer.md")
	os.WriteFile(path, []byte("---\ndescription: d\ntools:\n  apply_patch: deny\n  todo: deny\n  shell:\n    \"*\": deny\n  mcp__github__merge: deny\n  message:\n    user: deny\n---\nx"), 0o644)
	p, err := ReadPreset(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Tools, ","); got != "shell,read,grep,glob,skill,web_fetch,web_search,sheet" {
		t.Fatalf("tools %s", got)
	}
	pol := p.PresetPolicy()
	if verb(pol, "shell", "ls") != policy.Deny || verb(pol, "mcp__github__merge", "") != policy.Deny || verb(pol, "message", "user") != policy.Deny {
		t.Fatalf("rules %+v", pol.Rules())
	}
	if _, err := ReadPreset(writeRole(t, "---\ndescription: d\ntools: [read]\n---\nx")); err == nil || !strings.Contains(err.Error(), "deny removes one") {
		t.Fatalf("the list form should point at the new form: %v", err)
	}
}

func writeRole(t *testing.T, body string) string {
	path := filepath.Join(t.TempDir(), "r.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRoleDirsRemoved: working directories belong to the channel, so a
// role's dirs: key is a load error that says where they went.
func TestRoleDirsRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lead.md")
	if err := os.WriteFile(path, []byte("---\ndescription: Leads\ndirs: [../shared]\n---\nYou lead.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPreset(path); err == nil || !strings.Contains(err.Error(), "dirs: was removed") {
		t.Fatalf("err %v", err)
	}
}

// verb is a policy's decision on one argument: a command line for shell,
// text otherwise.
func verb(p any, tool, arg string) policy.Verb {
	sub := policy.Text(arg)
	if tool == "shell" {
		sub = policy.Command(arg)
	}
	switch p := p.(type) {
	case *policy.Layered:
		v, _ := p.Decide(tool, sub)
		return v
	case *policy.Set:
		return p.Decide(tool, sub)
	}
	panic("not a policy")
}

// TestSandboxConfig: the sandbox is on with the network by default; the
// global layer can turn either off and add paths, with ~ expanded.
func TestSandboxConfig(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	e, err := LoadGlobal()
	if err != nil || !e.Sandbox.Enabled || !e.Sandbox.Network {
		t.Fatalf("defaults %+v %v", e.Sandbox, err)
	}
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"sandbox":{"network":false,"writable":["~/.cache/x"],"hide":["${env:STAVLOS_TEST_HIDE}/y"]}}`), 0o644)
	t.Setenv("STAVLOS_TEST_HIDE", "/srv")
	e, err = LoadGlobal()
	home, _ := os.UserHomeDir()
	if err != nil || !e.Sandbox.Enabled || e.Sandbox.Network || strings.Join(e.Sandbox.Writable, ",") != filepath.Join(home, ".cache/x") || strings.Join(e.Sandbox.Hide, ",") != "/srv/y" {
		t.Fatalf("%+v %v", e.Sandbox, err)
	}
}
