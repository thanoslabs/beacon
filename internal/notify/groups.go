package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
)

// DefaultGroupPollInterval is how often a process looks for groups whose
// notification is due.
const DefaultGroupPollInterval = time.Second

// This file decides when a notification about an alert group is queued.
// docs/notifications.md describes the rules; in short:
//
//   - What sources report (alerts that join a group, alerts that resolve) is
//     batched. A group has a time at which its next notification is due, kept
//     in Postgres by the alert store; NotifyDueGroups queues what has fallen
//     due. The first notification of a group waits for its group_wait, and
//     later ones keep its group_interval apart.
//   - What people do (acknowledge, snooze, resolve) and the end of a snooze
//     is told at once, by Observe, to every destination that has already
//     been told about the group.

// RunGroups queues the notifications of groups as they fall due, until ctx is
// cancelled. Several replicas may run it at once; see NotifyDueGroups.
func (p *Pipeline) RunGroups(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := p.NotifyDueGroups(ctx); err != nil && ctx.Err() == nil {
			p.logger.ErrorContext(ctx, "notify about due groups", slog.Any("error", err))
		}
	}
}

// NotifyDueGroups queues a notification for every group whose notification
// is due, and reports for how many it did. Each due group is handled in one
// transaction that also marks it as notified, so a group is notified once
// however many replicas call this at the same time, and a crash leaves it
// due.
func (p *Pipeline) NotifyDueGroups(ctx context.Context) (n int, err error) {
	for ctx.Err() == nil {
		found, err := p.alerts.NotifyDueGroup(ctx, p.notifyGroup)
		if err != nil {
			return n, err
		}
		if !found {
			break
		}
		n++
	}
	return n, ctx.Err()
}

// notifyGroup queues what is due for g: the first notification of a group
// that has had none, and otherwise an update for everyone who was told.
func (p *Pipeline) notifyGroup(ctx context.Context, tx pgx.Tx, g alert.Group) error {
	if g.NotifiedAt == nil {
		return p.startEscalation(ctx, tx, g)
	}
	reason := ReasonGroupUpdate
	if g.Status == alert.GroupResolved {
		reason = ReasonResolved
	}
	return p.enqueueUpdate(ctx, tx, g.ID, reason)
}

// startEscalation queues the first notification about a group.
//
// Temporary: until routing gives a group its escalation policy, every group
// is told to the targets set with SetTargets.
func (p *Pipeline) startEscalation(ctx context.Context, tx pgx.Tx, g alert.Group) error {
	for _, t := range *p.targets.Load() {
		err := p.Enqueue(ctx, tx, Notification{GroupID: g.ID, IntegrationID: t.IntegrationID, Destination: t.Destination, Reason: ReasonEscalationStep})
		if err != nil {
			return err
		}
	}
	return nil
}

// immediate maps the state changes that are told at once, when a person or
// Beacon itself made them, to whether they can end a group.
var immediate = map[alert.EventType]bool{
	alert.EventAcknowledged: true,
	alert.EventSnoozed:      true,
	alert.EventSnoozeEnded:  true,
	alert.EventResolved:     true,
}

// Observe is an alert observer that queues an update when a person, or
// Beacon itself, changes the state of an alert. Register it with
// alert.Store.Observe.
//
// It queues nothing for what a source reports: that is batched by the
// group's timer. It queues nothing for a group that has not been notified
// yet either: there is no message to update, and the first one will show the
// state as it is by then.
func (p *Pipeline) Observe(ctx context.Context, tx pgx.Tx, ev alert.Event, _ alert.Alert) error {
	if !immediate[ev.Type] || ev.Actor.Type == alert.ActorSource {
		return nil
	}
	var notified, resolved bool
	err := tx.QueryRow(ctx, `SELECT notified_at IS NOT NULL, status = 'resolved' FROM alert_groups WHERE id = $1`, ev.GroupID).Scan(&notified, &resolved)
	if err != nil {
		return fmt.Errorf("read alert group: %w", err)
	}
	if !notified {
		return nil
	}
	reason := ReasonGroupUpdate
	if resolved {
		reason = ReasonResolved
	}
	return p.enqueueUpdate(ctx, tx, ev.GroupID, reason)
}

// enqueueUpdate queues a notification about the group to every destination
// that has been told about it before.
//
// A message is built when it is delivered, so a notification that is still
// waiting in the queue will show this change anyway. Such a destination gets
// no second notification: acknowledging a group of fifty alerts is fifty
// state changes and one update. The waiting notifications are locked first,
// which keeps a worker from claiming one between the check and the commit of
// the change it has to show.
func (p *Pipeline) enqueueUpdate(ctx context.Context, tx pgx.Tx, groupID string, reason Reason) error {
	now := p.now()
	_, err := tx.Exec(ctx, `SELECT 1 FROM notifications
		WHERE alert_group_id = $1 AND status = 'pending' AND attempts = 0 FOR UPDATE`, groupID)
	if err != nil {
		return fmt.Errorf("lock waiting notifications: %w", err)
	}
	// A waiting update takes the newer reason. The first notification of a
	// group keeps its own.
	_, err = tx.Exec(ctx, `UPDATE notifications SET reason = $2
		WHERE alert_group_id = $1 AND status = 'pending' AND attempts = 0 AND reason <> $3`, groupID, reason, ReasonEscalationStep)
	if err != nil {
		return fmt.Errorf("update waiting notifications: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO notifications (alert_group_id, integration_id, destination, reason, next_attempt_at, created_at)
		SELECT DISTINCT n.alert_group_id, n.integration_id, n.destination, $2, $3::timestamptz, $3::timestamptz
		FROM notifications n
		WHERE n.alert_group_id = $1 AND NOT EXISTS (
			SELECT 1 FROM notifications w
			WHERE w.alert_group_id = n.alert_group_id AND w.integration_id = n.integration_id
				AND w.destination = n.destination AND w.status = 'pending' AND w.attempts = 0)`,
		groupID, reason, now)
	if err != nil {
		return fmt.Errorf("queue update: %w", err)
	}
	return nil
}
