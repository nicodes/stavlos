package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

// TestSocketPeerPID: the pid behind a socket is the process listening on it.
func TestSocketPeerPID(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	pid, err := socketPeerPID(sock)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("pid %d err %v, want %d", pid, err, os.Getpid())
	}
	if _, err := socketPeerPID(filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("a missing socket has no peer")
	}
}

// TestIsVersionError: only the daemon's version refusal triggers a
// replacement, however it is wrapped.
func TestIsVersionError(t *testing.T) {
	refusal := &protocol.Error{Code: protocol.ErrVersion, Message: "protocol version 0 not served; this daemon serves 1"}
	if !isVersionError(refusal) || !isVersionError(fmt.Errorf("attach: %w", refusal)) {
		t.Fatal("a version refusal must be recognised")
	}
	if isVersionError(&protocol.Error{Code: protocol.ErrConflict, Message: "x"}) || isVersionError(errors.New("protocol version 0 not served")) {
		t.Fatal("other errors must not replace the daemon")
	}
}
