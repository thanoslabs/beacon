package schedule

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/config"
)

// Move is a stretch of time in which a change puts someone else on call.
// Time that an override covers never moves: the override holds whatever the
// files say.
type Move struct {
	Start time.Time
	End   time.Time
	// Was and Becomes are email addresses; empty means nobody.
	Was     string
	Becomes string
}

// Compare returns the stretches of time between from and until in which two
// plans have different people on call, in order. Time that a plan does not
// cover has nobody on call.
func Compare(was, becomes []Span, from, until time.Time) []Move {
	cuts := []time.Time{from, until}
	for _, s := range slices.Concat(was, becomes) {
		for _, t := range []time.Time{s.Start, s.End} {
			if t.After(from) && t.Before(until) {
				cuts = append(cuts, t)
			}
		}
	}
	slices.SortFunc(cuts, time.Time.Compare)
	onCall := func(plan []Span, at time.Time) string {
		for _, s := range plan {
			if !at.Before(s.Start) && at.Before(s.End) {
				return s.Member
			}
		}
		return ""
	}
	var out []Move
	for i := 0; i+1 < len(cuts); i++ {
		start, end := cuts[i], cuts[i+1]
		if !end.After(start) {
			continue
		}
		a, b := onCall(was, start), onCall(becomes, start)
		if a == b {
			continue
		}
		if n := len(out); n > 0 && out[n-1].End.Equal(start) && out[n-1].Was == a && out[n-1].Becomes == b {
			out[n-1].End = end
			continue
		}
		out = append(out, Move{Start: start, End: end, Was: a, Becomes: b})
	}
	return out
}

// What a preview says about a schedule.
const (
	// PreviewNew is a schedule that is not running: it starts when the
	// configuration is applied.
	PreviewNew = "new"
	// PreviewUnchanged is a schedule whose definition is the rotation in
	// force.
	PreviewUnchanged = "unchanged"
	// PreviewChanged is a schedule whose definition would be stored as a
	// change that takes over later.
	PreviewChanged = "changed"
	// PreviewPending is a schedule whose definition has been applied
	// already and is waiting to take over.
	PreviewPending = "pending"
	// PreviewRemoved is a running schedule that the configuration does not
	// have: it ends when the configuration is applied.
	PreviewRemoved = "removed"
)

// Preview is what applying a configuration would do to one schedule.
type Preview struct {
	Schedule string
	// Status is one of the Preview constants.
	Status string
	// TimeZone is the zone of the definition, in which a person would read
	// the times.
	TimeZone string
	// TakesOverAt is when a changed definition would take over, for
	// PreviewChanged and PreviewPending. Stated says that the files state
	// that time; otherwise it is the next handoff.
	TakesOverAt time.Time
	Stated      bool
	// Moves lists the time in which the person on call would not be who
	// they are now planned to be.
	Moves []Move
	// Upcoming lists the shifts as they would be, from the one in progress.
	Upcoming []Span
	// Skipped lists the members whose address no user has. They are passed
	// over, in the preview as in the rotation.
	Skipped []string
}

