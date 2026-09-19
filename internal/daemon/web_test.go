package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

// TestWebUIIsScopedAndRemembered turns the web UI on over the socket, signs a
// browser in with the code it hands out, and checks what that browser may
// call: reading and posting, never what loosens an agent's permissions.
func TestWebUIIsScopedAndRemembered(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	t.Setenv("STAVLOS_CACHE_DIR", t.TempDir())
	if err := os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(fmt.Sprintf(`{"model":"fake/m1","reminders":false,"web":{"port":%d}}`, port)), 0o600); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	h := newHarness(t, data, &fakeModel{})
	ctx := context.Background()
	if st, err := rpc.Do(ctx, h.c, protocol.WebStatusMethod, protocol.None{}); err != nil || st.Enabled {
		t.Fatalf("off until asked for: %+v %v", st, err)
	}
	if _, err := rpc.Do(ctx, h.c, protocol.WebOpen, protocol.None{}); err == nil {
		t.Fatal("a sign-in URL was handed out while disabled")
	}
	st, err := rpc.Do(ctx, h.c, protocol.WebEnable, protocol.None{})
	if err != nil || !st.Enabled || !strings.Contains(st.OpenURL, "#code=") {
		t.Fatalf("enable: %+v %v", st, err)
	}
	base := strings.TrimSuffix(st.URL, "/")
	code := st.OpenURL[strings.Index(st.OpenURL, "#code=")+6:]
	req, _ := http.NewRequest("POST", base+"/api/session", strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set("Origin", base)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusNoContent {
		t.Fatalf("sign-in: %v %v", res, err)
	}
	res.Body.Close()
	cookie := res.Cookies()[0]
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ws", http.Header{"Origin": {base}, "Cookie": {cookie.Name + "=" + cookie.Value}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	call := func(id int, method string, params any) protocol.Response {
		t.Helper()
		p, _ := json.Marshal(params)
		if err := ws.WriteJSON(map[string]any{"jsonrpc": "2.0", "v": protocol.Version, "id": id, "method": method, "params": json.RawMessage(p)}); err != nil {
			t.Fatal(err)
		}
		for {
			var r protocol.Response
			if err := ws.ReadJSON(&r); err != nil {
				t.Fatal(err)
			}
			if r.ID != nil && string(*r.ID) == fmt.Sprint(id) {
				return r
			}
		}
	}
	if r := call(1, protocol.MAttach, protocol.AttachParams{Client: "web", Tier: protocol.TierInteractive}); r.Error != nil {
		t.Fatalf("attach: %+v", r.Error)
	}
	if r := call(2, protocol.MChannelList, protocol.ChannelListParams{}); r.Error != nil {
		t.Fatalf("channel.list: %+v", r.Error)
	}
	for i, m := range []string{protocol.MChannelSetMode, protocol.MPromptReply, protocol.MTrustReply, protocol.MDaemonShutdown, protocol.MWebOpen, protocol.MChannelAddDir, protocol.MProviderLoginStart} {
		if r := call(10+i, m, struct{}{}); r.Error == nil || r.Error.Code != protocol.ErrForbidden {
			t.Fatalf("%s from a browser: %+v", m, r.Error)
		}
	}
	// it comes back on with the daemon, and off stays off
	h.close()
	h = newHarness(t, data, &fakeModel{})
	if st, err := rpc.Do(ctx, h.c, protocol.WebStatusMethod, protocol.None{}); err != nil || !st.Enabled {
		t.Fatalf("not restored: %+v %v", st, err)
	}
	if st, err := rpc.Do(ctx, h.c, protocol.WebDisable, protocol.None{}); err != nil || st.Enabled {
		t.Fatalf("disable: %+v %v", st, err)
	}
	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Fatal("still listening after disable")
	}
	h.close()
}
