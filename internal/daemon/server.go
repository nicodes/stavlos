package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Serve accepts protocol connections on the Unix socket until ctx ends.
func (d *Daemon) Serve(ctx context.Context, socket string) error {
	if len(socket) > 100 {
		return fmt.Errorf("socket path %q is too long for a Unix socket (max ~100 bytes); set STAVLOS_SOCKET to a shorter path", socket)
	}
	_ = os.Remove(socket)
	// The socket is born 0600: Listen creates it with the umask applied,
	// and the Chmod below would otherwise leave a moment where anyone could
	// connect. The daemon is single-purpose, so a process-wide umask is fine.
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", socket)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	_ = os.Chmod(socket, 0o600)
	go func() {
		<-ctx.Done()
		ln.Close()
		_ = os.Remove(socket)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.handleConn(ctx, conn)
	}
}

// conn is one client connection. Everything written to it goes through a
// bounded queue drained by one writer goroutine with a write deadline, so
// a client that stops reading never blocks the daemon: the agents' event
// appends, other clients' deliveries and new connections all go on. A
// client whose queue fills is dropped; it reconnects and resubscribes from
// its last sequence number.
type conn struct {
	d    *Daemon
	c    net.Conn
	cl   *client
	out  chan outMsg
	done chan struct{}
	once sync.Once
}

type outMsg struct {
	b         []byte
	droppable bool // a stream delta: the event that follows carries the whole text
}

// Outbound queue bounds. Stream deltas are dropped once the queue is half
// full, so a burst of tokens never costs a client its connection; events,
// replies and prompts are never dropped, and a queue full of them means the
// client is gone for practical purposes.
const outQueue = 1 << 14

// writeTimeout bounds one write to a client (a variable for tests).
var writeTimeout = 10 * time.Second

// maxInFlight bounds the requests one connection may have outstanding;
// maxLine bounds one request (a prompt with a big paste is well under it).
const (
	maxInFlight = 64
	maxLine     = 4 << 20
)

func (d *Daemon) handleConn(ctx context.Context, nc net.Conn) {
	if uc, ok := nc.(*net.UnixConn); ok {
		if raw, err := uc.SyscallConn(); err != nil || !samePeer(raw) {
			log.Printf("refused a connection from another user")
			nc.Close()
			return
		}
	}
	// Requests run under the connection's context: a client that goes away
	// mid-login.wait (or mid-anything) takes its work with it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := &conn{d: d, c: nc, out: make(chan outMsg, outQueue), done: make(chan struct{})}
	c.cl = &client{id: agent.NewID("c"), name: "anonymous", tier: protocol.TierInteractive, subs: map[string]int64{}, send: c.notify}
	d.addClient(c.cl)
	go c.writer()
	defer func() {
		d.removeClient(c.cl.id)
		c.close()
	}()
	inFlight := make(chan struct{}, maxInFlight)
	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 1<<20), maxLine)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			c.reply(nil, nil, &protocol.Error{Code: protocol.ErrParse, Message: err.Error()})
			continue
		}
		inFlight <- struct{}{} // a flood of requests waits its turn instead of spawning without bound
		go func() {
			defer func() { <-inFlight }()
			res, perr := c.dispatch(ctx, req)
			if req.ID != nil {
				c.reply(req.ID, res, perr)
			}
		}()
	}
}

func (c *conn) write(v any) { c.enqueue(v, false) }

// enqueue queues one message without blocking.
func (c *conn) enqueue(v any, droppable bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	if droppable && len(c.out) > outQueue/2 {
		return
	}
	select {
	case c.out <- outMsg{b: append(b, '\n'), droppable: droppable}:
	case <-c.done:
	default:
		log.Printf("client %s (%s) is not reading; dropping it", c.cl.id, c.cl.name)
		c.close()
	}
}

// writer is the only goroutine that writes to the socket. It batches what
// is queued into one flush and gives up on a write that stalls.
func (c *conn) writer() {
	w := bufio.NewWriter(c.c)
	for {
		select {
		case m := <-c.out:
			_ = c.c.SetWriteDeadline(time.Now().Add(writeTimeout))
			_, err := w.Write(m.b)
			if err == nil && len(c.out) == 0 {
				err = w.Flush()
			}
			if err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

// close ends the connection once; the reader then removes the client.
func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.c.Close()
	})
}

func (c *conn) reply(id *json.RawMessage, result any, perr *protocol.Error) {
	r := protocol.Response{JSONRPC: "2.0", ID: id, Error: perr}
	if perr == nil {
		b, err := json.Marshal(result)
		if err != nil {
			r.Error = &protocol.Error{Code: protocol.ErrInternal, Message: err.Error()}
		} else {
			r.Result = b
		}
	}
	c.write(r)
}

func (c *conn) notify(method string, params json.RawMessage) {
	c.enqueue(protocol.Response{JSONRPC: "2.0", Method: method, Params: params}, method == protocol.NStream)
}

func (c *conn) dispatch(ctx context.Context, req protocol.Request) (any, *protocol.Error) {
	if req.V != protocol.Version {
		return nil, &protocol.Error{Code: protocol.ErrVersion, Message: fmt.Sprintf("protocol version %d not served; this daemon serves %d", req.V, protocol.Version)}
	}
	h, ok := handlers[req.Method]
	if !ok {
		return nil, &protocol.Error{Code: protocol.ErrMethodNotFound, Message: "unknown method " + req.Method}
	}
	res, err := h(ctx, c, req)
	if err != nil {
		return nil, toProtocolError(err)
	}
	return res, nil
}

func tree(s *agent.Channel) []protocol.AgentInfo {
	var out []protocol.AgentInfo
	for _, a := range s.Agents() {
		out = append(out, a.Info())
	}
	return out
}

// subscribe replays from the requested offset, then goes live. Delivery is
// deduplicated per subscription by seq, so the handover cannot double-send.

// rememberModel makes the first model a user picks the global default when
// no config layer has set one, so the next channel does not start empty.
func (d *Daemon) rememberModel(s *agent.Channel, modelID string) {
	if s.Config().Model != "" {
		return
	}
	if err := config.SetGlobalModel(modelID); err != nil {
		log.Printf("could not save default model: %v", err)
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, ss := range d.channels {
		if cfg, err := config.Load(ss.Dir, d.trust); err == nil {
			ss.SetConfig(cfg)
		}
	}
}

// ProviderList builds the provider.list result.
func (d *Daemon) ProviderList() protocol.ProviderListResult {
	r := protocol.ProviderListResult{}
	if st := d.Registry.Store(); st != nil {
		r.AuthPath = st.Path()
	}
	for _, s := range d.Registry.List() {
		r.Providers = append(r.Providers, providerInfo(s))
	}
	return r
}

func providerInfo(s registry.Status) protocol.ProviderInfo {
	info := protocol.ProviderInfo{ID: s.ID, Name: s.Name, Connected: s.Connected, Kind: string(s.Kind), Priority: s.Priority,
		Models: s.Models, Label: s.Label, Account: s.Account}
	for _, m := range s.Methods {
		info.Methods = append(info.Methods, protocol.LoginMethod{ID: m.ID, Label: m.Label})
	}
	return info
}

var _ = log.Printf
