package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
)

// The routes of alerts and of their groups.
const (
	AlertsPath = "/api/v1/alerts"
	GroupsPath = "/api/v1/alert-groups"
)

// MaxSnooze bounds how long an alert may be snoozed through the API. A
// snooze that never ends is an alert nobody will see again.
const MaxSnooze = 30 * config.Day

// AlertStore is what the alerts API needs from the alert store.
type AlertStore interface {
	List(ctx context.Context, f alert.Filter) ([]alert.View, error)
	View(ctx context.Context, alertID string) (alert.View, error)
	Views(ctx context.Context, groupID string) ([]alert.View, error)
	GroupView(ctx context.Context, groupID string) (alert.GroupView, error)
	Events(ctx context.Context, alertID string, after int64, limit int) ([]alert.Event, bool, error)
	Apply(ctx context.Context, alertID string, action alert.Action, actor alert.Actor, opts alert.Options) (alert.Alert, error)
	ApplyToGroup(ctx context.Context, groupID string, action alert.Action, actor alert.Actor, opts alert.Options) ([]alert.Alert, error)
}

// Alerts serves the alerts API.
type Alerts struct {
	logger *slog.Logger
	store  AlertStore
	now    func() time.Time
}

// NewAlerts returns a handler that reads and changes alerts through store.
func NewAlerts(logger *slog.Logger, store AlertStore) *Alerts {
	return &Alerts{logger: logger, store: store, now: time.Now}
}

// SetClock replaces the clock, so tests can move time.
func (h *Alerts) SetClock(now func() time.Time) { h.now = now }

// Routes are the routes of alerts. All are open to members: seeing alerts
// and responding to them is what being on call is. Whoever acts is recorded
// on the timeline, as in chat.
func (h *Alerts) Routes() []access.Route {
	routes := []access.Route{
		{Pattern: "GET " + AlertsPath, Level: access.Member, Handler: http.HandlerFunc(h.list)},
		{Pattern: "GET " + AlertsPath + "/{id}", Level: access.Member, Handler: http.HandlerFunc(h.get)},
		{Pattern: "GET " + AlertsPath + "/{id}/timeline", Level: access.Member, Handler: http.HandlerFunc(h.timeline)},
		{Pattern: "GET " + GroupsPath + "/{id}", Level: access.Member, Handler: http.HandlerFunc(h.group)},
	}
	for _, action := range []alert.Action{alert.ActionAcknowledge, alert.ActionSnooze, alert.ActionResolve} {
		routes = append(routes,
			access.Route{Pattern: "POST " + AlertsPath + "/{id}/" + string(action), Level: access.Member, Handler: h.act(action, false)},
			access.Route{Pattern: "POST " + GroupsPath + "/{id}/" + string(action), Level: access.Member, Handler: h.act(action, true)},
		)
	}
	return routes
}

