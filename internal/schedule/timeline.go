package schedule

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxTimelineRange bounds the period that Timeline covers.
const MaxTimelineRange = 366 * 24 * time.Hour

// Info is a schedule as the API shows it: what the files define, where the
// rotation is, and who is on call.
type Info struct {
	ID   string
	Name string
	// Team is the name of the owning team.
	Team     string
	Position Position
	OnCall   OnCall
}

// find returns the identifier of the schedule of the configuration with a
// name, or ErrNotFound.
func (s *Store) find(ctx context.Context, q Querier, name string) (id, team string, err error) {
	err = q.QueryRow(ctx, `SELECT s.id::text, t.name FROM schedules s JOIN teams t ON t.id = s.team_id
		WHERE s.name = $1 AND s.removed_at IS NULL AND EXISTS (SELECT 1 FROM schedule_positions p WHERE p.schedule_id = s.id)`, name).Scan(&id, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read schedule: %w", err)
	}
	return id, team, nil
}

// Get returns one schedule by name.
func (s *Store) Get(ctx context.Context, name string) (Info, error) {
	id, team, err := s.find(ctx, s.db, name)
	if err != nil {
		return Info{}, err
	}
	return s.info(ctx, id, name, team)
}

func (s *Store) info(ctx context.Context, id, name, team string) (Info, error) {
	out := Info{ID: id, Name: name, Team: team}
	var err error
	if out.Position, err = s.Position(ctx, id); err != nil {
		return Info{}, err
	}
	if out.OnCall, err = s.OnCall(ctx, s.db, id); err != nil {
		return Info{}, err
	}
	return out, nil
}

// List returns every schedule of the configuration, by name.
func (s *Store) List(ctx context.Context) ([]Info, error) {
	return s.list(ctx, `TRUE`)
}

// ErrTeamNotFound is returned when no team in the configuration has the
// given name.
var ErrTeamNotFound = errors.New("team not found")

// TeamOnCall says who is on call for a team now: the person on call for
// each schedule that the team owns, by schedule name. A team with no
// schedule has nobody.
func (s *Store) TeamOnCall(ctx context.Context, team string) ([]OnCall, error) {
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM teams WHERE name = $1 AND removed_at IS NULL)`, team).Scan(&exists); err != nil {
		return nil, fmt.Errorf("read team: %w", err)
	}
	if !exists {
		return nil, ErrTeamNotFound
	}
	infos, err := s.list(ctx, `t.name = $1`, team)
	if err != nil {
		return nil, err
	}
	out := make([]OnCall, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.OnCall)
	}
	return out, nil
}

// OnCallForTeam is TeamOnCall by the identifier of the team's row, read
// through q, for callers that hold a transaction.
func (s *Store) OnCallForTeam(ctx context.Context, q Querier, teamID string) ([]OnCall, error) {
	rows, err := q.Query(ctx, `SELECT s.id::text FROM schedules s
		WHERE s.team_id = $1 AND s.removed_at IS NULL AND EXISTS (SELECT 1 FROM schedule_positions p WHERE p.schedule_id = s.id)
		ORDER BY s.name`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read the schedules of the team: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the schedules of the team: %w", err)
	}
	out := make([]OnCall, 0, len(ids))
	for _, id := range ids {
		on, err := s.OnCall(ctx, q, id)
		if err != nil {
			return nil, err
		}
		out = append(out, on)
	}
	return out, nil
}

func (s *Store) list(ctx context.Context, where string, args ...any) ([]Info, error) {
	rows, err := s.db.Query(ctx, `SELECT s.id::text, s.name, t.name FROM schedules s JOIN teams t ON t.id = s.team_id
		WHERE s.removed_at IS NULL AND EXISTS (SELECT 1 FROM schedule_positions p WHERE p.schedule_id = s.id) AND `+where+`
		ORDER BY s.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	type row struct{ id, name, team string }
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.name, &x.team)
	})
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	out := make([]Info, 0, len(found))
	for _, x := range found {
		info, err := s.info(ctx, x.id, x.name, x.team)
		if errors.Is(err, ErrNotFound) {
			// It left the configuration a moment ago.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// TimelineShift is a stretch of time during which one person is, was or
// will be on call for a schedule, with overrides taken into account.
type TimelineShift struct {
	// User is the email address of the person on call.
	User     string
	StartsAt time.Time
	EndsAt   time.Time
	// Kind is KindRotation or KindOverride.
	Kind string
	// OverrideID is the override, for KindOverride.
	OverrideID string
	// Planned is false for time that has passed and is on record, and true
	// for a shift that has not ended: it is what will happen if nothing but
	// time changes.
	Planned bool
}

// Timeline returns who is on call for a schedule between from and to, in
// order: the shifts on record for the time that has passed, and for the
// time to come the shifts that the rotation will have and the overrides
// that exist, by the same arithmetic that will carry them out. Where an
// override covers the rotation, only the override is listed, so the result
// has one person at any moment. Time in which nobody is on call is left
// out.
//
// It returns an *Error when the period is not one, and ErrNotFound when no
// schedule has the name.
func (s *Store) Timeline(ctx context.Context, name string, from, to time.Time) ([]TimelineShift, error) {
	switch {
	case !to.After(from):
		return nil, invalid("to", "must be after from")
	case to.Sub(from) > MaxTimelineRange:
		return nil, invalid("to", "the period may be at most a year")
	}
	var out []TimelineShift
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		id, _, err := s.find(ctx, tx, name)
		if err != nil {
			return err
		}
		out, err = s.timeline(ctx, tx, id, from, to)
		return err
	})
	return out, err
}

