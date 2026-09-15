package tui

// tea.Cmds wrapping every client call. Update never touches the client
// directly (PRD §7.2 implementation note); it only returns these.

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/event"
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
		res protocol.ReconcileResult
		err error
	}
	subscribedMsg struct{ err error }
	treeMsg       struct {
		agents []protocol.AgentInfo
		err    error
	}
	treeTickMsg struct{}
	// resultMsg reports a fire-and-forget call; ok is shown on success.
	resultMsg struct {
		ok  string
		err error
	}
	promptReplyMsg struct {
		id  string
		err error
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

// resultCmd is a fire-and-forget call reported as a resultMsg; ok is shown
// on success.
func resultCmd(ctx context.Context, ok string, do func(ctx context.Context) error) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg { return resultMsg{ok, do(ctx)} })
}

// tick delivers msg after d.
func tick(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

func reconcileCmd(ctx context.Context, c *client.Client, channel string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := c.Reconcile(ctx, channel)
		return reconcileMsg{res, err}
	})
}

func subscribeCmd(ctx context.Context, c *client.Client, channel string, from int64) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg { return subscribedMsg{c.Subscribe(ctx, channel, from)} })
}

func treeCmd(ctx context.Context, c *client.Client, channel string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		agents, err := c.Tree(ctx, channel)
		return treeMsg{agents, err}
	})
}

// treeDebounceCmd fires a tick after which the tree is refetched.
func treeDebounceCmd() tea.Cmd { return tick(250*time.Millisecond, treeTickMsg{}) }

func sendCmd(ctx context.Context, c *client.Client, agent string, kind protocol.Kind, text, ok string) tea.Cmd {
	return resultCmd(ctx, ok, func(ctx context.Context) error { return c.Send(ctx, agent, kind, text) })
}

// postCmd sends the human's message to the channel chat; the daemon
// delivers it by @mention and refuses a mention that names no agent.
func postCmd(ctx context.Context, c *client.Client, channel, text string) tea.Cmd {
	return resultCmd(ctx, "", func(ctx context.Context) error {
		_, err := c.Post(ctx, channel, text)
		return err
	})
}

// replyCmd claims prompt id and then replies to it. Claiming happens only
// here, once the human acted (PRD §7.4).
func replyCmd(ctx context.Context, c *client.Client, id string, reply func(ctx context.Context, c *client.Client) error) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		if err := c.ClaimPrompt(ctx, id); err != nil {
			return promptReplyMsg{id, err}
		}
		return promptReplyMsg{id, reply(ctx, c)}
	})
}

// providersCmd lists providers into msg, whose fields say what the list
// is for (a sign-in to jump to, a quiet refresh, a status to show).
func providersCmd(ctx context.Context, c *client.Client, msg providersMsg) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		msg.res, msg.err = c.Providers(ctx)
		return msg
	})
}

// loginStartCmd begins the device-code login for provider.
func loginStartCmd(ctx context.Context, c *client.Client, provider, method string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := c.LoginStart(ctx, provider, method)
		return loginStartMsg{provider: provider, res: res, err: err}
	})
}

// loginWaitCmd blocks until the login id completes. ctx is the model's
// cancellable login context (no call timeout: the user may take minutes);
// cancelling it abandons the wait.
func loginWaitCmd(ctx context.Context, c *client.Client, id string) tea.Cmd {
	return func() tea.Msg {
		info, err := c.LoginWait(ctx, id)
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
		if err := c.DisconnectProvider(ctx, id); err != nil {
			return resultMsg{"", err}
		}
		res, err := c.Providers(ctx)
		return providersMsg{res: res, err: err, status: "signed out of " + id}
	})
}

// modelsCmd lists models of connected providers only.
func modelsCmd(ctx context.Context, c *client.Client) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		ms, err := c.Models(ctx, "", false)
		return modelsMsg{ms, err}
	})
}

// rolesMsg carries the channel's roles: for the /role picker, or (quiet)
// to refresh the cached roles that filter the models and variants dialogs
// and tint role names.
type rolesMsg struct {
	roles []protocol.PresetInfo
	err   error
	quiet bool
}

func rolesCmd(ctx context.Context, c *client.Client, channel string, quiet bool) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		rs, err := c.Presets(ctx, channel)
		return rolesMsg{roles: rs, err: err, quiet: quiet}
	})
}

func pickRoleCmd(ctx context.Context, c *client.Client, agent, role string) tea.Cmd {
	return resultCmd(ctx, "role set to "+role, func(ctx context.Context) error { return c.SetAgentRole(ctx, agent, role) })
}

