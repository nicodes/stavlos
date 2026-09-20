package tui

// tea.Cmds wrapping every client call. Update never touches the client
// directly (PRD §7.2 implementation note); it only returns these.

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/navigation"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/pkg/client"
)

const callTimeout = 15 * time.Second

// --- messages produced by commands / the notification goroutine ---

type (
	eventMsg        struct{ ev event.Event }
	streamMsg       struct{ n protocol.StreamNotification }
	promptMsg       struct{ n protocol.PromptNotification }
	disconnectedMsg struct{ err error }

	reconcileMsg struct {
		scope requestScope
		res   protocol.ReconcileResult
		err   error
	}
	subscribedMsg struct {
		err   error
		scope requestScope
	}
	treeMsg struct {
		channel string
		agents  []protocol.AgentInfo
		err     error
	}
	treeTickMsg    struct{}
	catalogTickMsg struct{}
	// resultMsg reports a fire-and-forget call; ok is shown on success.
	resultMsg struct {
		ok  string
		err error
	}
	promptReplyMsg struct {
		id  string
		err error
	}
	directoryMsg struct {
		scope requestScope
		info  protocol.ChannelInfo
		err   error
	}
	clearStatusMsg struct{ token int }
	// placeholderTickMsg advances the input placeholder suggestion.
	placeholderTickMsg struct{}

	// providersMsg carries provider.list. refresh=true only updates the
	// cached list; jump names a provider to sign in to at once.
	providersMsg struct {
		res     protocol.ProviderListResult
		err     error
		refresh bool
		jump    string

		status string // shown after the dialog refreshes (e.g. after a sign-out)
	}
	// loginStartMsg reports provider.login.start for provider.
	loginStartMsg struct {
		provider string
		res      protocol.LoginStartResult
		err      error
	}
	// loginDoneMsg reports provider.login.wait for login id.
	loginDoneMsg struct {
		id   string
		info protocol.ProviderInfo
		err  error
	}
	modelsMsg struct {
		scope  requestScope
		models []protocol.ModelInfo
		err    error
	}
)

// rpcCmd runs one client call under the RPC timeout and turns its outcome
// into a message.
func rpcCmd(ctx context.Context, do func(ctx context.Context) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		return do(ctx)
	}
}

// call is client.Do for a caller that wants only the error.
func call[P, R any](ctx context.Context, c *client.Client, m protocol.Method[P, R], p P) error {
	_, err := client.Do(ctx, c, m, p)
	return err
}

// resultCmd is a fire-and-forget call reported as a resultMsg; ok is shown
// on success.
func resultCmd(ctx context.Context, ok string, do func(ctx context.Context) error) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg { return resultMsg{ok, do(ctx)} })
}

// tick delivers msg after d.
func tick(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

type requestScope struct {
	channel    string
	generation uint64
}

func (m Model) requestScope() requestScope  { return requestScope{m.channelID, m.generation} }
func (m Model) accepts(s requestScope) bool { return s.channel == "" || s == m.requestScope() }

func rememberChannelCmd(channel string) tea.Cmd {
	selected := clock()
	return func() tea.Msg { return resultMsg{err: navigation.Remember(channel, selected)} }
}

func reconcileCmd(ctx context.Context, c *client.Client, scope requestScope) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.Reconcile, protocol.ChannelRef{Channel: scope.channel})
		return reconcileMsg{res: res, err: err, scope: scope}
	})
}

func subscribeCmd(ctx context.Context, c *client.Client, scope requestScope, from int64) tea.Cmd {
	// Subscription includes the entire history replay, not just an RPC round
	// trip. Its lifetime is the TUI's context, rather than the short call timeout.
	return func() tea.Msg {
		return subscribedMsg{err: call(ctx, c, protocol.Subscribe, protocol.SubscribeParams{Channel: scope.channel, From: from}), scope: scope}
	}
}

func treeCmd(ctx context.Context, c *client.Client, channel string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.AgentTree, protocol.AgentTreeParams{Channel: channel})
		return treeMsg{channel, res.Agents, err}
	})
}

// treeDebounceCmd fires a tick after which the tree is refetched.
func treeDebounceCmd() tea.Cmd { return tick(250*time.Millisecond, treeTickMsg{}) }

func sendCmd(ctx context.Context, c *client.Client, agent string, kind protocol.Kind, text, ok string) tea.Cmd {
	return resultCmd(ctx, ok, func(ctx context.Context) error {
		return call(ctx, c, protocol.AgentSend, protocol.AgentSendParams{Agent: agent, Kind: kind, Text: text})
	})
}

// postCmd sends the human's message to the channel chat; the daemon
// delivers it by @mention and refuses a mention that names no agent.
func postCmd(ctx context.Context, c *client.Client, channel, text string) tea.Cmd {
	return resultCmd(ctx, "", func(ctx context.Context) error {
		return call(ctx, c, protocol.ChannelPost, protocol.ChannelPostParams{Channel: channel, Text: text})
	})
}

