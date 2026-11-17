package notify_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
)

// channelFunc adapts a function to notify.Channel.
type channelFunc func(ctx context.Context, m notify.Message) (string, error)

func (f channelFunc) Send(ctx context.Context, m notify.Message) (string, error) { return f(ctx, m) }

// fastConfig keeps tests quick: short polls and short backoff.
func fastConfig() notify.Config {
	return notify.Config{
		Workers:      4,
		PollInterval: 10 * time.Millisecond,
		MaxAttempts:  4,
		SendTimeout:  5 * time.Second,
		BackoffBase:  20 * time.Millisecond,
		BackoffMax:   50 * time.Millisecond,
	}
}

type fixture struct {
	t             *testing.T
	pool          *pgxpool.Pool
	alerts        *alert.Store
	sourceID      string
	integrationID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := postgrestest.NewMigrated(t)
	f := &fixture{t: t, pool: pool, alerts: alert.NewStore(pool)}
	var err error
	if f.sourceID, err = provision.Ensure(context.Background(), pool, provision.Sources, "test", map[string]string{}); err != nil {
		t.Fatalf("Ensure source: %v", err)
	}
	if f.integrationID, err = provision.Ensure(context.Background(), pool, provision.Integrations, "chat", map[string]string{}); err != nil {
		t.Fatalf("Ensure integration: %v", err)
	}
	return f
}

// pipeline returns a pipeline with ch registered for the fixture's
// integration.
func (f *fixture) pipeline(cfg notify.Config, ch notify.Channel) *notify.Pipeline {
	p := notify.New(f.pool, f.alerts, slog.New(slog.DiscardHandler), cfg)
	if ch != nil {
		p.Register(f.integrationID, ch)
	}
	return p
}

// run starts p and returns a function that stops it and waits for it.
func (f *fixture) run(p *notify.Pipeline) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				f.t.Error("the pipeline did not stop")
			}
		})
	}
	f.t.Cleanup(stop)
	return stop
}

// fire opens an alert and returns it.
func (f *fixture) fire(fingerprint string) alert.Alert {
	f.t.Helper()
	a, _, err := f.alerts.Fire(context.Background(), alert.Firing{
		SourceID: f.sourceID, Fingerprint: fingerprint, Severity: alert.SeverityCritical,
		Labels: map[string]string{"alertname": fingerprint},
	})
	if err != nil {
		f.t.Fatalf("Fire: %v", err)
	}
	return a
}

// enqueue queues one notification about the group to "#alerts".
func (f *fixture) enqueue(p *notify.Pipeline, groupID string, reason notify.Reason) {
	f.t.Helper()
	err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		return p.Enqueue(context.Background(), tx, notify.Notification{GroupID: groupID, IntegrationID: f.integrationID, Destination: "#alerts", Reason: reason})
	})
	if err != nil {
		f.t.Fatalf("Enqueue: %v", err)
	}
}

type row struct {
	status   string
	attempts int
	ref      *string
	next     time.Time
}

func (f *fixture) rows(groupID string) []row {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT status, attempts, external_ref, next_attempt_at FROM notifications
		WHERE alert_group_id = $1 ORDER BY created_at, id`, groupID)
	if err != nil {
		f.t.Fatalf("read notifications: %v", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.status, &x.attempts, &x.ref, &x.next)
	})
	if err != nil {
		f.t.Fatalf("read notifications: %v", err)
	}
	return out
}

// waitFor polls until cond is true.
func (f *fixture) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fixture) waitStatus(groupID string, want ...string) []row {
	f.t.Helper()
	var got []row
	f.waitFor(fmt.Sprintf("notifications of the group to be %v", want), func() bool {
		got = f.rows(groupID)
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i].status != want[i] {
				return false
			}
		}
		return true
	})
	return got
}

func (f *fixture) outcomes(groupID string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT a.outcome || COALESCE(': ' || a.error, '') FROM notification_attempts a
		JOIN notifications n ON n.id = a.notification_id WHERE n.alert_group_id = $1 ORDER BY a.id`, groupID)
	if err != nil {
		f.t.Fatalf("read attempts: %v", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatalf("read attempts: %v", err)
	}
	return out
}

