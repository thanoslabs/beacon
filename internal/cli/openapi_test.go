package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/thanoslabs/beacon/internal/api/apitest"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

// openAPIConfig has something of every kind, so that every route has
// something to answer.
const openAPIConfig = `sources:
  - name: default
    type: webhook
    token: {env: BEACON_SOURCE_TOKEN}
    labels: {env: test}
integrations:
  - name: slack
    type: slack
    slack: {bot_token: {env: BEACON_SLACK_BOT_TOKEN}, signing_secret: {env: BEACON_SLACK_SIGNING_SECRET}}
  - name: tickets
    type: webhook
    webhook: {url: "http://127.0.0.1:1/hook?token=x", signing_secret: {env: BEACON_HOOK_SIGNING_SECRET}, headers: {X-Team: platform}}
teams:
  - name: platform
    display_name: Platform
    members: [ada@example.com]
    groups: [platform-engineers]
schedules:
  - name: primary
    team: platform
    time_zone: Europe/London
    rotation: {members: [ada@example.com, bootstrap-admin@beacon.invalid], shift_length: 1w, handoff: {day: monday, time: "09:00"}}
escalation_policies:
  - name: default
    repeat: 1
    steps:
      - notify:
          - channel: C0ALERTS
            integration: slack
          - schedule: primary
          - team: platform
          - user: ada@example.com
            via: [slack]
          - integration: tickets
        timeout: 5m
routing:
  group_wait: 0s
  group_interval: 1s
  default: {team: platform, escalation_policy: default}
  rules:
    - name: database
      match: {team: dba}
      match_re: {severity: "critical|page"}
      service: postgres
      escalation_policy: default
auth:
  public_url: http://127.0.0.1:18080
  bootstrap_admin: {password_hash: {env: BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH}}
`

// notChecked are the operations of which this test checks no successful
// answer, each with the reason. Everything else must be seen to succeed.
var notChecked = map[string]string{
	"GET /auth/login":    "a success is a redirect to an identity provider, which this test does not have; internal/e2e signs in through Dex",
	"GET /auth/callback": "as for /auth/login",
	"POST /api/v1/me/identities/{integration}/link-code": "needs a person who signed in through an identity provider; TestLinkCodeAPI in internal/identity checks its answer against the description",
	"DELETE /api/v1/me/identities/{integration}":         "as for the link code",
}

