package agent

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

// TestConfigDirsJoinEveryChannel: the global stavlos.json's dirs are in
// every channel's working set, marked config, and cannot be removed from
// one channel.
func TestConfigDirsJoinEveryChannel(t *testing.T) {
	shared := t.TempDir()
	s, _ := newTestChannel(t, testConfig{json: `{"model":"fake/m1","dirs":[` + strconv.Quote(shared) + `]}`}, &fakeModel{})
	s.mu.Lock()
	infos := s.dirInfosLocked()
	s.mu.Unlock()
	if len(infos) != 2 || infos[0].Source != "channel" || infos[1].Path != shared || infos[1].Source != "config" {
		t.Fatalf("working set: %+v", infos)
	}
	if !inDirs(s.dirPaths(), filepath.Join(shared, "notes.md")) {
		t.Fatal("a file under a config dir is inside the working set")
	}
	if err := s.RemoveDir(context.Background(), shared); err == nil {
		t.Fatal("a config dir cannot be removed from one channel")
	}
	if err := s.AddDir(context.Background(), shared); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	n := len(s.dirInfosLocked())
	s.mu.Unlock()
	if n != 2 {
		t.Fatalf("adding a config dir again adds nothing: %d dirs", n)
	}
}
