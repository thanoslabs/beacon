package server

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/metrics/metricstest"
)

// TestHTTPMetrics: requests are counted and timed by the route that took
// them, never by the address or the method that a client chose.
func TestHTTPMetrics(t *testing.T) {
	reg := NewRegistry()
	s := New(Config{Authenticator: principalFromHeader{}}, slog.New(slog.DiscardHandler), &fakeDB{}, reg)
	s.Handle(access.Route{Pattern: "GET /api/v1/things/{id}", Level: access.Member, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "a thing")
	})})
	s.Handle(access.Route{Pattern: "POST /api/v1/things", Level: access.Admin, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})})
	s.Handle(access.Route{Pattern: "GET /broken", Level: access.Public, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a handler that fails")
	})})

	member := http.Header{"X-Test-Role": {"member"}}
	for _, id := range []string{"1", "2", "3", "a-very-long-identifier-that-a-client-chose"} {
		do(t, s, http.MethodGet, "/api/v1/things/"+id, member)
	}
	do(t, s, http.MethodGet, "/api/v1/things/missing", member)
	do(t, s, http.MethodGet, "/api/v1/things/1", nil)     // 401: nobody is signed in
	do(t, s, http.MethodPost, "/api/v1/things", member)   // 403: not an admin
	do(t, s, http.MethodGet, "/broken", nil)              // 500: the handler panicked
	do(t, s, http.MethodGet, "/wp-login.php", nil)        // no route
	do(t, s, http.MethodGet, "/.env", nil)                // no route
	do(t, s, "PROPFIND", "/made/up/by/a/scanner", member) // no route, and a method of the client's
	do(t, s, http.MethodGet, PathLiveness, nil)

	got := metricstest.Values(t, reg)
	metricstest.Expect(t, got, map[string]float64{
		`beacon_http_requests_total{code="200",method="GET",route="/api/v1/things/{id}"}`:      4,
		`beacon_http_requests_total{code="404",method="GET",route="/api/v1/things/{id}"}`:      1,
		`beacon_http_requests_total{code="401",method="GET",route="/api/v1/things/{id}"}`:      1,
		`beacon_http_requests_total{code="403",method="POST",route="/api/v1/things"}`:          1,
		`beacon_http_requests_total{code="500",method="GET",route="/broken"}`:                  1,
		`beacon_http_requests_total{code="401",method="any",route="unmatched"}`:                2,
		`beacon_http_requests_total{code="404",method="any",route="unmatched"}`:                1,
		`beacon_http_requests_total{code="200",method="GET",route="/healthz"}`:                 1,
		`beacon_http_request_duration_seconds_count{method="GET",route="/api/v1/things/{id}"}`: 6,
		`beacon_http_request_duration_seconds_count{method="GET",route="/broken"}`:             1,
		`beacon_http_request_duration_seconds_count{method="any",route="unmatched"}`:           3,
	})
	// Every route has its histogram from the start, and nothing a client
	// sent is in any label.
	if _, ok := got[`beacon_http_request_duration_seconds_count{method="POST",route="/api/v1/things"}`]; !ok {
		t.Error("a route that was asked once has no histogram")
	}
	if _, ok := got[`beacon_http_request_duration_seconds_count{method="GET",route="/readyz"}`]; !ok {
		t.Error("a route that was never asked has no histogram")
	}
	for series := range got {
		for _, leak := range []string{"wp-login", ".env", "scanner", "PROPFIND", "a-very-long", "missing", "things/1"} {
			if strings.Contains(series, leak) {
				t.Errorf("%s holds %q, which a request brought", series, leak)
			}
		}
	}
}

// TestMetricsToken: without a token the metrics are open; with one, a
// scrape must carry it, and the probes stay open.
func TestMetricsToken(t *testing.T) {
	const token = "metrics-token-marker-3e9c1f"
	open := New(Config{}, slog.New(slog.DiscardHandler), &fakeDB{}, NewRegistry())
	if rec := do(t, open, http.MethodGet, PathMetrics, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "beacon_build_info") {
		t.Errorf("without a token: %d", rec.Code)
	}

	logs := &syncBuffer{}
	s := New(Config{MetricsToken: token}, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), &fakeDB{}, NewRegistry())
	for name, header := range map[string]http.Header{
		"no credential":    nil,
		"a wrong token":    {"Authorization": {"Bearer metrics-token-marker-3e9c1x"}},
		"a prefix of it":   {"Authorization": {"Bearer metrics-token"}},
		"the wrong scheme": {"Authorization": {"Basic " + token}},
		"an empty token":   {"Authorization": {"Bearer "}},
		"a session's role": {"X-Test-Role": {"admin"}},
	} {
		rec := do(t, s, http.MethodGet, PathMetrics, header)
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" || strings.Contains(rec.Body.String(), "beacon_build_info") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	for _, header := range []http.Header{{"Authorization": {"Bearer " + token}}, {"Authorization": {"bearer " + token}}} {
		if rec := do(t, s, http.MethodGet, PathMetrics, header); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "beacon_build_info") {
			t.Errorf("with the token: %d", rec.Code)
		}
	}
	for _, path := range []string{PathLiveness, PathReadiness} {
		if rec := do(t, s, http.MethodGet, path, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d with a metrics token set, want it open", path, rec.Code)
		}
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "3e9c1x") {
		t.Errorf("the logs contain a token:\n%s", logs.String())
	}
}
