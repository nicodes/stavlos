package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

// animating reports whether anything on screen carries the spinner: the
// viewed chat's turn, running call or waiting post, or a dialog.
func (m *Model) animating() bool {
	if m.ov != nil {
		return true
	}
	t := m.transcripts[m.viewID()]
	if t != nil && (t.InTurn() || t.Running() || len(t.Waiting()) > 0) {
		return true
	}
	_, waiting := m.waitingOn() // an agent between turns waiting on agents or jobs keeps its spinner
	return !m.superChat && waiting
}

// ensureSpin schedules the spinner's tick when something animates and no
// tick is pending. An idle screen gets no ticks, so it is not redrawn
// twelve times a second for nothing.
func (m *Model) ensureSpin() tea.Cmd {
	if m.spinning || !m.animating() {
		return nil
	}
	m.spinning = true
	return m.sp.Tick
}

// onTick handles the timers: the spinner, the placeholder cycle, the
// compaction bar, the debounced tree refresh and the status line expiry.
func (m *Model) onTick(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case catalogTickMsg:
		m.viewDirty = true // the chat's stamps ("5 min") age; unchanged items redraw from the render cache
		cmds := []tea.Cmd{channelsCmd(m.ctx, m.c, m.requestScope(), channelsNav), tick(3*time.Second, catalogTickMsg{})}
		for _, s := range m.navChannels {
			if m.treeOpen[s.ID] {
				cmds = append(cmds, treeCmd(m.ctx, m.c, s.ID))
			}
		}
		return tea.Batch(cmds...)
	case spinner.TickMsg:
		if !m.animating() {
			m.spinning = false // the tick lapses; ensureSpin starts it again
			return nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		m.viewDirty = true // the "working…" indicator and the chat's loaders carry the spinner
		return cmd
	case placeholderTickMsg:
		m.input.Placeholder = m.placeholder()
		return placeholderTickCmd()
	case compactTickMsg:
		if !m.anyCompacting() {
			m.compactTick = false
			return nil
		}
		if t := m.transcripts[m.selectedID()]; t != nil && t.Compacting() {
			m.viewDirty = true
		}
		return compactTickCmd()
	case treeTickMsg:
		m.treeTimer = false
		cmds := []tea.Cmd{treeCmd(m.ctx, m.c, m.channelID)}
		for _, s := range m.navChannels { // the trees kept open beside it
			if s.ID != m.channelID && m.treeOpen[s.ID] {
				cmds = append(cmds, treeCmd(m.ctx, m.c, s.ID))
			}
		}
		return tea.Batch(cmds...)
	case clearStatusMsg:
		if msg.token == m.statusToken {
			m.status = ""
		}
	}
	return nil
}

// onDaemon handles what the daemon sends: the reconcile snapshot, events,
// streams, prompts, tree refreshes and call results. quit reports a lost
// connection or a failed attach.
func (m *Model) onDaemon(msg tea.Msg) (cmds []tea.Cmd, quit bool) {
	switch msg := msg.(type) {
	case customCommandsMsg:
		return []tea.Cmd{m.onCustomCommands(msg)}, false
	case customCommandRunMsg:
		if m.accepts(msg.scope) && msg.err != nil {
			return []tea.Cmd{m.setStatus(msg.err.Error(), true)}, false
		}
	case directoryMsg:
		if !m.accepts(msg.scope) {
			return nil, false
		}
		if msg.err != nil {
			return []tea.Cmd{m.setStatus(msg.err.Error(), true)}, false
		}
		m.generation++
		return []tea.Cmd{reconcileCmd(m.ctx, m.c, m.requestScope()), m.setStatus("default directory updated", false)}, false
	case reconcileMsg:
		return m.onReconcile(msg)
	case subscribedMsg:
		if !m.accepts(msg.scope) {
			return nil, false
		}
		if msg.err != nil {
			m.fatal = fmt.Errorf("subscribe: %w", msg.err)
			return nil, true
		}
	case eventMsg:
		return []tea.Cmd{m.applyEvent(msg.ev)}, false
	case streamMsg:
		if msg.n.Channel == "" || msg.n.Channel == m.channelID {
			m.transcript(msg.n.Agent).ApplyStream(msg.n)
			if msg.n.Agent == m.viewID() {
				m.viewDirty = true
			}
		}
	case promptMsg:
		return []tea.Cmd{m.applyPromptNotification(msg.n)}, false
	case disconnectedMsg:
		m.fatal = msg.err
		m.status, m.statusErr = "daemon disconnected", true
		return nil, true
	case treeMsg:
		if msg.channel != "" && msg.channel != m.channelID {
			// a tree kept open beside the bound channel; one the daemon no
			// longer holds simply keeps the tree it last had
			if msg.err == nil {
				m.keepTree(msg.channel, cleanAgents(msg.agents))
			}
			return nil, false
		}
		if msg.err != nil {
			return []tea.Cmd{m.setStatus("tree: "+msg.err.Error(), true)}, false
		}
		m.setAgents(msg.agents)
		if m.sidebarVisible() {
			return []tea.Cmd{channelsCmd(m.ctx, m.c, m.requestScope(), channelsNav)}, false
		}
	case resultMsg:
		if msg.err != nil {
			return []tea.Cmd{m.setStatus(msg.err.Error(), true)}, false
		}
		if msg.ok != "" {
			return []tea.Cmd{m.setStatus(msg.ok, false)}, false
		}
	case promptReplyMsg:
		return []tea.Cmd{m.onPromptReply(msg)}, false
	}
	return nil, false
}

