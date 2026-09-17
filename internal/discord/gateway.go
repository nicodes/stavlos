package discord

import (
	"context"
	"net"
	"sync"
	"time"

	dg "github.com/bwmarrin/discordgo"
)

// openGateway binds the SDK's otherwise unbounded handshake to our context.
// Reconnection belongs to Service: the SDK's reconnect loop cannot be stopped
// once entered, which would leave an orphan connection after /discord disconnect.
func openGateway(ctx context.Context, s *dg.Session) (func(), error) {
	s.ShouldReconnectOnError = false
	var mu sync.Mutex
	var conn net.Conn
	dialer := *s.Dialer
	dialer.HandshakeTimeout = 20 * time.Second
	dialer.NetDialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: 20 * time.Second}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		mu.Lock()
		conn = c
		mu.Unlock()
		if ctx.Err() != nil {
			_ = c.Close()
			return nil, ctx.Err()
		}
		return c, nil
	}
	s.Dialer = &dialer
	closeConn := func() {
		mu.Lock()
		defer mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	}
	stop := context.AfterFunc(ctx, closeConn)
	closeGateway := func() { stop(); closeConn(); _ = s.Close() }
	if err := s.Open(); err != nil {
		closeGateway()
		return nil, apiError(err)
	}
	if ctx.Err() != nil {
		closeGateway()
		return nil, ctx.Err()
	}
	mu.Lock()
	if conn != nil {
		_ = conn.SetDeadline(time.Time{})
	}
	mu.Unlock()
	return closeGateway, nil
}
