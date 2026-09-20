// Command stavlos is the CLI and TUI client (PRD §7.2).
//
//	stavlos                 resume the last viewed channel with the global channel list
//	stavlos new             start another channel in the current directory
//	stavlos open <#name>    open a channel by name or id
//	stavlos channels        list channels
//	stavlos daemon          run the daemon in the foreground
//	stavlos status          daemon status
//	stavlos init            write a starter global config
//	stavlos trust [dir]     review and confirm a project's .stavlos/ layer
//	stavlos tree <channel>  print the agent tree
//	stavlos send|steer|cancel|kill <agent> [text]
//	stavlos auth login      sign in to a subscription (tokens go to auth.json)
//	stavlos version         print the version (also --version)
//	stavlos plugin ...      (roadmap)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/config"

	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/daemon"
	"github.com/nicodes/stavlos/internal/discord"
	"github.com/nicodes/stavlos/internal/navigation"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/tui"
	"github.com/nicodes/stavlos/pkg/client"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stavlos:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd, args := commandOf(args)
	if f, ok := subcommands[cmd]; ok {
		return f(context.Background(), cmd, args)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

// commandOf splits the command word off args. A first argument naming a
// command (including --help and --version) is that command; any other
// word is an unknown command; leading flags belong to a new channel.
func commandOf(args []string) (cmd string, rest []string) {
	if len(args) == 0 {
		return "", nil
	}
	if _, ok := subcommands[args[0]]; ok || !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// subcommands maps each command word to its handler; "" opens the
// directory's channel.
var subcommands = map[string]func(ctx context.Context, cmd string, args []string) error{
	"":          cmdStart,
	"new":       cmdNew,
	"open":      cmdOpen,
	"channels":  cmdChannels,
	"daemon":    cmdDaemon,
	"status":    cmdStatus,
	"discord":   cmdDiscord,
	"web":       cmdWeb,
	"init":      func(_ context.Context, _ string, args []string) error { return initConfig(args) },
	"auth":      cmdAuth,
	"provider":  cmdAuth,
	"connect":   cmdAuth,
	"trust":     cmdTrust,
	"tree":      cmdTree,
	"send":      cmdSend,
	"steer":     cmdSend,
	"cancel":    cmdSend,
	"kill":      cmdSend,
	"plugin":    cmdPlugin,
	"help":      cmdHelp,
	"-h":        cmdHelp,
	"--help":    cmdHelp,
	"version":   cmdVersion,
	"--version": cmdVersion,
}

// cmdStart opens the global last-viewed channel, or creates the first one.
func cmdStart(ctx context.Context, _ string, args []string) error {
	return startChannel(ctx, "stavlos", args, false)
}

// cmdNew creates a channel with a directory defaulted from the launching shell.
func cmdNew(ctx context.Context, _ string, args []string) error {
	return startChannel(ctx, "new", args, true)
}

// startChannel resumes the last human selection, or creates a channel when
// explicitly requested (new or creation flags), or when the catalog is empty.
func startChannel(ctx context.Context, name string, args []string, fresh bool) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	modelID := fs.String("model", "", "provider/model-id for a new channel")
	root := fs.String("root", "", "root archetype for a new channel (default from config)")
	dir := fs.String("dir", "", "working directory (default: cwd)")
	noTUI := fs.Bool("no-tui", false, "open or create the channel and print its id without opening the TUI")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	d := cwd("")
	if *dir != "" {
		d, err = config.WorkingDirectory(d, *dir)
		if err != nil {
			return err
		}
	}
	var s protocol.ChannelInfo
	res, err := client.Do(ctx, c, protocol.ChannelList, protocol.ChannelListParams{})
	if err != nil {
		return err
	}
	if id := resumeChannel(res.Channels, navigation.Last()); id != "" && !fresh && *dir == "" && *modelID == "" && *root == "" {
		s, err = client.Do(ctx, c, protocol.ChannelResume, protocol.ChannelRef{Channel: id})
	} else {
		s, err = client.Do(ctx, c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: d, Model: *modelID, RootAgent: *root})
	}
	if err != nil {
		return err
	}
	if *noTUI {
		fmt.Println(s.ID)
		return nil
	}
	return tui.Run(ctx, c, s.ID)
}

