package discord

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/present"
	"github.com/nicodes/stavlos/internal/protocol"
)

func componentID(id, action, arg string) string { return "sv|" + id + "|" + action + "|" + arg }
func parseID(id string) (prompt, action, arg string) {
	p := strings.Split(id, "|")
	if len(p) != 4 || p[0] != "sv" {
		return "", "", ""
	}
	return p[1], p[2], p[3]
}
func button(id, action, label string, disabled bool) dg.MessageComponent {
	return dg.Button{CustomID: componentID(id, action, ""), Label: clip(label, 80), Style: dg.SecondaryButton, Disabled: disabled}
}
func row(c ...dg.MessageComponent) dg.MessageComponent { return dg.ActionsRow{Components: c} }

func promptView(p protocol.PromptInfo, clientID string) (string, []dg.MessageComponent) {
	disabled := p.ClaimedBy != "" && p.ClaimedBy != clientID
	text := fmt.Sprintf("**%s · %s**\n%s", p.From, p.Kind, p.Question)
	var buttons []dg.MessageComponent
	switch p.Kind {
	case protocol.PromptPermission:
		text = permissionHeading(p)
		buttons = append(buttons, button(p.ID, "allow", present.AllowOnce, disabled))
		if p.Dir != "" {
			buttons = append(buttons, button(p.ID, "always", present.AllowAddDir, disabled))
		} else if !p.Sticky { // a sticky ask is a human's every time: no standing allow to offer
			buttons = append(buttons, button(p.ID, "always", present.AllowChannel, disabled))
			if p.Prefix != "" {
				buttons = append(buttons, button(p.ID, "prefix", present.AllowPrefix(p.Prefix), disabled))
			}
		}
		buttons = append(buttons, button(p.ID, "deny-now", present.Deny, disabled), button(p.ID, "deny", "Deny with reason", disabled))
	case protocol.PromptQuestion:
		return questionPrompt(p, newDraft(p), clientID)
	case protocol.PromptTrust:
		text = trustText(p)
		buttons = []dg.MessageComponent{button(p.ID, "allow", "Trust this project's config", disabled), button(p.ID, "skip", "Not now", disabled)}
	}
	if p.Kind == protocol.PromptPermission && len(p.Input) > 0 {
		text += "\n" + permissionSubject(p)
	}
	if disabled {
		text += "\nClaimed by another client."
	}
	return text, []dg.MessageComponent{row(buttons...)}
}

// questionInteraction identifies controls whose acknowledgement updates the
// existing question card, rather than creating a private response message.
func questionInteraction(i *dg.Interaction) (string, bool) {
	var raw string
	switch i.Type {
	case dg.InteractionMessageComponent:
		raw = i.MessageComponentData().CustomID
	case dg.InteractionModalSubmit:
		raw = i.ModalSubmitData().CustomID
	default:
		return "", false
	}
	id, action, _ := parseID(raw)
	if id == "" {
		return "", false
	}
	switch action {
	case "questions", "pick", "toggle", "next", "back", "page-next", "page-back", "submit", "text", "text-submit", "clear-text", "allow", "always", "prefix", "deny-now", "deny-submit", "dir-submit", "skip":
		return id, true
	}
	return "", false
}

func (w *worker) questionDraft(p protocol.PromptInfo) *draft {
	d := w.drafts[p.ID]
	if d == nil {
		d = newDraft(p)
		w.drafts[p.ID] = d
	}
	return d
}

func (w *worker) promptView(p protocol.PromptInfo) (string, []dg.MessageComponent) {
	if p.Kind == protocol.PromptQuestion {
		return questionPrompt(p, w.questionDraft(p), w.link.id)
	}
	return promptView(p, w.link.id)
}

