// Package escalation routes permission and question prompts to clients
// (PRD §7.4): questions and permissions go to all clients immediately. Trust
// starts with interactive clients, escalating to fallback after N; permissions
// take the headless default after M. Claims happen on human engagement.
package escalation

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

// Answer is the outcome of a prompt.
type Answer struct {
	Details   []protocol.QuestionAnswer
	Value     string   // allow | deny | allow_always | free text
	Dir       string   // boundary prompts: the directory to add with allow_always, when the human edited it
	Reason    string   // deny: the human's optional note, passed to the agent
	Answers   []string // question batches: one answer per question, in order
	Client    string
	Defaulted bool
	Withdrawn bool
}

// Config holds the timers and default.
type Config struct {
	ClaimTimeout  time.Duration // N: escalate to fallback after this
	AnswerTimeout time.Duration // M: apply Default after this
	Default       string        // allow | deny
	ClaimExpiry   time.Duration // a claim with no reply expires after this
}

// Sink receives prompt notifications; the daemon fans them to clients.
type Sink interface {
	Notify(n protocol.PromptNotification, tiers []protocol.Tier)
}

type pending struct {
	info      protocol.PromptInfo
	answer    chan Answer
	claimedAt time.Time
	done      bool
}

// Manager owns pending prompts.
type Manager struct {
	cfg  Config
	sink Sink
	mu   sync.Mutex
	pend map[string]*pending
}

// New creates a manager.
func New(cfg Config, sink Sink) *Manager {
	if cfg.ClaimExpiry == 0 {
		cfg.ClaimExpiry = 2 * time.Minute
	}
	if cfg.Default == "" {
		cfg.Default = "deny"
	}
	return &Manager{cfg: cfg, sink: sink, pend: map[string]*pending{}}
}

// SetConfig applies timer/default edits to new requests; existing prompts keep
// the timers they were opened with.
func (m *Manager) SetConfig(cfg Config) {
	if cfg.Default == "" {
		cfg.Default = "deny"
	}
	if cfg.ClaimExpiry == 0 {
		cfg.ClaimExpiry = 2 * time.Minute
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// Errors.
var (
	ErrNotFound = errors.New("prompt not found or already answered")
	ErrClaimed  = errors.New("prompt is claimed by another client")
	ErrLate     = errors.New("prompt was already resolved; late answer rejected")
)

// Request opens a prompt and blocks until answered, defaulted, or ctx is
// cancelled (in which case the prompt is withdrawn everywhere). opened, if
// set, runs once the prompt can be listed and answered, before any answer
// is taken.
func (m *Manager) Request(ctx context.Context, info protocol.PromptInfo, opened func()) Answer {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	info.Created = time.Now().UTC().Format(time.RFC3339Nano)
	// Chat cards are immediately actionable from either client, including
	// clients reconnecting via Pending. Trust is here too: a project whose
	// configuration is not trusted runs on the global layer alone, losing
	// its roles, skills and commands, and nothing says so until someone
	// answers. Waiting out the claim timeout before the bridge hears about
	// it only makes that quiet gap longer.
	if info.Kind == protocol.PromptQuestion || info.Kind == protocol.PromptPermission || info.Kind == protocol.PromptTrust {
		info.Escalated = true
	}
	p := &pending{info: info, answer: make(chan Answer, 1)}
	m.mu.Lock()
	m.pend[info.ID] = p
	m.mu.Unlock()
	m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: info}, m.tiersFor(info))
	// The prompt can be listed and answered from here on: opened runs now
	// (the caller logs the request), and no answer is taken before it
	// returns, so what it logs always precedes the answer.
	if opened != nil {
		opened()
	}

	claimT := time.NewTimer(cfg.ClaimTimeout)
	if info.Escalated {
		claimT.Stop()
	}
	answerT := time.NewTimer(cfg.AnswerTimeout)
	if info.Kind == protocol.PromptQuestion || info.Kind == protocol.PromptTrust {
		// No sensible default for either: a question waits until answered
		// or withdrawn, and trusting a project is never decided by a timer
		// (it would also be asked again at the next resume).
		answerT.Stop()
	}
	defer claimT.Stop()
	defer answerT.Stop()
	expiry := time.NewTicker(10 * time.Second)
	defer expiry.Stop()

	for {
		select {
		case a := <-p.answer:
			return a
		case <-claimT.C:
			m.mu.Lock()
			if p.info.ClaimedBy == "" && !p.done {
				p.info.Escalated = true
			}
			esc := p.info.Escalated
			info := p.info
			m.mu.Unlock()
			if esc {
				m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionEscalated, Prompt: info}, []protocol.Tier{protocol.TierInteractive, protocol.TierFallback})
			}
		case <-expiry.C:
			// expire stale claims so the prompt can be re-claimed
			m.mu.Lock()
			if p.info.ClaimedBy != "" && time.Since(p.claimedAt) > cfg.ClaimExpiry {
				p.info.ClaimedBy = ""
				info := p.info
				m.mu.Unlock()
				m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: info}, m.tiersFor(info))
				continue
			}
			m.mu.Unlock()
		case <-answerT.C:
			a := Answer{Value: cfg.Default, Defaulted: true}
			if m.finish(info.ID, a) {
				m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionDefaulted, Prompt: m.snapshot(info.ID, p)}, m.tiersFor(p.info))
			} else {
				return <-p.answer
			}
			return a
		case <-ctx.Done():
			a := Answer{Withdrawn: true}
			if m.finish(info.ID, a) {
				m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionWithdrawn, Prompt: m.snapshot(info.ID, p)}, m.tiersFor(p.info))
			} else {
				return <-p.answer // an accepted answer wins a simultaneous cancellation
			}
			return a
		}
	}
}

