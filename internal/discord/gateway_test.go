package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dg "github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

func TestGatewayHandshakeCanBeCancelled(t *testing.T) {
	accepted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		close(accepted)
		// Never send HELLO: SDK Open would otherwise wait forever here.
		_, _, _ = c.ReadMessage()
	}))
	defer server.Close()
	s, err := dg.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		b, _ := json.Marshal(map[string]string{"url": strings.Replace(server.URL, "http://", "ws://", 1)})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		closeGateway, err := openGateway(ctx, s)
		if closeGateway != nil {
			closeGateway()
		}
		done <- err
	}()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not connect")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled handshake succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not interrupt the handshake")
	}
	if s.ShouldReconnectOnError {
		t.Fatal("SDK can reconnect outside service lifetime")
	}
}
