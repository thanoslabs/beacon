package schedule_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/schedule"
)

// userID returns the identifier of the user with an address.
func (f *fixture) userID(email string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		f.t.Fatalf("read user %s: %v", email, err)
	}
	return id
}

// as returns the principal of a signed-in user.
func (f *fixture) as(email string) access.Principal {
	return access.Principal{UserID: f.userID(email), Email: email, Role: access.RoleMember}
}

// override creates an override of the primary schedule as Ada.
func (f *fixture) override(who string, from, to time.Time) schedule.Override {
	f.t.Helper()
	o, err := f.store.CreateOverride(context.Background(), schedule.NewOverride{Schedule: "primary", User: who, StartsAt: from, EndsAt: to, Reason: "covering"}, f.as(ada))
	if err != nil {
		f.t.Fatalf("CreateOverride: %v", err)
	}
	return o
}

// onCall says who is on call for a schedule now.
func (f *fixture) onCall(name string) string {
	f.t.Helper()
	on, err := f.store.OnCall(context.Background(), f.pool, f.ids[name])
	if err != nil {
		f.t.Fatalf("OnCall(%s): %v", name, err)
	}
	return strings.TrimSuffix(on.Email, "@example.com")
}

// started is a fixture with a daily rotation of Ada, Grace and Linus on
// which Grace has been on call since 3 January, 09:00. It is 10:00.
func started(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.users(ada, grace, linus, "ken@example.com")
	f.apply(rota("primary", "UTC", ada, grace, linus))
	f.advance(day(3, 9, 0))
	f.clock.Set(day(3, 10, 0))
	return f
}

