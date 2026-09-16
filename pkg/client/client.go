// Package client is the Go protocol client (PRD §9, §14).
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/nicodes/stavlos/internal/peercred"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Client is a connection to stavlosd.
type Client struct {
	conn  net.Conn
	w     *bufio.Writer
	write chan struct{} // serializes writes without making cancellation wait for a mutex

	nextID  atomic.Int64
	pending map[int64]chan protocol.Response
	pmu     sync.Mutex

	// Notifications is fed every server→client notification. It is buffered;
	// a consumer that falls far behind blocks the reader, and the daemon then
	// drops the connection rather than wait (the client reconnects).
	Notifications chan protocol.Response

	closed chan struct{}
	err    error
	once   sync.Once
}

// Dial connects to the daemon socket.
func Dial(socket string) (*Client, error) {
	return DialContext(context.Background(), socket)
}

// DialContext connects with a cancellable dial.
func DialContext(ctx context.Context, socket string) (*Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	// A socket served by another user is not our daemon: whatever answers
	// would see every prompt and could answer it.
	if uc, ok := conn.(*net.UnixConn); ok {
		if _, err := peercred.OfSelf(uc); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", socket, err)
		}
	}
	return newClient(conn), nil
}

func newClient(conn net.Conn) *Client {
	c := &Client{
		conn:          conn,
		w:             bufio.NewWriter(conn),
		pending:       map[int64]chan protocol.Response{},
		Notifications: make(chan protocol.Response, 4096),
		closed:        make(chan struct{}),
		write:         make(chan struct{}, 1),
	}
	go c.readLoop()
	return c
}

func (c *Client) readLoop() {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r protocol.Response
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if r.ID == nil {
			select {
			case c.Notifications <- r:
			case <-c.closed:
				return
			}
			continue
		}
		var id int64
		_ = json.Unmarshal(*r.ID, &id)
		c.pmu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.pmu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
	c.shutdown(sc.Err())
}

func (c *Client) shutdown(err error) {
	c.once.Do(func() {
		if err == nil {
			err = errors.New("connection closed")
		}
		c.err = err
		close(c.closed)
		_ = c.conn.Close()
	})
}

// Closed is closed when the connection drops.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// Close closes the connection.
func (c *Client) Close() error { c.shutdown(nil); return nil }

// Call performs one JSON-RPC request; params is the method's params struct
// (nil for none). The protocol version travels in the envelope.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	idb, _ := json.Marshal(id)
	idr := json.RawMessage(idb)
	req := protocol.Request{JSONRPC: "2.0", V: protocol.Version, ID: &idr, Method: method, Params: raw}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}

	ch := make(chan protocol.Response, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() { // answered, abandoned or failed: the id is done either way
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()

	if err := c.send(ctx, append(b, '\n')); err != nil {
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return c.err
		}
		if r.Error != nil {
			return r.Error
		}
		if result != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.err
	}
}

// send closes a connection interrupted mid-write: a partial JSON frame cannot
// safely be followed by another request. Cancellation while waiting to write
// leaves the connection alone.
func (c *Client) send(ctx context.Context, b []byte) error {
	select {
	case c.write <- struct{}{}:
		defer func() { <-c.write }()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { c.shutdown(ctx.Err()) })
	defer stop()
	_, err := c.w.Write(b)
	if err == nil {
		err = c.w.Flush()
	}
	if err != nil {
		c.shutdown(err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

// Do calls method m with params p and returns its result: the method's
// descriptor fixes both types, so a mismatched call does not compile.
func Do[P, R any](ctx context.Context, c *Client, m protocol.Method[P, R], p P) (R, error) {
	var r R
	err := c.Call(ctx, m.Name, p, &r)
	return r, err
}

// DecodeNotification unpacks a notification into the typed struct for its method.
func DecodeNotification(r protocol.Response) (any, error) {
	switch r.Method {
	case protocol.NEvent:
		var n protocol.EventNotification
		return n, json.Unmarshal(r.Params, &n)
	case protocol.NStream:
		var n protocol.StreamNotification
		return n, json.Unmarshal(r.Params, &n)
	case protocol.NPrompt:
		var n protocol.PromptNotification
		return n, json.Unmarshal(r.Params, &n)
	}
	return nil, fmt.Errorf("unknown notification %q", r.Method)
}
