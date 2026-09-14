// Package config loads the three-layer configuration (PRD §10).
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/policy"
	"gopkg.in/yaml.v3"
)

// File is the schema of stavlos.json / stavlos.local.json (PRD §10.2).
// Every field is optional; zero values mean "fall through".
type File struct {
	Schema     string         `json:"$schema,omitempty"`
	Model      string         `json:"model,omitempty"`
	RootAgent  string         `json:"rootAgent,omitempty"`
	Limits     *Limits        `json:"limits,omitempty"`
	Escalation *Escalation    `json:"escalation,omitempty"`
	Compaction *Compaction    `json:"compaction,omitempty"`
	MCP        map[string]MCP `json:"mcp,omitempty"`
	Search     *Search        `json:"search,omitempty"` // web_search backend
	Env        *EnvConfig     `json:"env,omitempty"`    // what child processes inherit
	Policy     map[string]any `json:"policy,omitempty"` // tool → verb | {pattern: verb}
	Plugins    []string       `json:"plugins,omitempty"`
}

type Limits struct {
	MaxDepth  int `json:"maxDepth,omitempty"`
	MaxAgents int `json:"maxAgents,omitempty"`
}

type Escalation struct {
	ClaimTimeout  string `json:"claimTimeout,omitempty"`
	AnswerTimeout string `json:"answerTimeout,omitempty"`
	Default       string `json:"default,omitempty"` // allow | deny
}

type Compaction struct {
	Threshold     float64 `json:"threshold,omitempty"`
	MaxToolOutput string  `json:"maxToolOutput,omitempty"`
}

// Search configures web_search: a provider and its key (the key may be
// "${env:NAME}").
// EnvConfig shapes the environment of the processes agents run. Variables
// whose names look like credentials (…_API_KEY, …TOKEN, …SECRET,
// …PASSWORD…) and STAVLOS_* are dropped; Pass lists names kept anyway.
type EnvConfig struct {
	Pass []string `json:"pass,omitempty"`
}

type Search struct {
	Provider string `json:"provider,omitempty"` // brave | tavily | exa
	APIKey   string `json:"apiKey,omitempty"`
}

type MCP struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"` // values may reference the daemon's environment as ${env:NAME}
	URL     string            `json:"url,omitempty"` // parsed, not yet connected: stdio servers only in v1
}

// ExpandEnv replaces ${env:NAME} references in s with the daemon's
// environment, so config files carry references, never secrets.
func ExpandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[len("${env:") : len(m)-1])
	})
}

var envRef = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// Preset is a role definition from roles/<name>.md (PRD §10.3). The word
// "role" is what users see; "preset" and "archetype" are the same thing in
// code and in the log.
type Preset struct {
	Name        string
	Description string
	Mode        string      // primary | subagent | all
	Models      []ModelSpec // model whitelist, first is the default; empty = any, inherit
	Loop        string
	Tools       []string                     // tool names (and the group todo)
	ToolRules   map[string]map[string]string // tool → pattern → verb, from the map form of tools:
	Skills      []string
	MCP         []string
	Spawn       []string
	MaxTurns    int      // subagent only: turns before it must answer; 0 = unlimited
	Color       string   // one of RoleColors, or ""
	Dirs        []string // working directories besides the session's, relative to it or absolute (~ allowed)
	Body        string   // system prompt
	Source      string   // file path
	Layer       string   // global | project | builtin
}

// ModelSpec is one entry of a role's model whitelist: a model id (a glob
// such as "openai/*" is allowed) and, optionally, the variants allowed for
// it, the first being the default. No variants means any the provider
// offers, provider default.
type ModelSpec struct {
	ID       string   `yaml:"id"`
	Variants []string `yaml:"variants"`
}

// Role modes.
const (
	ModePrimary  = "primary"  // selectable for the main agent, never spawned
	ModeSubagent = "subagent" // only created with agent_create by a role that lists it
	ModeAll      = "all"      // both (the default)
)

// RoleColors are the named tints a role may pick for its rows in the TUI.
var RoleColors = []string{"red", "blue", "green", "yellow", "purple", "orange", "pink", "cyan"}

// AllowsModel reports whether the role's whitelist admits a model id (any
// model when the list is empty).
func (p Preset) AllowsModel(id string) bool {
	if len(p.Models) == 0 {
		return true
	}
	return p.modelSpec(id) != nil
}

