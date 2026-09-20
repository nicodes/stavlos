package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/statefile"
)

const editorMaxFile = 4 << 20

func editorPath(root, path string) (string, error) {
	root = filepath.Clean(root)
	if !filepath.IsLocal(path) || filepath.Clean(path) == "." {
		return "", fmt.Errorf("choose a relative path inside the config directory")
	}
	path = filepath.Join(root, filepath.FromSlash(path))
	for p := path; p != root; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlinked paths are not editable: %s", p)
		}
	}
	return path, nil
}

func EditorTree(root string) (protocol.ConfigTree, error) {
	tree := protocol.ConfigTree{Root: root}
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		tree.Files = append(tree.Files, protocol.ConfigEntry{Path: filepath.ToSlash(rel), Directory: entry.IsDir()})
		fmt.Fprintf(h, "%s\x00%v\x00", rel, entry.Type())
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			h.Write([]byte(target))
			return nil
		}
		st, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%d\x00", st.Size())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	tree.Revision = hex.EncodeToString(h.Sum(nil))
	return tree, err
}

func EditorRead(root, relative string) (protocol.ConfigDocument, error) {
	doc := protocol.ConfigDocument{Path: filepath.ToSlash(filepath.Clean(relative))}
	path, err := editorPath(root, relative)
	if err != nil {
		return doc, err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > editorMaxFile {
		return doc, fmt.Errorf("text editing is limited to 4 MiB")
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		b = []byte(EditorTemplate(relative))
		err = nil
	} else if err == nil {
		doc.Exists = true
	}
	if err != nil {
		return doc, err
	}
	if len(b) > editorMaxFile || !utf8.Valid(b) || bytes.ContainsRune(b, 0) {
		return doc, fmt.Errorf("file is not editable UTF-8 text (maximum 4 MiB)")
	}
	doc.Content = string(b)
	doc.Fields = EditorFields(doc.Path, doc.Content)
	return doc, nil
}

func EditorTemplate(path string) string {
	switch {
	case path == "stavlos.json" || path == "stavlos.local.json":
		return "{\n}\n"
	case strings.HasPrefix(path, "commands/") && strings.HasSuffix(path, ".md"):
		return "---\ndescription: Describe this command\n---\n\nWrite the prompt here.\n"
	case strings.HasPrefix(path, "agents/") && strings.HasSuffix(path, ".md"):
		return "---\ndescription: Describe this agent\ntype: all\n---\n\nDescribe the agent's role here.\n"
	case strings.HasPrefix(path, "skills/") && filepath.Base(path) == "SKILL.md":
		return "---\nname: " + filepath.Base(filepath.Dir(path)) + "\ndescription: Describe this skill\n---\n\nWrite the skill instructions here.\n"
	}
	return ""
}

// EditorApply validates a shadow copy before touching the real files. Revision
// checking detects edits made by another client/editor, and writes are atomic.
func EditorApply(root string, system bool, p protocol.ConfigEditParams) (protocol.ConfigTree, error) {
	globalWriteMu.Lock()
	defer globalWriteMu.Unlock()
	tree, err := EditorTree(root)
	if err != nil {
		return tree, err
	}
	if p.Revision == "" || tree.Revision != p.Revision {
		return tree, fmt.Errorf("config files changed on disk; reload before saving")
	}
	path, err := editorPath(root, p.Path)
	if err != nil {
		return tree, err
	}
	p.Path = filepath.ToSlash(filepath.Clean(p.Path))
	if p.Destination != "" {
		p.Destination = filepath.ToSlash(filepath.Clean(p.Destination))
	}
	var destination string
	if p.Action == "rename" {
		destination, err = editorPath(root, p.Destination)
		if err != nil {
			return tree, err
		}
		if _, err := os.Lstat(destination); !errors.Is(err, fs.ErrNotExist) {
			return tree, fmt.Errorf("destination already exists or is unavailable")
		}
	}
	if len(p.FieldPath) > 0 {
		doc, err := EditorRead(root, p.Path)
		if err != nil {
			return tree, err
		}
		p.Content, err = EditConfigField(p.Path, doc.Content, p.FieldPath, p.FieldValue)
		if err != nil {
			return tree, err
		}
	}
	if len(p.Content) > editorMaxFile {
		return tree, fmt.Errorf("config file exceeds 4 MiB")
	}
	if p.Action == "write" && (!utf8.ValidString(p.Content) || strings.ContainsRune(p.Content, 0)) {
		return tree, fmt.Errorf("config editor writes UTF-8 text without NUL bytes")
	}
	preview, err := os.MkdirTemp("", "stavlos-config-preview-*")
	if err != nil {
		return tree, err
	}
	defer os.RemoveAll(preview)
	if err := copyEditorTree(root, preview); err != nil {
		return tree, err
	}
	if err := mutateEditorTree(preview, p); err != nil {
		return tree, err
	}
	if err := validateEditorTree(preview, system); err != nil {
		return tree, fmt.Errorf("%s", strings.ReplaceAll(err.Error(), preview, root))
	}
	again, err := EditorTree(root)
	if err != nil || again.Revision != tree.Revision {
		return tree, fmt.Errorf("config files changed during validation; reload before saving")
	}
	switch p.Action {
	case "write":
		err = atomicConfigWrite(path, []byte(p.Content))
	case "rename":
		if err = os.MkdirAll(filepath.Dir(destination), 0o700); err == nil {
			err = os.Rename(path, destination)
		}
	case "delete":
		err = removeConfigPath(root, path)
	default:
		err = fmt.Errorf("unknown config operation %q", p.Action)
	}
	if err != nil {
		return tree, err
	}
	return EditorTree(root)
}

func removeConfigPath(root, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return os.Remove(path)
	}
	trash, err := os.MkdirTemp(filepath.Dir(root), ".stavlos-delete-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(trash)
	return os.Rename(path, filepath.Join(trash, "removed"))
}

func copyEditorTree(root, to string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			return os.Symlink(target, out)
		}
		return copyEditorFile(path, out)
	})
}

func copyEditorFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func mutateEditorTree(root string, p protocol.ConfigEditParams) error {
	path := filepath.Join(root, p.Path)
	switch p.Action {
	case "write":
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(p.Content), 0o600)
	case "delete":
		if _, err := os.Lstat(path); err != nil {
			return err
		}
		return os.RemoveAll(path)
	case "rename":
		to := filepath.Join(root, p.Destination)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		return os.Rename(path, to)
	}
	return fmt.Errorf("unknown config operation %q", p.Action)
}

func validateEditorTree(root string, system bool) error {
	for _, name := range []string{"stavlos.json", "stavlos.local.json"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := maskJSONC(b); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if system {
		_, err := loadGlobalFrom(root)
		return err
	}
	e, err := LoadGlobal()
	if err != nil {
		return err
	}
	for _, name := range []string{"stavlos.json", "stavlos.local.json"} {
		f, err := readFile(disk{}, filepath.Join(root, name))
		if err != nil {
			return err
		}
		if err := e.applyFile(f, "project"); err != nil {
			return err
		}
	}
	e.allowSearch()
	if err := e.loadRoles(disk{}, filepath.Join(root, "agents"), "project"); err != nil {
		return err
	}
	if err := e.loadSkills(disk{}, filepath.Join(root, "skills")); err != nil {
		return err
	}
	return e.loadCommands(disk{}, filepath.Join(root, "commands"))
}

func atomicConfigWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return statefile.WriteAtomic(path, data, 0o600, true)
}