// notificationEvents returns the notification events on the alert's timeline.
func (f *fixture) notificationEvents(alertID string) []alert.Event {
	f.t.Helper()
	events, err := f.alerts.Timeline(context.Background(), alertID)
	if err != nil {
		f.t.Fatalf("Timeline: %v", err)
	}
	var out []alert.Event
	for _, e := range events {
		if e.Type == notify.EventSent || e.Type == notify.EventFailed {
			out = append(out, e)
		}
	}
	return out
}

func TestDeliver(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	got := make(chan notify.Message, 1)
	p := f.pipeline(fastConfig(), channelFunc(func(_ context.Context, m notify.Message) (string, error) {
		got <- m
		return "ref-1", nil
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)

	m := <-got
	if m.Notification.GroupID != a.GroupID || m.Notification.Destination != "#alerts" || m.Notification.Reason != notify.ReasonEscalationStep || m.Notification.Attempts != 1 {
		t.Errorf("notification = %+v", m.Notification)
	}
	if len(m.Alerts) != 1 || m.Alerts[0].ID != a.ID || m.Alerts[0].Status != alert.StatusFiring {
		t.Errorf("alerts = %+v", m.Alerts)
	}
	if len(m.Events) != 1 || m.Events[0].Type != alert.EventFired {
		t.Errorf("events = %+v", m.Events)
	}
	if m.PreviousRef != "" {
		t.Errorf("previous ref = %q, want none for the first message", m.PreviousRef)
	}

	rows := f.waitStatus(a.GroupID, "sent")
	if rows[0].attempts != 1 || rows[0].ref == nil || *rows[0].ref != "ref-1" {
		t.Errorf("notification row = %+v", rows[0])
	}
	if got := f.outcomes(a.GroupID); len(got) != 1 || got[0] != "success" {
		t.Errorf("attempts = %v", got)
	}
	events := f.notificationEvents(a.ID)
	if len(events) != 1 || events[0].Type != notify.EventSent || events[0].Actor.Type != alert.ActorSystem || events[0].OccurredAt.IsZero() {
		t.Fatalf("timeline events = %+v", events)
	}
	if d := events[0].Data; d["destination"] != "#alerts" || d["outcome"] != "success" || d["attempt"] != float64(1) || d["reason"] != "escalation_step" {
		t.Errorf("event data = %v", d)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	var calls atomic.Int32
	p := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		if n := calls.Add(1); n < 3 {
			return "", fmt.Errorf("service unavailable (%d)", n)
		}
		return "ref", nil
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)

	rows := f.waitStatus(a.GroupID, "sent")
	if rows[0].attempts != 3 {
		t.Errorf("attempts = %d, want 3", rows[0].attempts)
	}
	want := []string{"retryable_error: service unavailable (1)", "retryable_error: service unavailable (2)", "success"}
	if got := f.outcomes(a.GroupID); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("attempts = %v, want %v", got, want)
	}
	events := f.notificationEvents(a.ID)
	if len(events) != 3 || events[0].Type != notify.EventFailed || events[1].Type != notify.EventFailed || events[2].Type != notify.EventSent {
		t.Fatalf("timeline events = %+v", events)
	}
	if d := events[0].Data; d["will_retry"] != true || d["error"] != "service unavailable (1)" || d["outcome"] != "retryable_error" {
		t.Errorf("first failure event data = %v", d)
	}
}

func TestGiveUpAfterMaxAttempts(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	var calls atomic.Int32
	p := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		calls.Add(1)
		return "", errors.New("still down")
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)

	rows := f.waitStatus(a.GroupID, "failed")
	if rows[0].attempts != 4 {
		t.Errorf("attempts = %d, want the limit of 4", rows[0].attempts)
	}
	time.Sleep(150 * time.Millisecond)
	if n := calls.Load(); n != 4 {
		t.Errorf("the channel was called %d times, want 4 and no more after giving up", n)
	}
	events := f.notificationEvents(a.ID)
	if len(events) != 4 {
		t.Fatalf("timeline has %d notification events, want 4", len(events))
	}
	if last := events[3].Data; last["will_retry"] != false || last["attempt"] != float64(4) {
		t.Errorf("last failure event data = %v", last)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	var calls atomic.Int32
	p := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		calls.Add(1)
		return "", notify.Permanent(errors.New("channel_not_found"))
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)

	rows := f.waitStatus(a.GroupID, "failed")
	time.Sleep(100 * time.Millisecond)
	if rows[0].attempts != 1 || calls.Load() != 1 {
		t.Errorf("attempts = %d, calls = %d; want 1", rows[0].attempts, calls.Load())
	}
	if got := f.outcomes(a.GroupID); len(got) != 1 || got[0] != "permanent_error: channel_not_found" {
		t.Errorf("attempts = %v", got)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	var calls atomic.Int32
	p := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		calls.Add(1)
		return "", notify.RetryAfter(errors.New("rate limited"), time.Hour)
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	start := time.Now()
	f.run(p)

	f.waitFor("the first attempt", func() bool { return len(f.outcomes(a.GroupID)) == 1 })
	time.Sleep(150 * time.Millisecond)
	rows := f.rows(a.GroupID)
	if rows[0].status != "pending" || calls.Load() != 1 {
		t.Errorf("status = %s after %d calls, want pending after 1", rows[0].status, calls.Load())
	}
	if wait := rows[0].next.Sub(start); wait < 59*time.Minute {
		t.Errorf("next attempt is in %v, want about the hour the channel asked for", wait)
	}
}

func TestNoChannelForIntegration(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	p := f.pipeline(fastConfig(), nil)
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)
	f.waitStatus(a.GroupID, "failed")
	if got := f.outcomes(a.GroupID); len(got) != 1 || !strings.Contains(got[0], "no channel is configured") {
		t.Errorf("attempts = %v", got)
	}
}