// resumeChannel prefers the last human selection, falling back to the newest
// available channel. The launch directory never filters the catalog.
func resumeChannel(channels []protocol.ChannelInfo, last string) string {
	for _, ch := range channels {
		if ch.ID == last && !ch.Archived {
			return ch.ID
		}
	}
	for _, ch := range channels {
		if !ch.Archived {
			return ch.ID
		}
	}
	return ""
}

// cmdOpen opens a channel by #name or id; without one, the last viewed channel.
func cmdOpen(ctx context.Context, _ string, args []string) error {
	if len(args) == 0 {
		return cmdStart(ctx, "", nil)
	}
	c, err := connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	want := strings.TrimPrefix(args[0], "#")
	res, err := client.Do(ctx, c, protocol.ChannelList, protocol.ChannelListParams{})
	if err != nil {
		return err
	}
	for _, ch := range res.Channels {
		if ch.ID == want || ch.Name == want {
			s, err := client.Do(ctx, c, protocol.ChannelResume, protocol.ChannelRef{Channel: ch.ID})
			if err != nil {
				return err
			}
			return tui.Run(ctx, c, s.ID)
		}
	}
	return fmt.Errorf("no channel #%s (stavlos channels lists them)", want)
}

func cmdChannels(ctx context.Context, _ string, _ []string) error {
	c, err := connect(ctx, false)
	if err != nil {
		return err
	}
	defer c.Close()
	res, err := client.Do(ctx, c, protocol.ChannelList, protocol.ChannelListParams{IncludeArchived: true})
	if err != nil {
		return err
	}
	for _, s := range res.Channels {
		archived := ""
		if s.Archived {
			archived = " (archived)"
		}
		fmt.Printf("#%-24s %s  %-40s %-30s live=%d cost=$%.4f seq=%d%s\n", s.Name, s.ID, s.Dir, s.Model, s.Live, s.CostUSD, s.Seq, archived)
	}
	return nil
}

func cmdDaemon(ctx context.Context, _ string, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	socket := fs.String("socket", paths.Socket(), "unix socket path")
	dataDir := fs.String("data-dir", paths.DataDir(), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	err := daemon.Main(ctx, daemon.Options{Socket: *socket, DataDir: *dataDir,
		Discord: func(ctx context.Context, socket, data string) daemon.DiscordService {
			return discord.NewService(ctx, socket, data)
		},
	})
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		return fmt.Errorf("%v; connect to it with `stavlos`, or stop it first", err)
	}
	return err
}

func cmdStatus(ctx context.Context, _ string, _ []string) error {
	c, err := connect(ctx, false)
	if err != nil {
		return err
	}
	defer c.Close()
	st, err := client.Do(ctx, c, protocol.DaemonStatus, protocol.None{})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdAuth(ctx context.Context, _ string, args []string) error { return authCmd(ctx, args) }

// cmdTrust shows a project's pending .stavlos/ layer and records the answer.
func cmdTrust(ctx context.Context, _ string, args []string) error {
	c, err := connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	d := cwd("")
	if len(args) > 0 {
		d = cwd(args[0])
	}
	st, err := client.Do(ctx, c, protocol.TrustStatus, protocol.TrustStatusParams{Dir: d})
	if err != nil {
		return err
	}
	if !st.Pending {
		fmt.Println("nothing pending for", d)
		return nil
	}
	fmt.Printf("Project configuration in %s is not yet trusted. It can start MCP servers, tighten policy, add roles and skills, and instruct agents.\nFiles:\n", textsafe.Visible(d))
	for _, f := range st.Files {
		fmt.Println("  ", textsafe.Visible(f)) // a repository's file names: controls shown, never obeyed
	}
	fmt.Print("Trust it? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	ok := strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
	if _, err := client.Do(ctx, c, protocol.TrustReply, protocol.TrustReplyParams{Dir: d, Hash: st.Hash, Trust: ok}); err != nil {
		return err
	}
	if ok {
		fmt.Println("trusted", st.Hash)
	}
	return nil
}

func cmdTree(ctx context.Context, _ string, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: stavlos tree <channel>")
	}
	c, err := connect(ctx, false)
	if err != nil {
		return err
	}
	defer c.Close()
	tree, err := client.Do(ctx, c, protocol.AgentTree, protocol.AgentTreeParams{Channel: args[0]})
	if err != nil {
		return err
	}
	for _, a := range tree.Agents {
		fmt.Printf("%s%s  %s (%s) %s turn=%d $%.4f %s\n", strings.Repeat("  ", a.Depth), a.ID, a.Name, a.Role, a.State, a.Turn, a.CostUSD, a.Model)
	}
	return nil
}

// cmdSend delivers a prompt, steer, cancel or kill to an agent.
func cmdSend(ctx context.Context, cmd string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: stavlos %s <agent> [text]", cmd)
	}
	c, err := connect(ctx, false)
	if err != nil {
		return err
	}
	defer c.Close()
	kind := map[string]protocol.Kind{"send": protocol.KindPrompt, "steer": protocol.KindSteer, "cancel": protocol.KindCancel, "kill": protocol.KindKill}[cmd]
	_, err = client.Do(ctx, c, protocol.AgentSend, protocol.AgentSendParams{Agent: args[0], Kind: kind, Text: strings.Join(args[1:], " ")})
	return err
}

