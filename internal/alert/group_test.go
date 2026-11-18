package alert_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
)

func TestGroupKey(t *testing.T) {
	labels := map[string]string{"alertname": "DiskFull", "cluster": "eu", "host": "db1"}
	key, shared := alert.GroupKey("", []string{"alertname", "cluster"}, labels)
	if want := map[string]string{"alertname": "DiskFull", "cluster": "eu"}; !reflect.DeepEqual(shared, want) {
		t.Errorf("group labels = %v, want %v", shared, want)
	}
	same := func(scope string, by []string, l map[string]string) bool {
		k, _ := alert.GroupKey(scope, by, l)
		return k == key
	}
	if !same("", []string{"alertname", "cluster"}, map[string]string{"alertname": "DiskFull", "cluster": "eu", "host": "db2"}) {
		t.Error("alerts that agree on the group labels have different keys")
	}
	for name, other := range map[string]bool{
		"another value":   same("", []string{"alertname", "cluster"}, map[string]string{"alertname": "DiskFull", "cluster": "us"}),
		"a missing label": same("", []string{"alertname", "cluster"}, map[string]string{"alertname": "DiskFull"}),
		"other labels":    same("", []string{"alertname"}, labels),
		"another scope":   same("rule-1", []string{"alertname", "cluster"}, labels),
		"shifted values":  same("", []string{"alertname", "cluster"}, map[string]string{"alertname": "DiskFull/7:cluster=2:eu", "cluster": ""}),
	} {
		if other {
			t.Errorf("%s gives the same key", name)
		}
	}
	// A label that is missing counts as empty, and is left out of the labels.
	a, shared := alert.GroupKey("", []string{"alertname", "cluster"}, map[string]string{"alertname": "X"})
	b, _ := alert.GroupKey("", []string{"alertname", "cluster"}, map[string]string{"alertname": "X", "cluster": ""})
	if a != b || len(shared) != 1 {
		t.Errorf("a missing and an empty label differ, or the group labels hold it: %v", shared)
	}
	// No labels at all: one group for everything in the scope.
	all1, _ := alert.GroupKey("", nil, labels)
	all2, _ := alert.GroupKey("", nil, map[string]string{"alertname": "Other"})
	if all1 != all2 {
		t.Error("grouping by no label does not put every alert in one group")
	}
}

// grouped returns a firing alert that joins the DiskFull group.
func (f *fixture) grouped(fingerprint string) alert.Firing {
	firing := f.firing(fingerprint)
	firing.Labels = map[string]string{"alertname": "DiskFull", "host": fingerprint}
	key, shared := alert.GroupKey("", []string{"alertname"}, firing.Labels)
	firing.Group = alert.Grouping{Key: key, Labels: shared, Wait: 30 * time.Second, Interval: 5 * time.Minute}
	return firing
}

func (f *fixture) mustFire(t *testing.T, firing alert.Firing) alert.Alert {
	t.Helper()
	a, created, err := f.store.Fire(context.Background(), firing)
	if err != nil || !created {
		t.Fatalf("Fire = created %v, %v", created, err)
	}
	return a
}

// due hands every due group to the store and returns what it handed over, as
// "first", "update" or "resolved" per group.
func (f *fixture) due(t *testing.T) []string {
	t.Helper()
	var got []string
	for {
		found, err := f.store.NotifyDueGroup(context.Background(), func(_ context.Context, _ pgx.Tx, g alert.Group, _ []alert.Alert) (alert.Outcome, error) {
			switch {
			case g.NotifiedAt == nil:
				got = append(got, "first")
			case g.Status == alert.GroupResolved:
				got = append(got, "resolved")
			default:
				got = append(got, "update")
			}
			return alert.Outcome{Notified: true}, nil
		})
		if err != nil {
			t.Fatalf("NotifyDueGroup: %v", err)
		}
		if !found {
			return got
		}
	}
}

func (f *fixture) group(t *testing.T, id string) alert.Group {
	t.Helper()
	g, err := f.store.Group(context.Background(), id)
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	return g
}

