package cli

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/metrics"
	"github.com/thanoslabs/beacon/internal/metrics/metricstest"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

// handling is an alert store whose changes are counted as beacon serve
// counts them, with a clock the test moves.
type handling struct {
	t        *testing.T
	pool     *pgxpool.Pool
	store    *alert.Store
	reg      *prometheus.Registry
	m        *metrics.Alerts
	now      time.Time
	sourceID string
	platform string
	// fail makes an observer that runs after the counting one refuse the
	// change, which rolls its transaction back.
	fail bool
}

func newHandling(t *testing.T) *handling {
	t.Helper()
	pool := postgrestest.NewMigrated(t)
	h := &handling{t: t, pool: pool, store: alert.NewStore(pool), reg: prometheus.NewRegistry(), m: metrics.NewAlerts(), now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	h.store.SetClock(func() time.Time { return h.now })
	h.m.Register(h.reg)
	h.store.Observe(observeAlerts(h.m))
	h.store.Observe(func(context.Context, pgx.Tx, alert.Event, alert.Alert) error {
		if h.fail {
			return errors.New("refused by a later observer")
		}
		return nil
	})
	h.sourceID = provisiontest.Row(t, pool, provision.Sources, "test")
	h.platform = provisiontest.Row(t, pool, provision.Teams, "platform")
	cfg := config.Config{Routing: &config.Routing{Rules: []config.RoutingRule{{Name: "database", Service: "postgres"}, {Name: "no-service"}}}}
	h.m.SetNames(metricNames(cfg, map[string]string{"platform": h.platform}))
	return h
}

func (h *handling) fire(fingerprint string, change func(*alert.Firing)) alert.Alert {
	h.t.Helper()
	f := alert.Firing{SourceID: h.sourceID, Fingerprint: fingerprint, Severity: alert.SeverityCritical, TeamID: h.platform,
		Labels: map[string]string{"alertname": "DiskFull-" + fingerprint}}
	if change != nil {
		change(&f)
	}
	a, _, err := h.store.Fire(context.Background(), f)
	if err != nil {
		h.t.Fatalf("Fire: %v", err)
	}
	return a
}

func (h *handling) apply(a alert.Alert, action alert.Action, opts alert.Options) {
	h.t.Helper()
	if _, err := h.store.Apply(context.Background(), a.ID, action, alert.ChatUser("slack", "U0ADA", "ada"), opts); err != nil {
		h.t.Fatalf("%s: %v", action, err)
	}
}

const (
	ackCount     = `beacon_alert_time_to_acknowledge_seconds_count{severity="critical",team="platform"}`
	ackSum       = `beacon_alert_time_to_acknowledge_seconds_sum{severity="critical",team="platform"}`
	resolveCount = `beacon_alert_time_to_resolve_seconds_count{severity="critical",team="platform"}`
	resolveSum   = `beacon_alert_time_to_resolve_seconds_sum{severity="critical",team="platform"}`
	openedCount  = `beacon_alerts_opened_total{service="none",severity="critical",team="platform"}`
)

// TestAlertHandlingIsObservedOnce: time to acknowledge and time to resolve
// are observed once per alert, at the transition, with the time since
// Beacon opened it; nothing an alert goes through afterwards, and no
// repeated delivery, observes again.
func TestAlertHandlingIsObservedOnce(t *testing.T) {
	h := newHandling(t)
	ctx := context.Background()

	// The histograms of a team exist before anything happened.
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{ackCount: 0, resolveCount: 0})

	a := h.fire("a", nil)
	// The source repeats the alert, as sources do.
	for range 3 {
		h.fire("a", nil)
	}
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{openedCount: 1, ackCount: 0})

	h.now = h.now.Add(90 * time.Second)
	h.apply(a, alert.ActionAcknowledge, alert.Options{})
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{ackCount: 1, ackSum: 90, resolveCount: 0})

	// Snoozed, firing again, acknowledged again: the alert was answered
	// once, after ninety seconds.
	h.now = h.now.Add(time.Minute)
	h.apply(a, alert.ActionSnooze, alert.Options{SnoozeUntil: h.now.Add(time.Hour)})
	h.now = h.now.Add(time.Hour)
	if n, err := h.store.ExpireSnoozes(ctx); n != 1 || err != nil {
		t.Fatalf("ExpireSnoozes = %d, %v", n, err)
	}
	h.now = h.now.Add(5 * time.Minute)
	h.apply(a, alert.ActionAcknowledge, alert.Options{})
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{ackCount: 1, ackSum: 90, resolveCount: 0})

	// Its source resolves it two hours after it opened. The source resolves
	// it again, and that finds nothing open.
	h.now = a.OpenedAt.Add(2 * time.Hour)
	for range 2 {
		if _, _, err := h.store.ResolveFromSource(ctx, h.sourceID, "a", time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{ackCount: 1, ackSum: 90, resolveCount: 1, resolveSum: 7200, openedCount: 1})

	// The same alert fires again: that is a new alert, with times of its
	// own. It is resolved by hand before anyone acknowledged it, so it has
	// a time to resolve and no time to acknowledge.
	b := h.fire("a", nil)
	h.now = h.now.Add(10 * time.Minute)
	h.apply(b, alert.ActionResolve, alert.Options{})
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{ackCount: 1, ackSum: 90, resolveCount: 2, resolveSum: 7200 + 600, openedCount: 2})

	// A source that says the alert ended before Beacon heard of it does not
	// make a negative time.
	h.fire("early", nil)
	if _, _, err := h.store.ResolveFromSource(ctx, h.sourceID, "early", h.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{resolveCount: 3, resolveSum: 7200 + 600})
}

