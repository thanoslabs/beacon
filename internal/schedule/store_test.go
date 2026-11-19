package schedule_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/schedule"
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

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// fixture is a database with a team, and a store whose time the test sets.
type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *schedule.Store
	clock *clock
	// cfg is the configuration that was applied last.
	cfg config.Config
	ids map[string]string
}

// The fixture starts on Thursday 1 January 2026 at 12:00 UTC.
var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: postgrestest.NewMigrated(t), clock: &clock{now: epoch}}
	f.store = f.restart()
	return f
}

// restart returns a new store on the same database, as a process that
// starts again has.
func (f *fixture) restart() *schedule.Store {
	s := schedule.NewStore(f.pool)
	s.SetClock(f.clock.Now)
	return s
}

// users creates a user for each address.
func (f *fixture) users(emails ...string) {
	f.t.Helper()
	for _, e := range emails {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO users (email) VALUES ($1)`, e); err != nil {
			f.t.Fatalf("create user: %v", err)
		}
	}
}

// rota is a schedule of the platform team that hands over daily at 09:00.
func rota(name, zone string, members ...string) config.Schedule {
	return config.Schedule{Name: name, Team: "platform", TimeZone: zone, Rotation: config.Rotation{
		Members: members, ShiftLength: config.Duration(config.Day), Handoff: config.Handoff{Time: "09:00"},
	}}
}

// apply reconciles a configuration with these schedules and applies them,
// as beacon serve does at start and on a reload.
func (f *fixture) apply(schedules ...config.Schedule) schedule.Applied {
	f.t.Helper()
	return f.applyWith(f.store, schedules...)
}

func (f *fixture) applyWith(store *schedule.Store, schedules ...config.Schedule) schedule.Applied {
	f.t.Helper()
	// The teams that the schedules name, and two that are always there.
	teams := []config.Team{{Name: "platform"}, {Name: "idle"}}
	for _, sched := range schedules {
		if !slices.ContainsFunc(teams, func(t config.Team) bool { return t.Name == sched.Team }) {
			teams = append(teams, config.Team{Name: sched.Team})
		}
	}
	f.cfg = config.Config{Teams: teams, Schedules: schedules}
	res, err := provision.Reconcile(context.Background(), f.pool, f.cfg)
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	f.ids = res.IDs[provision.Schedules]
	var provisioned []schedule.Provisioned
	for _, s := range schedules {
		provisioned = append(provisioned, schedule.Provisioned{ID: f.ids[s.Name], Schedule: s})
	}
	applied, err := store.Apply(context.Background(), provisioned)
	if err != nil {
		f.t.Fatalf("Apply: %v", err)
	}
	return applied
}

// advance moves the clock and carries out what has fallen due.
func (f *fixture) advance(to time.Time) []schedule.Handoff {
	f.t.Helper()
	f.clock.Set(to)
	done, err := f.store.Advance(context.Background())
	if err != nil {
		f.t.Fatalf("Advance: %v", err)
	}
	return done
}

// shifts lists every recorded shift of a schedule, oldest first, as
// "who start..end" with times in loc.
func (f *fixture) shifts(name string, loc *time.Location) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT u.email, f.starts_at, f.ends_at, f.kind FROM shifts f JOIN users u ON u.id = f.user_id JOIN schedules s ON s.id = f.schedule_id
		WHERE s.name = $1 ORDER BY f.starts_at, f.id`, name)
	if err != nil {
		f.t.Fatalf("read shifts: %v", err)
	}
	const layout = "Jan 2 15:04"
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var (
			email, kind string
			start       time.Time
			end         *time.Time
		)
		if err := r.Scan(&email, &start, &end, &kind); err != nil {
			return "", err
		}
		line := strings.TrimSuffix(email, "@example.com") + " " + start.In(loc).Format(layout) + ".."
		if end != nil {
			line += end.In(loc).Format(layout)
		}
		if kind != schedule.KindRotation {
			line += " " + kind
		}
		return line, nil
	})
	if err != nil {
		f.t.Fatalf("read shifts: %v", err)
	}
	return out
}

