package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

// authCmd implements `stavlos auth login|list|logout`. Credentials go to
// <data dir>/auth.json (mode 0600) through the daemon, so a running daemon
// picks them up immediately. Nothing is ever exported to the environment.
func authCmd(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	c, err := connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.Close()
	switch sub {
	case "login", "connect", "add":
		want := ""
		if len(args) > 0 {
			want = args[0]
		}
		_, err := loginFlow(ctx, c, want)
		return err
	case "list", "ls":
		return listProviders(ctx, c)
	case "logout", "remove", "rm":
		return logoutFlow(ctx, c, strings.Join(args, " "))
	}
	return fmt.Errorf("usage: stavlos auth login [provider] | list | logout [provider]")
}

func listProviders(ctx context.Context, c *client.Client) error {
	r, err := client.Do(ctx, c, protocol.ProviderList, protocol.None{})
	if err != nil {
		return err
	}
	fmt.Printf("Providers  %s\n", shortHome(r.AuthPath))
	for _, p := range r.Providers {
		if p.Connected {
			acct := p.Account
			if acct == "" {
				acct = "connected"
			}
			fmt.Printf("  %-10s %s\n", p.Name, acct)
		} else {
			fmt.Printf("  %-10s not connected — %s\n", p.Name, p.Label)
		}
	}
	return nil
}

// loginFlow picks a provider (or uses want) and runs the device-code login:
// prints a URL and a code, then waits for the user to finish in a browser.
func loginFlow(ctx context.Context, c *client.Client, want string) (string, error) {
	r, err := client.Do(ctx, c, protocol.ProviderList, protocol.None{})
	if err != nil {
		return "", err
	}
	var items []pick
	for _, p := range r.Providers {
		hint := p.Label
		if p.Connected {
			hint = "connected · " + p.Account
		}
		items = append(items, pick{id: p.ID, label: p.Name, hint: hint})
	}
	var chosen pick
	if want != "" {
		for _, it := range items {
			if strings.EqualFold(it.id, want) || strings.EqualFold(it.label, want) {
				chosen = it
			}
		}
		if chosen.id == "" {
			return "", fmt.Errorf("unknown provider %q; Stavlos supports openai (ChatGPT) and xai (Grok)", want)
		}
	} else {
		chosen, err = selectFrom("Sign in with", items)
		if err != nil {
			return "", err
		}
	}
	method := ""
	for _, p := range r.Providers {
		if p.ID == chosen.id && len(p.Methods) > 1 {
			var ms []pick
			for _, m := range p.Methods {
				ms = append(ms, pick{id: m.ID, label: m.Label})
			}
			mp, err := selectFrom("Login method", ms)
			if err != nil {
				return "", err
			}
			method = mp.id
		}
	}
	start, err := client.Do(ctx, c, protocol.ProviderLoginStart, protocol.LoginStartParams{Provider: chosen.id, Method: method})
	if err != nil {
		return "", err
	}
	if start.Code != "" {
		fmt.Printf("\nOpen this URL on any device:\n\n    %s\n\nand enter the code:\n\n    %s\n\n%s\n", start.URL, start.Code, start.Instructions)
	} else {
		fmt.Printf("\nOpening your browser to sign in. If it does not open, visit:\n\n    %s\n\n%s\n", start.URL, start.Instructions)
	}
	openBrowser(start.URL)
	fmt.Print("Waiting for you to finish signing in… (ctrl+c to cancel)\n")
	info, err := client.Do(ctx, c, protocol.ProviderLoginWait, protocol.LoginWaitParams{ID: start.ID})
	if err != nil {
		return "", err
	}
	fmt.Printf("✓ %s connected (%s)\n", info.Name, info.Account)
	return chosen.id, nil
}

// openBrowser makes a best-effort attempt to open url; failures are silent.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

func logoutFlow(ctx context.Context, c *client.Client, want string) error {
	r, err := client.Do(ctx, c, protocol.ProviderList, protocol.None{})
	if err != nil {
		return err
	}
	var items []pick
	for _, p := range r.Providers {
		if p.Connected {
			items = append(items, pick{id: p.ID, label: p.Name, hint: p.Account})
		}
	}
	if len(items) == 0 {
		return errors.New("no providers connected")
	}
	var chosen pick
	if want != "" {
		for _, it := range items {
			if strings.EqualFold(it.id, want) || strings.EqualFold(it.label, want) {
				chosen = it
			}
		}
		if chosen.id == "" {
			return fmt.Errorf("no stored credential for %q", want)
		}
	} else {
		chosen, err = selectFrom("Remove credential", items)
		if err != nil {
			return err
		}
	}
	if _, err := client.Do(ctx, c, protocol.ProviderDisconnect, protocol.ProviderRef{Provider: chosen.id}); err != nil {
		return err
	}
	fmt.Printf("signed out of %s\n", chosen.label)
	return nil
}

// --- tiny interactive helpers ---

type pick struct{ id, label, hint string }

// selectFrom prints a numbered, filterable list and returns the choice.
func selectFrom(title string, items []pick) (pick, error) {
	filter := ""
	for {
		var shown []pick
		for _, it := range items {
			if filter == "" || strings.Contains(strings.ToLower(it.id+" "+it.label), strings.ToLower(filter)) {
				shown = append(shown, it)
			}
		}
		fmt.Printf("\n%s", title)
		if filter != "" {
			fmt.Printf(" (filter: %q)", filter)
		}
		fmt.Println()
		limit := 15
		for i, it := range shown {
			if i >= limit {
				fmt.Printf("  … %d more; type to filter\n", len(shown)-limit)
				break
			}
			hint := ""
			if it.hint != "" {
				hint = "  — " + it.hint
			}
			fmt.Printf("  %2d) %-22s%s\n", i+1, it.label, hint)
		}
		line, err := readLine("Number, or text to filter (empty to cancel): ")
		if err != nil {
			return pick{}, err
		}
		if line == "" {
			return pick{}, errors.New("cancelled")
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(shown) && n <= limit {
			return shown[n-1], nil
		}
		for _, it := range shown {
			if strings.EqualFold(it.id, line) || strings.EqualFold(it.label, line) {
				return it, nil
			}
		}
		if len(shown) == 1 && filter != "" {
			return shown[0], nil
		}
		filter = line
	}
}

func readLine(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func shortHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