func (s *Store) timeline(ctx context.Context, tx pgx.Tx, scheduleID string, from, to time.Time) ([]TimelineShift, error) {
	now := s.now()
	// The rotation on record: shifts that have ended.
	rotation, err := recordedShifts(ctx, tx, scheduleID, KindRotation, from, to)
	if err != nil {
		return nil, err
	}

	// The rotation from the shift in progress on, run ahead from the
	// position. Handoffs that are due and not carried out yet are part of
	// it, with the times they will be recorded at.
	pos, err := scanPosition(tx.QueryRow(ctx, `SELECT `+positionColumns+` FROM schedule_positions p WHERE schedule_id = $1`, scheduleID))
	if err != nil {
		return nil, fmt.Errorf("read position: %w", err)
	}
	if _, err := describe(ctx, tx, &pos); err != nil {
		return nil, err
	}
	if pos.ShiftStartedAt.Before(to) {
		members := pos.Definition.Members
		if pos.Pending != nil {
			members = slices.Concat(members, pos.Pending.Members)
		}
		users, err := usersByEmail(ctx, tx, members)
		if err != nil {
			return nil, err
		}
		spans, err := pos.state().Project(to, func(email string) bool { return users[email] != "" })
		if err != nil {
			return nil, err
		}
		for _, sp := range spans {
			if sp.Member != "" && sp.End.After(from) && sp.Start.Before(to) {
				rotation = append(rotation, TimelineShift{User: sp.Member, StartsAt: sp.Start, EndsAt: sp.End, Kind: KindRotation, Planned: sp.End.After(now)})
			}
		}
	}

	// Overrides are looked for over all the time that those rotation shifts
	// span, which may reach beyond the period asked for: an override that
	// ended just before the period still covered the start of a shift that
	// reaches into it.
	lo, hi := from, to
	for _, r := range rotation {
		if r.StartsAt.Before(lo) {
			lo = r.StartsAt
		}
		if r.EndsAt.After(hi) {
			hi = r.EndsAt
		}
	}
	overrides, err := recordedShifts(ctx, tx, scheduleID, KindOverride, lo, hi)
	if err != nil {
		return nil, err
	}
	// And the overrides whose shift has not been closed: in force, to come,
	// or over without the worker having recorded it yet.
	rows, err := tx.Query(ctx, `
		SELECT u.email, o.starts_at, o.ends_at, o.id::text FROM schedule_overrides o JOIN users u ON u.id = o.user_id
		WHERE o.schedule_id = $1 AND o.cancelled_at IS NULL AND o.starts_at < $3 AND o.ends_at > $2
			AND NOT EXISTS (SELECT 1 FROM shifts f WHERE f.override_id = o.id AND f.ends_at IS NOT NULL)
		ORDER BY o.starts_at`, scheduleID, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("read overrides: %w", err)
	}
	f := TimelineShift{Kind: KindOverride}
	_, err = pgx.ForEachRow(rows, []any{&f.User, &f.StartsAt, &f.EndsAt, &f.OverrideID}, func() error {
		f.Planned = f.EndsAt.After(now)
		overrides = append(overrides, f)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read overrides: %w", err)
	}

	var out []TimelineShift
	for _, shift := range append(cover(rotation, overrides), overrides...) {
		if shift.StartsAt.Before(to) && shift.EndsAt.After(from) {
			shift.StartsAt, shift.EndsAt = shift.StartsAt.UTC(), shift.EndsAt.UTC()
			out = append(out, shift)
		}
	}
	slices.SortStableFunc(out, func(a, b TimelineShift) int { return a.StartsAt.Compare(b.StartsAt) })
	return out, nil
}

// recordedShifts returns the shifts of one kind that have ended and overlap
// a period.
func recordedShifts(ctx context.Context, tx pgx.Tx, scheduleID, kind string, from, to time.Time) ([]TimelineShift, error) {
	rows, err := tx.Query(ctx, `
		SELECT u.email, f.starts_at, f.ends_at, f.kind, COALESCE(f.override_id::text, '')
		FROM shifts f JOIN users u ON u.id = f.user_id
		WHERE f.schedule_id = $1 AND f.kind = $2 AND f.ends_at IS NOT NULL AND f.starts_at < $4 AND f.ends_at > $3
		ORDER BY f.starts_at, f.id`, scheduleID, kind, from, to)
	if err != nil {
		return nil, fmt.Errorf("read shifts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimelineShift, error) {
		var f TimelineShift
		return f, r.Scan(&f.User, &f.StartsAt, &f.EndsAt, &f.Kind, &f.OverrideID)
	})
	if err != nil {
		return nil, fmt.Errorf("read shifts: %w", err)
	}
	return out, nil
}

// cover returns the rotation shifts with the time that overrides cover cut
// out of them. A shift that an override covers in the middle becomes two.
func cover(rotation, overrides []TimelineShift) []TimelineShift {
	var out []TimelineShift
	for _, r := range rotation {
		pieces := []TimelineShift{r}
		for _, o := range overrides {
			var next []TimelineShift
			for _, p := range pieces {
				if !o.StartsAt.Before(p.EndsAt) || !o.EndsAt.After(p.StartsAt) {
					next = append(next, p)
					continue
				}
				if o.StartsAt.After(p.StartsAt) {
					before := p
					before.EndsAt = o.StartsAt
					next = append(next, before)
				}
				if o.EndsAt.Before(p.EndsAt) {
					after := p
					after.StartsAt = o.EndsAt
					next = append(next, after)
				}
			}
			pieces = next
		}
		out = append(out, pieces...)
	}
	return out
}
