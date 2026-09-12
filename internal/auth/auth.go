// Package auth is the credential store (PRD §8.4, roadmap item pulled into
// v1): provider API keys entered through /provider or `stavlos auth login`,
// kept in <data dir>/auth.json with mode 0600, never in project config.
// Environment variables still work and take precedence, so nothing here
// forces a user who prefers exported keys to change anything.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Credential is one stored credential. Type is "oauth" for subscription
// logins (ChatGPT, Grok) and "api" for API keys.
type Credential struct {
	Type     string            `json:"type"`
	Key      string            `json:"key,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Added    string            `json:"added,omitempty"`

	// oauth
	Access    string `json:"access,omitempty"`
	Refresh   string `json:"refresh,omitempty"`
	Expires   int64  `json:"expires,omitempty"` // unix milliseconds
	AccountID string `json:"account_id,omitempty"`
	Email     string `json:"email,omitempty"`
}

// Store reads and writes auth.json.
type Store struct {
	path string
	mu   sync.Mutex
}

// Open returns a store at path; the file need not exist yet.
func Open(path string) *Store { return &Store{path: path} }

// Path returns the file path.
func (s *Store) Path() string { return s.path }

func (s *Store) load() (map[string]Credential, error) {
	m := map[string]Credential{}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return m, nil
}

func (s *Store) save(m map[string]Credential) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// All returns every stored credential keyed by provider id.
func (s *Store) All() (map[string]Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Get returns the credential for a provider.
func (s *Store) Get(provider string) (Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return Credential{}, false
	}
	c, ok := m[norm(provider)]
	return c, ok
}

// Set stores a credential, replacing any existing one.
func (s *Store) Set(provider string, c Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if c.Type == "" {
		if c.Access != "" {
			c.Type = "oauth"
		} else {
			c.Type = "api"
		}
	}
	if c.Added == "" {
		c.Added = time.Now().UTC().Format(time.RFC3339)
	}
	m[norm(provider)] = c
	return s.save(m)
}

// Remove deletes a provider's credential. Removing a missing one is not an error.
func (s *Store) Remove(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	delete(m, norm(provider))
	return s.save(m)
}

// Providers lists provider ids with stored credentials, sorted.
func (s *Store) Providers() []string {
	m, err := s.All()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func norm(p string) string { return strings.TrimRight(strings.TrimSpace(p), "/") }

// Source says where a key came from.
type Source string

const (
	SourceNone  Source = ""
	SourceStore Source = "auth.json"
	SourceEnv   Source = "env"
)

// Resolve returns the key for a provider: the store first, then the first
// set env var from envVars. The returned string names the source (the env
// var name when from the environment).
func (s *Store) Resolve(provider string, envVars []string) (key string, source Source, via string) {
	if s != nil {
		if c, ok := s.Get(provider); ok && c.Key != "" {
			return c.Key, SourceStore, s.path
		}
	}
	for _, n := range envVars {
		if v := os.Getenv(n); v != "" {
			return v, SourceEnv, n
		}
	}
	return "", SourceNone, ""
}
