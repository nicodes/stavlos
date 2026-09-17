package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDiscordEnabledPreservesConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", dir)
	path := filepath.Join(dir, "stavlos.json")
	input := `{
  // "discord": { "enabled": false } is an example, not the real block
  "model": "openai/test",
  "discord": {
    "token": "a-secret-with-\"enabled\":false-and-//-inside",
    "guild": "123",
    "category": "stavlos",
    "approvers": ["456"],
    "dirs": ["/tmp"], // keep this trailing comment
  },
}`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false, true} {
		if err := SetDiscordEnabled(enabled); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadDiscord()
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil || cfg.Enabled != enabled || cfg.Token != `a-secret-with-"enabled":false-and-//-inside` {
			t.Fatal("configuration or token changed")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, keep := range []string{`// "discord": { "enabled": false } is an example`, `"model": "openai/test"`, `// keep this trailing comment`} {
			if !strings.Contains(string(b), keep) {
				t.Fatalf("lost %q", keep)
			}
		}
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("private config mode: %v %v", st, err)
		}
	}
}

func TestDiscordEnabledEditingShapes(t *testing.T) {
	for _, input := range []string{
		`{"discord":{"token":"literal"}}`,
		`{"discord":{}}`,
		`{"discord":{"enabled":false,"token":"${env:DISCORD_TOKEN}"}}`,
		`{"discord":{"enabled":false,"enabled":true}}`,
		"{\n\t\"discord\": {/* note */ \"enabled\": true, /* note */},\n}",
	} {
		out, err := editDiscordEnabled([]byte(input), false)
		if err != nil {
			t.Fatal(err)
		}
		var f File
		if err := json.Unmarshal(StripJSONC(out), &f); err != nil || f.Discord == nil || f.Discord.Enabled {
			t.Fatalf("invalid edit: %s (%v)", out, err)
		}
	}
	for _, input := range []string{`{`, `{"discord":"wrong"}`, `{"discord":{}} /* unfinished`} {
		if _, err := editDiscordEnabled([]byte(input), true); err == nil {
			t.Fatalf("invalid JSON accepted: %s", input)
		}
	}
	if _, err := editDiscordEnabled([]byte(`{}`), true); err == nil {
		t.Fatal("enabled an unconfigured integration")
	}
	if out, err := editDiscordEnabled([]byte(`{}`), false); err != nil || string(out) != `{}` {
		t.Fatal("disconnect should allow missing configuration")
	}
}

func TestDiscordEnabledPreservesConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", dir)
	target := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(target, []byte(`{"discord":{"token":"test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stavlos.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := SetDiscordEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(path); err != nil {
		t.Fatal("replaced the config symlink", err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := SetGlobalModel("openai/new"); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := SetDiscordEnabled(false); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	f, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Model != "openai/new" || f.Discord.Enabled || f.Discord.Token != "test" {
		t.Fatal("concurrent config writes lost a setting")
	}
}
