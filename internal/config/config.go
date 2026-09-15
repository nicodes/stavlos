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
	"github.com/nicodes/stavlos/internal/toolname"
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
	Reminders  *bool          `json:"reminders,omitempty"` // remind an agent that ends a turn owing a reply (default true)
	Sandbox    *SandboxConfig `json:"sandbox,omitempty"`   // the OS boundary shell commands and MCP servers run in
	Dirs       []string       `json:"dirs,omitempty"`      // directories every channel works in besides its own (global only)
}

// SandboxConfig shapes the sandbox (global layer only). Paths may use ~
// and ${env:NAME}.
type SandboxConfig struct {
	Enabled  *bool    `json:"enabled,omitempty"`  // default true
	Network  *bool    `json:"network,omitempty"`  // TCP from commands; default true
	Writable []string `json:"writable,omitempty"` // more directories commands may write (a cache, a toolchain's store)
	Hide     []string `json:"hide,omitempty"`     // more paths commands may not see
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

// EnvConfig shapes the environment of the processes agents run. Variables
// whose names look like credentials (…_API_KEY, …TOKEN, …SECRET,
// …PASSWORD…) and STAVLOS_* are dropped; Pass lists names kept anyway.
type EnvConfig struct {
	Pass []string `json:"pass,omitempty"`
}

// Search configures web_search: a provider and its key (the key may be
// "${env:NAME}").
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

// expandPath expands ${env:NAME} and a leading ~ and cleans the result.
func expandPath(p string) string {
	p = ExpandEnv(strings.TrimSpace(p))
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[1:])
		}
	}
	return filepath.Clean(p)
}

// Preset is a role definition from agents/<name>.md (PRD §10.3). The word
// "role" is what users see; "preset" and "archetype" are the same thing in
// code and in the log.
type Preset struct {
	Name        string
	Description string
	Type        string      // primary | subagent | all
	Models      []ModelSpec // model whitelist, first is the default; empty = any, inherit
	Loop        string
	Tools       []string                     // the tools it offers: RoleTools minus the ones tools: removes (todo stands for the group)
	ToolRules   map[string]map[string]string // tool → pattern → verb, from the map form of tools:
	Skills      []string
	MCP         []string
	Spawn       []string
	MaxTurns    int    // subagent only: turns before it must answer; 0 = unlimited
	Color       string // one of RoleColors, or ""
	Body        string // system prompt
	Source      string // file path
	Layer       string // global | project | builtin
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
	TypePrimary  = "primary"  // selectable for the main agent, never spawned
	TypeSubagent = "subagent" // only created with agent_create by a role that lists it
	TypeAll      = "all"      // both (the default)
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

// CanBePrimary / CanBeSubagent read the type.
func (p Preset) CanBePrimary() bool  { return p.Type != TypeSubagent }
func (p Preset) CanBeSubagent() bool { return p.Type != TypePrimary }

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
	// Reminders gives an agent that ends a turn owing a reply one reminder
	// turn (docs/super-chat.md).
	Reminders bool
	Sandbox   struct {
		Enabled, Network bool
		Writable, Hide   []string // expanded, absolute
	}

	// Dirs are the directories every channel works in besides its own (the
	// global stavlos.json's dirs), ~ and ${env:NAME} expanded; a relative one
	// is taken from each channel's directory.
	Dirs []string

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
			// stavlos.json and stavlos.local.json are both the repository's:
			// trust-gated, hashed, and able only to tighten.
			for _, l := range []struct{ file, layer string }{{"stavlos.json", "project"}, {"stavlos.local.json", "local"}} {
				f, err := readFile(filepath.Join(pdir, l.file))
				if err != nil {
					return nil, fmt.Errorf("%s config: %w", l.layer, err)
				}
				if err := e.applyFile(f, l.layer); err != nil {
					return nil, fmt.Errorf("%s config: %w", l.layer, err)
				}
			}
			if err := e.loadPresets(filepath.Join(pdir, "agents"), "project"); err != nil {
				return nil, err
			}
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
	return e, nil
}

