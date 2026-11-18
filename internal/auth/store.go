// Package auth signs people in through an OpenID Connect identity provider
// and keeps their sessions. docs/access.md describes it from the outside.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/access"
)

// User is a row of the users table.
type User struct {
	ID          string
	Email       string
	DisplayName string
	Role        access.Role
}

// Identity is what the identity provider says about the person who signed
// in, after the ID token has been verified.
type Identity struct {
	Issuer  string
	Subject string
	// Email is lowercase.
	Email string
	Name  string
}

// Errors that sign-in reports to the person.
var (
	// ErrEmailTaken means another user already has the email address.
	ErrEmailTaken = errors.New("another account already uses this email address")
	// ErrUserDisabled means the user exists and has been disabled.
	ErrUserDisabled = errors.New("the account is disabled")
	// ErrNoSession means the session does not exist or has expired.
	ErrNoSession = fmt.Errorf("no session: %w", access.ErrUnauthenticated)
	// ErrNoFlow means the sign-in was not started here, was already
	// completed, or took too long.
	ErrNoFlow = errors.New("the sign-in has expired or was already completed")
)

// flowLifetime is how long a person has to complete a sign-in at the
// identity provider.
const flowLifetime = 10 * time.Minute

// Store keeps users, sessions and sign-ins in progress in Postgres.
type Store struct {
	db  *pgxpool.Pool
	now func() time.Time
}

// NewStore returns a store.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db, now: time.Now}
}

// SetClock replaces the clock, so tests can move time.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// newToken returns 256 random bits in a form that is safe in a cookie and a
// URL.
func newToken() string {
	b := make([]byte, 32)
	// crypto/rand.Read never returns an error.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken is the form in which tokens are stored and looked up. The tokens
// are random and long, so a plain hash is enough: there is nothing to guess.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SignIn records a sign-in: it creates the user on the first one and updates
// the email address, name and time of sign-in on later ones. The user is
// found by issuer and subject, which the provider never changes, so a person
// whose email address changed keeps their account.
//
// The grant is applied as it stands: the user's role becomes g.Role, and
// their team memberships of origin "group" become exactly g.Teams. A group
// that the person has left therefore takes its role and its teams with it.
// Their memberships of origin "config" become exactly g.Listed, which is how
// someone listed in a team before they ever signed in joins it now, and how
// memberships follow a changed email address.
func (s *Store) SignIn(ctx context.Context, id Identity, g Grant) (u User, created bool, err error) {
	now := s.now()
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var disabled bool
		err := tx.QueryRow(ctx, `
			UPDATE users SET email = $3, display_name = $4, role = $6, last_sign_in_at = $5,
				updated_at = CASE WHEN email = $3 AND display_name = $4 AND role = $6 THEN updated_at ELSE $5 END
			WHERE oidc_issuer = $1 AND oidc_subject = $2
			RETURNING id::text, email, display_name, role, disabled_at IS NOT NULL`,
			id.Issuer, id.Subject, id.Email, id.Name, now, g.Role).Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			created = true
			err = tx.QueryRow(ctx, `
				INSERT INTO users (email, display_name, role, oidc_issuer, oidc_subject, created_at, updated_at, last_sign_in_at)
				VALUES ($3, $4, $6, $1, $2, $5, $5, $5)
				RETURNING id::text, email, display_name, role`,
				id.Issuer, id.Subject, id.Email, id.Name, now, g.Role).Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "users_email_key" {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		if disabled {
			return ErrUserDisabled
		}
		// Only rows of origin "group" are touched; memberships that come
		// from the members list of a team belong to the configuration.
		// A nil slice would reach Postgres as NULL, and comparing with NULL
		// matches nothing, so no membership would ever be removed.
		teams := g.Teams
		if teams == nil {
			teams = []string{}
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM team_members m USING teams t
			WHERE m.team_id = t.id AND m.user_id = $1 AND m.origin = 'group' AND NOT (t.name = ANY($2) AND t.removed_at IS NULL)`,
			u.ID, teams); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO team_members (team_id, user_id, origin, created_at)
			SELECT t.id, $1, 'group', $3 FROM teams t WHERE t.name = ANY($2) AND t.removed_at IS NULL
			ON CONFLICT DO NOTHING`, u.ID, teams, now); err != nil {
			return err
		}
		// The same for the teams that list the person by email address.
		// Reconciliation writes these rows for users who exist; this covers
		// the person who did not exist, or had another address, when the
		// configuration was last applied.
		teams = g.Listed
		if teams == nil {
			teams = []string{}
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM team_members m USING teams t
			WHERE m.team_id = t.id AND m.user_id = $1 AND m.origin = 'config' AND NOT (t.name = ANY($2) AND t.removed_at IS NULL)`,
			u.ID, teams); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO team_members (team_id, user_id, origin, created_at)
			SELECT t.id, $1, 'config', $3 FROM teams t WHERE t.name = ANY($2) AND t.removed_at IS NULL
			ON CONFLICT DO NOTHING`, u.ID, teams, now)
		return err
	})
	if err != nil {
		if !errors.Is(err, ErrEmailTaken) && !errors.Is(err, ErrUserDisabled) {
			err = fmt.Errorf("record sign-in: %w", err)
		}
		return User{}, false, err
	}
	return u, created, nil
}

