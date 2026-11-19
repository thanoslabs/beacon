package notify

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/thanoslabs/beacon/internal/alert"
)

// This file says what there is to tell about a group, in terms that every
// channel shares: which state it is shown in, what it is called, who was
// paged. How that is written out, in Block Kit, in an embed or in an email,
// is each channel's own business. Nothing here is formatted for a service,
// and nothing here is escaped: a channel escapes what it puts in a message.

// The annotations a message looks for.
const (
	AnnotationSummary     = "summary"
	AnnotationDescription = "description"
	AnnotationRunbook     = "runbook_url"
)

// StatusOrder ranks the states by how much they ask of the reader. A group
// is shown in the first state that any of its alerts has.
var StatusOrder = []alert.Status{alert.StatusFiring, alert.StatusAcknowledged, alert.StatusSnoozed, alert.StatusResolved}

// severityOrder ranks the levels, most severe first.
var severityOrder = []alert.Severity{alert.SeverityCritical, alert.SeverityWarning, alert.SeverityInfo}

// GroupStatus is the state a group is shown in: firing while any alert
// fires, then acknowledged, then snoozed, and resolved once all are.
func GroupStatus(alerts []alert.Alert) alert.Status {
	for _, status := range StatusOrder {
		for _, a := range alerts {
			if a.Status == status {
				return status
			}
		}
	}
	return alert.StatusResolved
}

// WorstSeverity is the highest level among the alerts that are not resolved,
// or among all of them once all are. alerts must not be empty.
func WorstSeverity(alerts []alert.Alert) alert.Severity {
	for _, open := range []bool{true, false} {
		for _, severity := range severityOrder {
			for _, a := range alerts {
				if a.Severity == severity && (!open || a.Status != alert.StatusResolved) {
					return severity
				}
			}
		}
	}
	return alerts[0].Severity
}

// Representative picks the alert whose last change a message reports: among
// the alerts in the group's state, the one that has fired longest, was
// acknowledged or resolved last, or whose snooze ends first. alerts must not
// be empty.
func Representative(alerts []alert.Alert, status alert.Status) alert.Alert {
	var best *alert.Alert
	better := func(a, b *alert.Alert) bool {
		switch status {
		case alert.StatusAcknowledged:
			return a.AcknowledgedAt != nil && (b.AcknowledgedAt == nil || a.AcknowledgedAt.After(*b.AcknowledgedAt))
		case alert.StatusSnoozed:
			return a.SnoozedUntil != nil && (b.SnoozedUntil == nil || a.SnoozedUntil.Before(*b.SnoozedUntil))
		case alert.StatusResolved:
			return a.ResolvedAt != nil && (b.ResolvedAt == nil || a.ResolvedAt.After(*b.ResolvedAt))
		default:
			return a.StartedAt.Before(b.StartedAt)
		}
	}
	for i := range alerts {
		if a := &alerts[i]; a.Status == status && (best == nil || better(a, best)) {
			best = a
		}
	}
	if best == nil {
		return alerts[0]
	}
	return *best
}

// GroupTitle names a group by the label values its alerts share: the alert
// name first, then the others in name order.
func GroupTitle(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		if name != "alertname" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var parts []string
	if v := labels["alertname"]; v != "" {
		parts = append(parts, v)
	}
	for _, name := range names {
		parts = append(parts, labels[name])
	}
	if len(parts) == 0 {
		return "Alerts"
	}
	return strings.Join(parts, " · ")
}

// AlertTitle is the one line that says what an alert is.
func AlertTitle(a alert.Alert) string {
	for _, s := range []string{a.Annotations[AnnotationSummary], a.Labels["alertname"]} {
		if s != "" {
			return s
		}
	}
	return "Alert " + a.Fingerprint
}

// CountLine says how many alerts a group has and in which states. It holds
// nothing that came from an alert.
func CountLine(alerts []alert.Alert) string {
	counts := map[alert.Status]int{}
	for _, a := range alerts {
		counts[a.Status]++
	}
	var parts []string
	for _, status := range StatusOrder {
		if n := counts[status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, status))
		}
	}
	return fmt.Sprintf("%d alerts: %s", len(alerts), strings.Join(parts, ", "))
}

// ByUrgency returns the alerts with those that ask most of the reader
// first: firing, then acknowledged, snoozed and resolved, each in the order
// they were given.
func ByUrgency(alerts []alert.Alert) []alert.Alert {
	ordered := make([]alert.Alert, 0, len(alerts))
	for _, status := range StatusOrder {
		for _, a := range alerts {
			if a.Status == status {
				ordered = append(ordered, a)
			}
		}
	}
	return ordered
}

