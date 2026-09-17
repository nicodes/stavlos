package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscordGlobalReferenceAndProjectRejection(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("DISCORD_TOKEN", "") // daemon does not need the bridge's credential
	if err := os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"discord":{"token":"${env:DISCORD_TOKEN}","guild":"123","category":"stavlos","approvers":["456"],"dirs":["/tmp"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	if e.Discord == nil || e.Discord.Token != "${env:DISCORD_TOKEN}" {
		t.Fatalf("token was expanded: %+v", e.Discord)
	}
	for _, layer := range []string{"project", "local"} {
		if err := e.applyFile(File{Discord: e.Discord}, layer); err == nil || !strings.Contains(err.Error(), "global-only") {
			t.Fatalf("%s: %v", layer, err)
		}
	}
	for _, name := range []string{"stavlos.json", "stavlos.local.json"} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".stavlos"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".stavlos", name), []byte(`{"discord":{"guild":"999"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir, allTrust{}); err == nil || !strings.Contains(err.Error(), "global-only") {
			t.Fatalf("trusted %s: %v", name, err)
		}
	}
	if _, err := e.Discord.Resolve(); err == nil {
		t.Fatal("bridge accepted missing token")
	}
	t.Setenv("DISCORD_TOKEN", "test-token")
	d, err := e.Discord.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if d.Token != "test-token" || e.Discord.Token != "${env:DISCORD_TOKEN}" {
		t.Fatal("resolve mutated shared config")
	}
}

func TestDiscordValidationAndCanonicalPaths(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "test-token")
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	base := Discord{Token: "${env:DISCORD_TOKEN}", Guild: "123", Category: "stavlos", Approvers: []string{"456"}, Dirs: []string{link}}
	d, err := base.Resolve()
	if err != nil || d.Dirs[0] != real {
		t.Fatalf("%+v %v", d, err)
	}
	for _, edit := range []func(*Discord){
		func(d *Discord) { d.Token = "" },
		func(d *Discord) { d.Token = "  " },
		func(d *Discord) { d.Token = "${env:DISCORD_TOKEN" },
		func(d *Discord) { d.Token += "suffix" },
		func(d *Discord) { d.Guild = "invalid" },
		func(d *Discord) { d.Approvers = nil },
		func(d *Discord) { d.Dirs = nil },
		func(d *Discord) { d.Dirs = []string{""} },
		func(d *Discord) { d.Dirs = []string{"."} },
	} {
		d := base
		edit(&d)
		if _, err := d.Resolve(); err == nil {
			t.Fatalf("accepted invalid config %+v", d)
		}
	}
}

func TestDiscordLiteralTokenFromGlobalConfig(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "")
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	if err := os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"discord":{"token":"  saved-test-token  ","guild":"123","category":"stavlos","approvers":["456"],"dirs":["/tmp"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.Discord.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if d.Token != "saved-test-token" {
		t.Fatal("literal token was not loaded")
	}
	if e.Discord.Token != "  saved-test-token  " {
		t.Fatal("resolve mutated shared config")
	}
}
