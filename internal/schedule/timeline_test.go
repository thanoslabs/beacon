package schedule_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/schedule"
)

// timeline returns who is on call for the primary schedule in a period, as
// "who start..end", with " override" and " planned" where they apply.
func (f *fixture) timeline(from, to time.Time) []string {
	f.t.Helper()
	shifts, err := f.store.Timeline(context.Background(), "primary", from, to)
	if err != nil {
		f.t.Fatalf("Timeline: %v", err)
	}
	var out []string
	for _, s := range shifts {
		line := strings.TrimSuffix(s.User, "@example.com") + " " + s.StartsAt.Format("Jan 2 15:04") + ".." + s.EndsAt.Format("Jan 2 15:04")
		if s.Kind == schedule.KindOverride {
			if s.OverrideID == "" {
				f.t.Errorf("the override shift %s names no override", line)
			}
			line += " override"
		}
		if s.Planned {
			line += " planned"
		}
		out = append(out, line)
	}
	return out
}

// withOverrides is a rotation of Ada, Grace and Linus, daily at 09:00, on
// 5 January at 12:00. Ken covered from the 3rd at 18:00 to the 4th at
// 12:00; Linus will cover from the 6th at 12:00 to the 7th at 12:00; an
// override for the 8th was removed.
func withOverrides(t *testing.T) *fixture {
	t.Helper()
	f := started(t)
	f.override("ken@example.com", day(3, 18, 0), day(4, 12, 0))
	f.override(linus, day(6, 12, 0), day(7, 12, 0))
	gone := f.override(ada, day(8, 0, 0), day(8, 12, 0))
	if _, err := f.store.CancelOverride(context.Background(), "primary", gone.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(day(5, 12, 0))
	return f
}

// TestTimeline asks who is on call over a week that began in the past: the
// record for what has happened, the plan for what is to come, and overrides
// in both. There is one person at every moment.
func TestTimeline(t *testing.T) {
	f := withOverrides(t)
	want := []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 3 18:00",
		"ken Jan 3 18:00..Jan 4 12:00 override",
		"linus Jan 4 12:00..Jan 5 09:00",
		"ada Jan 5 09:00..Jan 6 09:00 planned",
		"grace Jan 6 09:00..Jan 6 12:00 planned",
		"linus Jan 6 12:00..Jan 7 12:00 override planned",
		"linus Jan 7 12:00..Jan 8 09:00 planned",
		"ada Jan 8 09:00..Jan 9 09:00 planned",
	}
	equal(t, "timeline", f.timeline(day(2, 0, 0), day(9, 0, 0)), want)

	// A period inside one shift, and periods that end where a shift begins.
	equal(t, "inside a shift", f.timeline(day(5, 13, 0), day(5, 14, 0)), []string{"ada Jan 5 09:00..Jan 6 09:00 planned"})
	equal(t, "up to a handoff", f.timeline(day(4, 12, 0), day(5, 9, 0)), []string{"linus Jan 4 12:00..Jan 5 09:00"})
	equal(t, "only the future", f.timeline(day(7, 0, 0), day(7, 13, 0)), []string{"linus Jan 6 12:00..Jan 7 12:00 override planned", "linus Jan 7 12:00..Jan 8 09:00 planned"})
	// Before the schedule existed there is nothing.
	if got := f.timeline(time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 12, 8, 0, 0, 0, 0, time.UTC)); len(got) != 0 {
		t.Errorf("timeline before the schedule existed: %v", got)
	}

	// The answer does not depend on whether the worker has run: time
	// passes, two handoffs and the start of an override fall due, and
	// nothing carries them out.
	f.clock.Set(day(6, 13, 0))
	late := f.timeline(day(2, 0, 0), day(9, 0, 0))
	f.advance(day(6, 13, 0))
	equal(t, "timeline before and after the worker ran", late, f.timeline(day(2, 0, 0), day(9, 0, 0)))
	if late[4] != "ada Jan 5 09:00..Jan 6 09:00" || late[6] != "linus Jan 6 12:00..Jan 7 12:00 override planned" {
		t.Errorf("timeline at 13:00 on the 6th: %v", late)
	}

	for name, period := range map[string][2]time.Time{
		"an empty period":    {day(5, 0, 0), day(5, 0, 0)},
		"a backwards period": {day(6, 0, 0), day(5, 0, 0)},
		"more than a year":   {day(5, 0, 0), day(5, 0, 0).Add(schedule.MaxTimelineRange + time.Second)},
	} {
		var bad *schedule.Error
		if _, err := f.store.Timeline(context.Background(), "primary", period[0], period[1]); !errors.As(err, &bad) || bad.Field != "to" {
			t.Errorf("%s: %v, want an error about to", name, err)
		}
	}
	if _, err := f.store.Timeline(context.Background(), "nope", day(5, 0, 0), day(6, 0, 0)); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("Timeline of an unknown schedule = %v", err)
	}
}