// TestOverride follows an override through its life. While it is in force
// its person is on call in place of the rotation's, which goes on
// underneath; both are recorded, and nothing recorded is changed.
func TestOverride(t *testing.T) {
	f := started(t)
	// Ken, who is not in the rotation, covers from the 3rd at 18:00 to the
	// 5th at 12:00: across two handoffs.
	o := f.override("Ken@Example.com", day(3, 18, 0), day(5, 12, 0))
	if o.State != schedule.OverridePending || o.User != "ken@example.com" || o.CreatedBy != ada || o.Schedule != "primary" {
		t.Fatalf("override = %+v", o)
	}
	want := []string{"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00.."}
	equal(t, "shifts before the override begins", f.shifts("primary", time.UTC), want)

	for _, step := range []struct {
		at   time.Time
		want string
	}{
		{day(3, 17, 59), "grace"},
		{day(3, 18, 0), "ken"},
		{day(4, 9, 0), "ken"}, // the rotation hands over to Linus underneath
		{day(5, 9, 0), "ken"}, // and to Ada
		{day(5, 11, 59), "ken"},
		{day(5, 12, 0), "ada"}, // the override is over
		{day(6, 9, 0), "grace"},
	} {
		// Who is on call does not wait for the worker.
		f.clock.Set(step.at)
		if got := f.onCall("primary"); got != step.want {
			t.Errorf("at %s, before the worker ran, %s is on call; want %s", step.at.Format("Jan 2 15:04"), got, step.want)
		}
		f.advance(step.at)
		if got := f.onCall("primary"); got != step.want {
			t.Errorf("at %s %s is on call, want %s", step.at.Format("Jan 2 15:04"), got, step.want)
		}
	}
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 4 09:00",
		"ken Jan 3 18:00..Jan 5 12:00 override",
		"linus Jan 4 09:00..Jan 5 09:00",
		"ada Jan 5 09:00..Jan 6 09:00",
		"grace Jan 6 09:00..",
	})
	// The shift names its override.
	recorded, err := f.store.Recorded(context.Background(), f.ids["primary"], day(3, 0, 0), day(7, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range recorded {
		if s.Kind == schedule.KindOverride {
			found = s.OverrideID == o.ID
		} else if s.OverrideID != "" {
			t.Errorf("a rotation shift names override %s", s.OverrideID)
		}
	}
	if !found {
		t.Errorf("no recorded shift names override %s: %+v", o.ID, recorded)
	}
	// The rotation's own position was never touched by the override.
	if p := f.position("primary"); p.Email != grace {
		t.Errorf("the rotation has %s on call", p.Email)
	}
}

// TestOverrideThatBeginsNow is recorded in the request that creates it.
func TestOverrideThatBeginsNow(t *testing.T) {
	f := started(t)
	// A start in the past means now.
	o := f.override(linus, day(1, 0, 0), day(3, 20, 0))
	if o.State != schedule.OverrideActive || !o.StartsAt.Equal(day(3, 10, 0)) {
		t.Fatalf("override = %+v, want it active from now", o)
	}
	if got := f.onCall("primary"); got != "linus" {
		t.Errorf("%s is on call, want linus", got)
	}
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00..", "linus Jan 3 10:00.. override"})
	// The worker does not record it a second time.
	f.advance(day(3, 11, 0))
	f.advance(day(3, 20, 5))
	equal(t, "shifts after it ended", f.shifts("primary", time.UTC), []string{"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00..", "linus Jan 3 10:00..Jan 3 20:00 override"})
}

// who asks who is on call for the primary schedule at a moment, and checks
// that the timeline of the hour around that moment says the same.
func (f *fixture) who(at time.Time) string {
	f.t.Helper()
	f.clock.Set(at)
	on := f.onCall("primary")
	shifts, err := f.store.Timeline(context.Background(), "primary", at, at.Add(time.Second))
	if err != nil || len(shifts) != 1 {
		f.t.Fatalf("Timeline at %s = %+v, %v; want one shift", at.Format("Jan 2 15:04"), shifts, err)
	}
	if got := strings.TrimSuffix(shifts[0].User, "@example.com"); got != on {
		f.t.Errorf("at %s OnCall says %s and the timeline says %s", at.Format("Jan 2 15:04"), on, got)
	}
	return on
}

// TestOverridesAreLayered states the rule for overrides that overlap: the
// one created last is in force where they overlap, and the earlier one
// still holds outside that time. Who is on call, the timeline and the
// recorded shifts agree, before and after the worker runs.
func TestOverridesAreLayered(t *testing.T) {
	f := started(t)
	// Linus covers the 4th and the 5th. Then Ken is given the middle of
	// that, and Ada, last, an hour in the middle of Ken's time.
	long := f.override(linus, day(4, 0, 0), day(6, 0, 0))
	f.override("ken@example.com", day(4, 12, 0), day(5, 12, 0))
	f.override(ada, day(4, 18, 0), day(4, 19, 0))
	// And one that was created first and begins last is beneath them all.
	plan := f.timeline(day(3, 10, 0), day(6, 12, 0))
	equal(t, "the plan", plan, []string{
		"grace Jan 3 09:00..Jan 4 00:00 planned",
		"linus Jan 4 00:00..Jan 4 12:00 override planned",
		"ken Jan 4 12:00..Jan 4 18:00 override planned",
		"ada Jan 4 18:00..Jan 4 19:00 override planned",
		"ken Jan 4 19:00..Jan 5 12:00 override planned",
		"linus Jan 5 12:00..Jan 6 00:00 override planned",
		"ada Jan 6 00:00..Jan 6 09:00 planned",
		"grace Jan 6 09:00..Jan 7 09:00 planned",
	})

	for _, step := range []struct {
		at   time.Time
		want string
	}{
		{day(3, 23, 59), "grace"},
		{day(4, 0, 0), "linus"},
		{day(4, 11, 59), "linus"},
		{day(4, 12, 0), "ken"},
		{day(4, 18, 0), "ada"},
		{day(4, 18, 59), "ada"},
		{day(4, 19, 0), "ken"},
		{day(5, 11, 59), "ken"},
		{day(5, 12, 0), "linus"},
		{day(5, 23, 59), "linus"},
		{day(6, 0, 0), "ada"},
	} {
		// Before the worker has run, and after.
		if got := f.who(step.at); got != step.want {
			t.Errorf("at %s, before the worker ran, %s is on call; want %s", step.at.Format("Jan 2 15:04"), got, step.want)
		}
		f.advance(step.at)
		if got := f.who(step.at); got != step.want {
			t.Errorf("at %s %s is on call, want %s", step.at.Format("Jan 2 15:04"), got, step.want)
		}
	}

	// How long the person on call has it, with the layers taken into
	// account: Linus, on the 5th, since Ken's override ended.
	f.clock.Set(day(5, 13, 0))
	on, err := f.store.OnCall(context.Background(), f.pool, f.ids["primary"])
	if err != nil || on.Override == nil || on.Override.ID != long.ID || !on.Since.Equal(day(5, 12, 0)) || !on.Until.Equal(day(6, 0, 0)) || on.Rotation != ada {
		t.Errorf("OnCall = %+v, %v; want Linus by his override from the 5th at 12:00 to the 6th, covering Ada", on, err)
	}

	// What happened is what was planned.
	f.advance(day(6, 12, 0))
	record := f.timeline(day(3, 10, 0), day(6, 12, 0))
	for i := range plan {
		plan[i] = strings.TrimSuffix(plan[i], " planned")
	}
	plan[len(plan)-1] += " planned"
	equal(t, "the record against the plan", record, plan)

	// Every override is on record as a shift of its own, for its whole
	// period, like the rotation shifts beneath them: which of them was in
	// force follows from the order they were created in.
	equal(t, "recorded shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 4 09:00",
		"linus Jan 4 00:00..Jan 6 00:00 override",
		"linus Jan 4 09:00..Jan 5 09:00",
		"ken Jan 4 12:00..Jan 5 12:00 override",
		"ada Jan 4 18:00..Jan 4 19:00 override",
		"ada Jan 5 09:00..Jan 6 09:00",
		"grace Jan 6 09:00..",
	})
}