func (f *fixture) position(name string) schedule.Position {
	f.t.Helper()
	p, err := f.store.Position(context.Background(), f.ids[name])
	if err != nil {
		f.t.Fatalf("Position(%s): %v", name, err)
	}
	return p
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got:\n  %s\nwant:\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

const (
	ada   = "ada@example.com"
	grace = "grace@example.com"
	linus = "linus@example.com"
)

// TestRotation follows a schedule from the moment it is applied: the first
// member is on call at once, each handoff closes one shift and opens the
// next, and the position says who is on call and since when.
func TestRotation(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	applied := f.apply(rota("primary", "UTC", ada, grace, linus))
	if len(applied.Started) != 1 || len(applied.Skipped) != 0 {
		t.Fatalf("Apply = %+v, want one schedule started", applied)
	}

	// The first shift is never shorter than a normal one: it began at noon,
	// so it ends at 09:00 the day after tomorrow.
	p := f.position("primary")
	if p.Email != ada || !p.ShiftStartedAt.Equal(epoch) || !p.NextHandoffAt.Equal(time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("position = %s since %s until %s", p.Email, p.ShiftStartedAt, p.NextHandoffAt)
	}
	equal(t, "shifts at the start", f.shifts("primary", time.UTC), []string{"ada Jan 1 12:00.."})

	// Nothing happens before the handoff, and applying the same
	// configuration again changes nothing.
	if done := f.advance(time.Date(2026, 1, 3, 8, 59, 59, 0, time.UTC)); len(done) != 0 {
		t.Errorf("handoffs before the time: %+v", done)
	}
	if applied := f.apply(rota("primary", "UTC", ada, grace, linus)); len(applied.Started) != 0 {
		t.Errorf("applying again started %v", applied.Started)
	}
	equal(t, "shifts before the handoff", f.shifts("primary", time.UTC), []string{"ada Jan 1 12:00.."})

	// The handoff is carried out a little late, and is still recorded at
	// 09:00 exactly.
	done := f.advance(time.Date(2026, 1, 3, 9, 0, 4, 0, time.UTC))
	if len(done) != 1 || done[0].From != ada || done[0].To != grace || !done[0].At.Equal(time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("handoff = %+v", done)
	}
	f.advance(time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC))
	equal(t, "shifts after four handoffs", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 4 09:00",
		"linus Jan 4 09:00..Jan 5 09:00",
		"ada Jan 5 09:00..Jan 6 09:00",
		"grace Jan 6 09:00..",
	})
	if p := f.position("primary"); p.Email != grace || !p.ShiftStartedAt.Equal(time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("position = %s since %s", p.Email, p.ShiftStartedAt)
	}
}

