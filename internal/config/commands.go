package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Command is a named prompt. Its filename supplies the name; description is
// the only supported frontmatter field, with no agent or model overrides.
type Command struct {
	Name, Description, Body, Source string
}

var commandName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func ReadCommand(path string) (Command, error) {
	c := Command{Name: strings.TrimSuffix(filepath.Base(path), ".md"), Source: path}
	if !commandName.MatchString(c.Name) {
		return c, fmt.Errorf("%s: command name must use lowercase letters, digits, hyphens or underscores and start with a letter", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	var meta map[string]any
	body, err := frontmatter(string(b), &meta)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	for key := range meta {
		if key != "description" {
			return c, fmt.Errorf("%s: unsupported command field %q; only description is supported", path, key)
		}
	}
	description, ok := meta["description"].(string)
	if !ok || strings.TrimSpace(description) == "" {
		return c, fmt.Errorf("%s: command frontmatter needs a nonempty description", path)
	}
	if strings.TrimSpace(body) == "" {
		return c, fmt.Errorf("%s: command needs a Markdown prompt body", path)
	}
	c.Description = strings.Join(strings.Fields(description), " ")
	c.Body = strings.Trim(body, "\r\n")
	return c, nil
}

func (e *Effective) loadCommands(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.Commands == nil {
		e.Commands = map[string]Command{}
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		c, err := ReadCommand(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		e.Commands[c.Name] = c
	}
	return nil
}
