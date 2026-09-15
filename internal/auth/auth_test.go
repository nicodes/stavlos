package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "auth.json")
	s := Open(p)
	if _, ok := s.Get("openai"); ok {
		t.Fatal("empty store had a credential")
	}
	if err := s.Set("openai/", Credential{Access: "acc", Refresh: "ref", Expires: 42, AccountID: "acct", Email: "me@x.y"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v %v", st.Mode(), err)
	}
	c, ok := s.Get("openai")
	if !ok || c.Type != "oauth" || c.Access != "acc" || c.Refresh != "ref" || c.Expires != 42 || c.AccountID != "acct" || c.Email != "me@x.y" || c.Added == "" {
		t.Fatalf("%+v %v", c, ok)
	}
	if got := s.Providers(); len(got) != 1 || got[0] != "openai" {
		t.Fatalf("%v", got)
	}
	if err := s.Remove("openai"); err != nil {
		t.Fatal(err)
	}
	if got := s.Providers(); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if err := s.Remove("missing"); err != nil {
		t.Fatalf("removing a missing credential: %v", err)
	}
}

// TestStoreSeesOtherWriters: the in-memory copy is dropped when another
// process rewrites the file, and no temporary files are left behind.
func TestStoreSeesOtherWriters(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	a, b := Open(p), Open(p)
	if err := a.Set("openai", Credential{Access: "one", Refresh: "r"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := b.Get("openai"); c.Access != "one" {
		t.Fatalf("b before: %+v", c)
	}
	if err := a.Set("openai", Credential{Access: "two-longer", Refresh: "r"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := b.Get("openai"); c.Access != "two-longer" {
		t.Fatalf("b after: %+v", c)
	}
	all, _ := b.All()
	all["xai"] = Credential{}
	if _, ok := b.Get("xai"); ok {
		t.Fatal("All must return a copy")
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Get("openai"); ok {
		t.Fatal("a missed an external rewrite")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("files left: %v", entries)
	}
}
