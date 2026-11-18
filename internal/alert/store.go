package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no alert or group has the given identifier.
var ErrNotFound = errors.New("alert not found")

// ErrSnoozeUntil is returned when a snooze is asked to end in the past.
var ErrSnoozeUntil = errors.New("a snooze must end in the future")

// Observer is told about every state change, inside the transaction that
// makes it. What it writes through tx commits or rolls back with the change,
// which is how a notification is queued exactly when the state changes. An
// error aborts the change.
type Observer func(ctx context.Context, tx pgx.Tx, ev Event, a Alert) error

// Store keeps alerts and their timeline in Postgres. Every state change goes
// through Next, and is written together with its timeline event.
type Store struct {
	db        *pgxpool.Pool
	now       func() time.Time
	observers []Observer
}

// NewStore returns a store that uses db.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db, now: time.Now}
}

// SetClock replaces the clock, so tests can move time.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Observe registers o. Call it before the store is used.
func (s *Store) Observe(o Observer) { s.observers = append(s.observers, o) }

// Firing describes an alert that a source reports as firing.
type Firing struct {
	SourceID    string
	Fingerprint string
	Severity    Severity
	Labels      map[string]string
	Annotations map[string]string
	// Links are web addresses that belong to the alert, by name.
	Links map[string]string
	// StartsAt is when the source says the alert began. Zero means now.
	StartsAt time.Time
}

// Options carries the details some actions need.
type Options struct {
	// SnoozeUntil is when a snooze ends. Required for ActionSnooze.
	SnoozeUntil time.Time
	// ResolvedAt is when the alert was resolved, when the source says so.
	// Zero means now.
	ResolvedAt time.Time
}

const alertColumns = `id::text, source_id::text, group_id::text, fingerprint, status, severity,
	labels, annotations, links, started_at, last_received_at, acknowledged_at, snoozed_until, resolved_at`

func scanAlert(row interface{ Scan(...any) error }) (Alert, error) {
	var a Alert
	err := row.Scan(&a.ID, &a.SourceID, &a.GroupID, &a.Fingerprint, &a.Status, &a.Severity,
		&a.Labels, &a.Annotations, &a.Links, &a.StartedAt, &a.LastReceivedAt, &a.AcknowledgedAt, &a.SnoozedUntil, &a.ResolvedAt)
	return a, err
}

func collectAlerts(rows pgx.Rows, err error) ([]Alert, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Alert, error) { return scanAlert(r) })
}

