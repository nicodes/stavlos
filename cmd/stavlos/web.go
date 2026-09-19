package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

// cmdWeb turns the daemon's web UI on and off. "on" and "open" open it in a
// browser; the sign-in link carries a one-time code, so it is printed only
// for --print (an SSH box with no browser), never otherwise.
func cmdWeb(ctx context.Context, _ string, args []string) error {
	const usage = "usage: stavlos web [status|on|off|open] [--print]"
	action, print := "status", false
	for _, a := range args {
		switch a {
		case "status", "on", "off", "open":
			action = a
		case "--print":
			print = true
		default:
			return errors.New(usage)
		}
	}
	m := map[string]protocol.Method[protocol.None, protocol.WebStatus]{
		"status": protocol.WebStatusMethod, "on": protocol.WebEnable, "off": protocol.WebDisable, "open": protocol.WebOpen,
	}[action]
	c, err := connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	st, err := client.Do(ctx, c, m, protocol.None{})
	if err != nil {
		return err
	}
	switch {
	case !st.Enabled:
		fmt.Println("web UI off")
	case st.OpenURL != "" && print:
		fmt.Println(st.OpenURL)
	case st.OpenURL != "":
		openBrowser(st.OpenURL)
		fmt.Println("web UI on " + st.URL + " (opened in your browser)")
	default:
		fmt.Println("web UI on " + st.URL)
	}
	return nil
}
