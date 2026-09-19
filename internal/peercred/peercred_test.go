package peercred

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOfAndDescendsFrom(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			_, _ = c.Read(buf)
		}
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cred, err := OfSelf(c.(*net.UnixConn))
	if err != nil || cred.PID != os.Getpid() || cred.UID != os.Getuid() {
		t.Fatalf("cred %+v err %v", cred, err)
	}

	child := exec.Command("sh", "-c", "sleep 5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	descends := func(pid, ancestor int) bool {
		t.Helper()
		ok, err := DescendsFrom(pid, ancestor)
		if err != nil {
			t.Fatalf("DescendsFrom(%d, %d): %v", pid, ancestor, err)
		}
		return ok
	}
	if !descends(child.Process.Pid, os.Getpid()) {
		t.Fatal("a child should descend from its parent")
	}
	if descends(os.Getpid(), child.Process.Pid) || descends(os.Getpid(), os.Getpid()) {
		t.Fatal("descent runs one way and a process is not its own descendant")
	}
	if p, err := ParentOf(os.Getpid()); err != nil || p != os.Getppid() {
		t.Fatalf("parent %d err %v", p, err)
	}
}

// TestUnreadableAncestryIsAnErrorNotANo (1A.1): the question is asked to keep
// a process off the daemon's socket. A process whose /proc entry cannot be
// read used to be taken for a stranger, which is to say given every right.
func TestUnreadableAncestryIsAnErrorNotANo(t *testing.T) {
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if ok, err := DescendsFrom(gone.Process.Pid, os.Getpid()); err == nil || ok {
		t.Fatalf("a process that no longer exists: descends=%v err=%v, want an error", ok, err)
	}
}

// TestADetachedProcessStillDescendsFromASubreaper (1A.2): a double fork whose
// middle process exits is reparented to the nearest subreaper. With the
// daemon one, what an agent's command detaches is still the daemon's
// descendant, and still refused.
func TestADetachedProcessStillDescendsFromASubreaper(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("cannot become a subreaper here: %v", err)
	}
	defer unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0)
	pidFile := filepath.Join(t.TempDir(), "pid")
	// the shell starts a grandchild in the background and exits at once
	if err := exec.Command("sh", "-c", "(sleep 30 & echo $! > "+pidFile+") ; exit 0").Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Kill(pid, unix.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for {
		ok, err := DescendsFrom(pid, os.Getpid())
		if err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			parent, _ := ParentOf(pid)
			t.Fatalf("the detached process %d (parent %d) does not descend from the subreaper %d: %v", pid, parent, os.Getpid(), err)
		}
		time.Sleep(20 * time.Millisecond) // until its parent has exited and it is reparented
	}
}
