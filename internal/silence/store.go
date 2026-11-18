package silence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/access"
)

// ErrNotFound means that no silence has the identifier.
var ErrNotFound = errors.New("no such silence")

// maxListed bounds how many silences List returns.
const maxListed = 1000

// Store keeps silences in Postgres.
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

// New is a silence that someone asks for.
type New struct {
	Matchers []Matcher
	// StartsAt is when the silence begins. Zero means now.
	StartsAt time.Time
	EndsAt   time.Time
	Comment  string
}

// Create stores a silence that by asks for and returns it. An *Error says
// what is wrong with the request.
func (s *Store) Create(ctx context.Context, n New, by access.Principal) (Silence, error) {
	now := s.now()
	matchers, err := compile(n.Matchers)
	if err != nil {
		return Silence{}, err
	}
	comment := strings.TrimSpace(n.Comment)
	switch {
	case comment == "":
		return Silence{}, invalid("comment", "is required: say why the alerts are silenced")
	case utf8.RuneCountInString(comment) > MaxCommentLength || !utf8.ValidString(comment) || strings.ContainsRune(comment, 0):
		return Silence{}, invalid("comment", "must be text of at most %d characters", MaxCommentLength)
	}
	starts := n.StartsAt
	if starts.IsZero() || starts.Before(now) {
		// A silence cannot be made for the past.
		starts = now
	}
	switch {
	case n.EndsAt.IsZero():
		return Silence{}, invalid("ends_at", "is required: when the silence ends, or give a duration")
	case !n.EndsAt.After(now):
		return Silence{}, invalid("ends_at", "must be in the future")
	case !n.EndsAt.After(starts):
		return Silence{}, invalid("ends_at", "must be after starts_at")
	case starts.Sub(now) > MaxDuration:
		return Silence{}, invalid("starts_at", "must be within a year from now")
	case n.EndsAt.Sub(starts) > MaxDuration:
		return Silence{}, invalid("ends_at", "a silence may last at most a year")
	}
	if by.UserID == "" && by.TokenID == "" {
		return Silence{}, errors.New("a silence needs an author")
	}

	stored, err := json.Marshal(matchers)
	if err != nil {
		return Silence{}, fmt.Errorf("encode matchers: %w", err)
	}
	// Times are given and answered in UTC, whatever zone a request used.
	out := Silence{Matchers: matchers, StartsAt: starts.UTC(), EndsAt: n.EndsAt.UTC(), Comment: comment, CreatedAt: now.UTC()}
	if by.UserID != "" {
		out.CreatedBy = by.Email
	} else {
		out.CreatedByToken = by.Name
	}
	err = s.db.QueryRow(ctx, `
		INSERT INTO silences (matchers, starts_at, ends_at, created_by, created_by_token, comment, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
		stored, starts, n.EndsAt, nullable(by.UserID), nullableIf(by.UserID == "", by.TokenID), comment, now).Scan(&out.ID)
	if err != nil {
		return Silence{}, fmt.Errorf("create silence: %w", err)
	}
	out.State = out.StateAt(now)
	return out, nil
}

const columns = `s.id::text, s.matchers, s.starts_at, s.ends_at, COALESCE(u.email, ''), COALESCE(t.name, ''), s.comment, s.created_at`

// authors joins the author of a silence: the user or the token.
const authors = ` LEFT JOIN users u ON u.id = s.created_by LEFT JOIN api_tokens t ON t.id = s.created_by_token `

const from = ` FROM silences s` + authors

func (s *Store) scan(row pgx.Row, now time.Time) (Silence, error) {
	var (
		out Silence
		raw []byte
	)
	if err := row.Scan(&out.ID, &raw, &out.StartsAt, &out.EndsAt, &out.CreatedBy, &out.CreatedByToken, &out.Comment, &out.CreatedAt); err != nil {
		return Silence{}, err
	}
	matchers, err := decodeMatchers(raw)
	if err != nil {
		return Silence{}, fmt.Errorf("silence %s: %w", out.ID, err)
	}
	out.StartsAt, out.EndsAt, out.CreatedAt = out.StartsAt.UTC(), out.EndsAt.UTC(), out.CreatedAt.UTC()
	out.Matchers, out.State = matchers, out.StateAt(now)
	return out, nil
}

// Get returns one silence, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (Silence, error) {
	out, err := s.scan(s.db.QueryRow(ctx, `SELECT `+columns+from+`WHERE s.id = $1`, id), s.now())
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return Silence{}, ErrNotFound
	}
	if err != nil {
		return Silence{}, fmt.Errorf("read silence: %w", err)
	}
	return out, nil
}

// List returns the silences that are active or pending, the one that ends
// first at the top. With expired, it returns the expired ones as well, after
// the others and the most recently ended first. At most 1000 are returned.
func (s *Store) List(ctx context.Context, expired bool) ([]Silence, error) {
	now := s.now()
	rows, err := s.db.Query(ctx, `SELECT `+columns+from+`
		WHERE $2 OR s.ends_at > $1
		ORDER BY (s.ends_at > $1) DESC, CASE WHEN s.ends_at > $1 THEN s.ends_at END, s.ends_at DESC, s.id
		LIMIT $3`, now, expired, maxListed)
	if err != nil {
		return nil, fmt.Errorf("list silences: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Silence, error) { return s.scan(row, now) })
	if err != nil {
		return nil, fmt.Errorf("list silences: %w", err)
	}
	return out, nil
}

// Active returns the silences in force at a time, read through q, which may
// be a transaction.
func (s *Store) Active(ctx context.Context, q Querier, at time.Time) ([]Silence, error) {
	rows, err := q.Query(ctx, `SELECT `+columns+from+`WHERE s.starts_at <= $1 AND s.ends_at > $1 ORDER BY s.ends_at, s.id`, at)
	if err != nil {
		return nil, fmt.Errorf("read active silences: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Silence, error) { return s.scan(row, at) })
	if err != nil {
		return nil, fmt.Errorf("read active silences: %w", err)
	}
	return out, nil
}

// Querier is what a pool and a transaction have in common.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Expire ends a silence now and returns it as it is afterwards. A silence
// that has not begun never will. Expiring a silence that is over already
// changes nothing and is not an error. It returns ErrNotFound when there is
// no silence with the identifier.
func (s *Store) Expire(ctx context.Context, id string) (Silence, error) {
	now := s.now()
	out, err := s.scan(s.db.QueryRow(ctx, `
		WITH s AS (
			UPDATE silences SET ends_at = LEAST(ends_at, $2), starts_at = LEAST(starts_at, $2)
			WHERE id = $1 RETURNING *)
		SELECT `+columns+` FROM s`+authors, id, now), now)
	if errors.Is(err, pgx.ErrNoRows) || notUUID(err) {
		return Silence{}, ErrNotFound
	}
	if err != nil {
		return Silence{}, fmt.Errorf("expire silence: %w", err)
	}
	return out, nil
}

// notUUID reports whether Postgres refused an identifier that is not a UUID.
func notUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func nullable(s string) *string { return nullableIf(true, s) }

func nullableIf(cond bool, s string) *string {
	if !cond || s == "" {
		return nil
	}
	return &s
}