// alertBody is an alert, as the API answers.
type alertBody struct {
	ID          string            `json:"id"`
	GroupID     string            `json:"group_id"`
	Source      string            `json:"source"`
	Fingerprint string            `json:"fingerprint"`
	Status      alert.Status      `json:"status"`
	Severity    alert.Severity    `json:"severity"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	// Links are the web addresses the source sent with the alert, by name.
	Links map[string]string `json:"links"`
	// Team, Service and RoutingRule are what routing assigned, or null.
	Team           *string    `json:"team"`
	Service        *string    `json:"service"`
	RoutingRule    *string    `json:"routing_rule"`
	StartedAt      time.Time  `json:"started_at"`
	LastReceivedAt time.Time  `json:"last_received_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
	SnoozedUntil   *time.Time `json:"snoozed_until"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	// Actions are what can be done to the alert in its state.
	Actions []alert.Action `json:"actions"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// actionsOf lists what the state machine allows for an alert.
func actionsOf(status alert.Status) []alert.Action {
	out := []alert.Action{}
	for _, action := range []alert.Action{alert.ActionAcknowledge, alert.ActionSnooze, alert.ActionResolve} {
		if _, err := alert.Next(status, action); err == nil {
			out = append(out, action)
		}
	}
	return out
}

func alertOf(v alert.View) alertBody {
	return alertBody{
		ID: v.ID, GroupID: v.GroupID, Source: v.Source, Fingerprint: v.Fingerprint, Status: v.Status, Severity: v.Severity,
		Labels: nonNil(v.Labels), Annotations: nonNil(v.Annotations), Links: nonNil(v.Links),
		Team: optional(v.Team), Service: optional(v.Service), RoutingRule: optional(v.RoutingRule),
		StartedAt: v.StartedAt.UTC(), LastReceivedAt: v.LastReceivedAt.UTC(),
		AcknowledgedAt: utc(v.AcknowledgedAt), SnoozedUntil: utc(v.SnoozedUntil), ResolvedAt: utc(v.ResolvedAt),
		Actions: actionsOf(v.Status),
	}
}

// alertCursor is where a list of alerts continues.
type alertCursor struct {
	StartedAt time.Time `json:"t"`
	ID        string    `json:"id"`
}

// many reads a filter that may be given several times or as a list with
// commas: ?status=firing&status=acknowledged or ?status=firing,acknowledged.
func many(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (h *Alerts) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for name := range q {
		switch name {
		case "status", "severity", "team", "service", "source", "group", "limit", "cursor":
		default:
			Fail(h.logger, w, r, http.StatusBadRequest, name, "is not a filter of this list; the filters are status, severity, team, service, source and group")
			return
		}
	}
	f := alert.Filter{Team: q.Get("team"), Service: q.Get("service"), Source: q.Get("source"), GroupID: q.Get("group")}
	for _, s := range many(q["status"]) {
		switch status := alert.Status(s); status {
		case alert.StatusFiring, alert.StatusAcknowledged, alert.StatusSnoozed, alert.StatusResolved:
			f.Statuses = append(f.Statuses, status)
		case "open":
			// Everything that is not resolved.
			f.Statuses = append(f.Statuses, alert.StatusFiring, alert.StatusAcknowledged, alert.StatusSnoozed)
		default:
			Fail(h.logger, w, r, http.StatusBadRequest, "status", "must be firing, acknowledged, snoozed, resolved or open")
			return
		}
	}
	for _, s := range many(q["severity"]) {
		severity := alert.Severity(s)
		if !severity.Valid() {
			Fail(h.logger, w, r, http.StatusBadRequest, "severity", "must be critical, warning or info")
			return
		}
		f.Severities = append(f.Severities, severity)
	}
	if f.GroupID != "" && !IsUUID(f.GroupID) {
		Fail(h.logger, w, r, http.StatusBadRequest, "group", "must be the identifier of an alert group")
		return
	}
	limit, problem, ok := Limit(q)
	if !ok {
		Fail(h.logger, w, r, http.StatusBadRequest, "limit", problem)
		return
	}
	var c alertCursor
	present, err := ParseCursor(q.Get("cursor"), &c)
	if err != nil || (present && !IsUUID(c.ID)) {
		Fail(h.logger, w, r, http.StatusBadRequest, "cursor", ErrCursor.Error())
		return
	}
	if present {
		f.After = &alert.Position{StartedAt: c.StartedAt, ID: c.ID}
	}
	// One more than asked for, to know whether there is more.
	f.Limit = limit + 1
	views, err := h.store.List(r.Context(), f)
	if err != nil {
		Internal(h.logger, w, r, "list alerts", err)
		return
	}
	body := struct {
		Alerts     []alertBody `json:"alerts"`
		NextCursor string      `json:"next_cursor,omitempty"`
	}{Alerts: make([]alertBody, 0, min(len(views), limit))}
	for i, v := range views {
		if i == limit {
			last := views[limit-1]
			body.NextCursor = Cursor(alertCursor{StartedAt: last.StartedAt, ID: last.ID})
			break
		}
		body.Alerts = append(body.Alerts, alertOf(v))
	}
	Write(h.logger, w, r, http.StatusOK, body)
}

func (h *Alerts) get(w http.ResponseWriter, r *http.Request) {
	v, err := h.store.View(r.Context(), r.PathValue("id"))
	if errors.Is(err, alert.ErrNotFound) {
		Fail(h.logger, w, r, http.StatusNotFound, "", "no such alert")
		return
	}
	if err != nil {
		Internal(h.logger, w, r, "read alert", err)
		return
	}
	Write(h.logger, w, r, http.StatusOK, alertOf(v))
}

// eventBody is an entry of a timeline, as the API answers.
type eventBody struct {
	ID int64 `json:"id"`
	// AlertID is null for an event about the whole group, such as a
	// notification.
	AlertID    *string         `json:"alert_id"`
	GroupID    string          `json:"group_id"`
	Type       alert.EventType `json:"type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Actor      actorBody       `json:"actor"`
	// Data holds details specific to the type.
	Data map[string]any `json:"data"`
}

// actorBody is who or what caused an event.
type actorBody struct {
	Type alert.ActorType `json:"type"`
	// ID is the Beacon identifier of the user, token or source, or null.
	ID *string `json:"id"`
	// Name is what to call the actor, or null.
	Name *string `json:"name"`
	// Provider and ExternalID name the account in a chat service that a
	// person acted from, or are null.
	Provider   *string `json:"provider"`
	ExternalID *string `json:"external_id"`
}

func eventOf(e alert.Event) eventBody {
	data := map[string]any{}
	for k, v := range e.Data {
		// The name is the actor's, and is answered there.
		if k != "actor_name" {
			data[k] = v
		}
	}
	return eventBody{
		ID: e.ID, AlertID: optional(e.AlertID), GroupID: e.GroupID, Type: e.Type, OccurredAt: e.OccurredAt.UTC(), Data: data,
		Actor: actorBody{Type: e.Actor.Type, ID: optional(e.Actor.ID), Name: optional(e.Actor.Name), Provider: optional(e.Actor.Provider), ExternalID: optional(e.Actor.ExternalID)},
	}
}

// eventCursor is where a timeline continues: after the event with the ID.
type eventCursor struct {
	After int64 `json:"after"`
}

func (h *Alerts) timeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, problem, ok := Limit(q)
	if !ok {
		Fail(h.logger, w, r, http.StatusBadRequest, "limit", problem)
		return
	}
	var c eventCursor
	if _, err := ParseCursor(q.Get("cursor"), &c); err != nil || c.After < 0 {
		Fail(h.logger, w, r, http.StatusBadRequest, "cursor", ErrCursor.Error())
		return
	}
	events, known, err := h.store.Events(r.Context(), r.PathValue("id"), c.After, limit+1)
	if err != nil {
		Internal(h.logger, w, r, "read timeline", err)
		return
	}
	if !known {
		Fail(h.logger, w, r, http.StatusNotFound, "", "no such alert")
		return
	}
	body := struct {
		Events     []eventBody `json:"events"`
		NextCursor string      `json:"next_cursor,omitempty"`
	}{Events: make([]eventBody, 0, min(len(events), limit))}
	for i, e := range events {
		if i == limit {
			body.NextCursor = Cursor(eventCursor{After: events[limit-1].ID})
			break
		}
		body.Events = append(body.Events, eventOf(e))
	}
	Write(h.logger, w, r, http.StatusOK, body)
}

// groupBody is an alert group, as the API answers.
type groupBody struct {
	ID string `json:"id"`
	// Status is open, or resolved once every alert of the group is.
	Status alert.GroupStatus `json:"status"`
	// Labels are the label values the alerts of the group share.
	Labels map[string]string `json:"labels"`
	// Team, RoutingRule and EscalationPolicy are what routing assigned, or
	// null.
	Team             *string   `json:"team"`
	RoutingRule      *string   `json:"routing_rule"`
	EscalationPolicy *string   `json:"escalation_policy"`
	CreatedAt        time.Time `json:"created_at"`
	// NotifiedAt is when the group was last told about, or null when it has
	// not been yet.
	NotifiedAt *time.Time  `json:"notified_at"`
	ResolvedAt *time.Time  `json:"resolved_at"`
	Alerts     []alertBody `json:"alerts"`
	// Actions are what can be done to at least one alert of the group.
	Actions []alert.Action `json:"actions"`
}

func (h *Alerts) groupOf(ctx context.Context, id string) (groupBody, error) {
	g, err := h.store.GroupView(ctx, id)
	if err != nil {
		return groupBody{}, err
	}
	views, err := h.store.Views(ctx, id)
	if err != nil {
		return groupBody{}, err
	}
	body := groupBody{
		ID: g.ID, Status: g.Status, Labels: nonNil(g.Labels), Team: optional(g.Team), RoutingRule: optional(g.RoutingRule),
		EscalationPolicy: optional(g.EscalationPolicy), CreatedAt: g.CreatedAt.UTC(), NotifiedAt: utc(g.NotifiedAt), ResolvedAt: utc(g.ResolvedAt),
		Alerts: make([]alertBody, 0, len(views)), Actions: []alert.Action{},
	}
	allowed := map[alert.Action]bool{}
	for _, v := range views {
		a := alertOf(v)
		body.Alerts = append(body.Alerts, a)
		for _, action := range a.Actions {
			allowed[action] = true
		}
	}
	for _, action := range []alert.Action{alert.ActionAcknowledge, alert.ActionSnooze, alert.ActionResolve} {
		if allowed[action] {
			body.Actions = append(body.Actions, action)
		}
	}
	return body, nil
}

func (h *Alerts) group(w http.ResponseWriter, r *http.Request) {
	body, err := h.groupOf(r.Context(), r.PathValue("id"))
	if errors.Is(err, alert.ErrNotFound) {
		Fail(h.logger, w, r, http.StatusNotFound, "", "no such alert group")
		return
	}
	if err != nil {
		Internal(h.logger, w, r, "read alert group", err)
		return
	}
	Write(h.logger, w, r, http.StatusOK, body)
}

// snoozeRequest is the body of a request to snooze.
type snoozeRequest struct {
	// Until is when the snooze ends, in RFC 3339 form.
	Until *time.Time `json:"until"`
	// Duration is how long the snooze lasts, such as 1h, in place of Until.
	Duration string `json:"duration"`
}

const snoozeExample = `the body must be JSON such as {"duration": "1h"} or {"until": "2026-12-01T09:00:00Z"}`

// maxActionBytes bounds the body of a request that acts on an alert.
const maxActionBytes = 4096

// actorOf is who a request acts as: the signed-in user, or the API token.
func actorOf(p access.Principal) alert.Actor {
	if p.TokenID != "" {
		return alert.APIToken(p.TokenID, p.Name)
	}
	name := p.Name
	if name == "" {
		name = p.Email
	}
	return alert.User(p.UserID, name)
}

// act returns the handler that takes action on an alert, or, when group is
// set, on every alert of a group that allows it. It answers with the alert
// or the group as it is afterwards.
func (h *Alerts) act(action alert.Action, group bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		what, id := "alert", r.PathValue("id")
		if group {
			what = "alert group"
		}
		var opts alert.Options
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxActionBytes))
		if err != nil {
			Fail(h.logger, w, r, http.StatusRequestEntityTooLarge, "", "the body is larger than 4 KiB")
			return
		}
		if action == alert.ActionSnooze {
			var req snoozeRequest
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				Fail(h.logger, w, r, http.StatusBadRequest, "", snoozeExample)
				return
			}
			now := h.now()
			switch {
			case req.Until != nil && req.Duration != "":
				Fail(h.logger, w, r, http.StatusBadRequest, "duration", "give until or duration, not both")
				return
			case req.Until != nil:
				opts.SnoozeUntil = *req.Until
			case req.Duration != "":
				d, err := config.ParseDuration(req.Duration)
				if err != nil || d <= 0 {
					Fail(h.logger, w, r, http.StatusBadRequest, "duration", "must be a duration such as 30m, 2h or 1d")
					return
				}
				opts.SnoozeUntil = now.Add(time.Duration(d))
			default:
				Fail(h.logger, w, r, http.StatusBadRequest, "until", "until or duration is required: "+snoozeExample)
				return
			}
			field := "until"
			if req.Duration != "" {
				field = "duration"
			}
			if !opts.SnoozeUntil.After(now) {
				Fail(h.logger, w, r, http.StatusBadRequest, field, "a snooze must end in the future")
				return
			}
			if opts.SnoozeUntil.Sub(now) > MaxSnooze {
				Fail(h.logger, w, r, http.StatusBadRequest, field, "a snooze lasts at most 30 days")
				return
			}
		} else if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) {
			// Nothing in a body could mean anything here, and a caller who
			// sends one expects that it does.
			Fail(h.logger, w, r, http.StatusBadRequest, "", "this request takes no body")
			return
		}

		by, _ := access.FromContext(ctx)
		actor := actorOf(by)
		err = alert.ErrNotFound
		if IsUUID(id) {
			if group {
				_, err = h.store.ApplyToGroup(ctx, id, action, actor, opts)
			} else {
				_, err = h.store.Apply(ctx, id, action, actor, opts)
			}
		}
		var refused *alert.TransitionError
		switch {
		case errors.Is(err, alert.ErrNotFound):
			Fail(h.logger, w, r, http.StatusNotFound, "", "no such "+what)
			return
		case errors.As(err, &refused):
			// The request was understood; the state of the alert does not
			// allow it.
			Fail(h.logger, w, r, http.StatusConflict, "", "cannot "+string(refused.Action)+" an "+what+" that is "+string(refused.From))
			return
		case errors.Is(err, alert.ErrSnoozeUntil):
			Fail(h.logger, w, r, http.StatusBadRequest, "until", "a snooze must end in the future")
			return
		case err != nil:
			Internal(h.logger, w, r, string(action)+" "+what, err)
			return
		}
		h.logger.InfoContext(ctx, what+" changed through the API", slog.String("action", string(action)), slog.String("id", id),
			slog.String("by_user", by.UserID), slog.String("by_token", by.TokenID))

		if group {
			body, err := h.groupOf(ctx, id)
			if err != nil {
				Internal(h.logger, w, r, "read alert group", err)
				return
			}
			Write(h.logger, w, r, http.StatusOK, body)
			return
		}
		v, err := h.store.View(ctx, id)
		if err != nil {
			Internal(h.logger, w, r, "read alert", err)
			return
		}
		Write(h.logger, w, r, http.StatusOK, alertOf(v))
	})
}
