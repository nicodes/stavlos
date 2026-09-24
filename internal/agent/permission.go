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
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/pathx"
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
	control  string      // the file that steers the harness this call edits, "" for none: no mode and no permit answers that ask
	egress   bool        // the call sends data off the machine: auto leaves it asking
	bare     bool        // a command with no sandbox under it: asked every time, like a control file
	why      string      // the denial when the harness refuses without a rule (auto outside the directories)
}

// runTool applies policy, escalates if needed, executes, and logs.
func (a *Agent) runTool(turnCtx context.Context, turn int, c model.Block, defs []model.ToolDef, rv roleView, cfg *config.Effective) {
	// A tool that runs must have its start on the record: with none, a command
	// would have run, a file changed, and the log would not say so. The turn
	// ends at its next step with the write's error (takeMidTurn).
	if err := a.record(event.ToolStarted, event.ToolStartedPayload{Turn: turn, CallID: c.ID, Name: c.Name}); err != nil {
		return
	}
	var carried []string // instructions files the output carries
	finish := func(out string, isErr, cancelled, denied bool) {
		_ = a.record(event.ToolFinished, event.ToolFinishedPayload{Turn: turn, CallID: c.ID, Name: c.Name, Output: out, IsError: isErr, Cancelled: cancelled, Denied: denied, Instructions: carried})
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
	// A call the rules let through silently still asks when it is the same
	// call, arguments and all, for the third time running: a model polling
	// a file or a command in a loop re-sends its whole context every time,
	// and nobody would have heard of it before the turn's cap.
	if d.verb == policy.Allow && a.repeated(c) {
		denial, withdrawn, allowed := a.askRepeat(turnCtx, c, rv)
		switch {
		case withdrawn:
			finish("", true, true, false)
			return
		case !allowed:
			finish(denial, true, false, true)
			return
		}
	}
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
	// The sheets directory is inside the working set for the file tools, so
	// the limits on sheets have to hold for them too, not only for the sheet
	// tool: a known sheet's file, and no larger than a sheet may be.
	var sheetsAfter func() string
	if c.Name == toolname.ApplyPatch {
		refusal, after := a.guardSheets(d.sub)
		if refusal != "" {
			finish(refusal, true, false, true)
			return
		}
		sheetsAfter = after
	}
	res := t.Run(turnCtx, c.Input, a.toolEnv(turn, c, rv, cfg))
	if sheetsAfter != nil && !res.IsError {
		if undone := sheetsAfter(); undone != "" {
			res = tools.Result{Output: undone, IsError: true}
		}
	}
	if turnCtx.Err() != nil {
		finish(res.Output, true, true, false)
		return
	}
	if !res.IsError && c.Name == toolname.ApplyPatch {
		a.sheetsPatched(d.sub)
	}
	if !res.IsError {
		if note, files := a.instructionsFor(d.sub, cfg); note != "" {
			res.Output += "\n\n" + note
			carried = files
		}
	}
	finish(res.Output, res.IsError, false, false)
}

// decide is the verdict for a call before any human is asked.
func (a *Agent) decide(c model.Block, t tools.Tool, rv roleView, cfg *config.Effective) decision {
	// Policy judges every value of the subject (each path a patch touches)
	// and the most restrictive decision wins; the prompt names that value.
	sub := t.Subject(c.Input)
	ruled, arg := cfg.Policy.With(rv.def.RolePolicy()).Decide(c.Name, sub)
	// The channel's sheets are part of the working set for the file tools
	// (never for commands: the sandbox builds its own list).
	dirs := append(a.c.dirPaths(), a.c.SheetDir())
	control := controlFile(c.Name, sub, a.c.Dir(), dirs)
	boundary := outsideDir(sub, a.c.Dir(), dirs)
	hidden := a.c.hiddenFrom(sub, cfg)
	a.c.mu.Lock()
	f := facts{
		hidden:    hidden != "",
		bare:      sub.Kind == policy.KindCommand && unsandboxed(cfg),
		ruled:     ruled,
		compound:  sub.Kind == policy.KindCommand && !shellcmd.Simple(arg),
		control:   control != "",
		permitted: a.c.st.permits.covers(c.Name, sub) || sub.Kind == policy.KindURL && hostsAllow(cfg.Hosts, sub.Values),
		mode:      a.c.st.mode,
		egress:    egress(c.Name, sub),
		outside:   boundary != "",
	}
	a.c.mu.Unlock()
	verb := judge(f)
	if f.control && ruled == policy.Allow {
		arg = control // the prompt names the file that steers the harness
	}
	why := ""
	if hidden != "" {
		boundary, why = "", hidden+" is hidden from agents in every mode: it holds credentials or Stavlos's own state. Ask the human for what you need from it."
	} else if ruled == policy.Deny {
		boundary = "" // the rules refused it: where it reaches is beside the point
	} else if verb == policy.Deny {
		why = autoOutside(boundary)
	}
	return decision{sub: sub, arg: arg, verb: verb, boundary: boundary, why: why, control: control, egress: f.egress, bare: f.bare}
}

// facts is everything the verdict on a call depends on, read once.
type facts struct {
	bare      bool        // a command, where the sandbox is wanted and the kernel offers none
	hidden    bool        // a file tool reaching into what no agent may (sandbox.go hiddenPaths)
	ruled     policy.Verb // what the rules say of the call, the role's tightening included
	compound  bool        // a command line that is more than one simple command
	control   bool        // an edit to a file that steers the harness
	permitted bool        // the human allowed it for the channel, or stavlos.json lists the host
	mode      string
	egress    bool // it sends data out
	outside   bool // it reaches outside the channel's directories
}

// judge is the verdict for a call before any human is asked, as stages that
// each only tighten or only loosen, in this order:
//
//  1. the rules;
//  2. what an allow rule cannot speak for: more than one simple command
//     ("cat *" says nothing about "cat x; rm -rf ~"), and the files that steer
//     the harness, which ask whatever the rules and the mode say;
//  3. what answers an ask, never a deny and never a control-file ask: a
//     standing permit, a listed host;
//  4. the mode, for what is still asking and for anything that reaches
//     outside the directories even when the rules allow it.
//
// It reads nothing but its argument, so every combination can be tested and
// a change of mode can be judged again without the call.
func judge(f facts) policy.Verb {
	if f.hidden {
		return policy.Deny // before the rules: no rule, permit or mode opens these
	}
	verb := f.ruled
	if verb == policy.Allow && (f.compound || f.control || f.bare) {
		verb = policy.Ask
	}
	if verb == policy.Ask && f.bare {
		// Nothing stands between this command and the machine: no permit and
		// no mode answers for the human, who turns the sandbox off in
		// stavlos.json if that is what they want.
		if f.outside && f.mode == protocol.ModeAuto {
			return policy.Deny
		}
		return policy.Ask
	}
	if verb == policy.Ask && !f.control && f.permitted {
		verb = policy.Allow
	}
	if verb != policy.Deny && (verb == policy.Ask || f.outside) {
		verb = ModeVerdict(f.mode, f.control, f.egress, f.outside)
	}
	return verb
}

// ModeVerdict is what a permission mode says to a call that would otherwise
// ask the human, from the three things a mode cares about: whether the call
// edits what steers the harness (sticky: only a human answers that, in any
// mode), whether it sends data out, and whether it reaches outside the
// channel's directories. Ask leaves it with the human. It is the one place a
// mode's meaning is written: a call being decided and a prompt already
// waiting when the mode changes are judged by it alike.
func ModeVerdict(mode string, sticky, egress, outside bool) policy.Verb {
	switch mode {
	case protocol.ModeYolo:
		if !sticky {
			return policy.Allow
		}
	case protocol.ModeAuto:
		switch {
		case outside:
			return policy.Deny
		case !sticky && !egress:
			return policy.Allow
		}
	default:
	}
	return policy.Ask
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
			rel, ok := pathx.Rel(tools.ResolvePath("", d), p)
			if !ok {
				continue
			}
			for _, cf := range controlFiles {
				if rel == cf || strings.HasPrefix(rel, cf+string(filepath.Separator)) {
					return v
				}
			}
			if slices.Contains(instructions.Names, filepath.Base(rel)) {
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
	if d.bare {
		question += " · no sandbox on this system: the command runs with your full access"
		prefix = "" // a standing allow would not be honoured: nothing to offer
	}
	ans := a.ask(turnCtx, protocol.PromptInfo{
		ID: NewID("p"), Channel: a.c.ID, ChannelName: a.c.Name(), Agent: a.ID, From: rv.name, Role: rv.role, Kind: protocol.PromptPermission, Tool: c.Name, Input: c.Input,
		Question: question, Dir: d.boundary, Prefix: prefix, Sticky: d.control != "" || d.bare, Egress: d.egress,
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

// repeatLimit is how many times in a row the same call runs before a human
// is asked (OpenCode's DOOM_LOOP_THRESHOLD).
const repeatLimit = 3

// repeated counts a call against the one before it and reports the one
// that reaches repeatLimit, except in yolo mode, which asks nothing. The
// count starts over at every turn and after every prompt.
func (a *Agent) repeated(c model.Block) bool {
	key := c.Name + "\x00" + string(c.Input)
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	if a.repeat.key == key {
		a.repeat.n++
	} else {
		a.repeat = repeatCall{key: key, n: 1}
	}
	if a.repeat.n < repeatLimit || a.c.st.mode == protocol.ModeYolo {
		return false
	}
	a.repeat.n = 0
	return true
}

// repeatCall is the last tool call and how many times running it was made.
type repeatCall struct {
	key string
	n   int
}

// askRepeat puts a repeated call to the human. The prompt is sticky: there
// is no standing allow for "the same thing again".
func (a *Agent) askRepeat(turnCtx context.Context, c model.Block, rv roleView) (denial string, withdrawn, allowed bool) {
	question := fmt.Sprintf("%s has made the same %s call, with the same arguments, %d times in a row", rv.name, c.Name, repeatLimit)
	ans := a.ask(turnCtx, protocol.PromptInfo{
		ID: NewID("p"), Channel: a.c.ID, ChannelName: a.c.Name(), Agent: a.ID, From: rv.name, Role: rv.role, Kind: protocol.PromptPermission, Tool: c.Name, Input: c.Input,
		Question: question, Sticky: true,
	}, c.ID)
	switch {
	case ans.Withdrawn:
		return "", true, false
	case ans.Value == protocol.AnswerAllow, ans.Value == protocol.AnswerAllowAlways, ans.Value == protocol.AnswerAllowPrefix:
		return "", false, true
	}
	denial = fmt.Sprintf("Denied: this is the same %s call, with the same arguments, for the %d%s time in a row. Do something different, or end the turn and let a job or a message wake you.", c.Name, repeatLimit, ordinal(repeatLimit))
	if ans.Defaulted {
		denial = "Denied: nobody answered the prompt and the headless default is deny. " + denial
	} else if r := strings.TrimSpace(ans.Reason); r != "" {
		denial += " The user says: " + r
	}
	return denial, false, false
}

func ordinal(n int) string {
	switch n {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	}
	return "th"
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
	_ = a.recordFact(event.AskResolved, res) // the prompt is closed whether or not the log takes it
	return ans
}

// toolEnv is what a tool gets from this agent for one call.
func (a *Agent) toolEnv(turn int, c model.Block, rv roleView, cfg *config.Effective) *tools.Env {
	return &tools.Env{Dir: a.c.Dir(), Agent: a.ID, Skills: skills(cfg, rv), Orch: orchestrator{c: a.c}, Jobs: jobsAPI{a: a}, Todo: a.todoAPIFor(rv), Ask: askAPI{a: a}, Sheets: sheetsAPI{a: a},
		MaxOutput: cfg.Compaction.MaxToolOutput, Overflow: a.overflowDir(), Search: tools.SearchConfig{Provider: cfg.Search.Provider, APIKey: cfg.Search.APIKey}, PassEnv: cfg.PassEnv,
		Sandbox: a.c.sandboxSpec(cfg),
		Partial: func(out string) {
			a.c.host.Stream(protocol.StreamNotification{Channel: a.c.ID, Agent: a.ID, Turn: turn, ToolName: c.Name, Text: out})
		}}
}

// overflowDir is where a tool output over the cap is kept whole.
func (a *Agent) overflowDir() string { return filepath.Join(paths.CacheDir(), "tmp", a.c.ID, "output") }

func hasDef(defs []model.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}
