// Package tools implements the built-in tool set (PRD §14) and the
// orchestration tools (PRD §6.4).
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nicodes/stavlos/internal/clip"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/pathx"
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
	MaxOutput int              // truncate tool output beyond this many bytes (0 = clip.DefaultMax)
	Overflow  string           // a directory the whole of a truncated output is kept in for the agent to read ("" = not kept)
	Jobs      Jobs             // the agent's background jobs; nil if unavailable
	Todo      Todos            // the agent's todo list; nil if the role does not include "todo"
	Ask       Asker            // presents all questions immediately and waits; nil in tests without a runtime
	Search    SearchConfig     // web_search backend; zero → the tool explains how to configure it
	PassEnv   []string         // environment variables kept for child processes although their names look like secrets (config env.pass)
	Sandbox   *sandbox.Spec    // the boundary commands run in; nil runs them unsandboxed
	Sheets    Sheets           // the channel's sheets; nil when unavailable
	// Roots are the directories the file tools open through a root
	// (os.OpenRoot): the working set, the sheets and the scratch directory,
	// resolved. The policy judges a path with its links resolved; a link
	// swapped in between that check and the open would otherwise be
	// followed to wherever it leads (a key, the daemon's own files). Opened
	// through the root, a link that leaves it is refused instead. A path
	// under no root (one the human allowed outside the working set) is
	// opened as it is.
	Roots []string
	// Judged maps each path of the call, as the model wrote it, to the
	// filesystem path the policy judged (ResolvePath at decision time).
	Judged map[string]string
}

// at finds the root a resolved path lies beneath, opened, with the path
// relative to it. in is false for a path under no root.
func (env *Env) at(abs string) (root *os.Root, rel string, in bool, err error) {
	for _, r := range env.Roots {
		if rel, ok := pathx.Rel(r, abs); ok {
			root, err := os.OpenRoot(r)
			return root, rel, true, err
		}
	}
	return nil, "", false, nil
}

// openRead opens a regular file for reading, through its root where it has
// one.
func (env *Env) openRead(abs string) (*os.File, error) {
	root, rel, in, err := env.at(abs)
	if err != nil {
		return nil, err
	}
	var f *os.File
	if in {
		defer root.Close()
		f, err = root.Open(rel)
	} else {
		f, err = os.Open(abs)
	}
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%s is a directory", abs)
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", abs)
	}
	return f, nil
}

// check confirms that a judged path beneath a root still resolves inside
// it, for a tool that hands the path to a program (ripgrep) or a walk
// rather than opening it itself: a link swapped in since the judgement is
// refused here instead of followed there.
func (env *Env) check(abs string) error {
	root, rel, in, err := env.at(abs)
	if err != nil || !in {
		return err
	}
	defer root.Close()
	_, err = root.Stat(rel)
	return err
}

// remove deletes a file, through its root where it has one.
func (env *Env) remove(abs string) error {
	root, rel, in, err := env.at(abs)
	if err != nil {
		return err
	}
	if !in {
		return os.Remove(abs)
	}
	defer root.Close()
	return root.Remove(rel)
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
	// The child's model is the harness's choice among its role's
	// (docs/model-selection.md), never the caller's.
	Spawn(ctx context.Context, parent, archetype, label, task string) (id, name string, err error)
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
		todoTool{}, askTool{}, sheetTool{},
		webFetchTool{}, webSearchTool{},
	} {
		s[t.Def().Name] = t
	}
	return s
}

// The tool groups a role's list implies live in toolname; these names
// stay for the agent package's prompt assembly.
var (
	OrchestrationNames = toolname.Orchestration // implied by a non-empty spawn list
	MessagingNames     = toolname.Messaging     // every agent may message any other, or the human, and see the tree
	AsyncNames         = toolname.Async         // every agent that has shell
	AskNames           = toolname.Ask           // every agent: asking the human is never a role choice
	TodoNames          = toolname.Todo          // implied by "todo" in a role's tool list
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

// Clip bounds a tool result. What is cut is not lost: the whole output is
// written under env.Overflow and the result says where, so the next call is
// a read of the part that matters and not the same command again. (OpenCode
// does the same at 2,000 lines or 50 KB.)
func (env *Env) Clip(s string) string {
	max := env.MaxOutput
	if max <= 0 {
		max = clip.DefaultMax
	}
	if len(s) <= max {
		return s
	}
	out := clip.Middle(s, max)
	if env.Overflow == "" {
		return out
	}
	if err := os.MkdirAll(env.Overflow, 0o700); err != nil {
		return out
	}
	sum := sha256.Sum256([]byte(s))
	path := filepath.Join(env.Overflow, hex.EncodeToString(sum[:8])+".txt")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		return out
	}
	return out + fmt.Sprintf("\n[the whole output, %d bytes, is at %s: read it with an offset, or grep it]", len(s), path)
}
