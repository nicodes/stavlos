package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/peercred"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

// Finding the daemon, starting it, and replacing one this binary cannot or
// should not talk to. It was two hundred lines in the middle of main.

// connect dials the daemon, starting it in the background if needed.
func connect(ctx context.Context, autostart bool) (*client.Client, error) {
	sock := paths.Socket()
	c, err := client.Dial(sock)
	if err == nil {
		return attach(ctx, c)
	}
	if !autostart {
		return nil, fmt.Errorf("daemon not running at %s (start with `stavlos daemon` or run `stavlos`)", sock)
	}
	// A daemon that is still recovering holds the lock but has not bound
	// the socket yet: starting a second one would only lose the lock and
	// exit, leaving this terminal waiting for the first one regardless.
	if !daemonHoldsLock() {
		if err := startDaemon(); err != nil {
			return nil, err
		}
	}
	nc, err := waitForSocket(ctx, sock)
	if err != nil {
		return nil, err
	}
	return attach(ctx, nc)
}

// startupWait is how long a daemon is given to recover its channels and
// bind the socket. Recovery reads the whole event log, so a long-lived
// data directory takes seconds, not milliseconds.
const startupWait = 60 * time.Second

// startupQuiet is how long the socket may be missing before we say what we
// are waiting for. Under this, the daemon comes up before anyone reads it.
const startupQuiet = 750 * time.Millisecond

// lockWait is how long a daemon being replaced is given to exit and let go
// of the data directory.
const lockWait = 30 * time.Second

// lockGrace is how long a daemon we started is given to take the lock
// before its absence means it died rather than that it is slow to start.
const lockGrace = 3 * time.Second

