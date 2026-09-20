package daemon

import (
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestAdoptedZombiesAreReapedAndOwnChildrenLeftAlone: a process the daemon
// adopted and that has exited is collected on the second scan that sees it.
// A child of our own that has just exited and is about to be waited for is
// seen once at most, and its exit status is still there for its waiter.
func TestAdoptedZombiesAreReapedAndOwnChildrenLeftAlone(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("cannot become a subreaper here: %v", err)
	}
	defer unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0)
	// a grandchild that outlives its parent by a moment, then exits: ours to reap
	if err := exec.Command("sh", "-c", "(sleep 0.2 &) ; exit 0").Run(); err != nil {
		t.Fatal(err)
	}
	// a child of our own, exited and not yet waited for
	own := exec.Command("true")
	if err := own.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(zombieChildren(os.Getpid())) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("expected two zombies (the orphan and our own child), have %v", zombieChildren(os.Getpid()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	seen := reapSeenTwice(map[int]bool{}) // the first scan reaps nothing
	if len(seen) != 2 || len(zombieChildren(os.Getpid())) != 2 {
		t.Fatalf("the first scan: seen %v, zombies %v", seen, zombieChildren(os.Getpid()))
	}
	if err := own.Wait(); err != nil { // its waiter arrives between scans, as it does in the daemon
		t.Fatalf("our own child's exit status was taken from under its waiter: %v", err)
	}
	reapSeenTwice(seen)
	if z := zombieChildren(os.Getpid()); len(z) != 0 {
		t.Fatalf("the adopted zombie was not reaped: %v", z)
	}
	if slices.Contains(zombieChildren(os.Getpid()), own.Process.Pid) {
		t.Fatal("our own child is still a zombie")
	}
}