// replyCmd claims prompt id and then replies to it. Claiming happens only
// here, once the human acted (PRD §7.4).
func replyCmd(ctx context.Context, c *client.Client, id string, reply func(ctx context.Context, c *client.Client) error) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		if err := call(ctx, c, protocol.PromptClaim, protocol.PromptClaimParams{ID: id}); err != nil {
			return promptReplyMsg{id, err}
		}
		return promptReplyMsg{id, reply(ctx, c)}
	})
}

// providersCmd lists providers into msg, whose fields say what the list
// is for (a sign-in to jump to, a quiet refresh, a status to show).
func providersCmd(ctx context.Context, c *client.Client, msg providersMsg) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		msg.res, msg.err = client.Do(ctx, c, protocol.ProviderList, protocol.None{})
		return msg
	})
}

// loginStartCmd begins the device-code login for provider.
func loginStartCmd(ctx context.Context, c *client.Client, provider, method string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.ProviderLoginStart, protocol.LoginStartParams{Provider: provider, Method: method})
		return loginStartMsg{provider: provider, res: res, err: err}
	})
}

// loginWaitCmd blocks until the login id completes. ctx is the model's
// cancellable login context (no call timeout: the user may take minutes);
// cancelling it abandons the wait.
func loginWaitCmd(ctx context.Context, c *client.Client, id string) tea.Cmd {
	return func() tea.Msg {
		info, err := client.Do(ctx, c, protocol.ProviderLoginWait, protocol.LoginWaitParams{ID: id})
		return loginDoneMsg{id: id, info: info, err: err}
	}
}

// openBrowserCmd asks the OS to open url. Best effort: errors are ignored
// and the command is abandoned after 3s.
func openBrowserCmd(url string) tea.Cmd {
	if url == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.CommandContext(ctx, "open", url)
		case "windows":
			cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
		default:
			cmd = exec.CommandContext(ctx, "xdg-open", url)
		}
		_ = cmd.Run()
		return nil
	}
}

// disconnectProviderCmd signs out of a provider and re-lists providers so
// the open dialog refreshes.
func disconnectProviderCmd(ctx context.Context, c *client.Client, id string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		if err := call(ctx, c, protocol.ProviderDisconnect, protocol.ProviderRef{Provider: id}); err != nil {
			return resultMsg{"", err}
		}
		res, err := client.Do(ctx, c, protocol.ProviderList, protocol.None{})
		return providersMsg{res: res, err: err, status: "signed out of " + id}
	})
}

// modelsCmd lists models of connected providers only.
func modelsCmd(ctx context.Context, c *client.Client, scope requestScope) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.ModelList, protocol.ModelListParams{})
		return modelsMsg{models: res.Models, err: err, scope: scope}
	})
}

// rolesMsg carries the channel's roles: for the /role picker, or (quiet)
// to refresh the cached roles that filter the models and variants dialogs
// and tint role names.
type rolesMsg struct {
	scope requestScope
	roles []protocol.PresetInfo
	err   error
	quiet bool
}

func rolesCmd(ctx context.Context, c *client.Client, scope requestScope, quiet bool) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.Presets, protocol.PresetsParams{Channel: scope.channel})
		return rolesMsg{roles: res.Presets, err: err, quiet: quiet, scope: scope}
	})
}

func pickRoleCmd(ctx context.Context, c *client.Client, agent, role string) tea.Cmd {
	return resultCmd(ctx, "role set to "+role, func(ctx context.Context) error {
		return call(ctx, c, protocol.AgentSetRole, protocol.AgentSetRoleParams{Agent: agent, Role: role})
	})
}

func addDirCmd(ctx context.Context, c *client.Client, channel, dir string) tea.Cmd {
	return resultCmd(ctx, "added "+dir, func(ctx context.Context) error {
		return call(ctx, c, protocol.ChannelAddDir, protocol.ChannelDirParams{Channel: channel, Dir: dir})
	})
}

func removeDirCmd(ctx context.Context, c *client.Client, channel, dir string) tea.Cmd {
	return resultCmd(ctx, "removed "+format.ShortHome(dir), func(ctx context.Context) error {
		return call(ctx, c, protocol.ChannelRemoveDir, protocol.ChannelDirParams{Channel: channel, Dir: dir})
	})
}

func setDirCmd(ctx context.Context, c *client.Client, scope requestScope, dir string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		info, err := client.Do(ctx, c, protocol.ChannelSetDir, protocol.ChannelDirParams{Channel: scope.channel, Dir: dir})
		return directoryMsg{scope: scope, info: info, err: err}
	})
}

