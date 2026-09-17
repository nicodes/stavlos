package discord

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/config"
)

func serviceConfig(t *testing.T, enabled bool) config.Discord {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", dir)
	cfg := config.Discord{Enabled: enabled, Token: "test-token", Guild: "123", Category: "stavlos", Approvers: []string{"456"}, Dirs: []string{t.TempDir()}}
	b, _ := json.Marshal(config.File{Discord: &cfg})
	if err := os.WriteFile(filepath.Join(dir, "stavlos.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func fakeConnected(ctx context.Context) *Bridge {
	l := &link{ctx: ctx}
	return &Bridge{gatewayConnected: true, botName: "Stavlos", guildName: "Test Server", live: l, workers: map[string]*worker{"one": {ready: l}}}
}

func TestServiceLifecycleAndAutoconnect(t *testing.T) {
	serviceConfig(t, false)
	var runs, active, maxActive atomic.Int32
	runner := func(ctx context.Context, cfg config.Discord, publish func(*Bridge)) error {
		if cfg.Token != "test-token" {
			t.Error("token was not resolved")
		}
		n := active.Add(1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		runs.Add(1)
		defer active.Add(-1)
		publish(fakeConnected(ctx))
		<-ctx.Done()
		return ctx.Err()
	}
	newService := func() *Service {
		s := NewService(context.Background(), "/unused", t.TempDir())
		s.run = runner
		s.retry = time.Hour
		t.Cleanup(s.Close)
		return s
	}
	s := newService()
	s.Start()
	if st := s.Status(); st.Enabled || st.State != "disconnected" || !st.Configured {
		t.Fatalf("startup: %+v", st)
	}
	if _, err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "service connected", func() bool { return s.Status().State == "connected" })
	if st := s.Status(); st.Bot != "Stavlos" || st.GuildName != "Test Server" || st.Channels != 1 {
		t.Fatalf("status: %+v", st)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Connect(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if runs.Load() != 1 || maxActive.Load() != 1 {
		t.Fatal("duplicate integration started")
	}
	if _, err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "service stopped", func() bool { return s.Status().State == "disconnected" && active.Load() == 0 })
	cfg, err := config.LoadDiscord()
	if err != nil || cfg.Enabled {
		t.Fatalf("disconnect not persisted: %v", err)
	}
	if _, err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "service reconnected", func() bool { return runs.Load() == 2 && s.Status().State == "connected" })
	s.Close()
	cfg, err = config.LoadDiscord()
	if err != nil || !cfg.Enabled {
		t.Fatal("daemon shutdown cleared autoconnect")
	}
	s2 := newService()
	s2.Start()
	eventually(t, "autoconnect after daemon restart", func() bool { return runs.Load() == 3 && s2.Status().State == "connected" })
	if _, err := s2.Disconnect(); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3 := newService()
	s3.Start()
	if s3.Status().Enabled || runs.Load() != 3 {
		t.Fatal("disabled integration reconnected")
	}
}

func TestServiceErrorsRetryAndMissingConfiguration(t *testing.T) {
	serviceConfig(t, true)
	s := NewService(context.Background(), "/unused", t.TempDir())
	defer s.Close()
	s.retry = time.Hour
	var calls atomic.Int32
	s.run = func(ctx context.Context, _ config.Discord, publish func(*Bridge)) error {
		if calls.Add(1) == 1 {
			return errors.New("discord rejected the bot token (HTTP 401)")
		}
		publish(fakeConnected(ctx))
		<-ctx.Done()
		return ctx.Err()
	}
	s.Start()
	eventually(t, "connection error visible", func() bool { return s.Status().State == "error" })
	if st := s.Status(); !st.Enabled || !strings.Contains(st.Error, "401") {
		t.Fatalf("error status: %+v", st)
	}
	if _, err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "retry connected", func() bool { return s.Status().State == "connected" })
	s.mu.Lock()
	b := s.bridge
	s.mu.Unlock()
	b.gatewayState(false)
	if s.Status().State != "reconnecting" {
		t.Fatal("gateway loss not reflected")
	}
	s.Close()
	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	s2 := NewService(context.Background(), "/unused", t.TempDir())
	defer s2.Close()
	s2.Start()
	if _, err := s2.Connect(); err == nil || !strings.Contains(err.Error(), "discord block") {
		t.Fatalf("missing config: %v", err)
	}
	if _, err := s2.Disconnect(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedAndStandaloneShareLock(t *testing.T) {
	cfg := serviceConfig(t, false)
	dir := t.TempDir()
	state, lock, err := lockBridge(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, duplicate, err := lockBridge(dir, cfg); !errors.Is(err, errBridgeRunning) {
		if duplicate != nil {
			duplicate.Close()
		}
		t.Fatalf("duplicate lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	next, lock2, err := lockBridge(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer lock2.Close()
	if state != next {
		t.Fatal("migration changed the prompt message index")
	}
}

func TestDisconnectCancelsConnectingService(t *testing.T) {
	serviceConfig(t, false)
	s := NewService(context.Background(), "/unused", t.TempDir())
	defer s.Close()
	entered := make(chan struct{})
	s.run = func(ctx context.Context, _ config.Discord, _ func(*Bridge)) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	<-entered
	if s.Status().State != "connecting" {
		t.Fatal("handshake not represented as connecting")
	}
	if _, err := s.Disconnect(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "connecting service cancelled", func() bool { return s.Status().State == "disconnected" })
	cfg, err := config.LoadDiscord()
	if err != nil || cfg.Enabled {
		t.Fatal("cancelled connection can autostart again")
	}
}

func TestFailedPersistenceDoesNotPretendToDisconnect(t *testing.T) {
	serviceConfig(t, false)
	s := NewService(context.Background(), "/unused", t.TempDir())
	defer s.Close()
	s.run = func(ctx context.Context, _ config.Discord, publish func(*Bridge)) error {
		publish(fakeConnected(ctx))
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "connected", func() bool { return s.Status().State == "connected" })
	if err := os.WriteFile(s.Status().ConfigPath, []byte(`{invalid`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disconnect(); err == nil {
		t.Fatal("invalid config overwrite succeeded")
	}
	if st := s.Status(); !st.Enabled || st.State != "connected" || st.Error == "" {
		t.Fatalf("lost persistence error: %+v", st)
	}
}