// modelSpec finds the whitelist entry matching a model id (glob-aware).
func (p Preset) modelSpec(id string) *ModelSpec {
	for i := range p.Models {
		if ok, _ := filepath.Match(p.Models[i].ID, id); ok || p.Models[i].ID == id {
			return &p.Models[i]
		}
	}
	return nil
}

// DefaultModel is the whitelist's first entry when it is a plain id, else
// "" (a glob cannot be a default; the caller falls back to inheriting).
func (p Preset) DefaultModel() string {
	if len(p.Models) == 0 || strings.ContainsAny(p.Models[0].ID, "*?[") {
		return ""
	}
	return p.Models[0].ID
}

// AllowsVariant reports whether variant v may be used with model id under
// this role: any when the list is empty or the matching entry lists none.
// "" (the provider default) is allowed only when the entry lists none.
func (p Preset) AllowsVariant(id, v string) bool {
	spec := p.modelSpec(id)
	if spec == nil || len(spec.Variants) == 0 {
		return true
	}
	for _, x := range spec.Variants {
		if x == v {
			return true
		}
	}
	return false
}

// DefaultVariantList is the variants listed for model id (nil = any).
func (p Preset) DefaultVariantList(id string) []string {
	if spec := p.modelSpec(id); spec != nil {
		return spec.Variants
	}
	return nil
}

// DefaultVariant is the first variant listed for model id, "" when none.
func (p Preset) DefaultVariant(id string) string {
	if spec := p.modelSpec(id); spec != nil && len(spec.Variants) > 0 {
		return spec.Variants[0]
	}
	return ""
}

// CanBePrimary / CanBeSubagent read the mode.
func (p Preset) CanBePrimary() bool  { return p.Mode != ModeSubagent }
func (p Preset) CanBeSubagent() bool { return p.Mode != ModePrimary }

// Skill is a skills/<name>/SKILL.md (PRD §10.4).
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Body        string `yaml:"-"`
	Dir         string `yaml:"-"`
}

// Effective is the merged configuration for one working directory.
type Effective struct {
	Dir        string
	Model      string
	RootAgent  string
	Limits     Limits
	Escalation struct {
		ClaimTimeout, AnswerTimeout time.Duration
		Default                     policy.Verb
	}
	Compaction struct {
		Threshold     float64
		MaxToolOutput int
	}
	MCP      map[string]MCP
	Search   Search          // web_search backend, key expanded
	PassEnv  []string        // environment variables child processes keep although their names look like secrets
	Policy   *policy.Layered // global and local rules as the base; the trusted project's rules as an overlay that can only tighten
	Presets  map[string]Preset
	Skills   map[string]Skill
	AgentsMD string
	Plugins  []string

	// TrustPending is true when a project layer exists but has not been
	// confirmed; in that case project content has NOT been merged.
	TrustPending bool
	TrustHash    string
	TrustFiles   []string
}

// Trust records confirmed project layers, keyed by directory (PRD §10.6).
type Trust interface {
	Trusted(dir, hash string) bool
}

// Load builds the effective config for dir. Project content is merged only
// when trust confirms the current hash.
func Load(dir string, trust Trust) (*Effective, error) {
	dir, _ = filepath.Abs(dir)
	e, err := LoadGlobal()
	if err != nil {
		return nil, err
	}
	e.Dir = dir

	// project layer (trust-gated)
	pdir := paths.ProjectDir(dir)
	agentsMD := filepath.Join(dir, "AGENTS.md")
	files, hash, err := ProjectHash(dir)
	if err != nil {
		return nil, err
	}
	if len(files) > 0 {
		e.TrustHash = hash
		e.TrustFiles = files
		if trust != nil && trust.Trusted(dir, hash) {
			pf, err := readFile(filepath.Join(pdir, "stavlos.json"))
			if err != nil {
				return nil, fmt.Errorf("project config: %w", err)
			}
			if len(pf.Plugins) > 0 {
				fmt.Fprintf(os.Stderr, "stavlos: ignoring plugins in %s (global only)\n", pdir)
			}
			if err := e.applyFile(pf, "project"); err != nil {
				return nil, fmt.Errorf("project config: %w", err)
			}
			if err := e.loadPresets(filepath.Join(pdir, "roles"), "project"); err != nil {
				return nil, err
			}
			warnOldAgentsDir(filepath.Join(pdir, "agents"))
			if err := e.loadSkills(filepath.Join(pdir, "skills")); err != nil {
				return nil, err
			}
			if b, err := os.ReadFile(agentsMD); err == nil {
				e.AgentsMD = string(b)
			}
		} else {
			e.TrustPending = true
		}
	}

	// local layer (trusted, never prompts)
	lf, err := readFile(filepath.Join(pdir, "stavlos.local.json"))
	if err != nil {
		return nil, fmt.Errorf("local config: %w", err)
	}
	if err := e.applyFile(lf, "local"); err != nil {
		return nil, fmt.Errorf("local config: %w", err)
	}
	return e, nil
}

