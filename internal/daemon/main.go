package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/paths"
)

// Options configure Main. Empty fields take the standard locations.
type Options struct {
	Socket  string // unix socket path (paths.Socket)
	DataDir string // event log, trust records, lock (paths.DataDir)
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
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return err
	}
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
	log.Printf("stavlosd %s listening on %s (%d providers)", buildid.ID(), o.Socket, len(reg.Providers()))
	return d.Serve(sctx, o.Socket)
}
