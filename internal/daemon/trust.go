package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/eventlog"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Project trust (PRD §10.6): what the human has trusted, the prompt that asks,
// and what changes when they answer.

type trustStore struct {
	log *eventlog.Log
	mu  sync.RWMutex
	m   map[string]string // dir → hash
}

func (t *trustStore) load(ctx context.Context) error {
	m, err := t.log.Trust(ctx)
	t.m = m
	return err
}

func (t *trustStore) Trusted(dir, hash string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.m[dir] == hash
}

func (t *trustStore) set(ctx context.Context, dir, hash string) error {
	if err := t.log.SetTrust(ctx, dir, hash); err != nil {
		return err
	}
	t.mu.Lock()
	t.m[dir] = hash
	t.mu.Unlock()
	return nil
}

// maybeTrustPrompt raises a trust prompt for a channel whose project layer
// is pending. It is a channel-scoped prompt with long timeouts; the
// channel keeps running on global config meanwhile.
func (d *Daemon) maybeTrustPrompt(s *agent.Channel) {
	cfg := s.Config()
	dir := cfg.Dir
	if dir == "" {
		dir = s.Dir()
	}
	if !cfg.TrustPending {
		return
	}
	d.trustMu.Lock()
	if _, busy := d.trustPrompts[dir]; busy {
		d.trustMu.Unlock()
		return
	}
	id := agent.NewID("t")
	d.trustPrompts[dir] = id
	d.trustMu.Unlock()
	opened := make(chan struct{})
	go func() {
		input, _ := json.Marshal(map[string]any{"dir": dir, "hash": cfg.TrustHash, "files": cfg.TrustFiles})
		ans := d.esc.Request(context.Background(), protocol.PromptInfo{
			ID: id, Channel: s.ID, ChannelName: s.Name(), Kind: protocol.PromptTrust, Input: input,
			Question: fmt.Sprintf("Trust the project configuration in %s? It can define MCP servers (with ${env:…} values from the daemon's environment), policy, roles, skills and AGENTS.md, and it can change the sandbox, the hosts fetched without asking and the environment variables passed to commands.", dir),
			Options:  []string{"trust", "skip"},
		}, func() { close(opened) }) // published before a caller can change the channel directory
		d.trustMu.Lock()
		delete(d.trustPrompts, dir)
		d.trustMu.Unlock()
		if ans.Value == protocol.AnswerAllow || ans.Value == protocol.AnswerAllowAlways {
			_ = d.Trust(context.Background(), dir, cfg.TrustHash, true)
		}
	}()
	<-opened
}

// withdrawTrustPrompts settles the open trust prompts of one channel (every
// channel when id is empty) as declined: the channel moved directory, was
// archived, or the daemon is closing, and a prompt nobody can answer any
// more would otherwise sit in the list, and its goroutine in memory, for
// ever. A trust prompt has no answer timer (trust.go, escalation.Request).
func (d *Daemon) withdrawTrustPrompts(id, reason string) {
	if id != "" {
		for _, p := range d.esc.Pending(id) {
			if p.Kind == protocol.PromptTrust {
				_ = d.esc.Resolve(p.ID, reason, escalation.Answer{Value: protocol.AnswerDeny})
			}
		}
		return
	}
	d.trustMu.RLock()
	ids := make([]string, 0, len(d.trustPrompts))
	for _, pid := range d.trustPrompts {
		ids = append(ids, pid)
	}
	d.trustMu.RUnlock()
	for _, pid := range ids {
		_ = d.esc.Resolve(pid, reason, escalation.Answer{Value: protocol.AnswerDeny})
	}
}

// Trust records a decision and reloads config for channels in dir. The
// directory is normalised and the hash recomputed from what is on disk:
// a client says which directory it means and whether it trusts what it
// was shown; the daemon decides what that content is.
func (d *Daemon) Trust(ctx context.Context, dir, hash string, trust bool) error {
	dir, _ = filepath.Abs(dir)
	dir = filepath.Clean(dir)
	if trust {
		_, current, err := config.ProjectHash(dir)
		if err != nil {
			return err
		}
		if current == "" {
			return fmt.Errorf("%s has no project configuration to trust", dir)
		}
		if hash != current {
			return fmt.Errorf("%w: the project configuration in %s changed since it was shown; look again before trusting it", errTrustChanged, dir)
		}
		if err := d.trust.set(ctx, dir, current); err != nil {
			return err
		}
	}
	// resolve any open trust prompt for this dir
	d.trustMu.RLock()
	pid := d.trustPrompts[dir]
	d.trustMu.RUnlock()
	if pid != "" {
		ans := protocol.AnswerDeny
		if trust {
			ans = protocol.AnswerAllow
		}
		// Settle the open prompt even if a client had claimed it: the
		// decision has been made.
		_ = d.esc.Resolve(pid, "trust.reply", escalation.Answer{Value: ans})
	}
	if !trust {
		return nil
	}
	var ss []*agent.Channel
	for _, s := range d.channelList() { // (a channel is never called with a daemon lock held)
		if s.Dir() == dir {
			ss = append(ss, s)
		}
	}
	for _, s := range ss {
		cfg, err := config.Load(dir, d.trust)
		if err != nil {
			return err
		}
		s.SetConfig(cfg)
	}
	return nil
}

// TrustStatus reports whether dir's project layer is pending.
func (d *Daemon) TrustStatus(dir string) (protocol.TrustStatusResult, error) {
	dir, _ = filepath.Abs(dir)
	files, hash, err := config.ProjectHash(dir)
	if err != nil {
		return protocol.TrustStatusResult{}, err
	}
	return protocol.TrustStatusResult{Dir: dir, Pending: len(files) > 0 && !d.trust.Trusted(dir, hash), Hash: hash, Files: files}, nil
}
