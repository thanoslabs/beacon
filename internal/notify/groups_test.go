package notify_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
)

// clock is a settable time source shared by the store and the pipeline.
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

// groupFixture is a pipeline that tells one target about every group, with
// time in the test's hands. Nothing is delivered: the tests read the queue.
type groupFixture struct {
	*fixture
	p     *notify.Pipeline
	clock *clock
}

func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	f := &groupFixture{fixture: newFixture(t), clock: &clock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}}
	f.p = f.pipeline(fastConfig(), nil)
	f.p.SetClock(f.clock.Now)
	f.alerts.SetClock(f.clock.Now)
	f.p.SetTargets([]notify.Target{{IntegrationID: f.integrationID, Destination: "#alerts"}})
	f.alerts.Observe(f.p.Observe)
	return f
}

// join fires an alert into the DiskFull group: 30s wait, 5m interval.
func (f *groupFixture) join(host string) alert.Alert {
	f.t.Helper()
	labels := map[string]string{"alertname": "DiskFull", "host": host}
	key, shared := alert.GroupKey("", []string{"alertname"}, labels)
	a, _, err := f.alerts.Fire(context.Background(), alert.Firing{
		SourceID: f.sourceID, Fingerprint: host, Severity: alert.SeverityCritical, Labels: labels,
		Group: alert.Grouping{Key: key, Labels: shared, Wait: 30 * time.Second, Interval: 5 * time.Minute},
	})
	if err != nil {
		f.t.Fatalf("Fire: %v", err)
	}
	return a
}

func (f *groupFixture) resolveFromSource(host string) {
	f.t.Helper()
	if _, found, err := f.alerts.ResolveFromSource(context.Background(), f.sourceID, host, time.Time{}); err != nil || !found {
		f.t.Fatalf("ResolveFromSource(%s) = found %v, %v", host, found, err)
	}
}

// tick lets the pipeline queue what is due and returns how many groups it
// notified about.
func (f *groupFixture) tick() int {
	f.t.Helper()
	n, err := f.p.NotifyDueGroups(context.Background())
	if err != nil {
		f.t.Fatalf("NotifyDueGroups: %v", err)
	}
	return n
}

