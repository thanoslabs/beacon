package alert_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
)

// TestFirstResponseHappensOnce: the first time a person acknowledges or
// snoozes an alert is told to the observers as its first response, and
// nothing after it is, whatever the alert goes through. The alert is
// measured from the moment Beacon opened it, not from when its source says
// the problem began.
func TestFirstResponseHappensOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada := alert.ChatUser("slack", "U0ADA", "ada")
	var first []string
	f.store.Observe(func(_ context.Context, _ pgx.Tx, ev alert.Event, a alert.Alert) error {
		if ev.FirstResponse {
			first = append(first, a.Fingerprint+" "+string(ev.Type)+" after "+ev.OccurredAt.Sub(a.OpenedAt).String())
		}
		return nil
	})

	opened := f.clock.Now()
	firing := f.firing("acknowledged-first")
	firing.StartsAt = opened.Add(-3 * time.Hour)
	a, _, err := f.store.Fire(ctx, firing)
	if err != nil {
		t.Fatal(err)
	}
	if !a.OpenedAt.Equal(opened) || !a.StartedAt.Equal(firing.StartsAt) || a.FirstResponseAt != nil {
		t.Fatalf("a new alert: opened %s, started %s, first response %v", a.OpenedAt, a.StartedAt, a.FirstResponseAt)
	}
	// A repeated delivery opens nothing and moves nothing.
	f.clock.Advance(time.Minute)
	if a, _, _ = f.store.Fire(ctx, firing); !a.OpenedAt.Equal(opened) {
		t.Errorf("a repeated delivery moved opened_at to %s", a.OpenedAt)
	}

	apply := func(id string, action alert.Action, opts alert.Options) alert.Alert {
		t.Helper()
		a, err := f.store.Apply(ctx, id, action, ada, opts)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return a
	}
	f.clock.Advance(time.Minute)
	responded := f.clock.Now()
	a = apply(a.ID, alert.ActionAcknowledge, alert.Options{})
	if a.FirstResponseAt == nil || !a.FirstResponseAt.Equal(responded) {
		t.Fatalf("first response at %v, want %s", a.FirstResponseAt, responded)
	}
	// Snoozed, fired again when the snooze ran out, acknowledged again,
	// resolved: none of it is a first response, and the time stays.
	f.clock.Advance(time.Minute)
	apply(a.ID, alert.ActionSnooze, alert.Options{SnoozeUntil: f.clock.Now().Add(time.Hour)})
	f.clock.Advance(time.Hour)
	if n, err := f.store.ExpireSnoozes(ctx); n != 1 || err != nil {
		t.Fatalf("ExpireSnoozes = %d, %v", n, err)
	}
	if got, _ := f.store.Get(ctx, a.ID); got.AcknowledgedAt != nil || got.FirstResponseAt == nil || !got.FirstResponseAt.Equal(responded) {
		t.Errorf("after the snooze ended: acknowledged %v, first response %v", got.AcknowledgedAt, got.FirstResponseAt)
	}
	f.clock.Advance(time.Minute)
	apply(a.ID, alert.ActionAcknowledge, alert.Options{})
	a = apply(a.ID, alert.ActionResolve, alert.Options{})
	if !a.FirstResponseAt.Equal(responded) {
		t.Errorf("the first response moved to %s", a.FirstResponseAt)
	}

	// A snooze is a response too, and a resolution is not.
	b := f.fire(t, "snoozed-first")
	f.clock.Advance(30 * time.Second)
	apply(b.ID, alert.ActionSnooze, alert.Options{SnoozeUntil: f.clock.Now().Add(time.Hour)})
	c := f.fire(t, "resolved-unanswered")
	if c = apply(c.ID, alert.ActionResolve, alert.Options{}); c.FirstResponseAt != nil {
		t.Errorf("a resolution counted as a response at %s", c.FirstResponseAt)
	}

	want := []string{"acknowledged-first acknowledged after 2m0s", "snoozed-first snoozed after 30s"}
	if len(first) != len(want) || first[0] != want[0] || first[1] != want[1] {
		t.Errorf("first responses:\n got %q\nwant %q", first, want)
	}
}

// TestFirstResponseIsOneUnderConcurrency: when several people, or several
// replicas, acknowledge an alert at the same moment, one of them is first.
func TestFirstResponseIsOneUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var (
		mu    sync.Mutex
		first int
	)
	observe := func(_ context.Context, _ pgx.Tx, ev alert.Event, _ alert.Alert) error {
		if ev.FirstResponse {
			mu.Lock()
			first++
			mu.Unlock()
		}
		return nil
	}
	a := f.fire(t, "contested")
	var wg sync.WaitGroup
	for range 8 {
		// A store of its own, as another replica has.
		store := alert.NewStore(f.pool)
		store.SetClock(f.clock.Now)
		store.Observe(observe)
		wg.Go(func() {
			// All but one find the alert acknowledged already.
			_, _ = store.ApplyToGroup(ctx, a.GroupID, alert.ActionAcknowledge, alert.ChatUser("slack", "U0ADA", "ada"), alert.Options{})
		})
	}
	wg.Wait()
	if first != 1 {
		t.Errorf("%d first responses to one alert, want 1", first)
	}
}

func TestCountOpen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var team string
	if err := f.pool.QueryRow(ctx, `INSERT INTO teams (name) VALUES ('platform') RETURNING id::text`).Scan(&team); err != nil {
		t.Fatal(err)
	}
	fire := func(fingerprint string, severity alert.Severity, teamID string) alert.Alert {
		t.Helper()
		firing := f.firing(fingerprint)
		firing.Severity, firing.TeamID = severity, teamID
		a, _, err := f.store.Fire(ctx, firing)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if counts, err := f.store.CountOpen(ctx); err != nil || len(counts) != 0 {
		t.Fatalf("with no alert: %v, %v", counts, err)
	}
	fire("a", alert.SeverityCritical, team)
	fire("b", alert.SeverityCritical, team)
	acked := fire("c", alert.SeverityCritical, team)
	fire("d", alert.SeverityWarning, "")
	gone := fire("e", alert.SeverityInfo, "")
	someone := alert.ChatUser("slack", "U0ADA", "ada")
	if _, err := f.store.Apply(ctx, acked.ID, alert.ActionAcknowledge, someone, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Apply(ctx, gone.ID, alert.ActionResolve, someone, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	counts, err := f.store.CountOpen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[alert.OpenCount]bool{}
	for _, c := range counts {
		got[c] = true
	}
	for _, want := range []alert.OpenCount{
		{TeamID: team, Severity: alert.SeverityCritical, Status: alert.StatusFiring, Count: 2},
		{TeamID: team, Severity: alert.SeverityCritical, Status: alert.StatusAcknowledged, Count: 1},
		{TeamID: "", Severity: alert.SeverityWarning, Status: alert.StatusFiring, Count: 1},
	} {
		if !got[want] {
			t.Errorf("CountOpen has no %+v in %+v", want, counts)
		}
	}
	if len(counts) != 3 {
		t.Errorf("CountOpen = %+v, want three rows: a resolved alert is not open", counts)
	}
}
