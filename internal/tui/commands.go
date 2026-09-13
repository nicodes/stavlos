package tui

// tea.Cmds wrapping every client call. Update never touches the client
// directly (PRD §7.2 implementation note); it only returns these.

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
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
	presetsMsg  struct {
		presets []protocol.PresetInfo
		err     error
	}
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

	// providersMsg carries provider.list. notice=true renders the list into
	// the transcript instead of opening the overlay; refresh=true only
	// updates the cached list; jump names a provider to sign in to at once.
	providersMsg struct {
		res     protocol.ProviderListResult
		err     error
		notice  bool
		refresh bool
		jump    string
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

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, callTimeout)
}

func reconcileCmd(ctx context.Context, c *client.Client, session string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		res, err := c.Reconcile(ctx, session)
		return reconcileMsg{res, err}
	}
}

func subscribeCmd(ctx context.Context, c *client.Client, session string, from int64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return subscribedMsg{c.Subscribe(ctx, session, from)}
	}
}

func treeCmd(ctx context.Context, c *client.Client, session string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		agents, err := c.Tree(ctx, session)
		return treeMsg{agents, err}
	}
}

// treeDebounceCmd fires a tick after which the tree is refetched.
func treeDebounceCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return treeTickMsg{} })
}

func sendCmd(ctx context.Context, c *client.Client, agent string, kind protocol.Kind, text, ok string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return resultMsg{ok, c.Send(ctx, agent, kind, text)}
	}
}

func setAgentModelCmd(ctx context.Context, c *client.Client, agent, modelID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return resultMsg{"agent model → " + modelID, c.SetAgentModel(ctx, agent, modelID)}
	}
}

func setSessionModelCmd(ctx context.Context, c *client.Client, session, modelID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return resultMsg{"session model → " + modelID, c.SetSessionModel(ctx, session, modelID)}
	}
}

func presetsCmd(ctx context.Context, c *client.Client, session string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		p, err := c.Presets(ctx, session)
		return presetsMsg{p, err}
	}
}

// answerPromptCmd claims then replies in one step. Claiming happens only
// here, i.e. only once the user pressed a key (PRD §7.4).
func answerPromptCmd(ctx context.Context, c *client.Client, id, answer string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		if err := c.ClaimPrompt(ctx, id); err != nil {
			return promptReplyMsg{id, err}
		}
		return promptReplyMsg{id, c.ReplyPrompt(ctx, id, answer)}
	}
}

// trustReplyCmd claims the trust prompt then answers via trust.reply.
func trustReplyCmd(ctx context.Context, c *client.Client, id, dir, hash string, trust bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		if err := c.ClaimPrompt(ctx, id); err != nil {
			return promptReplyMsg{id, err}
		}
		return promptReplyMsg{id, c.TrustReply(ctx, dir, hash, trust)}
	}
}

func providersCmd(ctx context.Context, c *client.Client, notice bool, jump string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		res, err := c.Providers(ctx)
		return providersMsg{res: res, err: err, notice: notice, jump: jump}
	}
}

// refreshProvidersCmd re-fetches provider.list without opening anything.
func refreshProvidersCmd(ctx context.Context, c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		res, err := c.Providers(ctx)
		return providersMsg{res: res, err: err, refresh: true}
	}
}

// loginStartCmd begins the device-code login for provider.
func loginStartCmd(ctx context.Context, c *client.Client, provider, method string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		res, err := c.LoginStart(ctx, provider, method)
		return loginStartMsg{provider: provider, res: res, err: err}
	}
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

func disconnectProviderCmd(ctx context.Context, c *client.Client, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return resultMsg{"disconnected " + id, c.DisconnectProvider(ctx, id)}
	}
}

// modelsCmd lists models of connected providers only.
func modelsCmd(ctx context.Context, c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		ms, err := c.Models(ctx, "", false)
		return modelsMsg{ms, err}
	}
}

// rolesCmd lists presets for the /role picker; pickRoleCmd applies one.
type rolesMsg struct {
	roles []protocol.PresetInfo
	err   error
}

func rolesCmd(ctx context.Context, c *client.Client, session string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		rs, err := c.Presets(ctx, session)
		return rolesMsg{rs, err}
	}
}

func pickRoleCmd(ctx context.Context, c *client.Client, agent, role string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return resultMsg{"role set to " + role, c.SetAgentRole(ctx, agent, role)}
	}
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
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		vs, err := c.Variants(ctx, modelID)
		return variantsMsg{modelID, current, vs, err}
	}
}

func pickVariantCmd(ctx context.Context, c *client.Client, agent, variant string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		what := "variant set to " + variant
		if variant == "" {
			what = "variant reset to the provider default"
		}
		return resultMsg{what, c.SetAgentVariant(ctx, agent, variant)}
	}
}

// pickAgentModelCmd / pickSessionModelCmd are the /models overlay actions.
func pickAgentModelCmd(ctx context.Context, c *client.Client, agent, modelID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return resultMsg{"model set to " + modelID, c.SetAgentModel(ctx, agent, modelID)}
	}
}

func pickSessionModelCmd(ctx context.Context, c *client.Client, session, modelID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := withTimeout(ctx)
		defer cancel()
		return resultMsg{"session model set to " + modelID, c.SetSessionModel(ctx, session, modelID)}
	}
}

func placeholderTickCmd() tea.Cmd {
	return tea.Tick(placeholderPeriod, func(time.Time) tea.Msg { return placeholderTickMsg{} })
}

func clearStatusCmd(token int, after time.Duration) tea.Cmd {
	return tea.Tick(after, func(time.Time) tea.Msg { return clearStatusMsg{token} })
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
