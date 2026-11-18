package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/ingest"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
)

const (
	prodToken    = "prod-token-0123456789abcdef"
	stagingToken = "staging-token-0123456789abcdef"
	amToken      = "alertmanager-token-0123456789abcdef"
)

type fixture struct {
	pool  *pgxpool.Pool
	store *alert.Store
	mux   *http.ServeMux
	logs  *bytes.Buffer
	ids   map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := postgrestest.NewMigrated(t)
	f := &fixture{pool: pool, store: alert.NewStore(pool), mux: http.NewServeMux(), logs: &bytes.Buffer{}, ids: map[string]string{}}

	sources := []config.Source{
		{Name: "prod", Type: config.SourceTypeWebhook, Token: config.NewSecret(config.SecretRef{}, prodToken),
			Labels: map[string]string{"environment": "production", "team": "from-source"}},
		{Name: "staging", Type: config.SourceTypeWebhook, Token: config.NewSecret(config.SecretRef{}, stagingToken),
			Severity: config.SeverityMapping{Label: "priority", Mapping: map[string]alert.Severity{"page": alert.SeverityCritical}, Default: alert.SeverityInfo}},
		{Name: "am", Type: config.SourceTypeAlertmanager, Token: config.NewSecret(config.SecretRef{}, amToken),
			Labels: map[string]string{"environment": "production"}},
	}
	rows, err := provision.Reconcile(context.Background(), pool, config.Config{Sources: sources})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f.ids = rows.IDs[provision.Sources]
	var configured []ingest.Source
	for _, s := range sources {
		configured = append(configured, ingest.Source{ID: f.ids[s.Name], Source: s})
	}
	ingest.New(slog.New(slog.NewJSONHandler(f.logs, nil)), f.store, configured).Register(f.mux)
	return f
}

type result struct {
	code int
	body map[string]string
}

func (f *fixture) post(t *testing.T, source, token, body string) result {
	t.Helper()
	res := result{}
	res.code = f.postInto(t, source, token, body, &res.body)
	return res
}

// postInto sends body to a source and decodes the response into out.
func (f *fixture) postInto(t *testing.T, source, token, body string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/"+source, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("response %d is not the expected JSON: %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	return rec.Code
}

const diskFull = `{
	"labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"},
	"annotations": {"summary": "Disk is full", "runbook_url": "https://runbooks.example.com/disk"},
	"starts_at": "2026-03-01T11:50:00Z"
}`

