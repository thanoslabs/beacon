package alert

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file reads alerts for people: the lists and the single alerts that
// the API answers. Nothing here changes anything.

// View is an alert with the names of what it refers to, as a person reads
// it.
type View struct {
	Alert
	// Source is the name of the source. Team and RoutingRule are the names
	// of what routing assigned, or empty.
	Source      string
	Team        string
	RoutingRule string
}

// Position is where a list of alerts continues: after the alert that began
// at StartedAt and has this ID.
type Position struct {
	StartedAt time.Time
	ID        string
}

// Filter says which alerts a list holds. A field that is empty filters
// nothing.
type Filter struct {
	Statuses   []Status
	Severities []Severity
	// Team, Service and Source are names.
	Team    string
	Service string
	Source  string
	// GroupID is the group the alerts belong to.
	GroupID string
	// After continues a list from where the last one ended.
	After *Position
	// Limit bounds the list. It must be positive.
	Limit int
}

const viewColumns = `a.id::text, a.source_id::text, a.group_id::text, a.fingerprint, a.status, a.severity,
	a.labels, a.annotations, a.links, a.started_at, a.last_received_at, a.acknowledged_at, a.snoozed_until, a.resolved_at,
	COALESCE(a.routing_rule_id::text, ''), COALESCE(a.team_id::text, ''), COALESCE(a.service, ''),
	s.name, COALESCE(t.name, ''), COALESCE(r.name, '')`

const viewFrom = ` FROM alerts a JOIN sources s ON s.id = a.source_id
	LEFT JOIN teams t ON t.id = a.team_id LEFT JOIN routing_rules r ON r.id = a.routing_rule_id`

func scanView(row pgx.CollectableRow) (View, error) {
	var v View
	a := &v.Alert
	err := row.Scan(&a.ID, &a.SourceID, &a.GroupID, &a.Fingerprint, &a.Status, &a.Severity,
		&a.Labels, &a.Annotations, &a.Links, &a.StartedAt, &a.LastReceivedAt, &a.AcknowledgedAt, &a.SnoozedUntil, &a.ResolvedAt,
		&a.RoutingRuleID, &a.TeamID, &a.Service, &v.Source, &v.Team, &v.RoutingRule)
	return v, err
}

// List returns the alerts that f selects, newest first: by the time they
// began to fire, and by identifier among alerts that began together. The
// order is total, so After continues a list without a gap or a repeat even
// while alerts arrive.
func (s *Store) List(ctx context.Context, f Filter) ([]View, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if len(f.Statuses) > 0 {
		where = append(where, "a.status = ANY("+arg(f.Statuses)+")")
	}
	if len(f.Severities) > 0 {
		where = append(where, "a.severity = ANY("+arg(f.Severities)+")")
	}
	if f.Team != "" {
		where = append(where, "t.name = "+arg(f.Team))
	}
	if f.Service != "" {
		where = append(where, "a.service = "+arg(f.Service))
	}
	if f.Source != "" {
		where = append(where, "s.name = "+arg(f.Source))
	}
	if f.GroupID != "" {
		where = append(where, "a.group_id = "+arg(f.GroupID)+"::uuid")
	}
	if f.After != nil {
		where = append(where, "(a.started_at, a.id) < ("+arg(f.After.StartedAt)+", "+arg(f.After.ID)+"::uuid)")
	}
	query := `SELECT ` + viewColumns + viewFrom
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY a.started_at DESC, a.id DESC LIMIT " + arg(f.Limit)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanView)
	if notUUID(err) {
		// An identifier that is not one matches nothing.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	return out, nil
}

// View returns one alert with the names of what it refers to. An identifier
// that is not one is ErrNotFound, like one that no alert has.
func (s *Store) View(ctx context.Context, alertID string) (View, error) {
	rows, err := s.db.Query(ctx, `SELECT `+viewColumns+viewFrom+` WHERE a.id = $1`, alertID)
	if err != nil {
		return View{}, fmt.Errorf("read alert: %w", err)
	}
	v, err := pgx.CollectExactlyOneRow(rows, scanView)
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, fmt.Errorf("read alert: %w", err)
	}
	return v, nil
}

// Views returns the alerts of a group, oldest first, with the names of what
// they refer to.
func (s *Store) Views(ctx context.Context, groupID string) ([]View, error) {
	rows, err := s.db.Query(ctx, `SELECT `+viewColumns+viewFrom+` WHERE a.group_id = $1 ORDER BY a.started_at, a.id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("read the alerts of the group: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanView)
	if notUUID(err) {
		return nil, nil
	}
	return out, err
}

// GroupView is a group with the names of what it refers to.
type GroupView struct {
	Group
	Team             string
	RoutingRule      string
	EscalationPolicy string
}

// GroupView returns one group with the names of what it refers to. An
// identifier that is not one is ErrNotFound.
func (s *Store) GroupView(ctx context.Context, groupID string) (GroupView, error) {
	var v GroupView
	g := &v.Group
	err := s.db.QueryRow(ctx, `
		SELECT g.id::text, g.group_key, g.status, g.labels, g.created_at, g.notify_at, g.notified_at, g.resolved_at,
			COALESCE(g.routing_rule_id::text, ''), COALESCE(g.team_id::text, ''), COALESCE(g.escalation_policy_id::text, ''),
			COALESCE(t.name, ''), COALESCE(r.name, ''), COALESCE(p.name, '')
		FROM alert_groups g LEFT JOIN teams t ON t.id = g.team_id LEFT JOIN routing_rules r ON r.id = g.routing_rule_id
			LEFT JOIN escalation_policies p ON p.id = g.escalation_policy_id
		WHERE g.id = $1`, groupID).
		Scan(&g.ID, &g.Key, &g.Status, &g.Labels, &g.CreatedAt, &g.NotifyAt, &g.NotifiedAt, &g.ResolvedAt,
			&g.RoutingRuleID, &g.TeamID, &g.EscalationPolicyID, &v.Team, &v.RoutingRule, &v.EscalationPolicy)
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return GroupView{}, ErrNotFound
	}
	if err != nil {
		return GroupView{}, fmt.Errorf("read alert group: %w", err)
	}
	return v, nil
}

// Events returns a part of the timeline of an alert, in the order things
// happened: at most limit events after the event with the identifier after,
// including the events about the alert's group, such as notifications.
// known is false when there is no such alert.
func (s *Store) Events(ctx context.Context, alertID string, after int64, limit int) (events []Event, known bool, err error) {
	var groupID string
	err = s.db.QueryRow(ctx, `SELECT group_id::text FROM alerts WHERE id = $1`, alertID).Scan(&groupID)
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read alert: %w", err)
	}
	events, err = collectEvents(s.db.Query(ctx, `SELECT `+eventColumns+` FROM alert_events e
		WHERE e.alert_group_id = $2 AND (e.alert_id = $1 OR e.alert_id IS NULL) AND e.id > $3
		ORDER BY e.id LIMIT $4`, alertID, groupID, after, limit))
	if err != nil {
		return nil, true, fmt.Errorf("read timeline: %w", err)
	}
	return events, true, nil
}

// notUUID reports whether Postgres refused an identifier that is not a UUID.
func notUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// User is the actor for a signed-in person acting through the API.
func User(userID, name string) Actor {
	return Actor{Type: ActorUser, ID: userID, Name: name}
}

// APIToken is the actor for an API token.
func APIToken(tokenID, name string) Actor {
	return Actor{Type: ActorAPIToken, ID: tokenID, Name: name}
}
