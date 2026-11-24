package cli

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/metrics"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/schedule"
)

// observeAlerts returns the observer that counts alert handling: an alert
// that is opened, the first response to it, and its resolution.
//
// The observer runs inside the transaction of the change, and counts only
// once that transaction has committed. A change that is rolled back is not
// counted, and a change happens in one transaction of one replica, so
// nothing is counted twice: a repeated delivery of an open alert writes no
// event, and the first response is first by a column of the alert's row.
func observeAlerts(m *metrics.Alerts) alert.Observer {
	return func(ctx context.Context, _ pgx.Tx, ev alert.Event, a alert.Alert) error {
		severity := string(a.Severity)
		if ev.Type == alert.EventFired {
			postgres.AfterCommit(ctx, func() { m.Opened(a.TeamID, a.Service, severity) })
		}
		if ev.FirstResponse {
			postgres.AfterCommit(ctx, func() { m.Acknowledged(a.TeamID, severity, ev.OccurredAt.Sub(a.OpenedAt)) })
		}
		if ev.Type == alert.EventResolved && a.ResolvedAt != nil {
			// The source may say that the alert ended before Beacon heard
			// of it; that is the time it was resolved at.
			postgres.AfterCommit(ctx, func() { m.Resolved(a.TeamID, severity, a.ResolvedAt.Sub(a.OpenedAt)) })
		}
		return nil
	}
}

// metricNames are the names the active configuration gives to what the
// metrics are labelled by.
func metricNames(cfg config.Config, teams map[string]string) metrics.Names {
	names := metrics.Names{Teams: make(map[string]string, len(teams)), Services: map[string]bool{}}
	for name, id := range teams {
		names.Teams[id] = name
	}
	if cfg.Routing != nil {
		for _, rule := range cfg.Routing.Rules {
			if rule.Service != "" {
				names.Services[rule.Service] = true
			}
		}
	}
	return names
}

// installState reads what the metrics say about the whole install from
// Postgres: the queue, the alerts that are open, and whether each schedule
// has somebody on call. Each part is read by the package that owns it.
type installState struct {
	pool      *pgxpool.Pool
	pipeline  *notify.Pipeline
	alerts    *alert.Store
	schedules *schedule.Store
	metrics   *metrics.Alerts
	// live are the schedules of the active configuration: name to the
	// identifier of the row.
	live atomic.Pointer[map[string]string]
}

func (s *installState) read(ctx context.Context) (metrics.State, error) {
	var (
		state metrics.State
		err   error
	)
	if state.PendingNotifications, state.OldestPending, err = s.pipeline.QueueStats(ctx); err != nil {
		return metrics.State{}, err
	}
	open, err := s.alerts.CountOpen(ctx)
	if err != nil {
		return metrics.State{}, err
	}
	state.OpenAlerts = s.openAlerts(open)

	state.Covered = map[string]bool{}
	if live := s.live.Load(); live != nil {
		for name, id := range *live {
			onCall, err := s.schedules.OnCall(ctx, s.pool, id)
			if errors.Is(err, schedule.ErrNotFound) {
				// Not started yet, or just removed: no claim either way.
				continue
			}
			if err != nil {
				return metrics.State{}, err
			}
			state.Covered[name] = onCall.UserID != ""
		}
	}
	return state, nil
}

// openAlerts turns the counts of the store into series: by the names the
// configuration gives the teams, with a zero for every team, severity and
// state that has no alert, so that "no alert is firing" is a number.
func (s *installState) openAlerts(counts []alert.OpenCount) []metrics.OpenAlerts {
	type key struct{ team, severity, state string }
	byKey := map[key]int{}
	teams := []string{metrics.None}
	for _, name := range s.metrics.Names().Teams {
		teams = append(teams, name)
	}
	for _, team := range teams {
		for _, severity := range metrics.Severities {
			for _, st := range metrics.States {
				byKey[key{team, severity, st}] = 0
			}
		}
	}
	for _, c := range counts {
		byKey[key{s.metrics.Team(c.TeamID), metrics.Severity(string(c.Severity)), string(c.Status)}] += c.Count
	}
	out := make([]metrics.OpenAlerts, 0, len(byKey))
	for k, n := range byKey {
		out = append(out, metrics.OpenAlerts{Team: k.team, Severity: k.severity, State: k.state, Count: n})
	}
	return out
}
