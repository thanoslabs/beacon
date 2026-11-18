package alert_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thanoslabs/beacon/internal/alert"
)

// TestFireAndResolveConcurrently delivers one alert as firing and as resolved
// from many clients at once, as a source that retries or a pair of
// Alertmanagers does. Whatever order the deliveries land in, the source never
// has two open alerts with the fingerprint, no delivery fails, and the
// timeline of every alert that was opened is whole.
func TestFireAndResolveConcurrently(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const clients, rounds = 8, 6

	var created, resolved atomic.Int64
	var wg sync.WaitGroup
	for c := range clients {
		wg.Go(func() {
			for r := range rounds {
				// Half the deliveries fire and half resolve, interleaved.
				if (c+r)%2 == 0 {
					_, isNew, err := f.store.Fire(ctx, f.firing("same"))
					if err != nil {
						t.Errorf("Fire: %v", err)
					}
					if isNew {
						created.Add(1)
					}
				} else {
					_, found, err := f.store.ResolveFromSource(ctx, f.sourceID, "same", time.Time{})
					if err != nil {
						t.Errorf("ResolveFromSource: %v", err)
					}
					if found {
						resolved.Add(1)
					}
				}
			}
		})
	}
	wg.Wait()

	var alerts, open, groups, openGroups int64
	err := f.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM alerts), (SELECT count(*) FROM alerts WHERE status <> 'resolved'),
		(SELECT count(*) FROM alert_groups), (SELECT count(*) FROM alert_groups WHERE status = 'open')`).Scan(&alerts, &open, &groups, &openGroups)
	if err != nil {
		t.Fatal(err)
	}
	if created.Load() == 0 {
		t.Fatal("no delivery opened an alert")
	}
	if alerts != created.Load() {
		t.Errorf("%d alerts exist, but %d deliveries reported that they opened one", alerts, created.Load())
	}
	if open > 1 || alerts-open != resolved.Load() {
		t.Errorf("%d alerts are open and %d resolved, after %d deliveries resolved one; want at most one open", open, alerts-open, resolved.Load())
	}
	if openGroups != open {
		t.Errorf("%d groups are open for %d open alerts", openGroups, open)
	}

	// Every alert fired once, and resolved once if it is resolved.
	rows, err := f.pool.Query(ctx, `SELECT a.status,
			count(*) FILTER (WHERE e.type = 'fired'), count(*) FILTER (WHERE e.type = 'resolved'), count(*)
		FROM alerts a JOIN alert_events e ON e.alert_id = a.id GROUP BY a.id, a.status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var fired, resolves, events int
		if err := rows.Scan(&status, &fired, &resolves, &events); err != nil {
			t.Fatal(err)
		}
		wantResolves := map[string]int{"resolved": 1}[status]
		if fired != 1 || resolves != wantResolves || events != fired+resolves {
			t.Errorf("a %s alert has %d fired and %d resolved events among %d, want 1 and %d", status, fired, resolves, events, wantResolves)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestRepeatedDeliveriesWhileSomeoneResolves has a person resolve the alert
// while the source keeps repeating it. The repeat that lands after the
// resolution opens a new alert; none may fail, and none may reopen the
// resolved one.
func TestRepeatedDeliveriesWhileSomeoneResolves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.fire(t, "same")

	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for range 10 {
		wg.Go(func() {
			a, _, err := f.store.Fire(ctx, f.firing("same"))
			if err != nil {
				t.Errorf("Fire: %v", err)
				return
			}
			ids <- a.ID
		})
	}
	wg.Go(func() {
		if _, err := f.store.Apply(ctx, first.ID, alert.ActionResolve, alert.ChatUser("slack", "U1", "ada"), alert.Options{}); err != nil {
			t.Errorf("Apply: %v", err)
		}
	})
	wg.Wait()
	close(ids)

	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	delete(seen, first.ID)
	if len(seen) > 1 {
		t.Errorf("the repeats opened %d new alerts, want at most one", len(seen))
	}
	got, err := f.store.Get(ctx, first.ID)
	if err != nil || got.Status != alert.StatusResolved {
		t.Errorf("the alert that was resolved by hand is %s, %v", got.Status, err)
	}
	var open int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE status <> 'resolved'`).Scan(&open); err != nil || open > 1 {
		t.Errorf("%d alerts are open, %v; want at most one", open, err)
	}
}

// TestFingerprintsAreSeparatePerSource: the same fingerprint from two sources
// is two alerts, and resolving one leaves the other.
func TestFingerprintsAreSeparatePerSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var other string
	if err := f.pool.QueryRow(ctx, `INSERT INTO sources (name) VALUES ('other') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	a := f.fire(t, "same")
	firing := f.firing("same")
	firing.SourceID = other
	b, created, err := f.store.Fire(ctx, firing)
	if err != nil || !created || b.ID == a.ID {
		t.Fatalf("Fire from another source = %s created %v, %v; want a second alert", b.ID, created, err)
	}
	if _, found, err := f.store.ResolveFromSource(ctx, other, "same", time.Time{}); err != nil || !found {
		t.Fatalf("ResolveFromSource = found %v, %v", found, err)
	}
	if got, err := f.store.Get(ctx, a.ID); err != nil || got.Status != alert.StatusFiring {
		t.Errorf("the first source's alert is %s, %v; want it still firing", got.Status, err)
	}
}

