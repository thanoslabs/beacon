package cli

import (
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/thanoslabs/beacon/internal/metrics"
	"github.com/thanoslabs/beacon/internal/metrics/metricstest"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

// scrapeMetrics reads /metrics of a running server as Prometheus would.
func scrapeMetrics(t *testing.T, srv *serving, token string) (status int, body string, families map[string]*dto.MetricFamily) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.baseURL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, string(raw), nil
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err = parser.TextToMetricFamilies(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("the metrics are not in the text format: %v", err)
	}
	return resp.StatusCode, string(raw), families
}

// TestMetricsMatchTheCatalogue scrapes the real server after it has taken
// alerts, notified and answered the API, and holds what it exposes to the
// catalogue: every Beacon metric is described there with exactly the labels
// it has, every metric of the catalogue is exposed, the output passes the
// linter of the Prometheus client library, and nothing that an alert or a
// stranger brought, and nothing about a person, is in it.
//
// With the tests of internal/metrics, which hold the catalogue to the
// allow-list of labels and the source tree to the catalogue, this is what
// keeps a label with unbounded values from reaching /metrics.
func TestMetricsMatchTheCatalogue(t *testing.T) {
	const (
		sourceToken = "source-token-marker-7c2e41d0"
		botToken    = "xoxb-bot-token-marker-55d1"
	)
	fake := slacktest.New(t, botToken)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL": postgrestest.NewURL(t), "BEACON_CONFIG": writeConfig(t, openAPIConfig), "BEACON_SOURCE_TOKEN": sourceToken,
		"BEACON_SLACK_BOT_TOKEN": botToken, "BEACON_SLACK_SIGNING_SECRET": "signing-secret-marker-e07b", "BEACON_SLACK_API_URL": fake.URL,
		"BEACON_HOOK_SIGNING_SECRET": "hook-signing-secret-marker-4d1a", "BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH": "$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W",
	})
	post := func(source, token, body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.baseURL+"/api/v1/ingest/"+source, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck,gosec // test cleanup
		return resp.StatusCode
	}
	// What must never show up in a label: all of it is sent or asked for
	// below.
	unbounded := []string{"QueueTooDeep", "db-host-7", "checkout-from-a-label", "a1b2c3d4fingerprint", "invented-source", "ada@example.com", "C0ALERTS", "no-such-page"}
	if code := post("default", sourceToken, `{"fingerprint": "a1b2c3d4fingerprint", "labels": {"alertname": "QueueTooDeep", "host": "db-host-7", "service": "checkout-from-a-label", "severity": "critical"}}`); code != http.StatusCreated {
		t.Fatalf("fire = %d", code)
	}
	if code := post("default", sourceToken, `{"labels": {"alertname": "QueueTooDeep", "team": "dba", "severity": "page"}}`); code != http.StatusCreated {
		t.Fatalf("fire = %d", code)
	}
	post("invented-source", sourceToken, `{"labels": {"alertname": "QueueTooDeep"}}`)
	post("default", sourceToken, `{"labels": {}}`)
	for _, path := range []string{"/healthz", "/readyz", "/api/v1/alerts", "/api/v1/alerts/no-such-page", "/no-such-page", "/api/v1/openapi.json"} {
		resp, err := http.Get(srv.baseURL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck,gosec // test cleanup
	}
	waitFor(t, "the groups to be posted to Slack", func() bool { return len(fake.Messages()) >= 2 })

	// Every metric of the catalogue is exposed. Some follow background
	// work, so the scrape is repeated until they are all there.
	var (
		body     string
		families map[string]*dto.MetricFamily
	)
	missing := func() []string {
		var out []string
		for _, d := range metrics.Catalog() {
			if families[d.Name] == nil {
				out = append(out, d.Name)
			}
		}
		return out
	}
	waitFor(t, "a scrape with every metric of the catalogue", func() bool {
		var status int
		if status, body, families = scrapeMetrics(t, srv, ""); status != http.StatusOK {
			t.Fatalf("GET /metrics = %d", status)
		}
		return len(missing()) == 0
	})
	if t.Failed() {
		t.Fatalf("missing from the scrape: %v", missing())
	}

	for name, family := range families {
		if !strings.HasPrefix(name, "beacon_") {
			continue
		}
		d, ok := metrics.Lookup(name)
		if !ok {
			t.Errorf("%s is exposed and is not in the catalogue of internal/metrics", name)
			continue
		}
		if got := strings.ToLower(family.GetType().String()); got != string(d.Type) {
			t.Errorf("%s is a %s, and a %s in the catalogue", name, got, d.Type)
		}
		want := slices.Sorted(slices.Values(slices.Concat(d.Labels, d.ConstLabels)))
		for _, m := range family.GetMetric() {
			var got []string
			for _, l := range m.GetLabel() {
				got = append(got, l.GetName())
				if l.GetValue() == "" {
					t.Errorf("%s has an empty %s", metricstest.Series(name, m), l.GetName())
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("%s has the labels %v, and %v in the catalogue", metricstest.Series(name, m), got, want)
			}
		}
	}

	problems, err := promlint.New(strings.NewReader(body)).Lint()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "beacon_") {
			t.Errorf("promlint: %s: %s", p.Metric, p.Text)
		}
	}

	// Nothing unbounded, nothing about a person and no credential.
	for _, leak := range slices.Concat(unbounded, []string{sourceToken, botToken, "bootstrap-admin", "U0"}) {
		if strings.Contains(body, leak) {
			t.Errorf("/metrics contains %q", leak)
		}
	}
	identifier := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[^" ]+@[^" ]+\.[a-z]+`)
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "beacon_") && identifier.MatchString(line) {
			t.Errorf("a series holds an identifier or an address: %s", line)
		}
	}

	// What happened is what was counted.
	values := metricstest.FromFamilies(slices.Collect(mapValues(families)))
	metricstest.Expect(t, values, map[string]float64{
		`beacon_routing_unmatched_alerts_total{source="default"}`:                              1,
		`beacon_http_requests_total{code="201",method="POST",route="/api/v1/ingest/{source}"}`: 2,
		`beacon_http_requests_total{code="401",method="GET",route="/api/v1/alerts/{id}"}`:      1,
		`beacon_http_requests_total{code="200",method="GET",route="/healthz"}`:                 1,
		`beacon_config_last_reload_successful`:                                                 1,
	})
}

func mapValues[K comparable, V any](m map[K]V) func(func(V) bool) {
	return func(yield func(V) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
}
