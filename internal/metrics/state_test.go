package metrics_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/thanoslabs/beacon/internal/metrics"
)

var quiet = slog.New(slog.DiscardHandler)

// expect compares what the collector serves with the series a test wants,
// leaving the read duration out.
func expect(t *testing.T, reg *prometheus.Registry, want string) {
	t.Helper()
	names := []string{metrics.StateUp, metrics.NotificationsPending, metrics.NotificationsOldest}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), names...); err != nil {
		t.Error(err)
	}
}

func help(name string) string {
	d, _ := metrics.Lookup(name)
	return "# HELP " + d.Name + " " + d.Help + "\n# TYPE " + d.Name + " " + string(d.Type) + "\n"
}

// TestStateCollector: the global metrics are read when they are scraped,
// one read serves the scrapes that follow closely, and a failed read leaves
// them out instead of serving what an earlier read found.
func TestStateCollector(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var reads atomic.Int32
	var fail atomic.Bool
	state := metrics.State{PendingNotifications: map[string]int{"slack": 3, "email": 0}, OldestPending: 90 * time.Second}
	c := metrics.NewStateCollector(quiet, func(context.Context) (metrics.State, error) {
		reads.Add(1)
		if fail.Load() {
			return metrics.State{}, errors.New("the database is away")
		}
		return state, nil
	})
	c.SetTimes(time.Second, 5*time.Second, func() time.Time { return now })
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	read := help(metrics.NotificationsOldest) + "beacon_notifications_oldest_pending_age_seconds 90\n" +
		help(metrics.NotificationsPending) + "beacon_notifications_pending{integration=\"email\"} 0\nbeacon_notifications_pending{integration=\"slack\"} 3\n" +
		help(metrics.StateUp) + "beacon_state_metrics_up 1\n"
	expect(t, reg, read)
	// Scrapes that follow within the maximum age cost no read.
	now = now.Add(4 * time.Second)
	expect(t, reg, read)
	expect(t, reg, read)
	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads for three scrapes within five seconds, want 1", n)
	}

	// After it, the state is read again; a failure leaves the global
	// metrics out and says so.
	now = now.Add(time.Second)
	fail.Store(true)
	expect(t, reg, help(metrics.StateUp)+"beacon_state_metrics_up 0\n")
	if n := reads.Load(); n != 2 {
		t.Errorf("%d reads, want 2", n)
	}
	// The failure is not asked about again at once either.
	expect(t, reg, help(metrics.StateUp)+"beacon_state_metrics_up 0\n")
	if n := reads.Load(); n != 2 {
		t.Errorf("%d reads, want 2: a failed read is kept for the maximum age too", n)
	}

	now = now.Add(5 * time.Second)
	fail.Store(false)
	expect(t, reg, read)
}

// TestASlowDatabaseCannotHangAScrape: a read that does not return is
// abandoned after the timeout, the scrape goes on without the global
// metrics, and the database is not asked again while it still holds the
// first question. What the abandoned read answers in the end is never
// served: it is old by then.
func TestASlowDatabaseCannotHangAScrape(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var reads atomic.Int32
	release := make(chan struct{})
	var hang atomic.Bool
	hang.Store(true)
	c := metrics.NewStateCollector(quiet, func(context.Context) (metrics.State, error) {
		reads.Add(1)
		if hang.Load() {
			// Worse than slow: it does not even honour its deadline.
			<-release
			return metrics.State{PendingNotifications: map[string]int{"stale": 99}}, nil
		}
		return metrics.State{PendingNotifications: map[string]int{"slack": 1}}, nil
	})
	const timeout = 50 * time.Millisecond
	c.SetTimes(timeout, 5*time.Second, func() time.Time { return now })
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	down := help(metrics.StateUp) + "beacon_state_metrics_up 0\n"
	scraped := make(chan struct{})
	go func() {
		defer close(scraped)
		expect(t, reg, down)
	}()
	select {
	case <-scraped:
	case <-time.After(10 * time.Second):
		t.Fatal("the scrape waits for the database")
	}

	// The next scrapes that want a read do not start another, and do not
	// wait at all.
	for range 3 {
		now = now.Add(6 * time.Second)
		start := time.Now()
		expect(t, reg, down)
		if took := time.Since(start); took > timeout {
			t.Errorf("a scrape took %s while an earlier read was still out", took)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads while the first had not returned, want 1", n)
	}

	// The database comes back. The answer of the abandoned read is thrown
	// away and the state is read afresh.
	hang.Store(false)
	close(release)
	now = now.Add(6 * time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for {
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		up := false
		for _, f := range families {
			if f.GetName() == metrics.NotificationsPending {
				up = true
				if got := f.GetMetric()[0].GetLabel()[0].GetValue(); got != "slack" {
					t.Fatalf("the scrape served the answer of the abandoned read: %s", got)
				}
			}
		}
		if up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the global metrics did not come back")
		}
		now = now.Add(6 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
}
