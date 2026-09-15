package proc

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnvScrub(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-1")
	t.Setenv("MY_APIKEY", "x")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	t.Setenv("AWS_CHANNEL_TOKEN", "x")
	t.Setenv("GITHUB_TOKEN", "gh")
	t.Setenv("DB_PASSWORD", "x")
	t.Setenv("http_passwd", "x")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "x")
	t.Setenv("SSH_PRIVATE_KEY", "x")
	t.Setenv("STAVLOS_WEB_ALLOW_LOCAL", "1")
	t.Setenv("SSH_AUTH_SOCK", "/run/ssh")
	t.Setenv("GOPATH", "/go")
	t.Setenv("TOKENIZER_PARALLELISM", "false") // contains TOKEN: scrubbed, and listed as the price of a simple rule
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1/bus")
	t.Setenv("MY_ODD_CREDS", "x") // no telltale name: the allowlist keeps it out
	t.Setenv("NPM_CONFIG__AUTH", "x")
	t.Setenv("GOOGLE_API_KEY", "x")
	t.Setenv("GIT_AUTHOR_NAME", "me")
	env := Env([]string{"GITHUB_TOKEN"}, "EXTRA=1")
	has := func(name string) bool {
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				return true
			}
		}
		return false
	}
	for _, gone := range []string{"OPENAI_API_KEY", "MY_APIKEY", "AWS_SECRET_ACCESS_KEY", "AWS_CHANNEL_TOKEN", "DB_PASSWORD", "http_passwd", "GOOGLE_APPLICATION_CREDENTIALS", "SSH_PRIVATE_KEY", "STAVLOS_WEB_ALLOW_LOCAL", "TOKENIZER_PARALLELISM", "SSH_AUTH_SOCK", "DBUS_SESSION_BUS_ADDRESS", "MY_ODD_CREDS", "NPM_CONFIG__AUTH", "GOOGLE_API_KEY"} {
		if has(gone) {
			t.Errorf("%s should be scrubbed", gone)
		}
	}
	for _, kept := range []string{"GITHUB_TOKEN", "GOPATH", "PATH", "EXTRA", "GIT_AUTHOR_NAME"} {
		if !has(kept) {
			t.Errorf("%s should be kept", kept)
		}
	}
	if !Passes("HOME") || Passes("X_TOKEN") || Passes("STAVLOS_CONFIG_DIR") {
		t.Error("Passes")
	}
}

func TestStartKillTail(t *testing.T) {
	var mu sync.Mutex
	var streamed strings.Builder
	j, err := Start("echo one; echo two; sleep 30", t.TempDir(), Env(nil), func(s string) { mu.Lock(); streamed.WriteString(s); mu.Unlock() }, nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for j.Lines() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := streamed.String()
	mu.Unlock()
	if j.Output() != "one\ntwo\n" || got != "one\ntwo\n" {
		t.Fatalf("output %q streamed %q", j.Output(), got)
	}
	j.Detach()
	j.Kill()
	select {
	case <-j.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("kill should end the process group")
	}
	if j.Err() == nil || j.Started().IsZero() {
		t.Fatalf("err %v started %v", j.Err(), j.Started())
	}
	// The child sees a scrubbed environment.
	t.Setenv("MY_SECRET", "s")
	j, _ = Start("echo \"[$MY_SECRET][$HOME]\"", t.TempDir(), Env(nil), nil, nil)
	<-j.Done()
	if j.Output() != "[]["+os.Getenv("HOME")+"]\n" {
		t.Fatalf("child env: %q", j.Output())
	}
	// The tail cap keeps the end of a long output.
	j, _ = Start("yes 0123456789abcdef | head -c 599998", t.TempDir(), Env(nil), nil, nil)
	<-j.Done()
	out := j.Output()
	if len(out) > OutputCap+64 || len(out) < OutputCap/2 || !strings.HasPrefix(out, "… [earlier output dropped] …") || !strings.HasSuffix(out, "0123456789abcdef\n") {
		t.Fatalf("tail: %d bytes, head %q", len(out), out[:40])
	}
}
