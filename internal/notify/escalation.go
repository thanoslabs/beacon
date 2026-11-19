package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/silence"
)

// This file is the escalation engine: it tells the targets of an escalation
// policy about an alert group, step by step, until someone responds.
// docs/escalation.md describes the rules; in short:
//
//   - A group is escalated from the moment of its first notification
//     (startEscalation). That runs the first step of its policy.
//   - Each step has a timeout. When it passes and an alert of the group is
//     still firing, the next step runs (EscalateDue). After the last step
//     the policy starts again, as often as it says, and then gives up.
//   - Escalation stops when no alert of the group is firing any more:
//     acknowledged, snoozed or resolved (Observe).
//   - A group is paged once. After its escalation has stopped, an alert that
//     joins the group pages nobody: the messages that exist are updated,
//     and that is all. The one exception is a snooze that runs out, which
//     starts the policy again, because being asked again is what a snooze
//     is for.
//
// Where a group is in its policy is a row in the escalations table, and its
// timer is a column of that row. Nothing is kept in the process.

// DefaultEscalationPollInterval is how often a process looks for escalation
// steps that are due.
const DefaultEscalationPollInterval = time.Second

// The events the engine writes on the timeline of a group.
const (
	// EventEscalated is written for every step that runs, with the policy,
	// the step, the pass, who was notified and who could not be.
	EventEscalated alert.EventType = "escalated"
	// EventEscalationExhausted is written when a policy has run all its
	// steps, as often as it repeats, and an alert is still firing.
	EventEscalationExhausted alert.EventType = "escalation_exhausted"
)

// The states of an escalation.
const (
	escalationActive       = "active"
	escalationAcknowledged = "acknowledged"
	escalationExhausted    = "exhausted"
	escalationResolved     = "resolved"
)

// Policy is an escalation policy as the pipeline runs it.
type Policy struct {
	// Name is the policy's name in the configuration.
	Name string
	// Repeat is how many more times all steps run after the first pass.
	Repeat int
	// Steps are run in order. A policy with none notifies nobody.
	Steps []Step
}

// Step notifies its targets, then waits Timeout for a response before the
// next step runs.
type Step struct {
	Targets []StepTarget
	Timeout time.Duration
}

// The kinds of target.
const (
	TargetChannel  = "channel"
	TargetSchedule = "schedule"
	TargetTeam     = "team"
	TargetUser     = "user"
)

// StepTarget is who a step notifies: a shared destination, or people.
type StepTarget struct {
	// Kind is one of the Target constants.
	Kind string
	// Name is what the configuration calls the target: the channel, the
	// name of the schedule or team, or the user's email address. It is what
	// the timeline shows.
	Name string
	// ID is the row of the schedule or the team.
	ID string
	// Via lists the integrations to notify through. A channel target has
	// one. For people it is every integration that reaches a person
	// directly, or the ones the configuration narrows it to.
	Via []Via
}

// Via is an integration that a target is notified through.
type Via struct {
	// IntegrationID is the row of the integration, and Name its name.
	IntegrationID string
	Name          string
	// Provider is the service behind the integration, such as "slack". It
	// is what a person is said not to be connected to.
	Provider string
}

// PlatformName returns what to call a service in a sentence for people:
// "Slack" for "slack". An integration whose service is not known is called
// by its own name.
func PlatformName(provider, integration string) string {
	switch provider {
	case "slack":
		return "Slack"
	case "discord":
		return "Discord"
	case "":
		return integration
	}
	return strings.ToUpper(provider[:1]) + provider[1:]
}

// NotConnected is how a person is shown who was to be paged through a
// service in which they have no linked account: by name, with the reason.
// They were not notified there, and whoever reads the timeline or a
// message about the group sees that they were meant to be.
func NotConnected(name, platform string) string {
	return name + " (" + platform + " not connected)"
}

// ChannelTarget returns the target for a shared destination.
func ChannelTarget(integrationID, integration, destination string) StepTarget {
	return StepTarget{Kind: TargetChannel, Name: destination, Via: []Via{{IntegrationID: integrationID, Name: integration}}}
}

