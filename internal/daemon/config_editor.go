package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/protocol"
)

func (d *Daemon) editorRoot(scope protocol.ConfigScope, expected string) (root, dir string, err error) {
	switch scope.Scope {
	case "system":
		root = paths.ConfigDir()
	case "project":
		channel, err := d.channel(scope.Channel)
		if err != nil {
			return "", "", err
		}
		dir = channel.Dir()
		root = paths.ProjectDir(dir)
	default:
		return "", "", fmt.Errorf("config scope must be project or system")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	if real, e := filepath.EvalSymlinks(root); e == nil {
		root = real
	} else if !os.IsNotExist(e) {
		return "", "", e
	}
	if expected != "" && expected != root {
		return "", "", fmt.Errorf("configuration directory changed; reopen settings")
	}
	return root, dir, nil
}

func (d *Daemon) configList(p protocol.ConfigScope) (protocol.ConfigTree, error) {
	root, _, err := d.editorRoot(p, "")
	if err != nil {
		return protocol.ConfigTree{}, err
	}
	return config.EditorTree(root)
}

func (d *Daemon) configRead(p protocol.ConfigFileParams) (protocol.ConfigDocument, error) {
	root, _, err := d.editorRoot(p.ConfigScope, p.Root)
	if err != nil {
		return protocol.ConfigDocument{}, err
	}
	return config.EditorRead(root, p.Path)
}

func (d *Daemon) configEdit(ctx context.Context, p protocol.ConfigEditParams) (protocol.ConfigEditResult, error) {
	d.editorMu.Lock()
	defer d.editorMu.Unlock()
	var result protocol.ConfigEditResult
	if p.Root == "" {
		return result, fmt.Errorf("open the config directory before editing")
	}
	root, dir, err := d.editorRoot(p.ConfigScope, p.Root)
	if err != nil {
		return result, err
	}
	trusted := false
	if dir != "" {
		_, hash, err := config.ProjectHash(dir)
		if err != nil {
			return result, err
		}
		trusted = hash == "" || d.trust.Trusted(dir, hash)
	}
	result.Tree, err = config.EditorApply(root, p.Scope == "system", p)
	if err != nil {
		return result, err
	}
	result.Notice = "Saved to disk. "
	if dir != "" && trusted {
		_, hash, err := config.ProjectHash(dir)
		if err == nil && hash != "" {
			err = d.trust.set(ctx, dir, hash)
		}
		if err != nil {
			result.Notice += "Trust update failed: " + err.Error() + ". "
		}
	}
	result.Notice += d.reloadEditedConfig(p.ConfigScope, dir)
	path := p.Path
	if p.Action == "rename" {
		path = p.Destination
	}
	if p.Action != "delete" {
		if doc, err := config.EditorRead(root, path); err == nil {
			result.Document = &doc
		}
	}
	return result, nil
}

func (d *Daemon) reloadEditedConfig(scope protocol.ConfigScope, dir string) string {
	var problems []string
	if scope.Scope == "system" {
		if cfg, err := config.LoadGlobal(); err != nil {
			problems = append(problems, err.Error())
		} else {
			d.esc.SetConfig(escalation.Config{ClaimTimeout: cfg.Escalation.ClaimTimeout, AnswerTimeout: cfg.Escalation.AnswerTimeout, Default: string(cfg.Escalation.Default)})
		}
	}
	for _, channel := range d.channelList() {
		if scope.Scope == "project" && channel.Dir() != dir {
			continue
		}
		cfg, err := config.Load(channel.Dir(), d.trust)
		if err != nil {
			problems = append(problems, channel.Name()+": "+err.Error())
			continue
		}
		channel.SetConfig(cfg)
		d.maybeTrustPrompt(channel)
		if cfg.TrustPending {
			problems = append(problems, channel.Name()+": project trust required")
		}
	}
	if len(problems) > 0 {
		return "Reload: " + strings.Join(problems, "; ")
	}
	return "Reloaded. Policies/roles/commands: next step. Model/mode/root defaults: new channels. Escalation: new prompts. Discord: reconnect. Plugins: restart."
}
