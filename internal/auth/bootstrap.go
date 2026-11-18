package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/thanoslabs/beacon/internal/access"
)

// BootstrapPath is where the bootstrap admin signs in.
const BootstrapPath = "/auth/bootstrap"

// BootstrapEmail is the email address of the bootstrap admin's user. The
// .invalid domain can never belong to anyone, so no identity provider will
// sign a person in with it.
const BootstrapEmail = "bootstrap-admin@beacon.invalid"

// The bootstrap admin's sign-in is bounded: this many attempts, right or
// wrong, in each window, for the whole process. A password is the one
// credential here that a person chose, so guessing must be slow.
const (
	bootstrapAttempts = 5
	bootstrapWindow   = time.Minute
)

// attemptLimiter allows a fixed number of attempts in each window.
type attemptLimiter struct {
	mu    sync.Mutex
	start time.Time
	used  int
}

// allow reports whether another attempt may be made now, and otherwise how
// long to wait.
func (l *attemptLimiter) allow(now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= bootstrapWindow {
		l.start, l.used = now, 0
	}
	if l.used >= bootstrapAttempts {
		return false, l.start.Add(bootstrapWindow).Sub(now)
	}
	l.used++
	return true, 0
}

// handleBootstrap signs the bootstrap admin in with the password.
func (h *Handler) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := h.settings.Load()
	if s == nil || s.auth.BootstrapAdmin == nil {
		h.fail(w, r, http.StatusNotFound, "the bootstrap admin is not enabled")
		return
	}
	// The route is public, so the guard has not looked at where the request
	// comes from. Another site must not be able to sign a browser in.
	if err := h.csrf.Check(r); err != nil {
		h.fail(w, r, http.StatusForbidden, "the request came from another site")
		return
	}
	if ok, wait := h.attempts.allow(h.store.now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		h.logger.WarnContext(ctx, "bootstrap admin sign-in refused: too many attempts", slog.String("remote_addr", r.RemoteAddr))
		h.fail(w, r, http.StatusTooManyRequests, "too many attempts; wait a minute")
		return
	}

	password, ok := passwordFrom(w, r)
	if !ok {
		h.fail(w, r, http.StatusBadRequest, `send the password as JSON, {"password": "..."}, or as the form field password`)
		return
	}
	// bcrypt does the comparison, in time that does not depend on how much
	// of the password is right.
	if bcrypt.CompareHashAndPassword([]byte(s.auth.BootstrapAdmin.PasswordHash.Reveal()), []byte(password)) != nil {
		h.logger.WarnContext(ctx, "bootstrap admin sign-in failed: wrong password", slog.String("remote_addr", r.RemoteAddr))
		h.fail(w, r, http.StatusUnauthorized, "wrong password")
		return
	}

	u, err := h.store.bootstrapSignIn(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "bootstrap admin sign-in", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	token, expires, err := h.store.CreateSession(ctx, u.ID, s.auth.Lifetime())
	if err != nil {
		h.logger.ErrorContext(ctx, "bootstrap admin sign-in", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookie(), Value: token, Path: "/", Expires: expires, MaxAge: int(s.auth.Lifetime().Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	h.logger.InfoContext(ctx, "bootstrap admin signed in", slog.String("user_id", u.ID), slog.String("remote_addr", r.RemoteAddr))
	h.write(w, r, http.StatusOK, me{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName, Role: u.Role, SessionExpiresAt: &expires})
}

// passwordFrom reads the password from a JSON or form body.
func passwordFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return "", false
		}
		return body.Password, body.Password != ""
	}
	if err := r.ParseForm(); err != nil {
		return "", false
	}
	password := r.PostForm.Get("password")
	return password, password != ""
}

// bootstrapSignIn returns the bootstrap admin's user, creating it on the
// first sign-in and enabling it again if it had been turned off.
func (s *Store) bootstrapSignIn(ctx context.Context) (User, error) {
	var u User
	now := s.now()
	err := s.db.QueryRow(ctx, `
		INSERT INTO users (email, display_name, role, created_at, updated_at, last_sign_in_at)
		VALUES ($1, 'Bootstrap admin', $2, $3, $3, $3)
		ON CONFLICT (email) DO UPDATE SET role = $2, disabled_at = NULL, last_sign_in_at = $3
		WHERE users.oidc_issuer IS NULL
		RETURNING id::text, email, display_name, role`,
		BootstrapEmail, access.RoleAdmin, now).Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role)
	if err != nil {
		return User{}, fmt.Errorf("record bootstrap sign-in: %w", err)
	}
	return u, nil
}

// disableBootstrapAdmin turns the bootstrap admin's user off and ends its
// sessions. It reports whether there was anything to turn off.
func (s *Store) disableBootstrapAdmin(ctx context.Context) (bool, error) {
	var changed bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			UPDATE users SET disabled_at = $2, updated_at = $2
			WHERE email = $1 AND oidc_issuer IS NULL AND disabled_at IS NULL RETURNING id::text`, BootstrapEmail, s.now()).Scan(&id)
		if err == pgx.ErrNoRows { //nolint:errorlint // QueryRow returns the sentinel itself
			return nil
		}
		if err != nil {
			return err
		}
		changed = true
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("disable the bootstrap admin: %w", err)
	}
	return changed, nil
}