func addDirCmd(ctx context.Context, c *client.Client, channel, dir string) tea.Cmd {
	return resultCmd(ctx, "added "+dir, func(ctx context.Context) error { return c.AddChannelDir(ctx, channel, dir) })
}

func removeDirCmd(ctx context.Context, c *client.Client, channel, dir string) tea.Cmd {
	return resultCmd(ctx, "removed "+format.ShortHome(dir), func(ctx context.Context) error { return c.RemoveChannelDir(ctx, channel, dir) })
}

// replaceDirCmd swaps one directory for another (an edit in the dirs
// dialog): the new one is added first so no agent loses ground.
func replaceDirCmd(ctx context.Context, c *client.Client, channel, oldDir, newDir string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		if err := c.AddChannelDir(ctx, channel, newDir); err != nil {
			return resultMsg{"", err}
		}
		return resultMsg{"replaced " + format.ShortHome(oldDir) + " with " + newDir, c.RemoveChannelDir(ctx, channel, oldDir)}
	})
}

// renameChannelCmd is /rename: the daemon normalises the name and refuses one
// another channel has.
func renameChannelCmd(ctx context.Context, c *client.Client, channel, name string) tea.Cmd {
	return resultCmd(ctx, "channel renamed", func(ctx context.Context) error { return c.RenameChannel(ctx, channel, name) })
}

func setModeCmd(ctx context.Context, c *client.Client, channel, mode string) tea.Cmd {
	return resultCmd(ctx, "mode "+mode+": "+protocol.ModeSummary(mode), func(ctx context.Context) error { return c.SetChannelMode(ctx, channel, mode) })
}

// compactCmd is /compact. Summarising takes a model call, so it gets a
// longer timeout than the usual RPC.
func compactCmd(ctx context.Context, c *client.Client, agent string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		status, err := c.CompactAgent(ctx, agent)
		if status == "queued" {
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
	channelsPicker  channelsPurpose = iota // the /channels picker
	channelsHistory                        // earlier prompts for ↑/↓ on the start screen
	channelsNav                            // the sidebar's channels section
)

// channelsMsg carries channel.list for one purpose.
type channelsMsg struct {
	channels []protocol.ChannelInfo
	err      error
	purpose  channelsPurpose
}

func channelsCmd(ctx context.Context, c *client.Client, dir string, purpose channelsPurpose) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		ss, err := c.Channels(ctx, dir, false)
		return channelsMsg{ss, err, purpose}
	})
}

// resumable is the sidebar's channels section: the directory's other
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
		info, err := c.CreateNamedChannel(ctx, dir, name)
		if err == nil {
			_ = c.Unsubscribe(ctx, from)
		}
		return switchedMsg{info, err}
	})
}

func switchChannelCmd(ctx context.Context, c *client.Client, from, to string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		_ = c.Unsubscribe(ctx, from)
		info, err := c.ResumeChannel(ctx, to)
		return switchedMsg{info, err}
	})
}

// variantsMsg carries the variant names a model offers, for the /variants
// picker.
type variantsMsg struct {
	model    string
	current  string
	variants []string
	err      error
}

func variantsCmd(ctx context.Context, c *client.Client, modelID, current string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		vs, err := c.Variants(ctx, modelID)
		return variantsMsg{modelID, current, vs, err}
	})
}

func pickVariantCmd(ctx context.Context, c *client.Client, agent, variant string) tea.Cmd {
	what := "variant set to " + variant
	if variant == "" {
		what = "variant reset to the provider default"
	}
	return resultCmd(ctx, what, func(ctx context.Context) error { return c.SetAgentVariant(ctx, agent, variant) })
}

// pickAgentModelCmd / pickChannelModelCmd are the /models overlay actions.
func pickAgentModelCmd(ctx context.Context, c *client.Client, agent, modelID string) tea.Cmd {
	return resultCmd(ctx, "model set to "+modelID, func(ctx context.Context) error { return c.SetAgentModel(ctx, agent, modelID) })
}

func pickChannelModelCmd(ctx context.Context, c *client.Client, channel, modelID string) tea.Cmd {
	return resultCmd(ctx, "channel model set to "+modelID, func(ctx context.Context) error { return c.SetChannelModel(ctx, channel, modelID) })
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
			v, err := client.DecodeNotification(r)
			if err != nil {
				continue
			}
			switch n := v.(type) {
			case protocol.EventNotification:
				p.Send(eventMsg{n.Event})
			case protocol.StreamNotification:
				p.Send(streamMsg{n})
			case protocol.PromptNotification:
				p.Send(promptMsg{n})
			}
		}
	}
}
