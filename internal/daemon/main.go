package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/sandbox"
	"golang.org/x/sys/unix"
)

// tightenPerms makes the data directory and the files an older build
// created with looser modes private.
func tightenPerms(dataDir string) {
	_ = os.Chmod(dataDir, 0o700)
	for _, name := range []string{"events.db", "events.db-wal", "events.db-shm", "auth.json", "stavlosd.log", "stavlosd.lock"} {
		_ = os.Chmod(filepath.Join(dataDir, name), 0o600)
	}
}

// Options configure Main. Empty fields take the standard locations.
type Options struct {
	Socket  string                                               // unix socket path (paths.Socket)
	DataDir string                                               // event log, trust records, lock (paths.DataDir)
	Discord func(context.Context, string, string) DiscordService // socket, data directory
}

// Main runs the daemon in the foreground until SIGINT, SIGTERM or a
// client's daemon.shutdown. It is the one way the daemon starts: the
// stavlos command's "daemon" subcommand, and the background process the
// client launches, both land here.
func Main(ctx context.Context, o Options) error {
	if o.Socket == "" {
		o.Socket = paths.Socket()
	}
	if o.DataDir == "" {
		o.DataDir = paths.DataDir()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// What the daemon creates is its user's alone, and it cannot be read
	// through /proc or ptrace by another process of that user (an agent's
	// command): /proc/<pid>/environ and mem would hand over its environment
	// and the credentials it holds.
	buildid.ID() // before a go run binary can be deleted under us
	unix.Umask(0o077)
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
	// What the agents start stays a descendant of the daemon however it
	// forks, so the socket can go on refusing it (peercred.DescendsFrom); the
	// price of adopting orphans is reaping them.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		log.Printf("could not become a subreaper: %v (a process that detaches itself escapes the socket's descendant check)", err)
	} else {
		go reapOrphans(ctx, orphanScanEvery)
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return err
	}
	tightenPerms(o.DataDir)
	reg, err := registry.Default(ctx, auth.Open(paths.AuthFile()))
	if err != nil {
		return fmt.Errorf("model registry: %w", err)
	}
	d, err := New(ctx, o.DataDir, reg)
	if err != nil {
		return err
	}
	defer d.Close()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.Shutdown = cancel
	d.TreatInProcessAsBridge()
	if o.Discord != nil {
		d.Discord = o.Discord(sctx, o.Socket, o.DataDir)
	}
	d.runLoops()
	lvl, why := sandbox.Probe()
	log.Printf("stavlosd %s listening on %s (%d providers, sandbox %s)", buildid.ID(), o.Socket, len(reg.Providers()), lvl)
	if why != nil {
		log.Printf("sandbox: %v", why)
	}
	return d.Serve(sctx, o.Socket)
}
