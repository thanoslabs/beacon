package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/config"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

const (
	fakeClientID     = "beacon"
	fakeClientSecret = "fake-client-secret-marker-c41d"
)

// fakeProvider is a hand-written identity provider. It exists for what a
// real provider will not do: issue ID tokens that are wrong in one chosen
// way. The tests against Dex cover the flow itself.
type fakeProvider struct {
	t      *testing.T
	server *httptest.Server
	key    *rsa.PrivateKey
	// tokens maps an authorisation code to the ID token it buys and the
	// PKCE challenge that was sent when the code was asked for.
	tokens map[string]fakeGrant
}

type fakeGrant struct {
	idToken   string
	challenge string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{t: t, key: key, tokens: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token",
			"jwks_uri": f.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		if id == "" {
			id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
		}
		grant, ok := f.tokens[r.PostFormValue("code")]
		// As a real provider does: the client must authenticate, and prove
		// with the PKCE verifier that it is the one that asked for the code.
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		w.Header().Set("Content-Type", "application/json")
		if !ok || id != fakeClientID || secret != fakeClientSecret || base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		delete(f.tokens, r.PostFormValue("code"))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 60, "id_token": grant.idToken})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// sign returns a JWT with these claims, signed with key.
func sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signed := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// attempt is one sign-in against the fake provider, taken step by step so
// that a test can change what the provider or the browser does.
type attempt struct {
	t *testing.T
	b *beacon
	f *fakeProvider
	// cookie is the sign-in cookie that the login address set.
	cookie *http.Cookie
	// The parameters that Beacon sent to the provider.
	state, nonce, challenge, method string
}

// noRedirect is a client that returns redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// begin opens the login address and reads where Beacon sends the browser.
func begin(t *testing.T, b *beacon, f *fakeProvider) *attempt {
	t.Helper()
	resp, err := noRedirect.Get(b.server.URL + auth.LoginPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	if resp.StatusCode != http.StatusFound || len(resp.Cookies()) != 1 {
		t.Fatalf("GET %s = %d with %d cookies, want a redirect and the sign-in cookie", auth.LoginPath, resp.StatusCode, len(resp.Cookies()))
	}
	to := mustURL(t, resp.Header.Get("Location"))
	q := to.Query()
	if !strings.HasPrefix(to.String(), f.server.URL+"/authorize?") || q.Get("response_type") != "code" || q.Get("client_id") != fakeClientID {
		t.Fatalf("redirect to %s, want the provider's authorisation endpoint with the code flow", to)
	}
	return &attempt{t: t, b: b, f: f, cookie: resp.Cookies()[0],
		state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method")}
}

// claims returns the claims of a correct ID token for this sign-in.
func (a *attempt) claims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": a.f.server.URL, "aud": fakeClientID, "sub": "subject-1", "nonce": a.nonce,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"email": "Ada@Example.com", "email_verified": true, "name": "Ada Example",
	}
}

// grant makes the provider ready to hand out idToken for a code.
func (a *attempt) grant(code, idToken string) {
	a.f.tokens[code] = fakeGrant{idToken: idToken, challenge: a.challenge}
}

// callback sends the browser back to Beacon, as the provider's redirect
// does, and returns the status, the error in the body and the cookies set.
func (a *attempt) callback(query url.Values, cookies ...*http.Cookie) (int, string, []*http.Cookie) {
	a.t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.b.server.URL+auth.CallbackPath+"?"+query.Encode(), nil)
	if err != nil {
		a.t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Error, resp.Cookies()
}

func fakeAuth(f *fakeProvider, publicURL string) *config.Auth {
	return &config.Auth{PublicURL: publicURL, OIDC: &config.OIDC{
		Issuer: f.server.URL, ClientID: fakeClientID,
		ClientSecret: config.NewSecret(config.SecretRef{Env: "BEACON_OIDC_CLIENT_SECRET"}, fakeClientSecret),
	}}
}

