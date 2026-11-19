package schedule_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/schedule"
)

// api is the schedules API with a principal in every request, as the guard
// of the server puts one there.
type api struct {
	t   *testing.T
	mux *http.ServeMux
	as  access.Principal
}

func (f *fixture) api(as access.Principal) *api {
	a := &api{t: f.t, mux: http.NewServeMux(), as: as}
	for _, route := range schedule.NewHandler(slog.New(slog.DiscardHandler), f.store).Routes() {
		if route.Level != access.Member {
			f.t.Errorf("%s is %s, want member", route.Pattern, route.Level)
		}
		a.mux.HandleFunc(route.Pattern, func(w http.ResponseWriter, r *http.Request) {
			route.Handler.ServeHTTP(w, r.WithContext(access.WithPrincipal(r.Context(), a.as)))
		})
	}
	return a
}

func (a *api) call(method, path, body string) (int, map[string]any) {
	a.t.Helper()
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			a.t.Fatalf("%s %s answered %q, which is not JSON", method, path, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestOverridesAPI(t *testing.T) {
	f := started(t)
	a := f.api(f.as(grace))
	const path = "/api/v1/schedules/primary/overrides"

	code, created := a.call("POST", path, `{"user": "linus@example.com", "starts_at": "2026-01-04T00:00:00+01:00", "ends_at": "2026-01-05T00:00:00Z", "reason": "  swap  "}`)
	if code != http.StatusCreated || created["user"] != linus || created["schedule"] != "primary" || created["state"] != "pending" ||
		created["starts_at"] != "2026-01-03T23:00:00Z" || created["ends_at"] != "2026-01-05T00:00:00Z" || created["reason"] != "swap" ||
		created["created_by"] != grace || created["created_by_token"] != nil {
		t.Fatalf("create = %d %v", code, created)
	}
	id, _ := created["id"].(string)

	// With a duration, from now, by a token.
	a.as = access.Principal{TokenID: f.token("rota-bot"), Name: "rota-bot", Role: access.RoleMember}
	code, second := a.call("POST", path, `{"user": "ken@example.com", "duration": "2h"}`)
	if code != http.StatusCreated || second["state"] != "active" || second["starts_at"] != "2026-01-03T10:00:00Z" || second["ends_at"] != "2026-01-03T12:00:00Z" ||
		second["created_by_token"] != "rota-bot" || second["created_by"] != nil {
		t.Fatalf("create with a duration = %d %v", code, second)
	}

	code, list := a.call("GET", path, "")
	items, _ := list["overrides"].([]any)
	if code != http.StatusOK || len(items) != 2 || items[0].(map[string]any)["user"] != "ken@example.com" {
		t.Fatalf("list = %d %v", code, list)
	}

	// An overlap is a conflict, and says with what.
	code, body := a.call("POST", path, `{"user": "ada@example.com", "duration": "1h"}`)
	if msg, _ := body["error"].(string); code != http.StatusConflict || !strings.Contains(msg, "overlaps the override of ken@example.com from 2026-01-03T10:00:00Z to 2026-01-03T12:00:00Z") || body["field"] != "starts_at" {
		t.Errorf("overlap = %d %v", code, body)
	}

	if code, body := a.call("DELETE", path+"/"+id, ""); code != http.StatusNoContent || body != nil {
		t.Errorf("delete = %d %v, want 204", code, body)
	}
	if code, list := a.call("GET", path, ""); code != http.StatusOK || len(list["overrides"].([]any)) != 1 {
		t.Errorf("list after delete = %d %v", code, list)
	}
	code, list = a.call("GET", path+"?past=true", "")
	if items, _ := list["overrides"].([]any); code != http.StatusOK || len(items) != 2 || items[1].(map[string]any)["state"] != "cancelled" {
		t.Errorf("list with past = %d %v", code, list)
	}
	// A schedule with nothing answers with an empty list, not null.
	f.apply(rota("primary", "UTC", ada, grace, linus), rota("secondary", "UTC", linus))
	if code, list := a.call("GET", "/api/v1/schedules/secondary/overrides", ""); code != http.StatusOK || list["overrides"] == nil || len(list["overrides"].([]any)) != 0 {
		t.Errorf("empty list = %d %v", code, list)
	}

	for _, tc := range []struct {
		method, path, body string
		code               int
		field              string
	}{
		{"POST", path, `not json`, http.StatusBadRequest, ""},
		{"POST", path, `{"user": "linus@example.com", "duration": "1h", "by": "someone"}`, http.StatusBadRequest, ""},
		{"POST", path, `{"user": "linus@example.com", "duration": "1h", "ends_at": "2026-01-05T00:00:00Z"}`, http.StatusBadRequest, "duration"},
		{"POST", path, `{"user": "linus@example.com", "duration": "soon"}`, http.StatusBadRequest, "duration"},
		{"POST", path, `{"user": "linus@example.com"}`, http.StatusBadRequest, "ends_at"},
		{"POST", path, `{"user": "nobody@example.com", "duration": "1h"}`, http.StatusBadRequest, "user"},
		{"POST", path, `{"user": "linus@example.com", "duration": "1h", "reason": "` + strings.Repeat("x", 20000) + `"}`, http.StatusRequestEntityTooLarge, ""},
		{"POST", "/api/v1/schedules/nope/overrides", `{"user": "linus@example.com", "duration": "1h"}`, http.StatusNotFound, ""},
		{"GET", "/api/v1/schedules/nope/overrides", "", http.StatusNotFound, ""},
		{"GET", path + "?past=maybe", "", http.StatusBadRequest, "past"},
		{"DELETE", path + "/not-a-uuid", "", http.StatusNotFound, ""},
		{"DELETE", path + "/00000000-0000-4000-8000-000000000000", "", http.StatusNotFound, ""},
		{"DELETE", "/api/v1/schedules/nope/overrides/" + id, "", http.StatusNotFound, ""},
	} {
		code, body := a.call(tc.method, tc.path, tc.body)
		field, _ := body["field"].(string)
		if msg, _ := body["error"].(string); code != tc.code || field != tc.field || msg == "" {
			t.Errorf("%s %s %.60s = %d %v; want %d about %q", tc.method, tc.path, tc.body, code, body, tc.code, tc.field)
		}
	}
}

// token creates an API token row and returns its identifier.
func (f *fixture) token(name string) string {
	f.t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(), `INSERT INTO api_tokens (name, token_hash, role) VALUES ($1, $2, 'member') RETURNING id::text`, name, "hash-of-"+name).Scan(&id)
	if err != nil {
		f.t.Fatalf("create token: %v", err)
	}
	return id
}
