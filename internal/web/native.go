// Package web supervises the native Rust browser API. HTTP, WebSocket,
// session, sheet, replay and query handlers are implemented under api/.
package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/nativeapi"
)

const DefaultPort = 4999

// Delivery is an already-routed message for the independent agent runtime.
// It is never a browser RPC request and cannot change runtime authority.
type Delivery struct {
	ID          string   `json:"id"`
	RequestID   string   `json:"request_id"`
	Channel     string   `json:"channel"`
	Targets     []string `json:"targets"`
	Names       []string `json:"names"`
	Text        string   `json:"text"`
	MessageKind string   `json:"message_kind"`
}
type Options struct {
	Port     int
	Hosts    []string
	DataDir  string
	Snapshot func() any
	Deliver  func(context.Context, Delivery) error
}
type Status struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	Error   string `json:"error"`
}
type result struct {
	Kind    string `json:"kind"`
	ID      uint64 `json:"id"`
	Status  Status `json:"status"`
	OpenURL string `json:"open_url"`
	Error   string `json:"error"`
}
type process struct {
	command *exec.Cmd
	output  chan []byte
	facts   chan []byte
	done    chan struct{}
	ready   chan struct{}
	cancel  context.CancelFunc
	workers chan struct{}
}
type Server struct {
	o         Options
	operation sync.Mutex
	mu        sync.Mutex
	process   *process
	next      uint64
	pending   map[uint64]chan result
	status    Status
}