// TestRecordedShiftsAreNeverRewritten shows that what has been recorded
// stays as it is, whatever happens next: more handoffs, the configuration
// applied again with other members, and even a direct attempt to change a
// closed shift, which the database refuses.
func TestRecordedShiftsAreNeverRewritten(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	f.apply(rota("primary", "UTC", ada, grace, linus))
	f.advance(time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC))

	dump := func() string {
		rows, err := f.pool.Query(context.Background(), `SELECT id::text || ' ' || user_id::text || ' ' || starts_at::text || ' ' || ends_at::text || ' ' || kind
			FROM shifts WHERE ends_at IS NOT NULL ORDER BY starts_at`)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(lines, "\n")
	}
	before := dump()
	if strings.Count(before, "\n") != 2 {
		t.Fatalf("closed shifts:\n%s\nwant 3", before)
	}

	f.apply(rota("primary", "Australia/Sydney", linus, ada))
	f.advance(time.Date(2026, 1, 20, 9, 0, 0, 0, time.UTC))
	f.apply()
	if after := dump(); !strings.HasPrefix(after, before) {
		t.Errorf("closed shifts changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	ctx := context.Background()
	for _, change := range []string{
		`UPDATE shifts SET ends_at = ends_at + interval '1 hour' WHERE ends_at IS NOT NULL`,
		`UPDATE shifts SET starts_at = starts_at - interval '1 hour' WHERE ends_at IS NOT NULL`,
		`UPDATE shifts SET user_id = (SELECT id FROM users WHERE email = 'linus@example.com') WHERE ends_at IS NOT NULL`,
		`UPDATE shifts SET ends_at = NULL WHERE ends_at IS NOT NULL`,
	} {
		if _, err := f.pool.Exec(ctx, change); err == nil || !strings.Contains(err.Error(), "a closed shift is never changed") {
			t.Errorf("%s: %v, want the database to refuse", change, err)
		}
	}
	if after := dump(); !strings.HasPrefix(after, before) {
		t.Errorf("closed shifts changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRestartInTheMiddleOfAShift stops the process during a shift. The
// process that starts later, however much later, finds the position in the
// database, leaves the shift in progress as it was, and records the shifts
// that began while nothing ran with the times they began at.
func TestRestartInTheMiddleOfAShift(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	f.apply(rota("primary", "UTC", ada, grace, linus))
	f.advance(time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC))
	want := []string{"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00.."}
	equal(t, "before the restart", f.shifts("primary", time.UTC), want)

	// A restart within the shift: the start applies the configuration again.
	f.store = f.restart()
	f.clock.Set(time.Date(2026, 1, 3, 20, 0, 0, 0, time.UTC))
	if applied := f.apply(rota("primary", "UTC", ada, grace, linus)); len(applied.Started) != 0 {
		t.Errorf("the restart started %v again", applied.Started)
	}
	if done := f.advance(f.clock.Now()); len(done) != 0 {
		t.Errorf("the restart handed over: %+v", done)
	}
	equal(t, "after the restart", f.shifts("primary", time.UTC), want)
	if p := f.position("primary"); p.Email != grace || !p.ShiftStartedAt.Equal(time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("position after the restart = %s since %s", p.Email, p.ShiftStartedAt)
	}

	// Nothing runs for three days and a bit.
	f.store = f.restart()
	f.clock.Set(time.Date(2026, 1, 6, 10, 30, 0, 0, time.UTC))
	f.apply(rota("primary", "UTC", ada, grace, linus))
	if done := f.advance(f.clock.Now()); len(done) != 3 {
		t.Errorf("%d handoffs after the gap, want 3", len(done))
	}
	equal(t, "after the gap", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 4 09:00",
		"linus Jan 4 09:00..Jan 5 09:00",
		"ada Jan 5 09:00..Jan 6 09:00",
		"grace Jan 6 09:00..",
	})
}

// TestReplicasHandOverOnce runs several stores at once over many due
// handoffs. Every handoff must be carried out by exactly one of them.
func TestReplicasHandOverOnce(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	var schedules []config.Schedule
	for i := range 5 {
		schedules = append(schedules, rota(fmt.Sprintf("rota-%d", i), "UTC", ada, grace, linus))
	}
	f.apply(schedules...)
	// Ten handoffs are due on each schedule.
	f.clock.Set(time.Date(2026, 1, 12, 9, 0, 0, 0, time.UTC))

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for range 6 {
		store := f.restart()
		wg.Go(func() {
			done, err := store.Advance(context.Background())
			if err != nil {
				t.Errorf("Advance: %v", err)
			}
			mu.Lock()
			total += len(done)
			mu.Unlock()
		})
	}
	wg.Wait()
	// One store may find nothing left while another still works; a last
	// pass picks up whatever was held at that moment.
	total += len(f.advance(f.clock.Now()))
	if total != 50 {
		t.Errorf("%d handoffs carried out, want 50", total)
	}
	for i := range 5 {
		got := f.shifts(fmt.Sprintf("rota-%d", i), time.UTC)
		if len(got) != 11 || got[10] != "grace Jan 12 09:00.." || got[9] != "ada Jan 11 09:00..Jan 12 09:00" {
			t.Errorf("rota-%d has %d shifts, ending %v", i, len(got), got[max(len(got)-2, 0):])
		}
	}
	var overlaps int
	err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM shifts a JOIN shifts b
		ON a.schedule_id = b.schedule_id AND a.id < b.id AND a.starts_at < COALESCE(b.ends_at, 'infinity') AND b.starts_at < COALESCE(a.ends_at, 'infinity')`).Scan(&overlaps)
	if err != nil || overlaps != 0 {
		t.Errorf("%d pairs of shifts overlap, %v", overlaps, err)
	}
}

// TestMembersWhoAreNotUsers lists people in a rotation before they have
// signed in. They are reported and passed over; with nobody at all, nobody
// is on call until the first of them signs in.
func TestMembersWhoAreNotUsers(t *testing.T) {
	f := newFixture(t)
	applied := f.apply(rota("primary", "UTC", "Ada@Example.com", grace, linus))
	if len(applied.Skipped) != 3 || applied.Skipped[0] != (schedule.Skipped{Schedule: "primary", Email: ada}) {
		t.Fatalf("Skipped = %+v, want all three", applied.Skipped)
	}
	if p := f.position("primary"); p.Email != "" || p.UserID != "" {
		t.Fatalf("position = %+v, want nobody", p)
	}
	if got := f.shifts("primary", time.UTC); len(got) != 0 {
		t.Fatalf("shifts with no users: %v", got)
	}

	// Linus signs in. He is the first member who is a user, so he is on
	// call from then until the handoff that was planned.
	f.users(linus)
	done := f.advance(time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC))
	if len(done) != 1 || done[0].From != "" || done[0].To != linus {
		t.Fatalf("Advance = %+v, want Linus to begin", done)
	}
	// Ada signs in during his shift and takes her place at the handoff:
	// after Linus the order wraps to her.
	f.users(ada)
	if done := f.advance(time.Date(2026, 1, 2, 16, 0, 0, 0, time.UTC)); len(done) != 0 {
		t.Errorf("a sign-in during a shift moved it: %+v", done)
	}
	f.advance(time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC))
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"linus Jan 2 15:00..Jan 3 09:00",
		"ada Jan 3 09:00..Jan 4 09:00",
		"linus Jan 4 09:00..Jan 5 09:00",
		"ada Jan 5 09:00..",
	})
	if applied := f.apply(rota("primary", "UTC", ada, grace, linus)); len(applied.Skipped) != 1 || applied.Skipped[0].Email != grace {
		t.Errorf("Skipped = %+v, want Grace", applied.Skipped)
	}
}

// TestScheduleLeavesTheConfiguration removes a schedule and brings it back.
// Removing it ends the shift in progress; when it returns it starts afresh,
// with the shifts of its first life still on record.
func TestScheduleLeavesTheConfiguration(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace)
	f.apply(rota("primary", "UTC", ada, grace), rota("secondary", "UTC", grace, ada))
	f.advance(time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC))

	f.clock.Set(time.Date(2026, 1, 3, 15, 0, 0, 0, time.UTC))
	if applied := f.apply(rota("secondary", "UTC", grace, ada)); len(applied.Ended) != 1 || applied.Ended[0] != "primary" {
		t.Fatalf("Apply = %+v, want primary ended", applied)
	}
	ended := []string{"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00..Jan 3 15:00"}
	equal(t, "shifts of the removed schedule", f.shifts("primary", time.UTC), ended)
	// A removed schedule has no handoffs.
	if done := f.advance(time.Date(2026, 1, 8, 9, 0, 0, 0, time.UTC)); len(done) != 5 {
		t.Errorf("%d handoffs while one schedule is removed, want the 5 of the other", len(done))
	}
	equal(t, "shifts of the removed schedule, later", f.shifts("primary", time.UTC), ended)

	f.clock.Set(time.Date(2026, 1, 8, 10, 0, 0, 0, time.UTC))
	if applied := f.apply(rota("primary", "UTC", ada, grace), rota("secondary", "UTC", grace, ada)); len(applied.Started) != 1 {
		t.Fatalf("Apply = %+v, want primary started again", applied)
	}
	equal(t, "shifts after it returned", f.shifts("primary", time.UTC), append(ended, "ada Jan 8 10:00.."))
}

// TestRotationAcrossDaylightSavingInTheDatabase runs a stored rotation
// through the clock changes of a northern and a southern zone, one tick an
// hour, as the server would. The recorded shifts begin at 09:00 local time
// on every day.
func TestRotationAcrossDaylightSavingInTheDatabase(t *testing.T) {
	for _, tc := range []struct {
		zone       string
		start, end time.Time
		want       []string
	}{
		{
			zone: "America/New_York", start: time.Date(2026, 3, 5, 17, 0, 0, 0, time.UTC), end: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
			want: []string{"ada Mar 5 12:00..Mar 7 09:00", "grace Mar 7 09:00..Mar 8 09:00", "ada Mar 8 09:00..Mar 9 09:00", "grace Mar 9 09:00.."},
		},
		{
			zone: "Australia/Sydney", start: time.Date(2026, 4, 2, 1, 0, 0, 0, time.UTC), end: time.Date(2026, 4, 6, 12, 0, 0, 0, time.UTC),
			want: []string{"ada Apr 2 12:00..Apr 4 09:00", "grace Apr 4 09:00..Apr 5 09:00", "ada Apr 5 09:00..Apr 6 09:00", "grace Apr 6 09:00.."},
		},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			f := newFixture(t)
			f.users(ada, grace)
			f.clock.Set(tc.start)
			f.apply(rota("primary", tc.zone, ada, grace))
			for at := tc.start; at.Before(tc.end); at = at.Add(time.Hour) {
				f.advance(at)
			}
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			equal(t, "shifts", f.shifts("primary", loc), tc.want)
		})
	}
}

func day(d, h, m int) time.Time { return time.Date(2026, 1, d, h, m, 0, 0, time.UTC) }

// at returns a schedule that states when its definition takes over.
func at(s config.Schedule, when time.Time) config.Schedule {
	ts := config.Timestamp(when)
	s.EffectiveFrom = &ts
	return s
}

// TestDefinitionChangeLeavesTheShiftAlone edits a running schedule in the
// middle of a shift. Applying the change moves nothing: the person, the
// start of the shift and its end are as they were, and the rotation in
// force is still the old one. At the handoff the new order takes over,
// continued from the person who was on call, also across a restart.
func TestDefinitionChangeLeavesTheShiftAlone(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	f.apply(rota("primary", "UTC", ada, grace, linus))
	f.advance(day(3, 9, 0))
	before := f.position("primary")

	f.clock.Set(day(3, 15, 0))
	reversed := rota("primary", "UTC", linus, grace, ada)
	applied := f.apply(reversed)
	if c := applied.Changed; len(c) != 1 || c[0].Schedule != "primary" || !c[0].At.Equal(day(4, 9, 0)) || c[0].Stated || !c[0].New {
		t.Fatalf("Changed = %+v, want the change waiting for the handoff of the 4th", applied.Changed)
	}
	after := f.position("primary")
	if after.Email != grace || !after.ShiftStartedAt.Equal(before.ShiftStartedAt) || !after.NextHandoffAt.Equal(before.NextHandoffAt) ||
		!after.Definition.Equal(before.Definition) {
		t.Errorf("applying the change moved the position:\nbefore %+v\n after %+v", before, after)
	}
	if after.Pending == nil || after.Pending.Members[0] != linus || after.PendingAt != nil {
		t.Errorf("pending = %+v at %v, want the reversed rotation at the next handoff", after.Pending, after.PendingAt)
	}
	equal(t, "shifts after the change was applied", f.shifts("primary", time.UTC), []string{"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00.."})

	// Applying it again, as every reload and every replica does, is not news.
	if applied := f.apply(reversed); len(applied.Changed) != 1 || applied.Changed[0].New {
		t.Errorf("Changed on a second application = %+v, want it reported and not new", applied.Changed)
	}
	if done := f.advance(day(4, 8, 59)); len(done) != 0 {
		t.Errorf("the change took over early: %+v", done)
	}

	// The process restarts before the handoff. In the old order Linus
	// follows Grace; in the new one Ada does.
	f.store = f.restart()
	done := f.advance(day(4, 9, 0))
	if len(done) != 1 || !done[0].TookOver || done[0].From != grace || done[0].To != ada {
		t.Fatalf("handoff = %+v, want Grace to Ada with the new definition", done)
	}
	f.advance(day(6, 9, 0))
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 4 09:00",
		"ada Jan 4 09:00..Jan 5 09:00",
		"linus Jan 5 09:00..Jan 6 09:00",
		"grace Jan 6 09:00..",
	})
	if p := f.position("primary"); p.Pending != nil || p.Definition.Members[0] != linus {
		t.Errorf("after the handoff, in force %v and pending %v", p.Definition.Members, p.Pending)
	}
	if applied := f.apply(reversed); len(applied.Changed) != 0 {
		t.Errorf("Changed after the change took over = %+v", applied.Changed)
	}
}

// TestDefinitionChangeTakenBack applies a change and then the original
// definition again before the handoff. The rotation goes on as if nothing
// had been changed.
func TestDefinitionChangeTakenBack(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	original := rota("primary", "UTC", ada, grace, linus)
	f.apply(original)
	f.advance(day(3, 9, 0))
	f.apply(rota("primary", "UTC", linus, grace, ada))
	if applied := f.apply(original); len(applied.Changed) != 0 {
		t.Errorf("Changed = %+v after the change was taken back", applied.Changed)
	}
	if p := f.position("primary"); p.Pending != nil {
		t.Errorf("%+v is still pending", p.Pending)
	}
	if done := f.advance(day(4, 9, 0)); len(done) != 1 || done[0].To != linus || done[0].TookOver {
		t.Errorf("handoff = %+v, want Grace to Linus under the old order", done)
	}
}

// TestDefinitionChangeAtAStatedTime gives a change a time. Until then the
// old rotation runs. At that time the new one takes over, and whoever is on
// call stays on call; only someone the new rotation no longer has is
// relieved, at exactly the stated time however late the worker runs.
func TestDefinitionChangeAtAStatedTime(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	f.apply(rota("primary", "UTC", ada, grace, linus))
	f.advance(day(3, 9, 0))

	// From the 5th at 18:00 the handoff is at 20:00 and Linus has left.
	evening := rota("primary", "UTC", ada, grace)
	evening.Rotation.Handoff.Time = "20:00"
	f.clock.Set(day(3, 10, 0))
	applied := f.apply(at(evening, day(5, 18, 0)))
	if len(applied.Changed) != 1 || !applied.Changed[0].Stated || !applied.Changed[0].At.Equal(day(5, 18, 0)) {
		t.Fatalf("Changed = %+v, want a change stated for the 5th at 18:00", applied.Changed)
	}
	// Two ordinary handoffs come first; Linus is on call on the 5th.
	f.advance(day(5, 17, 59))
	equal(t, "before the stated time", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00..Jan 4 09:00", "linus Jan 4 09:00..Jan 5 09:00", "ada Jan 5 09:00..",
	})

	// Ada is in the new rotation: she stays, and her shift now ends at
	// 20:00, the first handoff of the new rotation.
	done := f.advance(day(5, 18, 0))
	if len(done) != 1 || !done[0].TookOver || done[0].ShiftBegan {
		t.Fatalf("at the stated time: %+v, want the definition to take over and no shift to begin", done)
	}
	if p := f.position("primary"); p.Email != ada || !p.ShiftStartedAt.Equal(day(5, 9, 0)) || !p.NextHandoffAt.Equal(day(5, 20, 0)) || p.Pending != nil {
		t.Errorf("position = %s since %s until %s, pending %v", p.Email, p.ShiftStartedAt, p.NextHandoffAt, p.Pending)
	}
	f.advance(day(6, 20, 0))

	// Ada is on call when a change that removes her takes over, and the
	// worker is half an hour late. Grace followed Ada, so Grace takes the
	// rest of the shift.
	without := rota("primary", "UTC", grace, linus)
	without.Rotation.Handoff.Time = "20:00"
	f.apply(at(without, day(7, 12, 0)))
	f.advance(day(7, 12, 30))
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00",
		"grace Jan 3 09:00..Jan 4 09:00",
		"linus Jan 4 09:00..Jan 5 09:00",
		"ada Jan 5 09:00..Jan 5 20:00",
		"grace Jan 5 20:00..Jan 6 20:00",
		"ada Jan 6 20:00..Jan 7 12:00",
		"grace Jan 7 12:00..",
	})
}

// TestStatedTimeInThePast applies a change whose stated time has passed. It
// takes over at once, and no shift is given a start in the past.
func TestStatedTimeInThePast(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	f.apply(rota("primary", "UTC", ada, grace, linus))
	f.advance(day(3, 9, 0))

	f.clock.Set(day(3, 15, 0))
	applied := f.apply(at(rota("primary", "UTC", ada, linus), day(2, 0, 0)))
	if len(applied.Changed) != 1 || !applied.Changed[0].At.Equal(day(3, 15, 0)) {
		t.Fatalf("Changed = %+v, want the change due now", applied.Changed)
	}
	// Later applications do not push the time further out.
	f.clock.Set(day(3, 15, 1))
	if applied := f.apply(at(rota("primary", "UTC", ada, linus), day(2, 0, 0))); len(applied.Changed) != 1 || applied.Changed[0].New || !applied.Changed[0].At.Equal(day(3, 15, 0)) {
		t.Errorf("Changed on a second application = %+v", applied.Changed)
	}
	f.advance(day(3, 15, 2))
	equal(t, "shifts", f.shifts("primary", time.UTC), []string{
		"ada Jan 1 12:00..Jan 3 09:00", "grace Jan 3 09:00..Jan 3 15:00", "linus Jan 3 15:00..",
	})
}

// positions dumps every position, to show that nothing wrote to them.
func (f *fixture) positions() string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT p::text FROM schedule_positions p ORDER BY schedule_id`)
	if err != nil {
		f.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

func (f *fixture) preview(until time.Time, schedules ...config.Schedule) map[string]schedule.Preview {
	f.t.Helper()
	list, err := f.store.Preview(context.Background(), schedules, until)
	if err != nil {
		f.t.Fatalf("Preview: %v", err)
	}
	out := map[string]schedule.Preview{}
	for _, p := range list {
		out[p.Schedule] = p
	}
	return out
}

func moves(p schedule.Preview) []string {
	var out []string
	for _, m := range p.Moves {
		short := func(s string) string {
			if s == "" {
				return "nobody"
			}
			return strings.TrimSuffix(s, "@example.com")
		}
		out = append(out, m.Start.UTC().Format("Jan 2 15:04")+".."+m.End.UTC().Format("Jan 2 15:04")+" "+short(m.Was)+">"+short(m.Becomes))
	}
	return out
}

// TestPreview asks what a configuration would do before it is applied, for
// every state a schedule can be in, and checks that asking changes nothing.
func TestPreview(t *testing.T) {
	f := newFixture(t)
	f.users(ada, grace, linus)
	f.apply(rota("primary", "UTC", ada, grace, linus), rota("secondary", "UTC", grace, ada), rota("old", "UTC", linus))
	f.advance(day(3, 9, 0))
	f.clock.Set(day(3, 15, 0))
	stored, shifts := f.positions(), strings.Join(f.shifts("primary", time.UTC), "\n")

	previews := f.preview(day(8, 9, 0),
		// Grace is on call until the 4th. Without Linus, Ada follows her.
		rota("primary", "UTC", ada, grace, "nobody@example.com"),
		rota("secondary", "UTC", grace, ada),
		rota("brand-new", "UTC", linus, ada),
	)
	if f.positions() != stored || strings.Join(f.shifts("primary", time.UTC), "\n") != shifts {
		t.Error("the preview changed what is stored")
	}

	p := previews["primary"]
	if p.Status != schedule.PreviewChanged || !p.TakesOverAt.Equal(day(4, 9, 0)) || p.Stated {
		t.Errorf("primary: %s, takes over %s, stated %v", p.Status, p.TakesOverAt, p.Stated)
	}
	equal(t, "primary moves", moves(p), []string{
		"Jan 4 09:00..Jan 5 09:00 linus>ada",
		"Jan 5 09:00..Jan 6 09:00 ada>grace",
		"Jan 6 09:00..Jan 7 09:00 grace>ada",
		"Jan 7 09:00..Jan 8 09:00 linus>grace",
	})
	if len(p.Skipped) != 1 || p.Skipped[0] != "nobody@example.com" {
		t.Errorf("primary skipped = %v", p.Skipped)
	}
	if p := previews["secondary"]; p.Status != schedule.PreviewUnchanged || len(p.Moves) != 0 || len(p.Upcoming) == 0 {
		t.Errorf("secondary: %s with %d moves and %d shifts", p.Status, len(p.Moves), len(p.Upcoming))
	}
	if p := previews["brand-new"]; p.Status != schedule.PreviewNew || len(p.Upcoming) == 0 || p.Upcoming[0].Member != linus || !p.Upcoming[0].Start.Equal(day(3, 15, 0)) {
		t.Errorf("brand-new: %s, upcoming %+v", p.Status, p.Upcoming)
	}
	if p := previews["old"]; p.Status != schedule.PreviewRemoved {
		t.Errorf("old: %s, want removed", p.Status)
	}

	// Once the change is applied, the same files are no longer a change:
	// the preview says it is waiting, and nothing more moves.
	changed := rota("primary", "UTC", ada, grace, "nobody@example.com")
	f.apply(changed, rota("secondary", "UTC", grace, ada))
	if p := f.preview(day(8, 9, 0), changed)["primary"]; p.Status != schedule.PreviewPending || len(p.Moves) != 0 || !p.TakesOverAt.Equal(day(4, 9, 0)) {
		t.Errorf("after applying: %s with moves %v", p.Status, moves(p))
	}
}

// TestPreviewIsWhatHappens previews a change, applies it, lets the time pass
// and compares the shifts that were recorded with the ones the preview
// showed. They must be the same, shift for shift.
func TestPreviewIsWhatHappens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func() config.Schedule
	}{
		{"members", func() config.Schedule { return rota("primary", "UTC", linus, ada) }},
		{"handoff time and zone", func() config.Schedule {
			s := rota("primary", "Australia/Sydney", grace, linus, ada)
			s.Rotation.Handoff.Time = "17:30"
			return s
		}},
		{"weekly, at a stated time", func() config.Schedule {
			s := rota("primary", "America/New_York", ada, linus)
			s.Rotation.ShiftLength, s.Rotation.Handoff.Day = config.Duration(config.Week), "friday"
			return at(s, day(6, 13, 0))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.users(ada, grace, linus)
			f.apply(rota("primary", "UTC", ada, grace, linus))
			f.advance(day(3, 9, 0))
			f.clock.Set(day(3, 15, 0))

			until := day(28, 0, 0)
			p := f.preview(until, tc.change())["primary"]
			var want []string
			for _, s := range p.Upcoming {
				want = append(want, strings.TrimSuffix(s.Member, "@example.com")+" "+s.Start.UTC().Format("Jan 2 15:04")+".."+s.End.UTC().Format("Jan 2 15:04"))
			}

			f.apply(tc.change())
			last := p.Upcoming[len(p.Upcoming)-1].End
			for now := day(3, 15, 0); !now.After(last); now = now.Add(7 * time.Hour) {
				f.advance(now)
			}
			f.advance(last)
			got := f.shifts("primary", time.UTC)
			// The preview begins with the shift in progress, and the record
			// goes one shift further: the one that began at the end.
			got = got[1 : len(got)-1]
			equal(t, "recorded shifts against the preview", got, want)
		})
	}
}