func TestAlertsJoinTheirGroup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.mustFire(t, f.grouped("db1"))
	f.clock.Advance(10 * time.Second)
	b := f.mustFire(t, f.grouped("db2"))
	if a.GroupID != b.GroupID {
		t.Fatalf("two alerts with the same group labels are in groups %s and %s", a.GroupID, b.GroupID)
	}
	g := f.group(t, a.GroupID)
	if g.Status != alert.GroupOpen || !reflect.DeepEqual(g.Labels, map[string]string{"alertname": "DiskFull"}) {
		t.Errorf("group = %+v", g)
	}
	if want := a.StartedAt.Add(30 * time.Second); g.NotifyAt == nil || !g.NotifyAt.Equal(want) || g.NotifiedAt != nil {
		t.Errorf("notify at %v, notified at %v; want the first notification 30s after the group opened, %v", g.NotifyAt, g.NotifiedAt, want)
	}

	other := f.grouped("db3")
	other.Labels["alertname"] = "HighLoad"
	other.Group.Key, other.Group.Labels = alert.GroupKey("", []string{"alertname"}, other.Labels)
	if c := f.mustFire(t, other); c.GroupID == a.GroupID {
		t.Error("an alert with another alertname joined the group")
	}
	// An alert without a grouping gets a group to itself, notified at once.
	alone := f.fire(t, "alone")
	if g := f.group(t, alone.GroupID); alone.GroupID == a.GroupID || g.NotifyAt == nil || !g.NotifyAt.Equal(f.clock.Now()) {
		t.Errorf("ungrouped alert: group %+v", g)
	}

	// A repeated delivery does not move an alert, whatever grouping it comes
	// with.
	again := f.grouped("db1")
	again.Group = alert.Grouping{Key: "elsewhere"}
	if got, created, err := f.store.Fire(ctx, again); err != nil || created || got.GroupID != a.GroupID {
		t.Errorf("repeat = group %s created %v, %v; want it to stay in %s", got.GroupID, created, err, a.GroupID)
	}

	// The group resolves with its last alert, and the next alert opens a new
	// group.
	for _, fp := range []string{"db1", "db2"} {
		if g := f.group(t, a.GroupID); g.Status != alert.GroupOpen {
			t.Fatalf("the group is %s while %s is open", g.Status, fp)
		}
		if _, found, err := f.store.ResolveFromSource(ctx, f.sourceID, fp, time.Time{}); err != nil || !found {
			t.Fatalf("resolve %s: found %v, %v", fp, found, err)
		}
	}
	if g := f.group(t, a.GroupID); g.Status != alert.GroupResolved || g.ResolvedAt == nil {
		t.Errorf("the group is %s after its last alert resolved", g.Status)
	}
	if next := f.mustFire(t, f.grouped("db1")); next.GroupID == a.GroupID {
		t.Error("an alert joined a resolved group")
	}
}

