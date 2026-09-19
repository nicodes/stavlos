package statefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := WriteAtomic(p, []byte("one"), 0o600, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(p, []byte("two"), 0o600, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if st, _ := os.Stat(p); string(b) != "two" || st.Mode().Perm() != 0o644 {
		t.Fatalf("%q %v: a kept mode is the file's own", b, st.Mode().Perm())
	}
	if err := WriteAtomic(p, []byte("three"), 0o600, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v: a secret's file is made private again", st.Mode().Perm())
	}
	// a directory that cannot be written leaves the file as it was
	if err := WriteAtomic(filepath.Join(dir, "missing", "x"), []byte("x"), 0o600, false); err == nil {
		t.Fatal("no error for a directory that does not exist")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}
