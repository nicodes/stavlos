package tools

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorFixedProtocolAndBoundedOutcome(t *testing.T) {
	body, err := operatorMessage(json.RawMessage(`{"grant":"` + strings.Repeat("a", 32) + `","operation":"merge","repository":"owner/repo","head":"` + strings.Repeat("b", 40) + `","pull_request":3}`))
	if err != nil {
		t.Fatal(err)
	}
	path := operatorSocketFixture(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte, 1)
	go func() {
		connection, e := listener.Accept()
		if e != nil {
			done <- nil
			return
		}
		defer connection.Close()
		request, _ := io.ReadAll(io.LimitReader(connection, 4097))
		done <- request
		_, _ = connection.Write([]byte(`{"ok":true,"result":{"status":"source_merged"}}`))
	}()
	result := operatorCall(context.Background(), path, body, os.Getuid())
	if result.IsError || result.Output != `{"status":"source_merged"}` {
		t.Fatal(result)
	}
	var received map[string]json.RawMessage
	if json.Unmarshal(<-done, &received) != nil || len(received) != 2 || received["grant"] == nil || received["request"] == nil {
		t.Fatal(received)
	}
}

func TestOperatorRejectsUnscopedShellSocketAndDifferentRevision(t *testing.T) {
	grant := `"grant":"` + strings.Repeat("a", 32) + `"`
	for _, input := range []string{
		`{` + grant + `,"operation":"shell","command":"true"}`,
		`{` + grant + `,"operation":"host_report","host":"approved.test","socket":"/tmp/foreign"}`,
		`{` + grant + `,"operation":"merge","repository":"owner/repo","head":"short","pull_request":1}`,
		`{` + grant + `,"operation":"host_report","host":"approved.test","head":"` + strings.Repeat("b", 40) + `"}`,
	} {
		if _, err := operatorMessage(json.RawMessage(input)); err == nil {
			t.Fatal("unscoped request accepted", input)
		}
	}
}

func TestOperatorRejectsForeignPeerBeforeSendingGrant(t *testing.T) {
	path := operatorSocketFixture(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	received := make(chan []byte, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			received <- nil
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		received <- b
	}()
	result := operatorCall(context.Background(), path, []byte(`{"grant":"never-disclose"}`), os.Getuid()+1)
	if !result.IsError || !strings.Contains(result.Output, "identity differs") {
		t.Fatal(result)
	}
	if b := <-received; len(b) != 0 {
		t.Fatalf("grant disclosed to foreign peer: %d bytes", len(b))
	}
}

func TestOperatorCancellationClosesConnection(t *testing.T) {
	path := operatorSocketFixture(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan struct{})
	go func() {
		c, e := listener.Accept()
		if e != nil {
			close(closed)
			return
		}
		defer c.Close()
		_, _ = io.ReadAll(c)
		cancel()
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	}()
	result := operatorCall(ctx, path, []byte(`{}`), os.Getuid())
	if !result.IsError {
		t.Fatal("cancelled operation accepted", result)
	}
	<-closed
}

func operatorSocketFixture(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "operator-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "operator.sock")
}