// Fire records that a source reports an alert as firing. If the source already
// has an open alert with the fingerprint, that alert is refreshed and created
// is false. Otherwise a new alert is opened in StatusFiring.
func (s *Store) Fire(ctx context.Context, f Firing) (a Alert, created bool, err error) {
	if !f.Severity.Valid() {
		return Alert{}, false, fmt.Errorf("unknown severity %q", f.Severity)
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := lockFingerprint(ctx, tx, f.SourceID, f.Fingerprint); err != nil {
			return err
		}
		now := s.now()
		a, err = scanAlert(tx.QueryRow(ctx, `
			UPDATE alerts SET severity = $3, labels = $4, annotations = $5, last_received_at = $6, links = $7
			WHERE source_id = $1 AND fingerprint = $2 AND status <> 'resolved'
			RETURNING `+alertColumns,
			f.SourceID, f.Fingerprint, f.Severity, nonNil(f.Labels), nonNil(f.Annotations), now, nonNil(f.Links)))
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// Until grouping exists, every alert gets a group of its own.
		var groupID string
		err = tx.QueryRow(ctx, `INSERT INTO alert_groups (group_key, created_at) VALUES ($1, $2) RETURNING id::text`,
			groupKey(f.SourceID, f.Fingerprint), now).Scan(&groupID)
		if err != nil {
			return fmt.Errorf("create alert group: %w", err)
		}
		startedAt := f.StartsAt
		if startedAt.IsZero() {
			startedAt = now
		}
		a, err = scanAlert(tx.QueryRow(ctx, `
			INSERT INTO alerts (source_id, fingerprint, severity, labels, annotations, group_id, started_at, last_received_at, links)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+alertColumns,
			f.SourceID, f.Fingerprint, f.Severity, nonNil(f.Labels), nonNil(f.Annotations), groupID, startedAt, now, nonNil(f.Links)))
		if err != nil {
			return fmt.Errorf("create alert: %w", err)
		}
		created = true
		return s.record(ctx, tx, a, EventFired, Source(f.SourceID), now, nil)
	})
	return a, created, err
}

// ResolveFromSource resolves the source's open alert with the fingerprint.
// found is false, with no error, when there is none: a source may send a
// resolve for an alert that was already resolved by hand.
func (s *Store) ResolveFromSource(ctx context.Context, sourceID, fingerprint string, resolvedAt time.Time) (a Alert, found bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := lockFingerprint(ctx, tx, sourceID, fingerprint); err != nil {
			return err
		}
		a, err = scanAlert(tx.QueryRow(ctx, `SELECT `+alertColumns+` FROM alerts
			WHERE source_id = $1 AND fingerprint = $2 AND status <> 'resolved' FOR UPDATE`, sourceID, fingerprint))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		a, err = s.transition(ctx, tx, a, ActionResolve, Source(sourceID), Options{ResolvedAt: resolvedAt})
		return err
	})
	return a, found, err
}

// Apply takes action on one alert. It returns a *TransitionError when the
// alert's current state does not allow the action.
func (s *Store) Apply(ctx context.Context, alertID string, action Action, actor Actor, opts Options) (a Alert, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		a, err = scanAlert(tx.QueryRow(ctx, `SELECT `+alertColumns+` FROM alerts WHERE id = $1 FOR UPDATE`, alertID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		a, err = s.transition(ctx, tx, a, action, actor, opts)
		return err
	})
	return a, err
}

// ApplyToGroup takes action on every alert of a group whose state allows it,
// and returns the alerts it changed. When no alert could change, it returns
// the *TransitionError of the first one, so the caller can say why.
func (s *Store) ApplyToGroup(ctx context.Context, groupID string, action Action, actor Actor, opts Options) (changed []Alert, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		changed = nil
		alerts, err := collectAlerts(tx.Query(ctx, `SELECT `+alertColumns+` FROM alerts WHERE group_id = $1 ORDER BY id FOR UPDATE`, groupID))
		if err != nil {
			return err
		}
		if len(alerts) == 0 {
			return ErrNotFound
		}
		var refused error
		for _, a := range alerts {
			a, err := s.transition(ctx, tx, a, action, actor, opts)
			var te *TransitionError
			switch {
			case errors.As(err, &te):
				if refused == nil {
					refused = err
				}
			case err != nil:
				return err
			default:
				changed = append(changed, a)
			}
		}
		if len(changed) == 0 {
			return refused
		}
		return nil
	})
	return changed, err
}

// ExpireSnoozes returns every alert whose snooze has run out to firing, and
// reports how many it moved. It is safe to call from several replicas at once:
// rows another caller is working on are skipped.
func (s *Store) ExpireSnoozes(ctx context.Context) (n int, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		due, err := collectAlerts(tx.Query(ctx, `SELECT `+alertColumns+` FROM alerts
			WHERE status = 'snoozed' AND snoozed_until <= $1
			ORDER BY snoozed_until LIMIT 100 FOR UPDATE SKIP LOCKED`, s.now()))
		if err != nil {
			return err
		}
		for _, a := range due {
			if _, err := s.transition(ctx, tx, a, ActionEndSnooze, System(), Options{}); err != nil {
				return err
			}
		}
		n = len(due)
		return nil
	})
	return n, err
}

// RunSnoozeExpiry calls ExpireSnoozes every interval until ctx is cancelled.
func (s *Store) RunSnoozeExpiry(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n, err := s.ExpireSnoozes(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.ErrorContext(ctx, "expire snoozes", slog.Any("error", err))
		case n > 0:
			logger.InfoContext(ctx, "snoozes ended", slog.Int("alerts", n))
		}
	}
}

// Get returns one alert.
func (s *Store) Get(ctx context.Context, alertID string) (Alert, error) {
	a, err := scanAlert(s.db.QueryRow(ctx, `SELECT `+alertColumns+` FROM alerts WHERE id = $1`, alertID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Alert{}, ErrNotFound
	}
	return a, err
}

// GroupAlerts returns the alerts of a group, oldest first.
func (s *Store) GroupAlerts(ctx context.Context, groupID string) ([]Alert, error) {
	return collectAlerts(s.db.Query(ctx, `SELECT `+alertColumns+` FROM alerts WHERE group_id = $1 ORDER BY started_at, id`, groupID))
}

const eventColumns = `e.id, COALESCE(e.alert_id::text, ''), e.alert_group_id::text, e.type, e.actor_type,
	COALESCE(e.actor_id::text, ''), COALESCE(e.actor_provider, ''), COALESCE(e.actor_external_id, ''),
	e.occurred_at, e.data`

func collectEvents(rows pgx.Rows, err error) ([]Event, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.AlertID, &e.GroupID, &e.Type, &e.Actor.Type,
			&e.Actor.ID, &e.Actor.Provider, &e.Actor.ExternalID, &e.OccurredAt, &e.Data)
		e.Actor.Name, _ = e.Data[dataActorName].(string)
		return e, err
	})
}

// Timeline returns the events of an alert in the order they happened,
// including the events about its group, such as notifications.
func (s *Store) Timeline(ctx context.Context, alertID string) ([]Event, error) {
	return collectEvents(s.db.Query(ctx, `SELECT `+eventColumns+` FROM alert_events e
		JOIN alerts a ON a.id = $1
		WHERE e.alert_id = a.id OR (e.alert_id IS NULL AND e.alert_group_id = a.group_id)
		ORDER BY e.id`, alertID))
}

// GroupTimeline returns every event of a group and of its alerts, in the
// order they happened.
func (s *Store) GroupTimeline(ctx context.Context, groupID string) ([]Event, error) {
	return collectEvents(s.db.Query(ctx, `SELECT `+eventColumns+` FROM alert_events e
		WHERE e.alert_group_id = $1 ORDER BY e.id`, groupID))
}

// dataActorName is the key in an event's data that holds the actor's display
// name.
const dataActorName = "actor_name"

// AppendEvent writes ev to the timeline through tx and sets its identifier.
// It is exported for the packages that record their own events, such as
// notifications. Observers are not told about events written this way.
func AppendEvent(ctx context.Context, tx pgx.Tx, ev *Event) error {
	data := map[string]any{}
	for k, v := range ev.Data {
		data[k] = v
	}
	if ev.Actor.Name != "" {
		data[dataActorName] = ev.Actor.Name
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO alert_events (alert_id, alert_group_id, type, actor_type, actor_id, actor_provider, actor_external_id, occurred_at, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		nullable(ev.AlertID), ev.GroupID, ev.Type, ev.Actor.Type, nullable(ev.Actor.ID),
		nullable(ev.Actor.Provider), nullable(ev.Actor.ExternalID), ev.OccurredAt, data).Scan(&ev.ID)
	if err != nil {
		return fmt.Errorf("record %s event: %w", ev.Type, err)
	}
	return nil
}

// transition applies action to a, which the caller has locked, and records
// the event.
func (s *Store) transition(ctx context.Context, tx pgx.Tx, a Alert, action Action, actor Actor, opts Options) (Alert, error) {
	to, err := Next(a.Status, action)
	if err != nil {
		return a, err
	}
	now := s.now()
	data := map[string]any{"from": a.Status, "to": to}

	var snoozedUntil, resolvedAt *time.Time
	switch to {
	case StatusSnoozed:
		if !opts.SnoozeUntil.After(now) {
			return a, ErrSnoozeUntil
		}
		snoozedUntil = &opts.SnoozeUntil
		data["until"] = opts.SnoozeUntil.UTC().Format(time.RFC3339)
	case StatusResolved:
		resolvedAt = &now
		if !opts.ResolvedAt.IsZero() {
			resolvedAt = &opts.ResolvedAt
		}
	}
	// Only a Beacon user can be referenced; a chat identity stays on the event.
	var acknowledgedBy *string
	if actor.Type == ActorUser {
		acknowledgedBy = nullable(actor.ID)
	}

	from := a
	a, err = scanAlert(tx.QueryRow(ctx, `
		UPDATE alerts SET status = $2, snoozed_until = $3, resolved_at = $4,
			acknowledged_at = CASE $2 WHEN 'acknowledged' THEN $5::timestamptz WHEN 'firing' THEN NULL ELSE acknowledged_at END,
			acknowledged_by = CASE $2 WHEN 'acknowledged' THEN $6::uuid WHEN 'firing' THEN NULL ELSE acknowledged_by END
		WHERE id = $1
		RETURNING `+alertColumns, a.ID, to, snoozedUntil, resolvedAt, now, acknowledgedBy))
	if err != nil {
		return from, fmt.Errorf("%s alert: %w", action, err)
	}
	if to == StatusResolved {
		// A group is resolved once all its alerts are.
		_, err = tx.Exec(ctx, `UPDATE alert_groups SET status = 'resolved', resolved_at = $2
			WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM alerts WHERE group_id = $1 AND status <> 'resolved')`, a.GroupID, now)
		if err != nil {
			return from, fmt.Errorf("resolve alert group: %w", err)
		}
	}
	return a, s.record(ctx, tx, a, eventFor[action], actor, now, data)
}

// record writes a state-change event and tells the observers.
func (s *Store) record(ctx context.Context, tx pgx.Tx, a Alert, typ EventType, actor Actor, at time.Time, data map[string]any) error {
	ev := Event{AlertID: a.ID, GroupID: a.GroupID, Type: typ, Actor: actor, OccurredAt: at, Data: data}
	if err := AppendEvent(ctx, tx, &ev); err != nil {
		return err
	}
	for _, o := range s.observers {
		if err := o(ctx, tx, ev, a); err != nil {
			return fmt.Errorf("after %s event: %w", typ, err)
		}
	}
	return nil
}

// lockFingerprint serialises the transactions that open or resolve the alert
// with this fingerprint, so concurrent deliveries of one alert cannot both
// find it missing. The lock is released when the transaction ends.
func lockFingerprint(ctx context.Context, tx pgx.Tx, sourceID, fingerprint string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, sourceID+"/"+fingerprint)
	return err
}

// groupKey is the key of the group a new alert is put in. Each alert gets its
// own group for now; grouping by labels replaces this.
func groupKey(sourceID, fingerprint string) string {
	sum := sha256.Sum256([]byte("alert/" + sourceID + "/" + fingerprint))
	return hex.EncodeToString(sum[:])
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
