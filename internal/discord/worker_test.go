package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "dial unix /run/stavlos.sock: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

func TestPlainErrorHidesInternals(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "The daemon did not answer in time. Try again."},
		{fmt.Errorf("prompt list: %w", context.DeadlineExceeded), "The daemon did not answer in time. Try again."},
		{&net.OpError{Op: "dial", Net: "unix", Err: timeoutError{}}, "The daemon did not answer in time. Try again."},
		{net.ErrClosed, "The daemon is not running."},
		{&net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}, "The daemon is not running."},
		{&net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}, "The daemon is not running."},
		{io.EOF, "The daemon is not running."},
		{&protocol.Error{Code: protocol.ErrNotFound, Message: "channel ch_8f3a not found"}, "That channel or agent no longer exists."},
		{&protocol.Error{Code: protocol.ErrConflict, Message: "prompt p_91 claimed by client c_3"}, "Could not complete the action."},
		{errors.New("write /run/stavlos.sock: broken pipe, seq 44"), "Could not complete the action."},
	}
	for _, c := range cases {
		if got := plainError(c.err); got != c.want {
			t.Errorf("%v: got %q, want %q", c.err, got, c.want)
		}
	}
}
