package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/pkg/client"
)

type customCommandsMsg struct {
	scope    requestScope
	commands []protocol.CommandInfo
	err      error
}

type customCommandRunMsg struct {
	scope requestScope
	err   error
}

func customCommandsCmd(ctx context.Context, c *client.Client, scope requestScope) tea.Cmd {
	if scope.channel == "" {
		return nil
	}
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.CommandList, protocol.ChannelRef{Channel: scope.channel})
		return customCommandsMsg{scope: scope, commands: res.Commands, err: err}
	})
}

func customCommandRunCmd(ctx context.Context, c *client.Client, scope requestScope, name, agent string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		_, err := client.Do(ctx, c, protocol.CommandRun, protocol.CommandRunParams{Channel: scope.channel, Name: name, Agent: agent})
		return customCommandRunMsg{scope: scope, err: err}
	})
}

func (m *Model) onCustomCommands(msg customCommandsMsg) tea.Cmd {
	if !m.accepts(msg.scope) {
		return nil
	}
	selected := ""
	if matches := m.paletteMatches(m.input.Value()); m.palIdx >= 0 && m.palIdx < len(matches) {
		selected = matches[m.palIdx].Name
	}
	m.customCommands = nil
	if msg.err != nil {
		return m.setStatus("commands: "+msg.err.Error(), true)
	}
	taken := map[string]bool{}
	for _, c := range commands {
		taken[c.Name] = true
		for _, alias := range c.Aliases {
			taken[alias] = true
		}
	}
	for _, c := range msg.commands {
		name := "/" + textsafe.Clean(c.Name)
		if taken[name] {
			continue
		} // built-ins and aliases always win
		taken[name] = true
		m.customCommands = append(m.customCommands, Command{Name: name, Desc: strings.Join(strings.Fields(textsafe.Clean(c.Description)), " "), Direct: true})
	}
	m.palIdx = 0
	for i, c := range m.paletteMatches(m.input.Value()) {
		if c.Name == selected {
			m.palIdx = i
			break
		}
	}
	return nil
}

func (m Model) paletteMatches(input string) []Command {
	registry := append(append([]Command(nil), commands...), m.customCommands...)
	return matchCommands(input, registry)
}