// TestTheDatabaseRefusesASecondOpenAlert checks the guard that does not
// depend on Beacon's own locking: the unique index.
func TestTheDatabaseRefusesASecondOpenAlert(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.fire(t, "same")
	insert := func() error {
		_, err := f.pool.Exec(ctx, `INSERT INTO alerts (source_id, fingerprint, severity, group_id, started_at, last_received_at)
			VALUES ($1, 'same', 'critical', $2, now(), now())`, f.sourceID, a.GroupID)
		return err
	}
	var pgErr *pgconn.PgError
	if err := insert(); !errors.As(err, &pgErr) || pgErr.ConstraintName != "alerts_open_fingerprint_key" {
		t.Fatalf("a second open alert with the fingerprint: %v, want a violation of alerts_open_fingerprint_key", err)
	}
	// Once the first is resolved, the fingerprint is free again.
	if _, found, err := f.store.ResolveFromSource(ctx, f.sourceID, "same", time.Time{}); err != nil || !found {
		t.Fatalf("ResolveFromSource = found %v, %v", found, err)
	}
	if err := insert(); err != nil {
		t.Errorf("an open alert after the first resolved: %v", err)
	}
}

// TestResolveTimeFromTheSource: the source's time is kept, unless it lies in
// the future, where it would make the alert resolved before it is.
func TestResolveTimeFromTheSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tests := []struct {
		name string
		at   func(now time.Time) time.Time
		want func(now time.Time) time.Time
	}{
		{"not given", func(time.Time) time.Time { return time.Time{} }, func(now time.Time) time.Time { return now }},
		{"in the past", func(now time.Time) time.Time { return now.Add(-3 * time.Minute) }, func(now time.Time) time.Time { return now.Add(-3 * time.Minute) }},
		{"in the future", func(now time.Time) time.Time { return now.Add(time.Hour) }, func(now time.Time) time.Time { return now }},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := fmt.Sprintf("fp%d", i)
			f.fire(t, fp)
			now := f.clock.Now()
			a, found, err := f.store.ResolveFromSource(ctx, f.sourceID, fp, tt.at(now))
			if err != nil || !found {
				t.Fatalf("ResolveFromSource = found %v, %v", found, err)
			}
			if a.ResolvedAt == nil || !a.ResolvedAt.Equal(tt.want(now)) {
				t.Errorf("resolved at %v, want %v", a.ResolvedAt, tt.want(now))
			}
			events := f.timeline(t, a.ID)
			if last := events[len(events)-1]; last.Type != alert.EventResolved || !last.OccurredAt.Equal(now) {
				t.Errorf("the resolved event = %+v, want it at the time Beacon learned of it, %v", last, now)
			}
		})
	}
}
