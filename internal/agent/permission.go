package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/instructions"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/shellcmd"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tools"
)

// A tool call goes through three stages: decide (policy, the channel's
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
func (a *Agent) runTool(turnCtx context.Context, turn int, c model.Block, defs []model.ToolDef, rv roleView, cfg *config.Effective) {
	_ = a.record(event.ToolStarted, event.ToolStartedPayload{Turn: turn, CallID: c.ID, Name: c.Name})
	finish := func(out string, isErr, cancelled, denied bool) {
		_ = a.record(event.ToolFinished, event.ToolFinishedPayload{Turn: turn, CallID: c.ID, Name: c.Name, Output: out, IsError: isErr, Cancelled: cancelled, Denied: denied})
	}
	t, ok := a.c.tools[c.Name]
	if !ok {
		t, ok = a.mcpTool(c.Name) // an MCP server's tool, owned by this agent
	}
	if !ok || !hasDef(defs, c.Name) {
		finish(fmt.Sprintf("unknown tool %q", c.Name), true, false, false)
		return
	}
	d := a.decide(c, t, rv, cfg)
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
	res := t.Run(turnCtx, c.Input, a.toolEnv(turn, c, rv, cfg))
	if turnCtx.Err() != nil {
		finish(res.Output, true, true, false)
		return
	}
	if !res.IsError && c.Name == toolname.ApplyPatch {
		a.sheetsPatched(d.sub)
	}
	if !res.IsError {
		if note := a.instructionsFor(d.sub, cfg); note != "" {
			res.Output += "\n\n" + note
		}
	}
	finish(res.Output, res.IsError, false, false)
}

