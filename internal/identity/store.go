// Package identity knows which account in a chat service belongs to which
// Beacon user. Beacon needs that in both directions: to reach a person in
// the service, and to know who a person acting from the service is.
//
// A Slack account is found by the user's email address (Linker). A Discord
// account is linked by the user themselves, with a one-time code (StartLink
// and CompleteLink). docs/access.md describes both.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The chat services, as in user_identities.provider and in the actor of a
// timeline event.
const (
	ProviderSlack   = "slack"
	ProviderDiscord = "discord"
)

// How an identity came to be known.
const (
	// OriginEmail is an account found by the user's email address.
	OriginEmail = "email"
	// OriginSelf is an account that the user linked themselves.
	OriginSelf = "self"
)

// ErrTaken means that the account in the chat service is linked to another
// user already.
var ErrTaken = errors.New("the account is linked to another user")

// ErrBadCode means that a link code does not exist, was used, has expired or
// was issued for another integration. Which of them is not said.
var ErrBadCode = errors.New("the code is not valid; ask Beacon for a new one")

// linkCodeLifetime is how long a person has to use a link code.
const linkCodeLifetime = 10 * time.Minute

// Querier is what a pool and a transaction have in common.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store keeps identities in Postgres.
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

// User is a Beacon user, as far as this package needs one.
type User struct {
	ID    string
	Email string
	// Name is what to call the person: their display name, or their email
	// address when they have none.
	Name string
}

// Identity is one account of a user in a chat service.
type Identity struct {
	// Integration is the name of the integration through which the account
	// is known.
	Integration string    `json:"integration"`
	Provider    string    `json:"provider"`
	ExternalID  string    `json:"external_id"`
	Origin      string    `json:"origin"`
	LinkedAt    time.Time `json:"linked_at"`
}

// Link records that a user has an account with an integration. The account
// a user had there before is replaced. It returns ErrTaken when the account
// is another user's: an account is never moved from one person to another
// without that person unlinking it.
func (s *Store) Link(ctx context.Context, q Querier, userID, integrationID, provider, externalID, origin string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO user_identities (user_id, integration_id, provider, external_id, origin, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, integration_id) DO UPDATE SET external_id = EXCLUDED.external_id, origin = EXCLUDED.origin,
			provider = EXCLUDED.provider, created_at = EXCLUDED.created_at
		WHERE user_identities.external_id <> EXCLUDED.external_id`,
		userID, integrationID, provider, externalID, origin, s.now())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "user_identities_account_key" {
		return ErrTaken
	}
	if err != nil {
		return fmt.Errorf("link identity: %w", err)
	}
	_, err = q.Exec(ctx, `DELETE FROM identity_lookups WHERE user_id = $1 AND integration_id = $2`, userID, integrationID)
	if err != nil {
		return fmt.Errorf("link identity: %w", err)
	}
	return nil
}

// Unlink removes the account a user has with an integration, and reports
// whether there was one.
func (s *Store) Unlink(ctx context.Context, userID, integrationID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM user_identities WHERE user_id = $1 AND integration_id = $2`, userID, integrationID)
	if err != nil {
		return false, fmt.Errorf("unlink identity: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UserFor returns the user to whom an account of a chat service belongs.
// found is false when the account is linked to nobody, or to a user who is
// disabled.
func (s *Store) UserFor(ctx context.Context, q Querier, integrationID, externalID string) (u User, found bool, err error) {
	err = q.QueryRow(ctx, `
		SELECT u.id::text, u.email, COALESCE(NULLIF(u.display_name, ''), u.email)
		FROM user_identities i JOIN users u ON u.id = i.user_id
		WHERE i.integration_id = $1 AND i.external_id = $2 AND u.disabled_at IS NULL`, integrationID, externalID).Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("read identity: %w", err)
	}
	return u, true, nil
}

// Address returns the account a user has with an integration: where that
// integration reaches them directly. found is false when they have none.
func (s *Store) Address(ctx context.Context, q Querier, integrationID, userID string) (externalID string, found bool, err error) {
	err = q.QueryRow(ctx, `SELECT external_id FROM user_identities WHERE integration_id = $1 AND user_id = $2`, integrationID, userID).Scan(&externalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read identity: %w", err)
	}
	return externalID, true, nil
}

// Identities lists the accounts of a user, by the name of the integration.
func (s *Store) Identities(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := s.db.Query(ctx, `
		SELECT n.name, i.provider, i.external_id, i.origin, i.created_at
		FROM user_identities i JOIN integrations n ON n.id = i.integration_id
		WHERE i.user_id = $1 AND n.removed_at IS NULL ORDER BY n.name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Identity, error) {
		var i Identity
		err := r.Scan(&i.Integration, &i.Provider, &i.ExternalID, &i.Origin, &i.LinkedAt)
		i.LinkedAt = i.LinkedAt.UTC()
		return i, err
	})
}

// hashCode returns the form in which a link code is stored. A code is
// random and short-lived, so one round of SHA-256 is right for it.
func hashCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeCode(code)))
	return hex.EncodeToString(sum[:])
}

// normalizeCode makes a code as a person types it comparable: without
// spaces or hyphens, in capitals.
func normalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(code))
}

// StartLink begins linking an account by the user themselves. It returns a
// code that the user gives to Beacon from inside the chat service, which
// proves that the account there is theirs. The code works once, within ten
// minutes, and only for this integration; a new code replaces the user's
// earlier ones for it.
func (s *Store) StartLink(ctx context.Context, userID, integrationID string) (code string, expires time.Time, err error) {
	// 80 random bits, as 16 characters that are easy to type.
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate link code: %w", err)
	}
	code = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	now := s.now()
	expires = now.Add(linkCodeLifetime)
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM identity_link_codes WHERE (user_id = $1 AND integration_id = $2) OR expires_at <= $3`, userID, integrationID, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO identity_link_codes (code_hash, user_id, integration_id, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
			hashCode(code), userID, integrationID, now, expires)
		return err
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("start link: %w", err)
	}
	return code, expires.UTC(), nil
}

// CompleteLink finishes a link that StartLink began: the account that
// presents the code, through the integration the code was issued for,
// becomes the account of the user who asked for the code. The caller must
// have verified that the request really comes from the chat service, and
// from that account. The code is used up whether or not the link succeeds.
//
// It returns ErrBadCode for a code that cannot be used, and ErrTaken when
// the account is linked to another user.
func (s *Store) CompleteLink(ctx context.Context, integrationID, provider, externalID, code string) (User, error) {
	var u User
	var taken bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM identity_link_codes WHERE code_hash = $1 AND integration_id = $2 AND expires_at > $3 RETURNING user_id::text`,
			hashCode(code), integrationID, s.now()).Scan(&u.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadCode
		}
		if err != nil {
			return err
		}
		// A failed link must not undo the use of the code, so the link is
		// tried in a savepoint.
		err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			return s.Link(ctx, sp, u.ID, integrationID, provider, externalID, OriginSelf)
		})
		if errors.Is(err, ErrTaken) {
			taken = true
			return nil
		}
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT email, COALESCE(NULLIF(display_name, ''), email) FROM users WHERE id = $1`, u.ID).Scan(&u.Email, &u.Name)
	})
	switch {
	case taken:
		return User{}, ErrTaken
	case errors.Is(err, ErrBadCode):
		return User{}, ErrBadCode
	case err != nil:
		return User{}, fmt.Errorf("complete link: %w", err)
	}
	return u, nil
}