func TestChannelPanicIsAFailedAttempt(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	var calls atomic.Int32
	p := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return "ref", nil
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)
	f.waitStatus(a.GroupID, "sent")
	if got := f.outcomes(a.GroupID); len(got) != 2 || got[0] != "retryable_error: channel panicked: boom" {
		t.Errorf("attempts = %v", got)
	}
}

func TestSendTimeout(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	cfg := fastConfig()
	cfg.SendTimeout = 50 * time.Millisecond
	cfg.MaxAttempts = 1
	p := f.pipeline(cfg, channelFunc(func(ctx context.Context, _ notify.Message) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)
	f.waitStatus(a.GroupID, "failed")
	if got := f.outcomes(a.GroupID); len(got) != 1 || !strings.Contains(got[0], "deadline exceeded") {
		t.Errorf("attempts = %v", got)
	}
}

// TestQueueSurvivesARestart checks that what was queued while nothing was
// running is delivered by the next process.
func TestQueueSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	f.enqueue(f.pipeline(fastConfig(), nil), a.GroupID, notify.ReasonEscalationStep)

	var calls atomic.Int32
	next := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		calls.Add(1)
		return "ref", nil
	}))
	f.run(next)
	f.waitStatus(a.GroupID, "sent")
	if calls.Load() != 1 {
		t.Errorf("the channel was called %d times, want 1", calls.Load())
	}
}

// TestWorkerDiesMidDelivery stands in for a process that claimed a
// notification and was killed before recording the attempt. The notification
// must come back once the claim runs out, and be delivered once.
func TestWorkerDiesMidDelivery(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")

	// The pipeline's clock is moved instead of waiting for the real claim
	// time, which is SendTimeout plus a margin.
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }

	dead := f.pipeline(fastConfig(), nil)
	dead.SetClock(clock)
	f.enqueue(dead, a.GroupID, notify.ReasonEscalationStep)
	n, ok, err := dead.Claim(context.Background())
	if err != nil || !ok || n.Attempts != 1 {
		t.Fatalf("Claim = %+v, %v, %v", n, ok, err)
	}
	// It is never recorded: the process is gone.

	var calls atomic.Int32
	next := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		calls.Add(1)
		return "ref", nil
	}))
	next.SetClock(clock)
	f.run(next)

	// While the claim holds, nobody else takes the notification.
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("the notification was delivered while another worker held its claim")
	}
	if rows := f.rows(a.GroupID); rows[0].status != "pending" {
		t.Fatalf("status = %s, want pending", rows[0].status)
	}

	offset.Store(int64(time.Minute))
	rows := f.waitStatus(a.GroupID, "sent")
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 1 || rows[0].attempts != 2 {
		t.Errorf("calls = %d, attempts = %d; want one delivery, counted as the second attempt", calls.Load(), rows[0].attempts)
	}
}

