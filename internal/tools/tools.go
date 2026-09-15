// Package tools implements the built-in tool set (PRD §14) and the
// orchestration tools (PRD §6.4).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
)

// Result is a tool's output.
type Result struct {
	Output  string
	IsError bool
}

// Env is what a tool gets from the calling agent.
type Env struct {
	Dir       string           // session working directory
	Skills    map[string]Skill // skills this agent may load
	Agent     string           // caller agent id
	Orch      Orchestrator     // nil if the agent cannot orchestrate
	Partial   func(string)     // receives streamed partial output (shell); may be nil
	MaxOutput int              // truncate tool output beyond this many bytes (0 = 32k)
	Mon       Monitors         // general monitors (background commands, watches, timers); nil if unavailable
	Todo      Todos            // the agent's todo list; nil if the preset does not include "todo"
	Ask       Asker            // raises a question batch to the human and waits; nil in tests without a runtime
	Search    SearchConfig     // web_search backend; zero → the tool explains how to configure it
	PassEnv   []string         // environment variables kept for child processes although their names look like secrets (config env.pass)
}

// Skill is a loadable skill: its front matter and its body.
type Skill struct {
	Name, Description, Body, Dir string
}

// Asker is implemented by the agent runtime: it blocks the turn on a
// question prompt (kind "question") until the human answers or the turn
// is cancelled. Answers come back one per question, in order.
type Asker interface {
	Ask(ctx context.Context, questions []protocol.Question) ([]string, error)
}

// Monitors is implemented by the agent runtime: background jobs, whose
// exit lands a result in the agent's mailbox and wakes it.
type Monitors interface {
	// AdoptCommand takes over a command the shell tool started (one that
	// outlived its wait window, or was started in the background) and
	// kills, reaps and reports it like any other job.
	AdoptCommand(command string, job Job, timeout time.Duration) (string, error)
	Stop(id string) error
	Has(id string) bool
}

// Job is a shell command running under the shell tool (a *proc.Job).
type Job interface {
	Done() <-chan struct{} // closed once the process has exited
	Err() error            // the exit error, valid after Done
	Kill()                 // ends the process group
	Output() string        // output so far (tail-capped)
	Lines() int
	Started() time.Time
}

// Tool is one callable tool.
type Tool interface {
	Def() model.ToolDef
	// Subject is what policy judges for a call: the strings rules match
	// (every path a patch touches, the URL as it will be fetched, the
	// command line) and their kind, which tells the harness what else the
	// text means.
	Subject(input json.RawMessage) policy.Subject
	Run(ctx context.Context, input json.RawMessage, env *Env) Result
}

// Orchestrator is implemented by the agent runtime (PRD §6.4).
type Orchestrator interface {
	// Spawn creates a child and returns its id and the name it got (label,
	// normalised and made unique in the session); dirs are directories to
	// grant it, each of which must be inside the parent's own.
	Spawn(ctx context.Context, parent, archetype, label, task, modelID string, dirs []string) (id, name string, err error)
	// Message sends text from the caller to an agent (name or id) or to
	// User. To an agent waiting on the caller it is an answer, delivered
	// between turns; to any other agent a new message, delivered at its next
	// step, that the caller then waits on. It returns the tool result.
	Message(caller, to, text string) (string, error)
	Cancel(parent, id string) error
	Status(parent, id string) ([]ChildStatus, error)
	// CanSpawn reports whether depth/fan-out limits currently permit a spawn.
	CanSpawn(agent string) (bool, string)
	// Archetypes the caller may spawn.
	Archetypes(agent string) []string
}

type ChildStatus struct {
	ID        string   `json:"id"`
	Parent    string   `json:"parent,omitempty"`
	You       bool     `json:"you,omitempty"` // this row is the caller
	Label     string   `json:"label"`
	Archetype string   `json:"archetype"`
	State     string   `json:"state"`
	Turn      int      `json:"turn"`
	CostUSD   float64  `json:"cost_usd"`
	Summary   string   `json:"summary,omitempty"`
	Dirs      []string `json:"dirs,omitempty"` // working directories (what a parent may grant on)
}

// Set is a named collection.
type Set map[string]Tool

// Builtin returns every built-in tool.
func Builtin() Set {
	s := Set{}
	for _, t := range []Tool{
		shellTool{}, readTool{}, patchTool{}, skillTool{},
		spawnTool{}, messageTool{}, cancelTool{}, statusTool{},
		shellKillTool{},
		todoAddTool{}, todoUpdateTool{}, askTool{},
		webFetchTool{}, webSearchTool{},
	} {
		s[t.Def().Name] = t
	}
	return s
}

// The tool groups a preset's list implies live in toolname; these names
// stay for the agent package's prompt assembly.
var (
	OrchestrationNames = toolname.Orchestration // implied by a non-empty spawn list
	MessagingNames     = toolname.Messaging     // every agent may message any other, or the human, and see the tree
	AsyncNames         = toolname.Async         // every agent that has shell
	AskNames           = toolname.Ask           // every agent: asking the human is never a role choice
	TodoNames          = toolname.Todo          // implied by "todo" in a preset's tool list
)

func errf(format string, a ...any) Result {
	return Result{Output: fmt.Sprintf(format, a...), IsError: true}
}

// Clip truncates tool output to max bytes (0 = 32k), keeping the head and
// the tail.
func Clip(s string, max int) string { return clip(s, max) }

func clip(s string, max int) string {
	if max <= 0 {
		max = 32 * 1024
	}
	if len(s) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	return cutRunes(s, head) + fmt.Sprintf("\n\n… [%d bytes truncated] …\n\n", len(s)-max) + tailRunes(s, tail)
}

// cutRunes returns at most n bytes of s, cut on a rune boundary so the
// model never receives half a character.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tailRunes returns at most the last n bytes of s, cut on a rune boundary.
func tailRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func decode(input json.RawMessage, v any) error {
	if len(input) == 0 {
		return nil
	}
	return json.Unmarshal(input, v)
}
