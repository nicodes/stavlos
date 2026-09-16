package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	dg "github.com/bwmarrin/discordgo"
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
		buttons = append(buttons, button(p.ID, "allow", "Allow once", disabled))
		if p.Dir != "" {
			text += "\nDirectory: " + p.Dir
			buttons = append(buttons, button(p.ID, "always", "Allow and add directory", disabled), button(p.ID, "dir", "Add another directory…", disabled))
		} else {
			buttons = append(buttons, button(p.ID, "always", "Allow in this channel", disabled))
			if p.Prefix != "" {
				buttons = append(buttons, button(p.ID, "prefix", "Allow "+p.Prefix+" in this channel", disabled))
			}
		}
		buttons = append(buttons, button(p.ID, "deny", "Deny…", disabled))
	case protocol.PromptQuestion:
		text += fmt.Sprintf("\n%d question(s) waiting for an answer.", len(p.Questions))
		for n, q := range p.Questions {
			text += fmt.Sprintf("\n\n%d. %s", n+1, q.Question)
			for j, o := range q.Options {
				text += fmt.Sprintf("\n  %d. %s — %s", j+1, o.Label, o.Description)
			}
		}
		buttons = []dg.MessageComponent{button(p.ID, "questions", "Answer questions", disabled)}
	case protocol.PromptTrust:
		var data struct {
			Dir   string   `json:"dir"`
			Files []string `json:"files"`
		}
		_ = json.Unmarshal(p.Input, &data)
		text += "\n" + data.Dir + "\n" + strings.Join(data.Files, "\n")
		buttons = []dg.MessageComponent{button(p.ID, "allow", "Trust this project's config", disabled), button(p.ID, "skip", "Not now", disabled)}
	}
	if p.Kind == protocol.PromptPermission && len(p.Input) > 0 {
		text += "\n" + string(p.Input)
	}
	if disabled {
		text += "\nClaimed by another client."
	}
	text += "\nPrompt: " + p.ID
	return text, []dg.MessageComponent{row(buttons...)}
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
	case "deny":
		label = "Reason (optional)"
	case "dir":
		label = "Directory to add"
	case "text":
		label = "Your answer (optional)"
	default:
		return nil
	}
	return &dg.InteractionResponse{Type: dg.InteractionResponseModal, Data: &dg.InteractionResponseData{
		Title: label, CustomID: componentID(id, action+"-submit", arg), Components: []dg.MessageComponent{
			row(dg.TextInput{CustomID: "value", Label: label, Style: dg.TextInputParagraph, Required: action == "dir", MaxLength: 4000}),
		},
	}}
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
		text, err := w.command(ctx, i)
		return text, nil, err
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
	case "deny-submit":
		r.Answer, r.Reason = protocol.AnswerDeny, value
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
	if err := w.finishPrompt(ctx, p.ID, text); err != nil {
		return "Decision accepted; could not update the public prompt.", err
	}
	return text, nil
}

func (w *worker) command(ctx context.Context, i *dg.Interaction) (string, error) {
	d := i.ApplicationCommandData()
	switch d.Name {
	case "status":
		return w.status(ctx)
	case "cancel":
		name := "main"
		for _, o := range d.Options {
			if o.Name == "agent" {
				name = strings.TrimPrefix(o.StringValue(), "@")
			}
		}
		r, err := call(ctx, w.link.rpc, protocol.AgentTree, protocol.AgentTreeParams{Channel: w.id})
		if err != nil {
			return "", err
		}
		for _, a := range r.Agents {
			if !strings.EqualFold(a.Name, name) {
				continue
			}
			_, err := call(ctx, w.link.rpc, protocol.AgentSend, protocol.AgentSendParams{Agent: a.ID, Kind: protocol.KindCancel})
			return "Cancellation sent to @" + a.Name, err
		}
		return "", fmt.Errorf("no agent named @%s in this channel", name)
	default:
		return "", errors.New("unknown command")
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
	key := p.ID + ":" + interactionUser(i)
	d := w.drafts[key]
	if d == nil {
		d = newDraft(p)
		w.drafts[key] = d
	}
	if action == "submit" {
		answers := make([]string, len(p.Questions))
		for q, question := range p.Questions {
			var parts []string
			for n, o := range question.Options {
				if d.selected[q][n] {
					parts = append(parts, o.Label)
				}
			}
			if d.text[q] != "" {
				parts = append(parts, d.text[q])
			}
			answers[q] = strings.Join(parts, ", ")
			if strings.TrimSpace(answers[q]) == "" {
				return "", nil, fmt.Errorf("question %d still needs an answer", q+1)
			}
		}
		text, err := w.answer(ctx, i, p, protocol.PromptReplyParams{ID: p.ID, Answer: protocol.AnswerAnswered, Answers: answers})
		if err == nil {
			delete(w.drafts, key)
		}
		return text, nil, err
	}
	if err := updateDraft(d, p, i, action, arg, value); err != nil {
		return "", nil, err
	}
	text, components := questionView(p, d)
	return text, components, nil
}

func updateDraft(d *draft, p protocol.PromptInfo, i *dg.Interaction, action, arg, value string) error {
	if action != "questions" {
		q, err := strconv.Atoi(strings.Split(arg, ".")[0])
		if err != nil || q != d.index {
			return errors.New("this question view is stale; open Answer questions again")
		}
	}
	switch action {
	case "questions":
	case "next":
		d.index = min(len(p.Questions)-1, d.index+1)
		d.page = 0
	case "back":
		d.index = max(0, d.index-1)
		d.page = 0
	case "page-next":
		d.page = min(max(0, (len(p.Questions[d.index].Options)-1)/25), d.page+1)
	case "page-back":
		d.page = max(0, d.page-1)
	case "text-submit":
		d.text[d.index] = value
	case "pick":
		return selectOptions(d, p, i, arg)
	default:
		return errors.New("invalid question action")
	}
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
	q := p.Questions[d.index]
	text := fmt.Sprintf("Question %d/%d: %s\nCustom answer: %s", d.index+1, len(p.Questions), q.Question, d.text[d.index])
	var components []dg.MessageComponent
	start, end := d.page*25, min(len(q.Options), (d.page+1)*25)
	var options []dg.SelectMenuOption
	for n := start; n < end; n++ {
		o := q.Options[n]
		options = append(options, dg.SelectMenuOption{Label: clip(fmt.Sprintf("%d. %s", n+1, o.Label), 100), Description: clip(o.Description, 100), Value: strconv.Itoa(n), Default: d.selected[d.index][n]})
	}
	if len(options) > 0 {
		zero := 0
		components = append(components, row(dg.SelectMenu{CustomID: componentID(p.ID, "pick", fmt.Sprintf("%d.%d", d.index, d.page)), Placeholder: "Select any options", MinValues: &zero, MaxValues: len(options), Options: options}))
	}
	qb := func(action, label string) dg.MessageComponent {
		return dg.Button{CustomID: componentID(p.ID, action, strconv.Itoa(d.index)), Label: label, Style: dg.SecondaryButton}
	}
	components = append(components, row(qb("back", "Previous question"), qb("next", "Next question"), qb("text", "Custom answer…"), qb("submit", "Submit batch")))
	if len(q.Options) > 25 {
		components = append(components, row(qb("page-back", "Previous options"), qb("page-next", "More options")))
	}
	return clip(text, 2000), components
}
