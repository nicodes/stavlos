package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/paths"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

// The switch writes one field of the global stavlos.json, keeps the rest of
// the file, and the channels that are open run under it at once.
func TestTheSandboxSwitch(t *testing.T) {
	setupConfig(t)
	cfgFile := filepath.Join(paths.ConfigDir(), "stavlos.json")
	before, _ := os.ReadFile(cfgFile)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	st, err := rpc.Do(ctx, h.c, protocol.SandboxStatusMethod, protocol.None{})
	if err != nil || !st.Enabled || st.Level == "" || st.Level == "landlock" {
		t.Fatalf("status before: %+v %v", st, err)
	}
	if st.Level == "full" && len(st.Missing) != 0 || st.Level != "full" && len(st.Missing) == 0 {
		t.Fatalf("what is missing does not go with the level: %+v", st)
	}
	st, err = rpc.Do(ctx, h.c, protocol.SandboxSet, protocol.SandboxSetParams{Enabled: false})
	if err != nil || st.Enabled {
		t.Fatalf("turning it off: %+v %v", st, err)
	}
	after, _ := os.ReadFile(cfgFile)
	if !strings.Contains(string(after), `"enabled": false`) && !strings.Contains(string(after), `"enabled":false`) {
		t.Fatalf("the setting was not saved:\n%s", after)
	}
	for _, kept := range []string{`"model":"fake/m1"`, `"echo*":"allow"`} { // what the harness's config already said
		if strings.Contains(string(before), kept) && !strings.Contains(string(after), kept) {
			t.Fatalf("the rest of the file was not kept: %s is gone from\n%s", kept, after)
		}
	}
	list, err := rpc.Do(ctx, h.c, protocol.ChannelList, protocol.ChannelListParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list.Channels {
		if c.ID == s.ID && c.Sandbox != "off" {
			t.Fatalf("the open channel still says sandbox %q", c.Sandbox)
		}
	}
	if st, err = rpc.Do(ctx, h.c, protocol.SandboxSet, protocol.SandboxSetParams{Enabled: true}); err != nil || !st.Enabled {
		t.Fatalf("turning it back on: %+v %v", st, err)
	}

	// a bridge, or a browser, is not who decides this
	h.d.TreatInProcessAsBridge()
	c, err := rpc.Dial(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := rpc.Do(ctx, c, protocol.Attach, protocol.AttachParams{Client: "discord", Tier: protocol.TierFallback}); err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.Do(ctx, c, protocol.SandboxSet, protocol.SandboxSetParams{Enabled: false}); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("a bridge turned the sandbox off: %v", err)
	}
}
