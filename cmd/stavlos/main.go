// Command stavlos is the CLI and TUI client (PRD §7.2).
//
//	stavlos                 open this directory's channel (a new one if it has none) in the TUI
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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nicodes/stavlos/internal/config"

	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/daemon"
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

// cmdStart opens the directory's channel in the TUI: its newest, or a new
// one when the directory has none (--model and --root apply only then).
func cmdStart(ctx context.Context, _ string, args []string) error {
	return startChannel(ctx, "stavlos", args, false)
}

// cmdNew starts another channel in a directory (reusing its newest while
// that one was never prompted) and opens the TUI.
func cmdNew(ctx context.Context, _ string, args []string) error {
	return startChannel(ctx, "new", args, true)
}

// startChannel opens a directory's newest channel or creates one: when fresh
// a new one unless the newest was never prompted, otherwise only when the
// directory has none.
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
	d := cwd(*dir)
	var s protocol.ChannelInfo
	if res, lerr := client.Do(ctx, c, protocol.ChannelList, protocol.ChannelListParams{Dir: d}); lerr == nil && len(res.Channels) > 0 && (!fresh || res.Channels[0].Title == "" && *modelID == "" && *root == "") {
		s, err = client.Do(ctx, c, protocol.ChannelResume, protocol.ChannelRef{ID: res.Channels[0].ID})
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

// cmdOpen opens a channel by #name or id; without one, the directory's.
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
			s, err := client.Do(ctx, c, protocol.ChannelResume, protocol.ChannelRef{ID: ch.ID})
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
	err := daemon.Main(ctx, daemon.Options{Socket: *socket, DataDir: *dataDir})
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
		fmt.Printf("%s%s  %s (%s) %s turn=%d $%.4f %s\n", strings.Repeat("  ", a.Depth), a.ID, a.Label, a.Archetype, a.State, a.Turn, a.CostUSD, a.Model)
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
	return errors.New("plugins are a roadmap item (PRD §11); Stavlos serves the ChatGPT (openai) and Grok (xai) subscriptions: see `stavlos auth login`")
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
  stavlos [--model p/m] [--root archetype] [--dir d]   this directory's channel (new if none) + TUI
  stavlos new [--model p/m] [--root archetype] [--dir d]   another channel + TUI
  stavlos open <#name|id>                               a channel by name + TUI
  stavlos channels | status | tree <channel>
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

// connect dials the daemon, starting it in the background if needed.
func connect(ctx context.Context, autostart bool) (*client.Client, error) {
	sock := paths.Socket()
	c, err := client.Dial(sock)
	if err == nil {
		return attach(ctx, c)
	}
	if !autostart {
		return nil, fmt.Errorf("daemon not running at %s (start with `stavlos daemon` or run `stavlos`)", sock)
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		if c, err = client.Dial(sock); err == nil {
			return attach(ctx, c)
		}
	}
	return nil, fmt.Errorf("daemon did not come up at %s; run `stavlos daemon` in another terminal to see why", sock)
}

func attach(ctx context.Context, c *client.Client) (*client.Client, error) {
	if _, err := client.Do(ctx, c, protocol.Attach, protocol.AttachParams{Client: fmt.Sprintf("tui:%d", os.Getpid()), Tier: protocol.TierInteractive}); err != nil {
		c.Close()
		if isVersionError(err) && os.Getenv("STAVLOS_KEEP_DAEMON") == "" {
			return replaceIncompatible(ctx, err)
		}
		return nil, err
	}
	if fresh, err := replaceStale(ctx, c); err != nil {
		return nil, err
	} else if fresh != nil {
		return fresh, nil
	}
	return c, nil
}

// isVersionError reports whether the daemon refused this client's protocol
// version.
func isVersionError(err error) bool {
	var pe *protocol.Error
	return errors.As(err, &pe) && pe.Code == protocol.ErrVersion
}

// replaceIncompatible stops a daemon that refuses this client's protocol
// version and starts one from this binary. Such a daemon was built before a
// protocol change and cannot answer daemon.status or daemon.shutdown either,
// so it is found as the process serving the socket and sent SIGTERM.
func replaceIncompatible(ctx context.Context, cause error) (*client.Client, error) {
	sock := paths.Socket()
	pid, err := socketPeerPID(sock)
	if err != nil {
		return nil, fmt.Errorf("%v; stop the running daemon and run stavlos again (finding it: %v)", cause, err)
	}
	fmt.Fprintf(os.Stderr, "daemon (pid %d) speaks an older protocol; restarting it\n", pid)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return nil, fmt.Errorf("%v; stopping daemon pid %d: %v", cause, pid, err)
	}
	if !waitExit(pid, 10*time.Second) {
		return nil, fmt.Errorf("%v; daemon pid %d did not exit after SIGTERM", cause, pid)
	}
	return restartDaemon(ctx, sock)
}

