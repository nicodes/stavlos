//go:build linux

package tools

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func operatorPeer(connection *net.UnixConn, owner int) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return err
	}
	var credential *unix.Ucred
	var result error
	if err := raw.Control(func(fd uintptr) {
		credential, result = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if result != nil {
		return result
	}
	if credential == nil || int(credential.Uid) != owner {
		return errors.New("operator peer differs")
	}
	return nil
}
