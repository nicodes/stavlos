package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/pkg/client"
)

// promptState is what waits on the human across every channel (the daemon
// sends every channel's prompts) and where the human is in answering it.
type promptState struct {
	prompts     []protocol.PromptInfo // pending, oldest first, every channel's
	claimedByUs map[string]bool
	promptBusy  string        // prompt id with a claim/reply in flight
	permSel     int           // highlighted option of the permission dialog
	permFor     string        // the prompt id permSel belongs to (a new prompt starts at the top)
	permEdit    string        // "" | "deny" (reason row open) | "dir" (path row open) in the permission dialog
	q           questionState // the questions dialog: where the human is in the current batch
	scope       promptScope   // what the open permission or questions dialog is limited to; zero = every channel
}

// promptScope limits the permission and questions dialogs to one channel's
// prompts, or one agent's: set when opening a channel or agent that waits on
// the human opens its dialog, zero when a tab opens it.
type promptScope struct{ channel, agent string }

// holds reports whether p is within the scope.
func (s promptScope) holds(p protocol.PromptInfo) bool {
	return (s.channel == "" || p.Channel == s.channel) && (s.agent == "" || p.Agent == s.agent)
}

// questionState is where the human is inside an ask_user batch: which
// question, which row the cursor is on, the picks so far (options and a
// typed answer), and whether the text field has the keys.
type questionState struct {
	id      string       // the prompt the state belongs to
	idx     int          // current question
	sel     int          // row under the cursor: an option, or the last row ("something else")
	marks   map[int]bool // toggled options of the current question
	custom  string       // the typed "something else" answer of the current question
	answers []string     // one per question, "" until answered
	typing  bool         // the free-text field has the keys
}

// bind resets the state when the batch under the dialog changes.
func (q *questionState) bind(p *protocol.PromptInfo) {
	if p == nil {
		*q = questionState{}
		return
	}
	if q.id == p.ID {
		return
	}
	*q = questionState{id: p.ID, marks: map[int]bool{}, answers: make([]string, len(p.Questions))}
}

// questionsKey handles keys in the questions dialog. Every question is a
// checklist: ↑/↓ move over the options and the last row, "something else";
// space toggles an option, or opens the text field on the last row; typing
// anywhere opens it too. Enter confirms the current question — the toggled
// options plus any typed text, joined — and moves on; the last confirmation
// submits the batch. ←/→ move between questions to review. Esc leaves the
// text field, or closes the dialog (the batch keeps waiting).
func (m *Model) questionsKey(msg tea.KeyMsg) tea.Cmd {
	p := m.currentQuestion()
	if p == nil {
		if key.Matches(msg, keys.OvClose) {
			return m.closeDialog()
		}
		return nil
	}
	m.q.bind(p)
	if m.q.idx >= len(p.Questions) {
		m.q.idx = len(p.Questions) - 1
	}
	cur := p.Questions[m.q.idx]
	nopt := len(cur.Options) // the row after the options is "something else"
	rows := nopt + 1
	picked := func() string {
		var out []string
		for i, o := range cur.Options {
			if m.q.marks[i] {
				out = append(out, o.Label)
			}
		}
		if c := strings.TrimSpace(m.q.custom); c != "" {
			out = append(out, c)
		}
		return strings.Join(out, ", ")
	}
	confirm := func() tea.Cmd {
		answer := picked()
		if answer == "" {
			return nil // nothing chosen yet
		}
		m.q.answers[m.q.idx] = answer
		m.q.typing = false
		m.promptInput.Reset()
		m.promptInput.Blur()
		if m.q.idx+1 < len(p.Questions) {
			m.q.idx++
			m.q.sel, m.q.marks, m.q.custom = 0, map[int]bool{}, ""
			return nil
		}
		return m.answerQuestions(p, m.q.answers)
	}
	if m.q.typing {
		switch {
		case key.Matches(msg, keys.OvClose):
			m.q.custom = strings.TrimSpace(m.promptInput.Value())
			m.q.typing = false
			m.promptInput.Blur()
			return nil
		case key.Matches(msg, keys.Submit):
			m.q.custom = strings.TrimSpace(m.promptInput.Value())
			return confirm()
		}
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case key.Matches(msg, keys.TabLeft):
		if m.q.idx > 0 {
			m.q.idx--
			m.q.sel, m.q.marks, m.q.custom = 0, map[int]bool{}, ""
		}
		return nil
	case key.Matches(msg, keys.TabRight):
		if m.q.idx+1 < len(p.Questions) {
			m.q.idx++
			m.q.sel, m.q.marks, m.q.custom = 0, map[int]bool{}, ""
		}
		return nil
	case stepCursor(msg, &m.q.sel, rows, false): // no j/k: letters start the typed answer
		return nil
	case key.Matches(msg, keys.Select):
		if m.q.sel == nopt { // "something else": type it
			m.q.typing = true
			m.promptInput.SetValue(m.q.custom)
			m.promptInput.CursorEnd()
			return m.promptInput.Focus()
		}
		m.q.marks[m.q.sel] = !m.q.marks[m.q.sel]
		return nil
	case key.Matches(msg, keys.Submit):
		return confirm()
	case msg.Type == tea.KeyRunes || msg.Type == tea.KeyBackspace:
		// typing starts the "something else" answer
		m.q.typing = true
		m.q.sel = nopt
		m.promptInput.SetValue(m.q.custom)
		m.promptInput.CursorEnd()
		cmd := m.promptInput.Focus()
		var cmd2 tea.Cmd
		m.promptInput, cmd2 = m.promptInput.Update(msg)
		return tea.Batch(cmd, cmd2)
	}
	return nil
}