// replaceDirCmd swaps one directory for another (an edit in the dirs
// dialog): the new one is added first so no agent loses ground.
func replaceDirCmd(ctx context.Context, c *client.Client, channel, oldDir, newDir string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		if err := call(ctx, c, protocol.ChannelAddDir, protocol.ChannelDirParams{Channel: channel, Dir: newDir}); err != nil {
			return resultMsg{"", err}
		}
		return resultMsg{"replaced " + format.ShortHome(oldDir) + " with " + newDir, call(ctx, c, protocol.ChannelRemoveDir, protocol.ChannelDirParams{Channel: channel, Dir: oldDir})}
	})
}

// renameChannelCmd is /rename: the daemon normalises the name and refuses one
// another channel has.
func renameChannelCmd(ctx context.Context, c *client.Client, channel, name string) tea.Cmd {
	return resultCmd(ctx, "channel renamed", func(ctx context.Context) error {
		return call(ctx, c, protocol.ChannelRename, protocol.ChannelRenameParams{Channel: channel, Name: name})
	})
}

// setRecapCmd is /recap: minutes of silence before the main agent is asked
// for a status report, 0 to turn it off.
func setRecapCmd(ctx context.Context, c *client.Client, channel string, minutes int) tea.Cmd {
	ok := fmt.Sprintf("recap every %d min of quiet", minutes)
	if minutes == 0 {
		ok = "recap off"
	}
	return resultCmd(ctx, ok, func(ctx context.Context) error {
		return call(ctx, c, protocol.ChannelSetRecap, protocol.ChannelSetRecapParams{Channel: channel, Minutes: minutes})
	})
}

func setModeCmd(ctx context.Context, c *client.Client, channel, mode string) tea.Cmd {
	return resultCmd(ctx, "mode "+mode+": "+protocol.ModeSummary(mode), func(ctx context.Context) error {
		return call(ctx, c, protocol.ChannelSetMode, protocol.ChannelSetModeParams{Channel: channel, Mode: mode})
	})
}

// compactCmd is /compact. Summarising takes a model call, so it gets a
// longer timeout than the usual RPC.
func compactCmd(ctx context.Context, c *client.Client, agent string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		res, err := client.Do(ctx, c, protocol.AgentCompact, protocol.AgentCompactParams{Agent: agent})
		if res.Status == "queued" {
			return resultMsg{"compaction queued: the agent is mid-turn and compacts before its next model call", err}
		}
		return resultMsg{"", err} // the Compacted event reports the result
	}
}

// copyCmd puts text on the clipboard: an OSC 52 sequence to the terminal
// (what the TUI can always reach), plus wl-copy, xclip or pbcopy when one
// is installed, so terminals that ignore OSC 52 still get it.
func copyCmd(text string) tea.Cmd {
	return func() tea.Msg {
		seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
		if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
			_, _ = tty.WriteString(seq)
			_ = tty.Close()
		} else {
			_, _ = os.Stdout.WriteString(seq)
		}
		for _, tool := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"pbcopy"}} {
			if _, err := exec.LookPath(tool[0]); err != nil {
				continue
			}
			cmd := exec.Command(tool[0], tool[1:]...)
			cmd.Stdin = strings.NewReader(text)
			if cmd.Run() == nil {
				break
			}
		}
		return nil
	}
}

// channelsPurpose is what a channel.list result is for.
type channelsPurpose int

const (
	channelsPicker channelsPurpose = iota // the /channels picker
	channelsNav                           // the sidebar's channels section
)

// channelsMsg carries channel.list for one purpose.
type channelsMsg struct {
	scope    requestScope
	channels []protocol.ChannelInfo
	err      error
	purpose  channelsPurpose
}

func channelsCmd(ctx context.Context, c *client.Client, scope requestScope, purpose channelsPurpose) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.ChannelList, protocol.ChannelListParams{})
		return channelsMsg{channels: res.Channels, err: err, purpose: purpose, scope: scope}
	})
}

// resumable is the sidebar's channels section: all other active
// channels, in alphabetical order.
func resumable(ss []protocol.ChannelInfo, current string) []protocol.ChannelInfo {
	var out []protocol.ChannelInfo
	for _, s := range ss {
		if s.ID != current {
			out = append(out, s)
		}
	}
	sortChannels(out)
	return out
}

// sortChannels puts channels in alphabetical order by name (then id), the
// order every channel list keeps: nothing moves when a channel is created,
// used or switched to.
func sortChannels(ss []protocol.ChannelInfo) {
	slices.SortStableFunc(ss, compareChannels)
}

func compareChannels(a, b protocol.ChannelInfo) int {
	return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
}

// switchedMsg reports a channel resume for the /channels picker: the TUI
// rebinds to info.ID and reconciles from scratch.
type switchedMsg struct {
	info protocol.ChannelInfo
	err  error
}