func TestFireAndResolve(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	res := f.post(t, "prod", prodToken, diskFull)
	if res.code != http.StatusCreated || res.body["result"] != "created" || res.body["status"] != "firing" {
		t.Fatalf("fire = %d %v", res.code, res.body)
	}
	a, err := f.store.Get(ctx, res.body["alert_id"])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a.SourceID != f.ids["prod"] || a.Status != alert.StatusFiring || a.Severity != alert.SeverityCritical {
		t.Errorf("alert = %+v", a)
	}
	wantLabels := map[string]string{"alertname": "DiskFull", "host": "db1", "severity": "critical", "environment": "production", "team": "from-source"}
	if len(a.Labels) != len(wantLabels) {
		t.Errorf("labels = %v, want %v", a.Labels, wantLabels)
	}
	for k, v := range wantLabels {
		if a.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, a.Labels[k], v)
		}
	}
	if a.Annotations["summary"] != "Disk is full" || a.Annotations["runbook_url"] != "https://runbooks.example.com/disk" {
		t.Errorf("annotations = %v", a.Annotations)
	}
	if want := time.Date(2026, 3, 1, 11, 50, 0, 0, time.UTC); !a.StartedAt.Equal(want) {
		t.Errorf("started_at = %v, want %v", a.StartedAt, want)
	}
	if a.Fingerprint != ingest.Fingerprint(map[string]string{"alertname": "DiskFull", "host": "db1", "severity": "critical"}) {
		t.Errorf("fingerprint = %q, want the hash of the labels that were sent", a.Fingerprint)
	}

	// The same alert again, with its labels in another order, updates it.
	again := f.post(t, "prod", prodToken, `{"labels": {"severity": "critical", "host": "db1", "alertname": "DiskFull"}}`)
	if again.code != http.StatusOK || again.body["result"] != "updated" || again.body["alert_id"] != a.ID {
		t.Errorf("second fire = %d %v, want the same alert updated", again.code, again.body)
	}

	resolved := f.post(t, "prod", prodToken, `{"status": "resolved", "labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"}, "ends_at": "2026-03-01T12:30:00Z"}`)
	if resolved.code != http.StatusOK || resolved.body["result"] != "resolved" || resolved.body["alert_id"] != a.ID || resolved.body["status"] != "resolved" {
		t.Fatalf("resolve = %d %v", resolved.code, resolved.body)
	}
	a, err = f.store.Get(ctx, a.ID)
	if err != nil || a.Status != alert.StatusResolved || !a.ResolvedAt.Equal(time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("alert = %s resolved at %v, %v", a.Status, a.ResolvedAt, err)
	}
	events, err := f.store.Timeline(ctx, a.ID)
	if err != nil || len(events) != 2 || events[0].Type != alert.EventFired || events[1].Type != alert.EventResolved {
		t.Fatalf("timeline = %+v, %v", events, err)
	}
	for _, e := range events {
		if e.Actor.Type != alert.ActorSource || e.Actor.ID != f.ids["prod"] {
			t.Errorf("%s event actor = %+v, want the source", e.Type, e.Actor)
		}
	}

	// Resolving what is not open is accepted, so a sender's retry succeeds.
	none := f.post(t, "prod", prodToken, `{"status": "resolved", "labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"}}`)
	if none.code != http.StatusOK || none.body["result"] != "not_found" {
		t.Errorf("resolve with nothing open = %d %v", none.code, none.body)
	}

	// Firing again after the resolve opens a new alert.
	reopened := f.post(t, "prod", prodToken, diskFull)
	if reopened.code != http.StatusCreated || reopened.body["alert_id"] == a.ID {
		t.Errorf("fire after resolve = %d %v, want a new alert", reopened.code, reopened.body)
	}
}

func TestExplicitFingerprint(t *testing.T) {
	f := newFixture(t)
	first := f.post(t, "prod", prodToken, `{"fingerprint": "disk/db1", "labels": {"alertname": "DiskFull", "usage": "91%"}}`)
	second := f.post(t, "prod", prodToken, `{"fingerprint": "disk/db1", "labels": {"alertname": "DiskFull", "usage": "97%"}}`)
	if first.code != http.StatusCreated || second.code != http.StatusOK || second.body["alert_id"] != first.body["alert_id"] {
		t.Fatalf("fires = %d %v, %d %v; want one alert", first.code, first.body, second.code, second.body)
	}
	a, err := f.store.Get(context.Background(), first.body["alert_id"])
	if err != nil || a.Fingerprint != "disk/db1" || a.Labels["usage"] != "97%" {
		t.Errorf("alert = %+v, %v", a, err)
	}
	resolved := f.post(t, "prod", prodToken, `{"status": "resolved", "fingerprint": "disk/db1"}`)
	if resolved.code != http.StatusOK || resolved.body["result"] != "resolved" {
		t.Errorf("resolve by fingerprint = %d %v", resolved.code, resolved.body)
	}
}

