// Command stavlos is the CLI and TUI client (PRD §7.2).
//
//	stavlos                 start a new session in the current directory and open the TUI
//	stavlos resume [id]     resume the latest (or given) session for this directory
//	stavlos sessions        list sessions
//	stavlos daemon          run the daemon in the foreground
//	stavlos status          daemon status
//	stavlos init            write a starter global config
//	stavlos trust [dir]     review and confirm a project's .stavlos/ layer
//	stavlos tree <session>  print the agent tree
//	stavlos send|steer|cancel|kill <agent> [text]
//	stavlos auth login      connect a provider (keys go to auth.json, never the environment)
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

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/daemon"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui"
	"github.com/nicodes/stavlos/pkg/client"
)

// buildTag is set with -ldflags -X for builds that must differ byte-wise (tests).
var buildTag string

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "stavlos:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	ctx := context.Background()
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "new":
		fs := flag.NewFlagSet("new", flag.ContinueOnError)
		modelID := fs.String("model", "", "provider/model-id for this session")
		root := fs.String("root", "", "root archetype (default from config)")
		dir := fs.String("dir", "", "working directory (default: cwd)")
		noTUI := fs.Bool("no-tui", false, "create the session and print its id without opening the TUI")
		if err := fs.Parse(args); err != nil {
			return err
		}
		c, err := connect(ctx, true)
		if err != nil {
			return err
		}
		defer c.Close()
		d := cwd(*dir)
		// A session only counts once someone has prompted it: if the
		// directory's newest session is still untouched, reuse it instead
		// of leaving another empty one behind.
		var s protocol.SessionInfo
		if list, lerr := c.Sessions(ctx, d, false); lerr == nil && len(list) > 0 && list[0].Title == "" && *modelID == "" && *root == "" {
			s, err = c.ResumeSession(ctx, list[0].ID)
		} else {
			s, err = c.CreateSession(ctx, d, *modelID, *root)
		}
		if err != nil {
			return err
		}
		if *noTUI {
			fmt.Println(s.ID)
			return nil
		}
		return tui.Run(ctx, c, s.ID)

	case "resume":
		c, err := connect(ctx, true)
		if err != nil {
			return err
		}
		defer c.Close()
		id := ""
		if len(args) > 0 {
			id = args[0]
		} else {
			list, err := c.Sessions(ctx, cwd(""), false)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				return errors.New("no sessions for this directory; run `stavlos` to start one")
			}
			id = list[0].ID
		}
		s, err := c.ResumeSession(ctx, id)
		if err != nil {
			return err
		}
		return tui.Run(ctx, c, s.ID)

	case "sessions":
		c, err := connect(ctx, false)
		if err != nil {
			return err
		}
		defer c.Close()
		list, err := c.Sessions(ctx, "", true)
		if err != nil {
			return err
		}
		for _, s := range list {
			flag := ""
			if s.Archived {
				flag = " (archived)"
			}
			fmt.Printf("%s  %-40s %-30s live=%d cost=$%.4f seq=%d%s\n", s.ID, s.Dir, s.Model, s.Live, s.CostUSD, s.Seq, flag)
		}
		return nil

	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
		socket := fs.String("socket", paths.Socket(), "unix socket path")
		dataDir := fs.String("data-dir", paths.DataDir(), "data directory")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return runDaemon(ctx, *socket, *dataDir)

	case "status":
		c, err := connect(ctx, false)
		if err != nil {
			return err
		}
		defer c.Close()
		st, err := c.Status(ctx)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
		return nil

	case "init":
		return initConfig(args)

	case "auth", "provider", "connect":
		return authCmd(ctx, args)

	case "trust":
		c, err := connect(ctx, true)
		if err != nil {
			return err
		}
		defer c.Close()
		d := cwd("")
		if len(args) > 0 {
			d = cwd(args[0])
		}
		st, err := c.TrustStatus(ctx, d)
		if err != nil {
			return err
		}
		if !st.Pending {
			fmt.Println("nothing pending for", d)
			return nil
		}
		fmt.Printf("Project configuration in %s is not yet trusted. It can start MCP servers, tighten policy, add roles and skills, and instruct agents.\nFiles:\n", d)
		for _, f := range st.Files {
			fmt.Println("  ", f)
		}
		fmt.Print("Trust it? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		ok := strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
		if err := c.TrustReply(ctx, d, st.Hash, ok); err != nil {
			return err
		}
		if ok {
			fmt.Println("trusted", st.Hash)
		}
		return nil

	case "tree":
		if len(args) < 1 {
			return errors.New("usage: stavlos tree <session>")
		}
		c, err := connect(ctx, false)
		if err != nil {
			return err
		}
		defer c.Close()
		agents, err := c.Tree(ctx, args[0])
		if err != nil {
			return err
		}
		for _, a := range agents {
			fmt.Printf("%s%s  %s (%s) %s turn=%d $%.4f %s\n", strings.Repeat("  ", a.Depth), a.ID, a.Label, a.Archetype, a.State, a.Turn, a.CostUSD, a.Model)
		}
		return nil

	case "send", "steer", "cancel", "kill":
		if len(args) < 1 {
			return fmt.Errorf("usage: stavlos %s <agent> [text]", cmd)
		}
		c, err := connect(ctx, false)
		if err != nil {
			return err
		}
		defer c.Close()
		kind := map[string]protocol.Kind{"send": protocol.KindPrompt, "steer": protocol.KindSteer, "cancel": protocol.KindCancel, "kill": protocol.KindKill}[cmd]
		return c.Send(ctx, args[0], kind, strings.Join(args[1:], " "))

	case "plugin":
		return errors.New("plugin install is a roadmap item; in-tree providers: anthropic, and every OpenAI-compatible provider listed on models.dev (ollama, lmstudio included)")

	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

const usage = `usage:
  stavlos [--model p/m] [--root archetype] [--dir d]   new session + TUI
  stavlos resume [session-id]                           resume + TUI
  stavlos sessions | status | tree <session>
  stavlos send|steer|cancel|kill <agent> [text]
  stavlos auth login [provider] | auth list | auth logout [provider]
  stavlos trust [dir] | init [--model p/m] | daemon
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
	if _, err := c.Attach(ctx, fmt.Sprintf("tui:%d", os.Getpid()), protocol.TierInteractive); err != nil {
		c.Close()
		return nil, err
	}
	if fresh, err := replaceStale(ctx, c); err != nil {
		return nil, err
	} else if fresh != nil {
		return fresh, nil
	}
	return c, nil
}

// replaceStale shuts down a daemon built from different code than this
// client (typical after editing and re-running with `go run`) and starts a
// new one from this binary. Returns nil, nil when the daemon is current.
func replaceStale(ctx context.Context, c *client.Client) (*client.Client, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return nil, nil // very old daemon without status; leave it
	}
	if st.Build == buildid.ID() || os.Getenv("STAVLOS_KEEP_DAEMON") != "" {
		return nil, nil
	}
	fmt.Fprintf(os.Stderr, "daemon build %s differs from this binary (%s); restarting it\n", short(st.Build), short(buildid.ID()))
	if err := c.Shutdown(ctx); err != nil && st.PID > 0 {
		// Older daemon without daemon.shutdown: ask the OS instead.
		if p, perr := os.FindProcess(st.PID); perr == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	c.Close()
	sock := paths.Socket()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		nc, err := client.Dial(sock)
		if err != nil {
			continue
		}
		if _, err := nc.Attach(ctx, fmt.Sprintf("tui:%d", os.Getpid()), protocol.TierInteractive); err != nil {
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
	fmt.Fprintf(os.Stderr, "started stavlosd (pid %d, log %s)\n", cmd.Process.Pid, logf.Name())
	return cmd.Process.Release()
}

func runDaemon(ctx context.Context, socket, dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	reg, err := registry.Default(ctx, auth.Open(paths.AuthFile()))
	if err != nil {
		return fmt.Errorf("model registry: %w", err)
	}
	d, err := daemon.New(ctx, dataDir, reg)
	if err != nil {
		return err
	}
	defer d.Close()
	sctx, stop := context.WithCancel(signalCtx(ctx))
	defer stop()
	d.Shutdown = stop
	fmt.Fprintf(os.Stderr, "%s stavlosd %s listening on %s (%d providers)\n", time.Now().Format(time.RFC3339), buildid.ID(), socket, len(reg.Providers()))
	return d.Serve(sctx, socket)
}

func initConfig(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	modelID := fs.String("model", "anthropic/claude-sonnet-5", "default model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := paths.ConfigDir()
	p := filepath.Join(dir, "stavlos.json")
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("%s already exists", p)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Join(dir, "skills"), 0o755)
	content := fmt.Sprintf(`{
  // Global Stavlos configuration. See docs/stavlos-prd.md §10.
  "model": %q,
  "rootAgent": "coder",
  "limits": { "maxDepth": 3, "maxAgents": 6 },
  "escalation": { "claimTimeout": "30s", "answerTimeout": "3m", "default": "deny" },
  // web_search backend (brave, tavily or exa) — leave out to keep web_search unconfigured
  // "search": { "provider": "brave", "apiKey": "${env:BRAVE_API_KEY}" },
  // commands and MCP servers run without STAVLOS_* and credential-looking variables; list the ones a build needs
  // "env": { "pass": ["GITHUB_TOKEN"] },
  "policy": {
    // read-only commands (grep, rg, find, ls, git status/log/diff, …) are allowed by the built-in defaults
    "shell": { "git push*": "ask", "rm -rf*": "deny" },
    "apply_patch": { "**": "ask" },
    "read":  "allow"
  }
}
`, *modelID)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", p)
	fmt.Println("run `stavlos` in a project directory; it will ask you to connect a provider on first use (or run `stavlos auth login` now)")
	return nil
}
