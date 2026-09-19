package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDaemonHoldsLock: a daemon that has started but not yet bound the
// socket still holds the data directory, and the CLI must see that — else
// it starts a second daemon that can only lose the lock and exit.
func TestDaemonHoldsLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_DATA_DIR", dir)
	if daemonHoldsLock() {
		t.Fatal("no lock file yet: nothing holds the directory")
	}
	path := filepath.Join(dir, "stavlosd.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if daemonHoldsLock() {
		t.Fatal("an unlocked file is not a running daemon")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if !daemonHoldsLock() {
		t.Fatal("a held lock is a daemon")
	}
	// and testing it must not have taken the lock away from the holder
	if !daemonHoldsLock() {
		t.Fatal("the check released somebody else's lock")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if daemonHoldsLock() {
		t.Fatal("released")
	}
}

// TestWaitForSocketGivesUpOnContext: the wait is interruptible, so ctrl+c
// during a long recovery ends the command instead of sitting out the whole
// startup budget.
func TestWaitForSocketGivesUpOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := waitForSocket(ctx, filepath.Join(t.TempDir(), "nothing.sock"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled context should end the wait")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait ignored its context")
	}
}

// TestWaitLockFree: replacing a daemon waits for it to let go of the data
// directory, not for its socket to vanish. A daemon closes its listener and
// removes the socket at the start of shutdown but holds the lock until the
// process exits, so a replacement started on the socket's absence dies with
// ErrAlreadyRunning and nothing ever binds.
func TestWaitLockFree(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_DATA_DIR", dir)
	f, err := os.OpenFile(filepath.Join(dir, "stavlosd.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if waitLockFree(context.Background(), 200*time.Millisecond) {
		t.Fatal("a held lock is not free")
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	}()
	if !waitLockFree(context.Background(), 5*time.Second) {
		t.Fatal("the release should end the wait")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if waitLockFree(ctx, 5*time.Second) {
		t.Fatal("a cancelled context ends the wait")
	}
}

// TestWaitForSocketFailsFastWhenTheDaemonDied: a daemon that exits during
// startup leaves the lock free, and the wait says so instead of sitting out
// the whole budget.
func TestWaitForSocketFailsFastWhenTheDaemonDied(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_DATA_DIR", dir)
	start := time.Now()
	_, err := waitForSocket(context.Background(), filepath.Join(dir, "nothing.sock"))
	if err == nil || !strings.Contains(err.Error(), "exited while starting") {
		t.Fatalf("err: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("it waited %s for a daemon that was never there", elapsed)
	}
}
