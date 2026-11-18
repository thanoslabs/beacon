package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/config"
)

// ErrNotFound is returned when no schedule in the configuration has the
// given identifier.
var ErrNotFound = errors.New("schedule not found")

// The kinds of shift.
const (
	KindRotation = "rotation"
	KindOverride = "override"
)

// Store keeps the position of every schedule and the shifts that have
// happened in Postgres. It takes the time from its clock, never from
// Postgres, so tests can move time.
type Store struct {
	db  *pgxpool.Pool
	now func() time.Time
}

// NewStore returns a store that uses db.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db, now: time.Now}
}

// SetClock replaces the clock, so tests can move time.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Provisioned is a schedule of the configuration and the identifier of its
// row.
type Provisioned struct {
	ID       string
	Schedule config.Schedule
}

// Skipped is an email address in a rotation that belongs to no user. Such a
// member is passed over until the person has signed in.
type Skipped struct {
	Schedule string
	Email    string
}

// Applied is the outcome of applying the schedules of a configuration.
type Applied struct {
	// Started names the schedules whose rotation began.
	Started []string
	// Ended names the schedules that left the configuration.
	Ended []string
	// Skipped lists the rotation members that no user has the address of.
	Skipped []Skipped
}

// Position is where a schedule is in its rotation.
type Position struct {
	ScheduleID string
	// UserID and Email are the person on call under the rotation, ignoring
	// overrides. Both are empty when no member of the rotation is a user.
	UserID         string
	Email          string
	ShiftStartedAt time.Time
	NextHandoffAt  time.Time
	// Definition is the rotation in force.
	Definition Definition
}

func (p Position) state() State {
	return State{Member: p.Email, ShiftStart: p.ShiftStartedAt, NextHandoff: p.NextHandoffAt, Definition: p.Definition}
}

// Shift is one recorded shift.
type Shift struct {
	ID         string
	ScheduleID string
	UserID     string
	Email      string
	StartsAt   time.Time
	// EndsAt is nil while the shift is in progress.
	EndsAt *time.Time
	Kind   string
}

