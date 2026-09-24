// Package auth is the credential store (PRD §8.4): the credentials of the
// subscription logins (ChatGPT, Grok, Z.ai) made through /providers or
// `stavlos auth login`, kept in <data dir>/auth.json with mode 0600, never
// in project config, and never read from the environment.
//
// Most logins store an OAuth pair. A plan that issues no OAuth credential
// stores the key its subscription is bound to instead (Z.ai's GLM Coding
// Plan): it is still a credential the user pasted into Stavlos once, kept
// in the same file under the same mode, not a key read from the
// environment or from a repository's configuration.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/statefile"
)

// Credential types.
const (
	TypeOAuth  = "oauth"  // an access/refresh pair from a device or browser flow
	TypeAPIKey = "apikey" // a key the user pasted, bound to their plan
)

// Credential is one stored subscription login.
type Credential struct {
	Type  string `json:"type"`
	Added string `json:"added,omitempty"`

	// oauth
	Access    string `json:"access,omitempty"`
	Refresh   string `json:"refresh,omitempty"`
	Expires   int64  `json:"expires,omitempty"` // unix milliseconds
	AccountID string `json:"account_id,omitempty"`
	Email     string `json:"email,omitempty"`

	// apikey
	Key string `json:"key,omitempty"`
}

// Secret is what a request authenticates with: the access token of an
// OAuth login, or the key of a pasted one.
func (c Credential) Secret() string {
	if c.Type == TypeAPIKey {
		return c.Key
	}
	return c.Access
}

// Store reads and writes auth.json. Reads are served from memory while the
// file's modification time and size are unchanged, so the per-request token
// lookups of a running daemon cost a stat, not a read and a JSON decode;
// a login made by another process is still seen at once.
type Store struct {
	path string
	mu   sync.Mutex

	cached  map[string]Credential // nil until the first load
	modTime time.Time
	size    int64
}

// Open returns a store at path; the file need not exist yet.
func Open(path string) *Store { return &Store{path: path} }

// Path returns the file path.
func (s *Store) Path() string { return s.path }

// load returns the stored credentials; callers hold mu and must not modify
// the map (it is the cache).
func (s *Store) load() (map[string]Credential, error) {
	st, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.remember(map[string]Credential{}, nil)
		return s.cached, nil
	}
	if err != nil {
		return nil, err
	}
	if s.cached != nil && st.ModTime().Equal(s.modTime) && st.Size() == s.size {
		return s.cached, nil
	}
	// The file is written 0600; one put in place by hand, or restored from
	// a backup, may not be. Tightened here rather than trusted.
	if st.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(s.path, 0o600)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	m := map[string]Credential{}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
	}
	s.remember(m, st)
	return m, nil
}

func (s *Store) remember(m map[string]Credential, st fs.FileInfo) {
	s.cached, s.modTime, s.size = m, time.Time{}, -1
	if st != nil {
		s.modTime, s.size = st.ModTime(), st.Size()
	}
}

// save writes m through a temporary file of its own (mode 0600 from the
// start) and renames it into place, then caches it.
func (s *Store) save(m map[string]Credential) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := statefile.WriteAtomic(s.path, append(b, '\n'), 0o600, false); err != nil {
		s.cached = nil
		return err
	}
	st, err := os.Stat(s.path)
	if err != nil {
		s.cached = nil
		return nil
	}
	s.remember(m, st)
	return nil
}

// All returns a copy of every stored credential keyed by provider id.
func (s *Store) All() (map[string]Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	return maps.Clone(m), nil
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
		c.Type = "oauth"
	}
	if c.Added == "" {
		c.Added = time.Now().UTC().Format(time.RFC3339)
	}
	m = maps.Clone(m)
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
	m = maps.Clone(m)
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
