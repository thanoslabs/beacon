// Package alert holds the alert model: what an alert is, the states it moves
// through, and the timeline that records every change.
//
// The state machine in this file is pure. Store applies it to Postgres.
package alert

import (
	"fmt"
	"time"
)

// Status is where an alert is in its lifecycle.
type Status string

// The alert states.
const (
	StatusFiring       Status = "firing"
	StatusAcknowledged Status = "acknowledged"
	StatusSnoozed      Status = "snoozed"
	StatusResolved     Status = "resolved"
)

// Severity is one of Beacon's fixed severity levels.
type Severity string

// The severity levels, most severe first.
const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Valid reports whether s is one of the fixed levels.
func (s Severity) Valid() bool {
	return s == SeverityCritical || s == SeverityWarning || s == SeverityInfo
}

// Action is something that moves an alert to another state.
type Action string

// The actions. A new alert starts in StatusFiring; no action leads there
// except the end of a snooze.
const (
	ActionAcknowledge Action = "acknowledge"
	ActionSnooze      Action = "snooze"
	ActionResolve     Action = "resolve"
	// ActionEndSnooze is taken by Beacon itself when a snooze runs out.
	ActionEndSnooze Action = "end_snooze"
)

// transitions lists every allowed move. Anything absent is invalid: a resolved
// alert never changes again, and no action leaves an alert where it was.
var transitions = map[Status]map[Action]Status{
	StatusFiring: {
		ActionAcknowledge: StatusAcknowledged,
		ActionSnooze:      StatusSnoozed,
		ActionResolve:     StatusResolved,
	},
	StatusAcknowledged: {
		ActionSnooze:  StatusSnoozed,
		ActionResolve: StatusResolved,
	},
	StatusSnoozed: {
		ActionAcknowledge: StatusAcknowledged,
		ActionEndSnooze:   StatusFiring,
		ActionResolve:     StatusResolved,
	},
	StatusResolved: {},
}

// TransitionError reports an action that is not allowed in the alert's
// current state.
type TransitionError struct {
	From   Status
	Action Action
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("cannot %s an alert that is %s", e.Action, e.From)
}

// Next returns the state that action leads to from status, or a
// *TransitionError when the move is not allowed.
func Next(from Status, action Action) (Status, error) {
	to, ok := transitions[from][action]
	if !ok {
		return "", &TransitionError{From: from, Action: action}
	}
	return to, nil
}

// Alert is one alert from a source.
type Alert struct {
	ID          string
	SourceID    string
	GroupID     string
	Fingerprint string
	Status      Status
	Severity    Severity
	Labels      map[string]string
	Annotations map[string]string
	// Links are the web addresses the source sent with the alert, by name:
	// see the Link constants.
	Links map[string]string
	// RoutingRuleID is the routing rule the alert matched when it was
	// opened. It is empty when the default applied.
	RoutingRuleID string
	// TeamID is the team the alert was assigned to, or empty.
	TeamID string
	// Service is the service the alert was assigned to, or empty.
	Service string

	StartedAt      time.Time
	LastReceivedAt time.Time
	AcknowledgedAt *time.Time
	SnoozedUntil   *time.Time
	ResolvedAt     *time.Time
}

// The names of the links an alert can carry. Which ones an alert has depends
// on its source.
const (
	// LinkGenerator is where the alert was generated: the expression in
	// Prometheus, or the alert rule in Grafana.
	LinkGenerator = "generator"
	// LinkDashboard and LinkPanel are the Grafana dashboard and panel of the
	// alert rule.
	LinkDashboard = "dashboard"
	LinkPanel     = "panel"
	// LinkSilence is the page that silences the alert at its source.
	LinkSilence = "silence"
)

// ActorType says what kind of thing caused an event.
type ActorType string

// The actor types.
const (
	ActorUser     ActorType = "user"
	ActorAPIToken ActorType = "api_token"
	ActorSource   ActorType = "source"
	ActorSystem   ActorType = "system"
)

// Actor is who or what caused an event.
type Actor struct {
	Type ActorType
	// ID is the Beacon identifier of the user, API token or source. It is
	// empty for the system, and for a person who acted from a chat service
	// and is not linked to a Beacon user yet.
	ID string
	// Provider and ExternalID identify a person in a chat service, for
	// example "slack" and a Slack user identifier. They are stored on the
	// event so it can be attributed to a Beacon user once identities are
	// linked.
	Provider   string
	ExternalID string
	// Name is a display name to show on the timeline. It is not an identity.
	Name string
}

// System is the actor for changes Beacon makes by itself.
func System() Actor { return Actor{Type: ActorSystem} }

// Source is the actor for changes reported by the source with this identifier.
func Source(id string) Actor { return Actor{Type: ActorSource, ID: id} }

// ChatUser is the actor for a person acting from a chat service, identified
// by their account there.
func ChatUser(provider, externalID, name string) Actor {
	return Actor{Type: ActorUser, Provider: provider, ExternalID: externalID, Name: name}
}

// LinkedChatUser is the actor for a person acting from a chat service whose
// account there is linked to a Beacon user. The event names both: the user,
// and the account they acted from.
func LinkedChatUser(userID, provider, externalID, name string) Actor {
	return Actor{Type: ActorUser, ID: userID, Provider: provider, ExternalID: externalID, Name: name}
}

// EventType names what happened in a timeline event.
type EventType string

// The event types written by this package. Other packages add their own, such
// as the notification events.
const (
	EventFired        EventType = "fired"
	EventAcknowledged EventType = "acknowledged"
	EventSnoozed      EventType = "snoozed"
	EventSnoozeEnded  EventType = "snooze_ended"
	EventResolved     EventType = "resolved"
)

// eventFor maps an action to the event that records it.
var eventFor = map[Action]EventType{
	ActionAcknowledge: EventAcknowledged,
	ActionSnooze:      EventSnoozed,
	ActionEndSnooze:   EventSnoozeEnded,
	ActionResolve:     EventResolved,
}

// Event is one entry on a timeline.
type Event struct {
	ID int64
	// AlertID is empty for an event about a whole group, such as a
	// notification.
	AlertID    string
	GroupID    string
	Type       EventType
	Actor      Actor
	OccurredAt time.Time
	// Data holds details specific to the type.
	Data map[string]any
}
