package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/api"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
)

// provisionedYAML has one object of every kind, with every credential the
// kinds can hold.
const provisionedYAML = `sources:
  - name: prod
    type: alertmanager
    token: {env: T_SOURCE}
    labels: {env: prod}
    severity: {label: level, mapping: {page: critical}, default: info}
  - name: plain
    type: webhook
    token: {env: T_SOURCE}
integrations:
  - name: slack
    type: slack
    slack: {bot_token: {env: T_BOT}, signing_secret: {env: T_SIGNING}}
  - name: discord
    type: discord
    discord: {bot_token: {env: T_BOT}, application_id: "400000000000000001", public_key: 5f1c0c8e7a9b4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e}
  - name: email
    type: email
    email: {host: smtp.example.com, port: 587, tls: starttls, from: beacon@example.com, username: beacon, password: {env: T_SMTP}}
  - name: tickets
    type: webhook
    webhook:
      url: "https://user:url-password-marker@tickets.example.com/hooks/beacon?token=url-token-marker#frag"
      signing_secret: {env: T_SIGNING}
      headers: {X-Team: platform, Authorization: {env: T_HEADER}}
teams:
  - name: platform
    display_name: Platform
    members: [ada@example.com, nobody-yet@example.com]
    groups: [platform-engineers]
  - name: empty
schedules:
  - name: primary
    team: platform
    time_zone: UTC
    rotation: {members: [ada@example.com], shift_length: 1w, handoff: {day: monday, time: "09:00"}}
escalation_policies:
  - name: default
    repeat: 2
    steps:
      - notify:
          - schedule: primary
            via: [slack]
          - channel: C0ALERTS
            integration: slack
        timeout: 5m
      - notify:
          - team: platform
          - user: grace@example.com
          - integration: tickets
        timeout: 1h30m
routing:
  group_by: [alertname, cluster]
  group_wait: 10s
  default: {team: platform, escalation_policy: default}
  rules:
    - name: database
      match: {team: dba}
      match_re: {severity: "critical|page"}
      service: postgres
      escalation_policy: default
      group_interval: 1m
`

var provisionedSecrets = map[string]string{
	"T_SOURCE": "source-token-marker-0001", "T_BOT": "bot-token-marker-0002", "T_SIGNING": "signing-secret-marker-0003",
	"T_SMTP": "smtp-password-marker-0004", "T_HEADER": "Bearer header-marker-0005",
}

type provisionedFixture struct {
	t   *testing.T
	mux *http.ServeMux
	h   *api.Provisioned
}

func newProvisionedFixture(t *testing.T) *provisionedFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beacon.yaml")
	if err := os.WriteFile(path, []byte(provisionedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Read(path, config.LoadOptions{Getenv: func(k string) string { return provisionedSecrets[k] }})
	if err != nil {
		t.Fatalf("the configuration of the test is not valid: %v", err)
	}
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	// Ada is in the files and in the group, Grace in the group alone, and
	// Linus was a member and is disabled.
	_, err = pool.Exec(ctx, `INSERT INTO users (email, display_name, role, oidc_issuer, oidc_subject, last_sign_in_at, disabled_at, created_at) VALUES
		('ada@example.com', 'Ada Lovelace', 'admin', 'https://idp.example.com', 'ada', '2026-03-01T12:00:00Z', NULL, '2026-02-01T00:00:00Z'),
		('grace@example.com', '', 'member', 'https://idp.example.com', 'grace', NULL, NULL, '2026-02-02T00:00:00Z'),
		('linus@example.com', 'Linus', 'member', 'https://idp.example.com', 'linus', NULL, now(), '2026-02-03T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provision.Reconcile(ctx, pool, cfg); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO team_members (team_id, user_id, origin)
		SELECT t.id, u.id, 'group' FROM teams t, users u WHERE t.name = 'platform' AND u.email IN ('ada@example.com', 'grace@example.com', 'linus@example.com')`)
	if err != nil {
		t.Fatal(err)
	}
	f := &provisionedFixture{t: t, mux: http.NewServeMux(), h: api.NewProvisioned(slog.New(slog.DiscardHandler), pool)}
	f.h.SetConfig(cfg)
	for _, route := range f.h.Routes() {
		if route.Level != access.Member || !strings.HasPrefix(route.Pattern, "GET ") {
			t.Errorf("%s is open to %s; what the configuration defines is read by members and changed by nobody", route.Pattern, route.Level)
		}
		f.mux.Handle(route.Pattern, route.Handler)
	}
	return f
}

// get answers a request as someone with the role, with the status, the body
// decoded and the body as it was sent.
func (f *provisionedFixture) get(role access.Role, path string) (int, map[string]any, string) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req.WithContext(access.WithPrincipal(req.Context(), access.Principal{UserID: "u", Role: role})))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("GET %s answered %d %q, not JSON", path, rec.Code, rec.Body)
	}
	return rec.Code, out, strings.TrimSpace(rec.Body.String())
}

