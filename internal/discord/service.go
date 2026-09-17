package discord

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Service supervises one integration for the daemon's lifetime. Connect and
// Disconnect persist intent and return promptly; network work is asynchronous.
type Service struct {
	ctx             context.Context
	cancel          context.CancelFunc
	socket, data    string
	op              sync.Mutex // commands/config writes and startup/close
	started, closed bool
	mu              sync.Mutex
	status          protocol.DiscordStatus
	bridge          *Bridge
	runCancel       context.CancelFunc
	wake            chan struct{}
	done            chan struct{}
	run             func(context.Context, config.Discord, func(*Bridge)) error
	retry           time.Duration
}

// NewService constructs the integration. Start is called after the daemon's
// socket is listening; no Discord connection is made by the constructor.
func NewService(ctx context.Context, socket, data string) *Service {
	ctx, cancel := context.WithCancel(ctx)
	s := &Service{ctx: ctx, cancel: cancel, socket: socket, data: data, wake: make(chan struct{}, 1), done: make(chan struct{}), retry: 5 * time.Second,
		status: protocol.DiscordStatus{State: "disconnected", ConfigPath: filepath.Join(paths.ConfigDir(), "stavlos.json")}}
	s.run = func(ctx context.Context, cfg config.Discord, publish func(*Bridge)) error {
		return runBridge(ctx, cfg, socket, data, publish)
	}
	return s
}

// Start honors the saved enabled setting without making startup depend on Discord.
func (s *Service) Start() {
	s.op.Lock()
	defer s.op.Unlock()
	if s.started || s.closed {
		return
	}
	cfg, err := config.LoadDiscord()
	s.mu.Lock()
	s.configure(cfg)
	if err != nil {
		s.status.State, s.status.Error = "error", err.Error()
	}
	s.mu.Unlock()
	s.startLoop()
}

func (s *Service) configure(cfg *config.Discord) {
	s.status.Configured = cfg != nil
	if cfg != nil {
		s.status.Enabled, s.status.Guild = cfg.Enabled, cfg.Guild
	}
}

func (s *Service) startLoop() {
	if s.started {
		return
	}
	s.started = true
	go s.loop()
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Connect enables automatic startup and starts or retries the integration.
func (s *Service) Connect() (protocol.DiscordStatus, error) {
	s.op.Lock()
	defer s.op.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return s.Status(), errors.New("daemon is stopping")
	}
	cfg, err := config.LoadDiscord()
	if err == nil && cfg == nil {
		err = errors.New("add a discord block to the global stavlos.json first (see docs/discord-setup.md)")
	}
	if err == nil {
		_, err = cfg.Resolve()
	}
	if err == nil {
		err = config.SetDiscordEnabled(true)
	}
	if err != nil {
		s.setError(err)
		return s.Status(), err
	}
	s.mu.Lock()
	s.configure(cfg)
	s.status.Enabled = true
	s.status.Error = ""
	wake := s.runCancel == nil || s.status.State == "stopping"
	if s.runCancel == nil {
		s.status.State = "connecting"
	}
	s.mu.Unlock()
	if wake {
		s.signal()
	}
	s.startLoop()
	return s.Status(), nil
}

// Disconnect disables automatic startup and cancels this integration only.
func (s *Service) Disconnect() (protocol.DiscordStatus, error) {
	s.op.Lock()
	defer s.op.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return s.Status(), errors.New("daemon is stopping")
	}
	if err := config.SetDiscordEnabled(false); err != nil {
		s.setError(err)
		return s.Status(), err
	}
	s.mu.Lock()
	s.status.Enabled, s.status.Error = false, ""
	cancel := s.runCancel
	s.status.State = "disconnected"
	if cancel != nil {
		s.status.State = "stopping"
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.startLoop()
	s.signal()
	return s.Status(), nil
}

// Status returns the latest network state and number of live channel mappings.
func (s *Service) Status() protocol.DiscordStatus {
	s.mu.Lock()
	out, b := s.status, s.bridge
	s.mu.Unlock()
	if b != nil {
		st := b.Status()
		out.Bot, out.GuildName, out.Channels = st.Bot, st.GuildName, st.Channels
		if out.Enabled && out.State != "stopping" {
			out.State = "reconnecting"
			if st.Gateway && st.Daemon {
				out.State = "connected"
			} else if st.Error != "" {
				out.Error = st.Error
			}
		}
	}
	return out
}

func (s *Service) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.State, s.status.Error = "error", err.Error()
}

func (s *Service) loop() {
	defer close(s.done)
	defer func() { s.mu.Lock(); s.bridge, s.runCancel = nil, nil; s.status.State = "disconnected"; s.mu.Unlock() }()
	backoff := s.retry
	for s.ctx.Err() == nil {
		s.mu.Lock()
		enabled := s.status.Enabled
		s.mu.Unlock()
		if !enabled {
			select {
			case <-s.ctx.Done():
				return
			case <-s.wake:
				backoff = s.retry
			}
			continue
		}
		select {
		case <-s.wake:
		default:
		} // consume the command that started this attempt
		started := time.Now()
		cfg, err := config.LoadDiscord()
		if err == nil && cfg == nil {
			err = errors.New("discord configuration is missing")
		}
		var resolved config.Discord
		if err == nil {
			resolved, err = cfg.Resolve()
		}
		if err == nil {
			err = s.attempt(resolved)
		}
		if s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		enabled = s.status.Enabled
		if enabled && err != nil {
			s.status.State, s.status.Error = "error", err.Error()
			if errors.Is(err, context.Canceled) {
				s.status.State, s.status.Error = "connecting", ""
			}
		}
		if !enabled {
			s.status.State, s.status.Error = "disconnected", ""
		}
		s.mu.Unlock()
		if !enabled {
			continue
		}
		if time.Since(started) > 30*time.Second {
			backoff = s.retry
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
			backoff = s.retry
		case <-time.After(backoff):
			backoff = min(max(5*time.Minute, s.retry), backoff*2)
		}
	}
}

func (s *Service) attempt(cfg config.Discord) error {
	ctx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	if !s.status.Enabled {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.runCancel = cancel
	s.status.State, s.status.Guild, s.status.Error = "connecting", cfg.Guild, ""
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.runCancel, s.bridge = nil, nil; s.mu.Unlock() }()
	return s.run(ctx, cfg, func(b *Bridge) {
		s.mu.Lock()
		s.bridge = b
		s.mu.Unlock()
	})
}

// Close stops the service without changing the saved autoconnect preference.
func (s *Service) Close() {
	s.op.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		if !s.started {
			close(s.done)
		}
	}
	s.op.Unlock()
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
	}
}