// Person is a user whom a step notifies.
type Person struct {
	UserID string
	Email  string
	// Name is what to call the person: their display name, or their email
	// address when they have none.
	Name string
}

// nameOf returns what to call a person in an event, or nothing for a
// shared destination, which is nobody.
func nameOf(p Person) string {
	if p.Email == "" && p.Name == "" {
		return ""
	}
	return p.name()
}

func (p Person) name() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Email
}

// People is what the engine needs to know about people: who a target
// stands for at this moment, and where an integration reaches a person.
// Every method reads through tx, the transaction of the step.
type People interface {
	// OnCall returns who is on call for a schedule now, taking overrides
	// into account. It is empty when nobody is.
	OnCall(ctx context.Context, tx pgx.Tx, scheduleID string) ([]Person, error)
	// TeamMembers returns the members of a team.
	TeamMembers(ctx context.Context, tx pgx.Tx, teamID string) ([]Person, error)
	// User returns the user with an email address, if there is one.
	User(ctx context.Context, tx pgx.Tx, email string) (Person, bool, error)
	// Address returns where the integration reaches the person directly,
	// such as their user ID in Slack. found is false when the person has no
	// linked account there.
	Address(ctx context.Context, tx pgx.Tx, integrationID, userID string) (address string, found bool, err error)
}

// SetPeople gives the pipeline the directory with which it resolves the
// schedule, team and user targets of a step. Call it before the pipeline is
// used. Without one, people targets notify nobody.
func (p *Pipeline) SetPeople(people People) { p.people.Store(&people) }

// escalation is a row of the escalations table.
type escalation struct {
	groupID   string
	policyID  string
	stepIndex int
	pass      int
	silenceID string
}