// Revoke takes away what earlier sign-ins gave the person with this issuer
// and subject: their sessions and their team memberships of origin "group".
// It is what happens when someone who is no longer in any group with access
// tries to sign in. The user's row stays, for the history that refers to it.
// found is false when there is no such user.
func (s *Store) Revoke(ctx context.Context, issuer, subject string) (found bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var userID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE oidc_issuer = $1 AND oidc_subject = $2 FOR UPDATE`, issuer, subject).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM team_members WHERE user_id = $1 AND origin = 'group'`, userID)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("revoke access: %w", err)
	}
	return found, nil
}

// CreateSession starts a session for the user and returns its token, which
// goes into the cookie and is not stored, and the time it ends.
func (s *Store) CreateSession(ctx context.Context, userID string, lifetime time.Duration) (token string, expires time.Time, err error) {
	token = newToken()
	now := s.now()
	expires = now.Add(lifetime)
	_, err = s.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), userID, now, expires)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return token, expires, nil
}

// Session returns the user of the session with this token, and when the
// session ends. It returns ErrNoSession when the token is unknown, the
// session has expired or the user has been disabled.
func (s *Store) Session(ctx context.Context, token string) (u User, expires time.Time, err error) {
	if token == "" {
		return User{}, time.Time{}, ErrNoSession
	}
	err = s.db.QueryRow(ctx, `
		SELECT u.id::text, u.email, u.display_name, u.role, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2 AND u.disabled_at IS NULL`,
		hashToken(token), s.now()).Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, time.Time{}, ErrNoSession
	}
	if err != nil {
		return User{}, time.Time{}, fmt.Errorf("read session: %w", err)
	}
	return u, expires, nil
}

// EndSession deletes the session with this token. Ending a session that does
// not exist is not an error.
func (s *Store) EndSession(ctx context.Context, token string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token)); err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	return nil
}

// flow is a sign-in that is on its way to the identity provider and back.
type flow struct {
	// state comes back from the provider in the callback, and is also kept
	// in a cookie, which ties the callback to the browser that started.
	state string
	// nonce must come back inside the ID token.
	nonce string
	// verifier is the PKCE code verifier, presented when the code is
	// exchanged.
	verifier string
}

// startFlow stores f until the callback takes it.
func (s *Store) startFlow(ctx context.Context, f flow) error {
	now := s.now()
	_, err := s.db.Exec(ctx, `INSERT INTO sign_in_flows (state_hash, nonce, code_verifier, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hashToken(f.state), f.nonce, f.verifier, now, now.Add(flowLifetime))
	if err != nil {
		return fmt.Errorf("store sign-in: %w", err)
	}
	return nil
}

// takeFlow returns the sign-in that was started with state and deletes it,
// so that a state works once. It returns ErrNoFlow when there is none or it
// has expired.
func (s *Store) takeFlow(ctx context.Context, state string) (flow, error) {
	f := flow{state: state}
	var expires time.Time
	err := s.db.QueryRow(ctx, `DELETE FROM sign_in_flows WHERE state_hash = $1 RETURNING nonce, code_verifier, expires_at`,
		hashToken(state)).Scan(&f.nonce, &f.verifier, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !expires.After(s.now())) {
		return flow{}, ErrNoFlow
	}
	if err != nil {
		return flow{}, fmt.Errorf("read sign-in: %w", err)
	}
	return f, nil
}

// DeleteExpired removes sessions and sign-ins whose time has passed. They
// are already refused; this only keeps the tables small.
func (s *Store) DeleteExpired(ctx context.Context) (n int64, err error) {
	now := s.now()
	for _, table := range []string{"sessions", "sign_in_flows"} {
		tag, err := s.db.Exec(ctx, `DELETE FROM `+table+` WHERE expires_at <= $1`, now)
		if err != nil {
			return n, fmt.Errorf("delete expired %s: %w", table, err)
		}
		n += tag.RowsAffected()
	}
	return n, nil
}