// TestGroupTimers walks a group through its life with the clock in hand:
// the wait before the first notification, the interval between later ones,
// and what does and does not make one due.
func TestGroupTimers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	expect := func(what string, want ...string) {
		t.Helper()
		if got := f.due(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: due = %v, want %v", what, got, want)
		}
	}

	a := f.mustFire(t, f.grouped("db1"))
	expect("a new group")
	f.clock.Advance(29 * time.Second)
	f.mustFire(t, f.grouped("db2"))
	expect("an alert joined during the wait")
	f.clock.Advance(time.Second)
	expect("the wait is over", "first")
	expect("nothing new")
	notified := f.clock.Now()

	// Alerts that join a notified group make one update due, when the
	// interval since the last notification has passed.
	f.clock.Advance(time.Minute)
	f.mustFire(t, f.grouped("db3"))
	f.clock.Advance(time.Minute)
	f.mustFire(t, f.grouped("db4"))
	if g := f.group(t, a.GroupID); g.NotifyAt == nil || !g.NotifyAt.Equal(notified.Add(5*time.Minute)) {
		t.Fatalf("next notification at %v, want one interval after the last, %v", g.NotifyAt, notified.Add(5*time.Minute))
	}
	// A repeated delivery is no reason for a notification.
	if _, _, err := f.store.Fire(ctx, f.grouped("db1")); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(3*time.Minute - time.Second)
	expect("a second before the interval has passed")
	f.clock.Advance(time.Second)
	expect("the interval has passed", "update")

	// Long after the last notification, a change is due at once.
	f.clock.Advance(time.Hour)
	if _, _, err := f.store.ResolveFromSource(ctx, f.sourceID, "db1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	expect("a source resolved one alert", "update")

	// What a person does is not the timer's business: an observer tells it
	// at once.
	bob := alert.ChatUser("slack", "U1", "bob")
	if _, err := f.store.ApplyToGroup(ctx, a.GroupID, alert.ActionAcknowledge, bob, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	expect("someone acknowledged the group")

	// The sources resolve the rest: one notification, for the resolved group.
	for _, fp := range []string{"db2", "db3", "db4"} {
		if _, _, err := f.store.ResolveFromSource(ctx, f.sourceID, fp, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	expect("the group resolved", "resolved")
	f.clock.Advance(24 * time.Hour)
	expect("a resolved group stays quiet")
}

func TestGroupThatResolvesBeforeItsFirstNotificationIsNeverNotified(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.mustFire(t, f.grouped("db1"))
	f.mustFire(t, f.grouped("db2"))
	f.clock.Advance(10 * time.Second)
	if _, _, err := f.store.ResolveFromSource(ctx, f.sourceID, "db1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if g := f.group(t, a.GroupID); g.NotifyAt == nil {
		t.Fatal("the first notification was dropped while an alert is still open")
	}
	if _, err := f.store.ApplyToGroup(ctx, a.GroupID, alert.ActionResolve, alert.ChatUser("slack", "U1", "bob"), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	if g := f.group(t, a.GroupID); g.NotifyAt != nil || g.Status != alert.GroupResolved {
		t.Fatalf("group = %s, notify at %v; want resolved with nothing due", g.Status, g.NotifyAt)
	}
	f.clock.Advance(time.Hour)
	if got := f.due(t); len(got) != 0 {
		t.Errorf("due = %v, want nothing for a problem that went away before anyone was told", got)
	}
}

func TestResolvingAGroupByHandDropsWhatWasDue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.mustFire(t, f.grouped("db1"))
	f.clock.Advance(30 * time.Second)
	f.due(t)
	f.clock.Advance(time.Minute)
	f.mustFire(t, f.grouped("db2")) // makes an update due in four minutes
	if _, err := f.store.ApplyToGroup(ctx, a.GroupID, alert.ActionResolve, alert.ChatUser("slack", "U1", "bob"), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	if got := f.due(t); len(got) != 0 {
		t.Errorf("due = %v, want nothing: the resolution was told when it was made", got)
	}
}

// TestDueGroupsAreHandedOutOnce has several callers ask for due groups at
// the same time, as replicas do.
func TestDueGroupsAreHandedOutOnce(t *testing.T) {
	f := newFixture(t)
	const groups, callers = 6, 8
	for i := range groups {
		firing := f.grouped(fmt.Sprintf("db%d", i))
		firing.Group.Key = fmt.Sprintf("group-%d", i)
		f.mustFire(t, firing)
	}
	f.clock.Advance(time.Minute)

	var mu sync.Mutex
	handed := map[string]int{}
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			for {
				found, err := f.store.NotifyDueGroup(context.Background(), func(_ context.Context, _ pgx.Tx, g alert.Group, _ []alert.Alert) (alert.Outcome, error) {
					mu.Lock()
					defer mu.Unlock()
					handed[g.ID]++
					return alert.Outcome{Notified: true}, nil
				})
				if err != nil {
					t.Errorf("NotifyDueGroup: %v", err)
				}
				if !found {
					// Another caller may hold the rest; they are its to finish.
					return
				}
			}
		})
	}
	wg.Wait()
	if len(handed) != groups {
		t.Errorf("%d groups were handed out, want %d", len(handed), groups)
	}
	for id, n := range handed {
		if n != 1 {
			t.Errorf("group %s was handed out %d times", id, n)
		}
	}
}

func TestAGroupThatCannotBeNotifiedDoesNotHoldUpTheOthers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := f.mustFire(t, f.grouped("db1"))
	f.clock.Advance(time.Second)
	other := f.grouped("db2")
	other.Group.Key = "other"
	good := f.mustFire(t, other)
	f.clock.Advance(time.Minute)

	var handed []string
	notify := func(_ context.Context, tx pgx.Tx, g alert.Group, _ []alert.Alert) (alert.Outcome, error) {
		handed = append(handed, g.ID)
		if g.ID == bad.GroupID {
			// What was written for the group goes when it fails.
			if _, err := tx.Exec(ctx, `UPDATE alert_groups SET labels = '{"written": "by the failed attempt"}' WHERE id = $1`, g.ID); err != nil {
				return alert.Outcome{}, err
			}
			return alert.Outcome{}, errors.New("no luck")
		}
		return alert.Outcome{Notified: true}, nil
	}
	if found, err := f.store.NotifyDueGroup(ctx, notify); !found || err == nil {
		t.Fatalf("NotifyDueGroup = found %v, %v; want the failure", found, err)
	}
	if g := f.group(t, bad.GroupID); g.NotifiedAt != nil || g.NotifyAt == nil || !g.NotifyAt.Equal(f.clock.Now().Add(30*time.Second)) || g.Labels["written"] != "" {
		t.Errorf("after the failure: %+v; want it not notified, unchanged and due again in 30s", g)
	}
	if found, err := f.store.NotifyDueGroup(ctx, notify); !found || err != nil {
		t.Fatalf("NotifyDueGroup = found %v, %v; want the other group", found, err)
	}
	if found, _ := f.store.NotifyDueGroup(ctx, notify); found {
		t.Error("the failed group is due again at once")
	}
	if want := []string{bad.GroupID, good.GroupID}; !reflect.DeepEqual(handed, want) {
		t.Errorf("handed = %v, want %v", handed, want)
	}
	f.clock.Advance(30 * time.Second)
	if found, _ := f.store.NotifyDueGroup(ctx, notify); !found || handed[len(handed)-1] != bad.GroupID {
		t.Error("the failed group was not tried again after its wait")
	}
}

