package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/project"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// runTurn is one run of the agent loop (PRD §6.2): model call, tool calls,
// model call, … until the model stops calling tools, finish is called, or
// the turn is cancelled.
func (a *Agent) runTurn(inputs []event.UserMessagePayload) {
	turnCtx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	a.turn++
	turn := a.turn
	a.state = StateRunning
	a.cancelTurn = cancel
	a.finishFlag = false
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		a.cancelTurn = nil
		if a.state == StateRunning || a.state == StateWaiting || a.state == StateBlocked {
			a.state = StateIdle
		}
		a.mu.Unlock()
	}()

	bg := context.Background() // logging must not be cut short by cancellation
	if _, err := a.record(bg, event.TurnStarted, event.TurnPayload{Turn: turn}); err != nil {
		return
	}
	for _, in := range inputs {
		in.Turn = turn
		_, _ = a.record(bg, event.UserMessage, in)
	}

	// end flips the agent back to idle before logging TurnEnded, so a client
	// that reacts to the event never observes a stale "running" state.
	end := func(reason, errText string) {
		a.mu.Lock()
		a.cancelTurn = nil
		if a.state == StateRunning || a.state == StateWaiting || a.state == StateBlocked {
			a.state = StateIdle
		}
		a.mu.Unlock()
		_, _ = a.record(bg, event.TurnEnded, event.TurnEndedPayload{Turn: turn, Reason: reason, Error: errText})
	}

	for {
		if turnCtx.Err() != nil {
			end("cancelled", "")
			return
		}
		// Steer boundary: inject pending steers before the model call.
		a.mu.Lock()
		steers := a.steers
		a.steers = nil
		a.mu.Unlock()
		for _, st := range steers {
			_, _ = a.record(bg, event.UserMessage, event.UserMessagePayload{Turn: turn, Kind: "steer", Text: st.text})
		}

		modelID := a.ModelID()
		if modelID == "" {
			end("error", ErrNoModel)
			return
		}
		m, info, err := a.s.host.Resolve(modelID)
		if err != nil {
			end("error", err.Error())
			return
		}
		system, defs := a.buildContext()
		history := project.Project(a.eventsCopy())
		if info.ContextWindow > 0 {
			cfg := a.s.Config()
			if project.EstimateTokens(history, system) > int(float64(info.ContextWindow)*cfg.Compaction.Threshold) {
				if err := a.compact(turnCtx, m, system); err == nil {
					history = project.Project(a.eventsCopy())
				}
			}
		}
		if len(history) == 0 || history[len(history)-1].Role != model.RoleUser {
			history = append(history, model.Message{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "(continue)"}}})
		}

		resp, err := m.Complete(turnCtx, model.Request{Model: bareID(modelID), System: system, Messages: history, Tools: defs},
			func(d model.Delta) {
				a.s.host.Stream(protocol.StreamNotification{Session: a.s.ID, Agent: a.ID, Turn: turn, Text: d.Text, Thinking: d.Thinking, ToolName: d.ToolName})
			})
		if resp.Usage != (model.Usage{}) {
			cost := info.Cost(resp.Usage)
			a.mu.Lock()
			a.usage.tokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
			a.usage.cost += cost
			a.mu.Unlock()
			_, _ = a.record(bg, event.Usage, event.UsagePayload{Turn: turn, Model: modelID, Usage: resp.Usage, CostUSD: cost})
		}
		if err != nil {
			if turnCtx.Err() != nil {
				if len(resp.Blocks) > 0 {
					_, _ = a.record(bg, event.AssistantMessage, event.AssistantMessagePayload{Turn: turn, Blocks: resp.Blocks, StopReason: "cancelled", Model: modelID})
				}
				end("cancelled", "")
				return
			}
			end("error", err.Error())
			return
		}
		_, _ = a.record(bg, event.AssistantMessage, event.AssistantMessagePayload{Turn: turn, Blocks: resp.Blocks, StopReason: string(resp.StopReason), Model: modelID})

		var calls []model.Block
		for _, b := range resp.Blocks {
			if b.Type == model.BlockToolUse {
				calls = append(calls, b)
			}
		}
		if len(calls) == 0 {
			switch resp.StopReason {
			case model.StopMaxTokens:
				end("max_tokens", "")
			default:
				end("end_turn", "")
			}
			return
		}
		for _, c := range calls {
			if turnCtx.Err() != nil {
				break
			}
			a.runTool(turnCtx, turn, c, defs)
			a.mu.Lock()
			fin := a.finishFlag
			a.mu.Unlock()
			if fin {
				end("finished", "")
				a.completeFinish()
				return
			}
		}
	}
}

func bareID(full string) string {
	_, id, err := model.Split(full)
	if err != nil {
		return full
	}
	return id
}