// waitExit waits up to d for process pid to be gone.
func waitExit(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}

func replaceStale(ctx context.Context, c *client.Client) (*client.Client, error) {
	st, err := client.Do(ctx, c, protocol.DaemonStatus, protocol.None{})
	if err != nil {
		return nil, nil // very old daemon without status; leave it
	}
	if st.Build == buildid.ID() || os.Getenv("STAVLOS_KEEP_DAEMON") != "" {
		return nil, nil
	}
	fmt.Fprintf(os.Stderr, "daemon build %s differs from this binary (%s); restarting it\n", short(st.Build), short(buildid.ID()))
	sock := paths.Socket()
	if _, err := client.Do(ctx, c, protocol.DaemonShutdown, protocol.None{}); err != nil {
		// A daemon that cannot shut down on request is stopped by the pid
		// the kernel reports for the socket, not the one it reports itself.
		if pid, perr := socketPeerPID(sock); perr == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	c.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return restartDaemon(ctx, sock)
}

// restartDaemon starts a daemon from this binary once the old one is gone
// and attaches to it.
func restartDaemon(ctx context.Context, sock string) (*client.Client, error) {
	if err := startDaemon(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		nc, err := client.Dial(sock)
		if err != nil {
			continue
		}
		if _, err := client.Do(ctx, nc, protocol.Attach, protocol.AttachParams{Client: fmt.Sprintf("tui:%d", os.Getpid()), Tier: protocol.TierInteractive}); err != nil {
			nc.Close()
			return nil, err
		}
		return nc, nil
	}
	return nil, fmt.Errorf("restarted daemon did not come up at %s", sock)
}

func short(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.DataDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(paths.DataDir(), "stavlosd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Stdin = nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "started the daemon (pid %d, log %s)\n", cmd.Process.Pid, logf.Name())
	return cmd.Process.Release()
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
	for _, sub := range []string{"roles", "skills"} {
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
  - add roles as roles/<name>.md next to stavlos.json (docs/stavlos-prd.md §10.3)
  - for web_search on your own quota, add "search": {"provider": "brave", "apiKey": "${env:BRAVE_API_KEY}"}
  - commands run without credential-looking variables; list what a build needs under "env": {"pass": ["GITHUB_TOKEN"]}`)
	return nil
}

// starterConfig is the global config stavlos init writes: the defaults
// spelled out, so the file shows where each setting lives. It is marshalled
// from config.File, so it always loads.
func starterConfig(model string) config.File {
	return config.File{
		Model:      model,
		Limits:     &config.Limits{MaxDepth: 3, MaxAgents: 6},
		Escalation: &config.Escalation{ClaimTimeout: "30s", AnswerTimeout: "3m", Default: "deny"},
		// No shell command is allowed by default (searching is the grep and
		// glob tools); allow the ones you trust here, or answer the prompt.
		Policy: map[string]any{
			"shell":       map[string]any{"git push*": "ask", "rm -rf*": "deny"},
			"apply_patch": map[string]any{"**": "ask"},
			"read":        "allow",
		},
	}
}
