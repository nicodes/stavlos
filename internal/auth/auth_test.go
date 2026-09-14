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
