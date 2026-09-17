package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

func cmdDiscord(ctx context.Context, _ string, args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	m := protocol.DiscordStatusMethod
	switch action {
	case "status":
	case "connect":
		m = protocol.DiscordConnect
	case "disconnect":
		m = protocol.DiscordDisconnect
	default:
		return errors.New("usage: stavlos discord [status|connect|disconnect]")
	}
	if len(args) > 1 {
		return errors.New("usage: stavlos discord [status|connect|disconnect]")
	}
	c, err := connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	status, err := client.Do(ctx, c, m, protocol.None{})
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
