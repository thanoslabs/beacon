package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/config"
)

// The limits of an override. It is created through the API, so everything in
// it is bounded.
const (
	// MaxOverrideLength bounds how long an override may last, and how far
	// ahead it may begin.
	MaxOverrideLength = 366 * config.Day
	// MaxReasonLength bounds the reason, in characters.
	MaxReasonLength = 500
)

// maxListedOverrides bounds how many overrides a list returns.
const maxListedOverrides = 1000

// ErrOverrideNotFound means that the schedule has no override with the
// identifier.
var ErrOverrideNotFound = errors.New("no such override")

// OverrideState says where an override is in its life.
type OverrideState string

// The states. They follow from the period and the time; nothing stores them.
const (
	OverridePending   OverrideState = "pending"
	OverrideActive    OverrideState = "active"
	OverrideEnded     OverrideState = "ended"
	OverrideCancelled OverrideState = "cancelled"
)

// Override puts one person on call for a schedule for a period, in place of
// whoever the rotation has. It is runtime state: people create and remove
// overrides through the API, and the files know nothing of them.
type Override struct {
	ID       string `json:"id"`
	Schedule string `json:"schedule"`
	// User is the email address of the person who covers.
	User     string    `json:"user"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Reason   string    `json:"reason"`
	// CreatedBy is the email address of the user who created it, and
	// CreatedByToken the name of the API token it was created with. One of
	// them is set.
	CreatedBy      string    `json:"created_by,omitempty"`
	CreatedByToken string    `json:"created_by_token,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	// CancelledAt is when it was removed, if it was.
	CancelledAt *time.Time    `json:"cancelled_at,omitempty"`
	State       OverrideState `json:"state"`

	userID string
}

// stateAt returns the state of the override at a time.
func (o Override) stateAt(now time.Time) OverrideState {
	switch {
	case o.CancelledAt != nil:
		return OverrideCancelled
	case now.Before(o.StartsAt):
		return OverridePending
	case now.Before(o.EndsAt):
		return OverrideActive
	}
	return OverrideEnded
}

// Error is something wrong with an override that someone asked for. Field
// names the part at fault.
type Error struct {
	Field string
	Msg   string
}

func (e *Error) Error() string { return e.Field + ": " + e.Msg }