// LoadGlobal is the daemon-wide configuration: the defaults and the global
// layer, nothing from any directory. It is what the daemon itself runs on
// (escalation timers, the fallback for a session whose own config fails to
// load); Load builds a session's config on top of it.
func LoadGlobal() (*Effective, error) {
	e := &Effective{Presets: map[string]Preset{}, Skills: map[string]Skill{}, MCP: map[string]MCP{}}

	// defaults
	e.Model = ""
	e.RootAgent = "general"
	e.Limits = Limits{MaxDepth: 3, MaxAgents: 6}
	e.Escalation.ClaimTimeout = 30 * time.Second
	e.Escalation.AnswerTimeout = 3 * time.Minute
	e.Escalation.Default = policy.Deny
	e.Compaction.Threshold = 0.8
	e.Compaction.MaxToolOutput = 32 * 1024
	e.Policy = policy.Layer(policy.New(
		policy.Rule{Tool: "read", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "skill", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_create", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_response", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_message", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_cancel", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_status", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "*", Verb: policy.Ask},
		policy.Rule{Tool: "shell_kill", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "web_fetch", Pattern: "*", Verb: policy.Ask}, // per host: the dialog offers "allow <host> for this session"
		policy.Rule{Tool: "web_search", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "todo_add", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "ask_user", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "todo_update", Pattern: "*", Verb: policy.Allow},
		// Read-only shell commands are allowed by default so searching and
		// looking around never prompts; anything that writes still asks.
		policy.Rule{Tool: "shell", Pattern: "grep *", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "rg *", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "find *", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "ls*", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "cat *", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "head *", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "tail *", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "wc *", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "pwd", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "tree*", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "git status*", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "git log*", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "git diff*", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "git show*", Verb: policy.Allow},
		policy.Rule{Tool: "shell", Pattern: "git blame*", Verb: policy.Allow},
		policy.Rule{Tool: "apply_patch", Pattern: "*", Verb: policy.Ask},
	))
	for _, p := range builtinPresets() {
		e.Presets[p.Name] = p
	}

	// global layer
	gdir := paths.ConfigDir()
	gf, err := readFile(filepath.Join(gdir, "stavlos.json"))
	if err != nil {
		return nil, fmt.Errorf("global config: %w", err)
	}
	if err := e.applyFile(gf, "global"); err != nil {
		return nil, fmt.Errorf("global config: %w", err)
	}
	e.Plugins = gf.Plugins
	if err := e.loadPresets(filepath.Join(gdir, "roles"), "global"); err != nil {
		return nil, err
	}
	warnOldAgentsDir(filepath.Join(gdir, "agents"))
	if err := e.loadSkills(filepath.Join(gdir, "skills")); err != nil {
		return nil, err
	}
	return e, nil
}

// applyFile layers one file onto e. Every value is validated: a setting
// that cannot be applied is an error, never a silent fallback to the
// default (an unreadable deny rule is the worst kind of failure).
func (e *Effective) applyFile(f File, layer string) error {
	if f.Model != "" {
		e.Model = f.Model
	}
	if f.RootAgent != "" {
		e.RootAgent = f.RootAgent
	}
	if f.Limits != nil {
		if f.Limits.MaxDepth < 0 || f.Limits.MaxAgents < 0 {
			return errors.New("limits: maxDepth and maxAgents must be positive")
		}
		if f.Limits.MaxDepth > 0 {
			e.Limits.MaxDepth = f.Limits.MaxDepth
		}
		if f.Limits.MaxAgents > 0 {
			e.Limits.MaxAgents = f.Limits.MaxAgents
		}
	}
	if f.Escalation != nil {
		if f.Escalation.ClaimTimeout != "" {
			d, err := time.ParseDuration(f.Escalation.ClaimTimeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("escalation.claimTimeout %q: want a duration such as 30s", f.Escalation.ClaimTimeout)
			}
			e.Escalation.ClaimTimeout = d
		}
		if f.Escalation.AnswerTimeout != "" {
			d, err := time.ParseDuration(f.Escalation.AnswerTimeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("escalation.answerTimeout %q: want a duration such as 3m", f.Escalation.AnswerTimeout)
			}
			e.Escalation.AnswerTimeout = d
		}
		if f.Escalation.Default != "" {
			v := policy.Verb(f.Escalation.Default)
			if v != policy.Allow && v != policy.Deny {
				return fmt.Errorf("escalation.default %q: allow or deny", f.Escalation.Default)
			}
			e.Escalation.Default = v
		}
	}
	if f.Compaction != nil {
		if f.Compaction.Threshold != 0 {
			if f.Compaction.Threshold <= 0 || f.Compaction.Threshold > 1 {
				return fmt.Errorf("compaction.threshold %v: a fraction of the context window between 0 and 1", f.Compaction.Threshold)
			}
			e.Compaction.Threshold = f.Compaction.Threshold
		}
		if f.Compaction.MaxToolOutput != "" {
			n, err := parseSize(f.Compaction.MaxToolOutput)
			if err != nil {
				return fmt.Errorf("compaction.maxToolOutput: %v", err)
			}
			e.Compaction.MaxToolOutput = n
		}
	}
	if f.Search != nil {
		if f.Search.Provider != "" {
			p := strings.ToLower(f.Search.Provider)
			switch p {
			case "brave", "tavily", "exa":
			default:
				return fmt.Errorf("search.provider %q: brave, tavily or exa", f.Search.Provider)
			}
			e.Search.Provider = p
		}
		if f.Search.APIKey != "" {
			if !strings.Contains(f.Search.APIKey, "${env:") {
				fmt.Fprintf(os.Stderr, "stavlos: search.apiKey is written into the %s config; prefer \"${env:NAME}\" so the key stays in the environment\n", layer)
			}
			e.Search.APIKey = ExpandEnv(f.Search.APIKey)
		}
	}
	for k, v := range f.MCP {
		e.MCP[k] = v
	}
	if f.Env != nil {
		for _, n := range f.Env.Pass {
			if !contains(e.PassEnv, n) {
				e.PassEnv = append(e.PassEnv, n)
			}
		}
	}
	rules, err := ParsePolicy(f.Policy)
	if err != nil {
		return err
	}
	if layer == "project" {
		// The project's rules are an overlay: they can only tighten what
		// the global and local layers decide (PRD §10.6).
		e.Policy = e.Policy.With(rules)
	} else {
		e.Policy = policy.Layer(e.Policy.Base().Merge(rules), e.Policy.Overlays()...)
	}
	return nil
}

// ParsePolicy converts the JSON policy shape (tool → verb, or tool →
// {pattern: verb}) into rules. A verb that is not allow, ask or deny, or
// a value of another shape, is an error naming the entry.
func ParsePolicy(m map[string]any) (*policy.Set, error) {
	var rules []policy.Rule
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, tool := range keys {
		switch v := m[tool].(type) {
		case string:
			pv := policy.Verb(v)
			if !pv.Valid() {
				return nil, fmt.Errorf("policy.%s: %q is not a verb (allow, ask or deny)", tool, v)
			}
			rules = append(rules, policy.Rule{Tool: tool, Pattern: "*", Verb: pv})
		case map[string]any:
			pk := make([]string, 0, len(v))
			for k := range v {
				pk = append(pk, k)
			}
			sort.Strings(pk)
			for _, pat := range pk {
				str, ok := v[pat].(string)
				if !ok {
					return nil, fmt.Errorf("policy.%s.%q: want a verb (allow, ask or deny), got %T", tool, pat, v[pat])
				}
				pv := policy.Verb(str)
				if !pv.Valid() {
					return nil, fmt.Errorf("policy.%s.%q: %q is not a verb (allow, ask or deny)", tool, pat, str)
				}
				rules = append(rules, policy.Rule{Tool: tool, Pattern: pat, Verb: pv})
			}
		default:
			return nil, fmt.Errorf("policy.%s: want a verb or a {pattern: verb} object, got %T", tool, m[tool])
		}
	}
	return policy.New(rules...), nil
}

func (e *Effective) loadPresets(dir, layer string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
			continue
		}
		p, err := ReadPreset(filepath.Join(dir, ent.Name()))
		if err != nil {
			return err
		}
		p.Layer = layer
		if err := e.checkTightening(p); err != nil {
			return err
		}
		e.Presets[p.Name] = p
	}
	return nil
}