// TestShutdownFinishesDeliveriesInProgress checks that stopping the pipeline
// neither loses nor repeats a delivery that was under way.
func TestShutdownFinishesDeliveriesInProgress(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	p := f.pipeline(fastConfig(), channelFunc(func(ctx context.Context, _ notify.Message) (string, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return "ref", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	stop := f.run(p)

	<-started
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("the pipeline stopped while a delivery was in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-stopped

	rows := f.rows(a.GroupID)
	if rows[0].status != "sent" || rows[0].attempts != 1 || calls.Load() != 1 {
		t.Errorf("after shutdown: status = %s, attempts = %d, calls = %d; want sent once", rows[0].status, rows[0].attempts, calls.Load())
	}
}

// TestManyWorkersSendEachNotificationOnce runs three pipelines, as three
// replicas would, against one queue.
func TestManyWorkersSendEachNotificationOnce(t *testing.T) {
	f := newFixture(t)
	const groups, perGroup = 40, 3

	var mu sync.Mutex
	sends := map[string]int{}
	inFlight := map[string]int{}
	overlaps := 0
	ch := channelFunc(func(_ context.Context, m notify.Message) (string, error) {
		mu.Lock()
		sends[m.Notification.ID]++
		inFlight[m.Notification.GroupID]++
		if inFlight[m.Notification.GroupID] > 1 {
			overlaps++
		}
		mu.Unlock()
		time.Sleep(time.Duration(len(m.Notification.ID)%3) * time.Millisecond)
		mu.Lock()
		inFlight[m.Notification.GroupID]--
		mu.Unlock()
		return "ref-" + m.Notification.ID, nil
	})

	enqueuer := f.pipeline(fastConfig(), nil)
	for i := range groups {
		a := f.fire(fmt.Sprintf("alert-%d", i))
		for range perGroup {
			f.enqueue(enqueuer, a.GroupID, notify.ReasonGroupUpdate)
		}
	}
	for range 3 {
		f.run(f.pipeline(fastConfig(), ch))
	}

	f.waitFor("every notification to be sent", func() bool {
		var pending int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE status <> 'sent'`).Scan(&pending); err != nil {
			t.Fatalf("count: %v", err)
		}
		return pending == 0
	})
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(sends) != groups*perGroup {
		t.Errorf("%d notifications were sent, want %d", len(sends), groups*perGroup)
	}
	for id, n := range sends {
		if n != 1 {
			t.Errorf("notification %s was sent %d times", id, n)
		}
	}
	if overlaps != 0 {
		t.Errorf("%d deliveries overlapped another delivery about the same group", overlaps)
	}
	var attempts int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_attempts`).Scan(&attempts); err != nil || attempts != groups*perGroup {
		t.Errorf("%d attempts recorded, %v; want %d", attempts, err, groups*perGroup)
	}
}

// TestUpdatesWaitForTheMessageTheyUpdate checks the order of notifications
// about one group: the second is not sent until the first is, and is told
// which message to update.
func TestUpdatesWaitForTheMessageTheyUpdate(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	other := f.fire("Unrelated")

	var mu sync.Mutex
	var log []string
	var firstCalls int
	ch := channelFunc(func(_ context.Context, m notify.Message) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if m.Notification.GroupID == a.GroupID && m.Notification.Reason == notify.ReasonEscalationStep {
			firstCalls++
			if firstCalls < 3 {
				return "", errors.New("not yet")
			}
		}
		log = append(log, fmt.Sprintf("%s %s previous=%q status=%s", m.Alerts[0].Labels["alertname"], m.Notification.Reason, m.PreviousRef, m.Alerts[0].Status))
		return "msg-" + m.Alerts[0].Labels["alertname"], nil
	})
	p := f.pipeline(fastConfig(), ch)
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	if _, err := f.alerts.Apply(context.Background(), a.ID, alert.ActionAcknowledge, alert.System(), alert.Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	f.enqueue(p, a.GroupID, notify.ReasonGroupUpdate)
	f.enqueue(p, other.GroupID, notify.ReasonEscalationStep)
	f.run(p)

	f.waitStatus(a.GroupID, "sent", "sent")
	f.waitStatus(other.GroupID, "sent")
	mu.Lock()
	defer mu.Unlock()
	// The unrelated group is not held up by the retries; the update is.
	want := []string{
		`Unrelated escalation_step previous="" status=firing`,
		`DiskFull escalation_step previous="" status=acknowledged`,
		`DiskFull group_update previous="msg-DiskFull" status=acknowledged`,
	}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("deliveries:\n%s\nwant:\n%s", strings.Join(log, "\n"), strings.Join(want, "\n"))
	}
}

func TestNotifyAll(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p := f.pipeline(fastConfig(), nil)
	f.alerts.Observe(p.NotifyAll([]notify.Target{{IntegrationID: f.integrationID, Destination: "#alerts"}}))

	reasons := func(groupID string) string {
		rows, err := f.pool.Query(ctx, `SELECT reason FROM notifications WHERE alert_group_id = $1 ORDER BY created_at, id`, groupID)
		if err != nil {
			t.Fatalf("read notifications: %v", err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read notifications: %v", err)
		}
		return strings.Join(got, ",")
	}

	a := f.fire("DiskFull")
	if got := reasons(a.GroupID); got != "escalation_step" {
		t.Fatalf("after firing: %q", got)
	}
	// A repeated delivery of the same alert is not a state change.
	f.fire("DiskFull")
	steps := []struct {
		action alert.Action
		opts   alert.Options
	}{
		{alert.ActionSnooze, alert.Options{SnoozeUntil: time.Now().Add(50 * time.Millisecond)}},
		{alert.ActionAcknowledge, alert.Options{}},
		{alert.ActionResolve, alert.Options{}},
	}
	for _, s := range steps {
		if _, err := f.alerts.Apply(ctx, a.ID, s.action, alert.System(), s.opts); err != nil {
			t.Fatalf("%s: %v", s.action, err)
		}
	}
	if got, want := reasons(a.GroupID), "escalation_step,group_update,group_update,resolved"; got != want {
		t.Errorf("notifications = %q, want %q", got, want)
	}

	// A snooze that ends is a state change too.
	b := f.fire("Other")
	if _, err := f.alerts.Apply(ctx, b.ID, alert.ActionSnooze, alert.System(), alert.Options{SnoozeUntil: time.Now().Add(10 * time.Millisecond)}); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if n, err := f.alerts.ExpireSnoozes(ctx); err != nil || n != 1 {
		t.Fatalf("ExpireSnoozes = %d, %v", n, err)
	}
	if got, want := reasons(b.GroupID), "escalation_step,group_update,group_update"; got != want {
		t.Errorf("notifications = %q, want %q", got, want)
	}

	// A refused action queues nothing: the notification is part of the
	// transaction that changes the state.
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.System(), alert.Options{}); err == nil {
		t.Fatal("acknowledging a resolved alert succeeded")
	}
	if got, want := reasons(a.GroupID), "escalation_step,group_update,group_update,resolved"; got != want {
		t.Errorf("notifications after a refused action = %q, want %q", got, want)
	}
}

func TestEnqueueRollsBackWithItsTransaction(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	p := f.pipeline(fastConfig(), nil)
	err := pgx.BeginFunc(context.Background(), f.pool, func(tx pgx.Tx) error {
		if err := p.Enqueue(context.Background(), tx, notify.Notification{GroupID: a.GroupID, IntegrationID: f.integrationID, Destination: "#alerts", Reason: notify.ReasonResolved}); err != nil {
			return err
		}
		return errors.New("the change failed")
	})
	if err == nil {
		t.Fatal("the transaction committed")
	}
	if rows := f.rows(a.GroupID); len(rows) != 0 {
		t.Errorf("%d notifications are queued after a rollback", len(rows))
	}
}

func TestBackoff(t *testing.T) {
	f := newFixture(t)
	p := f.pipeline(notify.Config{}, nil)
	// With the defaults: 5s doubling to a cap of 5m, less up to a fifth.
	for attempts, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second, 7: 5 * time.Minute, 8: 5 * time.Minute, 60: 5 * time.Minute} {
		for range 50 {
			got := time.Duration(p.Backoff(attempts))
			if got > want || got < want-want/5 {
				t.Fatalf("backoff after %d attempts = %v, want between %v and %v", attempts, got, want-want/5, want)
			}
		}
	}
}

func TestLongErrorIsStoredTruncated(t *testing.T) {
	f := newFixture(t)
	a := f.fire("DiskFull")
	p := f.pipeline(fastConfig(), channelFunc(func(context.Context, notify.Message) (string, error) {
		return "", notify.Permanent(errors.New(strings.Repeat("é", 400) + "\x00"))
	}))
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)
	f.waitStatus(a.GroupID, "failed")
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT length(error) FROM notification_attempts`).Scan(&n); err != nil || n != 250 {
		t.Errorf("stored error has %d characters, %v; want 250", n, err)
	}
}
