package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/shellcmd"
	"github.com/nicodes/stavlos/internal/tools"
)

// A tool call goes through three stages: decide (policy, the session's
// remembered allows, the mode, the directory boundary), escalate (the
// human, when the decision is ask), execute. Everything is logged as
// tool.started and tool.finished.

// decision is what the harness concluded about one call before running it.
type decision struct {
	sub      policy.Subject
	arg      string      // the subject value the verdict is about (a patch's worst path)
	verb     policy.Verb // allow, ask or deny after every rule has spoken
	boundary string      // the directory the call reaches outside the working set, "" when inside
	why      string      // the denial when the harness refuses without a rule (auto outside the directories)
}

// runTool applies policy, escalates if needed, executes, and logs.
func (a *Agent) runTool(turnCtx context.Context, turn int, c model.Block, defs []model.ToolDef, rv roleView) {
	bg := context.Background()
	_, _ = a.record(bg, event.ToolCallStarted, event.ToolStartedPayload{Turn: turn, CallID: c.ID, Name: c.Name, Input: c.Input})
	finish := func(out string, isErr, cancelled, denied bool) {
		_, _ = a.record(bg, event.ToolCallFinished, event.ToolFinishedPayload{Turn: turn, CallID: c.ID, Name: c.Name, Output: out, IsError: isErr, Cancelled: cancelled, Denied: denied})
	}
	t, ok := a.s.tools[c.Name]
	if !ok {
		t, ok = a.mcpTool(c.Name) // an MCP server's tool, owned by this agent
	}
	if !ok || !hasDef(defs, c.Name) {
		finish(fmt.Sprintf("unknown tool %q", c.Name), true, false, false)
		return
	}
	d := a.decide(c, t, rv)
	switch d.verb {
	case policy.Allow:
		// runs below
	case policy.Deny:
		why := d.why
		if why == "" {
			why = "Denied by policy: " + c.Name + " " + d.arg
		}
		finish(why, true, false, true)
		return
	case policy.Ask:
		denial, withdrawn, allowed := a.escalate(turnCtx, c, d, rv)
		switch {
		case withdrawn:
			finish("", true, true, false)
			return
		case !allowed:
			finish(denial, true, false, true)
			return
		}
	}
	res := t.Run(turnCtx, c.Input, a.toolEnv(turn, c, rv))
	if turnCtx.Err() != nil {
		finish(res.Output, true, true, false)
		return
	}
	finish(res.Output, res.IsError, false, false)
}

// decide is the verdict for a call before any human is asked.
func (a *Agent) decide(c model.Block, t tools.Tool, rv roleView) decision {
	// Policy judges every value of the subject (each path a patch touches)
	// and the most restrictive decision wins; the prompt names that value.
	sub := t.Subject(c.Input)
	arg := sub.Primary()
	pol := a.policy(rv)
	verb := pol.Decide(c.Name, arg)
	for _, x := range sub.Values[min(1, len(sub.Values)):] {
		if v := pol.Decide(c.Name, x); v.Rank() > verb.Rank() {
			verb, arg = v, x
		}
	}
	// A command allow rule speaks for one simple command: "cat *" says
	// nothing about "cat x; rm -rf ~" or "cat x > ~/.bashrc". A compound
	// command asks (auto and yolo then answer as they do for any ask).
	if sub.Kind == policy.KindCommand && verb == policy.Allow && !shellcmd.Simple(arg) {
		verb = policy.Ask
	}
	// What the human allowed for the session answers an ask, never a deny.
	if verb == policy.Ask && a.s.permits.covers(c.Name, sub) {
		verb = policy.Allow
	}
	mode := a.s.Mode()
	if verb == policy.Ask && mode != protocol.ModeAsk {
		verb = policy.Allow // auto and yolo answer every policy ask with allow
	}
	// A call that reaches outside the session's working directories is judged
	// by the mode even when policy allows the tool: ask mode asks (the prompt
	// names the directory; "allow_always" adds it to the agent), auto denies
	// it, yolo allows it.
	boundary, why := "", ""
	if verb != policy.Deny {
		if dir := a.outsideDir(sub); dir != "" {
			boundary = dir
			switch mode {
			case protocol.ModeYolo:
			case protocol.ModeAuto:
				verb, why = policy.Deny, autoOutside(dir)
			default:
				verb = policy.Ask
			}
		}
	}
	return decision{sub: sub, arg: arg, verb: verb, boundary: boundary, why: why}
}