func (m *Model) onReconcile(msg reconcileMsg) ([]tea.Cmd, bool) {
	if !m.accepts(msg.scope) || msg.res.Channel.ID != "" && msg.res.Channel.ID != m.channelID {
		return nil, false
	}
	if msg.err != nil {
		m.fatal = fmt.Errorf("reconcile: %w", msg.err)
		return nil, true
	}
	if m.reconciled && msg.res.Seq < m.seq {
		return []tea.Cmd{reconcileCmd(m.ctx, m.c, m.requestScope())}, false
	}
	m.channel = cleanChannel(msg.res.Channel)
	m.customCommands = nil
	m.reconciled = true
	m.setAgents(msg.res.Agents)
	if id := m.selectNext; id != "" {
		m.selectNext = ""
		if i := m.findAgent(id); i >= 0 {
			m.openAgent(i)
		}
	}
	for _, p := range msg.res.Prompts {
		m.upsertPrompt(p)
	}
	m.replayTo = msg.res.Seq
	cmds := []tea.Cmd{subscribeCmd(m.ctx, m.c, m.requestScope(), m.seq+1), channelsCmd(m.ctx, m.c, m.requestScope(), channelsNav), rolesCmd(m.ctx, m.c, m.requestScope(), true), customCommandsCmd(m.ctx, m.c, m.requestScope())}
	if m.rememberViews && m.lastRemembered != m.channelID {
		m.lastRemembered = m.channelID
		cmds = append(cmds, rememberChannelCmd(m.channelID))
	}
	if m.loading = msg.res.Seq > m.seq; !m.loading {
		cmds = append(cmds, m.caughtUp()...)
	}
	return cmds, false
}

// onPromptReply settles an answer the daemon accepted or refused.
func (m *Model) onPromptReply(msg promptReplyMsg) tea.Cmd {
	m.viewDirty = true
	if m.promptBusy == msg.id {
		m.promptBusy = ""
	}
	switch {
	case msg.err == nil:
		m.resolvePrompt(msg.id, true)
	case isConflict(msg.err):
		delete(m.claimedByUs, msg.id)
		if i := m.findPrompt(msg.id); i >= 0 && m.prompts[i].ClaimedBy == "" {
			m.prompts[i].ClaimedBy = "?"
		}
		return m.setStatus("claimed by another client", true)
	default:
		delete(m.claimedByUs, msg.id)
		return m.setStatus("prompt: "+msg.err.Error(), true)
	}
	return nil
}

// onDirChanged keeps the channel's working directories in step with the log.
// The attach snapshot may already hold a replayed add, so adds are
// idempotent.
func (m *Model) onDirChanged(ev event.Event) {
	switch ev.Type {
	case event.ChannelDirAdded:
		var p event.DirPayload
		if ev.Decode(&p) != nil || p.Dir == m.channel.Dir || slices.ContainsFunc(m.channel.Dirs, func(d protocol.DirInfo) bool { return d.Path == p.Dir }) {
			return
		}
		m.channel.Dirs = append(m.channel.Dirs, protocol.DirInfo{Path: textsafe.Clean(p.Dir), Source: p.Source})
	default:
		var p event.DirPayload
		if ev.Decode(&p) == nil {
			m.channel.Dirs = slices.DeleteFunc(m.channel.Dirs, func(d protocol.DirInfo) bool { return d.Path == p.Dir })
		}
	}
}