// TestLaterOverrideWinsWhateverItsPeriod: what decides between two
// overrides is which was created last, not which begins first, is longer,
// or was created at a later time on the clock.
func TestLaterOverrideWinsWhateverItsPeriod(t *testing.T) {
	f := started(t)
	// Both are created at the same instant on the clock; the order of the
	// requests decides.
	f.override("ken@example.com", day(4, 12, 0), day(4, 13, 0))
	f.override(linus, day(4, 0, 0), day(5, 0, 0))
	if got := f.who(day(4, 12, 30)); got != "linus" {
		t.Errorf("under a later, longer override %s is on call, want linus", got)
	}
	// The one beneath never comes into force, and is recorded all the same.
	f.advance(day(5, 1, 0))
	equal(t, "timeline", f.timeline(day(4, 0, 0), day(5, 0, 0)), []string{"linus Jan 4 00:00..Jan 5 00:00 override"})
	var recorded []string
	for _, shift := range f.shifts("primary", time.UTC) {
		if strings.HasSuffix(shift, " override") {
			recorded = append(recorded, shift)
		}
	}
	equal(t, "recorded override shifts", recorded, []string{"linus Jan 4 00:00..Jan 5 00:00 override", "ken Jan 4 12:00..Jan 4 13:00 override"})

	// The same period twice: the second request wins, eight at once all
	// succeed, and exactly one of them is on top.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := f.store.CreateOverride(context.Background(), schedule.NewOverride{Schedule: "primary", User: ada, StartsAt: day(10, 0, 0), EndsAt: day(11, 0, 0)}, f.as(grace)); err != nil {
				t.Errorf("CreateOverride: %v", err)
			}
		})
	}
	wg.Wait()
	equal(t, "eight overrides of one period", f.timeline(day(10, 0, 0), day(11, 0, 0)), []string{"ada Jan 10 00:00..Jan 11 00:00 override planned"})
}