// TestTimelineIsWhatHappens asks for the plan of the coming weeks, with a
// change of definition and an override ahead, lets the time pass, and asks
// for the same period again when all of it is on record. The plan and the
// record must name the same people at the same times.
func TestTimelineIsWhatHappens(t *testing.T) {
	f := withOverrides(t)
	changed := rota("primary", "Australia/Sydney", linus, "ken@example.com", ada)
	changed.Rotation.Handoff.Time = "07:30"
	f.apply(at(changed, day(9, 4, 0)))
	f.override(grace, day(12, 22, 0), day(14, 1, 0))

	from, to := day(5, 12, 0), day(20, 0, 0)
	plan := f.timeline(from, to)
	if len(plan) < 15 {
		t.Fatalf("the plan has only %d shifts: %v", len(plan), plan)
	}
	for now := from; now.Before(day(22, 0, 0)); now = now.Add(5 * time.Hour) {
		f.advance(now)
	}
	record := f.timeline(from, to)
	for i := range plan {
		plan[i] = strings.TrimSuffix(plan[i], " planned")
	}
	equal(t, "the record against the plan", record, plan)
	for _, line := range record {
		if strings.HasSuffix(line, " planned") {
			t.Errorf("a shift that is over is still planned: %s", line)
		}
	}
}

