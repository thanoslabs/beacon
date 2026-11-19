package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/api"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// fixture is the alerts API over a real database, with a clock that tests
// move.
type fixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	alerts *alert.Store
	mux    *http.ServeMux
	now    time.Time
	// The rows alerts refer to.
	prod, staging, platform, rule string
	// ada is a signed-in user, and ci an API token.
	ada, ci access.Principal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: postgrestest.NewMigrated(t), now: t0, mux: http.NewServeMux()}
	f.alerts = alert.NewStore(f.pool)
	f.alerts.SetClock(func() time.Time { return f.now })
	f.prod = provisiontest.Row(t, f.pool, provision.Sources, "prod")
	f.staging = provisiontest.Row(t, f.pool, provision.Sources, "staging")
	f.platform = provisiontest.Row(t, f.pool, provision.Teams, "platform")
	f.rule = provisiontest.Row(t, f.pool, provision.RoutingRules, "platform-critical")
	h := api.NewAlerts(slog.New(slog.DiscardHandler), f.alerts)
	h.SetClock(func() time.Time { return f.now })
	for _, route := range h.Routes() {
		if route.Level != access.Member {
			t.Errorf("%s is open to %s, want members", route.Pattern, route.Level)
		}
		f.mux.Handle(route.Pattern, route.Handler)
	}
	f.ada = access.Principal{Email: "ada@example.com", Name: "Ada Lovelace", Role: access.RoleMember}
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO users (email, display_name) VALUES ($1, $2) RETURNING id::text`, f.ada.Email, f.ada.Name).Scan(&f.ada.UserID); err != nil {
		t.Fatal(err)
	}
	f.ci = access.Principal{TokenID: "0b9c2f4e-51d6-4a8e-9f0a-3c2b1d4e5f60", Name: "ci", Role: access.RoleMember}
	return f
}

// fire opens an alert a minute after the last one and returns it.
func (f *fixture) fire(name, host string, severity alert.Severity, change func(*alert.Firing)) alert.Alert {
	f.t.Helper()
	f.now = f.now.Add(time.Minute)
	firing := alert.Firing{
		SourceID: f.prod, Fingerprint: name + "/" + host, Severity: severity,
		Labels:      map[string]string{"alertname": name, "host": host},
		Annotations: map[string]string{"summary": name + " on " + host},
		Group:       alert.Grouping{Key: name, Labels: map[string]string{"alertname": name}},
	}
	if change != nil {
		change(&firing)
	}
	a, _, err := f.alerts.Fire(context.Background(), firing)
	if err != nil {
		f.t.Fatalf("Fire: %v", err)
	}
	return a
}

// call sends a request as a principal and returns the status and the body.
func (f *fixture) call(p access.Principal, method, path, body string) (int, map[string]any) {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req.WithContext(access.WithPrincipal(req.Context(), p)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatalf("%s %s answered %d %q, not JSON", method, path, rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		f.t.Errorf("%s %s: content type %q, cache control %q", method, path, ct, rec.Header().Get("Cache-Control"))
	}
	return rec.Code, out
}

// names lists the alerts of a list answer as "alertname/host status".
func names(body map[string]any) string {
	var out []string
	list, _ := body["alerts"].([]any)
	for _, item := range list {
		a := item.(map[string]any)
		labels := a["labels"].(map[string]any)
		out = append(out, fmt.Sprintf("%v/%v %v", labels["alertname"], labels["host"], a["status"]))
	}
	return strings.Join(out, ", ")
}

func TestListAlerts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	routed := func(firing *alert.Firing) {
		firing.TeamID, firing.RoutingRuleID, firing.Service = f.platform, f.rule, "postgres"
	}
	disk1 := f.fire("DiskFull", "db1", alert.SeverityCritical, routed)
	disk2 := f.fire("DiskFull", "db2", alert.SeverityCritical, routed)
	f.fire("HighLoad", "web1", alert.SeverityWarning, nil)
	f.fire("CertExpires", "lb1", alert.SeverityInfo, func(firing *alert.Firing) { firing.SourceID = f.staging })
	if _, err := f.alerts.Apply(ctx, disk1.ID, alert.ActionAcknowledge, alert.User(f.ada.UserID, "Ada"), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.alerts.ResolveFromSource(ctx, f.prod, "HighLoad/web1", time.Time{}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		query, want string
	}{
		// Newest first.
		{"", "CertExpires/lb1 firing, HighLoad/web1 resolved, DiskFull/db2 firing, DiskFull/db1 acknowledged"},
		{"status=firing", "CertExpires/lb1 firing, DiskFull/db2 firing"},
		{"status=open", "CertExpires/lb1 firing, DiskFull/db2 firing, DiskFull/db1 acknowledged"},
		{"status=acknowledged&status=resolved", "HighLoad/web1 resolved, DiskFull/db1 acknowledged"},
		{"status=acknowledged,resolved", "HighLoad/web1 resolved, DiskFull/db1 acknowledged"},
		{"status=snoozed", ""},
		{"severity=critical", "DiskFull/db2 firing, DiskFull/db1 acknowledged"},
		{"severity=warning,info", "CertExpires/lb1 firing, HighLoad/web1 resolved"},
		{"team=platform", "DiskFull/db2 firing, DiskFull/db1 acknowledged"},
		{"team=nobody", ""},
		{"service=postgres&status=firing", "DiskFull/db2 firing"},
		{"source=staging", "CertExpires/lb1 firing"},
		{"group=" + disk2.GroupID, "DiskFull/db2 firing, DiskFull/db1 acknowledged"},
		{"group=11111111-2222-4333-8444-555555555555", ""},
	}
	for _, tt := range tests {
		code, body := f.call(f.ada, http.MethodGet, "/api/v1/alerts?"+tt.query, "")
		if code != http.StatusOK || names(body) != tt.want {
			t.Errorf("?%s = %d %s\nwant %s", tt.query, code, names(body), tt.want)
		}
		if _, more := body["next_cursor"]; more {
			t.Errorf("?%s: a list that is complete has a next_cursor", tt.query)
		}
		// An empty list is a list.
		if list, ok := body["alerts"].([]any); !ok || list == nil {
			t.Errorf("?%s: alerts = %v, want an array", tt.query, body["alerts"])
		}
	}

	// What is wrong with a request is said, with the part at fault.
	for query, field := range map[string]string{
		"status=broken": "status", "severity=high": "severity", "group=not-an-id": "group", "limit=0": "limit", "limit=201": "limit",
		"limit=ten": "limit", "cursor=garbage": "cursor", "cursor=" + api.Cursor(map[string]any{"t": "2026-03-01T12:00:00Z", "id": "x"}): "cursor",
		"state=firing": "state", "cursor=" + strings.Repeat("A", 600): "cursor",
	} {
		code, body := f.call(f.ada, http.MethodGet, "/api/v1/alerts?"+query, "")
		if code != http.StatusBadRequest || body["field"] != field || body["error"] == "" {
			t.Errorf("?%.40s = %d %v, want 400 naming %s", query, code, body, field)
		}
	}
}

// TestAlertsArePaged: a list is read a page at a time without a gap or a
// repeat, also while alerts arrive, and also among alerts that began at the
// same moment.
func TestAlertsArePaged(t *testing.T) {
	f := newFixture(t)
	for i := range 7 {
		f.fire("Paged", fmt.Sprintf("host%d", i), alert.SeverityWarning, func(firing *alert.Firing) {
			// Three of them began together.
			if i >= 2 && i <= 4 {
				firing.StartsAt = t0.Add(30 * time.Second)
			}
		})
	}
	seen := map[string]int{}
	var order, keys []string
	cursor, pages := "", 0
	for {
		path := "/api/v1/alerts?limit=3"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		code, body := f.call(f.ada, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("page %d = %d %v", pages, code, body)
		}
		pages++
		for _, item := range body["alerts"].([]any) {
			host := item.(map[string]any)["labels"].(map[string]any)["host"].(string)
			seen[host]++
			order = append(order, host)
			keys = append(keys, item.(map[string]any)["started_at"].(string)+" "+item.(map[string]any)["id"].(string))
		}
		// An alert that arrives while the list is being read is newer than
		// the page that was read, and does not shift the ones that follow.
		if pages == 1 {
			f.fire("Paged", "late", alert.SeverityWarning, nil)
		}
		next, more := body["next_cursor"].(string)
		if !more {
			break
		}
		if n := len(body["alerts"].([]any)); n != 3 {
			t.Errorf("page %d has %d alerts and a next_cursor, want a full page", pages, n)
		}
		cursor = next
	}
	if pages != 3 || len(seen) != 7 {
		t.Errorf("%d pages with %d alerts, want 3 pages with the 7 alerts that existed: %v", pages, len(seen), order)
	}
	for host, n := range seen {
		if n != 1 || host == "late" {
			t.Errorf("%s was listed %d times", host, n)
		}
	}
	// The order is total: by time, and by identifier among alerts that
	// began together. Without the second, a page could end in the middle of
	// such alerts and the next one begin anywhere among them.
	if !slices.IsSortedFunc(keys, func(a, b string) int { return strings.Compare(b, a) }) {
		t.Errorf("the alerts are not in descending order of time and identifier:\n%s", strings.Join(keys, "\n"))
	}
	if got := strings.Join(order[:2], ","); got != "host6,host5" {
		t.Errorf("the list begins with %s, want the newest", got)
	}
}

func TestGetAlert(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull", "db1", alert.SeverityCritical, func(firing *alert.Firing) {
		firing.TeamID, firing.RoutingRuleID, firing.Service = f.platform, f.rule, "postgres"
		firing.Links = map[string]string{alert.LinkGenerator: "https://prometheus.example.com/graph"}
	})
	code, body := f.call(f.ada, http.MethodGet, "/api/v1/alerts/"+a.ID, "")
	got, _ := json.Marshal(body)
	want := `{"acknowledged_at":null,"actions":["acknowledge","snooze","resolve"],"annotations":{"summary":"DiskFull on db1"},"fingerprint":"DiskFull/db1",` +
		`"group_id":"` + a.GroupID + `","id":"` + a.ID + `","labels":{"alertname":"DiskFull","host":"db1"},"last_received_at":"2026-03-01T12:01:00Z",` +
		`"links":{"generator":"https://prometheus.example.com/graph"},"resolved_at":null,"routing_rule":"platform-critical","service":"postgres","severity":"critical",` +
		`"snoozed_until":null,"source":"prod","started_at":"2026-03-01T12:01:00Z","status":"firing","team":"platform"}`
	if code != http.StatusOK || string(got) != want {
		t.Errorf("GET = %d\n%s\nwant\n%s", code, got, want)
	}
	// An alert that routing assigned nothing has nulls, not empty strings.
	bare := f.fire("HighLoad", "web1", alert.SeverityWarning, nil)
	if _, body := f.call(f.ada, http.MethodGet, "/api/v1/alerts/"+bare.ID, ""); body["team"] != nil || body["service"] != nil || body["routing_rule"] != nil || body["links"] == nil {
		t.Errorf("an alert without a team = %v", body)
	}
	for _, id := range []string{"11111111-2222-4333-8444-555555555555", "not-an-id", "1"} {
		for _, path := range []string{"/api/v1/alerts/" + id, "/api/v1/alerts/" + id + "/timeline", "/api/v1/alert-groups/" + id} {
			if code, body := f.call(f.ada, http.MethodGet, path, ""); code != http.StatusNotFound || !strings.HasPrefix(fmt.Sprint(body["error"]), "no such alert") {
				t.Errorf("GET %s = %d %v, want 404", path, code, body)
			}
		}
	}
}

// TestActOnAnAlert: acknowledge, snooze and resolve through the API, by a
// person and by a token, each recorded on the timeline as theirs.
func TestActOnAnAlert(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull", "db1", alert.SeverityCritical, nil)
	path := "/api/v1/alerts/" + a.ID

	f.now = f.now.Add(time.Minute)
	code, body := f.call(f.ada, http.MethodPost, path+"/acknowledge", "")
	if code != http.StatusOK || body["status"] != "acknowledged" || body["acknowledged_at"] != "2026-03-01T12:02:00Z" ||
		fmt.Sprint(body["actions"]) != "[snooze resolve]" {
		t.Fatalf("acknowledge = %d %v", code, body)
	}
	// What the state does not allow is a conflict, and changes nothing.
	if code, body := f.call(f.ada, http.MethodPost, path+"/acknowledge", ""); code != http.StatusConflict || body["error"] != "cannot acknowledge an alert that is acknowledged" {
		t.Errorf("second acknowledge = %d %v, want 409", code, body)
	}

	// A snooze needs to know until when.
	for body, field := range map[string]string{
		``: "", `{}`: "until", `not json`: "", `{"for": "1h"}`: "", `{"duration": "1h", "until": "2026-03-01T13:00:00Z"}`: "duration",
		`{"duration": "soon"}`: "duration", `{"duration": "-1h"}`: "duration", `{"duration": "31d"}`: "duration",
		`{"until": "2026-03-01T12:00:00Z"}`: "until", `{"until": "2027-03-01T12:00:00Z"}`: "until", `{"until": "tomorrow"}`: "",
		`{"duration": "` + strings.Repeat("1", 5000) + `h"}`: "",
	} {
		code, answer := f.call(f.ada, http.MethodPost, path+"/snooze", body)
		wantCode := http.StatusBadRequest
		if len(body) > 4096 {
			wantCode = http.StatusRequestEntityTooLarge
		}
		if code != wantCode || fmt.Sprint(answer["field"]) != fmt.Sprint(nilIfEmpty(field)) {
			t.Errorf("snooze with %.50q = %d %v, want %d naming %q", body, code, answer, wantCode, field)
		}
	}
	// By a token, for a duration.
	code, body = f.call(f.ci, http.MethodPost, path+"/snooze", `{"duration": "90m"}`)
	if code != http.StatusOK || body["status"] != "snoozed" || body["snoozed_until"] != "2026-03-01T13:32:00Z" || fmt.Sprint(body["actions"]) != "[acknowledge resolve]" {
		t.Fatalf("snooze = %d %v", code, body)
	}
	if code, body := f.call(f.ada, http.MethodPost, path+"/snooze", `{"until": "2026-03-01T14:00:00+01:00"}`); code != http.StatusConflict {
		t.Errorf("snooze of a snoozed alert = %d %v, want 409", code, body)
	}

	// Resolving by hand takes no body, and says so to a caller who sends one.
	if code, body := f.call(f.ada, http.MethodPost, path+"/resolve", `{"reason": "fixed"}`); code != http.StatusBadRequest || body["error"] != "this request takes no body" {
		t.Errorf("resolve with a body = %d %v", code, body)
	}
	f.now = f.now.Add(time.Minute)
	code, body = f.call(f.ada, http.MethodPost, path+"/resolve", "{}")
	if code != http.StatusOK || body["status"] != "resolved" || body["resolved_at"] != "2026-03-01T12:03:00Z" || body["snoozed_until"] != nil || fmt.Sprint(body["actions"]) != "[]" {
		t.Fatalf("resolve = %d %v", code, body)
	}
	for _, action := range []string{"acknowledge", "snooze", "resolve"} {
		payload := ""
		if action == "snooze" {
			payload = `{"duration": "1h"}`
		}
		if code, body := f.call(f.ada, http.MethodPost, path+"/"+action, payload); code != http.StatusConflict || body["error"] != "cannot "+action+" an alert that is resolved" {
			t.Errorf("%s of a resolved alert = %d %v, want 409", action, code, body)
		}
		for _, id := range []string{"11111111-2222-4333-8444-555555555555", "not-an-id"} {
			if code, _ := f.call(f.ada, http.MethodPost, "/api/v1/alerts/"+id+"/"+action, payload); code != http.StatusNotFound {
				t.Errorf("%s of %s = %d, want 404", action, id, code)
			}
		}
	}

	// The timeline tells the story, with who did what.
	code, body = f.call(f.ada, http.MethodGet, path+"/timeline", "")
	if code != http.StatusOK {
		t.Fatalf("timeline = %d %v", code, body)
	}
	var story []string
	for _, item := range body["events"].([]any) {
		e := item.(map[string]any)
		actor := e["actor"].(map[string]any)
		story = append(story, fmt.Sprintf("%v by %v %v %v", e["type"], actor["type"], actor["name"], actor["id"]))
		if e["alert_id"] != a.ID || e["group_id"] != a.GroupID || e["data"] == nil {
			t.Errorf("event = %v", e)
		}
		if _, leaked := e["data"].(map[string]any)["actor_name"]; leaked {
			t.Errorf("the data of an event repeats the actor's name: %v", e)
		}
	}
	want := []string{
		"fired by source <nil> " + f.prod,
		"acknowledged by user Ada Lovelace " + f.ada.UserID,
		"snoozed by api_token ci " + f.ci.TokenID,
		"resolved by user Ada Lovelace " + f.ada.UserID,
	}
	if strings.Join(story, "\n") != strings.Join(want, "\n") {
		t.Errorf("timeline:\n%s\nwant:\n%s", strings.Join(story, "\n"), strings.Join(want, "\n"))
	}
	if events := body["events"].([]any); events[2].(map[string]any)["data"].(map[string]any)["until"] != "2026-03-01T13:32:00Z" || events[1].(map[string]any)["occurred_at"] != "2026-03-01T12:02:00Z" {
		t.Errorf("events = %v", events)
	}

	// A timeline is paged like a list, oldest first.
	var types []string
	cursor := ""
	for range 10 {
		q := path + "/timeline?limit=3"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		_, page := f.call(f.ada, http.MethodGet, q, "")
		for _, item := range page["events"].([]any) {
			types = append(types, item.(map[string]any)["type"].(string))
		}
		next, more := page["next_cursor"].(string)
		if !more {
			break
		}
		cursor = next
	}
	if got := strings.Join(types, ","); got != "fired,acknowledged,snoozed,resolved" {
		t.Errorf("the timeline read in pages = %s", got)
	}
	if code, body := f.call(f.ada, http.MethodGet, path+"/timeline?cursor=nonsense", ""); code != http.StatusBadRequest || body["field"] != "cursor" {
		t.Errorf("a timeline with a cursor that is not one = %d %v", code, body)
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestActOnAGroup: an action on a group is taken by every alert of the
// group that allows it.
func TestActOnAGroup(t *testing.T) {
	f := newFixture(t)
	policy := provisiontest.Row(t, f.pool, provision.EscalationPolicies, "default")
	grouped := func(firing *alert.Firing) {
		firing.Group.TeamID, firing.Group.RoutingRuleID, firing.Group.EscalationPolicyID = f.platform, f.rule, policy
	}
	a := f.fire("DiskFull", "db1", alert.SeverityCritical, grouped)
	b := f.fire("DiskFull", "db2", alert.SeverityWarning, grouped)
	other := f.fire("HighLoad", "web1", alert.SeverityWarning, nil)
	if a.GroupID != b.GroupID || a.GroupID == other.GroupID {
		t.Fatalf("groups: %s %s %s", a.GroupID, b.GroupID, other.GroupID)
	}
	path := "/api/v1/alert-groups/" + a.GroupID

	code, body := f.call(f.ada, http.MethodGet, path, "")
	if code != http.StatusOK || body["status"] != "open" || body["team"] != "platform" || body["routing_rule"] != "platform-critical" || body["escalation_policy"] != "default" ||
		fmt.Sprint(body["labels"]) != "map[alertname:DiskFull]" || names(body) != "DiskFull/db1 firing, DiskFull/db2 firing" ||
		fmt.Sprint(body["actions"]) != "[acknowledge snooze resolve]" || body["created_at"] != "2026-03-01T12:01:00Z" || body["resolved_at"] != nil {
		t.Fatalf("group = %d %v", code, body)
	}

	// One alert is acknowledged already; the group's acknowledgement takes
	// the other, and says nothing is wrong.
	if code, _ := f.call(f.ada, http.MethodPost, "/api/v1/alerts/"+a.ID+"/acknowledge", ""); code != http.StatusOK {
		t.Fatal("acknowledge one alert")
	}
	code, body = f.call(f.ci, http.MethodPost, path+"/acknowledge", "")
	if code != http.StatusOK || names(body) != "DiskFull/db1 acknowledged, DiskFull/db2 acknowledged" || fmt.Sprint(body["actions"]) != "[snooze resolve]" {
		t.Fatalf("acknowledge the group = %d %v", code, body)
	}
	if code, body := f.call(f.ada, http.MethodPost, path+"/acknowledge", ""); code != http.StatusConflict || body["error"] != "cannot acknowledge an alert group that is acknowledged" {
		t.Errorf("acknowledge again = %d %v, want 409", code, body)
	}
	if code, body := f.call(f.ada, http.MethodPost, path+"/snooze", `{"duration": "2h"}`); code != http.StatusOK || names(body) != "DiskFull/db1 snoozed, DiskFull/db2 snoozed" {
		t.Errorf("snooze the group = %d %v", code, body)
	}
	code, body = f.call(f.ada, http.MethodPost, path+"/resolve", "")
	if code != http.StatusOK || body["status"] != "resolved" || body["resolved_at"] == nil || names(body) != "DiskFull/db1 resolved, DiskFull/db2 resolved" || fmt.Sprint(body["actions"]) != "[]" {
		t.Errorf("resolve the group = %d %v", code, body)
	}
	// The other group was left alone.
	if _, body := f.call(f.ada, http.MethodGet, "/api/v1/alerts/"+other.ID, ""); body["status"] != "firing" {
		t.Errorf("an alert of another group = %v", body)
	}
	for _, id := range []string{"11111111-2222-4333-8444-555555555555", "not-an-id"} {
		if code, _ := f.call(f.ada, http.MethodPost, "/api/v1/alert-groups/"+id+"/resolve", ""); code != http.StatusNotFound {
			t.Errorf("resolve of the group %s = %d, want 404", id, code)
		}
	}
}
