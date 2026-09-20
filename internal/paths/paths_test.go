package paths

import (
	"os"
	"strings"
	"testing"
)

// TestATestNeverReachesTheUsersOwnDirectories: with nothing set, a test
// binary's config, data, cache and socket are under the temp directory, never
// under the home directory or the runtime directory a real daemon uses. An
// explicit STAVLOS_* setting is still honoured.
func TestATestNeverReachesTheUsersOwnDirectories(t *testing.T) {
	for _, env := range []string{"STAVLOS_CONFIG_DIR", "STAVLOS_DATA_DIR", "STAVLOS_CACHE_DIR", "STAVLOS_SOCKET"} {
		t.Setenv(env, "")
	}
	home, _ := os.UserHomeDir()
	for name, got := range map[string]string{"config": ConfigDir(), "data": DataDir(), "cache": CacheDir(), "socket": Socket(), "auth": AuthFile()} {
		if !strings.HasPrefix(got, os.TempDir()) || (home != "" && strings.HasPrefix(got, home+"/.")) || strings.HasPrefix(got, "/run/") {
			t.Errorf("%s resolves to %q under test", name, got)
		}
	}
	t.Setenv("STAVLOS_DATA_DIR", "/somewhere/else")
	if DataDir() != "/somewhere/else" {
		t.Fatalf("an explicit directory is not honoured: %q", DataDir())
	}
}
