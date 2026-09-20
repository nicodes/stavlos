package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/nicodes/stavlos/internal/instructions"
	"github.com/nicodes/stavlos/internal/paths"
)

// A project's trust-gated files are read once. The hash a human is asked to
// trust and the configuration that is then loaded used to come from two
// reads of the same paths, so a file that changed in between was loaded
// under a hash that never covered it; a Snapshot is those bytes, and both
// the hash and every loader work from it.

// What a project layer may hold. A clone is untrusted until the human says
// otherwise, and hashing it happens before they are asked: without bounds a
// repository could stall or exhaust the daemon just by being opened.
const (
	snapshotMaxFile  = 16 << 20
	snapshotMaxTotal = 64 << 20
	snapshotMaxFiles = 4096
)

// Snapshot is a project's trust-gated files as they were read, once.
type Snapshot struct {
	Dir   string
	Files []string // relative to Dir, sorted
	Hash  string   // "" when there are no files
	data  map[string][]byte
	chain []instructions.File // root → Dir, as the agents are given them
}

// source is where a layer's loaders read from: the disk for the global
// layer, a Snapshot for a project's.
type source interface {
	ReadFile(path string) ([]byte, error)
	ReadDir(dir string) ([]fs.DirEntry, error)
}

type disk struct{}

func (disk) ReadFile(path string) ([]byte, error)      { return os.ReadFile(path) }
func (disk) ReadDir(dir string) ([]fs.DirEntry, error) { return os.ReadDir(dir) }

// TakeSnapshot reads the trust-gated files of dir: .stavlos/**, and the
// instructions files agents follow there (PRD §10.6).
func TakeSnapshot(dir string) (*Snapshot, error) {
	s := &Snapshot{Dir: dir, data: map[string][]byte{}}
	total := 0
	add := func(abs string) error {
		rel, _ := filepath.Rel(dir, abs)
		if _, ok := s.data[rel]; ok {
			return nil
		}
		if len(s.data) >= snapshotMaxFiles {
			return fmt.Errorf("the project's configuration has more than %d files", snapshotMaxFiles)
		}
		b, err := readRegular(abs, snapshotMaxFile)
		if err != nil {
			return err
		}
		if total += len(b); total > snapshotMaxTotal {
			return fmt.Errorf("the project's configuration is larger than %d MB", snapshotMaxTotal>>20)
		}
		s.data[rel] = b
		return nil
	}
	err := filepath.WalkDir(paths.ProjectDir(dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		return add(p)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// The instructions agents will follow: the files from the repository
	// root down to dir, and those below it that reach agents as they work.
	for _, f := range instructions.Chain(dir) {
		if err := add(f.Path); err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(dir, f.Path)
		s.chain = append(s.chain, instructions.File{Path: f.Path, Text: string(s.data[rel])})
	}
	for _, p := range instructions.Nested(dir) {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	if len(s.data) == 0 {
		return s, nil
	}
	for f := range s.data {
		s.Files = append(s.Files, f)
	}
	sort.Strings(s.Files)
	h := sha256.New()
	for _, f := range s.Files {
		fmt.Fprintf(h, "%s\x00%d\x00", f, len(s.data[f]))
		h.Write(s.data[f])
	}
	s.Hash = hex.EncodeToString(h.Sum(nil))[:32]
	return s, nil
}

// readRegular reads a regular file (a symlink to one counts: CLAUDE.md is
// often a link to AGENTS.md) of at most max bytes. A FIFO, which would block
// the read for ever, a device or a socket is refused before it is opened
// for reading; O_NONBLOCK covers one swapped in after the check.
func readRegular(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d MB", path, max>>20)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("%s is larger than %d MB", path, max>>20)
	}
	return b, err
}

// ProjectHash is the file list and hash of dir's trust-gated files.
func ProjectHash(dir string) ([]string, string, error) {
	s, err := TakeSnapshot(dir)
	if err != nil {
		return nil, "", err
	}
	return s.Files, s.Hash, nil
}

func (s *Snapshot) rel(path string) (string, bool) {
	rel, err := filepath.Rel(s.Dir, path)
	return rel, err == nil
}

func (s *Snapshot) ReadFile(path string) ([]byte, error) {
	if rel, ok := s.rel(path); ok {
		if b, ok := s.data[rel]; ok {
			return b, nil
		}
	}
	return nil, &fs.PathError{Op: "read", Path: path, Err: fs.ErrNotExist}
}

// ReadDir lists what the snapshot holds directly under dir, sorted by name
// as os.ReadDir does.
func (s *Snapshot) ReadDir(dir string) ([]fs.DirEntry, error) {
	rel, ok := s.rel(dir)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrNotExist}
	}
	prefix := rel + string(filepath.Separator)
	seen := map[string]bool{}
	var out []fs.DirEntry
	for _, f := range s.Files {
		rest, ok := strings.CutPrefix(f, prefix)
		if !ok {
			continue
		}
		name, _, below := strings.Cut(rest, string(filepath.Separator))
		if !seen[name] {
			seen[name] = true
			out = append(out, snapshotEntry{name: name, dir: below})
		}
	}
	if len(out) == 0 {
		return nil, &fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrNotExist}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

type snapshotEntry struct {
	name string
	dir  bool
}

func (e snapshotEntry) Name() string { return e.name }
func (e snapshotEntry) IsDir() bool  { return e.dir }
func (e snapshotEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e snapshotEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }
