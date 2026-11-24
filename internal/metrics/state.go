package metrics

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// State is what the whole install looks like at one moment, as far as the
// metrics tell it. It is read from Postgres, so every replica finds the same.
type State struct {
	// PendingNotifications counts the notifications that are neither
	// delivered nor given up, by the name of their integration.
	PendingNotifications map[string]int
	// OldestPending is the age of the oldest of them, or zero.
	OldestPending time.Duration
	// OpenAlerts counts the alerts that are not resolved.
	OpenAlerts []OpenAlerts
	// Covered says for each schedule, by name, whether somebody is on call.
	Covered map[string]bool
}

// OpenAlerts is how many alerts of one team and severity are in one state.
// The values are label values: they have been through Alerts.Team and
// Severity.
type OpenAlerts struct {
	Team, Severity, State string
	Count                 int
}

// Defaults of the state collector.
const (
	// StateTimeout bounds one read of the state. A database that does not
	// answer within it costs a scrape that long and no longer.
	StateTimeout = 2 * time.Second
	// StateMaxAge is how long a read is reused. Scrapes that come closer
	// together than this, from several Prometheus servers for example, cost
	// the database one read.
	StateMaxAge = 5 * time.Second
)

// StateCollector serves the global metrics: facts about the install that
// are in Postgres and not in any one process. It reads them when it is
// scraped, at most once per MaxAge, and gives up after Timeout.
//
// When a read fails or takes too long, the global metrics are left out of
// the scrape and beacon_state_metrics_up is 0. A value from an earlier read
// is never served in their place: an empty queue that was read five minutes
// ago says nothing about the queue.
type StateCollector struct {
	read    func(context.Context) (State, error)
	logger  *slog.Logger
	timeout time.Duration
	maxAge  time.Duration
	now     func() time.Time

	up, duration, pending, oldest, open, covered *prometheus.Desc

	mu sync.Mutex
	// state is the last read and readAt when it was made; ok is whether it
	// succeeded, and took how long it ran.
	state  State
	ok     bool
	readAt time.Time
	took   time.Duration
	// running is a read that a scrape stopped waiting for. While it has
	// not returned, no other is started: a database that hangs gets one
	// question, not one per scrape.
	running chan result
}

type result struct {
	state State
	err   error
}

// NewStateCollector returns a collector that asks read for the state.
func NewStateCollector(logger *slog.Logger, read func(context.Context) (State, error)) *StateCollector {
	return &StateCollector{
		read: read, logger: logger, timeout: StateTimeout, maxAge: StateMaxAge, now: time.Now,
		up: Desc(StateUp), duration: Desc(StateReadDuration), pending: Desc(NotificationsPending), oldest: Desc(NotificationsOldest),
		open: Desc(AlertsOpen), covered: Desc(ScheduleCovered),
	}
}

// SetTimes replaces the bounds and the clock, for tests.
func (c *StateCollector) SetTimes(timeout, maxAge time.Duration, now func() time.Time) {
	c.timeout, c.maxAge, c.now = timeout, maxAge, now
}

// Describe implements prometheus.Collector.
func (c *StateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.duration
	ch <- c.pending
	ch <- c.oldest
	ch <- c.open
	ch <- c.covered
}

// Collect implements prometheus.Collector.
func (c *StateCollector) Collect(ch chan<- prometheus.Metric) {
	// Scrapes take turns, so that those that arrive together share a read.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readAt.IsZero() || c.now().Sub(c.readAt) >= c.maxAge {
		c.refresh()
	}
	up := 0.0
	if c.ok {
		up = 1
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, up)
	ch <- prometheus.MustNewConstMetric(c.duration, prometheus.GaugeValue, c.took.Seconds())
	if !c.ok {
		return
	}
	for integration, n := range c.state.PendingNotifications {
		ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(n), integration)
	}
	ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, c.state.OldestPending.Seconds())
	for _, o := range c.state.OpenAlerts {
		ch <- prometheus.MustNewConstMetric(c.open, prometheus.GaugeValue, float64(o.Count), o.Team, o.Severity, o.State)
	}
	for schedule, covered := range c.state.Covered {
		value := 0.0
		if covered {
			value = 1
		}
		ch <- prometheus.MustNewConstMetric(c.covered, prometheus.GaugeValue, value, schedule)
	}
}

// refresh reads the state, waiting no longer than the timeout.
func (c *StateCollector) refresh() {
	start := time.Now()
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	if c.running != nil {
		// A read that an earlier scrape stopped waiting for. Its answer,
		// if it has come, is too old to serve; if it has not, the database
		// is not asked again while it still holds the question.
		select {
		case <-c.running:
			c.running = nil
		default:
			c.fail(start, errStillReading)
			return
		}
	}
	done := make(chan result, 1)
	c.running = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		state, err := c.read(ctx)
		done <- result{state, err}
	}()
	var res result
	select {
	case res = <-done:
		c.running = nil
	case <-timer.C:
		res.err = context.DeadlineExceeded
	}
	if res.err != nil {
		c.fail(start, res.err)
		return
	}
	c.took, c.readAt, c.ok, c.state = time.Since(start), c.now(), true, res.state
}

var errStillReading = errors.New("an earlier read has not returned")

// fail records a read that gave no state.
func (c *StateCollector) fail(start time.Time, err error) {
	c.took, c.readAt, c.ok, c.state = time.Since(start), c.now(), false, State{}
	c.logger.Warn("the global metrics could not be read from Postgres and are left out of this scrape", slog.Any("error", err))
}