func questionPrompt(p protocol.PromptInfo, d *draft, clientID string) (string, []dg.MessageComponent) {
	text, components := questionView(p, d)
	if p.ClaimedBy != "" && p.ClaimedBy != clientID {
		text = clip(text, 1950) + "\nClaimed by another client."
		for _, c := range components {
			r := c.(dg.ActionsRow)
			for n, c := range r.Components {
				switch c := c.(type) {
				case dg.Button:
					c.Disabled = true
					r.Components[n] = c
				case dg.SelectMenu:
					c.Disabled = true
					r.Components[n] = c
				}
			}
		}
	}
	return text, components
}

func modalResponse(i *dg.Interaction) *dg.InteractionResponse {
	if i.Type != dg.InteractionMessageComponent {
		return nil
	}
	id, action, arg := parseID(i.MessageComponentData().CustomID)
	if id == "" {
		return nil
	}
	label := ""
	switch action {
	case "text":
		label = "Custom answer"
	case "deny":
		label = "Reason"
	case "dir":
		label = "Directory to add"
	default:
		return nil
	}
	return &dg.InteractionResponse{Type: dg.InteractionResponseModal, Data: &dg.InteractionResponseData{
		Title: label, CustomID: componentID(id, action+"-submit", arg), Components: []dg.MessageComponent{
			row(dg.TextInput{CustomID: "value", Label: label, Style: dg.TextInputParagraph, Required: true, MaxLength: 4000, Placeholder: modalPlaceholder(action)}),
		},
	}}
}

func modalPlaceholder(action string) string {
	if action == "text" {
		return "Save your custom answer here, then use Submit answer on the question card."
	}
	return ""
}

func modalValue(i *dg.Interaction) string {
	for _, c := range i.ModalSubmitData().Components {
		if r, ok := c.(*dg.ActionsRow); ok {
			for _, c := range r.Components {
				if t, ok := c.(*dg.TextInput); ok && t.CustomID == "value" {
					return t.Value
				}
			}
		}
	}
	return ""
}

func (w *worker) interact(ctx context.Context, i *dg.Interaction) (string, []dg.MessageComponent, error) {
	if i.Type == dg.InteractionApplicationCommand {
		return w.command(ctx, i)
	}
	if all, page, ok := statusPage(i); ok {
		return w.status(ctx, all, page)
	}
	var raw, value string
	switch i.Type {
	case dg.InteractionMessageComponent:
		raw = i.MessageComponentData().CustomID
	case dg.InteractionModalSubmit:
		raw, value = i.ModalSubmitData().CustomID, modalValue(i)
	default:
		return "", nil, errors.New("unsupported interaction")
	}
	id, action, arg := parseID(raw)
	if err := w.refreshPrompts(ctx); err != nil {
		return "", nil, err
	}
	p, ok := w.prompts[id]
	if !ok {
		return "This prompt is already resolved or not available here.", nil, nil
	}
	if p.ClaimedBy != "" && p.ClaimedBy != w.link.id {
		return "This prompt is claimed by another client.", nil, nil
	}
	if p.Kind == protocol.PromptQuestion {
		return w.question(ctx, i, p, action, arg, value)
	}
	answer, err := permissionAnswer(p, action, value)
	if err != nil {
		return "", nil, err
	}
	text, err := w.answer(ctx, i, p, answer)
	return text, nil, err
}

func permissionAnswer(p protocol.PromptInfo, action, value string) (protocol.PromptReplyParams, error) {
	r := protocol.PromptReplyParams{ID: p.ID}
	if p.Kind == protocol.PromptTrust {
		switch action {
		case "allow":
			r.Answer = protocol.AnswerAllow
		case "skip":
			r.Answer = protocol.AnswerDeny
		default:
			return r, errors.New("invalid trust answer")
		}
		return r, nil
	}
	if p.Kind != protocol.PromptPermission {
		return r, errors.New("invalid prompt kind")
	}
	switch action {
	case "allow":
		r.Answer = protocol.AnswerAllow
	case "always":
		r.Answer, r.Dir = protocol.AnswerAllowAlways, p.Dir
	case "prefix":
		if p.Prefix == "" || p.Dir != "" {
			return r, errors.New("this prompt has no prefix choice")
		}
		r.Answer = protocol.AnswerAllowPrefix
	case "dir-submit":
		if p.Dir == "" || strings.TrimSpace(value) == "" {
			return r, errors.New("directory choice is unavailable or empty")
		}
		r.Answer, r.Dir = protocol.AnswerAllowAlways, strings.TrimSpace(value)
	case "deny-now":
		r.Answer = protocol.AnswerDeny
	case "deny-submit":
		if strings.TrimSpace(value) == "" {
			return r, errors.New("provide a reason, or use Deny")
		}
		r.Answer, r.Reason = protocol.AnswerDeny, strings.TrimSpace(value)
	default:
		return r, errors.New("invalid permission action")
	}
	return r, nil
}