func invalid(field, format string, args ...any) *Error {
	return &Error{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// OverlapError is returned for an override whose period overlaps one that
// the schedule already has.
type OverlapError struct {
	// Existing is the override in the way.
	Existing Override
}

func (e *OverlapError) Error() string {
	return fmt.Sprintf("the period overlaps the override of %s from %s to %s; remove that one first, or choose a period beside it",
		e.Existing.User, e.Existing.StartsAt.UTC().Format(time.RFC3339), e.Existing.EndsAt.UTC().Format(time.RFC3339))
}

// NewOverride is an override that someone asks for.
type NewOverride struct {
	// Schedule is the name of the schedule.
	Schedule string
	// User is the email address of the person who covers.
	User string
	// StartsAt is when the override begins. Zero means now, and so does a
	// time in the past.
	StartsAt time.Time
	EndsAt   time.Time
	Reason   string
}

const overrideColumns = `o.id::text, s.name, o.user_id::text, u.email, o.starts_at, o.ends_at, o.reason,
	COALESCE(a.email, ''), COALESCE(t.name, ''), o.created_at, o.cancelled_at`

const overrideTables = `schedule_overrides o JOIN schedules s ON s.id = o.schedule_id JOIN users u ON u.id = o.user_id
	LEFT JOIN users a ON a.id = o.created_by LEFT JOIN api_tokens t ON t.id = o.created_by_token`

func scanOverride(row pgx.Row, now time.Time) (Override, error) {
	var o Override
	err := row.Scan(&o.ID, &o.Schedule, &o.userID, &o.User, &o.StartsAt, &o.EndsAt, &o.Reason, &o.CreatedBy, &o.CreatedByToken, &o.CreatedAt, &o.CancelledAt)
	// Times are answered in UTC, whatever zone a request used.
	o.StartsAt, o.EndsAt, o.CreatedAt = o.StartsAt.UTC(), o.EndsAt.UTC(), o.CreatedAt.UTC()
	if o.CancelledAt != nil {
		at := o.CancelledAt.UTC()
		o.CancelledAt = &at
	}
	o.State = o.stateAt(now)
	return o, err
}

// lockSchedule finds a schedule of the configuration by name and holds its
// position until the transaction ends, which makes everything that changes
// the overrides of one schedule take turns.
func lockSchedule(ctx context.Context, tx pgx.Tx, name string) (id string, err error) {
	err = tx.QueryRow(ctx, `SELECT schedule_id::text FROM schedule_positions p
		WHERE schedule_id = (SELECT id FROM schedules WHERE name = $1 AND removed_at IS NULL) FOR UPDATE`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// CreateOverride stores an override that by asks for and returns it. An
// override that begins now is recorded as a shift at once.
//
// It returns ErrNotFound when no schedule has the name, an *Error when
// something is wrong with the request, and an *OverlapError when the period
// overlaps another override of the schedule: one person covers at a time,
// and Beacon does not guess which of two overrides was meant to win.
func (s *Store) CreateOverride(ctx context.Context, n NewOverride, by access.Principal) (Override, error) {
	now := s.now()
	reason := strings.TrimSpace(n.Reason)
	if utf8.RuneCountInString(reason) > MaxReasonLength || !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) {
		return Override{}, invalid("reason", "must be text of at most %d characters", MaxReasonLength)
	}
	starts := n.StartsAt
	if starts.IsZero() || starts.Before(now) {
		// Nobody can have been on call in the past by saying so now.
		starts = now
	}
	switch {
	case n.User == "":
		return Override{}, invalid("user", "is required: the email address of the person who covers")
	case n.EndsAt.IsZero():
		return Override{}, invalid("ends_at", "is required: when the override ends, or give a duration")
	case !n.EndsAt.After(now):
		return Override{}, invalid("ends_at", "must be in the future")
	case !n.EndsAt.After(starts):
		return Override{}, invalid("ends_at", "must be after starts_at")
	case starts.Sub(now) > MaxOverrideLength:
		return Override{}, invalid("starts_at", "must be within a year from now")
	case n.EndsAt.Sub(starts) > MaxOverrideLength:
		return Override{}, invalid("ends_at", "an override may last at most a year")
	}
	if by.UserID == "" && by.TokenID == "" {
		return Override{}, errors.New("an override needs an author")
	}

	var out Override
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		scheduleID, err := lockSchedule(ctx, tx, n.Schedule)
		if err != nil {
			return err
		}
		email := config.NormalizeEmail(n.User)
		users, err := usersByEmail(ctx, tx, []string{email})
		if err != nil {
			return err
		}
		userID := users[email]
		if userID == "" {
			return invalid("user", "no user has this email address; a person becomes a user when they first sign in")
		}
		existing, err := scanOverride(tx.QueryRow(ctx, `SELECT `+overrideColumns+` FROM `+overrideTables+`
			WHERE o.schedule_id = $1 AND o.cancelled_at IS NULL AND o.starts_at < $3 AND o.ends_at > $2
			ORDER BY o.starts_at LIMIT 1`, scheduleID, starts, n.EndsAt), now)
		if err == nil {
			return &OverlapError{Existing: existing}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look for overlapping overrides: %w", err)
		}
		var id string
		err = tx.QueryRow(ctx, `
			INSERT INTO schedule_overrides (schedule_id, user_id, starts_at, ends_at, created_by, created_by_token, reason, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
			scheduleID, userID, starts, n.EndsAt, nullable(by.UserID), nullableIf(by.UserID == "", by.TokenID), reason, now).Scan(&id)
		if err != nil {
			return fmt.Errorf("create override: %w", err)
		}
		if !starts.After(now) {
			if err := recordOverride(ctx, tx, id, scheduleID, userID, starts, nil); err != nil {
				return err
			}
		}
		out, err = scanOverride(tx.QueryRow(ctx, `SELECT `+overrideColumns+` FROM `+overrideTables+` WHERE o.id = $1`, id), now)
		return err
	})
	return out, err
}

// recordOverride writes the shift of an override that has begun.
func recordOverride(ctx context.Context, tx pgx.Tx, id, scheduleID, userID string, starts time.Time, ends *time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO shifts (schedule_id, user_id, starts_at, ends_at, kind, override_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		scheduleID, userID, starts, ends, KindOverride, id)
	if err != nil {
		return fmt.Errorf("record the shift of override %s: %w", id, err)
	}
	return nil
}

// CancelOverride removes an override of a schedule and returns it as it is
// afterwards. One that has not begun never will; one that is in force ends
// now, and its shift is closed. Removing an override that is over or was
// removed before changes nothing and is not an error. It returns ErrNotFound
// or ErrOverrideNotFound when there is no such schedule or override.
func (s *Store) CancelOverride(ctx context.Context, scheduleName, id string) (Override, error) {
	now := s.now()
	var out Override
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		scheduleID, err := lockSchedule(ctx, tx, scheduleName)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE schedule_overrides SET cancelled_at = $3
			WHERE id = $1 AND schedule_id = $2 AND cancelled_at IS NULL AND ends_at > $3`, id, scheduleID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `UPDATE shifts SET ends_at = GREATEST($2, starts_at) WHERE override_id = $1 AND ends_at IS NULL`, id, now); err != nil {
				return fmt.Errorf("close the shift of the override: %w", err)
			}
		}
		out, err = scanOverride(tx.QueryRow(ctx, `SELECT `+overrideColumns+` FROM `+overrideTables+`
			WHERE o.id = $1 AND o.schedule_id = $2`, id, scheduleID), now)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return Override{}, ErrOverrideNotFound
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Override{}, fmt.Errorf("remove override: %w", err)
	}
	return out, err
}

// Overrides lists the overrides of a schedule, by start. Without past, only
// the ones in force and the ones still to come are listed.
func (s *Store) Overrides(ctx context.Context, scheduleName string, past bool) ([]Override, error) {
	now := s.now()
	var scheduleID string
	err := s.db.QueryRow(ctx, `SELECT s.id::text FROM schedules s JOIN schedule_positions p ON p.schedule_id = s.id
		WHERE s.name = $1 AND s.removed_at IS NULL`, scheduleName).Scan(&scheduleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read schedule: %w", err)
	}
	rows, err := s.db.Query(ctx, `SELECT `+overrideColumns+` FROM `+overrideTables+`
		WHERE o.schedule_id = $1 AND ($2 OR (o.cancelled_at IS NULL AND o.ends_at > $3))
		ORDER BY o.starts_at, o.id LIMIT $4`, scheduleID, past, now, maxListedOverrides)
	if err != nil {
		return nil, fmt.Errorf("list overrides: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Override, error) { return scanOverride(r, now) })
}

// activeOverride returns the override of a schedule that is in force at a
// time, if there is one. Overrides of one schedule never overlap.
func activeOverride(ctx context.Context, q querier, scheduleID string, at time.Time) (Override, bool, error) {
	o, err := scanOverride(q.QueryRow(ctx, `SELECT `+overrideColumns+` FROM `+overrideTables+`
		WHERE o.schedule_id = $1 AND o.cancelled_at IS NULL AND o.starts_at <= $2 AND o.ends_at > $2`, scheduleID, at), at)
	if errors.Is(err, pgx.ErrNoRows) {
		return Override{}, false, nil
	}
	return o, err == nil, err
}

// recordOverrides brings the shifts of overrides up to date: an override
// whose start has come gets its shift, and a shift whose override is over is
// closed. Both are recorded with the times of the override, however late
// this runs, and each once however many replicas run it.
func (s *Store) recordOverrides(ctx context.Context) ([]Handoff, error) {
	var done []Handoff
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		done = nil
		now := s.now()
		// Closing first means that an override which began and ended while
		// nothing ran is opened below with its end already set.
		rows, err := tx.Query(ctx, `
			UPDATE shifts f SET ends_at = o.ends_at FROM schedule_overrides o
			WHERE f.override_id = o.id AND f.ends_at IS NULL AND o.ends_at <= $1
			RETURNING (SELECT name FROM schedules WHERE id = f.schedule_id), (SELECT email FROM users WHERE id = f.user_id), o.ends_at`, now)
		if err != nil {
			return fmt.Errorf("close the shifts of overrides: %w", err)
		}
		ended, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Handoff, error) {
			h := Handoff{Override: true}
			return h, r.Scan(&h.Schedule, &h.From, &h.At)
		})
		if err != nil {
			return fmt.Errorf("close the shifts of overrides: %w", err)
		}

		// The override rows are locked from their own table alone; see the
		// note at positionColumns.
		rows, err = tx.Query(ctx, `
			SELECT o.id::text, o.schedule_id::text, o.user_id::text, o.starts_at, o.ends_at FROM schedule_overrides o
			WHERE o.starts_at <= $1 AND o.cancelled_at IS NULL
				AND NOT EXISTS (SELECT 1 FROM shifts f WHERE f.override_id = o.id)
				AND NOT EXISTS (SELECT 1 FROM schedules s WHERE s.id = o.schedule_id AND s.removed_at IS NOT NULL)
			ORDER BY o.starts_at, o.id FOR UPDATE OF o SKIP LOCKED`, now)
		if err != nil {
			return fmt.Errorf("read the overrides that began: %w", err)
		}
		due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (begun, error) {
			var b begun
			return b, r.Scan(&b.id, &b.scheduleID, &b.userID, &b.starts, &b.ends)
		})
		if err != nil {
			return fmt.Errorf("read the overrides that began: %w", err)
		}
		for _, b := range due {
			h, recorded, err := recordBegun(ctx, tx, b, now)
			if err != nil {
				return err
			}
			if recorded {
				done = append(done, h)
			}
		}
		done = append(done, ended...)
		return nil
	})
	return done, err
}

