// stavlos-discord is the standalone Discord gateway client for a local daemon.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/discord"
	"github.com/nicodes/stavlos/internal/paths"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	e, err := config.LoadGlobal()
	if err != nil {
		return err
	}
	if e.Discord == nil {
		return fmt.Errorf("add a discord block to %s/stavlos.json (see docs/discord-setup.md)", paths.ConfigDir())
	}
	cfg, err := e.Discord.Resolve()
	if err != nil {
		return err
	}
	log.Print("discord: standalone mode; use /discord connect in Stavlos for daemon-managed operation")
	return discord.RunStandalone(ctx, cfg, paths.Socket(), paths.DataDir())
}
