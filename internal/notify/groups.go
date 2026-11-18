package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/silence"
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

// Silences is what the pipeline needs from the store of silences.
type Silences interface {
	// Active returns the silences in force at a time, read through q.
	Active(ctx context.Context, q silence.Querier, at time.Time) ([]silence.Silence, error)
}

// EventSilenced is written on the timeline of a group when a silence keeps
// its notification back, with the silence and when it ends.
const EventSilenced alert.EventType = "notification_silenced"

// quiet returns, for each alert that an active silence matches, the silence
// among them that ends first. It reads through q, which may be a
// transaction.
func (p *Pipeline) quiet(ctx context.Context, q silence.Querier, alerts []alert.Alert, at time.Time) (map[string]silence.Silence, error) {
	silences := *p.silences.Load()
	if silences == nil || len(alerts) == 0 {
		return nil, nil
	}
	active, err := silences.Active(ctx, q, at)
	if err != nil || len(active) == 0 {
		return nil, err
	}
	out := map[string]silence.Silence{}
	for _, a := range alerts {
		// Active returns the silence that ends first at the top.
		for _, s := range active {
			if s.Silences(a.Labels) {
				out[a.ID] = s
				break
			}
		}
	}
	return out, nil
}

// hidden reports whether an alert is kept out of notifications: a silence
// matches it, and no notification has shown it. An alert that people were
// told about before it was silenced stays in the group's message, so that
// the message does not keep showing it as it was.
func hidden(g alert.Group, a alert.Alert, quiet map[string]silence.Silence) bool {
	_, silenced := quiet[a.ID]
	_, shown := g.NotifiedAlerts[a.ID]
	return silenced && !shown
}

// notifyGroup decides what a due group gets, with silences taken into
// account, and queues it.
//
//   - A group that has had no notification gets its first when at least one
//     of its open alerts is not silenced.
//   - A group that has had one gets an update when there is news: an alert
//     that is not hidden and has not been shown, or has not been shown as it
//     is now. An alert that joins or resolves under a silence is no news.
//   - Whenever a silence keeps an open alert of the group quiet, the group
//     is due again when the silence that ends first ends, to see what there
//     is to tell then.
func (p *Pipeline) notifyGroup(ctx context.Context, tx pgx.Tx, g alert.Group, alerts []alert.Alert) (alert.Outcome, error) {
	now := p.now()
	quiet, err := p.quiet(ctx, tx, alerts, now)
	if err != nil {
		return alert.Outcome{}, err
	}
	var (
		out   alert.Outcome
		loud  bool
		news  bool
		shown = map[string]string{}
		wait  *silence.Silence
	)
	for _, a := range alerts {
		open := a.Status != alert.StatusResolved
		if s, silenced := quiet[a.ID]; silenced && open && (wait == nil || s.EndsAt.Before(wait.EndsAt)) {
			wait = &s
		}
		if hidden(g, a, quiet) {
			continue
		}
		loud = loud || open
		shown[a.ID] = alert.ShownAs(a)
		news = news || g.NotifiedAlerts[a.ID] != shown[a.ID]
	}
	if wait != nil {
		out.RetryAt, out.SilenceID = wait.EndsAt, wait.ID
	}

	switch {
	case g.NotifiedAt == nil && loud:
		err = p.startEscalation(ctx, tx, g)
	case g.NotifiedAt != nil && news:
		reason := ReasonGroupUpdate
		if g.Status == alert.GroupResolved {
			reason = ReasonResolved
		}
		err = p.enqueueUpdate(ctx, tx, g.ID, reason)
	default:
		// Nothing to tell. If that is because of a silence the group was
		// not waiting for already, the timeline says so, once.
		if wait != nil && wait.ID != g.SilenceID {
			err = alert.AppendEvent(ctx, tx, &alert.Event{GroupID: g.ID, Type: EventSilenced, Actor: alert.System(), OccurredAt: now,
				Data: map[string]any{"silence_id": wait.ID, "until": wait.EndsAt.UTC().Format(time.RFC3339)}})
		}
		return out, err
	}
	out.Notified, out.Shown = true, shown
	return out, err
}

// unsilenced returns the alerts of a group that a message about it shows:
// all but the hidden ones. When every alert is hidden, which happens when
// someone acts on a group whose alerts have all been silenced since, the
// message shows them all rather than nothing.
func (p *Pipeline) unsilenced(ctx context.Context, g alert.Group, alerts []alert.Alert) ([]alert.Alert, error) {
	quiet, err := p.quiet(ctx, p.db, alerts, p.now())
	if err != nil || len(quiet) == 0 {
		return alerts, err
	}
	shown := make([]alert.Alert, 0, len(alerts))
	for _, a := range alerts {
		if !hidden(g, a, quiet) {
			shown = append(shown, a)
		}
	}
	if len(shown) == 0 {
		return alerts, nil
	}
	return shown, nil
}

// Policy is what the pipeline needs to know of an escalation policy.
type Policy struct {
	// Name is the policy's name in the configuration.
	Name string
	// FirstStep lists the shared destinations that the first step of the
	// policy notifies: its {channel, integration} targets whose integration
	// has a channel that delivers.
	FirstStep []Target
}

// EventNotNotified is written on the timeline of a group whose first
// notification went to nobody, with the reason.
const EventNotNotified alert.EventType = "not_notified"

// startEscalation begins telling people about a group. It runs once per
// group, when the group's wait is over, inside the transaction that marks the
// group as notified.
//
// SEAM FOR THE ESCALATION ENGINE (milestone 5, issue 34). This function is
// the one place where a group starts to be escalated, and today it is the
// whole of escalation: it queues one notification for each channel target in
// the first step of the group's policy, and that is all. Later steps, step
// timeouts and repeats do not run, and the schedule, team and user targets
// of the first step are not resolved to people, so they are notified of
// nothing.
//
// The engine is expected to replace the body: insert the group's row in
// `escalations` here (policy, step 0, the step's timeout as next_step_at),
// queue the first step with Enqueue as below, including the people it
// resolves, and let its own worker run the later steps from that row. The
// rest of this file need not change: updates already go to everyone who was
// told, whoever told them.
func (p *Pipeline) startEscalation(ctx context.Context, tx pgx.Tx, g alert.Group) error {
	policy, known := (*p.policies.Load())[g.EscalationPolicyID]
	var why string
	switch {
	case g.EscalationPolicyID == "":
		why = "the group has no escalation policy: the configuration had no routing block when it opened"
	case !known:
		why = "the escalation policy of the group is no longer in the configuration"
	case len(policy.FirstStep) == 0:
		why = fmt.Sprintf("the first step of escalation policy %q names no channel that Beacon can deliver to", policy.Name)
	}
	if why != "" {
		p.logger.WarnContext(ctx, "an alert group notifies nobody", slog.String("group_id", g.ID), slog.String("reason", why))
		return alert.AppendEvent(ctx, tx, &alert.Event{GroupID: g.ID, Type: EventNotNotified, Actor: alert.System(), OccurredAt: p.now(),
			Data: map[string]any{"reason": why}})
	}
	for _, t := range policy.FirstStep {
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
