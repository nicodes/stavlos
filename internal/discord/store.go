package discord

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type promptMessage struct {
	Channel string `json:"channel"`
	Message string `json:"message"`
}

// promptStore contains no webhook tokens and no chat replay cursor. Writes
// replace the file atomically so a restart can retire old prompt controls.
type promptStore struct {
	mu    sync.Mutex
	path  string
	items map[string]promptMessage
}

func openStore(path string) (*promptStore, error) {
	s := &promptStore{path: path, items: map[string]promptMessage{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.items); err != nil {
		return nil, err
	}
	if s.items == nil {
		s.items = map[string]promptMessage{}
	}
	return s, nil
}
func (s *promptStore) snapshot() map[string]promptMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]promptMessage{}
	for k, v := range s.items {
		out[k] = v
	}
	return out
}
func (s *promptStore) set(id string, p promptMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Message == "" {
		delete(s.items, id)
	} else {
		s.items[id] = p
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s.items)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".discord-prompts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