func TestSeverityAndSourceLabels(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, source, token, body string
		want                      alert.Severity
	}{
		{"default is warning", "prod", prodToken, `{"labels": {"alertname": "A"}}`, alert.SeverityWarning},
		{"info", "prod", prodToken, `{"labels": {"alertname": "B", "severity": "info"}}`, alert.SeverityInfo},
		{"unknown value falls back", "prod", prodToken, `{"labels": {"alertname": "C", "severity": "sev1"}}`, alert.SeverityWarning},
		{"source mapping", "staging", stagingToken, `{"labels": {"alertname": "D", "priority": "page"}}`, alert.SeverityCritical},
		{"source default", "staging", stagingToken, `{"labels": {"alertname": "E", "severity": "critical"}}`, alert.SeverityInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.post(t, tt.source, tt.token, tt.body)
			if res.code != http.StatusCreated {
				t.Fatalf("fire = %d %v", res.code, res.body)
			}
			a, err := f.store.Get(context.Background(), res.body["alert_id"])
			if err != nil || a.Severity != tt.want {
				t.Errorf("severity = %s, %v; want %s", a.Severity, err, tt.want)
			}
		})
	}

	// A label from the alert wins over the source's label of the same name.
	res := f.post(t, "prod", prodToken, `{"labels": {"alertname": "F", "team": "from-alert"}}`)
	a, err := f.store.Get(context.Background(), res.body["alert_id"])
	if err != nil || a.Labels["team"] != "from-alert" || a.Labels["environment"] != "production" {
		t.Errorf("labels = %v, %v", a.Labels, err)
	}
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	body := `{"labels": {"alertname": "A"}}`
	do := func(source string, header string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/"+source, strings.NewReader(body))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("401 without a WWW-Authenticate header")
			}
			for _, secret := range []string{prodToken, stagingToken} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("the 401 body contains a token: %s", rec.Body.String())
				}
			}
		}
		return rec.Code
	}

	tests := []struct {
		name, source, header string
		want                 int
	}{
		{"own token", "prod", "Bearer " + prodToken, http.StatusCreated},
		{"scheme in lowercase", "staging", "bearer " + stagingToken, http.StatusCreated},
		{"no header", "prod", "", http.StatusUnauthorized},
		{"wrong token", "prod", "Bearer nope", http.StatusUnauthorized},
		{"empty token", "prod", "Bearer ", http.StatusUnauthorized},
		{"another source's token", "prod", "Bearer " + stagingToken, http.StatusUnauthorized},
		{"prefix of the token", "prod", "Bearer " + prodToken[:len(prodToken)-1], http.StatusUnauthorized},
		{"wrong scheme", "prod", "Basic " + prodToken, http.StatusUnauthorized},
		{"token without scheme", "prod", prodToken, http.StatusUnauthorized},
		{"unknown source", "nope", "Bearer " + prodToken, http.StatusUnauthorized},
		{"unknown source without a token", "nope", "Bearer ", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := do(tt.source, tt.header); got != tt.want {
				t.Errorf("status = %d, want %d", got, tt.want)
			}
		})
	}
	if out := f.logs.String(); strings.Contains(out, prodToken) || strings.Contains(out, stagingToken) {
		t.Errorf("the logs contain a token:\n%s", out)
	}

	// Only POST is routed.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ingest/prod", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
}