// checkTightening rejects a role rule that could never take effect because
// it is looser than the layered policy (PRD §10.6: roles only tighten). The
// layering itself makes such a rule inert whatever this check says; the
// error exists so the user is not left wondering why the rule did nothing.
// It samples one argument per pattern, so it catches the plain cases.
func (e *Effective) checkTightening(p Preset) error {
	for _, r := range p.PresetPolicy().Rules() {
		base := e.Policy.Decide(r.Tool, samplePattern(r.Pattern))
		if r.Verb.Rank() < base.Rank() {
			return fmt.Errorf("%s: tools.%s %q: %s loosens the policy (%s); roles may only tighten", p.Source, r.Tool, r.Pattern, r.Verb, base)
		}
	}
	return nil
}

// samplePattern turns a glob into a representative argument.
func samplePattern(p string) string {
	return strings.NewReplacer("*", "x", "?", "x").Replace(p)
}

// warnOldAgentsDir points at the rename once per load.
func warnOldAgentsDir(dir string) {
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		fmt.Fprintf(os.Stderr, "stavlos: %s is ignored; roles now live in %s\n", dir, filepath.Join(filepath.Dir(dir), "roles"))
	}
}

func (e *Effective) loadSkills(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		sf := filepath.Join(dir, ent.Name(), "SKILL.md")
		b, err := os.ReadFile(sf)
		if err != nil {
			continue // no SKILL.md → not a skill
		}
		var s Skill
		body, err := frontmatter(string(b), &s)
		if err != nil {
			return fmt.Errorf("%s: %w", sf, err)
		}
		if s.Name == "" {
			s.Name = ent.Name()
		}
		s.Body = body
		s.Dir = filepath.Join(dir, ent.Name())
		e.Skills[s.Name] = s
	}
	return nil
}