// Preview works out what applying these schedules would do to the shifts
// between now and until, without changing anything: it reads the positions
// and the users in a read-only transaction and runs the same arithmetic that
// applying and handing over do.
func (s *Store) Preview(ctx context.Context, schedules []config.Schedule, until time.Time) ([]Preview, error) {
	var out []Preview
	err := pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		out = nil
		now := s.now()
		defined := map[string]bool{}
		for _, sched := range schedules {
			defined[sched.Name] = true
			p, err := s.preview(ctx, tx, sched, now, until)
			if err != nil {
				return fmt.Errorf("schedule %q: %w", sched.Name, err)
			}
			out = append(out, p)
		}
		rows, err := tx.Query(ctx, `SELECT s.name FROM schedules s JOIN schedule_positions p ON p.schedule_id = s.id
			WHERE s.removed_at IS NULL ORDER BY s.name`)
		if err != nil {
			return fmt.Errorf("read schedules: %w", err)
		}
		running, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read schedules: %w", err)
		}
		for _, name := range running {
			if !defined[name] {
				out = append(out, Preview{Schedule: name, Status: PreviewRemoved})
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) preview(ctx context.Context, tx pgx.Tx, sched config.Schedule, now, until time.Time) (Preview, error) {
	def := DefinitionOf(sched)
	out := Preview{Schedule: sched.Name, TimeZone: def.TimeZone}

	pos, err := scanPosition(tx.QueryRow(ctx, `SELECT `+positionColumns+` FROM schedule_positions p
		WHERE schedule_id = (SELECT id FROM schedules WHERE name = $1 AND removed_at IS NULL)`, sched.Name))
	running := err == nil
	if err != nil && err != pgx.ErrNoRows { //nolint:errorlint // QueryRow returns ErrNoRows itself
		return Preview{}, fmt.Errorf("read position: %w", err)
	}
	members := def.Members
	if running {
		members = slices.Concat(members, pos.Definition.Members)
		if pos.Pending != nil {
			members = slices.Concat(members, pos.Pending.Members)
		}
	}
	users, err := usersByEmail(ctx, tx, members)
	if err != nil {
		return Preview{}, err
	}
	known := func(email string) bool { return users[email] != "" }
	for _, m := range def.Members {
		if !known(m) {
			out.Skipped = append(out.Skipped, m)
		}
	}

	if !running {
		start, err := Start(def, now, known)
		if err != nil {
			return Preview{}, err
		}
		out.Status = PreviewNew
		out.Upcoming, err = start.Project(until, known)
		return out, err
	}

	if _, err := describe(ctx, tx, &pos); err != nil {
		return Preview{}, err
	}
	var effective *time.Time
	if sched.EffectiveFrom != nil {
		at := time.Time(*sched.EffectiveFrom)
		effective = &at
	}
	before := pos.state()
	after := before.WithDefinition(def, effective, now)
	switch {
	case after.Pending == nil && before.Pending == nil:
		out.Status = PreviewUnchanged
	case samePending(before, after, now):
		out.Status = PreviewPending
	default:
		out.Status = PreviewChanged
	}
	if after.Pending != nil {
		out.TakesOverAt, out.Stated = after.takeover(), after.PendingAt != nil
	}
	was, err := before.Project(until, known)
	if err != nil {
		return Preview{}, err
	}
	if out.Upcoming, err = after.Project(until, known); err != nil {
		return Preview{}, err
	}
	// The overrides that exist hold whatever the files say, so they are
	// laid over both plans: time that an override covers does not move.
	if was, err = withOverrides(ctx, tx, pos.ScheduleID, was, now); err != nil {
		return Preview{}, err
	}
	if out.Upcoming, err = withOverrides(ctx, tx, pos.ScheduleID, out.Upcoming, now); err != nil {
		return Preview{}, err
	}
	out.Moves = Compare(was, out.Upcoming, now, until)
	return out, nil
}

// withOverrides lays the overrides of a schedule over a plan of its
// rotation, and returns who is on call with them taken into account.
func withOverrides(ctx context.Context, q querier, scheduleID string, plan []Span, now time.Time) ([]Span, error) {
	if len(plan) == 0 {
		return plan, nil
	}
	rotation := make([]TimelineShift, 0, len(plan))
	for _, sp := range plan {
		rotation = append(rotation, TimelineShift{User: sp.Member, StartsAt: sp.Start, EndsAt: sp.End, Kind: KindRotation})
	}
	layers, err := overrideLayers(ctx, q, scheduleID, plan[0].Start, plan[len(plan)-1].End, now)
	if err != nil {
		return nil, err
	}
	if len(layers) == 0 {
		return plan, nil
	}
	all := append(cover(rotation, layers), layers...)
	slices.SortStableFunc(all, func(a, b TimelineShift) int { return a.StartsAt.Compare(b.StartsAt) })
	out := make([]Span, 0, len(all))
	for _, shift := range all {
		out = append(out, Span{Member: shift.User, Start: shift.StartsAt, End: shift.EndsAt, Override: shift.Kind == KindOverride})
	}
	return out, nil
}
