// stavlos-discord is the standalone Discord gateway client for a local daemon.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/discord"
	"github.com/nicodes/stavlos/internal/paths"
	"golang.org/x/sys/unix"
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
	if err := os.MkdirAll(paths.DataDir(), 0o700); err != nil {
		return err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(cfg.Guild+"/"+cfg.Category)))[:16]
	state := filepath.Join(paths.DataDir(), "discord-"+key+"-prompts.json")
	lock, err := os.OpenFile(state+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("a bridge for this guild/category is already running: %w", err)
	}
	b, closeGateway, err := discord.Open(ctx, cfg.Token, cfg.Guild, func(api discord.API, bot string) (*discord.Bridge, error) {
		return discord.New(cfg, api, bot, state)
	})
	if err != nil {
		return err
	}
	defer closeGateway()
	log.Printf("discord: connected to guild %s", cfg.Guild)
	return b.Run(ctx, paths.Socket())
}