// roleFile is the frontmatter of roles/<name>.md. tools and models take
// two shapes each (a list, or a map/list of objects), so they are decoded
// from nodes.
type roleFile struct {
	Description string    `yaml:"description"`
	Mode        string    `yaml:"mode"`
	Models      yaml.Node `yaml:"models"`
	Loop        string    `yaml:"loop"`
	Tools       yaml.Node `yaml:"tools"`
	Skills      []string  `yaml:"skills"`
	MCP         []string  `yaml:"mcp"`
	Spawn       []string  `yaml:"spawn"`
	MaxTurns    int       `yaml:"max_turns"`
	Color       string    `yaml:"color"`
	Dirs        []string  `yaml:"dirs"`
	// Retired keys, named so the error can say what replaced them.
	Model  *string        `yaml:"model"`
	Policy map[string]any `yaml:"policy"`
	Hidden *bool          `yaml:"hidden"`
}

// DefaultTools is what a role gets when it lists none.
var DefaultTools = []string{"shell", "read", "apply_patch", "skill", "web_fetch", "web_search"}

// ReadPreset parses one roles/<name>.md file.
func ReadPreset(path string) (Preset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Preset{}, err
	}
	var f roleFile
	body, err := frontmatter(string(b), &f)
	if err != nil {
		return Preset{}, fmt.Errorf("%s: %w", path, err)
	}
	p := Preset{
		Name: strings.TrimSuffix(filepath.Base(path), ".md"), Description: strings.TrimSpace(f.Description),
		Mode: f.Mode, Loop: f.Loop, Skills: f.Skills, MCP: f.MCP, Spawn: f.Spawn, MaxTurns: f.MaxTurns, Color: f.Color, Dirs: f.Dirs,
		Body: strings.TrimSpace(body), Source: path,
	}
	fail := func(format string, args ...any) (Preset, error) {
		return Preset{}, fmt.Errorf("%s: "+format, append([]any{path}, args...)...)
	}
	switch {
	case f.Model != nil:
		return fail("model: is now models: (a list; the first entry is the default)")
	case f.Policy != nil:
		return fail("policy: is now written under tools: (tools.<name>.<pattern>: verb)")
	case f.Hidden != nil:
		return fail("hidden: is not supported; use mode: or leave the role out of spawn lists")
	case p.Description == "":
		return fail("description: is required")
	case p.MaxTurns < 0:
		return fail("max_turns: must be 0 or more")
	}
	switch p.Mode {
	case "":
		p.Mode = ModeAll
	case ModePrimary, ModeSubagent, ModeAll:
	default:
		return fail("mode: %q must be primary, subagent or all", p.Mode)
	}
	if p.Color != "" && !contains(RoleColors, p.Color) {
		return fail("color: %q must be one of %s", p.Color, strings.Join(RoleColors, ", "))
	}
	if p.Loop == "" {
		p.Loop = "default"
	}
	if p.Models, err = parseModels(&f.Models); err != nil {
		return fail("models: %v", err)
	}
	if p.Tools, p.ToolRules, err = parseTools(&f.Tools); err != nil {
		return fail("tools: %v", err)
	}
	if len(p.Tools) == 0 {
		p.Tools = append([]string(nil), DefaultTools...)
	}
	return p, nil
}