// runTool applies policy, escalates if needed, executes, and logs.
func (a *Agent) runTool(turnCtx context.Context, turn int, c model.Block, defs []model.ToolDef) {
	bg := context.Background()
	_, _ = a.record(bg, event.ToolCallStarted, event.ToolStartedPayload{Turn: turn, CallID: c.ID, Name: c.Name, Input: c.Input})
	finish := func(out string, isErr, cancelled, denied bool) {
		_, _ = a.record(bg, event.ToolCallFinished, event.ToolFinishedPayload{Turn: turn, CallID: c.ID, Name: c.Name, Output: out, IsError: isErr, Cancelled: cancelled, Denied: denied})
	}
	t, ok := a.s.tools[c.Name]
	if !ok || !hasDef(defs, c.Name) {
		finish(fmt.Sprintf("unknown tool %q", c.Name), true, false, false)
		return
	}
	arg := t.PolicyArg(c.Input)
	verb := a.policy().Decide(c.Name, arg)
	key := c.Name + "\x00" + arg
	a.s.mu.RLock()
	always := a.s.allowAlways[key]
	a.s.mu.RUnlock()
	if always {
		verb = policy.Allow
	}
	switch verb {
	case policy.Deny:
		finish("Denied by policy: "+c.Name+" "+arg, true, false, true)
		return
	case policy.Ask:
		a.setState(StateBlocked)
		ans := a.s.host.Prompt(turnCtx, protocol.PromptInfo{
			ID: NewID("p"), Session: a.s.ID, Agent: a.ID, Kind: "permission", Tool: c.Name, Input: c.Input,
			Question: fmt.Sprintf("%s wants to run %s", a.Label, c.Name),
		})
		a.setState(StateRunning)
		switch {
		case ans.Withdrawn:
			finish("", true, true, false)
			return
		case ans.Value == "allow_always":
			a.s.mu.Lock()
			a.s.allowAlways[key] = true
			a.s.mu.Unlock()
		case ans.Value == "allow":
		default:
			why := "Permission denied by the user."
			if ans.Defaulted {
				why = "Permission denied: nobody answered the prompt and the headless default is deny."
			}
			finish(why, true, false, true)
			return
		}
	}

	cfg := a.s.Config()
	env := &tools.Env{Dir: a.s.Dir, Agent: a.ID, Skills: a.skills(cfg), Orch: a.orch(), MaxOutput: cfg.Compaction.MaxToolOutput,
		Partial: func(s string) {
			a.s.host.Stream(protocol.StreamNotification{Session: a.s.ID, Agent: a.ID, Turn: turn, ToolName: c.Name, Text: s})
		}}
	if c.Name == "wait" {
		a.setState(StateWaiting)
	}
	res := t.Run(turnCtx, c.Input, env)
	if c.Name == "wait" {
		a.setState(StateRunning)
	}
	if turnCtx.Err() != nil {
		finish(res.Output, true, true, false)
		return
	}
	finish(res.Output, res.IsError, false, false)
}

