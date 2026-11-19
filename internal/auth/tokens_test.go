package auth_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/config"
)

// The bootstrap admin's password in these tests, and its hash.
const bootstrapPassword = "bootstrap-password-marker-e5a1"

func bootstrapHash(t *testing.T) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(bootstrapPassword), config.MinBcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(hash)
}

func bootstrapAuth(t *testing.T, publicURL string) *config.Auth {
	return &config.Auth{PublicURL: publicURL, BootstrapAdmin: &config.BootstrapAdmin{
		PasswordHash: config.NewSecret(config.SecretRef{Env: "BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH"}, bootstrapHash(t)),
	}}
}

// call sends a request to the test server and returns the status, the
// decoded JSON body and the headers of the response.
func (b *beacon) call(method, path, body string, header ...string) (int, map[string]any, http.Header) {
	b.t.Helper()
	req, err := http.NewRequest(method, b.server.URL+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, resp.Header
}

// cookies returns the cookies that a response set.
func cookies(h http.Header) []*http.Cookie {
	return (&http.Response{Header: h}).Cookies()
}

// as returns the header that presents token.
func as(token string) []string { return []string{"Authorization", "Bearer " + token} }

// newToken stores a token and returns its identifier and its secret.
func (b *beacon) newToken(name string, role access.Role, lifetime time.Duration) (id, secret string) {
	b.t.Helper()
	tok, secret, err := b.store.CreateToken(context.Background(), name, role, lifetime, "")
	if err != nil {
		b.t.Fatalf("CreateToken: %v", err)
	}
	return tok.ID, secret
}

// TestAPITokens covers the life of a token and what each role may do with
// the token routes, which are for admins only.
func TestAPITokens(t *testing.T) {
	// No auth block at all: tokens do not depend on sign-in.
	b := newBeacon(t, nil)
	_, admin := b.newToken("first-admin", access.RoleAdmin, 0)

	// Without a credential, or with something that is not a token.
	for _, header := range [][]string{nil, as(""), as("bcn_made-up"), as("not-a-token"), {"Authorization", "Basic YWRtaW46YWRtaW4="}, {"Authorization", admin}} {
		if code, _, h := b.call("GET", auth.TokensPath, "", header...); code != http.StatusUnauthorized || h.Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s with %v = %d, want 401", auth.TokensPath, header, code)
		}
	}

	// An admin token creates a member token. The secret is in the answer,
	// once, and only its hash is stored.
	code, created, _ := b.call("POST", auth.TokensPath, `{"name": "ci", "role": "member", "expires_in": "30d"}`, as(admin)...)
	member, _ := created["token"].(string)
	id, _ := created["id"].(string)
	if code != http.StatusCreated || !strings.HasPrefix(member, "bcn_") || len(member) < 40 || created["role"] != "member" || created["expires_at"] == nil {
		t.Fatalf("create = %d %v", code, created)
	}
	if n := b.count(`SELECT count(*) FROM api_tokens WHERE token_hash = $1`, auth.HashToken(member)); n != 1 {
		t.Error("the token is not stored under its hash")
	}
	if n := b.count(`SELECT count(*) FROM api_tokens WHERE token_hash LIKE 'bcn_%' OR name LIKE 'bcn_%'`); n != 0 {
		t.Error("a token is stored as it is")
	}

	// The member token authenticates, with its own role.
	code, who, _ := b.call("GET", auth.MePath, "", as(member)...)
	if code != http.StatusOK || who["role"] != "member" || who["display_name"] != "ci" || who["token_id"] != id || who["id"] != nil {
		t.Errorf("GET %s with the member token = %d %v", auth.MePath, code, who)
	}
	if n := b.count(`SELECT count(*) FROM api_tokens WHERE id = $1 AND last_used_at IS NOT NULL`, id); n != 1 {
		t.Error("the use of the token was not recorded")
	}

	// A member may not list, create or revoke tokens. If it could, it could
	// make itself an admin token.
	for _, attempt := range [][2]string{{"GET", auth.TokensPath}, {"POST", auth.TokensPath}, {"DELETE", auth.TokensPath + "/" + id}} {
		code, body, _ := b.call(attempt[0], attempt[1], `{"name": "escalation", "role": "admin"}`, as(member)...)
		if code != http.StatusForbidden || body["error"] != "this needs the admin role" {
			t.Errorf("%s %s with a member token = %d %v, want 403", attempt[0], attempt[1], code, body)
		}
	}
	if n := b.count(`SELECT count(*) FROM api_tokens`); n != 2 {
		t.Errorf("%d tokens, want 2: the member must not have created one", n)
	}

	// Requests that do not make sense.
	for body, want := range map[string]string{
		`{"role": "member"}`:                                   "name is required",
		`{"name": "x", "role": "owner"}`:                       "role is required",
		`{"name": "x"}`:                                        "role is required",
		`{"name": "x", "role": "admin", "expires_in": "soon"}`: "expires_in must be a duration",
		`{"name": "x", "role": "admin", "expires_in": "0s"}`:   "expires_in must be a duration",
		`{"name": "x", "role": "admin", "scope": "all"}`:       "the body must be JSON",
		`not json`: "the body must be JSON",
	} {
		if code, got, _ := b.call("POST", auth.TokensPath, body, as(admin)...); code != http.StatusBadRequest || !strings.Contains(got["error"].(string), want) {
			t.Errorf("create with %s = %d %v, want 400 %q", body, code, got, want)
		}
	}

	// The list shows every token and no secret or hash.
	code, list, h := b.call("GET", auth.TokensPath, "", as(admin)...)
	tokens, _ := list["tokens"].([]any)
	if code != http.StatusOK || len(tokens) != 2 {
		t.Fatalf("list = %d %v", code, list)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), member) || strings.Contains(string(raw), auth.HashToken(member)) || strings.Contains(string(raw), "hash") {
		t.Errorf("the list holds a secret or a hash: %s", raw)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Error("the list may be cached")
	}

	// Revoked: the token stops working at once, and stays in the list.
	if code, _, _ := b.call("DELETE", auth.TokensPath+"/"+id, "", as(admin)...); code != http.StatusNoContent {
		t.Errorf("revoke = %d, want 204", code)
	}
	if code, _, _ := b.call("GET", auth.MePath, "", as(member)...); code != http.StatusUnauthorized {
		t.Errorf("GET %s with a revoked token = %d, want 401", auth.MePath, code)
	}
	if code, _, _ := b.call("DELETE", auth.TokensPath+"/"+id, "", as(admin)...); code != http.StatusNoContent {
		t.Errorf("revoking again = %d, want 204", code)
	}
	for _, unknown := range []string{"00000000-0000-4000-8000-000000000000", "not-an-id"} {
		if code, _, _ := b.call("DELETE", auth.TokensPath+"/"+unknown, "", as(admin)...); code != http.StatusNotFound {
			t.Errorf("revoking %s = %d, want 404", unknown, code)
		}
	}
	if n := b.count(`SELECT count(*) FROM api_tokens WHERE id = $1 AND revoked_at IS NOT NULL`, id); n != 1 {
		t.Error("the revoked token is gone or not marked")
	}

	// Expired: a token with a lifetime stops working when it is over.
	_, shortLived := b.newToken("short", access.RoleMember, time.Hour)
	if code, _, _ := b.call("GET", auth.MePath, "", as(shortLived)...); code != http.StatusOK {
		t.Errorf("GET %s with a fresh token = %d", auth.MePath, code)
	}
	b.store.SetClock(func() time.Time { return time.Now().Add(61 * time.Minute) })
	if code, _, _ := b.call("GET", auth.MePath, "", as(shortLived)...); code != http.StatusUnauthorized {
		t.Errorf("GET %s with an expired token = %d, want 401", auth.MePath, code)
	}
	if code, _, _ := b.call("GET", auth.MePath, "", as(admin)...); code != http.StatusOK {
		t.Errorf("a token without an expiry stopped working: %d", code)
	}

	logs := b.logs.String()
	for _, secret := range []string{admin, member, shortLived} {
		if strings.Contains(logs, secret) || strings.Contains(logs, strings.TrimPrefix(secret, "bcn_")) {
			t.Errorf("the logs contain a token:\n%s", logs)
		}
	}
	if !strings.Contains(logs, `"msg":"api token created"`) || !strings.Contains(logs, `"msg":"api token revoked"`) {
		t.Errorf("the logs do not record the creation and the revocation:\n%s", logs)
	}
}