// TestTeamOnCall answers who is on call for a team: the person on call for
// each of its schedules, with overrides.
func TestTeamOnCall(t *testing.T) {
	f := started(t)
	other := rota("database", "UTC", linus)
	other.Team = "dba"
	f.apply(rota("primary", "UTC", ada, grace, linus), rota("secondary", "UTC", linus, ada), other)
	f.override("ken@example.com", time.Time{}, day(3, 20, 0))

	who := func(team string) string {
		t.Helper()
		on, err := f.store.TeamOnCall(context.Background(), team)
		if err != nil {
			t.Fatalf("TeamOnCall(%s): %v", team, err)
		}
		var out []string
		for _, o := range on {
			entry := o.Schedule + "=" + strings.TrimSuffix(o.Email, "@example.com")
			if o.Override != nil {
				entry += " covering " + strings.TrimSuffix(o.Rotation, "@example.com")
			}
			out = append(out, entry)
		}
		return strings.Join(out, ", ")
	}
	if got, want := who("platform"), "primary=ken covering grace, secondary=linus"; got != want {
		t.Errorf("platform: %s, want %s", got, want)
	}
	if got, want := who("dba"), "database=linus"; got != want {
		t.Errorf("dba: %s, want %s", got, want)
	}
	if got := who("idle"); got != "" {
		t.Errorf("a team without schedules: %s", got)
	}
	if _, err := f.store.TeamOnCall(context.Background(), "nope"); !errors.Is(err, schedule.ErrTeamNotFound) {
		t.Errorf("TeamOnCall of an unknown team = %v", err)
	}
	// By the identifier of the team, inside a transaction, as the
	// escalation engine asks.
	var teamID string
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM teams WHERE name = 'platform'`).Scan(&teamID); err != nil {
		t.Fatal(err)
	}
	on, err := f.store.OnCallForTeam(context.Background(), f.pool, teamID)
	if err != nil || len(on) != 2 || on[0].Email != "ken@example.com" || on[0].UserID != f.userID("ken@example.com") || on[1].Email != linus {
		t.Errorf("OnCallForTeam = %+v, %v", on, err)
	}

	// After the override, and after a handoff that nothing carried out.
	f.clock.Set(day(4, 9, 30))
	if got, want := who("platform"), "primary=linus, secondary=linus"; got != want {
		t.Errorf("platform the next day: %s, want %s", got, want)
	}
}

func TestSchedulesAPI(t *testing.T) {
	f := withOverrides(t)
	a := f.api(f.as(grace))
	other := rota("database", "UTC", "nobody@example.com")
	other.Team = "dba"
	weekly := rota("primary", "Europe/London", ada, grace)
	weekly.Rotation.Handoff.Time = "10:00"
	f.apply(at(weekly, day(7, 0, 0)), other)

	code, list := a.call("GET", "/api/v1/schedules", "")
	items, _ := list["schedules"].([]any)
	if code != http.StatusOK || len(items) != 2 {
		t.Fatalf("list = %d %v", code, list)
	}
	database, primary := items[0].(map[string]any), items[1].(map[string]any)
	// Nobody is a user on the database rotation.
	if on := database["on_call"].(map[string]any); database["name"] != "database" || database["team"] != "dba" || on["user"] != nil || on["rotation_user"] != nil {
		t.Errorf("database = %v", database)
	}

	code, one := a.call("GET", "/api/v1/schedules/primary", "")
	if code != http.StatusOK || one["name"] != primary["name"] {
		t.Fatalf("get = %d %v", code, one)
	}
	rotation, _ := one["rotation"].(map[string]any)
	pending, _ := one["pending"].(map[string]any)
	onCall, _ := one["on_call"].(map[string]any)
	if one["team"] != "platform" || one["next_handoff"] != "2026-01-06T09:00:00Z" ||
		rotation["time_zone"] != "UTC" || rotation["shift_length"] != "1d" || rotation["handoff_time"] != "09:00" || len(rotation["members"].([]any)) != 3 {
		t.Errorf("schedule = %v", one)
	}
	if pending == nil || pending["takes_over_at"] != "2026-01-07T00:00:00Z" || pending["stated"] != true || pending["rotation"].(map[string]any)["handoff_time"] != "10:00" {
		t.Errorf("pending = %v", pending)
	}
	if onCall["user"] != ada || onCall["rotation_user"] != ada || onCall["since"] != "2026-01-05T09:00:00Z" || onCall["until"] != "2026-01-06T09:00:00Z" || onCall["override"] != nil {
		t.Errorf("on_call = %v", onCall)
	}

	// During the override.
	f.clock.Set(day(6, 15, 0))
	_, one = a.call("GET", "/api/v1/schedules/primary", "")
	onCall, _ = one["on_call"].(map[string]any)
	override, _ := onCall["override"].(map[string]any)
	if onCall["user"] != linus || onCall["rotation_user"] != grace || onCall["until"] != "2026-01-07T12:00:00Z" || override["state"] != "active" || override["reason"] != "covering" {
		t.Errorf("on_call during an override = %v", onCall)
	}
	code, team := a.call("GET", "/api/v1/teams/platform/on-call", "")
	on, _ := team["on_call"].([]any)
	if code != http.StatusOK || team["team"] != "platform" || len(on) != 1 || on[0].(map[string]any)["user"] != linus || on[0].(map[string]any)["schedule"] != "primary" {
		t.Errorf("team on call = %d %v", code, team)
	}
	if code, team := a.call("GET", "/api/v1/teams/idle/on-call", ""); code != http.StatusOK || team["on_call"] == nil || len(team["on_call"].([]any)) != 0 {
		t.Errorf("team without schedules = %d %v", code, team)
	}

	// Shifts: four weeks from now unless the request says.
	code, body := a.call("GET", "/api/v1/schedules/primary/shifts", "")
	shifts, _ := body["shifts"].([]any)
	if code != http.StatusOK || body["from"] != "2026-01-06T15:00:00Z" || body["to"] != "2026-02-03T15:00:00Z" || len(shifts) < 20 {
		t.Fatalf("shifts = %d, from %v to %v, %d shifts", code, body["from"], body["to"], len(shifts))
	}
	first := shifts[0].(map[string]any)
	if first["user"] != linus || first["kind"] != "override" || first["override_id"] == nil || first["planned"] != true || first["starts_at"] != "2026-01-06T12:00:00Z" {
		t.Errorf("the first shift = %v", first)
	}
	code, body = a.call("GET", "/api/v1/schedules/primary/shifts?from=2026-01-03T00:00:00Z&to=2026-01-04T14:00:00%2B02:00", "")
	shifts, _ = body["shifts"].([]any)
	if code != http.StatusOK || len(shifts) != 3 || shifts[2].(map[string]any)["user"] != "ken@example.com" || shifts[0].(map[string]any)["planned"] != false || body["to"] != "2026-01-04T12:00:00Z" {
		t.Errorf("shifts of a past period = %d %v", code, body)
	}

	for _, tc := range []struct {
		path  string
		code  int
		field string
	}{
		{"/api/v1/schedules/nope", http.StatusNotFound, ""},
		{"/api/v1/schedules/nope/shifts", http.StatusNotFound, ""},
		{"/api/v1/teams/nope/on-call", http.StatusNotFound, ""},
		{"/api/v1/schedules/primary/shifts?from=yesterday", http.StatusBadRequest, "from"},
		{"/api/v1/schedules/primary/shifts?to=2026-01-08", http.StatusBadRequest, "to"},
		{"/api/v1/schedules/primary/shifts?from=2026-01-08T00:00:00Z&to=2026-01-07T00:00:00Z", http.StatusBadRequest, "to"},
		{"/api/v1/schedules/primary/shifts?to=2028-01-08T00:00:00Z", http.StatusBadRequest, "to"},
	} {
		code, body := a.call("GET", tc.path, "")
		field, _ := body["field"].(string)
		if msg, _ := body["error"].(string); code != tc.code || field != tc.field || msg == "" {
			t.Errorf("GET %s = %d %v; want %d about %q", tc.path, code, body, tc.code, tc.field)
		}
	}
}
