// Command stavlosd is the Stavlos daemon (PRD §4.1).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/daemon"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/paths"
)

func main() {
	socket := flag.String("socket", paths.Socket(), "unix socket path")
	dataDir := flag.String("data-dir", paths.DataDir(), "data directory (event log, trust records)")
	flag.Parse()
	if err := Run(*socket, *dataDir); err != nil {
		fmt.Fprintln(os.Stderr, "stavlosd:", err)
		os.Exit(1)
	}
}

// Run starts the daemon and blocks until SIGINT/SIGTERM.
func Run(socket, dataDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.Shutdown = cancel
	log.Printf("stavlosd %s listening on %s (%d providers)", buildid.ID(), socket, len(reg.Providers()))
	return d.Serve(sctx, socket)
}