func (m *Manager) tiersFor(info protocol.PromptInfo) []protocol.Tier {
	if info.Escalated {
		return []protocol.Tier{protocol.TierInteractive, protocol.TierFallback}
	}
	return []protocol.Tier{protocol.TierInteractive}
}

func (m *Manager) snapshot(id string, p *pending) protocol.PromptInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return p.info
}

// finish marks a prompt done; returns false if it already was.
func (m *Manager) finish(id string, a Answer) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pend[id]
	if !ok || p.done {
		return false
	}
	p.done = true
	delete(m.pend, id)
	select {
	case p.answer <- a:
	default:
	}
	return true
}

// ReleaseClient gives up every claim client holds, because it has gone: the
// prompts are announced again so another client (or the same human's,
// reconnected under a new id) can take them at once rather than after
// ClaimExpiry.
func (m *Manager) ReleaseClient(client string) {
	m.mu.Lock()
	var freed []protocol.PromptInfo
	for _, p := range m.pend {
		if p.info.ClaimedBy == client && !p.done {
			p.info.ClaimedBy = ""
			freed = append(freed, p.info)
		}
	}
	m.mu.Unlock()
	for _, info := range freed {
		m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: info}, m.tiersFor(info))
	}
}

// Claim marks a prompt as being answered by client (a human engaged).
func (m *Manager) Claim(id, client string) error {
	m.mu.Lock()
	p, ok := m.pend[id]
	if !ok || p.done {
		m.mu.Unlock()
		return ErrLate
	}
	if p.info.ClaimedBy != "" && p.info.ClaimedBy != client {
		m.mu.Unlock()
		return ErrClaimed
	}
	p.info.ClaimedBy = client
	p.claimedAt = time.Now()
	info := p.info
	m.mu.Unlock()
	m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionClaimed, Prompt: info}, m.tiersFor(info))
	return nil
}

// Reply answers a prompt. Only the claimant may reply; an unclaimed prompt
// is implicitly claimed by the replier. a carries the value and whatever
// the prompt's kind needs with it (a directory, a reason, a question
// batch's answers); its Client is set to client.
func (m *Manager) Reply(id, client string, a Answer) error {
	m.mu.Lock()
	p, ok := m.pend[id]
	if !ok || p.done {
		m.mu.Unlock()
		return ErrLate
	}
	if p.info.ClaimedBy != "" && p.info.ClaimedBy != client {
		m.mu.Unlock()
		return ErrClaimed
	}
	if p.info.Kind == protocol.PromptQuestion && p.info.QuestionNumber > 0 && !singleQuestionAnswer(a) {
		m.mu.Unlock()
		return errors.New("provide exactly one nonempty answer for this question")
	}
	p.info.ClaimedBy = client
	info := p.info
	m.mu.Unlock()
	return m.answer(id, info, client, a)
}

func singleQuestionAnswer(a Answer) bool {
	if len(a.Answers) == 1 {
		return strings.TrimSpace(a.Answers[0]) != ""
	}
	return len(a.Answers) == 0 && a.Value != protocol.AnswerAnswered && strings.TrimSpace(a.Value) != ""
}

// Resolve answers a prompt whatever claims it: the daemon settling a prompt
// through another path (a trust decision made with trust.reply).
func (m *Manager) Resolve(id, client string, a Answer) error {
	m.mu.Lock()
	p, ok := m.pend[id]
	if !ok || p.done {
		m.mu.Unlock()
		return ErrLate
	}
	info := p.info
	m.mu.Unlock()
	return m.answer(id, info, client, a)
}

func (m *Manager) answer(id string, info protocol.PromptInfo, client string, a Answer) error {
	a.Client = client
	if !m.finish(id, a) {
		return ErrLate
	}
	m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: info}, m.tiersFor(info))
	return nil
}

// AnswerAll answers every open prompt of one kind in a channel, ignoring
// claims (used when a channel switches to yolo: waiting permissions are
// allowed on the spot). Returns how many were answered.
func (m *Manager) AnswerAll(channel string, kind protocol.PromptKind, answer, client string) int {
	return m.AnswerWhere(channel, kind, answer, client, nil)
}

// AnswerWhere answers the channel's open prompts of one kind that keep
// admits (nil = all of them).
func (m *Manager) AnswerWhere(channel string, kind protocol.PromptKind, answer, client string, keep func(protocol.PromptInfo) bool) int {
	m.mu.Lock()
	var ids []string
	for id, p := range m.pend {
		if !p.done && p.info.Channel == channel && p.info.Kind == kind && (keep == nil || keep(p.info)) {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	n := 0
	for _, id := range ids {
		m.mu.Lock()
		p, ok := m.pend[id]
		if !ok || p.done {
			m.mu.Unlock()
			continue
		}
		p.info.ClaimedBy = client
		info := p.info
		m.mu.Unlock()
		if !m.finish(id, Answer{Value: answer, Client: client}) {
			continue
		}
		m.sink.Notify(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: info}, m.tiersFor(info))
		n++
	}
	return n
}

// Pending lists open prompts, optionally filtered by channel.
func (m *Manager) Pending(channel string) []protocol.PromptInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []protocol.PromptInfo
	for _, p := range m.pend {
		if channel == "" || p.info.Channel == channel {
			out = append(out, p.info)
		}
	}
	slices.SortFunc(out, func(a, b protocol.PromptInfo) int {
		ta, _ := time.Parse(time.RFC3339Nano, a.Created)
		tb, _ := time.Parse(time.RFC3339Nano, b.Created)
		if n := ta.Compare(tb); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}
