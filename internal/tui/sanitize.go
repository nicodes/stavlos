package tui

import (
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
)

// Everything the daemon hands the TUI that a model or a tool wrote passes
// through textsafe on the way in: a label, a question, a channel title, a
// line of transcript. The terminal then only ever draws text the TUI
// styled itself. The permission dialog keeps the hidden bytes visible
// (textsafe.Visible) so the human sees that a command tried to hide
// something.

func cleanAgents(agents []protocol.AgentInfo) []protocol.AgentInfo {
	for i := range agents {
		a := &agents[i]
		a.Label, a.Archetype, a.Summary, a.Status, a.LastError = textsafe.Clean(a.Label), textsafe.Clean(a.Archetype), textsafe.Clean(a.Summary), textsafe.Clean(a.Status), textsafe.Clean(a.LastError)
		for j := range a.Monitors {
			m := &a.Monitors[j]
			m.Label, m.Spec, m.Progress = textsafe.Clean(m.Label), textsafe.Clean(m.Spec), textsafe.Clean(m.Progress)
		}
		for j := range a.Todos {
			a.Todos[j].Text = textsafe.Clean(a.Todos[j].Text)
		}
		for j := range a.MCP {
			a.MCP[j].Error = textsafe.Clean(a.MCP[j].Error)
		}
	}
	return agents
}

func cleanPrompt(p *protocol.PromptInfo) {
	p.Question, p.Dir, p.Prefix = textsafe.Clean(p.Question), textsafe.Clean(p.Dir), textsafe.Clean(p.Prefix)
	for i := range p.Options {
		p.Options[i] = textsafe.Clean(p.Options[i])
	}
	for i := range p.Questions {
		q := &p.Questions[i]
		q.Question = textsafe.Clean(q.Question)
		for j := range q.Options {
			q.Options[j].Label, q.Options[j].Description = textsafe.Clean(q.Options[j].Label), textsafe.Clean(q.Options[j].Description)
		}
	}
	// p.Input is decoded by the renderer, which shows controls visibly.
}

func cleanChannel(s protocol.ChannelInfo) protocol.ChannelInfo {
	s.Title, s.Dir = textsafe.Clean(s.Title), textsafe.Clean(s.Dir)
	for i := range s.Dirs {
		s.Dirs[i].Path = textsafe.Clean(s.Dirs[i].Path)
	}
	return s
}

func cleanChannels(ss []protocol.ChannelInfo) []protocol.ChannelInfo {
	for i := range ss {
		ss[i] = cleanChannel(ss[i])
	}
	return ss
}
