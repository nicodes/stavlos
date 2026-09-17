// Package tools implements the built-in tool set (PRD §14) and the
// orchestration tools (PRD §6.4).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/nicodes/stavlos/internal/event"
	"time"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/sandbox"
	"github.com/nicodes/stavlos/internal/toolname"
)

// Result is a tool's output.
type Result struct {
	Output  string
	IsError bool
}

// Env is what a tool gets from the calling agent.
type Env struct {
	Dir       string           // channel working directory
	Skills    map[string]Skill // skills this agent may load
	Agent     string           // caller agent id
	Orch      Orchestrator     // nil if the agent cannot orchestrate
	Partial   func(string)     // receives streamed partial output (shell); may be nil
	MaxOutput int              // truncate tool output beyond this many bytes (0 = 32k)
	Jobs      Jobs             // the agent's background jobs; nil if unavailable
	Todo      Todos            // the agent's todo list; nil if the preset does not include "todo"
	Ask       Asker            // presents all questions immediately and waits; nil in tests without a runtime
	Search    SearchConfig     // web_search backend; zero → the tool explains how to configure it
	PassEnv   []string         // environment variables kept for child processes although their names look like secrets (config env.pass)
	Sandbox   *sandbox.Spec    // the boundary commands run in; nil runs them unsandboxed
}

// Skill is a loadable skill: its front matter and its body.
type Skill struct {
	Name, Description, Body, Dir string
}

// Asker publishes independent question prompts and blocks until all are
// answered or cancelled. Answers retain original positions, with empty slots
// for unanswered questions in an interrupted result.
type Asker interface {
	Ask(ctx context.Context, questions []protocol.Question) ([]string, error)
}

// Jobs is implemented by the agent runtime: background jobs, whose
// exit lands a result in the agent's mailbox and wakes it.
type Jobs interface {
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
	// normalised and made unique in the channel).
	Spawn(ctx context.Context, parent, archetype, label, task, modelID string) (id, name string, err error)
	// Message atomically sends text from the caller to agents (names or ids) or to
	// User, as kind KindRequest (the recipient owes a reply, the caller
	// waits; delivered at its next step), KindResponse (settles a request,
	// delivered between turns to the agent waiting on it) or KindInfo (no
	// reply, no wait, never wakes the recipient). Each sees the full recipient
	// list. Invalid targets reject the whole send. It returns the tool result.
	Message(caller string, to []string, text, kind string, replyTo ...string) (string, error)
	Cancel(parent, id string) error
	Status(parent, id string) ([]ChildStatus, error)
	// CanSpawn reports whether depth/fan-out limits currently permit a spawn.
	CanSpawn(agent string) (bool, string)
	// Archetypes the caller may spawn.
	Archetypes(agent string) []string
}

type ChildStatus struct {
	PendingReplies  []event.ReplyRequest `json:"pending_replies,omitempty"`
	AwaitingReplies []event.ReplyRequest `json:"awaiting_replies,omitempty"`
	ID              string               `json:"id"`
	Parent          string               `json:"parent,omitempty"`
	You             bool                 `json:"you,omitempty"` // this row is the caller
	Name            string               `json:"name"`
	Role            string               `json:"role"`
	State           string               `json:"state"`
	Turn            int                  `json:"turn"`
	CostUSD         float64              `json:"cost_usd"`
}

// Set is a named collection.
type Set map[string]Tool

// Builtin returns every built-in tool.
func Builtin() Set {
	s := Set{}
	for _, t := range []Tool{
		shellTool{}, readTool{}, grepTool{}, globTool{}, patchTool{}, skillTool{},
		spawnTool{}, messageTool{}, cancelTool{}, statusTool{},
		shellKillTool{},
		todoTool{}, askTool{},
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

func decode(input json.RawMessage, v any) error {
	if len(input) == 0 {
		return nil
	}
	return json.Unmarshal(input, v)
}
