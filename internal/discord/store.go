package discord

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/nicodes/stavlos/internal/protocol"
)

type promptMessage struct {
	Permission *protocol.PromptInfo `json:"permission,omitempty"`
	Questions  []protocol.Question  `json:"questions,omitempty"`
	Seq        int64                `json:"seq,omitempty"` // replay question outcomes after this point on reconnect
	Channel    string               `json:"channel"`
	Message    string               `json:"message"`
	Webhook    string               `json:"webhook,omitempty"` // empty for bot messages from older bridges or permission/trust prompts
}

func (m promptMessage) withPrompt(p protocol.PromptInfo, seq int64) promptMessage {
	switch p.Kind {
	case protocol.PromptQuestion:
		m.Questions, m.Seq = promptQuestions(p), seq
	case protocol.PromptPermission:
		m.Permission, m.Seq = &p, seq
	case protocol.PromptTrust:
	}
	return m
}

func (m promptMessage) missingPrompt(p protocol.PromptInfo) bool {
	return p.Kind == protocol.PromptPermission && m.Permission == nil || p.Kind == protocol.PromptQuestion && len(m.Questions) == 0
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
