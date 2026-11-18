package silence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/silence"
)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *silence.Store
	clock *clock
	logs  *bytes.Buffer
	mux   *http.ServeMux
	// ada is a member who signed in; ci is an API token.
	ada, ci access.Principal
	// as is who the next request is made by.
	as access.Principal
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := postgrestest.NewMigrated(t)
	f := &fixture{t: t, pool: pool, store: silence.NewStore(pool), clock: &clock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}, mux: http.NewServeMux()}
	f.store.SetClock(f.clock.Now)
	ctx := context.Background()
	f.ada = access.Principal{Email: "ada@example.com", Name: "Ada", Role: access.RoleMember}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, display_name, role) VALUES ($1, $2, 'member') RETURNING id::text`, f.ada.Email, f.ada.Name).Scan(&f.ada.UserID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	f.ci = access.Principal{Name: "ci", Role: access.RoleMember}
	if err := pool.QueryRow(ctx, `INSERT INTO api_tokens (name, token_hash, role) VALUES ('ci', 'hash', 'member') RETURNING id::text`).Scan(&f.ci.TokenID); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	f.as = f.ada
	// The guard of the real server puts the principal into the context; here
	// the test does.
	for _, route := range silence.NewHandler(slog.New(slog.NewJSONHandler(f.logs, nil)), f.store).Routes() {
		if route.Level != access.Member {
			t.Errorf("%s is open to %s, want members", route.Pattern, route.Level)
		}
		f.mux.Handle(route.Pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route.Handler.ServeHTTP(w, r.WithContext(access.WithPrincipal(r.Context(), f.as)))
		}))
	}
	return f
}

// call sends a request and returns the status and the decoded body.
func (f *fixture) call(method, path, body string) (int, map[string]any) {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			f.t.Fatalf("%s %s = %d %q, not a JSON object", method, path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			f.t.Errorf("Content-Type = %q", ct)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		f.t.Errorf("%s %s: no Cache-Control: no-store", method, path)
	}
	return rec.Code, out
}

func equal(label, value string) silence.Matcher {
	return silence.Matcher{Label: label, Operator: silence.Equal, Value: value}
}

func (f *fixture) create(n silence.New) silence.Silence {
	f.t.Helper()
	if n.Comment == "" {
		n.Comment = "maintenance"
	}
	s, err := f.store.Create(context.Background(), n, f.ada)
	if err != nil {
		f.t.Fatalf("Create: %v", err)
	}
	return s
}

func TestMatching(t *testing.T) {
	f := newFixture(t)
	labels := map[string]string{"alertname": "DiskFull", "host": "db1", "env": "prod"}
	tests := []struct {
		name     string
		matchers []silence.Matcher
		want     bool
	}{
		{"equal", []silence.Matcher{equal("alertname", "DiskFull")}, true},
		{"equal, another value", []silence.Matcher{equal("alertname", "DiskFul")}, false},
		{"all must match", []silence.Matcher{equal("alertname", "DiskFull"), equal("host", "db2")}, false},
		{"not equal", []silence.Matcher{equal("alertname", "DiskFull"), {Label: "env", Operator: silence.NotEqual, Value: "dev"}}, true},
		{"not equal, the same value", []silence.Matcher{equal("alertname", "DiskFull"), {Label: "env", Operator: silence.NotEqual, Value: "prod"}}, false},
		{"a pattern", []silence.Matcher{{Label: "host", Operator: silence.Matches, Value: "db[0-9]+"}}, true},
		{"a pattern is anchored", []silence.Matcher{{Label: "host", Operator: silence.Matches, Value: "db"}}, false},
		{"a pattern is anchored at the start too", []silence.Matcher{{Label: "host", Operator: silence.Matches, Value: "b1"}}, false},
		{"not a pattern", []silence.Matcher{equal("alertname", "DiskFull"), {Label: "host", Operator: silence.NotMatches, Value: "web.*"}}, true},
		{"not a pattern that matches", []silence.Matcher{equal("alertname", "DiskFull"), {Label: "host", Operator: silence.NotMatches, Value: "db.*"}}, false},
		{"a missing label is empty", []silence.Matcher{equal("alertname", "DiskFull"), equal("team", "")}, true},
		{"a missing label is not a value", []silence.Matcher{equal("team", "dba")}, false},
		{"a missing label matches a pattern that allows empty", []silence.Matcher{equal("alertname", "DiskFull"), {Label: "team", Operator: silence.Matches, Value: "dba|"}}, true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Through the database, as the pipeline reads them.
			s := f.create(silence.New{Matchers: tt.matchers, EndsAt: f.clock.Now().Add(time.Hour)})
			active, err := f.store.Active(context.Background(), f.pool, f.clock.Now())
			if err != nil || len(active) != i+1 {
				t.Fatalf("Active = %d silences, %v", len(active), err)
			}
			for _, a := range active {
				if a.ID == s.ID && a.Silences(labels) != tt.want {
					t.Errorf("Silences(%v) = %v, want %v", labels, !tt.want, tt.want)
				}
			}
		})
	}
	if (silence.Silence{}).Silences(labels) {
		t.Error("a silence without matchers silences everything")
	}
}

func TestCreateRefusesWhatIsNotASilence(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	ok := []silence.Matcher{equal("alertname", "DiskFull")}
	many := make([]silence.Matcher, silence.MaxMatchers+1)
	for i := range many {
		many[i] = equal(fmt.Sprintf("l%d", i), "x")
	}
	tests := []struct {
		name  string
		n     silence.New
		field string
		msg   string
	}{
		{"no matchers", silence.New{EndsAt: now.Add(time.Hour)}, "matchers", "at least one matcher"},
		{"too many matchers", silence.New{Matchers: many, EndsAt: now.Add(time.Hour)}, "matchers", "at most 20"},
		{"not a label name", silence.New{Matchers: []silence.Matcher{equal("bad-label", "x")}, EndsAt: now.Add(time.Hour)}, "matchers[0].label", "must be a label name"},
		{"an empty label name", silence.New{Matchers: []silence.Matcher{equal("", "x")}, EndsAt: now.Add(time.Hour)}, "matchers[0].label", "must be a label name"},
		{"a long label name", silence.New{Matchers: []silence.Matcher{equal(strings.Repeat("a", 201), "x")}, EndsAt: now.Add(time.Hour)}, "matchers[0].label", "at most 200"},
		{"an unknown operator", silence.New{Matchers: []silence.Matcher{{Label: "a", Operator: "==", Value: "x"}}, EndsAt: now.Add(time.Hour)}, "matchers[0].operator", `must be "=", "!=", "=~" or "!~"`},
		{"no operator", silence.New{Matchers: []silence.Matcher{{Label: "a", Value: "x"}}, EndsAt: now.Add(time.Hour)}, "matchers[0].operator", `must be "="`},
		{"a long value", silence.New{Matchers: []silence.Matcher{equal("a", strings.Repeat("x", 1025))}, EndsAt: now.Add(time.Hour)}, "matchers[0].value", "longer than 1024"},
		{"a long pattern", silence.New{Matchers: []silence.Matcher{equal("a", "b"), {Label: "c", Operator: silence.Matches, Value: strings.Repeat("x", 1025)}}, EndsAt: now.Add(time.Hour)}, "matchers[1].value", "longer than 1024"},
		{"a broken pattern", silence.New{Matchers: []silence.Matcher{{Label: "a", Operator: silence.Matches, Value: "(x"}}, EndsAt: now.Add(time.Hour)}, "matchers[0].value", "is not a regular expression: missing closing )"},
		{"a broken negative pattern", silence.New{Matchers: []silence.Matcher{equal("a", "b"), {Label: "c", Operator: silence.NotMatches, Value: "[x"}}, EndsAt: now.Add(time.Hour)}, "matchers[1].value", "is not a regular expression"},
		{"a value that is not text", silence.New{Matchers: []silence.Matcher{equal("a", "x\x00y")}, EndsAt: now.Add(time.Hour)}, "matchers[0].value", "valid text"},
		{"only an empty value", silence.New{Matchers: []silence.Matcher{equal("team", "")}, EndsAt: now.Add(time.Hour)}, "matchers", "must not match an empty value"},
		{"only a negation", silence.New{Matchers: []silence.Matcher{{Label: "team", Operator: silence.NotEqual, Value: "dba"}}, EndsAt: now.Add(time.Hour)}, "matchers", "must not match an empty value"},
		{"only a pattern that allows empty", silence.New{Matchers: []silence.Matcher{{Label: "team", Operator: silence.Matches, Value: ".*"}}, EndsAt: now.Add(time.Hour)}, "matchers", "must not match an empty value"},
		{"no comment", silence.New{Matchers: ok, EndsAt: now.Add(time.Hour), Comment: " \n "}, "comment", "is required"},
		{"a long comment", silence.New{Matchers: ok, EndsAt: now.Add(time.Hour), Comment: strings.Repeat("é", 1001)}, "comment", "at most 1000"},
		{"no end", silence.New{Matchers: ok}, "ends_at", "is required"},
		{"an end in the past", silence.New{Matchers: ok, EndsAt: now.Add(-time.Minute)}, "ends_at", "must be in the future"},
		{"an end that is now", silence.New{Matchers: ok, EndsAt: now}, "ends_at", "must be in the future"},
		{"an end before the start", silence.New{Matchers: ok, StartsAt: now.Add(2 * time.Hour), EndsAt: now.Add(time.Hour)}, "ends_at", "must be after starts_at"},
		{"longer than a year", silence.New{Matchers: ok, EndsAt: now.Add(367 * 24 * time.Hour)}, "ends_at", "at most a year"},
		{"a start more than a year ahead", silence.New{Matchers: ok, StartsAt: now.Add(400 * 24 * time.Hour), EndsAt: now.Add(401 * 24 * time.Hour)}, "starts_at", "within a year"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.n.Comment == "" {
				tt.n.Comment = "maintenance"
			}
			_, err := f.store.Create(context.Background(), tt.n, f.ada)
			var bad *silence.Error
			if !errors.As(err, &bad) || bad.Field != tt.field || !strings.Contains(bad.Msg, tt.msg) {
				t.Errorf("Create = %v, want an error at %q containing %q", err, tt.field, tt.msg)
			}
		})
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM silences`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d silences were stored from refused requests, %v", n, err)
	}
	// The greatest sizes are accepted.
	most := make([]silence.Matcher, silence.MaxMatchers)
	for i := range most {
		most[i] = silence.Matcher{Label: strings.Repeat("l", 199) + string(rune('a'+i)), Operator: silence.Matches, Value: strings.Repeat("x", 1024)}
	}
	if _, err := f.store.Create(context.Background(), silence.New{Matchers: most, EndsAt: now.Add(366 * 24 * time.Hour), Comment: strings.Repeat("é", 1000)}, f.ada); err != nil {
		t.Errorf("a silence of the greatest size: %v", err)
	}
	// A silence needs an author.
	if _, err := f.store.Create(context.Background(), silence.New{Matchers: ok, EndsAt: now.Add(time.Hour), Comment: "x"}, access.Principal{}); err == nil {
		t.Error("a silence without an author was created")
	}
}

