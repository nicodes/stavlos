package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func pair(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	c := newClient(a)
	t.Cleanup(func() { _ = c.Close(); _ = b.Close() })
	return c, b
}

func TestCloseWithFullNotifications(t *testing.T) {
	c, peer := pair(t)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i <= cap(c.Notifications); i++ {
			if _, err := fmt.Fprintln(peer, `{"jsonrpc":"2.0","method":"event","params":{}}`); err != nil {
				return
			}
		}
	}()
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not fill notification buffer")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Closed():
	case <-time.After(time.Second):
		t.Fatal("close blocked behind notifications")
	}
	if err := c.Close(); err != nil {
		t.Fatal("close is not idempotent", err)
	}
}

func TestCancelledBlockedWrite(t *testing.T) {
	c, _ := pair(t) // the peer deliberately never reads
	// Long enough that the write has begun when it fires even under the race
	// detector, where encoding the megabyte alone outlasted 50ms: a call
	// cancelled before its write starts rightly leaves the connection open.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "blocked", strings.Repeat("x", 1<<20), nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("write ignored cancellation")
	}
	select {
	case <-c.Closed():
	default:
		t.Fatal("partial frame connection must be closed")
	}
}

func TestCancelledWaitingWriterLeavesConnectionOpen(t *testing.T) {
	c, _ := pair(t)
	c.write <- struct{}{} // another writer owns the connection
	defer func() { <-c.write }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "waiting", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-c.Closed():
		t.Fatal("cancellation before write closed connection")
	default:
	}
}

func TestCloseReleasesPendingCall(t *testing.T) {
	c, peer := pair(t)
	read := make(chan struct{})
	go func() { _, _ = bufio.NewReader(peer).ReadString('\n'); close(read) }()
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "pending", nil, nil) }()
	<-read
	_ = c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed call succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pending call leaked")
	}
}