func cmdPlugin(context.Context, string, []string) error {
	return errors.New("plugins are a roadmap item (PRD §11); Stavlos serves the ChatGPT (openai), Grok (xai), Z.ai Coding Plan (zai) and Kimi For Coding (kimi) subscriptions: see `stavlos auth login`")
}

func cmdHelp(context.Context, string, []string) error {
	fmt.Print(usage)
	return nil
}

// cmdVersion prints the version, commit, toolchain and build id of this
// binary; it does not contact the daemon.
func cmdVersion(context.Context, string, []string) error {
	fmt.Print(buildid.Read())
	return nil
}

const usage = `usage:
  stavlos                                             last viewed channel + global navigation
  stavlos new [--model p/m] [--root archetype] [--dir d]   new channel (directory defaults to cwd)
  stavlos --dir d [--model p/m] [--root archetype]       new channel with an explicit directory
  stavlos open <#name|id>                               a channel by name + TUI
  stavlos channels | status | tree <channel>
  stavlos discord [status|connect|disconnect]           manage the daemon's Discord integration
  stavlos web [status|on|off|open] [--print]            the browser UI on 127.0.0.1:4999 (--print shows the sign-in link instead of opening it)
  stavlos send|steer|cancel|kill <agent> [text]
  stavlos auth login [provider] | auth list | auth logout [provider]
  stavlos trust [dir] | init [--model p/m] | daemon
  stavlos version | --version | help
`

func cwd(d string) string {
	if d == "" {
		d, _ = os.Getwd()
	}
	abs, _ := filepath.Abs(d)
	return abs
}

func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	modelID := fs.String("model", "", "default model, such as openai/gpt-5.4 (leave it out to pick one with /models)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := paths.ConfigDir()
	p := filepath.Join(dir, "stavlos.json")
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("%s already exists", p)
	}
	for _, sub := range []string{"agents", "skills"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(starterConfig(*modelID), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote", p)
	fmt.Println(`next:
  - run stavlos in a project directory, then /providers to sign in and /models to pick a model (or run stavlos auth login now)
  - add agent definitions as agents/<name>.md next to stavlos.json (docs/stavlos-prd.md §10.3)
  - directories every channel may work in besides its own: "dirs": ["~/Work/shared"]
  - for web_search on your own quota, add "search": {"provider": "brave", "apiKey": "${env:BRAVE_API_KEY}"}
  - commands run without credential-looking variables; list what a build needs under "env": {"pass": ["GITHUB_TOKEN"]}`)
	return nil
}

// starterConfig is the global config stavlos init writes: the defaults
// spelled out (config.Defaults), the model, and shell rules to start from.
func starterConfig(model string) config.File {
	f := config.Defaults()
	f.Model = model
	// Allow the commands you trust here, or answer the prompt.
	f.Policy["shell"] = map[string]any{"*": "ask", "git push*": "ask", "rm -rf*": "deny"}
	return f
}
