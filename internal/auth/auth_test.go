package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "auth.json")
	s := Open(p)
	if _, ok := s.Get("anthropic"); ok {
		t.Fatal("empty store had a key")
	}
	if err := s.Set("anthropic/", Credential{Key: "sk-1"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v %v", st.Mode(), err)
	}
	c, ok := s.Get("anthropic")
	if !ok || c.Key != "sk-1" || c.Type != "api" || c.Added == "" {
		t.Fatalf("%+v %v", c, ok)
	}
	t.Setenv("FOO_KEY", "env-1")
	if k, src, via := s.Resolve("anthropic", []string{"FOO_KEY"}); k != "sk-1" || src != SourceStore || via != p {
		t.Fatalf("store should win: %q %q %q", k, src, via)
	}
	if k, src, via := s.Resolve("foo", []string{"NOPE", "FOO_KEY"}); k != "env-1" || src != SourceEnv || via != "FOO_KEY" {
		t.Fatalf("env: %q %q %q", k, src, via)
	}
	if k, src, _ := s.Resolve("bar", nil); k != "" || src != SourceNone {
		t.Fatal("none")
	}
	if err := s.Remove("anthropic"); err != nil {
		t.Fatal(err)
	}
	if got := s.Providers(); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
