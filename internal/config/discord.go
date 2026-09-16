package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Discord configures the standalone Discord bridge. Token is a reference,
// deliberately left unexpanded by the daemon's configuration loader.
type Discord struct {
	Token     string   `json:"token"`
	Guild     string   `json:"guild"`
	Category  string   `json:"category"`
	Approvers []string `json:"approvers"`
	Dirs      []string `json:"dirs"`
}

// Resolve validates bridge configuration and returns a copy with the token
// expanded. Only the bridge calls this; the daemon needs no Discord secret.
func (d Discord) Resolve() (Discord, error) {
	if envRef.FindString(d.Token) != d.Token || d.Token == "" {
		return d, errors.New("discord.token must be a ${env:NAME} reference")
	}
	d.Token = ExpandEnv(d.Token)
	if d.Token == "" || d.Guild == "" || d.Category == "" || len(d.Approvers) == 0 || len(d.Dirs) == 0 {
		return d, errors.New("discord requires a nonempty token environment variable, guild, category, approvers and dirs")
	}
	for _, id := range append([]string{d.Guild}, d.Approvers...) {
		if id == "" {
			return d, errors.New("discord IDs must be numeric")
		}
		for _, c := range id {
			if c < '0' || c > '9' {
				return d, errors.New("discord IDs must be numeric")
			}
		}
	}
	d.Dirs = append([]string(nil), d.Dirs...)
	for i, dir := range d.Dirs {
		if dir == "" {
			return d, errors.New("discord.dirs contains an empty path")
		}
		p, err := filepath.Abs(expandPath(dir))
		if err == nil {
			p, err = filepath.EvalSymlinks(p)
		}
		if err != nil {
			return d, fmt.Errorf("discord directory %q: %w", dir, err)
		}
		st, err := os.Stat(p)
		if err != nil {
			return d, err
		}
		if !st.IsDir() {
			return d, fmt.Errorf("discord directory %q is not a directory", dir)
		}
		d.Dirs[i] = p
	}
	return d, nil
}