// TestSignInParameters checks what Beacon sends to the provider, and the
// cookies it sets when it is reached over https.
func TestSignInParameters(t *testing.T) {
	f := newFakeProvider(t)
	b := newBeacon(t, fakeAuth(f, "https://beacon.example.com"))
	a := begin(t, b, f)

	// State, nonce and a PKCE challenge, all fresh for each sign-in.
	if len(a.state) < 40 || len(a.nonce) < 40 || len(a.challenge) < 40 || a.method != "S256" {
		t.Errorf("state %q, nonce %q, challenge %q, method %q", a.state, a.nonce, a.challenge, a.method)
	}
	if again := begin(t, b, f); again.state == a.state || again.nonce == a.nonce || again.challenge == a.challenge {
		t.Error("two sign-ins share a state, a nonce or a challenge")
	}
	if c := a.cookie; c.Name != "__Secure-beacon_sign_in" || c.Value != a.state || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/auth/" {
		t.Errorf("sign-in cookie = %+v", c)
	}
	// What is stored does not hold the state itself.
	if n := b.count(`SELECT count(*) FROM sign_in_flows WHERE state_hash = $1`, a.state); n != 0 {
		t.Error("the state is stored as it is")
	}

	a.grant("code-1", sign(t, f.key, a.claims()))
	status, msg, cookies := a.callback(url.Values{"code": {"code-1"}, "state": {a.state}}, a.cookie)
	if status != http.StatusSeeOther {
		t.Fatalf("callback = %d %q\nlogs:\n%s", status, msg, b.logs)
	}
	var session *http.Cookie
	for _, c := range cookies {
		if c.Name == "__Host-beacon_session" {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.Domain != "" || session.MaxAge != 12*60*60 {
		t.Fatalf("session cookie = %+v", session)
	}
	if n := b.count(`SELECT count(*) FROM sessions WHERE token_hash = $1`, auth.HashToken(session.Value)); n != 1 {
		t.Error("the session is not stored under the hash of its token")
	}
	// The email address is stored lowercase, as configuration compares it.
	if n := b.count(`SELECT count(*) FROM users WHERE email = 'ada@example.com' AND oidc_subject = 'subject-1' AND oidc_issuer = $1`, f.server.URL); n != 1 {
		t.Error("the user was not stored")
	}
	if strings.Contains(b.logs.String(), fakeClientSecret) || strings.Contains(b.logs.String(), session.Value) || strings.Contains(b.logs.String(), a.state) {
		t.Errorf("the logs contain a secret:\n%s", b.logs)
	}
}

// TestCallbackIsRefused goes through every way a callback can be wrong. None
// may create a user or a session.
func TestCallbackIsRefused(t *testing.T) {
	f := newFakeProvider(t)
	b := newBeacon(t, fakeAuth(f, "http://127.0.0.1:18080"))
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	// Each case changes one thing about an otherwise correct sign-in.
	tests := []struct {
		name string
		// token changes the claims of the ID token, or the key it is
		// signed with.
		token func(c map[string]any) string
		// send changes what the browser sends back.
		send       func(a *attempt, q url.Values) []*http.Cookie
		wantStatus int
		wantMsg    string
		wantLog    string
	}{
		{name: "another issuer", wantStatus: 401, wantLog: "id token issued by a different provider",
			token: func(c map[string]any) string {
				c["iss"] = "https://evil.example.com"
				return sign(t, f.key, c)
			}},
		{name: "another audience", wantStatus: 401, wantLog: "expected audience",
			token: func(c map[string]any) string { c["aud"] = "another-client"; return sign(t, f.key, c) }},
		{name: "expired", wantStatus: 401, wantLog: "token is expired",
			token: func(c map[string]any) string {
				c["exp"] = time.Now().Add(-time.Minute).Unix()
				return sign(t, f.key, c)
			}},
		{name: "signed with another key", wantStatus: 401, wantLog: "failed to verify signature",
			token: func(c map[string]any) string { return sign(t, otherKey, c) }},
		{name: "not signed", wantStatus: 401, wantLog: "verify the ID token",
			token: func(c map[string]any) string {
				signed := sign(t, f.key, c)
				return signed[:strings.LastIndex(signed, ".")+1]
			}},
		{name: "claims changed after signing", wantStatus: 401, wantLog: "failed to verify signature",
			token: func(c map[string]any) string {
				parts := strings.Split(sign(t, f.key, c), ".")
				c["email"] = "root@example.com"
				forged := strings.Split(sign(t, otherKey, c), ".")
				return parts[0] + "." + forged[1] + "." + parts[2]
			}},
		{name: "another nonce", wantStatus: 401, wantLog: "the nonce does not match",
			token: func(c map[string]any) string {
				c["nonce"] = "someone-elses-nonce"
				return sign(t, f.key, c)
			}},
		{name: "no nonce", wantStatus: 401, wantLog: "the nonce does not match",
			token: func(c map[string]any) string { delete(c, "nonce"); return sign(t, f.key, c) }},
		{name: "no email", wantStatus: 403, wantMsg: "did not share an email address",
			token: func(c map[string]any) string { delete(c, "email"); return sign(t, f.key, c) }},
		{name: "email not verified", wantStatus: 403, wantMsg: "has not verified this email address",
			token: func(c map[string]any) string { c["email_verified"] = false; return sign(t, f.key, c) }},
		{name: "email not verified, as a string", wantStatus: 403, wantMsg: "has not verified this email address",
			token: func(c map[string]any) string { c["email_verified"] = "false"; return sign(t, f.key, c) }},

		{name: "no sign-in cookie", wantStatus: 400, wantMsg: "was not started in this browser",
			send: func(*attempt, url.Values) []*http.Cookie { return nil }},
		{name: "the cookie of another sign-in", wantStatus: 400, wantMsg: "was not started in this browser",
			send: func(*attempt, url.Values) []*http.Cookie { return []*http.Cookie{begin(t, b, f).cookie} }},
		{name: "another state", wantStatus: 400, wantMsg: "was not started in this browser",
			send: func(a *attempt, q url.Values) []*http.Cookie {
				q.Set("state", "guessed")
				return []*http.Cookie{a.cookie}
			}},
		{name: "no state", wantStatus: 400, wantMsg: "was not started in this browser",
			send: func(a *attempt, q url.Values) []*http.Cookie { q.Del("state"); return []*http.Cookie{a.cookie} }},
		{name: "a state that was never issued", wantStatus: 400, wantMsg: "has expired or was already completed",
			send: func(a *attempt, q url.Values) []*http.Cookie {
				q.Set("state", "made-up")
				return []*http.Cookie{{Name: a.cookie.Name, Value: "made-up"}}
			}},
		{name: "another code", wantStatus: 401, wantLog: `error \"invalid_grant\"`,
			send: func(a *attempt, q url.Values) []*http.Cookie {
				q.Set("code", "guessed")
				return []*http.Cookie{a.cookie}
			}},
		{name: "the provider reports an error", wantStatus: 401, wantMsg: "refused the sign-in: access_denied",
			send: func(a *attempt, q url.Values) []*http.Cookie {
				q.Del("code")
				q.Set("error", "access_denied")
				return []*http.Cookie{a.cookie}
			}},
		{name: "the provider reports something that is not an error code", wantStatus: 401, wantMsg: "refused the sign-in: invalid",
			send: func(a *attempt, q url.Values) []*http.Cookie {
				q.Set("error", "<script>alert(1)</script>")
				return []*http.Cookie{a.cookie}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := begin(t, b, f)
			token := sign(t, f.key, a.claims())
			if tt.token != nil {
				token = tt.token(a.claims())
			}
			a.grant("code-"+tt.name, token)
			query := url.Values{"code": {"code-" + tt.name}, "state": {a.state}}
			cookies := []*http.Cookie{a.cookie}
			if tt.send != nil {
				cookies = tt.send(a, query)
			}
			before := len(b.logs.String())
			status, msg, set := a.callback(query, cookies...)
			if status != tt.wantStatus || !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("callback = %d %q, want %d %q", status, msg, tt.wantStatus, tt.wantMsg)
			}
			if logs := b.logs.String()[before:]; !strings.Contains(logs, tt.wantLog) {
				t.Errorf("the logs do not contain %q:\n%s", tt.wantLog, logs)
			}
			for _, c := range set {
				if c.Name == "beacon_session" && c.Value != "" {
					t.Error("a refused callback set a session cookie")
				}
			}
			if b.count(`SELECT count(*) FROM users`) != 0 || b.count(`SELECT count(*) FROM sessions`) != 0 {
				t.Error("a refused callback created a user or a session")
			}
		})
	}
}

