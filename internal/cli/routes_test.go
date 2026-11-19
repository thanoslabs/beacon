package cli

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/api"
	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/identity"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/schedule"
	"github.com/thanoslabs/beacon/internal/server"
	"github.com/thanoslabs/beacon/internal/silence"
)

// wantRoutes is every route of beacon serve and who may use it. It is
// written out here on purpose, apart from the code that registers the
// routes: adding a route, or changing its level, fails TestRoutes until this
// list says the same, which makes the level a decision taken twice.
//
// The public ones are the allow-list of what a stranger can reach. Each is
// there for a reason:
//
//   - /healthz, /readyz and /metrics are called by machines with no session;
//   - the ingest endpoint checks the source's own token;
//   - the interactions endpoint checks Slack's signature, or Discord's when
//     the integration named in the path is a Discord one;
//   - /auth/login and /auth/callback are how a person gets a session;
//   - /auth/bootstrap is how the bootstrap admin gets one: it checks the
//     password and bounds the attempts, and answers 404 unless the account
//     is enabled.
var wantRoutes = []string{
	"public GET /healthz",
	"public GET /readyz",
	"public GET /metrics",
	"public POST /api/v1/ingest/{source}",
	"public POST /api/v1/integrations/{name}/interactions",
	"admin  POST /api/v1/config/reload",
	"public GET /auth/login",
	"public GET /auth/callback",
	"member POST /auth/logout",
	"member GET /api/v1/me",
	"public POST /auth/bootstrap",
	"admin  GET /api/v1/tokens",
	"admin  POST /api/v1/tokens",
	"admin  DELETE /api/v1/tokens/{id}",
	"member GET /api/v1/alerts",
	"member GET /api/v1/alerts/{id}",
	"member GET /api/v1/alerts/{id}/timeline",
	"member GET /api/v1/alert-groups/{id}",
	"member POST /api/v1/alerts/{id}/acknowledge",
	"member POST /api/v1/alert-groups/{id}/acknowledge",
	"member POST /api/v1/alerts/{id}/snooze",
	"member POST /api/v1/alert-groups/{id}/snooze",
	"member POST /api/v1/alerts/{id}/resolve",
	"member POST /api/v1/alert-groups/{id}/resolve",
	"member GET /api/v1/silences",
	"member POST /api/v1/silences",
	"member GET /api/v1/silences/{id}",
	"member DELETE /api/v1/silences/{id}",
	"member GET /api/v1/schedules",
	"member GET /api/v1/schedules/{name}",
	"member GET /api/v1/schedules/{name}/shifts",
	"member GET /api/v1/teams/{name}/on-call",
	"member GET /api/v1/schedules/{name}/overrides",
	"member POST /api/v1/schedules/{name}/overrides",
	"member DELETE /api/v1/schedules/{name}/overrides/{id}",
	"member GET /api/v1/me/identities",
	"member POST /api/v1/me/identities/{integration}/link-code",
	"member DELETE /api/v1/me/identities/{integration}",
}

var pathParamRE = regexp.MustCompile(`\{[a-z]+\}`)

func TestRoutes(t *testing.T) {
	// The routes, registered as beacon serve registers them.
	logger := slog.New(slog.DiscardHandler)
	srv := server.New(server.Config{}, logger, nil, server.NewRegistry())
	for _, route := range (&provisioner{auth: auth.NewHandler(logger, nil), alertsAPI: api.NewAlerts(logger, nil), silences: silence.NewHandler(logger, nil), scheduleAPI: schedule.NewHandler(logger, nil),
		identityAPI: identity.NewHandler(logger, nil, nil)}).routes() {
		srv.Handle(route)
	}
	var got []string
	for _, r := range srv.Routes() {
		got = append(got, strings.TrimSpace(r.Level.String()+strings.Repeat(" ", 7-len(r.Level.String()))+r.Pattern))
	}
	if strings.Join(got, "\n") != strings.Join(wantRoutes, "\n") {
		t.Fatalf("routes:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantRoutes, "\n"))
	}

	// Against a running server, nobody is signed in. Every route that is
	// not public must refuse, and so must everything that is not a route.
	running := startServe(t, map[string]string{"BEACON_DATABASE_URL": postgrestest.NewURL(t)})
	request := func(method, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, running.baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		// As a browser would send it, with a cookie that is not a session.
		req.Header.Set("Cookie", "beacon_session=not-a-session; __Host-beacon_session=not-a-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	closed := 0
	for _, route := range wantRoutes {
		fields := strings.Fields(route)
		level, method, path := fields[0], fields[1], pathParamRE.ReplaceAllString(fields[2], "x")
		code, body := request(method, path)
		switch {
		case level == "public" && strings.Contains(body, "authentication is required"):
			t.Errorf("%s %s is public but asked for a session: %d %s", method, path, code, body)
		case level != "public" && (code != http.StatusUnauthorized || !strings.Contains(body, "authentication is required")):
			t.Errorf("%s %s without a session = %d %s, want 401", method, path, code, body)
		case level != "public":
			closed++
		}
	}
	if closed != 30 {
		t.Errorf("%d routes refused a stranger, want 30", closed)
	}
	for _, probe := range []string{"GET /", "PUT /api/v1/alerts", "DELETE /api/v1/alerts/x", "GET /api/v1/alert-groups", "POST /api/v1/alerts/x/delete", "GET /api/v1/teams", "GET /api/v1/teams/x", "POST /api/v1/schedules", "DELETE /api/v1/schedules/x", "GET /api/v1/schedules/x/overrides/y", "PUT /api/v1/schedules/x/overrides", "PUT /api/v1/tokens", "DELETE /api/v1/me", "GET /auth/logout", "PUT /healthz", "GET /api/v1/config/reload", "GET /admin"} {
		method, path, _ := strings.Cut(probe, " ")
		if code, body := request(method, path); code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d %s, want 401", probe, code, body)
		}
	}
}