// waitLockFree waits until no daemon holds the data directory.
func waitLockFree(ctx context.Context, d time.Duration) bool {
	for start := time.Now(); time.Since(start) < d; time.Sleep(100 * time.Millisecond) {
		if !daemonHoldsLock() {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}

// waitForSocket dials until the daemon is listening, saying what it is
// doing once the wait is long enough to look like a hang: recovery has no
// output of its own, and silence here reads as a stuck terminal.
func waitForSocket(ctx context.Context, sock string) (*client.Client, error) {
	start := time.Now()
	said := false
	done := func() {
		if said {
			fmt.Fprintln(os.Stderr)
		}
	}
	for time.Since(start) < startupWait {
		if c, err := client.Dial(sock); err == nil {
			if said {
				fmt.Fprintf(os.Stderr, " ready in %s\n", time.Since(start).Round(100*time.Millisecond))
			}
			return c, nil
		}
		// A daemon takes the data directory before it recovers anything, so
		// once the grace period is over an unheld lock means the one we
		// started is gone: waiting out the whole budget for a dead process
		// is the difference between a slow start and a hang.
		if time.Since(start) > lockGrace && !daemonHoldsLock() {
			done()
			return nil, fmt.Errorf("the daemon exited while starting; see %s", filepath.Join(paths.DataDir(), "stavlosd.log"))
		}
		if !said && time.Since(start) > startupQuiet {
			fmt.Fprint(os.Stderr, "waiting for the daemon to recover its channels…")
			said = true
		}
		if err := ctx.Err(); err != nil {
			done()
			return nil, err
		}
		time.Sleep(150 * time.Millisecond)
	}
	done()
	return nil, fmt.Errorf("daemon did not come up at %s after %s; see %s", sock, startupWait, filepath.Join(paths.DataDir(), "stavlosd.log"))
}

// daemonHoldsLock reports whether a daemon holds the data directory, which
// includes one that has started but is still recovering. The lock is the
// daemon's own (internal/daemon/lock.go); taking it here only tests it, and
// the descriptor is closed either way.
func daemonHoldsLock() bool {
	f, err := os.OpenFile(filepath.Join(paths.DataDir(), "stavlosd.lock"), os.O_RDWR, 0o600)
	if err != nil {
		return false // no lock file: no daemon has run here
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return true // somebody else holds it
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
}

func attach(ctx context.Context, c *client.Client) (*client.Client, error) {
	if _, err := client.Do(ctx, c, protocol.Attach, protocol.AttachParams{Client: fmt.Sprintf("tui:%d", os.Getpid()), Tier: protocol.TierInteractive}); err != nil {
		c.Close()
		if isVersionError(err) && os.Getenv("STAVLOS_KEEP_DAEMON") == "" {
			return replaceIncompatible(ctx, err)
		}
		return nil, err
	}
	if fresh, err := replaceStale(ctx, c); err != nil {
		return nil, err
	} else if fresh != nil {
		return fresh, nil
	}
	return c, nil
}

// isVersionError reports whether the daemon refused this client's protocol
// version.
func isVersionError(err error) bool {
	var pe *protocol.Error
	return errors.As(err, &pe) && pe.Code == protocol.ErrVersion
}

// replaceIncompatible stops a daemon that refuses this client's protocol
// version and starts one from this binary. Such a daemon was built before a
// protocol change and cannot answer daemon.status or daemon.shutdown either,
// so it is found as the process serving the socket and sent SIGTERM.
func replaceIncompatible(ctx context.Context, cause error) (*client.Client, error) {
	sock := paths.Socket()
	pid, err := socketPeerPID(sock)
	if err != nil {
		return nil, fmt.Errorf("%v; stop the running daemon and run stavlos again (finding it: %v)", cause, err)
	}
	fmt.Fprintf(os.Stderr, "daemon (pid %d) speaks an older protocol; restarting it\n", pid)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return nil, fmt.Errorf("%v; stopping daemon pid %d: %v", cause, pid, err)
	}
	if !waitExit(pid, 10*time.Second) {
		return nil, fmt.Errorf("%v; daemon pid %d did not exit after SIGTERM", cause, pid)
	}
	return restartDaemon(ctx, sock)
}

// waitExit waits up to d for process pid to be gone.
func waitExit(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}

// restartEnv replaces a daemon of another build even while agents are working.
const restartEnv = "STAVLOS_RESTART_DAEMON"

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func replaceStale(ctx context.Context, c *client.Client) (*client.Client, error) {
	st, err := client.Do(ctx, c, protocol.DaemonStatus, protocol.None{})
	if err != nil {
		return nil, nil // very old daemon without status; leave it
	}
	if st.Build == buildid.ID() || os.Getenv("STAVLOS_KEEP_DAEMON") != "" {
		return nil, nil
	}
	// A restart ends every turn in progress and loses every running job. A
	// rebuilt binary is no reason for that: the daemon is replaced when
	// nothing is working, or when asked to be.
	if st.Working > 0 && os.Getenv(restartEnv) == "" {
		fmt.Fprintf(os.Stderr, "daemon build %s differs from this binary (%s), but %s working, so it was left running.\nIt is replaced the next time stavlos starts with nothing in progress; %s=1 replaces it now.\n",
			short(st.Build), short(buildid.ID()), plural(st.Working, "agent is", "agents are"), restartEnv)
		return nil, nil
	}
	fmt.Fprintf(os.Stderr, "daemon build %s differs from this binary (%s); restarting it\n", short(st.Build), short(buildid.ID()))
	sock := paths.Socket()
	if _, err := client.Do(ctx, c, protocol.DaemonShutdown, protocol.None{}); err != nil {
		// A daemon that cannot shut down on request is stopped by the pid
		// the kernel reports for the socket, not the one it reports itself.
		if pid, perr := socketPeerPID(sock); perr == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	c.Close()
	// Wait for the lock, not for the socket. A daemon shutting down closes
	// its listener and removes the socket first, then stops its channels,
	// and only releases the data directory when the process exits. Starting
	// the replacement while the socket is gone but the lock is still held
	// gives it ErrAlreadyRunning: it dies at once, nothing ever binds, and
	// this terminal waits for a daemon that no longer exists.
	if !waitLockFree(ctx, lockWait) {
		return nil, fmt.Errorf("the daemon did not let go of %s within %s; stop it and run stavlos again", paths.DataDir(), lockWait)
	}
	return restartDaemon(ctx, sock)
}

// restartDaemon starts a daemon from this binary once the old one is gone
// and attaches to it.
func restartDaemon(ctx context.Context, sock string) (*client.Client, error) {
	if err := startDaemon(); err != nil {
		return nil, err
	}
	nc, err := waitForSocket(ctx, sock)
	if err != nil {
		return nil, err
	}
	if _, err := client.Do(ctx, nc, protocol.Attach, protocol.AttachParams{Client: fmt.Sprintf("tui:%d", os.Getpid()), Tier: protocol.TierInteractive}); err != nil {
		nc.Close()
		return nil, err
	}
	return nc, nil
}

func short(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func startDaemon() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.DataDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(paths.DataDir(), "stavlosd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Stdin = nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "started the daemon (pid %d, log %s)\n", cmd.Process.Pid, logf.Name())
	return cmd.Process.Release()
}

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