func hasDef(defs []model.ToolDef, name string) bool {
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func (a *Agent) setState(st State) {
	a.mu.Lock()
	if a.state != StateKilled && a.state != StateFinished {
		a.state = st
	}
	a.mu.Unlock()
}

// policy returns the effective policy for this agent: session policy
// tightened by the preset.
func (a *Agent) policy() *policy.Set {
	return a.s.Config().Policy.Tighten(a.preset.PresetPolicy())
}

func (a *Agent) skills(cfg *config.Effective) map[string]config.Skill {
	out := map[string]config.Skill{}
	for _, name := range a.preset.Skills {
		if sk, ok := cfg.Skills[name]; ok {
			out[name] = sk
		}
	}
	return out
}

// canOrchestrate reports whether the orchestration tools are offered.
func (a *Agent) canOrchestrate() bool { return len(a.preset.Spawn) > 0 }

func (a *Agent) orch() tools.Orchestrator {
	if !a.canOrchestrate() && a.Parent == "" {
		return nil
	}
	return orchestrator{s: a.s}
}

// buildContext assembles the system prompt and tool list for a model call.
func (a *Agent) buildContext() (string, []model.ToolDef) {
	cfg := a.s.Config()
	var sb strings.Builder
	sb.WriteString(a.preset.Body)
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "Working directory: %s\n", a.s.Dir)
	if a.Parent != "" {
		fmt.Fprintf(&sb, "You are a subagent (archetype %s, label %q) working for a parent agent. The task you were given arrives as the first message. When it is complete, or cannot be completed, call the finish tool exactly once with a summary; your parent only sees what you put there. Do not finish until the work is actually done.\n", a.Archetype, a.Label)
	}
	if cfg.AgentsMD != "" {
		sb.WriteString("\n# Project instructions (AGENTS.md)\n\n" + cfg.AgentsMD + "\n")
	}
	skills := a.skills(cfg)
	if len(skills) > 0 {
		sb.WriteString("\n# Skills\nLoad a skill with the skill tool when its description matches your task.\n")
		names := make([]string, 0, len(skills))
		for n := range skills {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&sb, "- %s: %s\n", n, skills[n].Description)
		}
	}

	names := append([]string(nil), a.preset.Tools...)
	if a.Parent != "" {
		names = append(names, "finish")
	}
	can, why := a.s.canSpawn(a)
	if a.canOrchestrate() {
		sb.WriteString("\n# Delegation\n")
		if can {
			sb.WriteString("You may spawn child agents with the spawn tool. Archetypes available to you:\n")
			for _, arch := range a.preset.Spawn {
				if p, ok := cfg.Presets[arch]; ok {
					fmt.Fprintf(&sb, "- %s: %s\n", arch, p.Description)
				}
			}
			fmt.Fprintf(&sb, "Limits: depth %d of %d, %d of %d agents live in this session. Children run in the background; their finish results arrive as messages, or use wait to block. Each child starts with no context beyond the task text you give it.\n", a.Depth, cfg.Limits.MaxDepth, a.s.Live(), cfg.Limits.MaxAgents)
			names = append(names, tools.OrchestrationNames...)
		} else {
			fmt.Fprintf(&sb, "You cannot spawn right now (%s). Do the work yourself.\n", why)
			for _, n := range tools.OrchestrationNames {
				if n != "spawn" {
					names = append(names, n)
				}
			}
		}
	}
	seen := map[string]bool{}
	var defs []model.ToolDef
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if t, ok := a.s.tools[n]; ok {
			defs = append(defs, t.Def())
		}
	}
	return sb.String(), defs
}

// completeFinish runs after the finish tool: logs AgentFinished, closes
// done, and delivers ChildFinished to the parent.
func (a *Agent) completeFinish() {
	a.mu.Lock()
	r := a.finished
	a.state = StateFinished
	a.mu.Unlock()
	if r == nil {
		return
	}
	var arts []event.Artifact
	for _, x := range r.Artifacts {
		arts = append(arts, event.Artifact{Path: x.Path, Description: x.Description})
	}
	_, _ = a.record(context.Background(), event.AgentFinished, event.AgentFinishedPayload{Summary: r.Summary, Status: r.Status, Artifacts: arts})
	a.closeDone()
	if p, ok := a.s.Agent(a.Parent); ok {
		p.deliverChildFinished(*r)
	}
	a.kill() // release the goroutine and any children
}

// compact summarises older turns (PRD §4.3). It picks the turn boundary
// nearest two thirds through the agent's events, summarises everything up
// to it with the model, and logs a Compacted event.
func (a *Agent) compact(ctx context.Context, m model.Model, system string) error {
	evs := a.eventsCopy()
	cut := -1
	target := len(evs) * 2 / 3
	for i, e := range evs {
		if e.Type == event.TurnEnded && i <= target {
			cut = i
		}
	}
	if cut < 0 {
		return errors.New("nothing to compact")
	}
	old := project.Project(evs[:cut+1])
	transcript := project.Transcript(old)
	if len(transcript) > 400_000 {
		transcript = transcript[len(transcript)-400_000:]
	}
	req := model.Request{
		Model:  bareID(a.ModelID()),
		System: "You summarise an AI coding agent's conversation so it can continue with less context. Preserve: the task and its current status, decisions made and why, files touched with paths, commands run and their outcomes, open problems, and anything the user asked for. Be concrete and complete; omit pleasantries.",
		Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText,
			Text: "Summarise this transcript:\n\n" + transcript}}}},
		MaxTokens: 4000,
	}
	resp, err := m.Complete(ctx, req, nil)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, b := range resp.Blocks {
		if b.Type == model.BlockText {
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 {
		return errors.New("empty summary")
	}
	_, err = a.record(context.Background(), event.Compacted, event.CompactedPayload{FromSeq: evs[0].Seq, ToSeq: evs[cut].Seq, Summary: sb.String()})
	return err
}

// Finish is called by the finish tool (via the orchestrator).
func (a *Agent) setFinished(summary, status string, arts []tools.Artifact) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished != nil {
		return errors.New("finish already called")
	}
	if a.Parent == "" {
		return errors.New("the root agent does not finish; just stop")
	}
	a.finished = &tools.ChildResult{ID: a.ID, Label: a.Label, Status: status, Summary: summary, Artifacts: arts}
	a.finishFlag = true
	return nil
}

var _ = json.Marshal
var _ escalation.Answer
