package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The daemon is a subreaper (Main), so a process an agent's command detaches
// is reparented to it, not to init, and stays refused at the socket. An
// adopted process that exits becomes the daemon's zombie, and nothing waits
// for it: the code that waits on a child only knows the ones it started.
//
// reapOrphans collects those. It must not take an exit status away from code
// that is about to wait for its own child, which a blanket wait4(-1) would,
// so it only reaps a child it has seen as a zombie on two scans in a row:
// a child somebody waits on is collected the moment it exits and is never
// seen twice. A child nobody waits on (an MCP server that crashed while
// idle) may be collected here first; its eventual Wait then reports "no
// child processes", which is an error message, not a leak.

// orphanScanEvery is how often the daemon looks for zombies it adopted.
const orphanScanEvery = 15 * time.Second

func reapOrphans(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	seen := map[int]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			seen = reapSeenTwice(seen)
		}
	}
}

// reapSeenTwice reaps the zombie children that were zombies at the last scan
// too, and returns this scan's zombies for the next.
func reapSeenTwice(before map[int]bool) map[int]bool {
	now := map[int]bool{}
	for _, pid := range zombieChildren(os.Getpid()) {
		if before[pid] {
			var ws unix.WaitStatus
			_, _ = unix.Wait4(pid, &ws, unix.WNOHANG, nil)
			continue
		}
		now[pid] = true
	}
	return now
}

// zombieChildren lists the zombie processes whose parent is ppid.
func zombieChildren(ppid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')') // comm may hold spaces and parentheses
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) >= 2 && f[0] == "Z" && f[1] == strconv.Itoa(ppid) {
			out = append(out, pid)
		}
	}
	return out
}
