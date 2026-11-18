package alert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GroupStatus says whether a group still has an alert that is not resolved.
type GroupStatus string

// The group states. A group is resolved once all its alerts are, and never
// opens again: an alert that would have joined it opens a new group.
const (
	GroupOpen     GroupStatus = "open"
	GroupResolved GroupStatus = "resolved"
)

// Group is a set of related alerts that are notified about together.
type Group struct {
	ID string
	// Key identifies the group among the open groups.
	Key    string
	Status GroupStatus
	// Labels are the label values the alerts of the group share.
	Labels    map[string]string
	CreatedAt time.Time
	// NotifyAt is when the next notification about the group is due, or nil.
	NotifyAt *time.Time
	// NotifiedAt is when the last notification about the group was queued,
	// or nil when there has been none.
	NotifiedAt *time.Time
	ResolvedAt *time.Time
	// RoutingRuleID, TeamID and EscalationPolicyID are what routing decided
	// for the alert that opened the group. Each may be empty.
	RoutingRuleID      string
	TeamID             string
	EscalationPolicyID string
	// NotifiedAlerts holds the alerts that a notification about the group
	// has shown, by identifier, each with what it was shown as: ShownOpen or
	// ShownResolved.
	NotifiedAlerts map[string]string
	// SilenceID is the silence that the group's next notification waits
	// for, or empty.
	SilenceID string
}

// What an alert was shown as in a notification.
const (
	ShownOpen     = "open"
	ShownResolved = "resolved"
)

// ShownAs returns what a notification sent now would show the alert as.
func ShownAs(a Alert) string {
	if a.Status == StatusResolved {
		return ShownResolved
	}
	return ShownOpen
}

// Grouping says which group a new alert joins and how often that group
// notifies. The zero value puts the alert in a group of its own that
// notifies at once.
type Grouping struct {
	// Key identifies the group: a new alert joins the open group with this
	// key, or opens one. Make it with GroupKey.
	Key string
	// Labels are the label values that define the group.
	Labels map[string]string
	// Wait is how long a new group waits before its first notification, so
	// that alerts which arrive together are told about together.
	Wait time.Duration
	// Interval is the least time between two notifications about the group
	// that its sources cause.
	Interval time.Duration
	// RoutingRuleID, TeamID and EscalationPolicyID are stored on a group
	// that this alert opens. Alerts that share a key share them, when the
	// key was made with the routing rule as its scope.
	RoutingRuleID      string
	TeamID             string
	EscalationPolicyID string
}

// GroupKey returns the key of the group for an alert with these labels, and
// the label values that define it: alerts are grouped when they agree on
// every label in by, a missing label counting as empty. Alerts in different
// scopes never share a group.
func GroupKey(scope string, by []string, labels map[string]string) (key string, groupLabels map[string]string) {
	sum := sha256.New()
	fmt.Fprintf(sum, "group/%d:%s", len(scope), scope)
	groupLabels = make(map[string]string, len(by))
	for _, name := range by {
		value := labels[name]
		// Lengths keep different label sets from producing the same bytes.
		fmt.Fprintf(sum, "/%d:%s=%d:%s", len(name), name, len(value), value)
		if value != "" {
			groupLabels[name] = value
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), groupLabels
}

// ownGroupKey is the key of the group of an alert that is not grouped with
// any other.
func ownGroupKey(sourceID, fingerprint string) string {
	sum := sha256.Sum256([]byte("alert/" + sourceID + "/" + fingerprint))
	return hex.EncodeToString(sum[:])
}

// joinGroup puts a new alert into the open group with the key, opening the
// group if there is none, and returns the group's identifier.
//
// A group that is new waits g.Wait before its first notification. A group
// that has been notified before gets one for when its interval has passed.
// Neither moves a notification that is due sooner: the earlier time stands.
// It can be later only when the group is waiting for a silence to end, and
// then the alert that joins may be one the silence does not cover.
//
// The statement holds the group's row until the transaction ends, so that
// alerts joining at once take turns.
func (s *Store) joinGroup(ctx context.Context, tx pgx.Tx, f Firing, now time.Time) (string, error) {
	g := f.Group
	if g.Key == "" {
		g = Grouping{Key: ownGroupKey(f.SourceID, f.Fingerprint)}
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO alert_groups (group_key, labels, update_interval, created_at, notify_at, routing_rule_id, team_id, escalation_policy_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (group_key) WHERE status = 'open' DO UPDATE SET notify_at = LEAST(
			COALESCE(alert_groups.notify_at, 'infinity'),
			CASE WHEN alert_groups.notified_at IS NULL THEN EXCLUDED.notify_at
				ELSE GREATEST(EXCLUDED.created_at, alert_groups.notified_at + alert_groups.update_interval) END)
		RETURNING id::text`,
		g.Key, nonNil(g.Labels), max(g.Interval, 0), now, now.Add(max(g.Wait, 0)),
		nullable(g.RoutingRuleID), nullable(g.TeamID), nullable(g.EscalationPolicyID)).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("join alert group: %w", err)
	}
	return id, nil
}

// afterResolve brings a group up to date when one of its alerts has been
// resolved: the group is resolved once all its alerts are, and its next
// notification is planned.
//
//   - A group that was never notified and is now resolved will never be: the
//     problem went away before anyone was told.
//   - A resolution reported by a source is told about when the group's
//     interval allows.
//   - A resolution by a person is told about at once, by the observer that
//     queues it, so a notification that was still due for a resolved group is
//     dropped.
func (s *Store) afterResolve(ctx context.Context, tx pgx.Tx, groupID string, bySource bool, now time.Time) error {
	// The row is locked first, in a statement of its own, so that the next
	// one sees an alert that joined the group a moment ago.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM alert_groups WHERE id = $1 FOR UPDATE`, groupID); err != nil {
		return fmt.Errorf("lock alert group: %w", err)
	}
	_, err := tx.Exec(ctx, `
		UPDATE alert_groups g SET
			status = CASE WHEN r.done THEN 'resolved' ELSE g.status END,
			resolved_at = CASE WHEN r.done THEN $2 ELSE g.resolved_at END,
			notify_at = CASE
				WHEN g.notified_at IS NULL AND r.done THEN NULL
				WHEN g.notified_at IS NULL THEN g.notify_at
				WHEN $3 THEN LEAST(COALESCE(g.notify_at, 'infinity'), GREATEST($2, g.notified_at + g.update_interval))
				WHEN r.done THEN NULL
				ELSE g.notify_at END,
			silence_id = CASE WHEN r.done THEN NULL ELSE g.silence_id END
		FROM (SELECT NOT EXISTS (SELECT 1 FROM alerts WHERE group_id = $1 AND status <> 'resolved') AS done) r
		WHERE g.id = $1`, groupID, now, bySource)
	if err != nil {
		return fmt.Errorf("update alert group: %w", err)
	}
	return nil
}

const groupColumns = `id::text, group_key, status, labels, created_at, notify_at, notified_at, resolved_at,
	COALESCE(routing_rule_id::text, ''), COALESCE(team_id::text, ''), COALESCE(escalation_policy_id::text, ''),
	notified_alerts, COALESCE(silence_id::text, '')`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Key, &g.Status, &g.Labels, &g.CreatedAt, &g.NotifyAt, &g.NotifiedAt, &g.ResolvedAt,
		&g.RoutingRuleID, &g.TeamID, &g.EscalationPolicyID, &g.NotifiedAlerts, &g.SilenceID)
	return g, err
}