// begun is an override whose start has come.
type begun struct {
	id, scheduleID, userID string
	starts, ends           time.Time
}

// recordBegun writes the shift of an override whose row the transaction
// holds, unless it has one. recorded is false when it had.
//
// That question is asked here, in a statement of its own, although the read
// that found the override asked it too. A replica can take an override the
// moment another replica has committed its shift and let go of the row; the
// read that found the row began before that commit and did not see the
// shift. With the row held, a new statement does.
func recordBegun(ctx context.Context, tx pgx.Tx, b begun, now time.Time) (h Handoff, recorded bool, err error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shifts WHERE override_id = $1)`, b.id).Scan(&exists); err != nil {
		return Handoff{}, false, fmt.Errorf("look for the shift of override %s: %w", b.id, err)
	}
	if exists {
		return Handoff{}, false, nil
	}
	var ends *time.Time
	if !b.ends.After(now) {
		ends = &b.ends
	}
	if err := recordOverride(ctx, tx, b.id, b.scheduleID, b.userID, b.starts, ends); err != nil {
		return Handoff{}, false, err
	}
	h = Handoff{Override: true, At: b.starts, Next: b.ends, ShiftBegan: true}
	err = tx.QueryRow(ctx, `SELECT (SELECT name FROM schedules WHERE id = $1), (SELECT email FROM users WHERE id = $2)`, b.scheduleID, b.userID).Scan(&h.Schedule, &h.To)
	if err != nil {
		return Handoff{}, false, fmt.Errorf("describe override %s: %w", b.id, err)
	}
	return h, true, nil
}

// notUUID reports whether Postgres refused an identifier that is not a UUID.
func notUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func nullableIf(cond bool, s string) *string {
	if !cond {
		return nil
	}
	return nullable(s)
}
