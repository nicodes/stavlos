package discord

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nicodes/stavlos/internal/config"
	"golang.org/x/sys/unix"
)

var errBridgeRunning = errors.New("a Discord bridge is already running for this server/category (an old standalone stavlos-discord process or service? stop it first)")

func runBridge(ctx context.Context, cfg config.Discord, socket, data string, publish func(*Bridge)) error {
	state, lock, err := lockBridge(data, cfg)
	if err != nil {
		return err
	}
	defer lock.Close()
	b, closeGateway, err := Open(ctx, cfg.Token, cfg.Guild, func(api API, bot string) (*Bridge, error) {
		return New(cfg, api, bot, state)
	})
	if err != nil {
		return err
	}
	defer closeGateway()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if publish != nil {
		publish(b)
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-b.gatewayLost:
			cancel()
		case <-bridgeCtx.Done():
		}
	}()
	err = b.Run(bridgeCtx, socket)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	return errors.New("discord gateway disconnected; reconnecting")
}

func lockBridge(data string, cfg config.Discord) (string, *os.File, error) {
	if err := os.MkdirAll(data, 0o700); err != nil {
		return "", nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(cfg.Guild+"/"+cfg.Category)))[:16]
	state := filepath.Join(data, "discord-"+key+"-prompts.json")
	lock, err := os.OpenFile(state+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return "", nil, errBridgeRunning
	}
	return state, lock, nil
}