func (w *worker) answer(ctx context.Context, i *dg.Interaction, p protocol.PromptInfo, r protocol.PromptReplyParams) (string, error) {
	if _, err := call(ctx, w.link.rpc, protocol.PromptClaim, protocol.PromptClaimParams{ID: p.ID}); err != nil {
		return "", err
	}
	if _, err := call(ctx, w.link.rpc, protocol.PromptReply, r); err != nil {
		return "", err
	}
	labels := map[string]string{protocol.AnswerAllow: "Allowed once", protocol.AnswerAllowAlways: "Allowed for this channel", protocol.AnswerAllowPrefix: "Prefix allowed for this channel", protocol.AnswerDeny: "Denied", protocol.AnswerAnswered: "Questions answered"}
	label := labels[r.Answer]
	if p.Kind == protocol.PromptTrust {
		label = "Trust decision submitted"
		if r.Answer == protocol.AnswerDeny {
			label = "Trust deferred"
		}
	}
	text := "✓ " + label + " — <@" + interactionUser(i) + ">"
	if r.Reason != "" {
		text += "\n" + r.Reason
	}
	if p.Kind == protocol.PromptQuestion {
		text = answeredQuestions(p, w.drafts[p.ID])
	}
	if p.Kind == protocol.PromptPermission {
		text = recordedPermissionResult(p, event.AskResolvedPayload{Outcome: event.AskAnswered, Answer: r.Answer, Reason: r.Reason, Dir: r.Dir})
	}
	if p.Kind == protocol.PromptTrust {
		note := "\n\n" + text
		text = clipMarkdown(trustText(p), max(0, 2000-units(note))) + note
	}
	if err := w.finishPrompt(ctx, p.ID, text); err != nil {
		return "Decision accepted; could not update the public prompt.", err
	}
	return text, nil
}

func (w *worker) command(ctx context.Context, i *dg.Interaction) (string, []dg.MessageComponent, error) {
	d := i.ApplicationCommandData()
	switch d.Name {
	case "status":
		all := false
		if len(d.Options) > 1 {
			return "", nil, errors.New("use /status active or /status all")
		}
		for _, option := range d.Options {
			if option.Type != dg.ApplicationCommandOptionSubCommand || option.Name != "active" && option.Name != "all" {
				return "", nil, errors.New("use /status active or /status all")
			}
			all = option.Name == "all"
		}
		return w.status(ctx, all, 0)
	case "cancel":
		name := "main"
		for _, o := range d.Options {
			if o.Name == "agent" {
				name = strings.TrimPrefix(o.StringValue(), "@")
			}
		}
		r, err := call(ctx, w.link.rpc, protocol.AgentTree, protocol.AgentTreeParams{Channel: w.id})
		if err != nil {
			return "", nil, err
		}
		for _, a := range r.Agents {
			if !strings.EqualFold(a.Name, name) {
				continue
			}
			_, err := call(ctx, w.link.rpc, protocol.AgentSend, protocol.AgentSendParams{Agent: a.ID, Kind: protocol.KindCancel})
			return "Cancellation sent to @" + a.Name, nil, err
		}
		return "", nil, fmt.Errorf("no agent named @%s in this channel", name)
	default:
		return "", nil, errors.New("unknown command")
	}
}