// answerQuestions sends a batch's answers.
func (m *Model) answerQuestions(p *protocol.PromptInfo, answers []string) tea.Cmd {
	answers = append([]string(nil), answers...)
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return call(ctx, c, protocol.PromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerAnswered, Answers: answers})
	})
}

// permOption is one row of the permission dialog's single-select list.
type permOption struct {
	id    string // allow | always | prefix | add | add_other | deny | trust | skip
	label string
	desc  string
}

// permOptions are the hard-coded answers a prompt offers, top to bottom.
// A plain permission: once, this exact call for the channel, the command's
// prefix for the channel (shell, when one can be derived), deny. A boundary
// prompt: once, add the offered directory, add another one, deny. Trust:
// trust the project config, or not now.
func permOptions(p *protocol.PromptInfo) []permOption {
	switch {
	case p.Kind == "trust":
		return []permOption{
			{"trust", "Trust this project's config", "until these files change"},
			{"skip", "Not now", "run on the global config only"},
		}
	case p.Dir != "":
		return []permOption{
			{"allow", "Allow once", ""},
			{"add", "Allow and add " + format.ShortHome(p.Dir), "every agent in the channel can use it"},
			{"add_other", "Allow and add another directory…", "type the path"},
			{"deny", "Deny", "with an optional reason"},
		}
	}
	what := "this exact call"
	switch p.Tool {
	case toolname.Shell:
		what = "this exact command"
	case toolname.WebFetch:
		what = "this exact URL"
	}
	opts := []permOption{
		{"allow", "Allow once", ""},
		{"always", "Allow for this channel", what},
	}
	if pre := p.Prefix; pre != "" {
		desc := "every command starting with it"
		if p.Tool == toolname.WebFetch {
			desc = "every page on this host"
		}
		opts = append(opts, permOption{"prefix", "Allow " + pre + " for this channel", desc})
	}
	return append(opts, permOption{"deny", "Deny", "with an optional reason"})
}

// permSelection is the highlighted row for p: the stored one when it
// belongs to this prompt, else the top.
func (m Model) permSelection(p *protocol.PromptInfo) int {
	if m.permFor != p.ID {
		return 0
	}
	if n := len(permOptions(p)); m.permSel >= n {
		return n - 1
	}
	return m.permSel
}

