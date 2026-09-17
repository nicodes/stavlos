package navigation

import (
	"os"
	"testing"
	"time"
)

func TestLastSelectionIsGlobalAndOrdered(t *testing.T) {
	t.Setenv("STAVLOS_DATA_DIR", t.TempDir())
	if Last() != "" {
		t.Fatal("new navigation should be empty")
	}
	now := time.Now()
	if err := Remember("project-a", now); err != nil {
		t.Fatal(err)
	}
	if err := Remember("project-b", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := Remember("delayed-a", now); err != nil {
		t.Fatal(err)
	}
	if Last() != "project-b" {
		t.Fatal("late selection overwrote newer one")
	}
	st, err := os.Stat(filename())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := os.WriteFile(filename(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Last() != "" {
		t.Fatal("broken navigation should fall back")
	}
}
