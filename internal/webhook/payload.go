package webhook

import (
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
)

// Version is the version of the payload. It changes when a field is removed
// or changes its meaning; fields may be added without a new version.
// docs/outbound-webhook.md is the contract.
const Version = "1"

// MaxAlerts bounds how many alerts of a group a payload lists. The rest are
// counted.
const MaxAlerts = 50

// Payload is the body of a request. It describes the group as it is when
// the request is made.
type Payload struct {
	Version string `json:"version"`
	// ID identifies the notification. It is the same in every attempt to
	// deliver it, so a receiver that has seen it can ignore a repeat.
	ID string `json:"id"`
	// Reason says why the notification was queued.
	Reason notify.Reason `json:"reason"`
	SentAt time.Time     `json:"sent_at"`
	Group  Group         `json:"group"`
	// Alerts are the alerts of the group, those that ask most of the reader
	// first, at most MaxAlerts of them.
	Alerts []Alert `json:"alerts"`
	// AlertsOmitted counts the alerts that did not fit.
	AlertsOmitted int `json:"alerts_omitted"`
	// LastChange is the latest state change of the alert the group's status
	// is taken from, when there is one on the timeline.
	LastChange *Change `json:"last_change,omitempty"`
	// Paged lists the people the escalation policy has paged so far, or
	// meant to.
	Paged []Person `json:"paged"`
}

// Group is the alert group a payload is about.
type Group struct {
	ID string `json:"id"`
	// Status is firing while any alert of the group fires, then
	// acknowledged, then snoozed, and resolved once all are.
	Status alert.Status `json:"status"`
	// Severity is the highest level among the alerts that are not resolved.
	Severity alert.Severity `json:"severity"`
	// Title is what the group is called: the summary of its alert, or the
	// label values its alerts share.
	Title string `json:"title"`
	// Labels are the labels the alerts of the group share.
	Labels map[string]string `json:"labels"`
	// Counts says how many alerts are in each state.
	Counts map[alert.Status]int `json:"counts"`
}

// Alert is one alert of the group.
type Alert struct {
	ID          string            `json:"id"`
	Fingerprint string            `json:"fingerprint"`
	Status      alert.Status      `json:"status"`
	Severity    alert.Severity    `json:"severity"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	// Links are the web addresses the source sent with the alert, by name.
	Links          map[string]string `json:"links"`
	Service        string            `json:"service,omitempty"`
	StartedAt      time.Time         `json:"started_at"`
	AcknowledgedAt *time.Time        `json:"acknowledged_at,omitempty"`
	SnoozedUntil   *time.Time        `json:"snoozed_until,omitempty"`
	ResolvedAt     *time.Time        `json:"resolved_at,omitempty"`
	// URL is the address of the alert in Beacon, when Beacon knows its own.
	URL string `json:"url,omitempty"`
}

// Change is a state change and who made it.
type Change struct {
	// Type is fired, acknowledged, snoozed, snooze_ended or resolved.
	Type alert.EventType `json:"type"`
	At   time.Time       `json:"at"`
	// ActorType is user, api_token, source or system, and ActorName what to
	// call the actor: "the source", "Beacon", or a person's name.
	ActorType alert.ActorType `json:"actor_type"`
	ActorName string          `json:"actor_name"`
}

// Person is someone the escalation policy paged, or meant to.
type Person struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	// NotConnected lists the services in which the person has no linked
	// account and through which they were to be paged. They were not
	// notified there.
	NotConnected []string `json:"not_connected"`
}

// build describes the group of m. alertURL returns the address of an alert
// in Beacon, or nothing.
func build(m notify.Message, now time.Time, alertURL func(id string) string) Payload {
	p := Payload{
		Version: Version, ID: m.Notification.ID, Reason: m.Notification.Reason, SentAt: now.UTC(),
		Group:  Group{ID: m.Notification.GroupID, Status: alert.StatusResolved, Labels: nonNil(m.Group.Labels), Counts: map[alert.Status]int{}},
		Alerts: []Alert{}, Paged: []Person{},
	}
	for _, status := range notify.StatusOrder {
		p.Group.Counts[status] = 0
	}
	for _, person := range notify.Paged(m) {
		missing := person.Missing
		if missing == nil {
			missing = []string{}
		}
		p.Paged = append(p.Paged, Person{Email: person.Email, Name: person.Name, NotConnected: missing})
	}
	if len(m.Alerts) == 0 {
		return p
	}
	p.Group.Status = notify.GroupStatus(m.Alerts)
	p.Group.Severity = notify.WorstSeverity(m.Alerts)
	representative := notify.Representative(m.Alerts, p.Group.Status)
	p.Group.Title = notify.AlertTitle(representative)
	if len(m.Alerts) > 1 {
		p.Group.Title = notify.GroupTitle(m.Group.Labels)
	}
	for _, a := range m.Alerts {
		p.Group.Counts[a.Status]++
	}
	for i, a := range notify.ByUrgency(m.Alerts) {
		if i == MaxAlerts {
			p.AlertsOmitted = len(m.Alerts) - MaxAlerts
			break
		}
		p.Alerts = append(p.Alerts, Alert{
			ID: a.ID, Fingerprint: a.Fingerprint, Status: a.Status, Severity: a.Severity,
			Labels: nonNil(a.Labels), Annotations: nonNil(a.Annotations), Links: nonNil(a.Links), Service: a.Service,
			StartedAt: a.StartedAt.UTC(), AcknowledgedAt: utc(a.AcknowledgedAt), SnoozedUntil: utc(a.SnoozedUntil), ResolvedAt: utc(a.ResolvedAt),
			URL: alertURL(a.ID),
		})
	}
	if last := notify.LastChange(representative, m.Events); last != nil {
		p.LastChange = &Change{Type: last.Type, At: last.OccurredAt.UTC(), ActorType: last.Actor.Type, ActorName: notify.ActorName(last)}
	}
	return p
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