// TestCallbackWorksOnce replays a completed callback, and uses one that took
// too long.
func TestCallbackWorksOnce(t *testing.T) {
	f := newFakeProvider(t)
	b := newBeacon(t, fakeAuth(f, "http://127.0.0.1:18080"))

	a := begin(t, b, f)
	a.grant("code-1", sign(t, f.key, a.claims()))
	query := url.Values{"code": {"code-1"}, "state": {a.state}}
	if status, msg, _ := a.callback(query, a.cookie); status != http.StatusSeeOther {
		t.Fatalf("callback = %d %q", status, msg)
	}
	a.grant("code-1", sign(t, f.key, a.claims()))
	if status, msg, _ := a.callback(query, a.cookie); status != http.StatusBadRequest || !strings.Contains(msg, "already completed") {
		t.Errorf("replayed callback = %d %q, want 400", status, msg)
	}

	late := begin(t, b, f)
	late.grant("code-2", sign(t, f.key, late.claims()))
	b.store.SetClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	if status, msg, _ := late.callback(url.Values{"code": {"code-2"}, "state": {late.state}}, late.cookie); status != http.StatusBadRequest || !strings.Contains(msg, "has expired") {
		t.Errorf("late callback = %d %q, want 400", status, msg)
	}
	if n := b.count(`SELECT count(*) FROM sessions`); n != 1 {
		t.Errorf("%d sessions, want only the one of the first callback", n)
	}
}