// Apply makes the positions follow the schedules of a configuration, which
// must be valid. A schedule that has no position starts its rotation now. A
// schedule that left the configuration ends: its shift in progress is closed
// and its position dropped, so that it starts afresh if it ever returns. The
// shifts it recorded stay.
//
// It is safe to repeat and safe from several replicas at once.
func (s *Store) Apply(ctx context.Context, schedules []Provisioned) (Applied, error) {
	var out Applied
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		out = Applied{}
		now := s.now()
		rows, err := tx.Query(ctx, `
			SELECT p.schedule_id::text, s.name FROM schedule_positions p JOIN schedules s ON s.id = p.schedule_id
			WHERE s.removed_at IS NOT NULL ORDER BY s.name FOR UPDATE OF p`)
		if err != nil {
			return fmt.Errorf("read the positions of removed schedules: %w", err)
		}
		type removed struct{ id, name string }
		gone, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (removed, error) {
			var g removed
			return g, r.Scan(&g.id, &g.name)
		})
		if err != nil {
			return fmt.Errorf("read the positions of removed schedules: %w", err)
		}
		for _, g := range gone {
			if _, err := tx.Exec(ctx, `UPDATE shifts SET ends_at = GREATEST($2, starts_at) WHERE schedule_id = $1 AND ends_at IS NULL`, g.id, now); err != nil {
				return fmt.Errorf("close the shift of schedule %q: %w", g.name, err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM schedule_positions WHERE schedule_id = $1`, g.id); err != nil {
				return fmt.Errorf("drop the position of schedule %q: %w", g.name, err)
			}
			out.Ended = append(out.Ended, g.name)
		}

		for _, p := range schedules {
			def := DefinitionOf(p.Schedule)
			users, err := usersByEmail(ctx, tx, def.Members)
			if err != nil {
				return err
			}
			for _, m := range def.Members {
				if _, ok := users[m]; !ok {
					out.Skipped = append(out.Skipped, Skipped{Schedule: p.Schedule.Name, Email: m})
				}
			}
			started, err := s.start(ctx, tx, p.ID, def, users, now)
			if err != nil {
				return fmt.Errorf("schedule %q: %w", p.Schedule.Name, err)
			}
			if started {
				out.Started = append(out.Started, p.Schedule.Name)
			}
		}
		return nil
	})
	if err != nil {
		return Applied{}, fmt.Errorf("apply schedules: %w", err)
	}
	return out, nil
}

// start gives a schedule its position if it has none, and records the first
// shift.
func (s *Store) start(ctx context.Context, tx pgx.Tx, scheduleID string, def Definition, users map[string]string, now time.Time) (bool, error) {
	state, err := Start(def, now, func(email string) bool { return users[email] != "" })
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO schedule_positions (schedule_id, user_id, shift_started_at, next_handoff_at, definition, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $3, $3) ON CONFLICT (schedule_id) DO NOTHING`,
		scheduleID, nullable(users[state.Member]), state.ShiftStart, state.NextHandoff, def)
	if err != nil {
		return false, fmt.Errorf("create position: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, openShift(ctx, tx, scheduleID, users[state.Member], state.ShiftStart)
}

// openShift records that a rotation shift began. With no user it records
// nothing: nobody is on call.
func openShift(ctx context.Context, tx pgx.Tx, scheduleID, userID string, at time.Time) error {
	if userID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO shifts (schedule_id, user_id, starts_at, kind) VALUES ($1, $2, $3, $4)`,
		scheduleID, userID, at, KindRotation)
	if err != nil {
		return fmt.Errorf("record shift: %w", err)
	}
	return nil
}

// Handoff is one change of the person on call that Advance made.
type Handoff struct {
	Schedule string
	// From and To are email addresses. Either may be empty: nobody was, or
	// nobody is, on call.
	From, To string
	At       time.Time
	// Next is when the shift that began ends.
	Next time.Time
}

// Advance brings every schedule up to date: each handoff that has fallen due
// closes the shift in progress and opens the next one, and a schedule on
// which nobody is on call gets its first member who has become a user. It
// returns what it did.
//
// A handoff happens at the moment it was due, also when it is carried out
// late: after a restart, the shifts that began while no process ran are
// recorded with the times they began at. Each one is carried out in a
// transaction of its own that holds the schedule's position, so every
// handoff happens once however many replicas call this together, and a
// process that dies leaves it due.
func (s *Store) Advance(ctx context.Context) ([]Handoff, error) {
	var done []Handoff
	for ctx.Err() == nil {
		h, found, err := s.handOver(ctx)
		if err != nil {
			return done, err
		}
		if !found {
			break
		}
		done = append(done, h)
	}
	if err := ctx.Err(); err != nil {
		return done, err
	}
	filled, err := s.fill(ctx)
	return append(done, filled...), err
}

// The position is read from its own table alone wherever it is locked. A
// locking read that waited for another transaction sees the row as that
// transaction left it, but would pair it with the rows of joined tables as
// they were before: the person on call would be the one who has just been
// relieved.
const positionColumns = `schedule_id::text, COALESCE(user_id::text, ''), shift_started_at, next_handoff_at, definition`

// live limits a read of schedule_positions p to schedules that are in the
// configuration.
const live = `NOT EXISTS (SELECT 1 FROM schedules s WHERE s.id = p.schedule_id AND s.removed_at IS NOT NULL)`

func scanPosition(row pgx.Row) (p Position, err error) {
	err = row.Scan(&p.ScheduleID, &p.UserID, &p.ShiftStartedAt, &p.NextHandoffAt, &p.Definition)
	return p, err
}

// describe fills in what a position does not hold itself: the address of
// the person on call. It returns the name of the schedule.
func describe(ctx context.Context, q querier, p *Position) (name string, err error) {
	if err := q.QueryRow(ctx, `SELECT name FROM schedules WHERE id = $1`, p.ScheduleID).Scan(&name); err != nil {
		return "", fmt.Errorf("read schedule: %w", err)
	}
	if p.UserID != "" {
		if err := q.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, p.UserID).Scan(&p.Email); err != nil {
			return "", fmt.Errorf("read the user on call for schedule %q: %w", name, err)
		}
	}
	return name, nil
}

// handOver carries out the handoff that has been due longest, if any.
func (s *Store) handOver(ctx context.Context) (h Handoff, found bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		pos, err := scanPosition(tx.QueryRow(ctx, `SELECT `+positionColumns+` FROM schedule_positions p
			WHERE next_handoff_at <= $1 AND `+live+`
			ORDER BY next_handoff_at LIMIT 1 FOR UPDATE SKIP LOCKED`, s.now()))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read due position: %w", err)
		}
		name, err := describe(ctx, tx, &pos)
		if err != nil {
			return err
		}
		users, err := usersByEmail(ctx, tx, pos.Definition.Members)
		if err != nil {
			return err
		}
		before := pos.state()
		after, err := before.Handoff(func(email string) bool { return users[email] != "" })
		if err != nil {
			return fmt.Errorf("schedule %q: %w", name, err)
		}
		if err := s.move(ctx, tx, pos.ScheduleID, users[after.Member], after, true); err != nil {
			return fmt.Errorf("schedule %q: %w", name, err)
		}
		h, found = Handoff{Schedule: name, From: before.Member, To: after.Member, At: after.ShiftStart, Next: after.NextHandoff}, true
		return nil
	})
	return h, found, err
}

// move writes a new state of a position. When a shift ended, the one in
// progress is closed and the next is recorded, both at the moment the new
// state says the shift began.
func (s *Store) move(ctx context.Context, tx pgx.Tx, scheduleID, userID string, to State, shiftEnded bool) error {
	if shiftEnded {
		if _, err := tx.Exec(ctx, `UPDATE shifts SET ends_at = GREATEST($2, starts_at) WHERE schedule_id = $1 AND kind = $3 AND ends_at IS NULL`,
			scheduleID, to.ShiftStart, KindRotation); err != nil {
			return fmt.Errorf("close shift: %w", err)
		}
		if err := openShift(ctx, tx, scheduleID, userID, to.ShiftStart); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE schedule_positions SET user_id = $2, shift_started_at = $3, next_handoff_at = $4, definition = $5, updated_at = $6
		WHERE schedule_id = $1`, scheduleID, nullable(userID), to.ShiftStart, to.NextHandoff, to.Definition, s.now())
	if err != nil {
		return fmt.Errorf("move position: %w", err)
	}
	return nil
}

// fill puts the first member who is a user on call on every schedule that
// has nobody, which is how a rotation whose members had not signed in yet
// gets going. The shift begins now and ends at the handoff that was planned.
func (s *Store) fill(ctx context.Context) ([]Handoff, error) {
	var done []Handoff
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		done = nil
		now := s.now()
		rows, err := tx.Query(ctx, `SELECT `+positionColumns+` FROM schedule_positions p
			WHERE user_id IS NULL AND `+live+` ORDER BY schedule_id FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return fmt.Errorf("read empty positions: %w", err)
		}
		empties, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Position, error) { return scanPosition(r) })
		if err != nil {
			return fmt.Errorf("read empty positions: %w", err)
		}
		for _, e := range empties {
			name, err := describe(ctx, tx, &e)
			if err != nil {
				return err
			}
			users, err := usersByEmail(ctx, tx, e.Definition.Members)
			if err != nil {
				return err
			}
			to := e.state()
			to.Member = successor(e.Definition.Members, e.Definition.Members, "", func(email string) bool { return users[email] != "" })
			if to.Member == "" {
				continue
			}
			to.ShiftStart = now
			if err := s.move(ctx, tx, e.ScheduleID, users[to.Member], to, true); err != nil {
				return fmt.Errorf("schedule %q: %w", name, err)
			}
			done = append(done, Handoff{Schedule: name, To: to.Member, At: now, Next: to.NextHandoff})
		}
		return nil
	})
	return done, err
}

// Run calls Advance every interval until ctx is cancelled, and logs each
// handoff.
func (s *Store) Run(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		done, err := s.Advance(ctx)
		for _, h := range done {
			logger.InfoContext(ctx, "on-call shift began", slog.String("schedule", h.Schedule),
				slog.String("from", h.From), slog.String("to", h.To), slog.Time("at", h.At), slog.Time("next_handoff", h.Next))
			if h.To == "" {
				logger.ErrorContext(ctx, "nobody is on call: no member of the rotation is a user; members take their place once they have signed in",
					slog.String("schedule", h.Schedule))
			}
		}
		if err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "advance schedules", slog.Any("error", err))
		}
	}
}

// Position returns where a schedule is in its rotation.
func (s *Store) Position(ctx context.Context, scheduleID string) (Position, error) {
	p, err := scanPosition(s.db.QueryRow(ctx, `SELECT `+positionColumns+` FROM schedule_positions p
		WHERE schedule_id = $1 AND `+live, scheduleID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Position{}, ErrNotFound
	}
	if err != nil {
		return Position{}, err
	}
	_, err = describe(ctx, s.db, &p)
	return p, err
}

// Recorded returns the shifts of a schedule that overlap the time from from
// to to, oldest first. A shift in progress has no end and overlaps every
// time after its start.
func (s *Store) Recorded(ctx context.Context, scheduleID string, from, to time.Time) ([]Shift, error) {
	rows, err := s.db.Query(ctx, `
		SELECT f.id::text, f.schedule_id::text, f.user_id::text, u.email, f.starts_at, f.ends_at, f.kind
		FROM shifts f JOIN users u ON u.id = f.user_id
		WHERE f.schedule_id = $1 AND f.starts_at < $3 AND (f.ends_at IS NULL OR f.ends_at > $2)
		ORDER BY f.starts_at, f.id`, scheduleID, from, to)
	if err != nil {
		return nil, fmt.Errorf("read shifts: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Shift, error) {
		var f Shift
		return f, r.Scan(&f.ID, &f.ScheduleID, &f.UserID, &f.Email, &f.StartsAt, &f.EndsAt, &f.Kind)
	})
}

// querier is what a pool and a transaction have in common.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// usersByEmail maps each of the addresses that an enabled user has to the
// identifier of that user.
func usersByEmail(ctx context.Context, q querier, emails []string) (map[string]string, error) {
	// Never nil: as a query argument nil would be NULL, and a comparison
	// with NULL matches nothing.
	list := append([]string{}, emails...)
	rows, err := q.Query(ctx, `SELECT email, id::text FROM users WHERE email = ANY($1) AND disabled_at IS NULL`, list)
	if err != nil {
		return nil, fmt.Errorf("read users: %w", err)
	}
	out := map[string]string{}
	var email, id string
	_, err = pgx.ForEachRow(rows, []any{&email, &id}, func() error {
		out[email] = id
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read users: %w", err)
	}
	return out, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
