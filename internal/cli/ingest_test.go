package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// postJSON sends body to url with an optional bearer token and returns the
// status and the decoded JSON object.
func postJSON(t *testing.T, url, token, body string) (int, map[string]string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("POST %s = %d %q, not a JSON object", url, resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

func TestServeIngest(t *testing.T) {
	const token = "source-token-marker-7c2e41d0"
	dbURL := postgrestest.NewURL(t)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL": dbURL,
		"BEACON_SOURCE_NAME":  "prod",
		"BEACON_SOURCE_TOKEN": token,
	})
	url := srv.baseURL + "/api/v1/ingest/prod"

	code, body := postJSON(t, url, token, `{"labels": {"alertname": "DiskFull", "severity": "critical"}}`)
	if code != http.StatusCreated || body["result"] != "created" || body["alert_id"] == "" {
		t.Fatalf("fire = %d %v", code, body)
	}
	if code, body := postJSON(t, url, "wrong-token", `{"labels": {"alertname": "DiskFull"}}`); code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d %v, want 401", code, body)
	}
	if code, body := postJSON(t, srv.baseURL+"/api/v1/ingest/default", token, `{"labels": {"alertname": "DiskFull"}}`); code != http.StatusUnauthorized {
		t.Errorf("another source name = %d %v, want 401", code, body)
	}
	if code, body := postJSON(t, url, token, `{"labels": {"alertname": 5}}`); code != http.StatusBadRequest || body["field"] != "labels.alertname" {
		t.Errorf("invalid payload = %d %v, want 400 naming labels.alertname", code, body)
	}
	if code, body := postJSON(t, url, token, `{"status": "resolved", "labels": {"alertname": "DiskFull", "severity": "critical"}}`); code != http.StatusOK || body["result"] != "resolved" {
		t.Errorf("resolve = %d %v", code, body)
	}

	pool, err := postgres.Open(context.Background(), postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()
	var status, spec string
	err = pool.QueryRow(context.Background(), `SELECT a.status, s.spec::text FROM alerts a JOIN sources s ON s.id = a.source_id WHERE s.name = 'prod'`).Scan(&status, &spec)
	if err != nil || status != "resolved" {
		t.Errorf("stored alert status = %q, %v; want resolved", status, err)
	}
	assertNoSecrets(t, "the stored source", spec, token)
	if !strings.Contains(spec, "BEACON_SOURCE_TOKEN") {
		t.Errorf("the stored source does not name where its token comes from: %s", spec)
	}

	if code := srv.stop(t); code != exitOK {
		t.Errorf("exit code = %d", code)
	}
	logs := srv.logs.String()
	assertNoSecrets(t, "the logs", logs, token, "wrong-token")
	for _, want := range []string{"source configured", "alert received", `"result":"created"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs do not contain %q:\n%s", want, logs)
		}
	}
}

func TestServeWithoutASourceRefusesIngest(t *testing.T) {
	srv := startServe(t, map[string]string{"BEACON_DATABASE_URL": postgrestest.NewURL(t)})
	code, _ := postJSON(t, srv.baseURL+"/api/v1/ingest/default", "", `{"labels": {"alertname": "A"}}`)
	if code != http.StatusUnauthorized {
		t.Errorf("POST without a configured source = %d, want 401", code)
	}
	if code, _ := postJSON(t, srv.baseURL+"/api/v1/ingest/default", "anything", `{"labels": {"alertname": "A"}}`); code != http.StatusUnauthorized {
		t.Errorf("POST with a token and no configured source = %d, want 401", code)
	}
	if !strings.Contains(srv.logs.String(), "no source is configured") {
		t.Errorf("no warning about the missing source:\n%s", srv.logs.String())
	}
}