// Group returns one group.
func (s *Store) Group(ctx context.Context, groupID string) (Group, error) {
	g, err := scanGroup(s.db.QueryRow(ctx, `SELECT `+groupColumns+` FROM alert_groups WHERE id = $1`, groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	return g, err
}

// retryDueGroupAfter is how long a group whose notification could not be
// queued waits before it is tried again. Without the wait it would be first
// in line every time, and hold up every other group.
const retryDueGroupAfter = 30 * time.Second

// Outcome is what became of a group whose notification was due.
type Outcome struct {
	// Notified says that the group's notification was dealt with: queued,
	// or found to have nobody to go to. The group counts as notified from
	// now on. When it is false nothing was told, and nothing is remembered
	// but RetryAt and SilenceID.
	Notified bool
	// Shown lists the alerts the notification shows, with what each is
	// shown as. It is added to what the group remembers.
	Shown map[string]string
	// RetryAt is when the group is due again, when not zero: the end of the
	// silence SilenceID, which keeps some of its alerts quiet.
	RetryAt   time.Time
	SilenceID string
}

// NotifyDueGroup takes the group whose notification has been due longest, if
// there is one, and calls notify for it and its alerts inside a transaction
// that holds the group. What notify returns is recorded on the group, and
// what it queued through tx commits with that. found is false when no group
// is due.
//
// It is safe to call from several replicas at once: each due group is handed
// to one caller, because a group that another caller holds is skipped. A
// process that dies in the middle leaves the group due.
func (s *Store) NotifyDueGroup(ctx context.Context, notify func(ctx context.Context, tx pgx.Tx, g Group, alerts []Alert) (Outcome, error)) (found bool, err error) {
	var failed string
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		now := s.now()
		g, err := scanGroup(tx.QueryRow(ctx, `SELECT `+groupColumns+` FROM alert_groups
			WHERE notify_at <= $1 ORDER BY notify_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		alerts, err := collectAlerts(tx.Query(ctx, `SELECT `+alertColumns+` FROM alerts WHERE group_id = $1 ORDER BY started_at, id`, g.ID))
		if err != nil {
			return fmt.Errorf("read the alerts of group %s: %w", g.ID, err)
		}
		out, err := notify(ctx, tx, g, alerts)
		if err != nil {
			failed = g.ID
			return fmt.Errorf("notify about group %s: %w", g.ID, err)
		}
		var retryAt *time.Time
		if !out.RetryAt.IsZero() {
			retryAt = &out.RetryAt
		}
		_, err = tx.Exec(ctx, `
			UPDATE alert_groups SET notify_at = $2, silence_id = $3,
				notified_at = CASE WHEN $4 THEN $5 ELSE notified_at END,
				notified_alerts = CASE WHEN $4 THEN notified_alerts || $6::jsonb ELSE notified_alerts END
			WHERE id = $1`, g.ID, retryAt, nullable(out.SilenceID), out.Notified, now, nonNil(out.Shown))
		return err
	})
	if failed != "" {
		// Best effort: if this fails too, the group is simply tried again.
		_, _ = s.db.Exec(context.WithoutCancel(ctx), `UPDATE alert_groups SET notify_at = $2 WHERE id = $1 AND notify_at IS NOT NULL`,
			failed, s.now().Add(retryDueGroupAfter))
	}
	return found, err
}

// WakeGroupsWaitingFor makes the notification of every group that waits for
// the end of a silence due now. Call it, through the transaction that ends
// the silence early, so that what the silence held back is told at once and
// not at the end the silence used to have.
func (s *Store) WakeGroupsWaitingFor(ctx context.Context, tx pgx.Tx, silenceID string) error {
	now := s.now()
	_, err := tx.Exec(ctx, `UPDATE alert_groups SET notify_at = $2 WHERE silence_id = $1 AND notify_at > $2`, silenceID, now)
	if err != nil {
		return fmt.Errorf("wake the groups that wait for silence %s: %w", silenceID, err)
	}
	return nil
}