// queued lists the reasons of the group's notifications, oldest first.
func (f *groupFixture) queued(groupID string) string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT reason FROM notifications WHERE alert_group_id = $1 ORDER BY created_at, id`, groupID)
	if err != nil {
		f.t.Fatalf("read notifications: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatalf("read notifications: %v", err)
	}
	return strings.Join(got, ",")
}

// sent marks every queued notification as delivered, as a worker would, a
// second later.
func (f *groupFixture) sent() {
	f.t.Helper()
	f.clock.Advance(time.Second)
	_, err := f.pool.Exec(context.Background(), `UPDATE notifications SET status = 'sent', sent_at = $1, attempts = 1, external_ref = 'msg' WHERE status = 'pending'`, f.clock.Now())
	if err != nil {
		f.t.Fatalf("mark sent: %v", err)
	}
}

func (f *groupFixture) expect(what, groupID, want string) {
	f.t.Helper()
	if got := f.queued(groupID); got != want {
		f.t.Fatalf("%s: notifications = %q, want %q", what, got, want)
	}
}

// TestOneNotificationPerGroup follows a group from its first alert to its
// resolution: who is told what, and when.
func TestOneNotificationPerGroup(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()

	a := f.join("db1")
	f.join("db2")
	f.tick()
	f.expect("during the wait", a.GroupID, "")
	f.clock.Advance(30 * time.Second)
	if n := f.tick(); n != 1 {
		t.Fatalf("%d groups notified, want 1", n)
	}
	f.expect("two alerts, one notification", a.GroupID, "escalation_step")
	f.sent()

	// Three more alerts join over two minutes: one update, once the
	// interval has passed.
	for _, host := range []string{"db3", "db4", "db5"} {
		f.clock.Advance(40 * time.Second)
		f.join(host)
		f.tick()
	}
	f.expect("before the interval has passed", a.GroupID, "escalation_step")
	f.clock.Advance(3 * time.Minute)
	f.tick()
	f.expect("the interval has passed", a.GroupID, "escalation_step,group_update")
	f.sent()

	// A source resolves two of them, a minute apart: one update again.
	f.clock.Advance(time.Minute)
	f.resolveFromSource("db1")
	f.clock.Advance(time.Minute)
	f.resolveFromSource("db2")
	f.tick()
	f.expect("what a source reports waits for the interval", a.GroupID, "escalation_step,group_update")
	f.clock.Advance(3 * time.Minute)
	f.tick()
	f.expect("two alerts resolved", a.GroupID, "escalation_step,group_update,group_update")
	f.sent()

	// Someone acknowledges the group: three alerts change, and the update
	// does not wait for the interval.
	bob := alert.ChatUser("slack", "U1", "bob")
	if changed, err := f.alerts.ApplyToGroup(ctx, a.GroupID, alert.ActionAcknowledge, bob, alert.Options{}); err != nil || len(changed) != 3 {
		t.Fatalf("ApplyToGroup changed %d, %v", len(changed), err)
	}
	f.expect("acknowledged by a person", a.GroupID, "escalation_step,group_update,group_update,group_update")
	f.sent()

	// The sources resolve the rest. The group is over: one last notification.
	f.clock.Advance(10 * time.Minute)
	for _, host := range []string{"db3", "db4", "db5"} {
		f.resolveFromSource(host)
	}
	f.tick()
	f.expect("the group resolved", a.GroupID, "escalation_step,group_update,group_update,group_update,resolved")
	f.sent()
	f.clock.Advance(24 * time.Hour)
	if n := f.tick(); n != 0 {
		t.Errorf("%d groups notified after everything resolved", n)
	}
}

func TestRepeatedDeliveriesNotifyNobody(t *testing.T) {
	f := newGroupFixture(t)
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.sent()
	// Alertmanager sends the group again every repeat interval.
	for range 5 {
		f.clock.Advance(time.Hour)
		f.join("db1")
		if n := f.tick(); n != 0 {
			t.Fatalf("a repeated delivery made a notification due")
		}
	}
	f.expect("after five repeats", a.GroupID, "escalation_step")
}

func TestGroupThatResolvesDuringItsWaitNotifiesNobody(t *testing.T) {
	f := newGroupFixture(t)
	a := f.join("db1")
	f.clock.Advance(10 * time.Second)
	f.resolveFromSource("db1")
	f.clock.Advance(time.Hour)
	if n := f.tick(); n != 0 {
		t.Errorf("%d groups notified, want none", n)
	}
	f.expect("resolved before anyone was told", a.GroupID, "")
}

func TestActionsBeforeTheFirstNotificationWaitForIt(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	a := f.join("db1")
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.System(), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.expect("acknowledged during the wait", a.GroupID, "")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.expect("the first notification still goes out", a.GroupID, "escalation_step")
}

// TestPeopleAreToldAtOnce covers each change that does not wait for the
// group's interval, and the snooze that ends by itself.
func TestPeopleAreToldAtOnce(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	bob := alert.ChatUser("slack", "U1", "bob")
	a := f.join("db1")
	b := f.join("db2")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.sent()

	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionSnooze, bob, alert.Options{SnoozeUntil: f.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.expect("snoozed", a.GroupID, "escalation_step,group_update")
	f.sent()
	f.clock.Advance(time.Hour)
	if n, err := f.alerts.ExpireSnoozes(ctx); err != nil || n != 1 {
		t.Fatalf("ExpireSnoozes = %d, %v", n, err)
	}
	f.expect("the snooze ended", a.GroupID, "escalation_step,group_update,group_update")
	f.sent()
	// One of two alerts resolved by hand: the group is not over.
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionResolve, bob, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.expect("one alert resolved by hand", a.GroupID, "escalation_step,group_update,group_update,group_update")
	f.sent()
	if _, err := f.alerts.Apply(ctx, b.ID, alert.ActionResolve, bob, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.expect("the last alert resolved by hand", a.GroupID, "escalation_step,group_update,group_update,group_update,resolved")
	f.sent()
	// A refused action queues nothing.
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, bob, alert.Options{}); err == nil {
		t.Fatal("acknowledging a resolved alert succeeded")
	}
	f.clock.Advance(time.Hour)
	f.tick()
	f.expect("at the end", a.GroupID, "escalation_step,group_update,group_update,group_update,resolved")
}

// TestWaitingNotificationShowsLaterChanges: a notification that no worker
// has taken yet is built from the state at delivery, so changes made in the
// meantime need no notification of their own.
func TestWaitingNotificationShowsLaterChanges(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	bob := alert.ChatUser("slack", "U1", "bob")
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	// The first notification is still in the queue.
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, bob, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.expect("acknowledged before delivery", a.GroupID, "escalation_step")
	f.sent()
	// Now it is delivered: the next two changes share one update, which
	// takes the reason of the later one.
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionSnooze, bob, alert.Options{SnoozeUntil: f.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionResolve, bob, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.expect("two changes while one update waits", a.GroupID, "escalation_step,resolved")

	// A notification that a worker has taken may have been built already,
	// so a change after that gets its own.
	c := f.join("other")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.sent()
	if _, err := f.alerts.Apply(ctx, c.ID, alert.ActionAcknowledge, bob, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE notifications SET attempts = 1 WHERE alert_group_id = $1 AND status = 'pending'`, c.GroupID); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Second)
	if _, err := f.alerts.Apply(ctx, c.ID, alert.ActionResolve, bob, alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.expect("a change while an update is being delivered", c.GroupID, "escalation_step,group_update,resolved")
}