func TestLifeOfASilence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	start := f.clock.Now()
	now := f.create(silence.New{Matchers: []silence.Matcher{equal("alertname", "A")}, EndsAt: start.Add(2 * time.Hour)})
	later := f.create(silence.New{Matchers: []silence.Matcher{equal("alertname", "B")}, StartsAt: start.Add(time.Hour), EndsAt: start.Add(3 * time.Hour)})
	if now.State != silence.Active || !now.StartsAt.Equal(start) || now.CreatedBy != "ada@example.com" || now.CreatedByToken != "" {
		t.Errorf("a silence without a start = %+v, want it active from now, by ada", now)
	}
	if later.State != silence.Pending {
		t.Errorf("a silence that starts in an hour is %s", later.State)
	}
	// A start in the past is now: a silence cannot be made for the past.
	if past := f.create(silence.New{Matchers: []silence.Matcher{equal("alertname", "C")}, StartsAt: start.Add(-time.Hour), EndsAt: start.Add(time.Minute)}); !past.StartsAt.Equal(start) {
		t.Errorf("a silence with a start in the past starts at %v", past.StartsAt)
	}

	active := func() string {
		t.Helper()
		list, err := f.store.Active(ctx, f.pool, f.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range list {
			names = append(names, s.Matchers[0].Value)
		}
		return strings.Join(names, ",")
	}
	state := func(id string) silence.State {
		t.Helper()
		s, err := f.store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return s.State
	}
	if got := active(); got != "C,A" {
		t.Errorf("active at the start = %q, want C and A, the one that ends first at the top", got)
	}
	f.clock.Advance(time.Hour)
	if got := active(); got != "A,B" || state(later.ID) != silence.Active {
		t.Errorf("active after an hour = %q, B is %s", got, state(later.ID))
	}
	// The end is exclusive: at ends_at the silence is over.
	f.clock.Advance(time.Hour)
	if got := active(); got != "B" || state(now.ID) != silence.Expired {
		t.Errorf("active after two hours = %q, A is %s", got, state(now.ID))
	}
}

