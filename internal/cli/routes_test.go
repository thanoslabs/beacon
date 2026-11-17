package cli

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/server"
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
//   - the interactions endpoint checks Slack's signature;
//   - /auth/login and /auth/callback are how a person gets a session.
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
}

var pathParamRE = regexp.MustCompile(`\{[a-z]+\}`)

func TestRoutes(t *testing.T) {
	// The routes, registered as beacon serve registers them.
	logger := slog.New(slog.DiscardHandler)
	srv := server.New(server.Config{}, logger, nil, server.NewRegistry())
	for _, route := range (&provisioner{auth: auth.NewHandler(logger, nil)}).routes() {
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
	if closed != 3 {
		t.Errorf("%d routes refused a stranger, want 3", closed)
	}
	for _, probe := range []string{"GET /", "GET /api/v1/alerts", "POST /api/v1/tokens", "DELETE /api/v1/me", "GET /auth/logout", "PUT /healthz", "GET /api/v1/config/reload", "GET /admin"} {
		method, path, _ := strings.Cut(probe, " ")
		if code, body := request(method, path); code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d %s, want 401", probe, code, body)
		}
	}
}