// TestCancellingAnOverrideRevealsTheOneBeneath removes the override on top,
// before it begins and while it is in force.
func TestCancellingAnOverrideRevealsTheOneBeneath(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	f.override(linus, day(4, 0, 0), day(6, 0, 0))
	top := f.override("ken@example.com", day(4, 12, 0), day(5, 12, 0))
	later := f.override(ada, day(5, 0, 0), day(5, 6, 0))

	// The one that has not begun is removed: Ken has that time again.
	f.advance(day(4, 13, 0))
	if _, err := f.store.CancelOverride(ctx, "primary", later.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.who(day(5, 3, 0)); got != "ken" {
		t.Errorf("after the override on top was removed before it began, %s is on call; want ken", got)
	}
	// Ken's is removed while it is in force: Linus, beneath, is on call at
	// once, and for the rest of his own period.
	f.clock.Set(day(5, 4, 0))
	if _, err := f.store.CancelOverride(ctx, "primary", top.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.who(day(5, 4, 0)); got != "linus" {
		t.Errorf("after the override in force was removed %s is on call, want linus", got)
	}
	on, err := f.store.OnCall(ctx, f.pool, f.ids["primary"])
	if err != nil || !on.Since.Equal(day(5, 4, 0)) || !on.Until.Equal(day(6, 0, 0)) {
		t.Errorf("OnCall = since %s until %s, %v; want from the removal to the end of Linus's override", on.Since, on.Until, err)
	}
	f.advance(day(6, 1, 0))
	equal(t, "timeline", f.timeline(day(4, 0, 0), day(6, 0, 0)), []string{
		"linus Jan 4 00:00..Jan 4 12:00 override",
		"ken Jan 4 12:00..Jan 5 04:00 override",
		"linus Jan 5 04:00..Jan 6 00:00 override",
	})
	// Removing the one beneath as well leaves the rotation.
	f.override(linus, day(7, 0, 0), day(8, 0, 0))
	under := f.override(grace, day(7, 6, 0), day(7, 12, 0))
	if _, err := f.store.CancelOverride(ctx, "primary", under.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.who(day(7, 8, 0)); got != "linus" {
		t.Errorf("%s is on call, want linus", got)
	}
}

// TestCancelOverride removes overrides before, during and after their
// period.
func TestCancelOverride(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	pending := f.override(linus, day(4, 0, 0), day(5, 0, 0))
	active := f.override("ken@example.com", time.Time{}, day(3, 20, 0))

	// One that has not begun never does.
	got, err := f.store.CancelOverride(ctx, "primary", pending.ID)
	if err != nil || got.State != schedule.OverrideCancelled || got.CancelledAt == nil {
		t.Fatalf("CancelOverride(pending) = %+v, %v", got, err)
	}
	// One in force ends now, and the rotation's person is on call again.
	f.clock.Set(day(3, 14, 0))
	if got := f.onCall("primary"); got != "ken" {
		t.Fatalf("%s is on call, want ken", got)
	}
	if _, err := f.store.CancelOverride(ctx, "primary", active.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.onCall("primary"); got != "grace" {
		t.Errorf("after the override was removed %s is on call, want grace", got)
	}
	f.advance(day(6, 0, 0))
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00..Jan 4 09:00", "ken Jan 3 10:00..Jan 3 14:00 override",
		"linus Jan 4 09:00..Jan 5 09:00", "ada Jan 5 09:00..",
	})

	// Removing again, or after the end, changes nothing and is no error.
	again, err := f.store.CancelOverride(ctx, "primary", active.ID)
	if err != nil || !again.CancelledAt.Equal(day(3, 14, 0)) {
		t.Errorf("CancelOverride twice = %+v, %v", again, err)
	}
	over := f.override(linus, day(6, 1, 0), day(6, 2, 0))
	f.advance(day(6, 3, 0))
	if got, err := f.store.CancelOverride(ctx, "primary", over.ID); err != nil || got.State != schedule.OverrideEnded {
		t.Errorf("CancelOverride(ended) = %+v, %v", got, err)
	}

	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if _, err := f.store.CancelOverride(ctx, "primary", id); !errors.Is(err, schedule.ErrOverrideNotFound) {
			t.Errorf("CancelOverride(%s) = %v, want ErrOverrideNotFound", id, err)
		}
	}
	if _, err := f.store.CancelOverride(ctx, "nope", over.ID); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("CancelOverride on an unknown schedule = %v, want ErrNotFound", err)
	}
	// An override belongs to its schedule.
	f.apply(rota("primary", "UTC", ada, grace, linus), rota("secondary", "UTC", linus))
	if _, err := f.store.CancelOverride(ctx, "secondary", over.ID); !errors.Is(err, schedule.ErrOverrideNotFound) {
		t.Errorf("CancelOverride through another schedule = %v, want ErrOverrideNotFound", err)
	}

	listed, err := f.store.Overrides(ctx, "primary", false)
	if err != nil || len(listed) != 0 {
		t.Errorf("Overrides = %+v, %v; want none in force or to come", listed, err)
	}
	if listed, err = f.store.Overrides(ctx, "primary", true); err != nil || len(listed) != 3 {
		t.Errorf("Overrides(past) = %d, %v; want 3", len(listed), err)
	}
}

