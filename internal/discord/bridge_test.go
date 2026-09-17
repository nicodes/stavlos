package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

type sentMessage struct {
	ID, Channel, User, Text string
	Components              []dg.MessageComponent
	Guild, UserID, Avatar   string
	Webhook                 string
}
type fakeAPI struct {
	mu        sync.Mutex
	channels  []*dg.Channel
	messages  map[string]sentMessage
	sends     []sentMessage
	responses []*dg.InteractionResponse
	replies   []string
	next      int
	target    *dg.Message
}

func newAPI() *fakeAPI { return &fakeAPI{messages: map[string]sentMessage{}} }
func (f *fakeAPI) Channels(context.Context, string) ([]*dg.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*dg.Channel(nil), f.channels...), nil
}
func (f *fakeAPI) Create(_ context.Context, guild string, d dg.GuildChannelCreateData) (*dg.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	c := &dg.Channel{ID: fmt.Sprint(f.next), GuildID: guild, Name: d.Name, Type: d.Type, ParentID: d.ParentID, Topic: d.Topic}
	f.channels = append(f.channels, c)
	return c, nil
}
func (f *fakeAPI) Rename(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n, c := range f.channels {
		if c.ID == id {
			cc := *c
			cc.Name = name
			f.channels[n] = &cc
		}
	}
	return nil
}
func (f *fakeAPI) Send(_ context.Context, channel, user, text string, components []dg.MessageComponent) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	m := sentMessage{ID: fmt.Sprint(f.next), Channel: channel, User: user, Text: text, Components: components}
	f.messages[m.ID] = m
	f.sends = append(f.sends, m)
	return m.ID, nil
}

func (f *fakeAPI) SendUser(_ context.Context, channel, guild, user, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	m := sentMessage{ID: fmt.Sprint(f.next), Channel: channel, User: "Operator", Text: text,
		Guild: guild, UserID: user, Avatar: "https://example.com/avatar.png"}
	f.messages[m.ID] = m
	f.sends = append(f.sends, m)
	return m.ID, nil
}
func (f *fakeAPI) Edit(_ context.Context, channel, id, text string, components []dg.MessageComponent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.messages[id]
	if m.Webhook != "" {
		return errors.New("webhook message edited through the bot endpoint")
	}
	m.ID, m.Channel, m.Text, m.Components = id, channel, text, components
	f.messages[id] = m
	return nil
}

func (f *fakeAPI) SendQuestion(_ context.Context, channel, agent, text string, components []dg.MessageComponent) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	m := sentMessage{ID: fmt.Sprint(f.next), Channel: channel, User: agent, Text: text, Components: components, Webhook: "agent-hook"}
	f.messages[m.ID] = m
	f.sends = append(f.sends, m)
	return m.ID, m.Webhook, nil
}

func (f *fakeAPI) EditWebhook(_ context.Context, webhook, id, text string, components []dg.MessageComponent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[id]
	if !ok || m.Webhook != webhook {
		return errors.New("wrong webhook for question message")
	}
	m.Text, m.Components = text, components
	f.messages[id] = m
	return nil
}
func (f *fakeAPI) Message(context.Context, string, string) (*dg.Message, error) { return f.target, nil }
func (f *fakeAPI) OwnWebhook(_ context.Context, _, id string) bool              { return id == "owned" }
func (f *fakeAPI) Typing(context.Context, string) error                         { return nil }
func (f *fakeAPI) Respond(_ context.Context, _ *dg.Interaction, r *dg.InteractionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, r)
	return nil
}
func (f *fakeAPI) ResponseEdit(_ context.Context, _ *dg.Interaction, text string, _ []dg.MessageComponent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, text)
	return nil
}
func (f *fakeAPI) snapshot() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentMessage
	for _, m := range f.messages {
		out = append(out, m)
	}
	return out
}

type fakeRPC func(context.Context, string, any, any) error