func TestInvalidPayloads(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, body, field, msg string
	}{
		{"not JSON", `labels=1`, "", "must be a JSON object"},
		{"array", `[{"labels": {"a": "b"}}]`, "", "must be a JSON object"},
		{"null", `null`, "", "must be a JSON object"},
		{"empty body", ``, "", "must be a JSON object"},
		{"no labels", `{}`, "labels", "at least one label"},
		{"empty labels", `{"labels": {}}`, "labels", "at least one label"},
		{"labels not an object", `{"labels": ["a"]}`, "labels", "object of string values"},
		{"labels null", `{"labels": null}`, "labels", "object of string values"},
		{"label value is a number", `{"labels": {"alertname": "A", "port": 5432}}`, "labels.port", "must be a string"},
		{"label value is null", `{"labels": {"alertname": null}}`, "labels.alertname", "must be a string"},
		{"empty label name", `{"labels": {"": "x"}}`, "labels", "empty name"},
		{"annotation value is an object", `{"labels": {"a": "b"}, "annotations": {"summary": {"text": "x"}}}`, "annotations.summary", "must be a string"},
		{"unknown status", `{"status": "pending", "labels": {"a": "b"}}`, "status", `must be "firing" or "resolved"`},
		{"status is a number", `{"status": 1, "labels": {"a": "b"}}`, "status", "must be a string"},
		{"empty fingerprint", `{"fingerprint": "", "labels": {"a": "b"}}`, "fingerprint", "between 1 and 256"},
		{"long fingerprint", `{"fingerprint": "` + strings.Repeat("x", 257) + `", "labels": {"a": "b"}}`, "fingerprint", "between 1 and 256"},
		{"bad starts_at", `{"labels": {"a": "b"}, "starts_at": "yesterday"}`, "starts_at", "RFC 3339"},
		{"starts_at is a number", `{"labels": {"a": "b"}, "starts_at": 1700000000}`, "starts_at", "must be a string"},
		{"ends_at while firing", `{"labels": {"a": "b"}, "ends_at": "2026-03-01T12:00:00Z"}`, "ends_at", "only allowed when status"},
		{"unknown field", `{"labels": {"a": "b"}, "label": {"c": "d"}}`, "label", "unknown field"},
		{"resolve without identity", `{"status": "resolved"}`, "labels", "labels or the fingerprint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.post(t, "prod", prodToken, tt.body)
			if res.code != http.StatusBadRequest {
				t.Fatalf("status = %d %v, want 400", res.code, res.body)
			}
			if res.body["field"] != tt.field || !strings.Contains(res.body["error"], tt.msg) {
				t.Errorf("error = %v, want field %q and a message containing %q", res.body, tt.field, tt.msg)
			}
		})
	}

	// The payload is only read once the token is accepted.
	if res := f.post(t, "prod", "wrong", `{}`); res.code != http.StatusUnauthorized {
		t.Errorf("invalid payload with a wrong token = %d, want 401", res.code)
	}

	large := `{"labels": {"alertname": "` + strings.Repeat("x", 1<<20) + `"}}`
	if res := f.post(t, "prod", prodToken, large); res.code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body = %d, want 413", res.code)
	}
}

// failingAlerts stands in for a database that is down.
type failingAlerts struct{}

func (failingAlerts) Fire(context.Context, alert.Firing) (alert.Alert, bool, error) {
	return alert.Alert{}, false, errors.New("connection refused to db.internal")
}

func (failingAlerts) ResolveFromSource(context.Context, string, string, time.Time) (alert.Alert, bool, error) {
	return alert.Alert{}, false, errors.New("connection refused to db.internal")
}

func TestStoreFailureIsNotLeaked(t *testing.T) {
	var logs bytes.Buffer
	mux := http.NewServeMux()
	src := ingest.Source{ID: "id", Source: config.Source{Name: "prod", Token: config.NewSecret(config.SecretRef{}, prodToken)}}
	ingest.New(slog.New(slog.NewJSONHandler(&logs, nil)), failingAlerts{}, []ingest.Source{src}).Register(mux)

	for _, body := range []string{`{"labels": {"a": "b"}}`, `{"status": "resolved", "labels": {"a": "b"}}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/prod", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+prodToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "db.internal") {
			t.Errorf("response = %d %s, want a 500 that does not repeat the cause", rec.Code, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "db.internal") {
		t.Errorf("the cause is not in the logs: %s", logs.String())
	}
}

func TestFingerprint(t *testing.T) {
	a := ingest.Fingerprint(map[string]string{"alertname": "A", "host": "db1"})
	if b := ingest.Fingerprint(map[string]string{"host": "db1", "alertname": "A"}); a != b {
		t.Error("the fingerprint depends on the order of the labels")
	}
	for _, other := range []map[string]string{
		{"alertname": "A", "host": "db2"},
		{"alertname": "A"},
		{"alertname": "Ahost", "": "db1"},
		{"alertnam": "eA", "host": "db1"},
	} {
		if ingest.Fingerprint(other) == a {
			t.Errorf("labels %v have the same fingerprint as different labels", other)
		}
	}
}
