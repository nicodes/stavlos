// Package httpx makes the HTTP clients the harness calls services with:
// model providers, their sign-in and usage endpoints, the model catalogue,
// Discord. Each of them carries a credential or trusts the answer, so they
// share one rule the standard client does not have: a redirect stays on the
// host that was asked. (web_fetch, which follows redirects to anywhere
// public on purpose, keeps its own client and its own address guard.)
package httpx

import (
	"fmt"
	"net/http"
	"time"
)

// maxRedirects bounds a chain on one host.
const maxRedirects = 5

// Options are the two ways the callers differ.
type Options struct {
	// Timeout bounds the whole exchange; zero for a stream, which its
	// context bounds instead.
	Timeout time.Duration
	// HeaderTimeout bounds the wait for the response's first header.
	HeaderTimeout time.Duration
}

// New is a client with proxies from the environment and SameHost redirects.
func New(o Options) *http.Client {
	return &http.Client{
		Timeout:       o.Timeout,
		CheckRedirect: SameHost,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: o.HeaderTimeout,
			TLSHandshakeTimeout:   30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
			ForceAttemptHTTP2:     true,
		},
	}
}

// SameHost refuses a redirect that leaves the scheme and host first asked,
// so a credential in a header or a body never reaches a host nobody named,
// and an answer never comes from one.
func SameHost(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return fmt.Errorf("refused a redirect from %s to %s", first.Host, req.URL.Host)
	}
	return nil
}
