// Package config loads the three-layer configuration (PRD §10).
package config

import (
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

type MCP struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

// Preset is an archetype definition from agents/<name>.md (PRD §10.3).
type Preset struct {
	Name        string         `yaml:"-"`
	Description string         `yaml:"description"`
	Model       string         `yaml:"model"`
	Loop        string         `yaml:"loop"`
	Tools       []string       `yaml:"tools"`
	Skills      []string       `yaml:"skills"`
	MCP         []string       `yaml:"mcp"`
	Spawn       []string       `yaml:"spawn"`
	Policy      map[string]any `yaml:"policy"`
	Body        string         `yaml:"-"` // system prompt
	Source      string         `yaml:"-"` // file path
	Layer       string         `yaml:"-"` // global | project | builtin
}

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
	Policy   *policy.Set
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
	e := &Effective{Dir: dir, Presets: map[string]Preset{}, Skills: map[string]Skill{}, MCP: map[string]MCP{}}

	// defaults
	e.Model = ""
	e.RootAgent = "coder"
	e.Limits = Limits{MaxDepth: 3, MaxAgents: 6}
	e.Escalation.ClaimTimeout = 30 * time.Second
	e.Escalation.AnswerTimeout = 5 * time.Minute
	e.Escalation.Default = policy.Deny
	e.Compaction.Threshold = 0.8
	e.Compaction.MaxToolOutput = 32 * 1024
	e.Policy = policy.New(
		policy.Rule{Tool: "read", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "skill", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_create", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "finish", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_prompt", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_steer", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_cancel", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_kill", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_result", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "agent_status", Pattern: "*", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "*", Verb: policy.Ask},
		policy.Rule{Tool: "bash_async", Pattern: "*", Verb: policy.Ask},
		policy.Rule{Tool: "bash_kill", Pattern: "*", Verb: policy.Allow},
		// Read-only shell commands are allowed by default so searching and
		// looking around never prompts; anything that writes still asks.
		policy.Rule{Tool: "bash", Pattern: "grep *", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "rg *", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "find *", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "ls*", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "cat *", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "head *", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "tail *", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "wc *", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "pwd", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "tree*", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "git status*", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "git log*", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "git diff*", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "git show*", Verb: policy.Allow},
		policy.Rule{Tool: "bash", Pattern: "git blame*", Verb: policy.Allow},
		policy.Rule{Tool: "apply_patch", Pattern: "*", Verb: policy.Ask},
	)
	for _, p := range builtinPresets() {
		e.Presets[p.Name] = p
	}

	// global layer
	gdir := paths.ConfigDir()
	gf, err := readFile(filepath.Join(gdir, "stavlos.json"))
	if err != nil {
		return nil, fmt.Errorf("global config: %w", err)
	}
	e.applyFile(gf, "global")
	e.Plugins = gf.Plugins
	if err := e.loadPresets(filepath.Join(gdir, "agents"), "global"); err != nil {
		return nil, err
	}
	if err := e.loadSkills(filepath.Join(gdir, "skills")); err != nil {
		return nil, err
	}

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
			e.applyFile(pf, "project")
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

	// local layer (trusted, never prompts)
	lf, err := readFile(filepath.Join(pdir, "stavlos.local.json"))
	if err != nil {
		return nil, fmt.Errorf("local config: %w", err)
	}
	e.applyFile(lf, "local")
	return e, nil
}

func (e *Effective) applyFile(f File, layer string) {
	if f.Model != "" {
		e.Model = f.Model
	}
	if f.RootAgent != "" {
		e.RootAgent = f.RootAgent
	}
	if f.Limits != nil {
		if f.Limits.MaxDepth > 0 {
			e.Limits.MaxDepth = f.Limits.MaxDepth
		}
		if f.Limits.MaxAgents > 0 {
			e.Limits.MaxAgents = f.Limits.MaxAgents
		}
	}
	if f.Escalation != nil {
		if d, err := time.ParseDuration(f.Escalation.ClaimTimeout); err == nil {
			e.Escalation.ClaimTimeout = d
		}
		if d, err := time.ParseDuration(f.Escalation.AnswerTimeout); err == nil {
			e.Escalation.AnswerTimeout = d
		}
		if v := policy.Verb(f.Escalation.Default); v == policy.Allow || v == policy.Deny {
			e.Escalation.Default = v
		}
	}
	if f.Compaction != nil {
		if f.Compaction.Threshold > 0 {
			e.Compaction.Threshold = f.Compaction.Threshold
		}
		if n := parseSize(f.Compaction.MaxToolOutput); n > 0 {
			e.Compaction.MaxToolOutput = n
		}
	}
	for k, v := range f.MCP {
		e.MCP[k] = v
	}
	rules := ParsePolicy(f.Policy)
	if layer == "project" {
		e.Policy = e.Policy.Tighten(rules)
	} else {
		e.Policy = e.Policy.Merge(rules)
	}
}