// caughtUp runs once the channel's replay has reached the snapshot: the
// view is drawn, the tree refetched, and a compaction that was running when
// the TUI attached gets its animation.
func (m *Model) caughtUp() []tea.Cmd {
	m.viewDirty = true
	cmds := []tea.Cmd{m.markTreeDirty()}
	if m.anyCompacting() && !m.compactTick {
		m.compactTick = true
		cmds = append(cmds, compactTickCmd())
	}
	return cmds
}

// applyEvent routes one log event into the right transcript and schedules a
// tree refresh for events that change agent state or cost.
func (m *Model) applyEvent(ev event.Event) tea.Cmd {
	if ev.Channel != "" && ev.Channel != m.channelID {
		return nil
	}
	if ev.Seq > m.seq {
		m.seq = ev.Seq
	}
	target, cmds := m.eventSideEffects(ev)
	m.followChild(ev)
	if target != "" {
		m.transcript(target).Apply(ev)
		if !m.loading && target == m.viewID() {
			m.viewDirty = true
		}
	}
	if transcript.ChatEvent(ev.Type) {
		m.transcript(chatView).Apply(ev)
		if !m.loading && m.superChat {
			m.viewDirty = true
		}
	}
	if !m.loading && changesTree(ev) {
		cmds = append(cmds, m.markTreeDirty())
	}
	if m.loading && ev.Seq >= m.replayTo {
		m.loading = false
		cmds = append(cmds, m.caughtUp()...)
	}
	return tea.Batch(cmds...)
}

// eventSideEffects applies what an event changes outside its transcript and
// returns the agent whose transcript shows it.
func (m *Model) eventSideEffects(ev event.Event) (target string, cmds []tea.Cmd) {
	target = ev.Agent
	switch ev.Type {
	case event.ChannelDirAdded, event.ChannelDirRemoved:
		m.onDirChanged(ev) // the channel's set; the chat of the agent whose prompt added one notes it
	case event.AgentSpawned:
		if id := m.onAgentSpawned(ev); id != "" {
			target = id
		}
	case event.AgentKilled:
		m.onAgentKilled(ev.Agent)
	case event.InputQueued, event.ChatPosted:
		m.rememberPrompt(ev)
	case event.CompactionStarted: // the chat item's bar animates until the result lands
		if !m.loading && !m.compactTick {
			m.compactTick = true
			cmds = append(cmds, compactTickCmd())
		}
	case event.ChannelUpdated:
		m.onChannelUpdated(ev)
		var p event.ChannelUpdatedPayload
		if ev.Decode(&p) == nil && p.Dir != nil && !m.loading {
			m.generation++
			cmds = append(cmds, reconcileCmd(m.ctx, m.c, m.requestScope()), m.setStatus("default directory changed to "+*p.Dir, false))
		}
	case event.TurnEnded:
		var p event.TurnEndedPayload
		if !m.loading && ev.Decode(&p) == nil && p.Reason == "error" &&
			(strings.Contains(p.Error, "not connected") || strings.Contains(p.Error, "/provider")) {
			cmds = append(cmds, m.setStatus("provider not connected — run /providers", true))
		}
	}
	return target, cmds
}

// onAgentSpawned records a new agent (a placeholder row until the tree
// refresh lands) and ties it to its parent's agent_create line. It returns
// the new agent's id, "" when the payload is unusable.
func (m *Model) onAgentSpawned(ev event.Event) string {
	var p event.AgentSpawnedPayload
	if ev.Decode(&p) != nil || p.ID == "" {
		return ""
	}
	if m.spawned == nil {
		m.spawned = map[string]time.Time{}
	}
	m.spawned[p.ID] = ev.Time
	if !m.loading && m.findAgent(p.ID) < 0 {
		// Placeholder until the debounced tree refresh lands.
		m.agents = append(m.agents, protocol.AgentInfo{
			ID: p.ID, Channel: ev.Channel, Parent: p.Parent, Role: p.Role,
			Name: p.Name, Model: p.Model, Variant: p.Variant, Depth: p.Depth, State: protocol.AgentIdle,
		})
	}
	if p.Parent != "" {
		// The parent's agent_create line tracks this child's life.
		if m.parentOf == nil {
			m.parentOf = map[string]string{}
		}
		m.parentOf[p.ID] = p.Parent
		m.transcript(p.Parent).ChildSpawned(p.ID)
		if !m.loading && p.Parent == m.viewID() {
			m.viewDirty = true
		}
	}
	return p.ID
}