type draft struct {
	prompt      string
	index, page int
	selected    []map[int]bool
	text        []string
}

func newDraft(p protocol.PromptInfo) *draft {
	d := &draft{prompt: p.ID, selected: make([]map[int]bool, len(p.Questions)), text: make([]string, len(p.Questions))}
	for i := range d.selected {
		d.selected[i] = map[int]bool{}
	}
	return d
}

func (w *worker) question(ctx context.Context, i *dg.Interaction, p protocol.PromptInfo, action, arg, value string) (string, []dg.MessageComponent, error) {
	if len(p.Questions) == 0 {
		return "", nil, errors.New("empty question batch")
	}
	// One shared draft backs the one public question card. Every authorized
	// operator sees the same selections; the worker serializes edits/submission.
	d := w.questionDraft(p)
	if action == "submit" {
		answers := make([]string, len(p.Questions))
		details := make([]protocol.QuestionAnswer, len(p.Questions))
		for q, question := range p.Questions {
			details[q].Custom = strings.TrimSpace(d.text[q])
			for n := range question.Options {
				if d.selected[q][n] {
					details[q].Selected = append(details[q].Selected, n)
				}
			}
			answers[q] = draftAnswer(question, d.selected[q], d.text[q])
			if answers[q] == "" {
				previous := d.index
				d.index, d.page = q, 0
				// Old cards offered Submit on every page. Treat that as Next
				// when the current answer is complete, never strand the human
				// on a page different from the one that needs an answer.
				if q > previous {
					text, components := questionView(p, d)
					return text, components, nil
				}
				return "", nil, missingAnswer(q)
			}
		}
		text, err := w.answer(ctx, i, p, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerAnswered, Answers: answers, Details: details})
		if err == nil {
			delete(w.drafts, p.ID)
		}
		return text, nil, err
	}
	if err := updateDraft(d, p, i, action, arg, value); err != nil {
		return "", nil, err
	}
	text, components := questionView(p, d)
	return text, components, nil
}

func draftAnswer(q protocol.Question, selected map[int]bool, custom string) string {
	var parts []string
	for n, option := range q.Options {
		if selected[n] {
			parts = append(parts, option.Label)
		}
	}
	if custom = strings.TrimSpace(custom); custom != "" {
		parts = append(parts, custom)
	}
	return strings.TrimSpace(strings.Join(parts, ", "))
}

func missingAnswer(index int) error {
	return fmt.Errorf("question %d still needs an answer: select options or enter a custom answer", index+1)
}

func updateDraft(d *draft, p protocol.PromptInfo, i *dg.Interaction, action, arg, value string) error {
	if action != "questions" {
		q, err := strconv.Atoi(strings.Split(arg, ".")[0])
		if err != nil || q != d.index {
			return errors.New("this question view changed; use the current form")
		}
	}
	switch action {
	case "questions":
	case "next":
		if draftAnswer(p.Questions[d.index], d.selected[d.index], d.text[d.index]) == "" {
			return missingAnswer(d.index)
		}
		d.index = min(len(p.Questions)-1, d.index+1)
		d.page = 0
	case "back":
		d.index = max(0, d.index-1)
		d.page = 0
	case "page-next":
		d.page = min(max(0, (len(p.Questions[d.index].Options)-1)/questionPageSize), d.page+1)
	case "page-back":
		d.page = max(0, d.page-1)
	case "text-submit":
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("enter a custom answer, or close the popup to leave it unchecked")
		}
		d.text[d.index] = value
	case "clear-text":
		d.text[d.index] = ""
	case "toggle":
		return toggleOption(d, p, arg)
	case "pick":
		return selectOptions(d, p, i, arg)
	default:
		return errors.New("invalid question action")
	}
	return nil
}

// Normal questions have at most four options; legacy larger lists keep paging.
const questionPageSize = 4

