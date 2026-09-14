package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// socketPeerPID dials a unix socket and returns the pid of the process
// serving it, as the kernel reports it (SO_PEERCRED). A socket served by
// another user is refused: its process is not ours to stop.
func socketPeerPID(sock string) (int, error) {
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	if int(cred.Uid) != os.Getuid() {
		return 0, fmt.Errorf("the socket is served by another user (uid %d)", cred.Uid)
	}
	if cred.Pid <= 0 {
		return 0, fmt.Errorf("the kernel reported no pid for the socket's peer")
	}
	return int(cred.Pid), nil
}