// OwnLabels returns the labels of an alert that tell it apart from the
// others of its group: all but the ones the group shares.
func OwnLabels(a alert.Alert, shared map[string]string) map[string]string {
	own := make(map[string]string, len(a.Labels))
	for name, value := range a.Labels {
		if _, isShared := shared[name]; !isShared {
			own[name] = value
		}
	}
	return own
}

// LastChange returns the latest state change of the alert on the timeline,
// or nil.
func LastChange(a alert.Alert, events []alert.Event) *alert.Event {
	for i := len(events) - 1; i >= 0; i-- {
		e := &events[i]
		if e.AlertID != a.ID {
			continue
		}
		switch e.Type {
		case alert.EventFired, alert.EventAcknowledged, alert.EventSnoozed, alert.EventSnoozeEnded, alert.EventResolved:
			return e
		}
	}
	return nil
}

// ActorName names who caused an event, for a sentence such as "Resolved by
// …". A person's name comes from outside Beacon and is not escaped.
func ActorName(e *alert.Event) string {
	switch {
	case e == nil:
		return "someone"
	case e.Actor.Type == alert.ActorSource:
		return "the source"
	case e.Actor.Type == alert.ActorSystem:
		return "Beacon"
	case e.Actor.Name != "":
		return e.Actor.Name
	default:
		return "someone"
	}
}

// Actions returns what can be done to the group, in the order a message
// offers it: an action is offered when the state of at least one alert
// allows it. It is derived from the state machine, so what a message offers
// cannot drift from what is allowed.
func Actions(alerts []alert.Alert) []alert.Action {
	var out []alert.Action
	for _, action := range []alert.Action{alert.ActionAcknowledge, alert.ActionSnooze, alert.ActionResolve} {
		for _, a := range alerts {
			if _, err := alert.Next(a.Status, action); err == nil {
				out = append(out, action)
				break
			}
		}
	}
	return out
}

// PagedPerson is someone the escalation of a group has paged, or was meant
// to.
type PagedPerson struct {
	// Email is the person's address in Beacon, and Name what to call them:
	// their display name, or the address when they have none.
	Email string
	Name  string
	// Destination is where the integration that sends this message reached
	// the person, such as their user ID in the chat service. It is empty for
	// a person who was reached through another integration only, or not at
	// all.
	Destination string
	// Missing lists the services in which the person has no linked account
	// and through which they were to be paged, as people call them:
	// "Slack". They were not notified there.
	Missing []string
}

// ShownAs is how the person is named in plain text: by name, followed by the
// services they are not connected to, as in "Grace Hopper (Slack not
// connected)".
func (p PagedPerson) ShownAs() string {
	if len(p.Missing) == 0 {
		return p.Name
	}
	return NotConnected(p.Name, strings.Join(p.Missing, ", "))
}

// Paged returns everyone the escalation of the group has paged so far, in
// the order they were first named. It is read from the steps on the
// timeline, so it says what happened and not what the policy says now.
func Paged(m Message) []PagedPerson {
	var order []string
	people := map[string]*PagedPerson{}
	get := func(entry map[string]any) *PagedPerson {
		email, _ := entry["user"].(string)
		if email == "" {
			return nil
		}
		p, ok := people[email]
		if !ok {
			p = &PagedPerson{Email: email, Name: email}
			people[email] = p
			order = append(order, email)
		}
		if name, _ := entry["name"].(string); name != "" {
			p.Name = name
		}
		return p
	}
	entries := func(e alert.Event, key string) []map[string]any {
		list, _ := e.Data[key].([]any)
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if entry, ok := item.(map[string]any); ok {
				out = append(out, entry)
			}
		}
		return out
	}
	for _, e := range m.Events {
		if e.Type != EventEscalated {
			continue
		}
		for _, entry := range entries(e, "notified") {
			p := get(entry)
			if p == nil {
				continue
			}
			if id, _ := entry["destination"].(string); entry["integration_id"] == m.Notification.IntegrationID {
				p.Destination = id
			}
		}
		for _, entry := range entries(e, "skipped") {
			platform, _ := entry["not_connected"].(string)
			if p := get(entry); p != nil && platform != "" && !slices.Contains(p.Missing, platform) {
				p.Missing = append(p.Missing, platform)
			}
		}
	}
	out := make([]PagedPerson, 0, len(order))
	for _, email := range order {
		out = append(out, *people[email])
	}
	return out
}