func TestExpire(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	start := f.clock.Now()
	current := f.create(silence.New{Matchers: []silence.Matcher{equal("alertname", "A")}, EndsAt: start.Add(2 * time.Hour)})
	pending := f.create(silence.New{Matchers: []silence.Matcher{equal("alertname", "B")}, StartsAt: start.Add(time.Hour), EndsAt: start.Add(3 * time.Hour)})

	f.clock.Advance(30 * time.Minute)
	at := f.clock.Now()
	got, err := f.store.Expire(ctx, current.ID)
	if err != nil || got.State != silence.Expired || !got.EndsAt.Equal(at) || !got.StartsAt.Equal(start) || got.CreatedBy != "ada@example.com" {
		t.Fatalf("Expire = %+v, %v; want it ended now, with its start and author", got, err)
	}
	// A silence that has not begun never will.
	got, err = f.store.Expire(ctx, pending.ID)
	if err != nil || got.State != silence.Expired || !got.EndsAt.Equal(at) || !got.StartsAt.Equal(at) {
		t.Fatalf("Expire of a pending silence = %+v, %v", got, err)
	}
	if list, err := f.store.Active(ctx, f.pool, at.Add(time.Hour)); err != nil || len(list) != 0 {
		t.Errorf("active after both were expired: %v, %v", list, err)
	}
	// Expiring again changes nothing, and does not move the end.
	f.clock.Advance(time.Hour)
	if got, err := f.store.Expire(ctx, current.ID); err != nil || !got.EndsAt.Equal(at) {
		t.Errorf("second Expire = ends at %v, %v; want %v", got.EndsAt, err, at)
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid", ""} {
		if _, err := f.store.Expire(ctx, id); !errors.Is(err, silence.ErrNotFound) {
			t.Errorf("Expire(%q) = %v, want ErrNotFound", id, err)
		}
		if _, err := f.store.Get(ctx, id); !errors.Is(err, silence.ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

// TestAPI goes through the four routes as a member and as an API token.
func TestAPI(t *testing.T) {
	f := newFixture(t)

	code, created := f.call("POST", "/api/v1/silences", `{
		"matchers": [{"label": "alertname", "operator": "=", "value": "DiskFull"}, {"label": "host", "operator": "=~", "value": "db[0-9]+"}],
		"duration": "2h", "comment": "  replacing the disk  "}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, created)
	}
	want := map[string]any{
		"matchers": []any{
			map[string]any{"label": "alertname", "operator": "=", "value": "DiskFull"},
			map[string]any{"label": "host", "operator": "=~", "value": "db[0-9]+"},
		},
		"starts_at": "2026-03-01T12:00:00Z", "ends_at": "2026-03-01T14:00:00Z", "created_at": "2026-03-01T12:00:00Z",
		"created_by": "ada@example.com", "comment": "replacing the disk", "state": "active",
	}
	id, _ := created["id"].(string)
	delete(created, "id")
	if fmt.Sprint(created) != fmt.Sprint(want) || len(id) != 36 {
		t.Errorf("created:\n%v\nwant:\n%v\nid %q", created, want, id)
	}

	// With an API token, and with explicit times: the token is the author.
	f.as = f.ci
	code, byToken := f.call("POST", "/api/v1/silences", `{"matchers": [{"label": "team", "operator": "=", "value": "dba"}],
		"starts_at": "2026-03-01T18:00:00+01:00", "ends_at": "2026-03-02T06:00:00+01:00", "comment": "night shift"}`)
	if code != http.StatusCreated || byToken["created_by_token"] != "ci" || byToken["created_by"] != nil || byToken["state"] != "pending" ||
		byToken["starts_at"] != "2026-03-01T17:00:00Z" || byToken["ends_at"] != "2026-03-02T05:00:00Z" {
		t.Errorf("created with a token = %d %v", code, byToken)
	}

	code, got := f.call("GET", "/api/v1/silences/"+id, "")
	if code != http.StatusOK || got["id"] != id || got["created_by"] != "ada@example.com" || got["state"] != "active" {
		t.Errorf("get = %d %v", code, got)
	}
	list := func(query string) []string {
		t.Helper()
		code, body := f.call("GET", "/api/v1/silences"+query, "")
		if code != http.StatusOK {
			t.Fatalf("list = %d %v", code, body)
		}
		var out []string
		for _, s := range body["silences"].([]any) {
			out = append(out, fmt.Sprint(s.(map[string]any)["comment"], " ", s.(map[string]any)["state"]))
		}
		return out
	}
	if got := list(""); fmt.Sprint(got) != "[replacing the disk active night shift pending]" {
		t.Errorf("list = %v", got)
	}

	// A token may end a silence that a person made.
	if code, body := f.call("DELETE", "/api/v1/silences/"+id, ""); code != http.StatusNoContent || body != nil {
		t.Errorf("expire = %d %v, want 204 and no body", code, body)
	}
	if code, _ := f.call("DELETE", "/api/v1/silences/"+id, ""); code != http.StatusNoContent {
		t.Errorf("expire again = %d, want 204", code)
	}
	if got := list(""); fmt.Sprint(got) != "[night shift pending]" {
		t.Errorf("list after the expiry = %v", got)
	}
	if got := list("?expired=true"); fmt.Sprint(got) != "[night shift pending replacing the disk expired]" {
		t.Errorf("list with the expired = %v", got)
	}
	if code, got := f.call("GET", "/api/v1/silences/"+id, ""); code != http.StatusOK || got["state"] != "expired" || got["ends_at"] != "2026-03-01T12:00:00Z" {
		t.Errorf("the expired silence = %d %v", code, got)
	}

	for _, path := range []string{"/api/v1/silences/00000000-0000-0000-0000-000000000000", "/api/v1/silences/nope"} {
		for _, method := range []string{"GET", "DELETE"} {
			if code, body := f.call(method, path, ""); code != http.StatusNotFound || body["error"] != "no such silence" {
				t.Errorf("%s %s = %d %v, want 404", method, path, code, body)
			}
		}
	}
	// Who did what is in the log, by identifier.
	for _, wantLog := range []string{`"msg":"silence created","silence_id":"` + id + `"`, `"by_user":"` + f.ada.UserID + `"`, `"msg":"silence expired"`, `"by_token":"` + f.ci.TokenID + `"`} {
		if !strings.Contains(f.logs.String(), wantLog) {
			t.Errorf("logs do not contain %s:\n%s", wantLog, f.logs.String())
		}
	}

	// An empty list is a list.
	f.call("DELETE", "/api/v1/silences/"+byToken["id"].(string), "")
	if code, body := f.call("GET", "/api/v1/silences", ""); code != http.StatusOK || fmt.Sprint(body) != "map[silences:[]]" {
		t.Errorf("empty list = %d %v", code, body)
	}
}

func TestAPIRefusesBadRequests(t *testing.T) {
	f := newFixture(t)
	matcher := `{"label": "alertname", "operator": "=", "value": "A"}`
	tests := []struct {
		name, body, field, msg string
		code                   int
	}{
		{"not JSON", `matchers=1`, "", "the body must be JSON such as", 400},
		{"an unknown field", `{"matchers": [` + matcher + `], "duration": "1h", "comment": "x", "author": "mallory"}`, "", "the body must be JSON such as", 400},
		{"a time that is not one", `{"matchers": [` + matcher + `], "ends_at": "tomorrow", "comment": "x"}`, "", "RFC 3339", 400},
		{"no end", `{"matchers": [` + matcher + `], "comment": "x"}`, "ends_at", "is required", 400},
		{"an end and a duration", `{"matchers": [` + matcher + `], "ends_at": "2026-03-01T13:00:00Z", "duration": "1h", "comment": "x"}`, "duration", "not both", 400},
		{"a bad duration", `{"matchers": [` + matcher + `], "duration": "soon", "comment": "x"}`, "duration", "must be a duration", 400},
		{"a duration of zero", `{"matchers": [` + matcher + `], "duration": "0s", "comment": "x"}`, "duration", "must be a duration", 400},
		{"a duration over a year", `{"matchers": [` + matcher + `], "duration": "53w", "comment": "x"}`, "duration", "at most a year", 400},
		{"no matchers", `{"matchers": [], "duration": "1h", "comment": "x"}`, "matchers", "at least one matcher", 400},
		{"a broken pattern", `{"matchers": [{"label": "a", "operator": "=~", "value": "(x"}], "duration": "1h", "comment": "x"}`, "matchers[0].value", "is not a regular expression", 400},
		{"no comment", `{"matchers": [` + matcher + `], "duration": "1h"}`, "comment", "is required", 400},
		{"a body over the limit", `{"matchers": [` + matcher + `], "duration": "1h", "comment": "` + strings.Repeat("x", 70<<10) + `"}`, "", "larger than 64 KiB", 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := f.call("POST", "/api/v1/silences", tt.body)
			field, _ := body["field"].(string)
			msg, _ := body["error"].(string)
			if code != tt.code || field != tt.field || !strings.Contains(msg, tt.msg) {
				t.Errorf("= %d %v, want %d at %q containing %q", code, body, tt.code, tt.field, tt.msg)
			}
		})
	}
	if code, body := f.call("GET", "/api/v1/silences?expired=maybe", ""); code != http.StatusBadRequest || body["field"] != "expired" {
		t.Errorf("list with a bad filter = %d %v", code, body)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM silences`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d silences were stored from refused requests, %v", n, err)
	}
}

// TestTheDatabaseKeepsASilenceWhole: the constraints hold whatever the code
// does.
func TestTheDatabaseKeepsASilenceWhole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, query := range map[string]string{
		"no author":               `INSERT INTO silences (matchers, starts_at, ends_at, comment) VALUES ('[]', now(), now() + interval '1 hour', 'x')`,
		"two authors":             `INSERT INTO silences (matchers, starts_at, ends_at, comment, created_by, created_by_token) VALUES ('[]', now(), now() + interval '1 hour', 'x', '` + f.ada.UserID + `', '` + f.ci.TokenID + `')`,
		"an end before the start": `INSERT INTO silences (matchers, starts_at, ends_at, comment, created_by) VALUES ('[]', now(), now() - interval '1 hour', 'x', '` + f.ada.UserID + `')`,
		"an author who is nobody": `INSERT INTO silences (matchers, starts_at, ends_at, comment, created_by) VALUES ('[]', now(), now() + interval '1 hour', 'x', gen_random_uuid())`,
	} {
		if _, err := f.pool.Exec(ctx, query); err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
}