// autoOutside is what an agent is told when auto mode denies a call outside
// its working directories.
func autoOutside(dir string) string {
	return "Denied in auto mode: " + dir + " is outside the session's working directories, and auto mode does not allow calls outside them."
}

// escalate asks the human about a call and records what they allowed for
// the session. It returns the denial text when the answer was no,
// withdrawn when the turn ended first, and allowed when the call may run.
func (a *Agent) escalate(turnCtx context.Context, c model.Block, d decision, rv roleView) (denial string, withdrawn, allowed bool) {
	a.setState(StateBlocked)
	question := fmt.Sprintf("%s wants to run %s", rv.label, c.Name)
	if d.boundary != "" {
		question = fmt.Sprintf("%s wants to run %s outside the session's directories (%s)", rv.label, c.Name, d.boundary)
	}
	// The prefix a client may offer to allow is the daemon's to derive
	// from the call itself; the prompt carries it for display.
	prefix := prefixFor(d.sub.Kind, d.arg)
	ans := a.s.host.Prompt(turnCtx, protocol.PromptInfo{
		ID: NewID("p"), Session: a.s.ID, Agent: a.ID, Kind: protocol.PromptPermission, Tool: c.Name, Input: c.Input,
		Question: question, Dir: d.boundary, Prefix: prefix,
	})
	a.setState(StateRunning)
	if ans.Withdrawn {
		return "", true, false
	}
	switch ans.Value {
	case protocol.AnswerAllowPrefix:
		if prefix != "" {
			a.grantPermit(event.PermitPayload{Tool: c.Name, Prefix: prefix})
		} else {
			a.grantPermit(event.PermitPayload{Tool: c.Name, Call: d.arg})
		}
	case protocol.AnswerAllowAlways:
		a.grantPermit(event.PermitPayload{Tool: c.Name, Call: d.arg})
	case protocol.AnswerAllow:
	default:
		why := "Permission denied by the user."
		if r := strings.TrimSpace(ans.Reason); r != "" {
			why = "Permission denied by the user: " + r
		}
		if ans.Defaulted {
			why = "Permission denied: nobody answered the prompt and the headless default is deny."
		}
		if ans.Client == protocol.ModeAuto && d.boundary != "" {
			why = autoOutside(d.boundary) // waiting when the session switched to auto
		}
		return why, false, false
	}
	if d.boundary != "" && ans.Value != protocol.AnswerAllow {
		dir := d.boundary
		if strings.TrimSpace(ans.Dir) != "" {
			dir = resolveDir(a.s.Dir, ans.Dir) // the human edited the offered directory
		}
		_ = a.s.addDir(context.Background(), a.ID, dir, "human")
	}
	return "", false, true
}

// grantPermit remembers an allow for the session and logs it, so recovery
// restores it.
func (a *Agent) grantPermit(p event.PermitPayload) {
	a.s.permits.apply(p)
	_, _ = a.record(context.Background(), event.PermitGranted, p)
}

// toolEnv is what a tool gets from this agent for one call.
func (a *Agent) toolEnv(turn int, c model.Block, rv roleView) *tools.Env {
	cfg := a.s.Config()
	return &tools.Env{Dir: a.s.Dir, Agent: a.ID, Skills: a.skills(cfg, rv), Orch: a.orch(), Mon: a.monitorsAPI(), Todo: a.todoAPIIfEnabled(), Ask: a.askAPI(), MaxOutput: cfg.Compaction.MaxToolOutput,
		Search: tools.SearchConfig{Provider: cfg.Search.Provider, APIKey: cfg.Search.APIKey}, PassEnv: cfg.PassEnv,
		Partial: func(s string) {
			a.s.host.Stream(protocol.StreamNotification{Session: a.s.ID, Agent: a.ID, Turn: turn, ToolName: c.Name, Text: s})
		}}
}

func hasDef(defs []model.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

// policy returns the effective policy for this agent: the session's
// layered policy with the role's rules as one more tightening overlay.
func (a *Agent) policy(rv roleView) *policy.Layered {
	return a.s.Config().Policy.With(rv.preset.PresetPolicy())
}

// orch is the runtime behind the agent_* tools. Every agent gets one
// (messaging is universal); which tools are offered is decided in
// buildContext.
func (a *Agent) orch() tools.Orchestrator { return orchestrator{s: a.s} }
