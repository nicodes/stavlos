// Package peercred asks the kernel who is on the other end of a Unix
// socket (SO_PEERCRED) and how processes are related, so the daemon and
// its clients decide on facts the peer cannot forge: a daemon's self-reported
// pid, or a client's claim about what it is, are not such facts.
package peercred

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Cred is a socket peer's process and user, as the kernel reports them.
type Cred struct {
	PID, UID int
}

// Of returns the credentials of a connection's peer.
func Of(c *net.UnixConn) (Cred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Cred{}, err
	}
	if credErr != nil {
		return Cred{}, credErr
	}
	return Cred{PID: int(cred.Pid), UID: int(cred.Uid)}, nil
}

// OfSelf reports whether a connection's peer runs as this process's user.
func OfSelf(c *net.UnixConn) (Cred, error) {
	cred, err := Of(c)
	if err != nil {
		return cred, err
	}
	if cred.UID != os.Getuid() {
		return cred, fmt.Errorf("the socket's peer runs as another user (uid %d)", cred.UID)
	}
	return cred, nil
}

// DescendsFrom reports whether process pid was started, directly or
// through other processes, by ancestor. It walks parents in /proc.
//
// An ancestry that cannot be read is an error, and the caller must treat it
// as one: the question is asked to keep a process off something, so "I could
// not tell" is never "no". (It used to be: a peer whose /proc entry vanished
// mid-walk was taken for a stranger and given every right.)
//
// A process that detaches itself (a double fork, its parent gone) is
// reparented to the nearest subreaper, or to init. The daemon therefore
// makes itself a subreaper (daemon.Main), so the processes its agents start
// stay its descendants however they fork.
func DescendsFrom(pid, ancestor int) (bool, error) {
	for i := 0; i < 128 && pid > 1; i++ {
		parent, err := ParentOf(pid)
		if err != nil {
			return false, fmt.Errorf("the ancestry of process %d cannot be read: %w", pid, err)
		}
		if parent == ancestor {
			return true, nil
		}
		pid = parent
	}
	if pid > 1 {
		return false, fmt.Errorf("the ancestry of process %d is deeper than this walks", pid)
	}
	return false, nil
}

// ParentOf is a process's parent pid, from /proc/<pid>/stat.
func ParentOf(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// "pid (comm) state ppid …": comm may hold spaces and parentheses, so
	// the fields start after the last ")".
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("/proc/%d/stat: unexpected format", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("/proc/%d/stat: unexpected format", pid)
	}
	return strconv.Atoi(f[1])
}
