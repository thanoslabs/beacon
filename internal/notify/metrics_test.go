package notify_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thanoslabs/beacon/internal/metrics/metricstest"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

// TestDeliveryMetrics: every attempt is counted once with its outcome, a
// notification that is given up is counted once however many attempts it
// took, and the labels are the name and type the configuration gives the
// integration.
func TestDeliveryMetrics(t *testing.T) {
	f := newFixture(t)
	var calls atomic.Int32
	// The first notification succeeds at once, the second at its third
	// attempt, the third never, and the fourth is refused for good.
	behaviour := map[string]func(n int) error{}
	ch := channelFunc(func(_ context.Context, m notify.Message) (string, error) {
		calls.Add(1)
		return "ref", behaviour[m.Group.ID](m.Notification.Attempts)
	})
	p := f.pipeline(fastConfig(), ch)
	reg := prometheus.NewRegistry()
	p.RegisterMetrics(reg)
	p.SetIntegrations(map[string]notify.Via{f.integrationID: {IntegrationID: f.integrationID, Name: "chat", Provider: "slack"}})

	// The series of a configured integration exist before anything is sent.
	metricstest.Expect(t, metricstest.Values(t, reg), map[string]float64{
		`beacon_notification_attempts_total{integration="chat",outcome="success",type="slack"}`:         0,
		`beacon_notification_attempts_total{integration="chat",outcome="retryable_error",type="slack"}`: 0,
		`beacon_notification_attempts_total{integration="chat",outcome="permanent_error",type="slack"}`: 0,
		`beacon_notifications_failed_total{integration="chat",type="slack"}`:                            0,
	})

	ok, flaky, down, refused := f.fire("ok"), f.fire("flaky"), f.fire("down"), f.fire("refused")
	behaviour[ok.GroupID] = func(int) error { return nil }
	behaviour[flaky.GroupID] = func(n int) error {
		if n < 3 {
			return errors.New("try again")
		}
		return nil
	}
	behaviour[down.GroupID] = func(int) error { return errors.New("still down") }
	behaviour[refused.GroupID] = func(int) error { return notify.Permanent(errors.New("no such channel")) }
	for _, a := range []string{ok.GroupID, flaky.GroupID, down.GroupID, refused.GroupID} {
		f.enqueue(p, a, notify.ReasonEscalationStep)
	}
	f.run(p)
	f.waitStatus(ok.GroupID, "sent")
	f.waitStatus(flaky.GroupID, "sent")
	f.waitStatus(down.GroupID, "failed")
	f.waitStatus(refused.GroupID, "failed")

	// 1 + 3 + 4 (every attempt the configuration allows) + 1 attempts.
	want := map[string]float64{
		`beacon_notification_attempts_total{integration="chat",outcome="success",type="slack"}`:         2,
		`beacon_notification_attempts_total{integration="chat",outcome="retryable_error",type="slack"}`: 2 + 4,
		`beacon_notification_attempts_total{integration="chat",outcome="permanent_error",type="slack"}`: 1,
		`beacon_notifications_failed_total{integration="chat",type="slack"}`:                            2,
		`beacon_notification_send_duration_seconds_count{type="slack"}`:                                 9,
	}
	// The counters move a moment after the row does.
	f.waitFor("the counters to follow the deliveries", func() bool {
		got := metricstest.Values(t, reg)
		for series, value := range want {
			if got[series] != value {
				return false
			}
		}
		return true
	})
	metricstest.Expect(t, metricstest.Values(t, reg), want)
	if n := calls.Load(); n != 9 {
		t.Errorf("the channel was called %d times, want 9", n)
	}
	// Nothing was counted twice in the meantime.
	metricstest.Expect(t, metricstest.Values(t, reg), want)
}

// TestDeliveryMetricsForAnIntegrationThatLeft: a notification queued for an
// integration that the configuration no longer has is counted under a fixed
// name, never under its identifier.
func TestDeliveryMetricsForAnIntegrationThatLeft(t *testing.T) {
	f := newFixture(t)
	p := f.pipeline(fastConfig(), nil)
	reg := prometheus.NewRegistry()
	p.RegisterMetrics(reg)
	a := f.fire("orphan")
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	f.run(p)
	f.waitStatus(a.GroupID, "failed")
	want := map[string]float64{
		`beacon_notification_attempts_total{integration="other",outcome="permanent_error",type="other"}`: 1,
		`beacon_notifications_failed_total{integration="other",type="other"}`:                            1,
	}
	f.waitFor("the counters", func() bool {
		return metricstest.Values(t, reg)[`beacon_notifications_failed_total{integration="other",type="other"}`] == 1
	})
	got := metricstest.Values(t, reg)
	metricstest.Expect(t, got, want)
	if len(metricstest.WithPrefix(got, "beacon_notification_attempts_total")) != 1 {
		t.Errorf("series = %v", metricstest.WithPrefix(got, "beacon_notification"))
	}
}

// TestQueueStats: the depth of the queue counts what is neither delivered
// nor given up, by integration, with a zero for an integration that has
// nothing waiting; the age is that of the oldest such notification, by the
// pipeline's clock.
func TestQueueStats(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	email := provisiontest.Row(t, f.pool, provision.Integrations, "email")
	now := time.Now().UTC().Truncate(time.Second)
	p := f.pipeline(fastConfig(), nil)
	p.SetClock(func() time.Time { return now })

	pending, oldest, err := p.QueueStats(ctx)
	if err != nil || len(pending) != 2 || pending["chat"] != 0 || pending["email"] != 0 || oldest != 0 {
		t.Fatalf("an empty queue = %v, %s, %v", pending, oldest, err)
	}

	a, b := f.fire("a"), f.fire("b")
	f.enqueue(p, a.GroupID, notify.ReasonEscalationStep)
	now = now.Add(40 * time.Second)
	f.enqueue(p, b.GroupID, notify.ReasonEscalationStep)
	f.enqueue(p, b.GroupID, notify.ReasonGroupUpdate)
	now = now.Add(20 * time.Second)
	// What was delivered or given up is not in the queue.
	if _, err := f.pool.Exec(ctx, `INSERT INTO notifications (alert_group_id, integration_id, destination, reason, next_attempt_at, created_at, status, sent_at)
		VALUES ($1, $2, 'x', 'resolved', $3, $3, 'sent', $3), ($1, $2, 'x', 'resolved', $3, $3, 'failed', NULL)`, a.GroupID, email, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending, oldest, err = p.QueueStats(ctx)
	if err != nil || len(pending) != 2 || pending["chat"] != 3 || pending["email"] != 0 || oldest != time.Minute {
		t.Errorf("QueueStats = %v, %s, %v; want chat 3, email 0, one minute", pending, oldest, err)
	}

	// An integration that left the configuration is counted while it still
	// has something in the queue, and gone once it has not.
	if _, err := f.pool.Exec(ctx, `UPDATE integrations SET removed_at = now() WHERE id = ANY($1)`, []string{email, f.integrationID}); err != nil {
		t.Fatal(err)
	}
	pending, _, err = p.QueueStats(ctx)
	if err != nil || len(pending) != 1 || pending["chat"] != 3 {
		t.Errorf("QueueStats after the integrations left = %v, %v; want chat 3 alone", pending, err)
	}
}