func toggleOption(d *draft, p protocol.PromptInfo, arg string) error {
	parts := strings.Split(arg, ".")
	if len(parts) != 2 {
		return errors.New("invalid option")
	}
	n, err := strconv.Atoi(parts[1])
	start, end := d.page*questionPageSize, min(len(p.Questions[d.index].Options), (d.page+1)*questionPageSize)
	if err != nil || n < start || n >= end {
		return errors.New("this option page changed; use the current buttons")
	}
	d.selected[d.index][n] = !d.selected[d.index][n]
	return nil
}

func selectOptions(d *draft, p protocol.PromptInfo, i *dg.Interaction, arg string) error {
	if i.Type != dg.InteractionMessageComponent {
		return errors.New("invalid selection")
	}
	if arg != fmt.Sprintf("%d.%d", d.index, d.page) {
		return errors.New("this option page is stale")
	}
	start, end := d.page*25, min(len(p.Questions[d.index].Options), (d.page+1)*25)
	for n := start; n < end; n++ {
		delete(d.selected[d.index], n)
	}
	for _, v := range i.MessageComponentData().Values {
		n, err := strconv.Atoi(v)
		if err != nil || n < start || n >= end {
			return errors.New("invalid option")
		}
		d.selected[d.index][n] = true
	}
	return nil
}

func questionView(p protocol.PromptInfo, d *draft) (string, []dg.MessageComponent) {
	if len(p.Questions) == 0 {
		return "No questions available.", nil
	}
	q := p.Questions[d.index]
	footer := ""
	if custom := strings.TrimSpace(d.text[d.index]); custom != "" {
		footer = "\nCustom answer: " + clip(custom, 1000) + footer
	}
	text := questionHeading(q.Question, 2000-units(footer)) + footer
	var components []dg.MessageComponent
	start, end := d.page*questionPageSize, min(len(q.Options), (d.page+1)*questionPageSize)
	for n := start; n < end; n++ {
		o := q.Options[n]
		label := o.Label
		if o.Description != "" {
			label += " — " + o.Description
		}
		mark, style := "⬜", dg.SecondaryButton
		if d.selected[d.index][n] {
			mark, style = "✅", dg.SuccessButton
		}
		components = append(components, row(dg.Button{CustomID: componentID(p.ID, "toggle", fmt.Sprintf("%d.%d", d.index, n)), Label: clip(label, 80), Emoji: &dg.ComponentEmoji{Name: mark}, Style: style}))
	}
	components = append(components, row(customAnswerButton(p.ID, d.index, d.text[d.index])))
	qb := func(action, label string) dg.MessageComponent {
		style := dg.SecondaryButton
		if action == "next" || action == "submit" {
			style = dg.PrimaryButton
		}
		return dg.Button{CustomID: componentID(p.ID, action, strconv.Itoa(d.index)), Label: label, Style: style}
	}
	var actions []dg.MessageComponent
	if d.index > 0 {
		actions = append(actions, qb("back", "Previous question"))
	}
	if d.index+1 < len(p.Questions) {
		actions = append(actions, qb("next", "Next question"))
	} else {
		label := "Submit answer"
		if len(p.Questions) > 1 {
			label = "Submit answers"
		}
		actions = append(actions, qb("submit", label))
	}
	if len(q.Options) > questionPageSize {
		actions = append(actions, qb("page-back", "Previous options"), qb("page-next", "More options"))
	}
	for _, action := range actions {
		components = append(components, row(action))
	}
	return clip(text, 2000), components
}

func customAnswerButton(id string, index int, value string) dg.Button {
	label, action, mark, style := "Custom answer", "text", "⬜", dg.SecondaryButton
	if value = strings.TrimSpace(value); value != "" {
		label, action, mark, style = strings.Join(strings.Fields(value), " "), "clear-text", "✅", dg.SuccessButton
	}
	return dg.Button{CustomID: componentID(id, action, strconv.Itoa(index)), Label: clip(label, 80), Emoji: &dg.ComponentEmoji{Name: mark}, Style: style}
}