// TestAnswersMatchTheOpenAPIDescription uses every route of the real server
// and checks each answer against the OpenAPI description: the status must
// be described, and the body must have the fields the schema says and no
// others. With TestRoutes, which holds the routes and the description to the
// same list, this is what keeps the description from drifting.
func TestAnswersMatchTheOpenAPIDescription(t *testing.T) {
	const (
		sourceToken   = "source-token-marker-7c2e41d0"
		signingSecret = "signing-secret-marker-e07b"
		password      = "openapi-test-password"
	)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		t.Fatal(err)
	}
	fake := slacktest.New(t, "xoxb-bot-token-marker-55d1")
	path := writeConfig(t, openAPIConfig)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL": postgrestest.NewURL(t), "BEACON_CONFIG": path, "BEACON_SOURCE_TOKEN": sourceToken,
		"BEACON_SLACK_BOT_TOKEN": "xoxb-bot-token-marker-55d1", "BEACON_SLACK_SIGNING_SECRET": signingSecret, "BEACON_SLACK_API_URL": fake.URL,
		"BEACON_HOOK_SIGNING_SECRET": "hook-signing-secret-marker-4d1a", "BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH": string(hash),
	})
	spec := apitest.Load(t)
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// call sends a request, checks the answer against the description, and
	// returns the status and the decoded body.
	call := func(method, target, token, body string, headers ...string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(method, srv.baseURL+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		raw, _ := io.ReadAll(resp.Body)
		op, ok := spec.Match(method, target)
		if !ok {
			t.Fatalf("%s %s is not in the OpenAPI description", method, target)
		}
		if err := spec.Check(op, resp.StatusCode, resp.Header.Get("Content-Type"), raw); err != nil {
			t.Error(err)
		}
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		return resp.StatusCode, decoded
	}
	// want fails the test when a step of the walk did not go as planned,
	// which would leave later answers unchecked.
	want := func(what string, got int, body map[string]any, status int) map[string]any {
		t.Helper()
		if got != status {
			t.Fatalf("%s = %d %v, want %d", what, got, body, status)
		}
		return body
	}

	// Machines.
	for _, target := range []string{"/healthz", "/readyz", "/metrics", "/api/v1/openapi.json", "/api/v1/openapi.yaml", "/auth/methods"} {
		code, body := call("GET", target, "", "")
		want("GET "+target, code, body, http.StatusOK)
	}
	fire := func(host string) string {
		t.Helper()
		code, body := call("POST", "/api/v1/ingest/default", sourceToken, `{"labels": {"alertname": "DiskFull", "host": "`+host+`", "severity": "critical"}, "annotations": {"summary": "Disk is full"}}`)
		return want("fire", code, body, http.StatusCreated)["alert_id"].(string)
	}
	one, two, three := fire("db1"), fire("db2"), fire("db3")
	// The group is told about before anyone acts, so that the timelines
	// below hold notification and escalation events too.
	waitFor(t, "the group to be told about", func() bool { return len(fake.Messages()) > 0 })
	code, body := call("POST", "/api/v1/ingest/default", sourceToken, `{"status": "resolved", "labels": {"alertname": "DiskFull", "host": "db3", "severity": "critical"}}`)
	want("resolve from the source", code, body, http.StatusOK)
	call("POST", "/api/v1/ingest/default", sourceToken, `{"labels": {}}`)
	call("POST", "/api/v1/ingest/default", "wrong-token", `{}`)
	// What a chat service sends: signed by Slack, and of a kind Beacon has
	// no use for, which it still has to accept.
	interaction := url.Values{"payload": {`{"type": "view_submission"}`}}.Encode()
	timestamp, signature := slacktest.Sign(signingSecret, time.Now(), []byte(interaction))
	code, body = call("POST", "/api/v1/integrations/slack/interactions", "", interaction, "Content-Type", "application/x-www-form-urlencoded", "X-Slack-Request-Timestamp", timestamp, "X-Slack-Signature", signature)
	want("a signed interaction", code, body, http.StatusOK)
	call("POST", "/api/v1/integrations/slack/interactions", "", interaction, "Content-Type", "application/x-www-form-urlencoded")

	// Without a credential every other route answers the same error.
	call("GET", "/api/v1/alerts", "", "")
	call("GET", "/auth/login", "", "")
	call("POST", "/auth/bootstrap", "", `{"password": "wrong"}`)
	code, body = call("POST", "/auth/bootstrap", "", `{"password": "`+password+`"}`)
	want("bootstrap sign-in", code, body, http.StatusOK)

	// Access: the session, tokens, chat accounts.
	code, body = call("GET", "/api/v1/me", "", "")
	want("me", code, body, http.StatusOK)
	code, body = call("POST", "/api/v1/tokens", "", `{"name": "ci", "role": "member", "expires_in": "30d"}`)
	created := want("create a token", code, body, http.StatusCreated)
	member := created["token"].(string)
	call("POST", "/api/v1/tokens", "", `{"name": "", "role": "owner"}`)
	call("GET", "/api/v1/tokens", member, "")
	code, body = call("GET", "/api/v1/tokens?limit=1", "", "")
	want("list tokens", code, body, http.StatusOK)
	code, body = call("GET", "/api/v1/me", member, "")
	want("me, as a token", code, body, http.StatusOK)
	code, body = call("GET", "/api/v1/me/identities", "", "")
	want("identities", code, body, http.StatusOK)
	call("POST", "/api/v1/me/identities/slack/link-code", "", "")
	call("DELETE", "/api/v1/me/identities/nope", "", "")

	// Alerts.
	for _, target := range []string{"/api/v1/alerts", "/api/v1/alerts?status=open&severity=critical&limit=1", "/api/v1/alerts/" + one, "/api/v1/alerts/" + one + "/timeline", "/api/v1/alerts/" + three + "/timeline?limit=1"} {
		code, body := call("GET", target, member, "")
		want("GET "+target, code, body, http.StatusOK)
	}
	call("GET", "/api/v1/alerts?status=nope", member, "")
	call("GET", "/api/v1/alerts/not-an-id", member, "")
	code, body = call("POST", "/api/v1/alerts/"+one+"/acknowledge", member, "")
	acked := want("acknowledge", code, body, http.StatusOK)
	call("POST", "/api/v1/alerts/"+one+"/acknowledge", member, "")
	code, body = call("POST", "/api/v1/alerts/"+one+"/snooze", "", `{"duration": "1h"}`)
	want("snooze", code, body, http.StatusOK)
	call("POST", "/api/v1/alerts/"+one+"/snooze", "", `{}`)
	code, body = call("POST", "/api/v1/alerts/"+one+"/resolve", "", "")
	want("resolve", code, body, http.StatusOK)
	group := "/api/v1/alert-groups/" + acked["group_id"].(string)
	code, body = call("GET", group, member, "")
	want("group", code, body, http.StatusOK)
	code, body = call("POST", group+"/acknowledge", member, "")
	want("acknowledge the group", code, body, http.StatusOK)
	code, body = call("POST", group+"/snooze", member, `{"until": "`+time.Now().Add(time.Hour).Format(time.RFC3339)+`"}`)
	want("snooze the group", code, body, http.StatusOK)
	code, body = call("POST", group+"/resolve", member, "")
	want("resolve the group", code, body, http.StatusOK)
	call("POST", group+"/resolve", member, "")
	call("GET", "/api/v1/alert-groups/11111111-2222-4333-8444-555555555555", member, "")
	// The timeline of an alert that was told about, with its notification
	// and escalation events.
	code, body = call("GET", "/api/v1/alerts/"+two+"/timeline", member, "")
	want("timeline", code, body, http.StatusOK)

	// What the configuration defines, as a member and as an admin.
	for _, target := range []string{
		"/api/v1/sources", "/api/v1/sources/default", "/api/v1/integrations", "/api/v1/integrations/slack", "/api/v1/integrations/tickets",
		"/api/v1/escalation-policies", "/api/v1/escalation-policies/default", "/api/v1/routing", "/api/v1/teams", "/api/v1/teams/platform",
		"/api/v1/teams/platform/on-call", "/api/v1/users", "/api/v1/users?role=admin&limit=1",
		"/api/v1/schedules", "/api/v1/schedules/primary", "/api/v1/schedules/primary/shifts", "/api/v1/schedules/primary/overrides?past=true",
	} {
		for _, token := range []string{member, ""} {
			code, body := call("GET", target, token, "")
			want("GET "+target, code, body, http.StatusOK)
		}
	}
	for _, target := range []string{"/api/v1/sources/nope", "/api/v1/teams/nope", "/api/v1/teams/nope/on-call", "/api/v1/schedules/nope", "/api/v1/users?limit=0", "/api/v1/schedules/primary/shifts?from=yesterday"} {
		call("GET", target, member, "")
	}

	// Silences and overrides.
	const matchers = `"matchers": [{"label": "alertname", "operator": "=", "value": "DiskFull"}]`
	code, body = call("POST", "/api/v1/silences/preview", member, `{`+matchers+`}`)
	want("preview", code, body, http.StatusOK)
	code, body = call("POST", "/api/v1/silences", member, `{`+matchers+`, "duration": "1h", "comment": "replacing the disk"}`)
	silence := want("create a silence", code, body, http.StatusCreated)["id"].(string)
	call("POST", "/api/v1/silences", "", `{"matchers": []}`)
	code, body = call("POST", "/api/v1/silences", "", `{`+matchers+`, "duration": "1h", "comment": "by a person"}`)
	want("create a silence in the browser", code, body, http.StatusCreated)
	for _, target := range []string{"/api/v1/silences", "/api/v1/silences?expired=true&limit=1", "/api/v1/silences/" + silence} {
		code, body := call("GET", target, member, "")
		want("GET "+target, code, body, http.StatusOK)
	}
	code, body = call("DELETE", "/api/v1/silences/"+silence, member, "")
	want("expire a silence", code, body, http.StatusNoContent)
	call("DELETE", "/api/v1/silences/nope", member, "")
	code, body = call("POST", "/api/v1/schedules/primary/overrides", member, `{"user": "bootstrap-admin@beacon.invalid", "duration": "8h", "reason": "covering"}`)
	override := want("create an override", code, body, http.StatusCreated)["id"].(string)
	call("POST", "/api/v1/schedules/primary/overrides", member, `{"user": "nobody@example.com", "duration": "8h"}`)
	for _, target := range []string{"/api/v1/schedules/primary/overrides", "/api/v1/schedules/primary", "/api/v1/teams/platform/on-call", "/api/v1/schedules/primary/shifts"} {
		code, body := call("GET", target, member, "")
		want("GET "+target+" during an override", code, body, http.StatusOK)
	}
	code, body = call("DELETE", "/api/v1/schedules/primary/overrides/"+override, member, "")
	want("remove an override", code, body, http.StatusNoContent)

	// Admin routes: refused to a member, a reload that fails and one that
	// works, and the end of a token and of the session.
	call("POST", "/api/v1/config/reload", member, "")
	file := filepath.Join(path, "beacon.yaml")
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		file = path
	}
	good, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(string(good), "escalation_policy: default}\n  rules", "escalation_policy: missing}\n  rules", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = call("POST", "/api/v1/config/reload", "", "")
	want("a reload of a broken configuration", code, body, http.StatusUnprocessableEntity)
	if err := os.WriteFile(file, good, 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = call("POST", "/api/v1/config/reload", "", "")
	want("reload", code, body, http.StatusOK)
	code, body = call("DELETE", "/api/v1/tokens/"+created["id"].(string), "", "")
	want("revoke a token", code, body, http.StatusNoContent)
	call("DELETE", "/api/v1/tokens/nope", "", "")
	code, body = call("POST", "/auth/logout", "", "")
	want("sign out", code, body, http.StatusNoContent)
	call("GET", "/api/v1/me", "", "")

	// Every operation was seen to succeed, but for the ones listed above.
	for _, key := range spec.Unseen() {
		if _, excused := notChecked[key]; !excused {
			t.Errorf("no successful answer of %s was checked against the OpenAPI description; use it in this test", key)
		}
	}
	for key := range notChecked {
		if !strings.Contains(fmt.Sprint(spec.Unseen()), key) {
			t.Errorf("%s is listed as not checked, and was", key)
		}
	}
}