// TestProvisionedObjectsAreReadable: every kind is answered as a list and by
// name, with the defaults said out loud and without a single credential.
func TestProvisionedObjectsAreReadable(t *testing.T) {
	f := newProvisionedFixture(t)
	tests := []struct{ path, want string }{
		{"/api/v1/sources/prod", `{"name":"prod","type":"alertmanager","ingest_path":"/api/v1/ingest/prod","labels":{"env":"prod"},"severity":{"label":"level","mapping":{"page":"critical"},"default":"info"},"provisioned":true}`},
		{"/api/v1/sources/plain", `{"name":"plain","type":"webhook","ingest_path":"/api/v1/ingest/plain","labels":{},"severity":{"label":"severity","mapping":{},"default":"warning"},"provisioned":true}`},
		{"/api/v1/integrations/slack", `{"name":"slack","type":"slack","interactions_path":"/api/v1/integrations/slack/interactions","settings":{},"provisioned":true}`},
		{"/api/v1/integrations/discord", `{"name":"discord","type":"discord","interactions_path":"/api/v1/integrations/discord/interactions","settings":{"application_id":"400000000000000001","public_key":"5f1c0c8e7a9b4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e"},"provisioned":true}`},
		{"/api/v1/integrations/email", `{"name":"email","type":"email","interactions_path":null,"settings":{"authenticated":true,"from":"beacon@example.com","host":"smtp.example.com","port":587,"tls":"starttls"},"provisioned":true}`},
		// The address without what may hold a credential, and the names of
		// the headers without their values.
		{"/api/v1/integrations/tickets", `{"name":"tickets","type":"webhook","interactions_path":null,"settings":{"headers":["Authorization","X-Team"],"url":"https://tickets.example.com/hooks/beacon"},"provisioned":true}`},
		{"/api/v1/escalation-policies/default", `{"name":"default","repeat":2,"steps":[` +
			`{"timeout":"5m","notify":[{"kind":"schedule","schedule":"primary","team":null,"user":null,"channel":null,"integration":null,"via":["slack"]},` +
			`{"kind":"channel","schedule":null,"team":null,"user":null,"channel":"C0ALERTS","integration":"slack","via":[]}]},` +
			`{"timeout":"1h30m","notify":[{"kind":"team","schedule":null,"team":"platform","user":null,"channel":null,"integration":null,"via":[]},` +
			`{"kind":"user","schedule":null,"team":null,"user":"grace@example.com","channel":null,"integration":null,"via":[]},` +
			`{"kind":"webhook","schedule":null,"team":null,"user":null,"channel":null,"integration":"tickets","via":[]}]}],"provisioned":true}`},
		// A rule is answered with the grouping that applies to it.
		{"/api/v1/routing", `{"group_by":["alertname","cluster"],"group_wait":"10s","group_interval":"5m","default":{"team":"platform","escalation_policy":"default"},` +
			`"rules":[{"name":"database","match":{"team":"dba"},"match_re":{"severity":"critical|page"},"team":null,"service":"postgres","escalation_policy":"default",` +
			`"group_by":["alertname","cluster"],"group_wait":"10s","group_interval":"1m"}],"provisioned":true}`},
	}
	for _, tt := range tests {
		if code, _, raw := f.get(access.RoleMember, tt.path); code != http.StatusOK || raw != tt.want {
			t.Errorf("GET %s = %d\n%s\nwant\n%s", tt.path, code, raw, tt.want)
		}
	}
	var everything strings.Builder
	for path, want := range map[string]string{"/api/v1/sources": "prod plain", "/api/v1/integrations": "slack discord email tickets", "/api/v1/escalation-policies": "default", "/api/v1/teams": "platform empty"} {
		code, body, raw := f.get(access.RoleAdmin, path)
		everything.WriteString(raw)
		var names []string
		for _, list := range body {
			for _, item := range list.([]any) {
				names = append(names, item.(map[string]any)["name"].(string))
			}
		}
		if code != http.StatusOK || len(body) != 1 || strings.Join(names, " ") != want {
			t.Errorf("GET %s = %d %v, want %s in the order of the files", path, code, names, want)
		}
		if code, body, _ := f.get(access.RoleMember, path+"/nope"); code != http.StatusNotFound || !strings.HasPrefix(fmt.Sprint(body["error"]), "no such ") {
			t.Errorf("GET %s/nope = %d %v, want 404", path, code, body)
		}
	}
	_, _, raw := f.get(access.RoleAdmin, "/api/v1/routing")
	everything.WriteString(raw)
	// No credential, and no reference to one.
	for _, secret := range []string{"marker", "T_SOURCE", "T_BOT", "T_SIGNING", "T_SMTP", "T_HEADER", "\"env\":\"T", "password", "token=", "user:"} {
		if strings.Contains(everything.String(), secret) {
			t.Errorf("the answers contain %q:\n%s", secret, everything.String())
		}
	}

	// A configuration without the optional parts.
	f.h.SetConfig(config.Config{})
	for _, path := range []string{"/api/v1/sources", "/api/v1/integrations", "/api/v1/escalation-policies", "/api/v1/teams"} {
		if code, _, raw := f.get(access.RoleMember, path); code != http.StatusOK || !strings.HasSuffix(raw, `":[]}`) {
			t.Errorf("GET %s without configuration = %d %s, want an empty list", path, code, raw)
		}
	}
	if code, body, _ := f.get(access.RoleMember, "/api/v1/routing"); code != http.StatusNotFound || !strings.Contains(fmt.Sprint(body["error"]), "no routing block") {
		t.Errorf("GET routing without a routing block = %d %v", code, body)
	}
}

