package ingest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentDeliveries sends the same notification to each type of source
// from many clients at once, as a sender that retries after a timeout does,
// or the members of an Alertmanager cluster. Every request succeeds and each
// alert exists once.
func TestConcurrentDeliveries(t *testing.T) {
	f := newFixture(t)
	const clients = 12
	tests := []struct {
		source, token, body string
		alerts              int
	}{
		{"prod", prodToken, diskFull, 1},
		{"am", amToken, captured(t, "alertmanager/firing.json"), 2},
		{"grafana", grafanaToken, captured(t, "grafana/firing.json"), 1},
	}
	var wg sync.WaitGroup
	for _, tt := range tests {
		for range clients {
			wg.Go(func() {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/"+tt.source, strings.NewReader(tt.body))
				req.Header.Set("Authorization", "Bearer "+tt.token)
				rec := httptest.NewRecorder()
				f.mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
					t.Errorf("%s: %d %s", tt.source, rec.Code, rec.Body.String())
				}
			})
		}
	}
	wg.Wait()

	for _, tt := range tests {
		var alerts, fingerprints, fired int
		err := f.pool.QueryRow(context.Background(), `SELECT count(*), count(DISTINCT fingerprint),
				(SELECT count(*) FROM alert_events e JOIN alerts x ON x.id = e.alert_id WHERE x.source_id = $1 AND e.type = 'fired')
			FROM alerts WHERE source_id = $1`, f.ids[tt.source]).Scan(&alerts, &fingerprints, &fired)
		if err != nil {
			t.Fatal(err)
		}
		if alerts != tt.alerts || fingerprints != tt.alerts || fired != tt.alerts {
			t.Errorf("%s: %d alerts with %d fingerprints and %d fired events after %d deliveries, want %d of each",
				tt.source, alerts, fingerprints, fired, clients, tt.alerts)
		}
	}
}