// ParsePolicy converts the JSON policy shape into rules.
func ParsePolicy(m map[string]any) *policy.Set {
	var rules []policy.Rule
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, tool := range keys {
		switch v := m[tool].(type) {
		case string:
			if pv := policy.Verb(v); pv.Valid() {
				rules = append(rules, policy.Rule{Tool: tool, Pattern: "*", Verb: pv})
			}
		case map[string]any:
			pk := make([]string, 0, len(v))
			for k := range v {
				pk = append(pk, k)
			}
			sort.Strings(pk)
			for _, pat := range pk {
				if s, ok := v[pat].(string); ok {
					if pv := policy.Verb(s); pv.Valid() {
						rules = append(rules, policy.Rule{Tool: tool, Pattern: pat, Verb: pv})
					}
				}
			}
		}
	}
	return policy.New(rules...)
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
		e.Presets[p.Name] = p
	}
	return nil
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

// ReadPreset parses one agents/<name>.md file.
func ReadPreset(path string) (Preset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Preset{}, err
	}
	var p Preset
	body, err := frontmatter(string(b), &p)
	if err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	p.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	p.Body = strings.TrimSpace(body)
	p.Source = path
	if p.Loop == "" {
		p.Loop = "default"
	}
	if len(p.Tools) == 0 {
		p.Tools = []string{"bash", "read", "apply_patch", "skill"}
	}
	return p, nil
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
func readFile(path string) (File, error) {
	var f File
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(StripJSONC(b), &f); err != nil {
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

func parseSize(s string) int {
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
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n * mult
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

// PresetPolicy returns the preset's tightening rules.
func (p Preset) PresetPolicy() *policy.Set {
	return ParsePolicy(normalizeYAML(p.Policy))
}

// normalizeYAML converts yaml's map[string]interface{} nesting to the shape
// ParsePolicy expects (already map[string]any in yaml.v3).
func normalizeYAML(m map[string]any) map[string]any { return m }

func builtinPresets() []Preset {
	return []Preset{
		{
			Name: "coder", Layer: "builtin",
			Description: "Implements features and fixes bugs in this repository",
			Tools:       []string{"bash", "read", "apply_patch", "skill"},
			Spawn:       []string{"explorer", "tester"},
			Loop:        "default",
			Body: `You are a senior software engineer working in the user's repository at the current working directory.
Work carefully: read before you edit, prefer small targeted changes, and run the project's tests or build after changing code.
Delegate reading unfamiliar or large areas of code to an explorer subagent when it would save your own context; delegate running test suites to a tester subagent when the suite is slow.
When you spawn subagents, give each a specific task and a short label, then keep working or end your turn; each child's result comes back to you as a message when it finishes. Run slow commands such as test suites with bash_async.
Report what you changed and what you verified.`,
		},
		{
			Name: "explorer", Layer: "builtin",
			Description: "Read-only investigation of a codebase; reports findings",
			Tools:       []string{"read", "bash", "skill"},
			Loop:        "default",
			Body: `You are a read-only code explorer. Answer the question you were given by reading files and searching with bash (grep -rn, rg, find, ls). Do not modify anything.
When you have the answer, call finish with a concise summary that includes exact file paths and line numbers.`,
		},
		{
			Name: "tester", Layer: "builtin",
			Description: "Runs tests and builds; reports results",
			Tools:       []string{"bash", "read", "skill"},
			Loop:        "default",
			Body: `You run the project's tests, builds, or linters as instructed and report the results faithfully. Do not edit source files.
Call finish with the outcome: what you ran, whether it passed, and the relevant failing output if not.`,
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
		return os.WriteFile(p, []byte(fmt.Sprintf("{\n  \"model\": %q\n}\n", modelID)), 0o644)
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
	return os.WriteFile(p, []byte(s), 0o644)
}
