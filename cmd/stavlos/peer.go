package main

import (
	"fmt"
	"net"
	"time"

	"github.com/nicodes/stavlos/internal/peercred"
)

// socketPeerPID dials a unix socket and returns the pid of the process
// serving it, as the kernel reports it (SO_PEERCRED), never as the process
// says. A socket served by another user is refused: its process is not
// ours to stop.
func socketPeerPID(sock string) (int, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	cred, err := peercred.OfSelf(conn.(*net.UnixConn))
	if err != nil {
		return 0, err
	}
	if cred.PID <= 0 {
		return 0, fmt.Errorf("the kernel reported no pid for the socket's peer")
	}
	return cred.PID, nil
}
