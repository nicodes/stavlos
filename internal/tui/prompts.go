package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/present"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/pkg/client"
)

// promptState is what waits on the human across every channel (the daemon
// sends every channel's prompts) and where the human is in answering it.
type promptState struct {
	permissionDrafts map[string]permissionDraft
	prompts          []protocol.PromptInfo // pending, oldest first, every channel's
	claimedByUs      map[string]bool
	promptBusy       string                   // prompt id with a claim/reply in flight
	permSel          int                      // highlighted option of the permission dialog
	permFor          string                   // the prompt id permSel belongs to (a new prompt starts at the top)
	permEdit         string                   // "" | "deny" (reason row open) | "dir" (path row open) in the permission dialog
	q                questionState            // the active inline question card
	questionDrafts   map[string]questionState // per-prompt drafts; IDs remain channel-local when displayed
	scope            promptScope              // permission dialog scope; zero = every channel
}

// promptScope limits the permission dialog to a channel or agent. Questions
// always belong to the viewed chat and do not use the global permission scope.
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
	details []protocol.QuestionAnswer
	typing  bool // the free-text field has the keys
}

// bind resets the state when the question card changes.
func (q *questionState) bind(p *protocol.PromptInfo) {
	if p == nil {
		*q = questionState{}
		return
	}
	if q.id == p.ID {
		return
	}
	*q = questionState{id: p.ID, marks: map[int]bool{}, answers: make([]string, len(p.Questions)), details: make([]protocol.QuestionAnswer, len(p.Questions))}
}

// questionsKey handles an inline question card: arrows select controls,
// space toggles options, typing opens the custom reply field, and Enter saves
// that text before a separate Submit. Legacy batches retain local navigation.
func (m *Model) questionsKey(msg tea.KeyMsg) tea.Cmd {
	defer func() { m.refreshViewport(); m.scrollToQuestionRow() }()
	p := m.currentQuestion()
	if p != nil && (m.promptBusy == p.ID || p.ClaimedBy != "" && !m.claimedByUs[p.ID]) {
		if key.Matches(msg, keys.OvClose) {
			return m.setFocus(focusInput)
		}
		return nil
	}
	if p == nil {
		if key.Matches(msg, keys.OvClose) {
			return m.setFocus(focusInput)
		}
		return nil
	}
	m.bindQuestion(p)
	if len(p.Questions) == 0 {
		return m.setFocus(focusInput)
	}
	if m.q.idx >= len(p.Questions) {
		m.q.idx = len(p.Questions) - 1
	}
	cur := p.Questions[m.q.idx]
	nopt := len(cur.Options) // the row after the options is "something else"
	rows := nopt + 2         // options, custom answer, Submit answer
	if m.q.typing {
		switch {
		case key.Matches(msg, keys.OvClose):
			m.q.custom = strings.TrimSpace(m.promptInput.Value())
			m.q.typing = false
			m.promptInput.Blur()
			return nil
		case key.Matches(msg, keys.Submit):
			m.q.custom = strings.TrimSpace(m.promptInput.Value())
			m.q.typing = false
			m.q.sel = nopt + 1
			m.promptInput.Blur()
			return nil
		}
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.TabLeft):
		if m.q.idx > 0 {
			m.q.idx--
			m.q.restoreAnswer()
		}
		return nil
	case key.Matches(msg, keys.TabRight):
		if m.q.idx+1 < len(p.Questions) {
			m.q.idx++
			m.q.restoreAnswer()
		}
		return nil
	case stepCursor(msg, &m.q.sel, rows, false): // no j/k: letters start the typed answer
		return nil
	case key.Matches(msg, keys.Submit): // enter confirms the answers; space toggles an option
		return m.confirmQuestion(p)
	case key.Matches(msg, keys.Select):
		if m.q.sel == nopt+1 {
			return m.confirmQuestion(p)
		}
		if m.q.sel == nopt { // "something else": type it
			m.q.typing = true
			m.promptInput.SetValue(m.q.custom)
			m.promptInput.CursorEnd()
			return m.promptInput.Focus()
		}
		m.q.marks[m.q.sel] = !m.q.marks[m.q.sel]
		return nil
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

func (m *Model) confirmQuestion(p *protocol.PromptInfo) tea.Cmd {
	var parts []string
	detail := protocol.QuestionAnswer{Custom: strings.TrimSpace(m.q.custom)}
	for i, o := range p.Questions[m.q.idx].Options {
		if m.q.marks[i] {
			parts = append(parts, o.Label)
			detail.Selected = append(detail.Selected, i)
		}
	}
	if detail.Custom != "" {
		parts = append(parts, detail.Custom)
	}
	answer := strings.Join(parts, ", ")
	if answer == "" {
		return nil
	}
	m.q.answers[m.q.idx], m.q.details[m.q.idx] = answer, detail
	m.q.typing = false
	m.promptInput.Reset()
	m.promptInput.Blur()
	if m.q.idx+1 < len(p.Questions) {
		m.q.idx++
		m.q.restoreAnswer()
		return nil
	}
	for i, a := range m.q.answers {
		if a == "" {
			m.q.idx = i
			m.q.restoreAnswer()
			return nil
		}
	}
	return m.answerQuestions(p, m.q.answers)
}

func (q *questionState) restoreAnswer() {
	q.sel, q.marks = 0, map[int]bool{}
	d := q.details[q.idx]
	q.custom = d.Custom
	for _, i := range d.Selected {
		q.marks[i] = true
	}
}

// answerQuestions sends a batch's answers.
func (m *Model) answerQuestions(p *protocol.PromptInfo, answers []string) tea.Cmd {
	answers = append([]string(nil), answers...)
	details := append([]protocol.QuestionAnswer(nil), m.q.details...)
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return call(ctx, c, protocol.PromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerAnswered, Answers: answers, Details: details})
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
			{"allow", present.AllowOnce, ""},
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
		{"allow", present.AllowOnce, ""},
		{"always", present.AllowChannel, what},
	}
	if pre := p.Prefix; pre != "" {
		desc := "every command starting with it"
		if p.Tool == toolname.WebFetch {
			desc = "every page on this host"
		}
		opts = append(opts, permOption{"prefix", present.AllowPrefix(pre), desc})
	}
	return append(opts, permOption{"deny", present.Deny, "with an optional reason"})
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
	if m.focus == focusInlinePermission {
		defer func() { m.refreshViewport(); m.scrollToPermissionRow() }()
		if m.loading {
			if key.Matches(msg, keys.Clear) {
				return m.closeDialog()
			}
			return nil
		}
	}
	p := m.currentPrompt()
	if p != nil && (m.promptBusy == p.ID || p.ClaimedBy != "" && !m.claimedByUs[p.ID]) {
		if key.Matches(msg, keys.Clear) {
			return m.closeDialog()
		}
		return nil
	}
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
			m.savePermissionDraft()
			m.permEdit = ""
			m.dirInput.Blur()
			return nil
		case key.Matches(msg, keys.Submit):
			text := strings.TrimSpace(m.dirInput.Value())
			edit := m.permEdit
			if edit == "dir" && text == "" {
				return nil
			}
			m.savePermissionDraft()
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
		return m.choosePermission(p, opts[m.permSel].id)
	}
	return nil
}

