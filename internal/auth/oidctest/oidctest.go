// Package oidctest signs test users in through a real OpenID Connect
// identity provider: the Dex that compose.yaml runs.
//
// Tests that use it need Dex running and its issuer URL in the
// BEACON_TEST_OIDC_ISSUER environment variable:
//
//	docker compose up -d --wait dex
//	export BEACON_TEST_OIDC_ISSUER='http://127.0.0.1:5556/dex'
//
// Without the variable the tests are skipped, except in CI, where a missing
// provider is a failure. The client and the users below are defined in
// dev/dex/config.yaml.
package oidctest

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// EnvIssuer names the environment variable that holds Dex's issuer URL.
const EnvIssuer = "BEACON_TEST_OIDC_ISSUER"

// The client that Dex knows Beacon as, and the address Beacon must give as
// its public URL: Dex only sends people back to redirect URIs it knows.
const (
	ClientID     = "beacon"
	ClientSecret = "beacon-local-development-client-secret"
	PublicURL    = "http://127.0.0.1:18080"
)

// Password is the password of every user.
const Password = "password"

// The users. Ada is in the groups beacon-admins and platform-engineers,
// Grace in platform-engineers, Linus in none. Mallory's email address is not
// verified.
const (
	Ada     = "ada@example.com"
	Grace   = "grace@example.com"
	Linus   = "linus@example.com"
	Mallory = "mallory@example.com"
)

// Issuer returns Dex's issuer URL, or skips the test when there is none.
func Issuer(t testing.TB) string {
	t.Helper()
	issuer := os.Getenv(EnvIssuer)
	if issuer == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set; sign-in tests must not be skipped in CI", EnvIssuer)
		}
		t.Skipf("%s is not set; skipping test that needs an identity provider", EnvIssuer)
	}
	return issuer
}

// Browser returns an HTTP client that behaves like a browser: it keeps
// cookies and follows redirects. Requests to PublicURL reach the Beacon that
// listens on addr, as if that were its public address.
func Browser(t testing.TB, addr string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	public, _ := url.Parse(PublicURL)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, to string) (net.Conn, error) {
			if to == public.Host {
				to = addr
			}
			return dialer.DialContext(ctx, network, to)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Jar: jar, Transport: transport, Timeout: 30 * time.Second}
}

// SignIn takes the browser through a whole sign-in as the user: it opens
// Beacon's login address, is sent to Dex, fills in Dex's form, and follows
// the redirects back. A completed sign-in ends on the web interface, which
// says nothing about the person, so SignIn then asks who is signed in and
// returns that answer. A sign-in that was refused returns the refusal.
func SignIn(t testing.TB, browser *http.Client, email, password string) (status int, body string) {
	t.Helper()
	resp, err := browser.Get(PublicURL + "/auth/login")
	if err != nil {
		t.Fatalf("open the login address: %v", err)
	}
	page := read(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `name="password"`) {
		// Beacon answered by itself, without sending the browser to Dex.
		return resp.StatusCode, page
	}
	// Dex's form posts back to the address it was served from.
	resp, err = browser.PostForm(resp.Request.URL.String(), url.Values{"login": {email}, "password": {password}})
	if err != nil {
		t.Fatalf("submit Dex's sign-in form: %v", err)
	}
	page = read(t, resp)
	if landed := resp.Request.URL; landed.Host != mustParse(PublicURL).Host || landed.Path != "/" {
		// Dex's own page again, or Beacon's refusal at the callback.
		return resp.StatusCode, page
	}
	resp, err = browser.Get(PublicURL + "/api/v1/me")
	if err != nil {
		t.Fatalf("ask who is signed in: %v", err)
	}
	return resp.StatusCode, read(t, resp)
}

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func read(t testing.TB, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(body)
}
