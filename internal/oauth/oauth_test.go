package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	p, err := f.Start(context.Background())
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
	p, err := g.Start(context.Background())
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