// permissionKey handles keys while the permission dialog has focus: ↑/↓
// move through the options, space chooses one (Deny opens a row for an
// optional reason, "add another directory" a row for the path, both
// submitted with enter and cancelled with esc); esc closes the dialog with
// the prompt still waiting.
func (m *Model) permissionKey(msg tea.KeyMsg) tea.Cmd {
	p := m.currentPrompt()
	if p == nil { // empty dialog: nothing to answer
		if key.Matches(msg, keys.Clear) {
			return m.closeDialog()
		}
		return nil
	}
	if m.permFor != p.ID {
		m.permFor, m.permSel, m.permEdit = p.ID, 0, ""
	}
	if m.permEdit != "" {
		switch {
		case key.Matches(msg, keys.OvClose):
			m.permEdit = ""
			m.dirInput.Blur()
			return nil
		case key.Matches(msg, keys.Submit):
			text := strings.TrimSpace(m.dirInput.Value())
			edit := m.permEdit
			if edit == "dir" && text == "" {
				return nil
			}
			m.permEdit = ""
			m.dirInput.Blur()
			if edit == "dir" {
				return m.answerPromptDir(p, text)
			}
			return m.denyPrompt(p, text)
		}
		var cmd tea.Cmd
		m.dirInput, cmd = m.dirInput.Update(msg)
		return cmd
	}
	opts := permOptions(p)
	n := len(opts)
	switch {
	case key.Matches(msg, keys.Clear):
		return m.closeDialog()
	case stepCursor(msg, &m.permSel, n, false):
		return nil
	case key.Matches(msg, keys.Select):
		if m.permSel >= n {
			m.permSel = n - 1
		}
		switch opts[m.permSel].id {
		case "allow", "trust":
			return m.answerPrompt(p, "allow")
		case "always", "add":
			return m.answerPrompt(p, "allow_always")
		case "prefix":
			return m.answerPromptPrefix(p)
		case "skip":
			return m.answerPrompt(p, "deny")
		case "add_other":
			m.permEdit = "dir"
			m.dirInput.Placeholder = "path (absolute, ~, or relative to the channel directory)"
			m.dirInput.SetValue(p.Dir)
			m.dirInput.CursorEnd()
			return m.dirInput.Focus()
		case "deny":
			m.permEdit = "deny"
			m.dirInput.Placeholder = "why not? (optional) · enter denies"
			m.dirInput.SetValue("")
			return m.dirInput.Focus()
		}
	}
	return nil
}

// applyPromptNotification keeps the prompt queue in step with the daemon.
// The first prompt to arrive while the input is idle (focused, nothing
// typed, no overlay) opens the permission tab so it can be answered at
// once; a draft in progress is never interrupted.
func (m *Model) applyPromptNotification(n protocol.PromptNotification) tea.Cmd {
	before := len(m.prompts) // every channel's prompts are kept: the tabs span channels
	switch n.Action {
	case protocol.ActionRequested, protocol.ActionEscalated, protocol.ActionClaimed:
		m.upsertPrompt(n.Prompt)
	case protocol.ActionAnswered, protocol.ActionWithdrawn, protocol.ActionDefaulted:
		m.removePrompt(n.Prompt.ID)
	}
	// The turn indicator switches between "working…" and "permission
	// requested" on prompt changes, which arrive outside the event stream.
	if n.Prompt.Agent == m.selectedID() {
		m.viewDirty = true
	}
	if before == 0 && len(m.prompts) > 0 && (n.Prompt.Channel == "" || n.Prompt.Channel == m.channelID) && m.focus == focusInput && m.ov == nil && strings.TrimSpace(m.input.Value()) == "" {
		if n.Prompt.Kind == "question" {
			return m.setFocus(focusQuestions)
		}
		return m.setFocus(focusPermission)
	}
	if m.focus == focusQuestions {
		m.q.bind(m.currentQuestion()) // a batch that changed under the dialog resets it
	}
	return nil
}

// currentPrompt is the head of the permission queue: the oldest waiting
// permission or trust prompt (questions have their own tab and queue).
func (m *Model) currentPrompt() *protocol.PromptInfo { return m.firstPrompt(false) }

// currentQuestion is the waiting ask_user batch the questions dialog shows.
func (m *Model) currentQuestion() *protocol.PromptInfo { return m.firstPrompt(true) }

