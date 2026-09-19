package web

import (
	"bytes"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn makes a WebSocket look like the Unix socket the protocol is
// written for: newline-delimited JSON. Each text message read gets a
// newline; what is written is cut back into one message per line, however
// the writer's buffering split it.
type wsConn struct {
	c *websocket.Conn

	rmu sync.Mutex
	r   io.Reader // the rest of the message being read, then its newline

	wmu     sync.Mutex
	pending []byte // a line not yet ended

	once sync.Once
}

func newWSConn(c *websocket.Conn) *wsConn { return &wsConn{c: c} }

func (w *wsConn) Read(p []byte) (int, error) {
	w.rmu.Lock()
	defer w.rmu.Unlock()
	for {
		if w.r != nil {
			n, err := w.r.Read(p)
			if err == io.EOF {
				w.r, err = nil, nil
			}
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		kind, r, err := w.c.NextReader()
		if err != nil {
			return 0, err
		}
		if kind != websocket.TextMessage {
			continue
		}
		w.r = io.MultiReader(r, bytes.NewReader([]byte{'\n'}))
	}
}

func (w *wsConn) Write(p []byte) (int, error) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		if err := w.c.WriteMessage(websocket.TextMessage, w.pending[:i]); err != nil {
			return 0, err
		}
		w.pending = w.pending[i+1:]
		if len(w.pending) == 0 {
			w.pending = nil // let a big line's buffer go
		}
	}
}

func (w *wsConn) Close() error {
	var err error
	w.once.Do(func() { err = w.c.Close() })
	return err
}

func (w *wsConn) LocalAddr() net.Addr  { return w.c.LocalAddr() }
func (w *wsConn) RemoteAddr() net.Addr { return w.c.RemoteAddr() }

func (w *wsConn) SetDeadline(t time.Time) error {
	if err := w.c.SetReadDeadline(t); err != nil {
		return err
	}
	return w.c.SetWriteDeadline(t)
}
func (w *wsConn) SetReadDeadline(t time.Time) error  { return w.c.SetReadDeadline(t) }
func (w *wsConn) SetWriteDeadline(t time.Time) error { return w.c.SetWriteDeadline(t) }
