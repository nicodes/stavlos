package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "h." + base64.RawURLEncoding.EncodeToString(b) + ".s"
}

func init() { pollMargin = 0 }

func TestClaims(t *testing.T) {
	tok := jwt(map[string]any{"email": "a@b.c", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_1"}, "exp": time.Now().Add(time.Minute).Unix()})
	c := Claims(tok)
	if c.Email != "a@b.c" || c.AccountID() != "acct_1" {
		t.Fatalf("%+v", c)
	}
	if Expiring(tok, 10*time.Second) || !Expiring(tok, 2*time.Minute) {
		t.Fatal("expiring")
	}
	if Claims("opaque").AccountID() != "" {
		t.Fatal("opaque")
	}
}

func TestChatGPTDeviceFlow(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != chatGPTClientID {
			t.Errorf("client id %q", body["client_id"])
		}
		w.Write([]byte(`{"device_auth_id":"dev1","user_code":"abcd-efgh","interval":"0"}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) < 2 {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"authorization_code":"code1","code_verifier":"ver1"}`))
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "code1" || r.Form.Get("code_verifier") != "ver1" || !strings.HasSuffix(r.Form.Get("redirect_uri"), "/deviceauth/callback") {
				t.Errorf("bad exchange form %v", r.Form)
			}
			id := jwt(map[string]any{"email": "me@x.y", "chatgpt_account_id": "acct_9"})
			w.Write([]byte(`{"access_token":"acc1","refresh_token":"ref1","id_token":"` + id + `","expires_in":3600}`))
		case "refresh_token":
			if r.Form.Get("refresh_token") != "ref1" {
				t.Errorf("bad refresh %v", r.Form)
			}
			w.Write([]byte(`{"access_token":"acc2","expires_in":100}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := &ChatGPT{Issuer: srv.URL}
	p, err := f.Start(context.Background(), MethodDevice)
	if err != nil || p.Code != "ABCD-EFGH" || !strings.HasSuffix(p.URL, "/codex/device") {
		t.Fatalf("%+v %v", p, err)
	}
	p.interval = 0
	tok, err := f.Wait(context.Background(), p)
	if err != nil || tok.Access != "acc1" || tok.Refresh != "ref1" || tok.AccountID != "acct_9" || tok.Email != "me@x.y" {
		t.Fatalf("%+v %v", tok, err)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls %d", polls.Load())
	}
	tok2, err := f.Refresh(context.Background(), tok.Refresh)
	if err != nil || tok2.Access != "acc2" || tok2.Refresh != "ref1" {
		t.Fatalf("%+v %v", tok2, err)
	}
}

func TestGrokDeviceFlow(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/device/code", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_id") != grokClientID || !strings.Contains(r.Form.Get("scope"), "grok-cli:access") {
			t.Errorf("form %v", r.Form)
		}
		w.Write([]byte(`{"device_code":"dc","user_code":"wxyz","verification_uri":"https://x/dev","verification_uri_complete":"https://x/dev?c=wxyz","expires_in":60,"interval":0}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" {
			w.Write([]byte(`{"access_token":"a2","refresh_token":"r2","expires_in":10}`))
			return
		}
		switch polls.Add(1) {
		case 1:
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"authorization_pending"}`))
		case 2:
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"slow_down"}`))
		default:
			if r.Form.Get("device_code") != "dc" {
				t.Errorf("device code %v", r.Form)
			}
			w.Write([]byte(`{"access_token":"a1","refresh_token":"r1","expires_in":3600}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := &Grok{DeviceURL: srv.URL + "/device/code", TokenURL: srv.URL + "/token"}
	p, err := g.Start(context.Background(), "")
	if err != nil || p.Code != "WXYZ" || p.URL != "https://x/dev?c=wxyz" {
		t.Fatalf("%+v %v", p, err)
	}
	// shrink the waits the test would otherwise take
	p.interval = 0
	tok, err := g.Wait(context.Background(), p)
	if err != nil || tok.Access != "a1" || tok.Refresh != "r1" {
		t.Fatalf("%+v %v", tok, err)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls %d", polls.Load())
	}
	tok2, err := g.Refresh(context.Background(), "r1")
	if err != nil || tok2.Access != "a2" || tok2.Refresh != "r2" {
		t.Fatalf("%+v %v", tok2, err)
	}
	// cancellation
	polls.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.interval = 5 * time.Second
	if _, err := g.Wait(ctx, p); err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestChatGPTBrowserFlow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "c0de" || r.Form.Get("code_verifier") == "" || !strings.Contains(r.Form.Get("redirect_uri"), "/auth/callback") {
			t.Errorf("bad exchange %v", r.Form)
		}
		id := jwt(map[string]any{"email": "b@x.y", "chatgpt_account_id": "acct_b"})
		w.Write([]byte(`{"access_token":"accB","refresh_token":"refB","id_token":"` + id + `","expires_in":10}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := &ChatGPT{Issuer: srv.URL, Port: 18455}
	if ms := f.Methods(); len(ms) != 2 || ms[0].ID != MethodBrowser {
		t.Fatalf("%+v", ms)
	}
	p, err := f.Start(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Method != MethodBrowser || p.Code != "" || !strings.Contains(p.URL, "/oauth/authorize?") || !strings.Contains(p.URL, "code_challenge_method=S256") || !strings.Contains(p.URL, "originator=stavlos") {
		t.Fatalf("%+v", p)
	}
	// port is held while pending
	if _, err := f.Start(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "18455") {
		t.Fatalf("second start: %v", err)
	}
	u, _ := url.Parse(p.URL)
	state := u.Query().Get("state")
	done := make(chan Tokens, 1)
	errc := make(chan error, 1)
	go func() {
		tok, err := f.Wait(context.Background(), p)
		if err != nil {
			errc <- err
			return
		}
		done <- tok
	}()
	time.Sleep(50 * time.Millisecond)
	// wrong state is rejected, then the real callback lands
	resp, err := http.Get("http://127.0.0.1:18455/auth/callback?code=x&state=bad")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("bad state status %d", resp.StatusCode)
	}
	select {
	case err := <-errc:
		if !strings.Contains(err.Error(), "state mismatch") {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no error after bad state")
	}
	// start over and succeed
	p, err = f.Start(context.Background(), MethodBrowser)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = url.Parse(p.URL)
	state = u.Query().Get("state")
	go func() {
		tok, err := f.Wait(context.Background(), p)
		if err != nil {
			errc <- err
			return
		}
		done <- tok
	}()
	time.Sleep(50 * time.Millisecond)
	resp, err = http.Get("http://127.0.0.1:18455/auth/callback?code=c0de&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Signed in") {
		t.Fatalf("callback %d %s", resp.StatusCode, body)
	}
	select {
	case tok := <-done:
		if tok.Access != "accB" || tok.AccountID != "acct_b" || tok.Email != "b@x.y" {
			t.Fatalf("%+v", tok)
		}
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
	// server released the port
	time.Sleep(50 * time.Millisecond)
	if _, err := net.Dial("tcp", "127.0.0.1:18455"); err == nil {
		t.Fatal("port still open")
	}
}