// startEscalation begins telling people about a group. It runs once per
// group, when the group's wait is over, inside the transaction that marks
// the group as notified: it creates the group's escalation and runs the
// first step of its policy.
//
// The first step runs whatever state the alerts are in, because the group
// has something to tell and nobody has been told. Whether more steps follow
// depends on whether an alert is still firing.
func (p *Pipeline) startEscalation(ctx context.Context, tx pgx.Tx, g alert.Group, alerts []alert.Alert) error {
	now := p.now()
	policy, known := (*p.policies.Load())[g.EscalationPolicyID]
	var why string
	switch {
	case g.EscalationPolicyID == "":
		why = "the group has no escalation policy: the configuration had no routing block when it opened"
	case !known:
		why = "the escalation policy of the group is no longer in the configuration"
	case len(policy.Steps) == 0:
		why = fmt.Sprintf("escalation policy %q has no steps", policy.Name)
	}
	if why != "" {
		p.logger.WarnContext(ctx, "an alert group notifies nobody", slog.String("group_id", g.ID), slog.String("reason", why))
		return alert.AppendEvent(ctx, tx, &alert.Event{GroupID: g.ID, Type: EventNotNotified, Actor: alert.System(), OccurredAt: now,
			Data: map[string]any{"reason": why}})
	}

	esc := escalation{groupID: g.ID, policyID: g.EscalationPolicyID, stepIndex: 0}
	if err := p.runStep(ctx, tx, g, policy, esc); err != nil {
		return err
	}
	status, next := escalationActive, ptr(now.Add(policy.Steps[0].Timeout))
	if !anyFiring(alerts) {
		status, next = escalationAcknowledged, nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO escalations (alert_group_id, escalation_policy_id, status, step_index, pass, next_step_at, created_at, updated_at)
		VALUES ($1, $2, $3, 0, 0, $4, $5, $5)`, g.ID, g.EscalationPolicyID, status, next, now)
	if err != nil {
		return fmt.Errorf("create escalation: %w", err)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// anyFiring reports whether an alert is waiting for someone to respond.
func anyFiring(alerts []alert.Alert) bool {
	for _, a := range alerts {
		if a.Status == alert.StatusFiring {
			return true
		}
	}
	return false
}

// RunEscalations runs the steps of escalations as they fall due, until ctx
// is cancelled. Several replicas may run it at once; see EscalateDue.
func (p *Pipeline) RunEscalations(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := p.EscalateDue(ctx); err != nil && ctx.Err() == nil {
			p.logger.ErrorContext(ctx, "run due escalation steps", slog.Any("error", err))
		}
	}
}

// retryEscalationAfter is how long an escalation whose step could not be
// run waits before it is tried again, so that it does not hold up the
// others.
const retryEscalationAfter = 30 * time.Second

// EscalateDue deals with every escalation whose timer has run out, and
// reports how many it dealt with. Each is handled in one transaction that
// holds its row and moves its timer, so a step runs once however many
// replicas call this at the same time, and a process that dies in the
// middle leaves the step due.
func (p *Pipeline) EscalateDue(ctx context.Context) (n int, err error) {
	for ctx.Err() == nil {
		found, err := p.escalateOne(ctx)
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

func (p *Pipeline) escalateOne(ctx context.Context) (found bool, err error) {
	var failed string
	err = pgx.BeginFunc(ctx, p.db, func(tx pgx.Tx) error {
		// The row is read from its own table alone: a locking read that
		// waited sees the row as the other transaction left it, but would
		// pair it with joined rows as they were before.
		var esc escalation
		err := tx.QueryRow(ctx, `
			SELECT alert_group_id::text, escalation_policy_id::text, step_index, pass, COALESCE(silence_id::text, '')
			FROM escalations WHERE status = 'active' AND next_step_at <= $1
			ORDER BY next_step_at LIMIT 1 FOR UPDATE SKIP LOCKED`, p.now()).
			Scan(&esc.groupID, &esc.policyID, &esc.stepIndex, &esc.pass, &esc.silenceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read due escalation: %w", err)
		}
		found = true
		if err := p.escalate(ctx, tx, esc); err != nil {
			failed = esc.groupID
			return fmt.Errorf("escalate group %s: %w", esc.groupID, err)
		}
		return nil
	})
	if failed != "" {
		// Best effort: if this fails too, the escalation is simply tried
		// again.
		_, _ = p.db.Exec(context.WithoutCancel(ctx), `UPDATE escalations SET next_step_at = $2 WHERE alert_group_id = $1 AND status = 'active'`,
			failed, p.now().Add(retryEscalationAfter))
	}
	return found, err
}

// escalate decides what becomes of an escalation whose timer has run out,
// and does it: the next step, a wait for a silence, or the end.
func (p *Pipeline) escalate(ctx context.Context, tx pgx.Tx, esc escalation) error {
	now := p.now()
	g, alerts, err := p.alerts.GroupWithAlerts(ctx, tx, esc.groupID)
	if err != nil {
		return err
	}
	// Nobody needs to respond any more. The observer has normally said so
	// already, at the moment of the change.
	var firing []alert.Alert
	for _, a := range alerts {
		if a.Status == alert.StatusFiring {
			firing = append(firing, a)
		}
	}
	switch {
	case g.Status == alert.GroupResolved:
		return p.endEscalation(ctx, tx, esc, escalationResolved)
	case len(firing) == 0:
		return p.endEscalation(ctx, tx, esc, escalationAcknowledged)
	}

	// A silence keeps Beacon quiet about an alert, and that includes asking
	// more people about it. While every firing alert is silenced the step
	// waits for the silence that ends first.
	quiet, err := p.quiet(ctx, tx, firing, now)
	if err != nil {
		return err
	}
	if len(quiet) == len(firing) {
		var wait *silence.Silence
		for _, s := range quiet {
			if wait == nil || s.EndsAt.Before(wait.EndsAt) {
				wait = &s
			}
		}
		if wait.ID != esc.silenceID {
			err := alert.AppendEvent(ctx, tx, &alert.Event{GroupID: g.ID, Type: EventSilenced, Actor: alert.System(), OccurredAt: now,
				Data: map[string]any{"silence_id": wait.ID, "until": wait.EndsAt.UTC().Format(time.RFC3339), "escalation": true}})
			if err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE escalations SET next_step_at = $2, silence_id = $3, updated_at = $4 WHERE alert_group_id = $1`,
			g.ID, wait.EndsAt, wait.ID, now)
		return err
	}

	policy, known := (*p.policies.Load())[esc.policyID]
	if !known || len(policy.Steps) == 0 {
		return p.exhaust(ctx, tx, esc, "the escalation policy of the group is no longer in the configuration", policy)
	}
	// The policy may have fewer steps than when the last one ran: a reload
	// changes it for the escalations in progress too.
	esc.stepIndex++
	if esc.stepIndex >= len(policy.Steps) {
		esc.stepIndex, esc.pass = 0, esc.pass+1
	}
	if esc.pass > policy.Repeat {
		return p.exhaust(ctx, tx, esc, "", policy)
	}
	if err := p.runStep(ctx, tx, g, policy, esc); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE escalations SET step_index = $2, pass = $3, next_step_at = $4, silence_id = NULL, updated_at = $5 WHERE alert_group_id = $1`,
		g.ID, esc.stepIndex, esc.pass, now.Add(policy.Steps[esc.stepIndex].Timeout), now)
	if err != nil {
		return fmt.Errorf("move escalation: %w", err)
	}
	return nil
}

// endEscalation stops the timer of an escalation.
func (p *Pipeline) endEscalation(ctx context.Context, tx pgx.Tx, esc escalation, status string) error {
	_, err := tx.Exec(ctx, `UPDATE escalations SET status = $2, next_step_at = NULL, silence_id = NULL, updated_at = $3 WHERE alert_group_id = $1`,
		esc.groupID, status, p.now())
	if err != nil {
		return fmt.Errorf("end escalation: %w", err)
	}
	return nil
}

// exhaust ends an escalation that has nothing left to run while an alert is
// still firing, and says so on the timeline.
func (p *Pipeline) exhaust(ctx context.Context, tx pgx.Tx, esc escalation, why string, policy Policy) error {
	data := map[string]any{}
	if why == "" {
		why = fmt.Sprintf("every step of escalation policy %q ran, %s, and nobody responded", policy.Name, times(policy.Repeat+1))
		data["policy"], data["passes"] = policy.Name, policy.Repeat+1
	}
	data["reason"] = why
	p.logger.WarnContext(ctx, "escalation ended without a response", slog.String("group_id", esc.groupID), slog.String("reason", why))
	if err := alert.AppendEvent(ctx, tx, &alert.Event{GroupID: esc.groupID, Type: EventEscalationExhausted, Actor: alert.System(), OccurredAt: p.now(), Data: data}); err != nil {
		return err
	}
	return p.endEscalation(ctx, tx, esc, escalationExhausted)
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// notified is one notification that a step queued, and skipped one that it
// could not, as the timeline shows them.
type notified struct {
	Target string `json:"target"`
	// User is the email address of the person, and Name what to call them.
	// Both are absent for a shared destination.
	User          string `json:"user,omitempty"`
	Name          string `json:"name,omitempty"`
	Integration   string `json:"integration"`
	IntegrationID string `json:"integration_id"`
	Destination   string `json:"destination"`
}

type skipped struct {
	Target        string `json:"target"`
	User          string `json:"user,omitempty"`
	Name          string `json:"name,omitempty"`
	Integration   string `json:"integration,omitempty"`
	IntegrationID string `json:"integration_id,omitempty"`
	Reason        string `json:"reason"`
	// NotConnected is the service in which the person has no linked
	// account, when that is why they were skipped, and ShownAs how they are
	// named wherever Beacon says who was paged: "Grace Hopper (Slack not
	// connected)".
	NotConnected string `json:"not_connected,omitempty"`
	ShownAs      string `json:"shown_as,omitempty"`
}

// runStep notifies the targets of one step and records the step on the
// timeline. A target that cannot be notified is skipped, logged and named in
// the event; it never stops the others.
func (p *Pipeline) runStep(ctx context.Context, tx pgx.Tx, g alert.Group, policy Policy, esc escalation) error {
	step := policy.Steps[esc.stepIndex]
	channels := *p.channels.Load()
	var (
		sent    = []notified{}
		missed  = []skipped{}
		queued  = map[Target]bool{}
		people  People
		loadedP = p.people.Load()
	)
	if loadedP != nil {
		people = *loadedP
	}
	skip := func(s skipped) {
		missed = append(missed, s)
		p.logger.WarnContext(ctx, "escalation target not notified", slog.String("group_id", g.ID), slog.String("policy", policy.Name),
			slog.Int("step", esc.stepIndex+1), slog.String("target", s.Target), slog.String("user", s.User), slog.String("integration", s.Integration),
			slog.String("reason", s.Reason))
	}
	// deliver queues one notification, once per destination and step.
	deliver := func(label string, via Via, destination string, person Person) error {
		if _, delivers := channels[via.IntegrationID]; !delivers {
			skip(skipped{Target: label, User: person.Email, Name: nameOf(person), Integration: via.Name, IntegrationID: via.IntegrationID, Reason: "the integration does not deliver notifications"})
			return nil
		}
		t := Target{IntegrationID: via.IntegrationID, Destination: destination}
		if queued[t] {
			return nil
		}
		queued[t] = true
		sent = append(sent, notified{Target: label, User: person.Email, Name: nameOf(person), Integration: via.Name, IntegrationID: via.IntegrationID, Destination: destination})
		return p.Enqueue(ctx, tx, Notification{GroupID: g.ID, IntegrationID: via.IntegrationID, Destination: destination, Reason: ReasonEscalationStep, UserID: person.UserID})
	}

	for _, target := range step.Targets {
		label := target.Kind + " " + target.Name
		if target.Kind == TargetChannel {
			for _, via := range target.Via {
				if err := deliver(label, via, target.Name, Person{}); err != nil {
					return err
				}
			}
			continue
		}
		persons, none, err := resolve(ctx, tx, people, target)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", label, err)
		}
		if len(persons) == 0 {
			skip(skipped{Target: label, Reason: none})
			continue
		}
		if len(target.Via) == 0 {
			skip(skipped{Target: label, Reason: "no integration in the configuration can reach a person directly"})
			continue
		}
		for _, person := range persons {
			for _, via := range target.Via {
				if _, delivers := channels[via.IntegrationID]; !delivers {
					skip(skipped{Target: label, User: person.Email, Name: nameOf(person), Integration: via.Name, IntegrationID: via.IntegrationID, Reason: "the integration does not deliver notifications"})
					continue
				}
				address, found, err := people.Address(ctx, tx, via.IntegrationID, person.UserID)
				if err != nil {
					return fmt.Errorf("find where %s reaches %s: %w", via.Name, person.Email, err)
				}
				if !found {
					// Skipped for delivery, and still named: they were
					// meant to be paged.
					platform := PlatformName(via.Provider, via.Name)
					skip(skipped{Target: label, User: person.Email, Name: person.name(), Integration: via.Name, IntegrationID: via.IntegrationID,
						Reason: "the person has no linked account for this integration", NotConnected: platform, ShownAs: NotConnected(person.name(), platform)})
					continue
				}
				if err := deliver(label, via, address, person); err != nil {
					return err
				}
			}
		}
	}
	if len(sent) == 0 {
		p.logger.WarnContext(ctx, "an escalation step notified nobody", slog.String("group_id", g.ID), slog.String("policy", policy.Name), slog.Int("step", esc.stepIndex+1))
	}
	return alert.AppendEvent(ctx, tx, &alert.Event{GroupID: g.ID, Type: EventEscalated, Actor: alert.System(), OccurredAt: p.now(), Data: map[string]any{
		"policy":   policy.Name,
		"step":     esc.stepIndex + 1,
		"steps":    len(policy.Steps),
		"pass":     esc.pass + 1,
		"passes":   policy.Repeat + 1,
		"timeout":  config.Duration(step.Timeout).String(),
		"notified": sent,
		"skipped":  missed,
	}})
}

// resolve returns the people a target stands for now. When there are none,
// none says why.
func resolve(ctx context.Context, tx pgx.Tx, people People, target StepTarget) (persons []Person, none string, err error) {
	if people == nil {
		return nil, "people cannot be resolved", nil
	}
	switch target.Kind {
	case TargetSchedule:
		persons, err = people.OnCall(ctx, tx, target.ID)
		// A gap in coverage is a state a team may have chosen. The step
		// says so and goes on.
		none = "the schedule has nobody on call: a gap in coverage"
	case TargetTeam:
		persons, err = people.TeamMembers(ctx, tx, target.ID)
		none = "the team has no members who are users"
	case TargetUser:
		person, found, userErr := people.User(ctx, tx, target.Name)
		if found {
			persons = []Person{person}
		}
		err, none = userErr, "no user has this email address"
	default:
		none = "the kind of target is not known"
	}
	return persons, none, err
}

// observeEscalation keeps an escalation in step with the alerts of its
// group, in the transaction of the change:
//
//   - When no alert of the group is firing any more, because the last one
//     was acknowledged, snoozed or resolved, by a person or by the source,
//     the escalation stops and its timer is cleared.
//   - When a snooze runs out in a group whose escalation had stopped that
//     way, the escalation starts again from its first step, at the next
//     look of the worker. The person who snoozed asked to be reminded.
//   - When a new alert joins such a group, nothing happens here. One page
//     per alert group: the group was taken, and the alert that joins is
//     shown in the messages that exist, which the group's timer updates
//     without notifying anyone.
//
// SEAM FOR THE RE-PAGE POLICY (issue 100). A policy will be able to state an
// interval after which a group that is still not resolved is paged again,
// also after it was acknowledged or its policy ran out. That belongs here
// and in endEscalation: instead of clearing next_step_at when an escalation
// stops, set it to the re-page time and let escalate run the first step
// again when it falls due. Without that setting, the rule below stands: an
// alert that fires into a group that was taken does not restart anything.
func (p *Pipeline) observeEscalation(ctx context.Context, tx pgx.Tx, ev alert.Event) error {
	now := p.now()
	switch ev.Type {
	case alert.EventAcknowledged, alert.EventSnoozed, alert.EventResolved:
		_, err := tx.Exec(ctx, `
			UPDATE escalations e SET status = CASE WHEN g.status = 'resolved' THEN 'resolved' ELSE 'acknowledged' END,
				next_step_at = NULL, silence_id = NULL, updated_at = $2
			FROM alert_groups g
			WHERE g.id = e.alert_group_id AND e.alert_group_id = $1 AND e.status <> 'resolved'
				AND NOT EXISTS (SELECT 1 FROM alerts a WHERE a.group_id = e.alert_group_id AND a.status = 'firing')`, ev.GroupID, now)
		if err != nil {
			return fmt.Errorf("stop escalation: %w", err)
		}
	case alert.EventSnoozeEnded:
		_, err := tx.Exec(ctx, `
			UPDATE escalations SET status = 'active', step_index = -1, pass = 0, next_step_at = $2, updated_at = $2
			WHERE alert_group_id = $1 AND status = 'acknowledged'`, ev.GroupID, now)
		if err != nil {
			return fmt.Errorf("restart escalation: %w", err)
		}
	}
	return nil
}

// WakeEscalationsWaitingFor makes the next step of every escalation that
// waits for the end of a silence due now. Call it through the transaction
// that ends the silence early.
func (p *Pipeline) WakeEscalationsWaitingFor(ctx context.Context, tx pgx.Tx, silenceID string) error {
	now := p.now()
	_, err := tx.Exec(ctx, `UPDATE escalations SET next_step_at = $2, updated_at = $2 WHERE silence_id = $1 AND status = 'active' AND next_step_at > $2`, silenceID, now)
	if err != nil {
		return fmt.Errorf("wake the escalations that wait for silence %s: %w", silenceID, err)
	}
	return nil
}