// decide is the verdict for a call before any human is asked.
func (a *Agent) decide(c model.Block, t tools.Tool, rv roleView, cfg *config.Effective) decision {
	// Policy judges every value of the subject (each path a patch touches)
	// and the most restrictive decision wins; the prompt names that value.
	sub := t.Subject(c.Input)
	verb, arg := cfg.Policy.With(rv.preset.PresetPolicy()).Decide(c.Name, sub)
	// A command allow rule speaks for one simple command: "cat *" says
	// nothing about "cat x; rm -rf ~" or "cat x > ~/.bashrc".
	if sub.Kind == policy.KindCommand && verb == policy.Allow && !shellcmd.Simple(arg) {
		verb = policy.Ask
	}
	// The channel's sheets are part of the working set for the file tools
	// (never for commands: the sandbox builds its own list).
	dirs := append(a.c.dirPaths(), a.c.SheetDir())
	// An edit to the files that steer the harness itself asks whatever
	// policy says and whatever the mode.
	control := controlFile(c.Name, sub, a.c.Dir(), dirs)
	if control != "" && verb == policy.Allow {
		verb, arg = policy.Ask, control
	}
	a.c.mu.Lock()
	covered := verb == policy.Ask && a.c.st.permits.covers(c.Name, sub)
	mode := a.c.st.mode
	a.c.mu.Unlock()
	// What the human allowed for the channel answers an ask, never a deny,
	// and so do the hosts stavlos.json lists for a fetch.
	if covered || verb == policy.Ask && sub.Kind == policy.KindURL && hostsAllow(cfg.Hosts, sub.Values) {
		verb = policy.Allow
	}
	if verb == policy.Ask && control == "" && (mode == protocol.ModeYolo || mode == protocol.ModeAuto && !egress(c.Name, sub)) {
		verb = policy.Allow // yolo answers every other ask; auto every one that sends nothing out
	}
	// A call that reaches outside the working directories is judged by the
	// mode even when policy allows it: ask mode asks, auto denies, yolo
	// allows.
	boundary, why := "", ""
	if verb != policy.Deny {
		if dir := outsideDir(sub, a.c.Dir(), dirs); dir != "" {
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

// egress reports whether a call sends data out of the machine or to a
// process the harness does not inspect: a fetch, a search, an MCP tool.
// Auto mode leaves these asking.
func egress(tool string, sub policy.Subject) bool {
	return sub.Kind == policy.KindURL || tool == toolname.WebSearch || strings.HasPrefix(tool, toolname.MCPPrefix)
}

// controlFiles are the paths, relative to a working directory, whose edits
// always ask: the harness's config and roles, git's internals (hooks run
// code) and direnv's script. Every instructions file (AGENTS.md, CLAUDE.md)
// at any depth asks too: it is what every agent follows.
var controlFiles = []string{".stavlos", ".git", ".envrc"}

// controlFile is the first control file an apply_patch call edits, "" when
// it edits none.
func controlFile(tool string, sub policy.Subject, base string, dirs []string) string {
	if tool != toolname.ApplyPatch {
		return ""
	}
	for _, v := range sub.Values {
		p := tools.ResolvePath(base, v)
		for _, d := range dirs {
			rel, err := filepath.Rel(tools.ResolvePath("", d), p)
			if err != nil {
				continue
			}
			for _, cf := range controlFiles {
				if rel == cf || strings.HasPrefix(rel, cf+string(filepath.Separator)) {
					return v
				}
			}
			if !strings.HasPrefix(rel, "..") && slices.Contains(instructions.Names, filepath.Base(rel)) {
				return v
			}
		}
	}
	return ""
}

// autoOutside is what an agent is told when auto mode denies a call outside
// its working directories.
func autoOutside(dir string) string {
	return "Denied in auto mode: " + dir + " is outside the channel's working directories, and auto mode does not allow calls outside them."
}

// escalate asks the human about a call and records what they allowed for
// the channel. It returns the denial text when the answer was no,
// withdrawn when the turn ended first, and allowed when the call may run.
func (a *Agent) escalate(turnCtx context.Context, c model.Block, d decision, rv roleView) (denial string, withdrawn, allowed bool) {
	question := fmt.Sprintf("%s wants to run %s", rv.name, c.Name)
	if d.boundary != "" {
		question = fmt.Sprintf("%s wants to run %s outside the channel's directories (%s)", rv.name, c.Name, d.boundary)
	}
	// The prefix a client may offer to allow is the daemon's to derive from
	// the call itself; the prompt carries it for display.
	prefix := prefixFor(d.sub.Kind, d.arg)
	ans := a.ask(turnCtx, protocol.PromptInfo{
		ID: NewID("p"), Channel: a.c.ID, ChannelName: a.c.Name(), Agent: a.ID, From: rv.name, Role: rv.role, Kind: protocol.PromptPermission, Tool: c.Name, Input: c.Input,
		Question: question, Dir: d.boundary, Prefix: prefix,
	}, c.ID)
	if ans.Withdrawn {
		return "", true, false
	}
	switch ans.Value {
	case protocol.AnswerAllowPrefix, protocol.AnswerAllowAlways:
		var grants []event.Event
		if ans.Value == protocol.AnswerAllowPrefix && prefix != "" {
			grants = append(grants, a.c.event(a.ID, event.PermitGranted, event.PermitPayload{Tool: c.Name, Prefix: prefix}))
		} else {
			for _, v := range d.sub.Values { // the call as a whole: every path it touches
				grants = append(grants, a.c.event(a.ID, event.PermitGranted, event.PermitPayload{Tool: c.Name, Call: v}))
			}
		}
		_ = a.recordAll(grants...)
	case protocol.AnswerAllow:
	default:
		return denialText(ans, d), false, false
	}
	if d.boundary != "" && ans.Value != protocol.AnswerAllow {
		dir := d.boundary
		if strings.TrimSpace(ans.Dir) != "" {
			dir = resolveDir(a.c.Dir(), ans.Dir) // the human edited the offered directory
		}
		_ = a.c.addDir(context.Background(), a.ID, dir, "human")
	}
	return "", false, true
}

// denialText is what the agent is told when a prompt ends in no.
func denialText(ans escalation.Answer, d decision) string {
	switch {
	case ans.Defaulted:
		return "Permission denied: nobody answered the prompt and the headless default is deny."
	case ans.Client == protocol.ModeAuto && d.boundary != "":
		return autoOutside(d.boundary) // waiting when the channel switched to auto
	case strings.TrimSpace(ans.Reason) != "":
		return "Permission denied by the user: " + strings.TrimSpace(ans.Reason)
	}
	return "Permission denied by the user."
}

// ask logs a prompt, puts it to the human, and logs how it ended.
func (a *Agent) ask(ctx context.Context, info protocol.PromptInfo, callID string) escalation.Answer {
	return a.askOpened(ctx, info, callID, nil)
}

func (a *Agent) askOpened(ctx context.Context, info protocol.PromptInfo, callID string, opened func()) escalation.Answer {
	// ask.requested is logged once the prompt is open, so a client that sees
	// it can list the prompt; the answer is logged after.
	ans := a.c.host.Prompt(ctx, info, func() {
		_ = a.record(event.AskRequested, event.AskRequestedPayload{ID: info.ID, Kind: string(info.Kind), CallID: callID, Tool: info.Tool, Input: info.Input, Dir: info.Dir, Prefix: info.Prefix, Question: info.Question, Questions: info.Questions, From: info.From, Role: info.Role, QuestionNumber: info.QuestionNumber, QuestionTotal: info.QuestionTotal})
		if opened != nil {
			opened()
		}
	})
	res := event.AskResolvedPayload{ID: info.ID, Outcome: event.AskAnswered, Answer: ans.Value, By: ans.Client, Answers: ans.Answers, Details: ans.Details, Reason: ans.Reason, Dir: ans.Dir}
	switch {
	case ans.Withdrawn:
		res.Outcome = event.AskWithdrawn
	case ans.Defaulted:
		res.Outcome = event.AskDefaulted
	case len(ans.Answers) > 0:
		res.Answer = strings.Join(ans.Answers, " · ")
	}
	_ = a.record(event.AskResolved, res)
	return ans
}

// toolEnv is what a tool gets from this agent for one call.
func (a *Agent) toolEnv(turn int, c model.Block, rv roleView, cfg *config.Effective) *tools.Env {
	return &tools.Env{Dir: a.c.Dir(), Agent: a.ID, Skills: skills(cfg, rv), Orch: orchestrator{c: a.c}, Jobs: jobsAPI{a: a}, Todo: a.todoAPIFor(rv), Ask: askAPI{a: a}, Sheets: sheetsAPI{a: a},
		MaxOutput: cfg.Compaction.MaxToolOutput, Search: tools.SearchConfig{Provider: cfg.Search.Provider, APIKey: cfg.Search.APIKey}, PassEnv: cfg.PassEnv,
		Sandbox: a.c.sandboxSpec(cfg),
		Partial: func(out string) {
			a.c.host.Stream(protocol.StreamNotification{Channel: a.c.ID, Agent: a.ID, Turn: turn, ToolName: c.Name, Text: out})
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