// parseModels reads the models whitelist: a list whose entries are either
// a model id or {id, variants}.
func parseModels(n *yaml.Node) ([]ModelSpec, error) {
	if n.Kind == 0 {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, errors.New("must be a list of model ids or {id, variants} entries")
	}
	var out []ModelSpec
	for _, it := range n.Content {
		switch it.Kind {
		case yaml.ScalarNode:
			out = append(out, ModelSpec{ID: strings.TrimSpace(it.Value)})
		case yaml.MappingNode:
			var m ModelSpec
			if err := it.Decode(&m); err != nil {
				return nil, err
			}
			if m.ID == "" {
				return nil, errors.New("an entry needs an id")
			}
			out = append(out, m)
		default:
			return nil, errors.New("entries are model ids or {id, variants}")
		}
	}
	return out, nil
}

// parseTools reads the tools key: a list of names (inherited policy), or a
// map of name → verb | {pattern: verb}. Names come back in file order.
func parseTools(n *yaml.Node) ([]string, map[string]map[string]string, error) {
	if n.Kind == 0 {
		return nil, nil, nil
	}
	switch n.Kind {
	case yaml.SequenceNode:
		var names []string
		if err := n.Decode(&names); err != nil {
			return nil, nil, err
		}
		return names, nil, nil
	case yaml.MappingNode:
		var names []string
		rules := map[string]map[string]string{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			name, val := n.Content[i].Value, n.Content[i+1]
			names = append(names, name)
			switch val.Kind {
			case yaml.ScalarNode:
				if val.Tag == "!!null" || val.Value == "" {
					continue // present, inherited policy
				}
				if !policy.Verb(val.Value).Valid() {
					return nil, nil, fmt.Errorf("%s: %q is not allow, ask or deny", name, val.Value)
				}
				rules[name] = map[string]string{"*": val.Value}
			case yaml.MappingNode:
				m := map[string]string{}
				for k := 0; k+1 < len(val.Content); k += 2 {
					pat, verb := val.Content[k].Value, val.Content[k+1].Value
					if !policy.Verb(verb).Valid() {
						return nil, nil, fmt.Errorf("%s.%s: %q is not allow, ask or deny", name, pat, verb)
					}
					m[pat] = verb
				}
				rules[name] = m
			default:
				return nil, nil, fmt.Errorf("%s: give a verb or a map of pattern → verb", name)
			}
		}
		return names, rules, nil
	}
	return nil, nil, errors.New("must be a list of tool names or a map of tool → rules")
}