// Defaults is the configuration every install starts from, written as a
// file: LoadGlobal applies the global layer over it, and stavlos init writes
// it out so the file shows where each setting lives.
func Defaults() File {
	on, network := true, true
	allow, ask := string(policy.Allow), string(policy.Ask)
	return File{
		RootAgent:  "general",
		Limits:     &Limits{MaxDepth: 3, MaxAgents: 6},
		Escalation: &Escalation{ClaimTimeout: "30s", AnswerTimeout: "3m", Default: string(policy.Deny)},
		Compaction: &Compaction{Threshold: 0.8, MaxToolOutput: "32kb"},
		Reminders:  &on,
		Sandbox:    &SandboxConfig{Enabled: &on, Network: &network},
		Policy: map[string]any{
			toolname.Read:        allow,
			toolname.Grep:        allow,
			toolname.Glob:        allow,
			toolname.Skill:       allow,
			toolname.AgentCreate: allow,
			toolname.Message:     allow,
			toolname.AgentCancel: allow,
			toolname.AgentStatus: allow,
			toolname.ShellKill:   allow,
			toolname.TodoAdd:     allow,
			toolname.TodoUpdate:  allow,
			toolname.AskUser:     allow,
			toolname.Shell:       ask, // no command is allowed by default: searching is grep and glob
			toolname.ApplyPatch:  ask,
			toolname.WebFetch:    ask, // per host: the dialog offers "allow <host> for this channel"
			toolname.WebSearch:   ask, // allowed once a search backend is configured (see LoadGlobal)
		},
	}
}

// LoadGlobal is the daemon-wide configuration: the defaults and the global
// layer, nothing from any directory. It is what the daemon itself runs on
// (escalation timers, the fallback for a channel whose own config fails to
// load); Load builds a channel's config on top of it.
func LoadGlobal() (*Effective, error) {
	e := &Effective{Presets: map[string]Preset{}, Skills: map[string]Skill{}, MCP: map[string]MCP{}}

	// defaults, then the global layer over them
	e.Policy = policy.Layer(policy.New())
	if err := e.applyFile(Defaults(), "global"); err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
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
	// web_search asks until a backend is configured: without one every
	// query would go to the keyless fallback, a third party the user never
	// chose. With one, it is allowed like any read-only call unless the
	// file says otherwise.
	if _, explicit := gf.Policy[toolname.WebSearch]; e.Search.Provider != "" && !explicit {
		e.Policy = policy.Layer(e.Policy.Base().Merge(policy.New(policy.Rule{Tool: toolname.WebSearch, Pattern: "*", Verb: policy.Allow})), e.Policy.Overlays()...)
	}
	e.Plugins = gf.Plugins
	if err := e.loadPresets(filepath.Join(gdir, "agents"), "global"); err != nil {
		return nil, err
	}
	if err := e.loadSkills(filepath.Join(gdir, "skills")); err != nil {
		return nil, err
	}
	return e, nil
}