// newChannelCmd is + channel: a channel named name in dir, bound like a
// resumed one. The current channel is left only once the new one exists, so
// a refused name keeps the TUI where it was.
func newChannelCmd(ctx context.Context, c *client.Client, from, dir, name string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		info, err := client.Do(ctx, c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: dir, Name: name})
		if err == nil {
			_ = call(ctx, c, protocol.Unsubscribe, protocol.SubscribeParams{Channel: from})
		}
		return switchedMsg{info, err}
	})
}

func createChannelCmd(ctx context.Context, c *client.Client, from, base, dir, name string) tea.Cmd {
	return func() tea.Msg {
		resolved, err := config.WorkingDirectory(base, dir)
		if err != nil {
			return switchedMsg{err: err}
		}
		return newChannelCmd(ctx, c, from, resolved, name)()
	}
}

func switchChannelCmd(ctx context.Context, c *client.Client, from, to string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		info, err := client.Do(ctx, c, protocol.ChannelResume, protocol.ChannelRef{Channel: to})
		if err == nil {
			_ = call(ctx, c, protocol.Unsubscribe, protocol.SubscribeParams{Channel: from})
		}
		return switchedMsg{info, err}
	})
}

// variantsMsg carries the variant names a model offers, for the /variants
// picker.
type variantsMsg struct {
	scope    requestScope
	model    string
	current  string
	variants []string
	err      error
}

func variantsCmd(ctx context.Context, c *client.Client, scope requestScope, modelID, current string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.Variants, protocol.VariantsParams{Model: modelID})
		return variantsMsg{model: modelID, current: current, variants: res.Variants, err: err, scope: scope}
	})
}

func pickVariantCmd(ctx context.Context, c *client.Client, agent, variant string) tea.Cmd {
	what := "variant set to " + variant
	if variant == "" {
		what = "variant reset to the provider default"
	}
	return resultCmd(ctx, what, func(ctx context.Context) error {
		return call(ctx, c, protocol.AgentSetVariant, protocol.AgentSetVariantParams{Agent: agent, Variant: variant})
	})
}

// pickAgentModelCmd / pickChannelModelCmd are the /models overlay actions.
func pickAgentModelCmd(ctx context.Context, c *client.Client, agent, modelID string) tea.Cmd {
	return resultCmd(ctx, "model set to "+modelID, func(ctx context.Context) error {
		return call(ctx, c, protocol.AgentSetModel, protocol.AgentSetModelParams{Agent: agent, Model: modelID})
	})
}

func pickChannelModelCmd(ctx context.Context, c *client.Client, channel, modelID string) tea.Cmd {
	return resultCmd(ctx, "channel model set to "+modelID, func(ctx context.Context) error {
		return call(ctx, c, protocol.ChannelSetModel, protocol.ChannelSetModelParams{Channel: channel, Model: modelID})
	})
}

// compactTickMsg animates the compaction bar while a summariser runs.
type compactTickMsg struct{}

func compactTickCmd() tea.Cmd     { return tick(render.CompactTick, compactTickMsg{}) }
func placeholderTickCmd() tea.Cmd { return tick(placeholderPeriod, placeholderTickMsg{}) }

func clearStatusCmd(token int, after time.Duration) tea.Cmd {
	return tick(after, clearStatusMsg{token})
}

// isConflict reports whether err is the daemon's conflict error (prompt
// already claimed, late answer).
func isConflict(err error) bool {
	var pe *protocol.Error
	return errors.As(err, &pe) && pe.Code == protocol.ErrConflict
}

// forwardNotifications reads the client's notification channel and pushes
// typed messages into the program until ctx ends or the connection drops.
func forwardNotifications(ctx context.Context, c *client.Client, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Closed():
			p.Send(disconnectedMsg{errors.New("daemon disconnected")})
			return
		case r := <-c.Notifications:
			p.Send(notificationBatch(r, c.Notifications))
		}
	}
}

// A bounded batch preserves wire order while doing layout/render work once
// per batch, rather than once per historical event. Never wait to fill a batch.
type notificationBatchMsg []tea.Msg

func notificationBatch(first protocol.Response, pending <-chan protocol.Response) notificationBatchMsg {
	batch := make(notificationBatchMsg, 0, 128)
	r := first
	for i := 0; i < 128; i++ {
		v, err := client.DecodeNotification(r)
		if err == nil {
			switch n := v.(type) {
			case protocol.EventNotification:
				batch = append(batch, eventMsg{n.Event})
			case protocol.StreamNotification:
				batch = append(batch, streamMsg{n})
			case protocol.PromptNotification:
				batch = append(batch, promptMsg{n})
			case protocol.ChangedNotification:
				batch = append(batch, changedMsg{n.What})
			}
		}
		if i == 127 {
			break
		}
		select {
		case next, ok := <-pending:
			if !ok {
				return batch
			}
			r = next
		default:
			return batch
		}
	}
	return batch
}