func TestUpdatesGoToEveryoneWhoWasTold(t *testing.T) {
	f := newGroupFixture(t)
	f.p.SetTargets([]notify.Target{{IntegrationID: f.integrationID, Destination: "#alerts"}, {IntegrationID: f.integrationID, Destination: "#platform"}})
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.sent()
	// The configuration changes: new groups go elsewhere. The group that
	// was told about is updated where it was told.
	f.p.SetTargets([]notify.Target{{IntegrationID: f.integrationID, Destination: "#elsewhere"}})
	f.clock.Advance(time.Hour)
	f.resolveFromSource("db1")
	f.tick()
	rows, err := f.pool.Query(context.Background(), `SELECT destination || ' ' || reason FROM notifications WHERE alert_group_id = $1 ORDER BY created_at, destination`, a.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	want := []string{"#alerts escalation_step", "#platform escalation_step", "#alerts resolved", "#platform resolved"}
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("notifications = %v, %v; want %v", got, err, want)
	}
}

// TestGroupTimersSurviveARestartAndFireOnce: the timers are rows, so a new
// process finds what was due, and several replicas queue it once.
func TestGroupTimersSurviveARestartAndFireOnce(t *testing.T) {
	f := newGroupFixture(t)
	const groups, replicas = 5, 6
	var ids []string
	for i := range groups {
		labels := map[string]string{"alertname": fmt.Sprintf("Alert%d", i)}
		key, shared := alert.GroupKey("", []string{"alertname"}, labels)
		a, _, err := f.alerts.Fire(context.Background(), alert.Firing{
			SourceID: f.sourceID, Fingerprint: labels["alertname"], Severity: alert.SeverityWarning, Labels: labels,
			Group: alert.Grouping{Key: key, Labels: shared, Wait: 30 * time.Second, Interval: 5 * time.Minute},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.GroupID)
	}
	f.clock.Advance(time.Minute)

	// The process that took the alerts is gone. These are new ones, with
	// nothing in memory, started at the same moment.
	var wg sync.WaitGroup
	for range replicas {
		store := alert.NewStore(f.pool)
		store.SetClock(f.clock.Now)
		p := notify.New(f.pool, store, slog.New(slog.DiscardHandler), fastConfig())
		p.SetClock(f.clock.Now)
		p.SetTargets([]notify.Target{{IntegrationID: f.integrationID, Destination: "#alerts"}})
		wg.Go(func() {
			if _, err := p.NotifyDueGroups(context.Background()); err != nil {
				t.Errorf("NotifyDueGroups: %v", err)
			}
		})
	}
	wg.Wait()
	for _, id := range ids {
		f.expect("after the restart", id, "escalation_step")
	}
}

// TestGroupIsDeliveredAsOneMessage runs the workers: a channel is handed the
// group with all its alerts, and later the same message to update.
func TestGroupIsDeliveredAsOneMessage(t *testing.T) {
	f := newGroupFixture(t)
	var mu sync.Mutex
	var deliveries []string
	f.p.Register(f.integrationID, channelFunc(func(_ context.Context, m notify.Message) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		firing := 0
		for _, a := range m.Alerts {
			if a.Status == alert.StatusFiring {
				firing++
			}
		}
		deliveries = append(deliveries, fmt.Sprintf("%s group=%s %s alerts=%d firing=%d previous=%q",
			m.Notification.Reason, m.Group.Labels["alertname"], m.Group.Status, len(m.Alerts), firing, m.PreviousRef))
		return "msg-1", nil
	}))
	a := f.join("db1")
	f.join("db2")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.run(f.p)
	f.waitStatus(a.GroupID, "sent")

	f.clock.Advance(time.Hour)
	f.join("db3")
	f.resolveFromSource("db1")
	f.tick()
	f.waitStatus(a.GroupID, "sent", "sent")
	f.resolveFromSource("db2")
	f.resolveFromSource("db3")
	f.clock.Advance(time.Hour)
	f.tick()
	f.waitStatus(a.GroupID, "sent", "sent", "sent")

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		`escalation_step group=DiskFull open alerts=2 firing=2 previous=""`,
		`group_update group=DiskFull open alerts=3 firing=2 previous="msg-1"`,
		`resolved group=DiskFull resolved alerts=3 firing=0 previous="msg-1"`,
	}
	if fmt.Sprint(deliveries) != fmt.Sprint(want) {
		t.Errorf("deliveries:\n%s\nwant:\n%s", strings.Join(deliveries, "\n"), strings.Join(want, "\n"))
	}
}