func (m *Model) choosePermission(p *protocol.PromptInfo, choice string) tea.Cmd {
	switch choice {
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
	case "deny":
		m.permEdit = "deny"
		m.dirInput.Placeholder = "why not? (optional) · enter denies"
		m.dirInput.SetValue("")
	default:
		return nil
	}
	if d := m.permissionDrafts[p.ID]; d.edit == m.permEdit {
		m.dirInput.SetValue(d.text)
	}
	m.dirInput.CursorEnd()
	return m.dirInput.Focus()
}

// applyPromptNotification keeps chat cards in step with the daemon. Questions
// and permissions do not take focus on arrival; project trust may open its
// dialog when the input is idle.
func (m *Model) applyPromptNotification(n protocol.PromptNotification) tea.Cmd {
	before := len(m.prompts)
	switch n.Action {
	case protocol.ActionRequested, protocol.ActionEscalated, protocol.ActionClaimed:
		m.upsertPrompt(n.Prompt)
	case protocol.ActionAnswered:
		m.resolvePrompt(n.Prompt.ID, true)
	case protocol.ActionWithdrawn, protocol.ActionDefaulted:
		m.removePrompt(n.Prompt.ID)
	}
	// The turn indicator switches between "working…" and "permission
	// requested" on prompt changes, which arrive outside the event stream.
	if n.Prompt.Channel == m.channelID || n.Prompt.Agent == m.selectedID() {
		m.viewDirty = true
	}
	if before == 0 && len(m.prompts) > 0 && (n.Prompt.Channel == "" || n.Prompt.Channel == m.channelID) && m.focus == focusInput && m.ov == nil && m.cfgEditor == nil && strings.TrimSpace(m.input.Value()) == "" {
		if n.Prompt.Kind == protocol.PromptQuestion || n.Prompt.Kind == protocol.PromptPermission {
			return nil // messages appear inline; arrival never takes the keyboard
		}
		return m.setFocus(focusPermission)
	}
	if m.focus == focusQuestions {
		m.bindQuestion(m.currentQuestion())
	}
	return nil
}

// currentQuestion is the active question, or the first pending card in the
// viewed chat. Another channel's prompts never participate in this selection.
func (m *Model) currentQuestion() *protocol.PromptInfo {
	var first *protocol.PromptInfo
	for i := range m.prompts {
		p := &m.prompts[i]
		if !m.questionVisible(*p) {
			continue
		}
		if p.ID == m.q.id {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

// currentPrompt picks the selected agent's oldest permission or trust prompt,
// otherwise the oldest within the permission dialog's scope.
func (m *Model) currentPrompt() *protocol.PromptInfo {
	if m.focus == focusInlinePermission {
		return m.inlinePermission()
	}
	sel := m.selectedID()
	var first *protocol.PromptInfo
	for i := range m.prompts {
		p := &m.prompts[i]
		if p.Kind == protocol.PromptQuestion || !m.scope.holds(*p) {
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
func (m *Model) promptCounts() (perms, questions int) {
	perms, _ = m.promptCountsIn(promptScope{})
	_, questions = m.promptCountsIn(promptScope{channel: m.channelID})
	return
}

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
	m.resolvePrompt(id, false)
}

func (m *Model) resolvePrompt(id string, answered bool) {
	delete(m.permissionDrafts, id)
	if m.permFor == id {
		m.permFor, m.permEdit = "", ""
		m.dirInput.Blur()
		if m.focus == focusInlinePermission {
			m.setFocus(focusInput)
		}
	}
	m.viewDirty = true
	delete(m.questionDrafts, id)
	delete(m.claimedByUs, id)
	if m.promptBusy == id {
		m.promptBusy = ""
	}
	i := m.findPrompt(id)
	if i < 0 {
		return
	} // duplicate notification/reply must not close the next question
	p := m.prompts[i]
	m.prompts = append(m.prompts[:i], m.prompts[i+1:]...)
	perms, _ := m.promptCountsIn(m.scope)
	if perms == 0 && m.focus == focusPermission {
		m.closeDialog() // the last permission it shows was answered: the dialog closes
	}
	if p.Kind == protocol.PromptQuestion && m.q.id == id {
		m.q = questionState{}
		if m.focus == focusQuestions {
			m.setFocus(focusInput)
		}
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
