package alert_test

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
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
	pool     *pgxpool.Pool
	store    *alert.Store
	clock    *clock
	sourceID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := postgrestest.NewMigrated(t)
	f := &fixture{pool: pool, store: alert.NewStore(pool), clock: &clock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}}
	f.store.SetClock(f.clock.Now)
	if err := pool.QueryRow(context.Background(), `INSERT INTO sources (name) VALUES ('test') RETURNING id::text`).Scan(&f.sourceID); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	return f
}

func (f *fixture) firing(fingerprint string) alert.Firing {
	return alert.Firing{
		SourceID:    f.sourceID,
		Fingerprint: fingerprint,
		Severity:    alert.SeverityCritical,
		Labels:      map[string]string{"alertname": "DiskFull", "host": "db1"},
		Annotations: map[string]string{"summary": "Disk is full"},
		Links:       map[string]string{alert.LinkGenerator: "https://prometheus.example.com/graph"},
	}
}

func (f *fixture) fire(t *testing.T, fingerprint string) alert.Alert {
	t.Helper()
	a, created, err := f.store.Fire(context.Background(), f.firing(fingerprint))
	if err != nil || !created {
		t.Fatalf("Fire = created %v, %v; want a new alert", created, err)
	}
	return a
}

func (f *fixture) timeline(t *testing.T, alertID string) []alert.Event {
	t.Helper()
	events, err := f.store.Timeline(context.Background(), alertID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	return events
}

func eventTypes(events []alert.Event) []alert.EventType {
	types := make([]alert.EventType, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func TestFireStoresTheAlert(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	started := f.clock.Now().Add(-10 * time.Minute)
	firing := f.firing("fp1")
	firing.StartsAt = started

	a, created, err := f.store.Fire(ctx, firing)
	if err != nil || !created {
		t.Fatalf("Fire = created %v, %v", created, err)
	}
	got, err := f.store.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, a) {
		t.Errorf("Get = %+v, want what Fire returned: %+v", got, a)
	}
	if got.SourceID != f.sourceID || got.Fingerprint != "fp1" || got.Status != alert.StatusFiring || got.Severity != alert.SeverityCritical {
		t.Errorf("alert = %+v", got)
	}
	if !reflect.DeepEqual(got.Labels, firing.Labels) || !reflect.DeepEqual(got.Annotations, firing.Annotations) || !reflect.DeepEqual(got.Links, firing.Links) {
		t.Errorf("labels = %v, annotations = %v, links = %v", got.Labels, got.Annotations, got.Links)
	}
	if !got.StartedAt.Equal(started) || !got.LastReceivedAt.Equal(f.clock.Now()) {
		t.Errorf("started_at = %v, last_received_at = %v", got.StartedAt, got.LastReceivedAt)
	}

	events := f.timeline(t, a.ID)
	if len(events) != 1 {
		t.Fatalf("timeline has %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != alert.EventFired || e.AlertID != a.ID || e.GroupID != a.GroupID || !e.OccurredAt.Equal(f.clock.Now()) {
		t.Errorf("event = %+v", e)
	}
	if e.Actor.Type != alert.ActorSource || e.Actor.ID != f.sourceID {
		t.Errorf("actor = %+v, want the source", e.Actor)
	}

	var groupStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM alert_groups WHERE id = $1`, a.GroupID).Scan(&groupStatus); err != nil || groupStatus != "open" {
		t.Errorf("group status = %q, %v; want open", groupStatus, err)
	}
}

func TestFireRejectsUnknownSeverity(t *testing.T) {
	f := newFixture(t)
	firing := f.firing("fp1")
	firing.Severity = "page"
	if _, _, err := f.store.Fire(context.Background(), firing); err == nil {
		t.Error("Fire accepted an unknown severity")
	}
}

func TestFireAgainRefreshesTheOpenAlert(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.fire(t, "fp1")
	if _, err := f.store.Apply(ctx, first.ID, alert.ActionAcknowledge, alert.System(), alert.Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	f.clock.Advance(time.Minute)
	again := f.firing("fp1")
	again.Severity = alert.SeverityWarning
	again.Annotations = map[string]string{"summary": "Disk is nearly full"}
	a, created, err := f.store.Fire(ctx, again)
	if err != nil || created {
		t.Fatalf("second Fire = created %v, %v; want the existing alert", created, err)
	}
	if a.ID != first.ID || a.Status != alert.StatusAcknowledged {
		t.Errorf("alert = %s %s, want %s still acknowledged", a.ID, a.Status, first.ID)
	}
	if a.Severity != alert.SeverityWarning || a.Annotations["summary"] != "Disk is nearly full" || !a.LastReceivedAt.Equal(f.clock.Now()) {
		t.Errorf("alert was not refreshed: %+v", a)
	}
	if got := eventTypes(f.timeline(t, a.ID)); !reflect.DeepEqual(got, []alert.EventType{alert.EventFired, alert.EventAcknowledged}) {
		t.Errorf("timeline = %v, want no event for the repeated delivery", got)
	}
}

func TestFireConcurrentlyOpensOneAlert(t *testing.T) {
	f := newFixture(t)
	const n = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[string]bool{}
		created int
	)
	for range n {
		wg.Go(func() {
			a, c, err := f.store.Fire(context.Background(), f.firing("same"))
			if err != nil {
				t.Errorf("Fire: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[a.ID] = true
			if c {
				created++
			}
		})
	}
	wg.Wait()
	if len(ids) != 1 || created != 1 {
		t.Errorf("%d concurrent deliveries opened %d alerts (%d reported created), want 1", n, len(ids), created)
	}
}

// TestTransitions walks every allowed transition through the store and checks
// the alert and the event each one leaves.
func TestTransitions(t *testing.T) {
	alice := alert.ChatUser("slack", "U123", "alice")
	hour := time.Hour
	tests := []struct {
		name      string
		setup     []alert.Action
		action    alert.Action
		actor     alert.Actor
		want      alert.Status
		wantEvent alert.EventType
	}{
		{name: "firing to acknowledged", action: alert.ActionAcknowledge, actor: alice, want: alert.StatusAcknowledged, wantEvent: alert.EventAcknowledged},
		{name: "firing to snoozed", action: alert.ActionSnooze, actor: alice, want: alert.StatusSnoozed, wantEvent: alert.EventSnoozed},
		{name: "firing to resolved", action: alert.ActionResolve, actor: alice, want: alert.StatusResolved, wantEvent: alert.EventResolved},
		{name: "acknowledged to snoozed", setup: []alert.Action{alert.ActionAcknowledge}, action: alert.ActionSnooze, actor: alice, want: alert.StatusSnoozed, wantEvent: alert.EventSnoozed},
		{name: "acknowledged to resolved", setup: []alert.Action{alert.ActionAcknowledge}, action: alert.ActionResolve, actor: alice, want: alert.StatusResolved, wantEvent: alert.EventResolved},
		{name: "snoozed to acknowledged", setup: []alert.Action{alert.ActionSnooze}, action: alert.ActionAcknowledge, actor: alice, want: alert.StatusAcknowledged, wantEvent: alert.EventAcknowledged},
		{name: "snoozed to firing", setup: []alert.Action{alert.ActionSnooze}, action: alert.ActionEndSnooze, actor: alert.System(), want: alert.StatusFiring, wantEvent: alert.EventSnoozeEnded},
		{name: "snoozed to resolved", setup: []alert.Action{alert.ActionSnooze}, action: alert.ActionResolve, actor: alice, want: alert.StatusResolved, wantEvent: alert.EventResolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			a := f.fire(t, "fp1")
			opts := func() alert.Options { return alert.Options{SnoozeUntil: f.clock.Now().Add(hour)} }
			for _, action := range tt.setup {
				if _, err := f.store.Apply(ctx, a.ID, action, alert.System(), opts()); err != nil {
					t.Fatalf("setup %s: %v", action, err)
				}
			}
			from, err := f.store.Get(ctx, a.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			f.clock.Advance(time.Minute)
			until := f.clock.Now().Add(hour)
			got, err := f.store.Apply(ctx, a.ID, tt.action, tt.actor, opts())
			if err != nil {
				t.Fatalf("Apply(%s): %v", tt.action, err)
			}
			if got.Status != tt.want {
				t.Fatalf("status = %s, want %s", got.Status, tt.want)
			}
			if (got.SnoozedUntil != nil) != (tt.want == alert.StatusSnoozed) || (got.SnoozedUntil != nil && !got.SnoozedUntil.Equal(until)) {
				t.Errorf("snoozed_until = %v", got.SnoozedUntil)
			}
			if (got.ResolvedAt != nil) != (tt.want == alert.StatusResolved) || (got.ResolvedAt != nil && !got.ResolvedAt.Equal(f.clock.Now())) {
				t.Errorf("resolved_at = %v", got.ResolvedAt)
			}
			if tt.want == alert.StatusAcknowledged && (got.AcknowledgedAt == nil || !got.AcknowledgedAt.Equal(f.clock.Now())) {
				t.Errorf("acknowledged_at = %v", got.AcknowledgedAt)
			}
			if tt.want == alert.StatusFiring && got.AcknowledgedAt != nil {
				t.Errorf("acknowledged_at = %v, want it cleared when the alert fires again", got.AcknowledgedAt)
			}

			events := f.timeline(t, a.ID)
			e := events[len(events)-1]
			if e.Type != tt.wantEvent || !e.OccurredAt.Equal(f.clock.Now()) {
				t.Errorf("last event = %s at %v, want %s at %v", e.Type, e.OccurredAt, tt.wantEvent, f.clock.Now())
			}
			if e.Actor != tt.actor {
				t.Errorf("actor = %+v, want %+v", e.Actor, tt.actor)
			}
			if e.Data["from"] != string(from.Status) || e.Data["to"] != string(tt.want) {
				t.Errorf("event data = %v, want from %s to %s", e.Data, from.Status, tt.want)
			}
		})
	}
}

func TestInvalidTransitionChangesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.fire(t, "fp1")
	if _, err := f.store.Apply(ctx, a.ID, alert.ActionResolve, alert.System(), alert.Options{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	before := f.timeline(t, a.ID)

	for _, action := range []alert.Action{alert.ActionAcknowledge, alert.ActionSnooze, alert.ActionResolve, alert.ActionEndSnooze} {
		_, err := f.store.Apply(ctx, a.ID, action, alert.System(), alert.Options{SnoozeUntil: f.clock.Now().Add(time.Hour)})
		var te *alert.TransitionError
		if !errors.As(err, &te) || te.From != alert.StatusResolved {
			t.Errorf("%s a resolved alert: err = %v, want a TransitionError from resolved", action, err)
		}
	}
	got, err := f.store.Get(ctx, a.ID)
	if err != nil || got.Status != alert.StatusResolved {
		t.Errorf("status = %s, %v; want resolved", got.Status, err)
	}
	if after := f.timeline(t, a.ID); len(after) != len(before) {
		t.Errorf("timeline grew from %d to %d events after refused actions", len(before), len(after))
	}
}

func TestSnoozeMustEndInTheFuture(t *testing.T) {
	f := newFixture(t)
	a := f.fire(t, "fp1")
	for _, until := range []time.Time{{}, f.clock.Now(), f.clock.Now().Add(-time.Minute)} {
		_, err := f.store.Apply(context.Background(), a.ID, alert.ActionSnooze, alert.System(), alert.Options{SnoozeUntil: until})
		if !errors.Is(err, alert.ErrSnoozeUntil) {
			t.Errorf("snooze until %v: err = %v, want ErrSnoozeUntil", until, err)
		}
	}
}

func TestSnoozedAlertFiresAgainWhenTheSnoozeEnds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.fire(t, "fp1")
	other := f.fire(t, "fp2")
	if _, err := f.store.Apply(ctx, a.ID, alert.ActionSnooze, alert.ChatUser("slack", "U1", "bob"), alert.Options{SnoozeUntil: f.clock.Now().Add(30 * time.Minute)}); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	if _, err := f.store.Apply(ctx, other.ID, alert.ActionSnooze, alert.System(), alert.Options{SnoozeUntil: f.clock.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	f.clock.Advance(29 * time.Minute)
	if n, err := f.store.ExpireSnoozes(ctx); err != nil || n != 0 {
		t.Fatalf("ExpireSnoozes before the end = %d, %v; want 0", n, err)
	}

	f.clock.Advance(time.Minute)
	// Several replicas run this at the same time; the snooze must end once.
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for range 4 {
		wg.Go(func() {
			n, err := f.store.ExpireSnoozes(ctx)
			if err != nil {
				t.Errorf("ExpireSnoozes: %v", err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		})
	}
	wg.Wait()
	if total != 1 {
		t.Errorf("ExpireSnoozes moved %d alerts in total, want 1", total)
	}

	got, err := f.store.Get(ctx, a.ID)
	if err != nil || got.Status != alert.StatusFiring || got.SnoozedUntil != nil {
		t.Errorf("alert = %s until %v, %v; want firing", got.Status, got.SnoozedUntil, err)
	}
	events := f.timeline(t, a.ID)
	if want := []alert.EventType{alert.EventFired, alert.EventSnoozed, alert.EventSnoozeEnded}; !reflect.DeepEqual(eventTypes(events), want) {
		t.Fatalf("timeline = %v, want %v", eventTypes(events), want)
	}
	if last := events[2]; last.Actor.Type != alert.ActorSystem || !last.OccurredAt.Equal(f.clock.Now()) {
		t.Errorf("snooze_ended event = %+v, want the system actor at the current time", last)
	}
	if still, err := f.store.Get(ctx, other.ID); err != nil || still.Status != alert.StatusSnoozed {
		t.Errorf("the alert with the longer snooze is %s, %v; want snoozed", still.Status, err)
	}
}

func TestRunSnoozeExpiry(t *testing.T) {
	f := newFixture(t)
	a := f.fire(t, "fp1")
	if _, err := f.store.Apply(context.Background(), a.ID, alert.ActionSnooze, alert.System(), alert.Options{SnoozeUntil: f.clock.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	f.clock.Advance(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.store.RunSnoozeExpiry(ctx, slog.New(slog.DiscardHandler), 10*time.Millisecond)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := f.store.Get(context.Background(), a.ID)
		if err == nil && got.Status == alert.StatusFiring {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alert is still %s, %v", got.Status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSnoozeExpiry did not stop when its context was cancelled")
	}
}

func TestResolveFromSource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.fire(t, "fp1")
	ended := f.clock.Now().Add(-time.Minute)

	got, found, err := f.store.ResolveFromSource(ctx, f.sourceID, "fp1", ended)
	if err != nil || !found {
		t.Fatalf("ResolveFromSource = found %v, %v", found, err)
	}
	if got.ID != a.ID || got.Status != alert.StatusResolved || got.ResolvedAt == nil || !got.ResolvedAt.Equal(ended) {
		t.Errorf("alert = %+v, want %s resolved at %v", got, a.ID, ended)
	}
	events := f.timeline(t, a.ID)
	if last := events[len(events)-1]; last.Type != alert.EventResolved || last.Actor.Type != alert.ActorSource || last.Actor.ID != f.sourceID {
		t.Errorf("last event = %+v, want resolved by the source", last)
	}
	var groupStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM alert_groups WHERE id = $1`, a.GroupID).Scan(&groupStatus); err != nil || groupStatus != "resolved" {
		t.Errorf("group status = %q, %v; want resolved", groupStatus, err)
	}

	// A second resolve finds nothing and is not an error.
	if _, found, err := f.store.ResolveFromSource(ctx, f.sourceID, "fp1", time.Time{}); err != nil || found {
		t.Errorf("second ResolveFromSource = found %v, %v; want not found", found, err)
	}

	// The same fingerprint firing again opens a new alert in a new group.
	again := f.fire(t, "fp1")
	if again.ID == a.ID || again.GroupID == a.GroupID {
		t.Errorf("firing after a resolve reused alert %s or its group", a.ID)
	}
}

func TestChatActorIsStoredForLaterLinking(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.fire(t, "fp1")
	if _, err := f.store.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.ChatUser("slack", "U42", "carol"), alert.Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var actorType, provider, externalID string
	var actorID *string
	err := f.pool.QueryRow(ctx, `SELECT actor_type, actor_id::text, actor_provider, actor_external_id
		FROM alert_events WHERE alert_id = $1 AND type = 'acknowledged'`, a.ID).Scan(&actorType, &actorID, &provider, &externalID)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if actorType != "user" || actorID != nil || provider != "slack" || externalID != "U42" {
		t.Errorf("event actor = %s %v %s %s, want an unlinked user identified as slack U42", actorType, actorID, provider, externalID)
	}
}

func TestObserverRunsInTheTransaction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var seen []alert.EventType
	fail := false
	f.store.Observe(func(ctx context.Context, tx pgx.Tx, ev alert.Event, a alert.Alert) error {
		if fail {
			return errors.New("observer failed")
		}
		seen = append(seen, ev.Type)
		// The observer sees the change it is told about.
		var status alert.Status
		if err := tx.QueryRow(ctx, `SELECT status FROM alerts WHERE id = $1`, a.ID).Scan(&status); err != nil {
			return err
		}
		if status != a.Status || ev.AlertID != a.ID || ev.ID == 0 {
			t.Errorf("observer got event %+v with alert status %s, the row says %s", ev, a.Status, status)
		}
		return nil
	})

	a := f.fire(t, "fp1")
	if _, err := f.store.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.System(), alert.Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if want := []alert.EventType{alert.EventFired, alert.EventAcknowledged}; !reflect.DeepEqual(seen, want) {
		t.Errorf("observer saw %v, want %v", seen, want)
	}

	fail = true
	if _, err := f.store.Apply(ctx, a.ID, alert.ActionResolve, alert.System(), alert.Options{}); err == nil {
		t.Fatal("Apply succeeded although the observer failed")
	}
	got, err := f.store.Get(ctx, a.ID)
	if err != nil || got.Status != alert.StatusAcknowledged {
		t.Errorf("status = %s, %v; want the failed change rolled back", got.Status, err)
	}
	if n := len(f.timeline(t, a.ID)); n != 2 {
		t.Errorf("timeline has %d events, want 2", n)
	}
}

func TestApplyToGroup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.fire(t, "fp1")
	actor := alert.ChatUser("slack", "U1", "dave")

	changed, err := f.store.ApplyToGroup(ctx, a.GroupID, alert.ActionAcknowledge, actor, alert.Options{})
	if err != nil || len(changed) != 1 || changed[0].ID != a.ID || changed[0].Status != alert.StatusAcknowledged {
		t.Fatalf("ApplyToGroup = %+v, %v", changed, err)
	}
	alerts, err := f.store.GroupAlerts(ctx, a.GroupID)
	if err != nil || len(alerts) != 1 || alerts[0].Status != alert.StatusAcknowledged {
		t.Errorf("GroupAlerts = %+v, %v", alerts, err)
	}

	changed, err = f.store.ApplyToGroup(ctx, a.GroupID, alert.ActionAcknowledge, actor, alert.Options{})
	var te *alert.TransitionError
	if !errors.As(err, &te) || te.From != alert.StatusAcknowledged || len(changed) != 0 {
		t.Errorf("second acknowledge = %+v, %v; want a TransitionError from acknowledged", changed, err)
	}
}

func TestNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const missing = "00000000-0000-0000-0000-000000000000"
	if _, err := f.store.Get(ctx, missing); !errors.Is(err, alert.ErrNotFound) {
		t.Errorf("Get: err = %v, want ErrNotFound", err)
	}
	if _, err := f.store.Apply(ctx, missing, alert.ActionResolve, alert.System(), alert.Options{}); !errors.Is(err, alert.ErrNotFound) {
		t.Errorf("Apply: err = %v, want ErrNotFound", err)
	}
	if _, err := f.store.ApplyToGroup(ctx, missing, alert.ActionResolve, alert.System(), alert.Options{}); !errors.Is(err, alert.ErrNotFound) {
		t.Errorf("ApplyToGroup: err = %v, want ErrNotFound", err)
	}
}
