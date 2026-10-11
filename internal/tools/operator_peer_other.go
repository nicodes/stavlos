//go:build !linux

package tools

import (
	"errors"
	"net"
)

func operatorPeer(_ *net.UnixConn, _ int) error {
	return errors.New("fleet operator peer authentication requires Linux")
}
