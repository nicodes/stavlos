package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/paths"
)

// Discord configures the daemon-managed Discord integration. Token is either a literal
// token or an environment reference, left unexpanded by shared config loading.
type Discord struct {
	Enabled   bool     `json:"enabled,omitempty"` // reconnect automatically with the daemon
	Token     string   `json:"token"`
	Guild     string   `json:"guild"`
	Category  string   `json:"category"`
	Approvers []string `json:"approvers"`
	Dirs      []string `json:"dirs"`
}

// LoadDiscord reads only the global bridge configuration, keeping token
// references unexpanded until the service attempts to connect.
func LoadDiscord() (*Discord, error) {
	globalWriteMu.Lock()
	defer globalWriteMu.Unlock()
	f, err := readFile(filepath.Join(paths.ConfigDir(), "stavlos.json"))
	return f.Discord, err
}

// Resolve validates bridge configuration and returns a copy with the token
// expanded if it is an environment reference. Only the bridge calls this.
func (d Discord) Resolve() (Discord, error) {
	d.Token = strings.TrimSpace(d.Token)
	if strings.Contains(d.Token, "${") {
		if envRef.FindString(d.Token) != d.Token {
			return d, errors.New("discord.token must be a literal token or a complete ${env:NAME} reference")
		}
		d.Token = strings.TrimSpace(ExpandEnv(d.Token))
	}
	if d.Token == "" || d.Guild == "" || d.Category == "" || len(d.Approvers) == 0 || len(d.Dirs) == 0 {
		return d, errors.New("discord requires a nonempty token, guild, category, approvers and dirs")
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
		p := expandPath(dir)
		if !filepath.IsAbs(p) {
			return d, fmt.Errorf("discord directory %q must be absolute (or start with ~/)", dir)
		}
		p, err := filepath.EvalSymlinks(p)
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
