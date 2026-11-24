package ingest_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/ingest"
	"github.com/thanoslabs/beacon/internal/metrics/metricstest"
)

// TestIngestMetrics: requests are counted by source and result, the alerts
// in them by what became of each, and nothing a stranger sends becomes a
// label.
func TestIngestMetrics(t *testing.T) {
	f := newFixture(t)
	reg := prometheus.NewRegistry()
	f.handler.RegisterMetrics(reg)

	// Every source has its series before anything arrives.
	start := metricstest.Values(t, reg)
	metricstest.Expect(t, start, map[string]float64{
		`beacon_ingest_requests_total{result="accepted",source="grafana"}`: 0,
		`beacon_ingest_requests_total{result="invalid",source="am"}`:       0,
		`beacon_ingest_requests_total{result="error",source="prod"}`:       0,
		`beacon_ingest_alerts_total{result="created",source="staging"}`:    0,
		`beacon_ingest_alerts_total{result="not_found",source="am"}`:       0,
		`beacon_ingest_unauthorized_requests_total`:                        0,
	})

	// The generic webhook: a new alert, a repeat of it, its resolution, and
	// a resolution of something Beacon never saw.
	for _, step := range []struct {
		body string
		code int
	}{
		{`{"labels": {"alertname": "A", "host": "db1"}}`, http.StatusCreated},
		{`{"labels": {"alertname": "A", "host": "db1"}}`, http.StatusOK},
		{`{"status": "resolved", "labels": {"alertname": "A", "host": "db1"}}`, http.StatusOK},
		{`{"status": "resolved", "labels": {"alertname": "Never"}}`, http.StatusOK},
		{`{"labels": {}}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
	} {
		if res := f.post(t, "prod", prodToken, step.body); res.code != step.code {
			t.Fatalf("POST %s = %d %v, want %d", step.body, res.code, res.body, step.code)
		}
	}
	// A notification of Alertmanager with two alerts is one request.
	var group map[string]any
	if code := f.postInto(t, "am", amToken, captured(t, "alertmanager/firing.json"), &group); code != http.StatusOK {
		t.Fatalf("alertmanager = %d %v", code, group)
	}
	alerts := group["received"].(float64)
	if alerts < 1 {
		t.Fatalf("the fixture holds no alert: %v", group)
	}

	// Strangers: a source that does not exist, under a name of their
	// choosing, and a real source with a wrong token.
	for _, attempt := range [][2]string{{"made-up-by-a-stranger", prodToken}, {"prod", "wrong-token-wrong-token"}, {"staging", ""}} {
		var out map[string]any
		if code := f.postInto(t, attempt[0], attempt[1], `{"labels": {"alertname": "A"}}`, &out); code != http.StatusUnauthorized {
			t.Fatalf("POST to %s = %d", attempt[0], code)
		}
	}

	got := metricstest.Values(t, reg)
	metricstest.Expect(t, got, map[string]float64{
		`beacon_ingest_requests_total{result="accepted",source="prod"}`:    4,
		`beacon_ingest_requests_total{result="invalid",source="prod"}`:     2,
		`beacon_ingest_requests_total{result="error",source="prod"}`:       0,
		`beacon_ingest_requests_total{result="accepted",source="am"}`:      1,
		`beacon_ingest_requests_total{result="accepted",source="staging"}`: 0,
		`beacon_ingest_alerts_total{result="created",source="prod"}`:       1,
		`beacon_ingest_alerts_total{result="updated",source="prod"}`:       1,
		`beacon_ingest_alerts_total{result="resolved",source="prod"}`:      1,
		`beacon_ingest_alerts_total{result="not_found",source="prod"}`:     1,
		`beacon_ingest_alerts_total{result="created",source="am"}`:         alerts,
		`beacon_ingest_unauthorized_requests_total`:                        3,
	})
	// Nothing new appeared: the series are those of the four sources, as
	// at the start. A name that a stranger asked for is not among them, and
	// neither is anything an alert carried.
	if len(got) != len(start) {
		t.Errorf("%d series after the requests, %d before: something that was sent became a label", len(got), len(start))
	}
	for series := range got {
		for _, leak := range []string{"made-up", "alertname", "db1", "Never"} {
			if strings.Contains(series, leak) {
				t.Errorf("%s holds %q, which a request brought", series, leak)
			}
		}
	}
}

// TestIngestCountsWhatCouldNotBeStored: a request that fails in the store
// is counted as an error, which is what an alert on Beacon itself watches.
func TestIngestCountsWhatCouldNotBeStored(t *testing.T) {
	mux := http.NewServeMux()
	src := ingest.Source{ID: "id", Source: config.Source{Name: "prod", Token: config.NewSecret(config.SecretRef{}, prodToken)}}
	h := ingest.New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), failingAlerts{}, []ingest.Source{src})
	h.Register(mux)
	reg := prometheus.NewRegistry()
	h.RegisterMetrics(reg)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/prod", strings.NewReader(`{"labels": {"a": "b"}}`))
	req.Header.Set("Authorization", "Bearer "+prodToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d", rec.Code)
	}
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{
		`beacon_ingest_requests_total{result="error",source="prod"}`:    1,
		`beacon_ingest_requests_total{result="accepted",source="prod"}`: 0,
		`beacon_ingest_alerts_total{result="created",source="prod"}`:    0,
	})
}