// TestOverrideAcrossARestart creates overrides and stops everything. The
// process that starts later records what began, and what began and ended,
// while nothing ran, with the times of the overrides.
func TestOverrideAcrossARestart(t *testing.T) {
	f := started(t)
	f.override(linus, day(3, 12, 0), day(3, 16, 0))
	f.override("ken@example.com", day(4, 12, 0), day(8, 0, 0))

	f.store = f.restart()
	f.clock.Set(day(5, 10, 0))
	f.apply(rota("primary", "UTC", ada, grace, linus))
	// Several processes start at once.
	var wg sync.WaitGroup
	for range 4 {
		store := f.restart()
		wg.Go(func() {
			if _, err := store.Advance(context.Background()); err != nil {
				t.Errorf("Advance: %v", err)
			}
		})
	}
	wg.Wait()
	f.advance(day(5, 10, 0))
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 4 09:00",
		"linus Jan 3 12:00..Jan 3 16:00 override",
		"linus Jan 4 09:00..Jan 5 09:00",
		"ken Jan 4 12:00.. override",
		"ada Jan 5 09:00..",
	})
	if got := f.onCall("primary"); got != "ken" {
		t.Errorf("%s is on call, want ken", got)
	}
}

// TestOverridesEndWithTheirSchedule removes a schedule that has overrides.
func TestOverridesEndWithTheirSchedule(t *testing.T) {
	f := started(t)
	f.override("ken@example.com", time.Time{}, day(4, 0, 0))
	f.override(linus, day(5, 0, 0), day(6, 0, 0))
	f.clock.Set(day(3, 12, 0))
	f.apply()
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00..Jan 3 12:00", "ken Jan 3 10:00..Jan 3 12:00 override",
	})
	// The schedule returns; the old overrides do not.
	f.apply(rota("primary", "UTC", ada, grace, linus))
	f.advance(day(5, 12, 0))
	for _, s := range f.shifts("primary", time.UTC)[3:] {
		if strings.Contains(s, "override") {
			t.Errorf("an override of the removed schedule began: %s", s)
		}
	}
}

func TestCreateOverrideRefusesWhatIsNotAnOverride(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	ok := schedule.NewOverride{Schedule: "primary", User: linus, EndsAt: day(4, 0, 0)}
	for name, tc := range map[string]struct {
		change func(*schedule.NewOverride)
		field  string
	}{
		"no user":            {func(n *schedule.NewOverride) { n.User = "" }, "user"},
		"an unknown user":    {func(n *schedule.NewOverride) { n.User = "nobody@example.com" }, "user"},
		"no end":             {func(n *schedule.NewOverride) { n.EndsAt = time.Time{} }, "ends_at"},
		"an end in the past": {func(n *schedule.NewOverride) { n.EndsAt = day(2, 0, 0) }, "ends_at"},
		"an end before the start": {func(n *schedule.NewOverride) {
			n.StartsAt, n.EndsAt = day(5, 0, 0), day(4, 0, 0)
		}, "ends_at"},
		"longer than a year": {func(n *schedule.NewOverride) { n.EndsAt = day(3, 10, 0).Add(schedule.MaxOverrideLength + time.Hour) }, "ends_at"},
		"too far ahead": {func(n *schedule.NewOverride) {
			n.StartsAt = day(3, 10, 0).Add(schedule.MaxOverrideLength + time.Hour)
			n.EndsAt = n.StartsAt.Add(time.Hour)
		}, "starts_at"},
		"a long reason": {func(n *schedule.NewOverride) { n.Reason = strings.Repeat("x", schedule.MaxReasonLength+1) }, "reason"},
	} {
		n := ok
		tc.change(&n)
		var bad *schedule.Error
		if _, err := f.store.CreateOverride(ctx, n, f.as(ada)); !errors.As(err, &bad) || bad.Field != tc.field {
			t.Errorf("%s: %v, want an error about %s", name, err, tc.field)
		}
	}
	if _, err := f.store.CreateOverride(ctx, schedule.NewOverride{Schedule: "nope", User: linus, EndsAt: day(4, 0, 0)}, f.as(ada)); !errors.Is(err, schedule.ErrNotFound) {
		t.Errorf("an unknown schedule: %v, want ErrNotFound", err)
	}
	if _, err := f.store.CreateOverride(ctx, ok, access.Principal{}); err == nil {
		t.Error("an override without an author was accepted")
	}
	if n := len(f.shifts("primary", time.UTC)); n != 2 {
		t.Errorf("%d shifts after only refused requests, want 2", n)
	}
}