// TestEmailAddressChanges signs the same person in after the provider
// changed their email address, and then someone else with an address that
// is taken.
func TestEmailAddressChanges(t *testing.T) {
	f := newFakeProvider(t)
	b := newBeacon(t, fakeAuth(f, "http://127.0.0.1:18080"))
	signIn := func(subject, email string) (int, string) {
		a := begin(t, b, f)
		c := a.claims()
		c["sub"], c["email"] = subject, email
		a.grant("code", sign(t, f.key, c))
		status, msg, _ := a.callback(url.Values{"code": {"code"}, "state": {a.state}}, a.cookie)
		return status, msg
	}
	if status, msg := signIn("subject-1", "ada@example.com"); status != http.StatusSeeOther {
		t.Fatalf("first sign-in = %d %q", status, msg)
	}
	if status, msg := signIn("subject-1", "ada.lovelace@example.com"); status != http.StatusSeeOther {
		t.Fatalf("sign-in with a new address = %d %q", status, msg)
	}
	if b.count(`SELECT count(*) FROM users`) != 1 || b.count(`SELECT count(*) FROM users WHERE email = 'ada.lovelace@example.com'`) != 1 {
		t.Error("a changed email address did not update the same user")
	}
	// Another person whose address is the one Ada has now.
	status, msg := signIn("subject-2", "ada.lovelace@example.com")
	if status != http.StatusForbidden || !strings.Contains(msg, "another account already uses this email address") {
		t.Errorf("sign-in with a taken address = %d %q, want 403", status, msg)
	}
	if n := b.count(`SELECT count(*) FROM users`); n != 1 {
		t.Errorf("%d users, want 1", n)
	}
}

func TestSignInIsOffWithoutConfiguration(t *testing.T) {
	b := newBeacon(t, nil)
	for _, path := range []string{auth.LoginPath, auth.CallbackPath} {
		resp, err := noRedirect.Get(b.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s without configuration = %d, want 404", path, resp.StatusCode)
		}
	}
	// A provider that cannot be reached does not stop Beacon; signing in
	// says so and tries again next time.
	b.handler.Configure(context.Background(), &config.Auth{PublicURL: "http://127.0.0.1:18080", OIDC: &config.OIDC{
		Issuer: "http://127.0.0.1:1/nothing", ClientID: "beacon", ClientSecret: config.NewSecret(config.SecretRef{}, fakeClientSecret)}})
	resp, err := noRedirect.Get(b.server.URL + auth.LoginPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET %s with an unreachable provider = %d, want 503", auth.LoginPath, resp.StatusCode)
	}
}
