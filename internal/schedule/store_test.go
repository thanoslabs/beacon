package schedule_test

import (
	"context"
	"fmt"
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
	f.cfg = config.Config{Teams: []config.Team{{Name: "platform"}}, Schedules: schedules}
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