// TestTeams: a team is answered with what the files say and with who
// belongs to it now, and the addresses that no user has are for admins.
func TestTeams(t *testing.T) {
	f := newProvisionedFixture(t)
	const members = `"members":[{"email":"ada@example.com","display_name":"Ada Lovelace","origins":["config","group"]},{"email":"grace@example.com","display_name":null,"origins":["group"]}]`
	code, _, raw := f.get(access.RoleMember, "/api/v1/teams/platform")
	if want := `{"name":"platform","display_name":"Platform","groups":["platform-engineers"],` + members + `,"schedules":["primary"],"provisioned":true}`; code != http.StatusOK || raw != want {
		t.Errorf("as a member = %d\n%s\nwant\n%s", code, raw, want)
	}
	code, _, raw = f.get(access.RoleAdmin, "/api/v1/teams/platform")
	if want := `{"name":"platform","display_name":"Platform","groups":["platform-engineers"],` + members + `,"schedules":["primary"],"skipped_members":["nobody-yet@example.com"],"provisioned":true}`; code != http.StatusOK || raw != want {
		t.Errorf("as an admin = %d\n%s\nwant\n%s", code, raw, want)
	}
	if _, _, raw := f.get(access.RoleAdmin, "/api/v1/teams/empty"); raw != `{"name":"empty","display_name":"empty","groups":[],"members":[],"schedules":[],"skipped_members":[],"provisioned":true}` {
		t.Errorf("a team without anything = %s", raw)
	}
}