// applyFile layers one file onto e. Every value is validated: a setting
// that cannot be applied is an error, never a silent fallback to the
// default (an unreadable deny rule is the worst kind of failure).
func (e *Effective) applyFile(f File, layer string) error {
	if layer != "global" {
		if err := e.checkRepositoryFile(f); err != nil {
			return err
		}
	}
	if f.Model != "" {
		e.Model = f.Model
	}
	if f.RootAgent != "" {
		e.RootAgent = f.RootAgent
	}
	if f.Reminders != nil {
		e.Reminders = *f.Reminders
	}
	if s := f.Sandbox; s != nil {
		if s.Enabled != nil {
			e.Sandbox.Enabled = *s.Enabled
		}
		if s.Network != nil {
			e.Sandbox.Network = *s.Network
		}
		for _, p := range s.Writable {
			e.Sandbox.Writable = append(e.Sandbox.Writable, expandPath(p))
		}
		for _, p := range s.Hide {
			e.Sandbox.Hide = append(e.Sandbox.Hide, expandPath(p))
		}
	}
	for _, d := range f.Dirs {
		e.Dirs = append(e.Dirs, expandPath(d))
	}
	if err := e.applyLimits(f.Limits); err != nil {
		return err
	}
	if err := e.applyEscalation(f.Escalation); err != nil {
		return err
	}
	if err := e.applyCompaction(f.Compaction); err != nil {
		return err
	}
	if err := e.applySearch(f.Search, layer); err != nil {
		return err
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
	if layer != "global" {
		// A repository's rules are an overlay: they can only tighten what
		// the global layer decides (PRD §10.6).
		e.Policy = e.Policy.With(rules)
	} else {
		e.Policy = policy.Layer(e.Policy.Base().Merge(rules), e.Policy.Overlays()...)
	}
	return nil
}

// checkRepositoryFile refuses what a repository's files may not set,
// however trusted: anything that loosens the global layer (secrets passed
// to child processes, an allow default for unanswered prompts, higher
// limits) or chooses where data goes (a search backend and its key,
// plugins). Everything else they set tightens or is the project's own
// business (model, roles, MCP servers, which run sandboxed).
func (e *Effective) checkRepositoryFile(f File) error {
	switch {
	case f.Env != nil:
		return errors.New("env: is global only: a repository cannot pass secrets to the processes agents run")
	case f.Sandbox != nil:
		return errors.New("sandbox: is global only: a repository cannot widen the boundary its commands run in")
	case len(f.Dirs) > 0:
		return errors.New("dirs: is global only: a repository cannot add directories its agents may work in")
	case f.Search != nil:
		return errors.New("search: is global only: a repository cannot choose where queries and keys go")
	case len(f.Plugins) > 0:
		return errors.New("plugins: is global only")
	case f.Escalation != nil && policy.Verb(f.Escalation.Default) == policy.Allow:
		return errors.New("escalation.default: a repository cannot make unanswered prompts allow")
	case f.Limits != nil && (f.Limits.MaxDepth > e.Limits.MaxDepth || f.Limits.MaxAgents > e.Limits.MaxAgents):
		return errors.New("limits: a repository may only lower them")
	}
	return nil
}

func (e *Effective) applyLimits(l *Limits) error {
	if l == nil {
		return nil
	}
	if l.MaxDepth < 0 || l.MaxAgents < 0 {
		return errors.New("limits: maxDepth and maxAgents must be positive")
	}
	if l.MaxDepth > 0 {
		e.Limits.MaxDepth = l.MaxDepth
	}
	if l.MaxAgents > 0 {
		e.Limits.MaxAgents = l.MaxAgents
	}
	return nil
}

func (e *Effective) applyEscalation(x *Escalation) error {
	if x == nil {
		return nil
	}
	if x.ClaimTimeout != "" {
		d, err := time.ParseDuration(x.ClaimTimeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("escalation.claimTimeout %q: want a duration such as 30s", x.ClaimTimeout)
		}
		e.Escalation.ClaimTimeout = d
	}
	if x.AnswerTimeout != "" {
		d, err := time.ParseDuration(x.AnswerTimeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("escalation.answerTimeout %q: want a duration such as 3m", x.AnswerTimeout)
		}
		e.Escalation.AnswerTimeout = d
	}
	if x.Default != "" {
		v := policy.Verb(x.Default)
		if v != policy.Allow && v != policy.Deny {
			return fmt.Errorf("escalation.default %q: allow or deny", x.Default)
		}
		e.Escalation.Default = v
	}
	return nil
}

func (e *Effective) applyCompaction(c *Compaction) error {
	if c == nil {
		return nil
	}
	if c.Threshold != 0 {
		if c.Threshold <= 0 || c.Threshold > 1 {
			return fmt.Errorf("compaction.threshold %v: a fraction of the context window between 0 and 1", c.Threshold)
		}
		e.Compaction.Threshold = c.Threshold
	}
	if c.MaxToolOutput != "" {
		n, err := parseSize(c.MaxToolOutput)
		if err != nil {
			return fmt.Errorf("compaction.maxToolOutput: %v", err)
		}
		e.Compaction.MaxToolOutput = n
	}
	return nil
}

func (e *Effective) applySearch(s *Search, layer string) error {
	if s == nil {
		return nil
	}
	if s.Provider != "" {
		p := strings.ToLower(s.Provider)
		switch p {
		case "brave", "tavily", "exa":
		default:
			return fmt.Errorf("search.provider %q: brave, tavily or exa", s.Provider)
		}
		e.Search.Provider = p
	}
	if s.APIKey != "" {
		if !strings.Contains(s.APIKey, "${env:") {
			fmt.Fprintf(os.Stderr, "stavlos: search.apiKey is written into the %s config; prefer \"${env:NAME}\" so the key stays in the environment\n", layer)
		}
		e.Search.APIKey = ExpandEnv(s.APIKey)
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
		base, _ := e.Policy.Decide(r.Tool, policy.Text(samplePattern(r.Pattern)))
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

// roleFile is the frontmatter of agents/<name>.md. tools and models take
// two shapes each (a list, or a map/list of objects), so they are decoded
// from nodes.
type roleFile struct {
	Description string    `yaml:"description"`
	Type        string    `yaml:"type"`
	Models      yaml.Node `yaml:"models"`
	Loop        string    `yaml:"loop"`
	Tools       yaml.Node `yaml:"tools"`
	Skills      []string  `yaml:"skills"`
	MCP         []string  `yaml:"mcp"`
	Spawn       []string  `yaml:"spawn"`
	MaxTurns    int       `yaml:"max_turns"`
	Color       string    `yaml:"color"`
	// Retired keys, named so the error can say what replaced them.
	Model  *string        `yaml:"model"`
	Policy map[string]any `yaml:"policy"`
	Hidden *bool          `yaml:"hidden"`
	Mode   *string        `yaml:"mode"`
	Dirs   []string       `yaml:"dirs"`
}

// RoleTools are the tools every role offers unless its tools: key removes
// one (todo stands for todo_add and todo_update). The messaging set and
// ask_user come on top for every agent, shell_kill with shell, and the
// lifecycle tools with a non-empty spawn list.
var RoleTools = []string{toolname.Shell, toolname.Read, toolname.Grep, toolname.Glob, toolname.ApplyPatch, toolname.Skill, toolname.GroupTodo, toolname.WebFetch, toolname.WebSearch}

// ReadPreset parses one agents/<name>.md file.
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
		Type: f.Type, Loop: f.Loop, Skills: f.Skills, MCP: f.MCP, Spawn: f.Spawn, MaxTurns: f.MaxTurns, Color: f.Color,
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
		return fail("hidden: is not supported; use type: or leave the role out of spawn lists")
	case f.Mode != nil:
		return fail("mode: is now type: (primary, subagent or all), so it does not read like the permission mode")
	case f.Dirs != nil:
		return fail("dirs: was removed: working directories belong to the channel (the dirs tab, or \"Allow and add\" on a boundary prompt)")
	case p.Description == "":
		return fail("description: is required")
	case p.MaxTurns < 0:
		return fail("max_turns: must be 0 or more")
	}
	switch p.Type {
	case "":
		p.Type = TypeAll
	case TypePrimary, TypeSubagent, TypeAll:
	default:
		return fail("type: %q must be primary, subagent or all", p.Type)
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

// parseTools reads the tools key, a map of tool → verb | {pattern: verb}.
// Every tool in RoleTools is offered unless the key removes it with a bare
// deny; any other verb, or patterns under a tool, is a rule that tightens
// the policy for a tool the role keeps. It returns the tools the role
// offers, in RoleTools order, and the rules.
func parseTools(n *yaml.Node) ([]string, map[string]map[string]string, error) {
	removed := map[string]bool{}
	rules := map[string]map[string]string{}
	switch n.Kind {
	case 0:
	case yaml.SequenceNode:
		return nil, nil, errors.New("is no longer a list: every tool is available, and <tool>: deny removes one")
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			name, val := n.Content[i].Value, n.Content[i+1]
			switch {
			case val.Kind == yaml.ScalarNode && (val.Tag == "!!null" || val.Value == ""):
				// listed with no rule: available under the layered policy
			case val.Kind == yaml.ScalarNode && policy.Verb(val.Value) == policy.Deny && contains(RoleTools, name):
				removed[name] = true
			case val.Kind == yaml.ScalarNode && policy.Verb(val.Value) == policy.Deny && keptTool(name) != "":
				return nil, nil, fmt.Errorf("%s: %s", name, keptTool(name))
			case val.Kind == yaml.ScalarNode:
				if !policy.Verb(val.Value).Valid() {
					return nil, nil, fmt.Errorf("%s: %q is not allow, ask or deny", name, val.Value)
				}
				rules[name] = map[string]string{"*": val.Value}
			case val.Kind == yaml.MappingNode:
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
	default:
		return nil, nil, errors.New("must be a map of tool → verb or pattern rules")
	}
	var tools []string
	for _, t := range RoleTools {
		if !removed[t] {
			tools = append(tools, t)
		}
	}
	return tools, rules, nil
}

// keptTool says why a role cannot remove a tool, "" when it can (or when it
// is not a built-in tool, where a deny is only a rule).
func keptTool(name string) string {
	switch name {
	case toolname.Message, toolname.AgentStatus, toolname.AskUser:
		return "every agent has it; it cannot be removed"
	case toolname.AgentCreate, toolname.AgentCancel:
		return "comes with spawn: leave spawn empty to remove it"
	case toolname.ShellKill:
		return "comes with shell: remove shell instead"
	case toolname.TodoAdd, toolname.TodoUpdate:
		return "remove todo, which covers todo_add and todo_update"
	}
	return ""
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

// ProjectHash lists the trust-gated files under dir (.stavlos/**, plus
// AGENTS.md) and hashes their contents (PRD §10.6).
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
func toolGroup(name string) []string { return toolname.Expand([]string{name}) }

// builtinPresets is the one role every install starts with. It can do
// everything and can delegate to copies of itself; users add specialised
// roles as <config>/agents/<name>.md or <project>/.stavlos/agents/<name>.md.
func builtinPresets() []Preset {
	return []Preset{
		{
			Name: "general", Layer: "builtin", Type: TypeAll,
			Description: "General-purpose engineer: reads, edits, runs, and delegates",
			Tools:       append([]string(nil), RoleTools...),
			Spawn:       []string{"general"},
			Loop:        "default",
			Body: `You are a senior software engineer working in the user's repository at the current working directory.
Work carefully: read before you edit, prefer small targeted changes, and run the project's tests or build after changing code.
Find files with glob, search their contents with grep, read them with read, and edit with apply_patch; shell is for building, testing and running things, and every command asks the human unless the channel's mode answers for them. A slow command such as a test suite continues as a background job on its own; start servers with background: true.
Delegate independent pieces of work to subagents when that saves your own context or lets things run in parallel: give each a specific task and a short label, then keep working or end your turn; each child's answer comes back to you as a message. A child stays alive in the channel: message it again for follow-ups. Subagents can delegate too.
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