// firstPrompt picks the prompt a dialog shows: the selected agent's oldest
// one when it has any (the footer controls the selected agent; the nav
// badges point at the others), else the oldest overall so nothing waits
// unseen. question selects the question batches or the permission-ish
// prompts.
func (m *Model) firstPrompt(question bool) *protocol.PromptInfo {
	sel := m.selectedID()
	var first *protocol.PromptInfo
	for i := range m.prompts {
		p := &m.prompts[i]
		if (p.Kind == "question") != question || !m.scope.holds(*p) {
			continue
		}
		if p.Agent == sel {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

// promptCounts is how many permission-ish prompts and question batches wait
// in every channel.
func (m *Model) promptCounts() (perms, questions int) { return m.promptCountsIn(promptScope{}) }

// promptCountsIn counts only what scope holds.
func (m *Model) promptCountsIn(scope promptScope) (perms, questions int) {
	for _, p := range m.prompts {
		if !scope.holds(p) {
			continue
		}
		if p.Kind == "question" {
			questions++
		} else {
			perms++
		}
	}
	return
}

func (m *Model) findPrompt(id string) int {
	for i, p := range m.prompts {
		if p.ID == id {
			return i
		}
	}
	return -1
}

func (m *Model) upsertPrompt(p protocol.PromptInfo) {
	cleanPrompt(&p)
	if i := m.findPrompt(p.ID); i >= 0 {
		m.prompts[i] = p
		return
	}
	m.prompts = append(m.prompts, p)
}

func (m *Model) removePrompt(id string) {
	if i := m.findPrompt(id); i >= 0 {
		m.prompts = append(m.prompts[:i], m.prompts[i+1:]...)
	}
	perms, questions := m.promptCountsIn(m.scope)
	if perms == 0 && m.focus == focusPermission {
		m.closeDialog() // the last permission it shows was answered: the dialog closes
	}
	if questions == 0 && m.focus == focusQuestions {
		m.closeDialog()
	}
	delete(m.claimedByUs, id)
	if m.promptBusy == id {
		m.promptBusy = ""
	}
}

// answerPrompt claims and replies. answer is allow | deny | allow_always or
// free text for questions; for trust prompts "allow" means trust.
func (m *Model) answerPrompt(p *protocol.PromptInfo, answer string) tea.Cmd {
	// A trust prompt is answered like any other, by id: the daemon knows
	// which directory and hash it asked about.
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return call(ctx, c, protocol.PromptReply, protocol.PromptReplyParams{ID: id, Answer: answer})
	})
}

// claimThen sends one answer to p through reply, unless an answer to it is
// already in flight or another client holds it; the command claims the
// prompt first.
func (m *Model) claimThen(p *protocol.PromptInfo, reply func(ctx context.Context, c *client.Client, id string) error) tea.Cmd {
	if m.promptBusy == p.ID {
		return m.setStatus("answer in flight…", false)
	}
	if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		return m.setStatus("claimed by another client", true)
	}
	id := p.ID
	m.promptBusy = id
	m.claimedByUs[id] = true
	return replyCmd(m.ctx, m.c, id, func(ctx context.Context, c *client.Client) error { return reply(ctx, c, id) })
}

// denyPrompt denies a permission, passing the human's reason (may be empty).
func (m *Model) denyPrompt(p *protocol.PromptInfo, reason string) tea.Cmd {
	if p.Kind == "trust" {
		return m.answerPrompt(p, "deny") // trust has its own reply; no reason field
	}
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return call(ctx, c, protocol.PromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerDeny, Reason: reason})
	})
}

// answerPromptPrefix allows the call and every command of the tool that
// starts with prefix for the channel.
func (m *Model) answerPromptPrefix(p *protocol.PromptInfo) tea.Cmd {
	if p.Prefix == "" {
		return m.answerPrompt(p, "allow_always")
	}
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return call(ctx, c, protocol.PromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerAllowPrefix})
	})
}

// answerPromptDir is allow_always on a boundary prompt with an edited
// directory: the call runs and that directory joins the agent's set.
func (m *Model) answerPromptDir(p *protocol.PromptInfo, dir string) tea.Cmd {
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return call(ctx, c, protocol.PromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerAllowAlways, Dir: dir})
	})
}