func TestUsers(t *testing.T) {
	f := newProvisionedFixture(t)
	emails := func(body map[string]any) string {
		var out []string
		for _, item := range body["users"].([]any) {
			out = append(out, item.(map[string]any)["email"].(string))
		}
		return strings.Join(out, " ")
	}
	code, body, raw := f.get(access.RoleMember, "/api/v1/users")
	if code != http.StatusOK || emails(body) != "ada@example.com grace@example.com linus@example.com" {
		t.Fatalf("users = %d %s", code, raw)
	}
	ada, _ := json.Marshal(body["users"].([]any)[0])
	id := body["users"].([]any)[0].(map[string]any)["id"].(string)
	if want := `{"created_at":"2026-02-01T00:00:00Z","disabled":false,"display_name":"Ada Lovelace","email":"ada@example.com","id":"` + id + `","last_sign_in_at":"2026-03-01T12:00:00Z","role":"admin","teams":["platform"]}`; string(ada) != want || !api.IsUUID(id) {
		t.Errorf("Ada = %s\nwant  %s", ada, want)
	}
	if linus := body["users"].([]any)[2].(map[string]any); linus["disabled"] != true || linus["last_sign_in_at"] != nil {
		t.Errorf("a disabled user = %v", linus)
	}
	for query, want := range map[string]string{
		"role=admin": "ada@example.com", "role=member": "grace@example.com linus@example.com",
		"team=platform": "ada@example.com grace@example.com linus@example.com", "team=empty": "", "team=platform&role=member": "grace@example.com linus@example.com",
	} {
		if code, body, _ := f.get(access.RoleMember, "/api/v1/users?"+query); code != http.StatusOK || emails(body) != want {
			t.Errorf("?%s = %d %q, want %q", query, code, emails(body), want)
		}
	}
	// A page at a time.
	var seen []string
	cursor := ""
	for range 5 {
		_, body, _ := f.get(access.RoleMember, "/api/v1/users?limit=2&cursor="+url.QueryEscape(cursor))
		seen = append(seen, emails(body))
		next, more := body["next_cursor"].(string)
		if !more {
			break
		}
		cursor = next
	}
	if got := strings.Join(seen, " | "); got != "ada@example.com grace@example.com | linus@example.com" {
		t.Errorf("users in pages of two = %s", got)
	}
	for query, field := range map[string]string{"role=owner": "role", "limit=0": "limit", "cursor=x": "cursor", "email=ada": "email"} {
		if code, body, _ := f.get(access.RoleMember, "/api/v1/users?"+query); code != http.StatusBadRequest || body["field"] != field {
			t.Errorf("?%s = %d %v, want 400 naming %s", query, code, body, field)
		}
	}
}

// TestSlice: a list that is read whole is paged like any other.
func TestSlice(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	var pages []string
	q := url.Values{"limit": {"2"}}
	for range 5 {
		page, next, _, _, ok := api.Slice(items, q)
		if !ok || page == nil {
			t.Fatalf("Slice(%v) failed", q)
		}
		pages = append(pages, fmt.Sprint(page))
		if next == "" {
			break
		}
		q.Set("cursor", next)
	}
	if got := strings.Join(pages, " "); got != "[1 2] [3 4] [5]" {
		t.Errorf("pages = %s", got)
	}
	// Past the end, and without a limit.
	if page, next, _, _, ok := api.Slice(items, url.Values{"cursor": {api.Cursor(map[string]int{"offset": 99})}}); !ok || len(page) != 0 || next != "" {
		t.Errorf("past the end = %v %q %v", page, next, ok)
	}
	if page, next, _, _, ok := api.Slice([]int(nil), url.Values{}); !ok || page == nil || next != "" {
		t.Errorf("an empty list = %v %q %v", page, next, ok)
	}
	for _, bad := range []url.Values{{"limit": {"500"}}, {"cursor": {"nope"}}, {"cursor": {api.Cursor(map[string]int{"offset": -1})}}} {
		if _, _, field, problem, ok := api.Slice(items, bad); ok || field == "" || problem == "" {
			t.Errorf("Slice(%v) succeeded", bad)
		}
	}
}
