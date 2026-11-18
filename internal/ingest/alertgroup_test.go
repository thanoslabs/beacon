package ingest_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
)

// counts is the response to a notification about a group of alerts.
type counts struct {
	Result   string `json:"result"`
	Received int    `json:"received"`
	Created  int    `json:"created"`
	Updated  int    `json:"updated"`
	Resolved int    `json:"resolved"`
	NotFound int    `json:"not_found"`
	Error    string `json:"error"`
	Field    string `json:"field"`
}

func (c counts) String() string {
	if c.Error != "" {
		return fmt.Sprintf("error %q at %q", c.Error, c.Field)
	}
	return fmt.Sprintf("%s: %d received, %d created, %d updated, %d resolved, %d not found", c.Result, c.Received, c.Created, c.Updated, c.Resolved, c.NotFound)
}

// postGroup sends a group notification to a source.
func (f *fixture) postGroup(t *testing.T, source, token, body string) (int, counts) {
	t.Helper()
	var c counts
	code := f.postInto(t, source, token, body, &c)
	return code, c
}

// captured reads a payload that a real sender produced.
func captured(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// alerts lists the alerts of a source as "instance status".
func (f *fixture) alerts(t *testing.T, source string) map[string]alert.Alert {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT id::text FROM alerts WHERE source_id = $1 ORDER BY started_at, id`, f.ids[source])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	out := map[string]alert.Alert{}
	for _, id := range ids {
		a, err := f.store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		out[a.Labels["instance"]+" "+string(a.Status)] = a
	}
	if len(out) != len(ids) {
		t.Fatalf("%d alerts, but only %d distinct instance and status pairs", len(ids), len(out))
	}
	return out
}

// TestAlertmanager replays what a real Alertmanager sent about one group:
// two alerts firing, one of them resolving, then the other.
func TestAlertmanager(t *testing.T) {
	f := newFixture(t)

	code, got := f.postGroup(t, "am", amToken, captured(t, "alertmanager/firing.json"))
	if want := (counts{Result: "accepted", Received: 2, Created: 2}); code != http.StatusOK || got != want {
		t.Fatalf("firing = %d %v, want 200 %v", code, got, want)
	}
	alerts := f.alerts(t, "am")
	db1, ok := alerts["db1.example.com:9100 firing"]
	if !ok || len(alerts) != 2 {
		t.Fatalf("alerts = %v", alerts)
	}
	if db1.Fingerprint != "e9939cf30653bd3f" || db1.Severity != alert.SeverityCritical {
		t.Errorf("db1: fingerprint %q, severity %s; want Alertmanager's fingerprint and critical", db1.Fingerprint, db1.Severity)
	}
	wantLabels := map[string]string{"alertname": "DiskFull", "instance": "db1.example.com:9100", "severity": "critical", "team": "platform", "environment": "production"}
	if fmt.Sprint(db1.Labels) != fmt.Sprint(wantLabels) {
		t.Errorf("db1 labels = %v, want %v", db1.Labels, wantLabels)
	}
	wantAnnotations := map[string]string{"summary": "Disk is full on db1", "description": "/var/lib/postgresql has 2% free", "runbook_url": "https://runbooks.example.com/disk-full"}
	if fmt.Sprint(db1.Annotations) != fmt.Sprint(wantAnnotations) {
		t.Errorf("db1 annotations = %v, want %v", db1.Annotations, wantAnnotations)
	}
	if want := "http://prometheus.example.com:9090/graph?g0.expr=node_filesystem_avail_bytes+%3C+1e9&g0.tab=1"; db1.Links[alert.LinkGenerator] != want || len(db1.Links) != 1 {
		t.Errorf("db1 links = %v, want the generator link %s", db1.Links, want)
	}
	if want := time.Date(2026, 11, 18, 6, 5, 22, 791032837, time.UTC); !db1.StartedAt.Equal(want.Truncate(time.Microsecond)) {
		t.Errorf("db1 started at %v, want %v", db1.StartedAt, want)
	}
	if db2 := alerts["db2.example.com:9100 firing"]; db2.Severity != alert.SeverityWarning || db2.Annotations["summary"] != "Disk is filling up on db2" {
		t.Errorf("db2 = %+v", db2)
	}

	// Alertmanager sends a group again on its repeat interval, and again
	// when a delivery timed out on its side. Nothing new is opened.
	code, got = f.postGroup(t, "am", amToken, captured(t, "alertmanager/firing.json"))
	if want := (counts{Result: "accepted", Received: 2, Updated: 2}); code != http.StatusOK || got != want {
		t.Fatalf("repeated firing = %d %v, want 200 %v", code, got, want)
	}

	// One alert resolves; the notification still lists the other as firing.
	code, got = f.postGroup(t, "am", amToken, captured(t, "alertmanager/some-resolved.json"))
	if want := (counts{Result: "accepted", Received: 2, Updated: 1, Resolved: 1}); code != http.StatusOK || got != want {
		t.Fatalf("one resolved = %d %v, want 200 %v", code, got, want)
	}
	alerts = f.alerts(t, "am")
	db2, ok := alerts["db2.example.com:9100 resolved"]
	if !ok || len(alerts) != 2 {
		t.Fatalf("alerts = %v, want db2 resolved and db1 firing", alerts)
	}
	if want := time.Date(2026, 11, 18, 6, 5, 55, 0, time.UTC); db2.ResolvedAt == nil || !db2.ResolvedAt.Equal(want) {
		t.Errorf("db2 resolved at %v, want the time Alertmanager gave, %v", db2.ResolvedAt, want)
	}
	events, err := f.store.Timeline(context.Background(), db2.ID)
	if err != nil || len(events) != 2 || events[1].Type != alert.EventResolved || events[1].Actor.Type != alert.ActorSource || events[1].Actor.ID != f.ids["am"] {
		t.Errorf("db2 timeline = %+v, %v; want fired and resolved by the source", events, err)
	}

	code, got = f.postGroup(t, "am", amToken, captured(t, "alertmanager/resolved.json"))
	if want := (counts{Result: "accepted", Received: 1, Resolved: 1}); code != http.StatusOK || got != want {
		t.Fatalf("all resolved = %d %v, want 200 %v", code, got, want)
	}
	// A resolve that is delivered twice finds nothing left to do.
	code, got = f.postGroup(t, "am", amToken, captured(t, "alertmanager/resolved.json"))
	if want := (counts{Result: "accepted", Received: 1, NotFound: 1}); code != http.StatusOK || got != want {
		t.Fatalf("resolved again = %d %v, want 200 %v", code, got, want)
	}
	if alerts = f.alerts(t, "am"); len(alerts) != 2 || alerts["db1.example.com:9100 resolved"].ID == "" {
		t.Errorf("alerts = %v, want both resolved", alerts)
	}

	// The problem comes back: a new alert, not the old one reopened.
	code, got = f.postGroup(t, "am", amToken, captured(t, "alertmanager/firing.json"))
	if want := (counts{Result: "accepted", Received: 2, Created: 2}); code != http.StatusOK || got != want {
		t.Fatalf("firing again = %d %v, want 200 %v", code, got, want)
	}
	if strings.Contains(f.logs.String(), `"level":"WARN"`) {
		t.Errorf("a payload of the version Beacon reads was warned about:\n%s", f.logs.String())
	}
}

// amAlert builds a notification with one alert from its JSON.
func amAlert(alert string) string {
	return `{"version": "4", "status": "firing", "alerts": [` + alert + `]}`
}

func TestAlertmanagerDetails(t *testing.T) {
	f := newFixture(t)

	t.Run("an alert without a fingerprint is identified by its labels", func(t *testing.T) {
		// Alertmanager added the field in 0.17.
		for i, want := range []counts{{Result: "accepted", Received: 1, Created: 1}, {Result: "accepted", Received: 1, Updated: 1}} {
			code, got := f.postGroup(t, "am", amToken, amAlert(`{"status": "firing", "labels": {"alertname": "NoFingerprint", "instance": "a"}}`))
			if code != http.StatusOK || got != want {
				t.Errorf("delivery %d = %d %v, want %v", i+1, code, got, want)
			}
		}
		code, got := f.postGroup(t, "am", amToken, amAlert(`{"status": "resolved", "labels": {"instance": "a", "alertname": "NoFingerprint"}}`))
		if code != http.StatusOK || got.Resolved != 1 {
			t.Errorf("resolve by labels = %d %v", code, got)
		}
	})

	t.Run("the end of a firing alert is not a resolution", func(t *testing.T) {
		// Prometheus gives a firing alert the time at which Alertmanager
		// should give up on it, and old Alertmanagers passed that on.
		code, got := f.postGroup(t, "am", amToken, amAlert(`{"status": "firing", "labels": {"alertname": "FutureEnd", "instance": "b"}, "endsAt": "2099-01-01T00:00:00Z"}`))
		if code != http.StatusOK || got.Created != 1 {
			t.Fatalf("firing = %d %v", code, got)
		}
		if a := f.alerts(t, "am")["b firing"]; a.ID == "" || a.ResolvedAt != nil {
			t.Errorf("alert = %+v, want it firing", a)
		}
	})

	t.Run("a link that is not a web address is dropped", func(t *testing.T) {
		for instance, link := range map[string]string{"c1": "javascript:alert(1)", "c2": "", "c3": "https://prometheus.example.com/" + strings.Repeat("x", 2048)} {
			body := amAlert(fmt.Sprintf(`{"status": "firing", "labels": {"alertname": "Link", "instance": %q}, "generatorURL": %q}`, instance, link))
			if code, got := f.postGroup(t, "am", amToken, body); code != http.StatusOK || got.Created != 1 {
				t.Fatalf("firing = %d %v", code, got)
			}
			if a := f.alerts(t, "am")[instance+" firing"]; a.ID == "" || len(a.Links) != 0 {
				t.Errorf("links of %s = %v, want none", instance, a.Links)
			}
		}
	})

	t.Run("an empty notification is accepted", func(t *testing.T) {
		if code, got := f.postGroup(t, "am", amToken, `{"version": "4", "alerts": []}`); code != http.StatusOK || got != (counts{Result: "accepted"}) {
			t.Errorf("empty = %d %v", code, got)
		}
	})

	t.Run("another version and truncated alerts are read and warned about", func(t *testing.T) {
		f.logs.Reset()
		body := `{"version": "5", "truncatedAlerts": 3, "alerts": [{"status": "firing", "labels": {"alertname": "Newer", "instance": "d"}, "someNewField": {"a": 1}}], "anotherNewField": true}`
		if code, got := f.postGroup(t, "am", amToken, body); code != http.StatusOK || got.Created != 1 {
			t.Fatalf("version 5 = %d %v, want it read", code, got)
		}
		for _, want := range []string{"another version", `"version":"5"`, "left alerts out", `"truncated":3`} {
			if !strings.Contains(f.logs.String(), want) {
				t.Errorf("logs do not contain %q:\n%s", want, f.logs.String())
			}
		}
	})

	t.Run("a generic payload sent to an Alertmanager source is refused", func(t *testing.T) {
		code, got := f.postGroup(t, "am", amToken, `{"labels": {"alertname": "A"}}`)
		if code != http.StatusBadRequest || got.Field != "alerts" {
			t.Errorf("generic payload = %d %v, want 400 naming alerts", code, got)
		}
	})
}

func TestInvalidAlertmanagerPayloads(t *testing.T) {
	f := newFixture(t)
	ok := `{"status": "firing", "labels": {"alertname": "A"}}`
	tests := []struct {
		name, body, field, msg string
	}{
		{"not JSON", `alerts=1`, "", "must be a JSON object"},
		{"array", `[]`, "", "must be a JSON object"},
		{"no alerts", `{"version": "4"}`, "alerts", "is required"},
		{"alerts null", `{"alerts": null}`, "alerts", "must be a list"},
		{"alerts not a list", `{"alerts": {"a": 1}}`, "alerts", "must be a list"},
		{"version not a string", `{"version": 4, "alerts": []}`, "version", "must be a string"},
		{"truncatedAlerts not a number", `{"truncatedAlerts": "x", "alerts": []}`, "truncatedAlerts", "must be a number"},
		{"alert not an object", `{"alerts": ["a"]}`, "alerts[0]", "must be an object"},
		{"no status", `{"alerts": [` + ok + `, {"labels": {"a": "b"}}]}`, "alerts[1].status", `must be "firing" or "resolved"`},
		{"unknown status", `{"alerts": [{"status": "pending", "labels": {"a": "b"}}]}`, "alerts[0].status", `must be "firing" or "resolved"`},
		{"no labels", `{"alerts": [{"status": "firing"}]}`, "alerts[0].labels", "at least one label"},
		{"resolved with neither labels nor fingerprint", `{"alerts": [{"status": "resolved"}]}`, "alerts[0].labels", "at least one label"},
		{"label value is a number", `{"alerts": [{"status": "firing", "labels": {"alertname": "A", "port": 5432}}]}`, "alerts[0].labels.port", "must be a string"},
		{"annotations not an object", `{"alerts": [{"status": "firing", "labels": {"a": "b"}, "annotations": "x"}]}`, "alerts[0].annotations", "object of string values"},
		{"bad startsAt", `{"alerts": [{"status": "firing", "labels": {"a": "b"}, "startsAt": "yesterday"}]}`, "alerts[0].startsAt", "RFC 3339"},
		{"bad endsAt", `{"alerts": [{"status": "resolved", "labels": {"a": "b"}, "endsAt": 5}]}`, "alerts[0].endsAt", "must be a string"},
		{"fingerprint not a string", `{"alerts": [{"status": "firing", "labels": {"a": "b"}, "fingerprint": 5}]}`, "alerts[0].fingerprint", "must be a string"},
		{"long fingerprint", `{"alerts": [{"status": "firing", "labels": {"a": "b"}, "fingerprint": "` + strings.Repeat("x", 257) + `"}]}`, "alerts[0].fingerprint", "at most 256"},
		{"generatorURL not a string", `{"alerts": [{"status": "firing", "labels": {"a": "b"}, "generatorURL": 5}]}`, "alerts[0].generatorURL", "must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, got := f.postGroup(t, "am", amToken, tt.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d %v, want 400", code, got)
			}
			if got.Field != tt.field || !strings.Contains(got.Error, tt.msg) {
				t.Errorf("error = %v, want field %q and a message containing %q", got, tt.field, tt.msg)
			}
		})
	}
	// An invalid alert refuses the whole notification: nothing is stored.
	if alerts := f.alerts(t, "am"); len(alerts) != 0 {
		t.Errorf("alerts were stored from refused notifications: %v", alerts)
	}

	// A group may be larger than one alert from a webhook source, up to 4 MiB.
	big := `{"alerts": [{"status": "firing", "labels": {"alertname": "Big"}, "annotations": {"description": "` + strings.Repeat("x", 2<<20) + `"}}]}`
	if code, got := f.postGroup(t, "am", amToken, big); code != http.StatusOK || got.Created != 1 {
		t.Errorf("2 MiB group = %d %v, want it accepted", code, got)
	}
	huge := `{"alerts": [{"status": "firing", "labels": {"alertname": "` + strings.Repeat("x", 4<<20) + `"}}]}`
	if code, _ := f.postGroup(t, "am", amToken, huge); code != http.StatusRequestEntityTooLarge {
		t.Errorf("group over 4 MiB = %d, want 413", code)
	}
}

// TestAlertmanagerStoreFailure checks that a notification which could not be
// stored in full is refused, so that Alertmanager sends it again.
func TestAlertmanagerStoreFailure(t *testing.T) {
	f := newFixture(t)
	f.pool.Close()
	code, got := f.postGroup(t, "am", amToken, captured(t, "alertmanager/firing.json"))
	if code != http.StatusInternalServerError || got.Error != "internal error" {
		t.Errorf("with the database gone = %d %v, want 500", code, got)
	}
}