// onAgentKilled reddens the killed agent's agent_create line and every
// question asked of it: no answer is coming.
func (m *Model) onAgentKilled(id string) {
	if parent := m.parentOf[id]; parent != "" {
		m.transcript(parent).ChildState(id, protocol.AgentKilled)
	}
	name := m.agentLabel(id)
	for _, t := range m.transcripts {
		t.AskerGone(name)
	}
	if !m.loading {
		m.viewDirty = true
	}
}

// rememberPrompt replays the human's message typed into an agent's chat
// into the input history.
func (m *Model) rememberPrompt(ev event.Event) {
	if ev.Seq > 0 && ev.Seq <= m.historySeq {
		return
	}
	if ev.Seq > m.historySeq {
		m.historySeq = ev.Seq
	}
	if ev.Type == event.ChatPosted {
		var p event.ChatPayload
		if !m.loading || ev.Decode(&p) != nil || p.Text == "" {
			return
		}
		text := p.Text
		if len(p.To) > 0 && (len(p.To) != 1 || p.To[0] != "main") {
			text = "@" + strings.Join(p.To, " @") + " " + text
		}
		if n := len(m.history); n == 0 || m.history[n-1] != text {
			m.history = append(m.history, text)
		}
		m.histIdx = len(m.history)
		return
	}
	var in event.Input
	if !m.loading || ev.Decode(&in) != nil || in.From != "" || in.Post != "" || in.Text == "" || in.Kind != event.InputPrompt && in.Kind != event.InputSteer {
		return
	}
	if n := len(m.history); n == 0 || m.history[n-1] != in.Text {
		m.history = append(m.history, in.Text)
	}
	m.histIdx = len(m.history)
}

// onChannelUpdated records the channel's new name, model or permission
// mode; a mode switch is noted in every agent's chat, like model and role
// changes, since the event has no agent of its own.
func (m *Model) onChannelUpdated(ev event.Event) {
	var p event.ChannelUpdatedPayload
	if ev.Decode(&p) != nil {
		return
	}
	if p.Name != nil {
		m.channel.Name = textsafe.Clean(*p.Name)
	}
	if p.Model != nil {
		m.channel.Model = *p.Model
	}
	if p.Dir != nil && !m.loading {
		m.channel.Dir, m.channel.DirError, m.channel.Mode = textsafe.Clean(*p.Dir), "", protocol.ModeAsk
	}
	if p.Mode == nil && p.Dir == nil {
		return
	}
	if p.Mode != nil {
		m.channel.Mode = *p.Mode
	}
	for _, a := range m.agents {
		m.transcript(a.ID).Apply(ev)
	}
	if !m.loading {
		m.viewDirty = true
	}
}

// followChild keeps the parent's agent_create line in step with the
// child's turns: yellow while a turn runs, grey between turns.
func (m *Model) followChild(ev event.Event) {
	parent := m.parentOf[ev.Agent]
	if parent == "" {
		return
	}
	switch ev.Type {
	case event.TurnStarted:
		m.transcript(parent).ChildState(ev.Agent, protocol.AgentRunning)
	case event.TurnEnded, event.TurnAborted:
		m.transcript(parent).ChildState(ev.Agent, protocol.AgentIdle)
	}
}

// changesTree reports whether ev changes what the tabs read off the agent
// tree, so the tree needs refreshing.
func changesTree(ev event.Event) bool {
	switch ev.Type {
	case event.AgentSpawned, event.AgentKilled, event.AgentUpdated, event.ChannelUpdated,
		event.TurnStarted, event.TurnEnded, event.TurnAborted, event.AssistantMessage,
		event.JobStarted, event.JobFinished, event.JobStopped, event.TodoChanged,
		event.MCPStarted, event.MCPFailed, event.MCPStopped,
		event.InputQueued, event.InputTaken, event.ChatMessage, // what an agent waits on or owes changes
		event.AskRequested, event.AskResolved: // blocked or running
		return true
	}
	return false
}

// anyCompacting reports whether some agent's chat shows a running compaction.
func (m *Model) anyCompacting() bool {
	for _, t := range m.transcripts {
		if t.Compacting() {
			return true
		}
	}
	return false
}

func (m *Model) markTreeDirty() tea.Cmd {
	if m.treeTimer {
		return nil
	}
	m.treeTimer = true
	return treeDebounceCmd()
}