// TestBootstrapAdmin covers the account for installs with no identity
// provider: signing in, the bound on attempts, and turning it off.
func TestBootstrapAdmin(t *testing.T) {
	a := bootstrapAuth(t, "https://beacon.example.com")
	b := newBeacon(t, a)
	now := time.Now()
	b.store.SetClock(func() time.Time { return now })
	signIn := func(password string, header ...string) (int, map[string]any, *http.Cookie) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"password": password})
		code, decoded, h := b.call("POST", auth.BootstrapPath, string(body), header...)
		for _, c := range cookies(h) {
			if c.Name == "__Host-beacon_session" {
				return code, decoded, c
			}
		}
		return code, decoded, nil
	}
	me := func(session *http.Cookie) (int, map[string]any) {
		t.Helper()
		// The cookie is Secure, so it is attached by hand over plain http.
		code, body, _ := b.call("GET", auth.MePath, "", "Cookie", session.Name+"="+session.Value)
		return code, body
	}

	// A wrong password, an empty one and a request from another site.
	if code, body, session := signIn("not-the-password"); code != http.StatusUnauthorized || session != nil || body["error"] != "wrong password" {
		t.Errorf("sign-in with a wrong password = %d %v", code, body)
	}
	if code, _, session := signIn(""); code != http.StatusBadRequest || session != nil {
		t.Errorf("sign-in without a password = %d, want 400", code)
	}
	if code, _, session := signIn(bootstrapPassword, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden || session != nil {
		t.Errorf("cross-site sign-in = %d, want 403", code)
	}
	if n := b.count(`SELECT count(*) FROM users`); n != 0 {
		t.Errorf("%d users after failed sign-ins", n)
	}

	// The right password: an admin session, with the same cookie as any
	// other session.
	code, body, session := signIn(bootstrapPassword)
	if code != http.StatusOK || session == nil || body["role"] != "admin" || body["email"] != auth.BootstrapEmail {
		t.Fatalf("sign-in = %d %v", code, body)
	}
	if !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode || session.Path != "/" {
		t.Errorf("session cookie = %+v", session)
	}
	if code, who := me(session); code != http.StatusOK || who["role"] != "admin" {
		t.Errorf("GET %s as the bootstrap admin = %d %v", auth.MePath, code, who)
	}
	// It is an admin: it can create the first token.
	if code, created, _ := b.call("POST", auth.TokensPath, `{"name": "automation", "role": "admin"}`, "Cookie", session.Name+"="+session.Value); code != http.StatusCreated || created["created_by"] != auth.BootstrapEmail {
		t.Errorf("creating a token as the bootstrap admin = %d %v", code, created)
	}
	// Its user has no identity at any provider.
	if n := b.count(`SELECT count(*) FROM users WHERE email = $1 AND role = 'admin' AND oidc_issuer IS NULL AND oidc_subject IS NULL`, auth.BootstrapEmail); n != 1 {
		t.Error("the bootstrap admin's user is not as expected")
	}

	// The attempts are bounded, whatever the password: three have been made
	// in this minute (the cross-site one never got that far). The fourth
	// and fifth are let through, and the sixth is refused, even with the
	// right password.
	for _, guess := range []string{"guess-4", "guess-5"} {
		if code, _, _ := signIn(guess); code != http.StatusUnauthorized {
			t.Errorf("attempt with %s = %d, want 401", guess, code)
		}
	}
	code, body, session2 := signIn(bootstrapPassword)
	if code != http.StatusTooManyRequests || session2 != nil {
		t.Errorf("sixth attempt, with the right password = %d %v, want 429", code, body)
	}
	if code, _, h := b.call("POST", auth.BootstrapPath, `{"password": "x"}`); code != http.StatusTooManyRequests || h.Get("Retry-After") == "" {
		t.Errorf("seventh attempt = %d, want 429 with Retry-After", code)
	}
	// A minute later the right password works again.
	now = now.Add(61 * time.Second)
	if code, _, again := signIn(bootstrapPassword); code != http.StatusOK || again == nil {
		t.Errorf("sign-in after the wait = %d, want 200", code)
	}

	// Turned off: the configuration no longer has the account. Its session
	// ends at once, signing in answers 404, and the token it made lives on.
	off := *a
	off.BootstrapAdmin = nil
	off.OIDC = &config.OIDC{Issuer: "http://127.0.0.1:1/idp", ClientID: "beacon", ClientSecret: config.NewSecret(config.SecretRef{}, "x")}
	b.handler.Configure(context.Background(), &off, nil)
	if code, _ := me(session); code != http.StatusUnauthorized {
		t.Errorf("GET %s with the session of a bootstrap admin that was turned off = %d, want 401", auth.MePath, code)
	}
	now = now.Add(61 * time.Second)
	if code, _, _ := signIn(bootstrapPassword); code != http.StatusNotFound {
		t.Errorf("sign-in after the account was turned off = %d, want 404", code)
	}
	if b.count(`SELECT count(*) FROM users WHERE email = $1 AND disabled_at IS NOT NULL`, auth.BootstrapEmail) != 1 || b.count(`SELECT count(*) FROM sessions`) != 0 {
		t.Error("the user is not disabled, or its sessions were kept")
	}
	if b.count(`SELECT count(*) FROM api_tokens WHERE revoked_at IS NULL`) != 1 {
		t.Error("the token that the bootstrap admin created did not survive")
	}

	// Turned on again, it works again.
	b.handler.Configure(context.Background(), a, nil)
	now = now.Add(61 * time.Second)
	if code, _, session := signIn(bootstrapPassword); code != http.StatusOK || session == nil {
		t.Errorf("sign-in after the account was turned on again = %d", code)
	}

	logs := b.logs.String()
	if strings.Contains(logs, bootstrapPassword) || strings.Contains(logs, a.BootstrapAdmin.PasswordHash.Reveal()) || strings.Contains(logs, "guess-5") || strings.Contains(logs, "not-the-password") {
		t.Errorf("the logs contain the password, a guess or the hash:\n%s", logs)
	}
	for _, want := range []string{`"msg":"bootstrap admin signed in"`, `"msg":"bootstrap admin sign-in failed: wrong password"`, `"msg":"bootstrap admin sign-in refused: too many attempts"`, `"msg":"the bootstrap admin is turned off, and its sessions are ended"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs do not contain %s", want)
		}
	}
}

func TestBootstrapAdminIsOffByDefault(t *testing.T) {
	f := newFakeProvider(t)
	b := newBeacon(t, fakeAuth(f, "http://127.0.0.1:18080"))
	if code, body, _ := b.call("POST", auth.BootstrapPath, `{"password": "`+bootstrapPassword+`"}`); code != http.StatusNotFound {
		t.Errorf("POST %s without the account = %d %v, want 404", auth.BootstrapPath, code, body)
	}
	// A form works as well as JSON, for a person with only a browser or curl.
	b.handler.Configure(context.Background(), bootstrapAuth(t, "http://127.0.0.1:18080"), nil)
	req, _ := http.NewRequest("POST", b.server.URL+auth.BootstrapPath, strings.NewReader("password="+bootstrapPassword))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("sign-in with a form = %d, want 200", resp.StatusCode)
	}
}

// TestTokenIsJudgedAlone presents a token that is not valid together with a
// session that is. The token decides.
func TestTokenIsJudgedAlone(t *testing.T) {
	b := newBeacon(t, bootstrapAuth(t, "http://127.0.0.1:18080"))
	_, _, h := b.call("POST", auth.BootstrapPath, `{"password": "`+bootstrapPassword+`"}`)
	var cookie string
	for _, c := range cookies(h) {
		cookie = c.Name + "=" + c.Value
	}
	if code, _, _ := b.call("GET", auth.MePath, "", "Cookie", cookie); code != http.StatusOK {
		t.Fatalf("the session does not work: %d", code)
	}
	if code, _, _ := b.call("GET", auth.MePath, "", "Cookie", cookie, "Authorization", "Bearer bcn_revoked-or-made-up"); code != http.StatusUnauthorized {
		t.Errorf("a bad token with a good session = %d, want 401", code)
	}
}

// TestTokensArePagedAndMethodsAreSaid: the list of tokens takes limit and
// cursor like every list, and anyone may ask which ways to sign in exist.
func TestTokensArePagedAndMethodsAreSaid(t *testing.T) {
	b := newBeacon(t, nil)
	_, admin := b.newToken("admin", access.RoleAdmin, 0)
	for _, name := range []string{"one", "two", "three"} {
		b.newToken(name, access.RoleMember, 0)
	}
	seen := 0
	path := auth.TokensPath + "?limit=3"
	for range 4 {
		code, body, _ := b.call("GET", path, "", as(admin)...)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %v", path, code, body)
		}
		seen += len(body["tokens"].([]any))
		next, more := body["next_cursor"].(string)
		if !more {
			break
		}
		path = auth.TokensPath + "?limit=3&cursor=" + next
	}
	if seen != 4 {
		t.Errorf("%d tokens in pages of three, want 4", seen)
	}
	if code, body, _ := b.call("GET", auth.TokensPath+"?limit=0", "", as(admin)...); code != http.StatusBadRequest || body["field"] != "limit" {
		t.Errorf("limit=0 = %d %v", code, body)
	}

	// Without a credential, and with nothing configured.
	if code, body, _ := b.call("GET", auth.MethodsPath, ""); code != http.StatusOK || body["oidc"] != false || body["bootstrap_admin"] != false || len(body) != 2 {
		t.Errorf("GET %s = %d %v", auth.MethodsPath, code, body)
	}
}