// TestAlertHandlingIsNotCountedWhenTheChangeRollsBack: the counting runs
// inside the transaction of the change and takes effect when it commits. A
// change that is refused after it was counted leaves the metrics as they
// were, and the same change made again is counted once.
func TestAlertHandlingIsNotCountedWhenTheChangeRollsBack(t *testing.T) {
	h := newHandling(t)
	ctx := context.Background()
	someone := alert.ChatUser("slack", "U0ADA", "ada")

	h.fail = true
	if _, _, err := h.store.Fire(ctx, alert.Firing{SourceID: h.sourceID, Fingerprint: "a", Severity: alert.SeverityCritical, TeamID: h.platform}); err == nil {
		t.Fatal("the alert was opened although an observer refused it")
	}
	if got := metricstest.Values(t, h.reg)[openedCount]; got != 0 {
		t.Errorf("an alert that was not opened was counted: %v", got)
	}
	h.fail = false
	a := h.fire("a", nil)

	h.now = h.now.Add(time.Minute)
	h.fail = true
	for _, action := range []alert.Action{alert.ActionAcknowledge, alert.ActionResolve} {
		if _, err := h.store.Apply(ctx, a.ID, action, someone, alert.Options{}); err == nil {
			t.Fatalf("%s succeeded although an observer refused it", action)
		}
	}
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{openedCount: 1, ackCount: 0, resolveCount: 0})

	// The person tries again, a minute later, and it works.
	h.fail = false
	h.now = h.now.Add(time.Minute)
	h.apply(a, alert.ActionAcknowledge, alert.Options{})
	h.apply(a, alert.ActionResolve, alert.Options{})
	metricstest.Expect(t, metricstest.Values(t, h.reg), map[string]float64{openedCount: 1, ackCount: 1, ackSum: 120, resolveCount: 1, resolveSum: 120})
}

// TestAlertHandlingWithSeveralReplicas: each replica counts what it did
// itself. When several are asked to acknowledge and to resolve the same
// alert at the same moment, the alert is observed once among them all.
func TestAlertHandlingWithSeveralReplicas(t *testing.T) {
	h := newHandling(t)
	ctx := context.Background()
	someone := alert.ChatUser("slack", "U0ADA", "ada")
	a := h.fire("contested", nil)
	h.now = h.now.Add(time.Minute)

	const replicas = 8
	registries := make([]*prometheus.Registry, replicas)
	stores := make([]*alert.Store, replicas)
	for i := range replicas {
		m := metrics.NewAlerts()
		m.SetNames(h.m.Names())
		registries[i] = prometheus.NewRegistry()
		m.Register(registries[i])
		stores[i] = alert.NewStore(h.pool)
		stores[i].SetClock(func() time.Time { return h.now })
		stores[i].Observe(observeAlerts(m))
	}
	total := func(series string) (sum float64) {
		for _, reg := range registries {
			sum += metricstest.Values(t, reg)[series]
		}
		return sum
	}
	for _, action := range []alert.Action{alert.ActionAcknowledge, alert.ActionResolve} {
		var wg sync.WaitGroup
		for _, store := range stores {
			wg.Go(func() {
				// All but one find that it has been done.
				_, _ = store.ApplyToGroup(ctx, a.GroupID, action, someone, alert.Options{})
			})
		}
		wg.Wait()
	}
	if ack, resolved := total(ackCount), total(resolveCount); ack != 1 || resolved != 1 {
		t.Errorf("%v acknowledgements and %v resolutions observed among %d replicas, want one of each", ack, resolved, replicas)
	}
	if sum := total(ackSum); sum != 60 {
		t.Errorf("time to acknowledge = %v seconds, want 60", sum)
	}
}

// TestAlertLabelsAreBounded: a team is a label only by the name the
// configuration gives it, and a service only when a routing rule names it.
// What an alert brings in its own service label is counted as "other".
func TestAlertLabelsAreBounded(t *testing.T) {
	h := newHandling(t)
	h.fire("rule", func(f *alert.Firing) { f.Service = "postgres" })
	// Three alerts whose service came from a label of theirs, each with a
	// value of its own.
	for _, service := range []string{"checkout-7f3a", "checkout-9b1c", "anything-a-sender-likes"} {
		h.fire(service, func(f *alert.Firing) { f.Service = service; f.Severity = alert.SeverityWarning })
	}
	h.fire("none", func(f *alert.Firing) { f.TeamID = ""; f.Severity = alert.SeverityInfo })
	// A team that has left the configuration since the alert was routed.
	gone := provisiontest.Row(t, h.pool, provision.Teams, "disbanded")
	h.fire("gone", func(f *alert.Firing) { f.TeamID = gone })

	got := metricstest.Values(t, h.reg)
	want := []string{
		`beacon_alerts_opened_total{service="none",severity="critical",team="other"}`,
		`beacon_alerts_opened_total{service="none",severity="info",team="none"}`,
		`beacon_alerts_opened_total{service="other",severity="warning",team="platform"}`,
		`beacon_alerts_opened_total{service="postgres",severity="critical",team="platform"}`,
	}
	if series := metricstest.WithPrefix(got, "beacon_alerts_opened_total"); !slices.Equal(series, want) {
		t.Errorf("series:\n got %v\nwant %v", series, want)
	}
	if n := got[`beacon_alerts_opened_total{service="other",severity="warning",team="platform"}`]; n != 3 {
		t.Errorf("alerts with a service of their own = %v, want 3 in one series", n)
	}
}
