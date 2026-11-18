package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/auth/oidctest"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// logBuffer collects log output from the handler under test.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// beacon is the sign-in handler on a test server, with its database.
type beacon struct {
	t       *testing.T
	pool    *pgxpool.Pool
	store   *auth.Store
	handler *auth.Handler
	server  *httptest.Server
	logs    *logBuffer
}

func newBeacon(t *testing.T, a *config.Auth) *beacon {
	t.Helper()
	b := &beacon{t: t, pool: postgrestest.NewMigrated(t), logs: &logBuffer{}}
	b.store = auth.NewStore(b.pool)
	b.handler = auth.NewHandler(slog.New(slog.NewJSONHandler(b.logs, nil)), b.store)
	b.handler.Configure(context.Background(), a, nil)
	// The routes are guarded as the server guards them.
	mux := http.NewServeMux()
	guard := access.NewGuard(slog.New(slog.NewJSONHandler(b.logs, nil)), b.handler)
	for _, route := range b.handler.Routes() {
		mux.Handle(route.Pattern, guard.Wrap(route))
	}
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

// addr is the address the test server listens on.
func (b *beacon) addr() string { return strings.TrimPrefix(b.server.URL, "http://") }

func (b *beacon) count(query string, args ...any) int {
	b.t.Helper()
	var n int
	if err := b.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		b.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// dexAuth is the auth block for the Dex of compose.yaml.
func dexAuth(t *testing.T) *config.Auth {
	return &config.Auth{PublicURL: oidctest.PublicURL, OIDC: &config.OIDC{
		Issuer:       oidctest.Issuer(t),
		ClientID:     oidctest.ClientID,
		ClientSecret: config.NewSecret(config.SecretRef{Env: "BEACON_OIDC_CLIENT_SECRET"}, oidctest.ClientSecret),
	}}
}

// get fetches a path of Beacon with the browser and returns the status and
// the decoded JSON body.
func get(t *testing.T, browser *http.Client, method, path string, header ...string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, oidctest.PublicURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body
}

// TestSignInWithDex signs in through a real identity provider, with the
// authorisation code flow: Beacon sends the browser to Dex, the user enters
// a password there, and Dex sends the browser back with a code.
func TestSignInWithDex(t *testing.T) {
	b := newBeacon(t, dexAuth(t))
	browser := oidctest.Browser(t, b.addr())

	// Nobody is signed in yet.
	if code, _ := get(t, browser, "GET", auth.MePath); code != http.StatusUnauthorized {
		t.Fatalf("GET %s before sign-in = %d, want 401", auth.MePath, code)
	}

	// First sign-in: the user is created.
	code, body := oidctest.SignIn(t, browser, oidctest.Ada, oidctest.Password)
	if code != http.StatusOK || !strings.Contains(body, `"email":"ada@example.com"`) || !strings.Contains(body, `"display_name":"Ada Example"`) || !strings.Contains(body, `"role":"member"`) {
		t.Fatalf("after sign-in = %d %s\nlogs:\n%s", code, body, b.logs)
	}
	var issuer, subject string
	var firstSignIn time.Time
	err := b.pool.QueryRow(context.Background(), `SELECT oidc_issuer, oidc_subject, last_sign_in_at FROM users WHERE email = $1`, oidctest.Ada).Scan(&issuer, &subject, &firstSignIn)
	if err != nil || issuer != oidctest.Issuer(t) || subject == "" {
		t.Errorf("stored user: issuer %q, subject %q, %v", issuer, subject, err)
	}

	// The session works, and says when it ends.
	code, me := get(t, browser, "GET", auth.MePath)
	if code != http.StatusOK || me["email"] != oidctest.Ada || me["session_expires_at"] == nil {
		t.Errorf("GET %s = %d %v", auth.MePath, code, me)
	}
	// Only a hash of the session token is stored.
	for _, cookie := range browser.Jar.Cookies(mustURL(t, oidctest.PublicURL)) {
		if cookie.Name == "beacon_session" && b.count(`SELECT count(*) FROM sessions WHERE token_hash = $1`, cookie.Value) != 0 {
			t.Error("the session token is stored as it is")
		}
	}
	if b.count(`SELECT count(*) FROM sessions`) != 1 || b.count(`SELECT count(*) FROM sign_in_flows`) != 0 {
		t.Error("want one session and no sign-in left in progress")
	}

	// Later sign-in, from another browser: the same user, updated.
	second := oidctest.Browser(t, b.addr())
	if code, body := oidctest.SignIn(t, second, oidctest.Ada, oidctest.Password); code != http.StatusOK {
		t.Fatalf("second sign-in = %d %s", code, body)
	}
	if n := b.count(`SELECT count(*) FROM users`); n != 1 {
		t.Errorf("%d users after two sign-ins of one person, want 1", n)
	}
	if n := b.count(`SELECT count(*) FROM users WHERE last_sign_in_at > $1`, firstSignIn); n != 1 {
		t.Error("the later sign-in did not update the user")
	}

	// A request that another site made the browser send is refused, and
	// the session survives it.
	if code, _ := get(t, browser, "POST", auth.LogoutPath, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site sign-out = %d, want 403", code)
	}
	if code, _ := get(t, browser, "POST", auth.LogoutPath, "Origin", "https://evil.example.com"); code != http.StatusForbidden {
		t.Errorf("sign-out with a foreign Origin = %d, want 403", code)
	}
	if code, _ := get(t, browser, "GET", auth.MePath); code != http.StatusOK {
		t.Errorf("the session did not survive the cross-site requests: %d", code)
	}

	// Sign-out ends this session and not the other one.
	if code, _ := get(t, browser, "POST", auth.LogoutPath, "Sec-Fetch-Site", "same-origin"); code != http.StatusNoContent {
		t.Errorf("sign-out = %d, want 204", code)
	}
	if code, _ := get(t, browser, "GET", auth.MePath); code != http.StatusUnauthorized {
		t.Errorf("GET %s after sign-out = %d, want 401", auth.MePath, code)
	}
	if code, _ := get(t, second, "GET", auth.MePath); code != http.StatusOK {
		t.Errorf("the other browser's session ended too: %d", code)
	}

	// The session of the other browser ends when its time is up.
	b.store.SetClock(func() time.Time { return time.Now().Add(config.Auth{}.Lifetime() + time.Minute) })
	if code, _ := get(t, second, "GET", auth.MePath); code != http.StatusUnauthorized {
		t.Errorf("GET %s with an expired session = %d, want 401", auth.MePath, code)
	}
	if n, err := b.store.DeleteExpired(context.Background()); err != nil || n != 1 {
		t.Errorf("DeleteExpired = %d, %v; want the one expired session", n, err)
	}

	logs := b.logs.String()
	if strings.Contains(logs, oidctest.ClientSecret) {
		t.Errorf("the logs contain the client secret:\n%s", logs)
	}
	if !strings.Contains(logs, `"msg":"user signed in"`) || !strings.Contains(logs, `"first_sign_in":true`) || !strings.Contains(logs, `"msg":"user signed out"`) {
		t.Errorf("the logs do not show the sign-in and sign-out:\n%s", logs)
	}
}

func TestSignInWithDexRefusals(t *testing.T) {
	b := newBeacon(t, dexAuth(t))

	// A wrong password never leaves Dex.
	browser := oidctest.Browser(t, b.addr())
	if _, body := oidctest.SignIn(t, browser, oidctest.Ada, "not-the-password"); !strings.Contains(body, "Invalid") {
		t.Errorf("a wrong password did not stay on Dex's form: %s", body)
	}
	if n := b.count(`SELECT count(*) FROM users`); n != 0 {
		t.Errorf("%d users after a failed sign-in", n)
	}

	// An email address that the provider has not verified is refused.
	browser = oidctest.Browser(t, b.addr())
	code, body := oidctest.SignIn(t, browser, oidctest.Mallory, oidctest.Password)
	if code != http.StatusForbidden || !strings.Contains(body, "has not verified this email address") {
		t.Errorf("sign-in with an unverified address = %d %s", code, body)
	}
	if b.count(`SELECT count(*) FROM users`) != 0 || b.count(`SELECT count(*) FROM sessions`) != 0 {
		t.Error("a refused sign-in left a user or a session behind")
	}

	// A disabled user is refused, and loses the sessions they had.
	browser = oidctest.Browser(t, b.addr())
	if code, body := oidctest.SignIn(t, browser, oidctest.Grace, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	if _, err := b.pool.Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE email = $1`, oidctest.Grace); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, browser, "GET", auth.MePath); code != http.StatusUnauthorized {
		t.Errorf("GET %s as a disabled user = %d, want 401", auth.MePath, code)
	}
	if code, body := oidctest.SignIn(t, oidctest.Browser(t, b.addr()), oidctest.Grace, oidctest.Password); code != http.StatusForbidden || !strings.Contains(body, "disabled") {
		t.Errorf("sign-in as a disabled user = %d %s", code, body)
	}
}