func New(o Options) *Server {
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	return &Server{o: o, pending: map[uint64]chan result{}, status: Status{URL: fmt.Sprintf("http://127.0.0.1:%d/", o.Port)}}
}
func (s *Server) URL() string    { return fmt.Sprintf("http://127.0.0.1:%d/", s.o.Port) }
func (s *Server) Status() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.status }
func (s *Server) Enable(ctx context.Context) error {
	s.operation.Lock()
	defer s.operation.Unlock()
	if err := s.start(ctx); err != nil {
		return err
	}
	_, err := s.call("enable")
	return err
}
func (s *Server) Disable() { s.operation.Lock(); defer s.operation.Unlock(); _, _ = s.call("disable") }
func (s *Server) OpenURL() (string, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	reply, err := s.call("open")
	return reply.OpenURL, err
}
func (s *Server) Close() {
	s.operation.Lock()
	defer s.operation.Unlock()
	s.mu.Lock()
	owned := s.process
	s.mu.Unlock()
	if owned != nil {
		owned.cancel()
		select {
		case <-owned.done:
		case <-time.After(4 * time.Second):
			_ = owned.command.Process.Kill()
			<-owned.done
		}
	}
}
func (s *Server) start(ctx context.Context) error {
	s.mu.Lock()
	current := s.process
	s.mu.Unlock()
	if current != nil {
		select {
		case <-current.done:
		default:
			return nil
		}
	}
	path, cleanup, err := nativeapi.Prepare()
	if err != nil {
		return err
	}
	defer cleanup()
	command := exec.Command(path, "--data-dir", s.o.DataDir, "--port", strconv.Itoa(s.o.Port), "--hosts", strings.Join(s.o.Hosts, ","), "--parent-pid", strconv.Itoa(os.Getpid()))
	command.Stderr = os.Stderr
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	owner := &process{command: command, output: make(chan []byte, 16), facts: make(chan []byte, 1), done: make(chan struct{}), ready: make(chan struct{}), cancel: cancel, workers: make(chan struct{}, 16)}
	if err := command.Start(); err != nil {
		cancel()
		input.Close()
		output.Close()
		return err
	}
	cleanup()
	s.mu.Lock()
	s.process = owner
	s.mu.Unlock()
	go s.write(runCtx, owner, input)
	go s.read(runCtx, owner, output)
	go s.snapshots(runCtx, owner)
	go s.wait(runCtx, owner)
	select {
	case <-owner.ready:
		if s.o.Snapshot != nil {
			if !s.send(owner, map[string]any{"kind": "snapshot", "facts": s.o.Snapshot()}) {
				cancel()
				return errors.New("initial runtime state queue is full")
			}
		}
		return nil
	case <-owner.done:
		return errors.New("native API exited before readiness")
	case <-time.After(5 * time.Second):
		cancel()
		return errors.New("native API did not become ready")
	}
}
func (s *Server) wait(ctx context.Context, owner *process) {
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = owner.command.Process.Signal(os.Interrupt)
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				_ = owner.command.Process.Kill()
			}
		case <-stopped:
		}
	}()
	err := owner.command.Wait()
	close(stopped)
	owner.cancel()
	s.mu.Lock()
	if s.process == owner {
		s.status.Enabled = false
		if err != nil {
			s.status.Error = "native API stopped: " + err.Error()
		}
	}
	s.mu.Unlock()
	close(owner.done)
}
func (s *Server) call(op string) (result, error) {
	s.mu.Lock()
	owner := s.process
	if owner == nil {
		s.mu.Unlock()
		return result{}, errors.New("the web UI is disabled")
	}
	s.next++
	id := s.next
	response := make(chan result, 1)
	s.pending[id] = response
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, id); s.mu.Unlock() }()
	raw, _ := json.Marshal(map[string]any{"kind": "control", "id": id, "op": op})
	select {
	case owner.output <- raw:
	case <-owner.done:
		return result{}, errors.New("native API is unavailable")
	case <-time.After(5 * time.Second):
		return result{}, errors.New("native API control queue is full")
	}
	select {
	case reply := <-response:
		if reply.Error != "" {
			return reply, errors.New(reply.Error)
		}
		return reply, nil
	case <-owner.done:
		return result{}, errors.New("native API stopped")
	case <-time.After(5 * time.Second):
		return result{}, errors.New("native API control timed out")
	}
}
func (s *Server) write(ctx context.Context, owner *process, input io.WriteCloser) {
	defer input.Close()
	for {
		var raw []byte
		// Control replies and admitted deliveries take priority over snapshots.
		select {
		case raw = <-owner.output:
		default:
			select {
			case raw = <-owner.output:
			case raw = <-owner.facts:
			case <-ctx.Done():
				return
			}
		}
		if len(raw) == 0 {
			continue
		}
		raw = append(raw, '\n')
		if _, err := input.Write(raw); err != nil {
			owner.cancel()
			return
		}
	}
}
func (s *Server) read(ctx context.Context, owner *process, output io.ReadCloser) {
	defer output.Close()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	ready := false
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		var envelope struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &envelope) != nil {
			owner.cancel()
			return
		}
		switch envelope.Kind {
		case "ready":
			if ready {
				owner.cancel()
				return
			}
			ready = true
			close(owner.ready)
		case "control_result":
			var reply result
			if json.Unmarshal(raw, &reply) != nil {
				owner.cancel()
				return
			}
			s.mu.Lock()
			pending := s.pending[reply.ID]
			if s.process == owner {
				s.status = reply.Status
			}
			s.mu.Unlock()
			if pending != nil {
				select {
				case pending <- reply:
				default:
				}
			}
		case "delivery":
			s.delivery(ctx, owner, raw)
		default:
			owner.cancel()
			return
		}
	}
	owner.cancel()
}
func (s *Server) send(owner *process, value any) bool {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 16<<20 {
		return false
	}
	select {
	case owner.output <- raw:
		return true
	default:
		return false
	}
}
func (s *Server) delivery(ctx context.Context, owner *process, raw []byte) {
	var message Delivery
	if len(raw) > 4<<20 || json.Unmarshal(raw, &message) != nil {
		return
	}
	select {
	case owner.workers <- struct{}{}:
	default:
		s.send(owner, map[string]any{"kind": "delivery_result", "id": message.ID, "error": "agent input queue is full"})
		return
	}
	go func() {
		defer func() { <-owner.workers }()
		var err error
		if s.o.Deliver == nil {
			err = errors.New("agent runtime delivery is unavailable")
		} else {
			err = s.o.Deliver(ctx, message)
		}
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		if !s.send(owner, map[string]any{"kind": "delivery_result", "id": message.ID, "error": detail}) {
			owner.cancel()
		}
	}()
}
func (s *Server) snapshots(ctx context.Context, owner *process) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.o.Snapshot != nil {
			raw, err := json.Marshal(map[string]any{"kind": "snapshot", "facts": s.o.Snapshot()})
			if err == nil && len(raw) <= 16<<20 {
				select {
				case owner.facts <- raw:
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Notify carries a bounded transient domain event to the Rust subscription hub.
func (s *Server) Notify(value any) {
	s.mu.Lock()
	owner := s.process
	s.mu.Unlock()
	if owner == nil {
		return
	}
	raw, err := json.Marshal(map[string]any{"kind": "notification", "value": value})
	if err != nil || len(raw) > 1<<16 {
		return
	}
	select {
	case owner.output <- raw:
	default:
	}
}
