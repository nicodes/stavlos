package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepOlderRemovesOnlyOldOverflow(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "c1", "output")
	_ = os.MkdirAll(dir, 0o700)
	old, fresh := filepath.Join(dir, "old.txt"), filepath.Join(dir, "fresh.txt")
	_ = os.WriteFile(old, []byte("x"), 0o600)
	_ = os.WriteFile(fresh, []byte("y"), 0o600)
	_ = os.Chtimes(old, time.Now().Add(-8*24*time.Hour), time.Now().Add(-8*24*time.Hour))
	other := filepath.Join(root, "c1", "keep.txt") // not an overflow file
	_ = os.WriteFile(other, []byte("z"), 0o600)
	_ = os.Chtimes(other, time.Now().Add(-30*24*time.Hour), time.Now().Add(-30*24*time.Hour))
	n, err := sweepOlder(root, time.Now().Add(-7*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old overflow file kept")
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed", p)
		}
	}
	if n, err := sweepOlder(filepath.Join(root, "missing"), time.Now()); n != 0 || err != nil {
		t.Fatalf("missing root: %d %v", n, err)
	}
}