// frontmatter splits "---\nyaml\n---\nbody" and decodes the yaml into v.
func frontmatter(s string, v any) (string, error) {
	s = strings.TrimPrefix(s, "\uFEFF")
	if !strings.HasPrefix(s, "---") {
		return s, nil
	}
	rest := s[3:]
	rest = strings.TrimLeft(rest, " \t\r")
	if !strings.HasPrefix(rest, "\n") {
		return s, nil
	}
	rest = rest[1:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", errors.New("unterminated frontmatter")
	}
	fm := rest[:end]
	body := rest[end+4:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	if err := yaml.Unmarshal([]byte(fm), v); err != nil {
		return "", err
	}
	return body, nil
}

// readFile reads a JSONC file; a missing file is an empty File.
// readFile parses a stavlos.json (JSONC: comments and trailing commas).
// Unknown keys are errors: a misspelt "policy" would otherwise vanish, and
// with it every deny rule under it.
func readFile(path string) (File, error) {
	var f File
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(StripJSONC(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// StripJSONC removes // and /* */ comments and trailing commas.
func StripJSONC(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			if c == '\\' && i+1 < len(b) {
				i++
				out = append(out, b[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i++
		case c == ',':
			// trailing comma: look ahead past whitespace for } or ]
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\n' || b[j] == '\t' || b[j] == '\r') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

func parseSize(s string) (int, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 1
	switch {
	case strings.HasSuffix(s, "kb"):
		mult, s = 1024, strings.TrimSuffix(s, "kb")
	case strings.HasSuffix(s, "mb"):
		mult, s = 1024*1024, strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q: want a size such as 32kb or 1mb", orig)
	}
	return n * mult, nil
}

// ProjectHash lists the trust-gated files under dir (.stavlos/** except
// stavlos.local.json, plus AGENTS.md) and hashes their contents (PRD §10.6).
func ProjectHash(dir string) ([]string, string, error) {
	var files []string
	pdir := paths.ProjectDir(dir)
	err := filepath.WalkDir(pdir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(p) == "stavlos.local.json" {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, rel)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil {
		files = append(files, "AGENTS.md")
	}
	if len(files) == 0 {
		return nil, "", nil
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", f, len(b))
		h.Write(b)
	}
	return files, hex.EncodeToString(h.Sum(nil))[:32], nil
}

// PresetPolicy returns the role's tightening rules from the map form of
// tools:. Rules on todo cover todo_add and todo_update.
func (p Preset) PresetPolicy() *policy.Set {
	m := map[string]any{}
	for tool, rules := range p.ToolRules {
		r := map[string]any{}
		for pat, verb := range rules {
			r[pat] = verb
		}
		for _, t := range toolGroup(tool) {
			m[t] = r
		}
	}
	set, _ := ParsePolicy(m) // verbs were checked when the role file was read
	return set
}

// toolGroup expands a tools: key to the tool names it gates.
func toolGroup(name string) []string {
	switch name {
	case "todo":
		return []string{"todo_add", "todo_update"}
	}
	return []string{name}
}

// builtinPresets is the one role every install starts with. It can do
// everything and can delegate to copies of itself; users add specialised
// roles as <config>/roles/<name>.md or <project>/.stavlos/roles/<name>.md.
func builtinPresets() []Preset {
	return []Preset{
		{
			Name: "general", Layer: "builtin", Mode: ModeAll,
			Description: "General-purpose engineer: reads, edits, runs, and delegates",
			Tools:       []string{"shell", "read", "apply_patch", "skill", "todo", "web_fetch", "web_search"},
			Spawn:       []string{"general"},
			Loop:        "default",
			Body: `You are a senior software engineer working in the user's repository at the current working directory.
Work carefully: read before you edit, prefer small targeted changes, and run the project's tests or build after changing code.
Search and read with shell (grep -rn, rg, find, ls) and read; edit with apply_patch. A slow command such as a test suite continues as a background job on its own; start servers with background: true.
Delegate independent pieces of work to subagents when that saves your own context or lets things run in parallel: give each a specific task and a short label, then keep working or end your turn; each child's answer comes back to you as a message. A child stays alive for the session: message it again for follow-ups. Subagents can delegate too.
Report what you changed and what you verified.`,
		},
	}
}

// SetGlobalModel writes "model" into the global stavlos.json, creating the
// file if needed and replacing an existing "model" entry otherwise. Comments
// and other keys are preserved.
func SetGlobalModel(modelID string) error {
	p := filepath.Join(paths.ConfigDir(), "stavlos.json")
	b, err := os.ReadFile(p)
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(fmt.Sprintf("{\n  \"model\": %q\n}\n", modelID)), 0o600)
	}
	s := string(b)
	re := regexp.MustCompile(`"model"\s*:\s*"[^"]*"`)
	if re.MatchString(s) {
		s = re.ReplaceAllString(s, fmt.Sprintf(`"model": %q`, modelID))
	} else {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			return fmt.Errorf("%s is not a JSON object", p)
		}
		s = s[:i+1] + fmt.Sprintf("\n  \"model\": %q,", modelID) + s[i+1:]
	}
	return os.WriteFile(p, []byte(s), 0o600) // it may hold a search key
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