// TestAnAlertJoinsWhileTheLastOneResolves races a new alert against the
// resolution that would close its group. Either the alert joins and the
// group stays open, or the group resolves and the alert opens a new one; an
// open alert must never be left in a resolved group.
func TestAnAlertJoinsWhileTheLastOneResolves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for round := range 25 {
		first, second := fmt.Sprintf("a%d", round), fmt.Sprintf("b%d", round)
		f.mustFire(t, f.grouped(first))
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, _, err := f.store.Fire(ctx, f.grouped(second)); err != nil {
				t.Errorf("Fire: %v", err)
			}
		})
		wg.Go(func() {
			if _, _, err := f.store.ResolveFromSource(ctx, f.sourceID, first, time.Time{}); err != nil {
				t.Errorf("ResolveFromSource: %v", err)
			}
		})
		wg.Wait()
		var stranded, openGroups int
		err := f.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM alerts a JOIN alert_groups g ON g.id = a.group_id WHERE a.status <> 'resolved' AND g.status = 'resolved'),
			(SELECT count(*) FROM alert_groups WHERE status = 'open')`).Scan(&stranded, &openGroups)
		if err != nil {
			t.Fatal(err)
		}
		if stranded != 0 || openGroups != 1 {
			t.Fatalf("round %d: %d open alerts in resolved groups, %d open groups; want 0 and 1", round, stranded, openGroups)
		}
		if _, _, err := f.store.ResolveFromSource(ctx, f.sourceID, second, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAcknowledgingAGroupAcknowledgesItsAlerts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.mustFire(t, f.grouped("db1"))
	f.mustFire(t, f.grouped("db2"))
	c := f.mustFire(t, f.grouped("db3"))
	if _, err := f.store.Apply(ctx, c.ID, alert.ActionResolve, alert.System(), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	bob := alert.ChatUser("slack", "U1", "bob")
	changed, err := f.store.ApplyToGroup(ctx, a.GroupID, alert.ActionAcknowledge, bob, alert.Options{})
	if err != nil || len(changed) != 2 {
		t.Fatalf("ApplyToGroup changed %d alerts, %v; want the two that were firing", len(changed), err)
	}
	alerts, err := f.store.GroupAlerts(ctx, a.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]alert.Status{}
	for _, x := range alerts {
		states[x.Fingerprint] = x.Status
	}
	if want := map[string]alert.Status{"db1": alert.StatusAcknowledged, "db2": alert.StatusAcknowledged, "db3": alert.StatusResolved}; !reflect.DeepEqual(states, want) {
		t.Errorf("states = %v, want %v", states, want)
	}
	// An alert that joins afterwards is new: it fires.
	d := f.mustFire(t, f.grouped("db4"))
	if d.GroupID != a.GroupID || d.Status != alert.StatusFiring {
		t.Errorf("the new alert is %s in group %s", d.Status, d.GroupID)
	}
	// Acknowledging again takes the new one and leaves the rest.
	if changed, err := f.store.ApplyToGroup(ctx, a.GroupID, alert.ActionAcknowledge, bob, alert.Options{}); err != nil || len(changed) != 1 || changed[0].ID != d.ID {
		t.Errorf("second acknowledgement changed %v, %v; want only the new alert", changed, err)
	}
}

// TestOutcomeOfADueGroup: what the callback returns is what the group
// remembers.
func TestOutcomeOfADueGroup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.mustFire(t, f.grouped("db1"))
	b := f.mustFire(t, f.grouped("db2"))
	f.clock.Advance(30 * time.Second)

	// Not notified, and due again later: the group waits, and counts as
	// never notified.
	later := f.clock.Now().Add(time.Hour)
	var silenceID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id::text`).Scan(&silenceID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO silences (matchers, starts_at, ends_at, comment, created_by) VALUES ('[]', $1, $2, 'x', $3) RETURNING id::text`,
		f.clock.Now(), later, silenceID).Scan(&silenceID); err != nil {
		t.Fatal(err)
	}
	var handed []string
	found, err := f.store.NotifyDueGroup(ctx, func(_ context.Context, _ pgx.Tx, _ alert.Group, alerts []alert.Alert) (alert.Outcome, error) {
		for _, x := range alerts {
			handed = append(handed, x.Fingerprint)
		}
		return alert.Outcome{RetryAt: later, SilenceID: silenceID}, nil
	})
	slices.Sort(handed)
	if err != nil || !found || !reflect.DeepEqual(handed, []string{"db1", "db2"}) {
		t.Fatalf("NotifyDueGroup = found %v, %v, with alerts %v", found, err, handed)
	}
	g := f.group(t, a.GroupID)
	if g.NotifiedAt != nil || g.NotifyAt == nil || !g.NotifyAt.Equal(later) || g.SilenceID != silenceID || len(g.NotifiedAlerts) != 0 {
		t.Fatalf("after a deferral: %+v", g)
	}

	// An alert that joins meanwhile is due after its own wait, not when the
	// silence ends: it may be one the silence does not cover.
	f.clock.Advance(time.Minute)
	f.mustFire(t, f.grouped("db3"))
	if g := f.group(t, a.GroupID); g.NotifyAt == nil || !g.NotifyAt.Equal(f.clock.Now().Add(30*time.Second)) {
		t.Fatalf("after an alert joined a waiting group: due at %v, want in 30s", g.NotifyAt)
	}

	// The silence is ended early: the group is due at once.
	_, err = f.pool.Exec(ctx, `UPDATE alert_groups SET notify_at = $2 WHERE id = $1`, a.GroupID, later)
	if err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error { return f.store.WakeGroupsWaitingFor(ctx, tx, silenceID) })
	if g := f.group(t, a.GroupID); err != nil || g.NotifyAt == nil || !g.NotifyAt.Equal(f.clock.Now()) {
		t.Fatalf("after the silence was ended: due at %v, %v; want now", g.NotifyAt, err)
	}

	// Notified: the time, what was shown, and no silence to wait for.
	found, err = f.store.NotifyDueGroup(ctx, func(context.Context, pgx.Tx, alert.Group, []alert.Alert) (alert.Outcome, error) {
		return alert.Outcome{Notified: true, Shown: map[string]string{a.ID: alert.ShownOpen}}, nil
	})
	g = f.group(t, a.GroupID)
	if err != nil || !found || g.NotifiedAt == nil || g.NotifyAt != nil || g.SilenceID != "" || !reflect.DeepEqual(g.NotifiedAlerts, map[string]string{a.ID: alert.ShownOpen}) {
		t.Fatalf("after a notification: %+v, found %v, %v", g, found, err)
	}
	// What is shown later is added to what was shown.
	f.clock.Advance(time.Hour)
	f.mustFire(t, f.grouped("db4"))
	_, err = f.store.NotifyDueGroup(ctx, func(context.Context, pgx.Tx, alert.Group, []alert.Alert) (alert.Outcome, error) {
		return alert.Outcome{Notified: true, Shown: map[string]string{a.ID: alert.ShownResolved, b.ID: alert.ShownOpen}}, nil
	})
	if g := f.group(t, a.GroupID); err != nil || !reflect.DeepEqual(g.NotifiedAlerts, map[string]string{a.ID: alert.ShownResolved, b.ID: alert.ShownOpen}) {
		t.Errorf("shown = %v, %v", g.NotifiedAlerts, err)
	}
}