func (f fakeRPC) Call(ctx context.Context, m string, p, r any) error { return f(ctx, m, p, r) }
func result(dst, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func fixture(t *testing.T, rpc fakeRPC) (*Bridge, *worker, *fakeAPI) {
	t.Helper()
	api := newAPI()
	b, err := New(config.Discord{Guild: "guild", Category: "stavlos", Approvers: []string{"operator"}, Dirs: []string{t.TempDir()}}, api, "bot", filepath.Join(t.TempDir(), "prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	l := &link{rpc: rpc, id: "client", ctx: ctx, cancel: cancel}
	w := &worker{b: b, id: "channel", discord: "discord", ctx: ctx, cancel: cancel, queue: make(chan work, 256), ready: l, link: l,
		prompts: map[string]protocol.PromptInfo{}, drafts: map[string]*draft{}, shown: map[string]string{}}
	b.live, b.workers[w.id], b.byDiscord[w.discord] = l, w, w
	return b, w, api
}
func interaction(id, action string) *dg.Interaction {
	return &dg.Interaction{ID: "i", GuildID: "guild", ChannelID: "discord", Member: &dg.Member{User: &dg.User{ID: "operator"}}, Type: dg.InteractionMessageComponent,
		Data: dg.MessageComponentInteractionData{CustomID: componentID(id, action, "")}}
}

func TestPromptFilteringAndRestart(t *testing.T) {
	pending := []protocol.PromptInfo{
		{ID: "visible", Channel: "channel", Kind: protocol.PromptPermission, Question: "run?", Escalated: true},
		{ID: "early", Channel: "channel", Kind: protocol.PromptPermission},
		{ID: "other", Channel: "other", Kind: protocol.PromptPermission, Escalated: true},
	}
	b, w, api := fixture(t, func(_ context.Context, method string, p, r any) error {
		return result(r, protocol.PromptListResult{Prompts: pending})
	})
	ctx := context.Background()
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.snapshot()) != 1 {
		t.Fatalf("leaked un-escalated or other-channel prompt: %+v", api.snapshot())
	}
	stored, err := openStore(b.store.path)
	if err != nil {
		t.Fatal(err)
	}
	b.store = stored
	w.shown = map[string]string{}
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.snapshot()) != 1 {
		t.Fatal("restart duplicated prompt")
	}
	pending = nil
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range api.snapshot() {
		if len(m.Components) != 0 || strings.Contains(m.Text, "Allowed") {
			t.Fatalf("stale or invented answer: %+v", m)
		}
	}
	if len(b.store.snapshot()) != 1 {
		t.Fatal("permission destination lost before its result arrived")
	}
	if err := w.mirror(ctx, event.Event{Seq: 1, Channel: "channel", Type: event.AskResolved, Payload: event.MustPayload(event.AskResolvedPayload{ID: "visible", Outcome: event.AskAnswered, Answer: protocol.AnswerDeny})}); err != nil {
		t.Fatal(err)
	}
	if len(b.store.snapshot()) != 0 {
		t.Fatal("recorded permission result retained in index")
	}
}

func TestOperatorAndLoopFiltering(t *testing.T) {
	b, w, api := fixture(t, nil)
	for _, m := range []*dg.Message{
		{GuildID: "guild", ChannelID: "discord", Author: &dg.User{ID: "stranger"}, Content: "go"},
		{GuildID: "other", ChannelID: "discord", Author: &dg.User{ID: "operator"}, Content: "go"},
		{GuildID: "guild", ChannelID: "discord", Author: &dg.User{ID: "operator", Bot: true}, Content: "go"},
		{GuildID: "guild", ChannelID: "discord", Author: &dg.User{ID: "operator"}, WebhookID: "hook", Content: "go"},
	} {
		b.Message(&dg.MessageCreate{Message: m})
	}
	if len(w.queue) != 0 {
		t.Fatal("unauthorized or bot message queued")
	}
	i := interaction("p", "allow")
	i.Member.User.ID = "stranger"
	b.Interaction(&dg.InteractionCreate{Interaction: i})
	if len(w.queue) != 0 || len(api.responses) != 1 || api.responses[0].Data.Flags != dg.MessageFlagsEphemeral {
		t.Fatal("unauthorized click was not refused")
	}
	i = interaction("p", "deny")
	b.Interaction(&dg.InteractionCreate{Interaction: i})
	if api.responses[1].Type != dg.InteractionResponseModal || len(w.queue) != 0 {
		t.Fatal("modal opening deferred or claimed")
	}
	b.Message(&dg.MessageCreate{Message: &dg.Message{GuildID: "guild", ChannelID: "discord", Author: &dg.User{ID: "operator"}, Content: "go"}})
	if len(w.queue) != 1 {
		t.Fatal("operator message was not queued")
	}
}

func TestReplyAddressingAndSourceSuppression(t *testing.T) {
	var posts []string
	_, w, api := fixture(t, func(_ context.Context, method string, p, r any) error {
		if method == protocol.MChannelPost {
			posts = append(posts, p.(protocol.ChannelPostParams).Text)
			return result(r, protocol.ChannelPostResult{})
		}
		return nil
	})
	api.target = &dg.Message{WebhookID: "owned", Author: &dg.User{Username: "scout"}}
	ctx := context.Background()
	for _, text := range []string{"<@bot> inspect", "@coder inspect", "<@123> inspect"} {
		if err := w.post(ctx, &dg.Message{Content: text, MessageReference: &dg.MessageReference{MessageID: "reply", ChannelID: w.discord}}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(posts, ";") != "@scout inspect;@coder inspect;@scout <@123> inspect" {
		t.Fatalf("%v", posts)
	}
	for seq, from := range []string{"human:discord", "human:tui:1"} {
		e := event.Event{Seq: int64(seq + 1), Type: event.ChatPosted, Payload: event.MustPayload(event.ChatPayload{From: from, To: []string{"scout", "coder"}, Text: "hello"})}
		if err := w.mirror(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.snapshot()) != 1 {
		t.Fatal("echoed own post or missed terminal post")
	}
	m := api.snapshot()[0]
	if m.UserID != "operator" || m.Guild != "guild" || m.Text != "@scout @coder hello" {
		t.Fatalf("terminal post did not use the operator identity: %+v", m)
	}
}

func TestReplyToMirroredHumanDoesNotAddressAgent(t *testing.T) {
	var posted string
	_, w, api := fixture(t, func(_ context.Context, _ string, p, r any) error {
		posted = p.(protocol.ChannelPostParams).Text
		return result(r, protocol.ChannelPostResult{})
	})
	api.target = &dg.Message{WebhookID: "terminal", Author: &dg.User{Username: "scout"}}
	err := w.post(context.Background(), &dg.Message{Content: "follow up", MessageReference: &dg.MessageReference{MessageID: "human-post", ChannelID: w.discord}})
	if err != nil {
		t.Fatal(err)
	}
	if posted != "follow up" {
		t.Fatalf("human display name became an agent address: %q", posted)
	}
}

func TestTerminalIdentityOnLongMessagesAndMultipleOperators(t *testing.T) {
	b, w, api := fixture(t, nil)
	text := strings.Repeat("hello 😀\n", 700)
	if err := w.terminalPost(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	parts := append([]sentMessage(nil), api.sends...)
	api.mu.Unlock()
	var joined strings.Builder
	if len(parts) < 2 {
		t.Fatal("long post was not split")
	}
	for _, m := range parts {
		if m.UserID != "operator" || m.Avatar == "" || units(m.Text) > 2000 {
			t.Fatalf("incorrect chunk: %+v", m)
		}
		joined.WriteString(m.Text)
	}
	if joined.String() != text {
		t.Fatal("terminal text changed")
	}
	b.cfg.Approvers = []string{"one", "two"}
	if err := w.terminalPost(context.Background(), "ambiguous author"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	last := api.sends[len(api.sends)-1]
	api.mu.Unlock()
	if last.UserID != "" || last.User != "" || !strings.Contains(last.Text, "You (terminal)") {
		t.Fatal("guessed a human identity with multiple operators")
	}
}

func TestPermissionValidationAndLateModal(t *testing.T) {
	p := protocol.PromptInfo{ID: "p", Channel: "channel", Kind: protocol.PromptPermission, Dir: "/outside", Prefix: "go test", Escalated: true}
	if _, err := permissionAnswer(p, "prefix", ""); err == nil {
		t.Fatal("boundary got prefix approval")
	}
	r, err := permissionAnswer(p, "dir-submit", "/another")
	if err != nil || r.Dir != "/another" || r.Answer != protocol.AnswerAllowAlways {
		t.Fatalf("%+v %v", r, err)
	}
	called := 0
	_, w, _ := fixture(t, func(_ context.Context, method string, p, r any) error {
		called++
		return result(r, protocol.PromptListResult{})
	})
	i := interaction("p", "deny")
	i.Type = dg.InteractionModalSubmit
	i.Data = dg.ModalSubmitInteractionData{CustomID: componentID("p", "deny-submit", ""), Components: []dg.MessageComponent{&dg.ActionsRow{Components: []dg.MessageComponent{&dg.TextInput{CustomID: "value", Value: "no"}}}}}
	text, _, err := w.interact(context.Background(), i)
	if err != nil || !strings.Contains(text, "resolved") || called != 1 {
		t.Fatalf("late modal called reply: %q %v %d", text, err, called)
	}
}

func TestQuestionsCombineSelectionsAndText(t *testing.T) {
	p := protocol.PromptInfo{ID: "q", Channel: "channel", Kind: protocol.PromptQuestion, Escalated: true, Questions: []protocol.Question{{Question: "Choose", Options: []protocol.QuestionOption{{Label: "One"}, {Label: "Two"}}}, {Question: "Name"}}}
	var reply protocol.PromptReplyParams
	var methods []string
	_, w, _ := fixture(t, func(_ context.Context, method string, arg, r any) error {
		methods = append(methods, method)
		switch method {
		case protocol.MPromptList:
			return result(r, protocol.PromptListResult{Prompts: []protocol.PromptInfo{p}})
		case protocol.MPromptReply:
			reply = arg.(protocol.PromptReplyParams)
		}
		return result(r, protocol.None{})
	})
	d := newDraft(p)
	d.selected[0][0], d.selected[0][1] = true, true
	d.text[0], d.text[1] = "Three", "Stavlos"
	w.drafts["q"] = d
	_, _, err := w.interact(context.Background(), interaction("q", "submit"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reply.Answers, ";") != "One, Two, Three;Stavlos" {
		t.Fatalf("%+v", reply)
	}
	if strings.Join(methods, ";") != "prompt.list;prompt.claim;prompt.reply" {
		t.Fatal(methods)
	}
}

func TestLongRepliesAndFences(t *testing.T) {
	text := "```go\n" + strings.Repeat("😀 code\n", 900) + "```\nend"
	parts := splitText(text)
	if len(parts) < 2 {
		t.Fatal("did not split")
	}
	for _, p := range parts {
		if units(p) > 2000 {
			t.Fatal("Discord length limit exceeded")
		}
		if strings.Count(p, "```")%2 != 0 {
			t.Fatalf("unbalanced fence: %q", p)
		}
	}
	plain := strings.Repeat("😀", 3000)
	if strings.Join(splitText(plain), "") != plain {
		t.Fatal("split lost or duplicated Unicode")
	}
}

func TestMappingAndDirectoryBoundary(t *testing.T) {
	b, _, _ := fixture(t, nil)
	if b.allowed(filepath.Join(b.cfg.Dirs[0], "nested")) || !b.allowed(b.cfg.Dirs[0]) {
		t.Fatal("directory coverage is not exact")
	}
	channels := []*dg.Channel{{ID: "one", Type: dg.ChannelTypeGuildText, ParentID: "cat", Name: "renamed", Topic: "stavlos-channel:stable"}}
	m, err := channelMarkers(channels, "cat")
	if err != nil || m["stable"].ID != "one" {
		t.Fatalf("%v %v", m, err)
	}
	channels = append(channels, &dg.Channel{ID: "two", Type: dg.ChannelTypeGuildText, ParentID: "cat", Topic: "stavlos-channel:stable"})
	if _, err := channelMarkers(channels, "cat"); err == nil {
		t.Fatal("ambiguous marker accepted")
	}
}

func TestQueueOverflowCancelsConnection(t *testing.T) {
	_, w, _ := fixture(t, nil)
	for i := 0; i < cap(w.queue); i++ {
		if !w.enqueue(work{link: w.link}) {
			t.Fatal("queue filled early")
		}
	}
	if w.enqueue(work{link: w.link}) {
		t.Fatal("overflow succeeded")
	}
	select {
	case <-w.link.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("overflow did not reconnect")
	}
}

func TestCommandsStayWithinChannel(t *testing.T) {
	var sent protocol.AgentSendParams
	_, w, _ := fixture(t, func(_ context.Context, method string, p, r any) error {
		switch method {
		case protocol.MAgentTree:
			return result(r, protocol.AgentTreeResult{Agents: []protocol.AgentInfo{{ID: "agent", Name: "main", State: protocol.AgentRunning}}})
		case protocol.MAgentSend:
			sent = p.(protocol.AgentSendParams)
		}
		return result(r, protocol.None{})
	})
	i := interaction("", "")
	i.Type = dg.InteractionApplicationCommand
	i.Data = dg.ApplicationCommandInteractionData{Name: "status"}
	text, _, err := w.command(context.Background(), i)
	if err != nil || !strings.Contains(text, "@main") {
		t.Fatalf("%q %v", text, err)
	}
	i.Data = dg.ApplicationCommandInteractionData{Name: "cancel"}
	if _, _, err := w.command(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	if sent.Agent != "agent" || sent.Kind != protocol.KindCancel {
		t.Fatalf("%+v", sent)
	}
	sent = protocol.AgentSendParams{}
	i.Data = dg.ApplicationCommandInteractionData{Name: "cancel", Options: []*dg.ApplicationCommandInteractionDataOption{{Name: "agent", Type: dg.ApplicationCommandOptionString, Value: "some-other-channel-id"}}}
	if _, _, err := w.command(context.Background(), i); err == nil || sent.Agent != "" {
		t.Fatal("cancel escaped the mapped channel")
	}
}

func TestQuestionPaginationKeepsSelections(t *testing.T) {
	p := protocol.PromptInfo{ID: "q", Questions: []protocol.Question{{Question: "Choose"}}}
	for i := 0; i < 30; i++ {
		p.Questions[0].Options = append(p.Questions[0].Options, protocol.QuestionOption{Label: fmt.Sprint(i)})
	}
	d := newDraft(p)
	i := interaction("q", "toggle")
	if err := updateDraft(d, p, i, "toggle", "0.3", ""); err != nil {
		t.Fatal(err)
	}
	for d.page < 6 {
		if err := updateDraft(d, p, i, "page-next", "0", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := updateDraft(d, p, i, "toggle", "0.27", ""); err != nil {
		t.Fatal(err)
	}
	if !d.selected[0][3] || !d.selected[0][27] {
		t.Fatal("changing pages lost earlier selections")
	}
	if err := updateDraft(d, p, i, "toggle", "0.3", ""); err == nil {
		t.Fatal("stale view accepted")
	}
	_, components := questionView(p, d)
	if 1+2*len(components) > 40 {
		t.Fatal("too many Discord V2 components")
	}
	for _, c := range components {
		if len(c.(dg.ActionsRow).Components) != 1 {
			t.Fatal("buttons must be vertically stacked")
		}
	}
	if err := updateDraft(d, p, i, "page-next", "0", ""); err != nil {
		t.Fatal(err)
	}
	_, components = questionView(p, d)
	if len(components) != 6 || !hasQuestionAction(components, "text") {
		t.Fatal("last page should stack two options, custom answer, Submit and two navigation buttons")
	}
}

func TestDuplicateGatewayPostIsNotSentTwice(t *testing.T) {
	calls := 0
	_, w, _ := fixture(t, func(_ context.Context, _ string, _, r any) error {
		calls++
		return result(r, protocol.ChannelPostResult{})
	})
	m := &dg.Message{ID: "same-message", Content: "task"}
	for range 2 {
		if err := w.post(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("sent %d times", calls)
	}
}
