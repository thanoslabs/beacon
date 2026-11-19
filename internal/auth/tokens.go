package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/api"
	"github.com/thanoslabs/beacon/internal/config"
)

// TokensPath is the route of the API tokens.
const TokensPath = "/api/v1/tokens"

// tokenPrefix starts every API token, so that one can be recognised where
// it should not be, such as in a commit.
const tokenPrefix = "bcn_"

// lastUsedPrecision is how exact a token's time of last use is. It keeps a
// busy token from writing its row on every request.
const lastUsedPrecision = time.Minute

// The limits of a token's name and lifetime.
const (
	maxTokenNameLength = 100
	maxTokenLifetime   = 5 * 365 * config.Day
)

// Token is an API token as it is stored: without the secret.
type Token struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	Role access.Role `json:"role"`
	// CreatedBy is the email address of the user who created the token. It
	// is empty when the token was created with another token.
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ErrNoToken means that no token has this identifier.
var ErrNoToken = errors.New("no such token")

// CreateToken stores a new token and returns it with its secret, which
// cannot be read again: only its hash is kept. userID is the user who
// creates it, or empty.
func (s *Store) CreateToken(ctx context.Context, name string, role access.Role, lifetime time.Duration, userID string) (Token, string, error) {
	secret := tokenPrefix + newToken()
	now := s.now()
	t := Token{Name: name, Role: role, CreatedAt: now.UTC()}
	if lifetime > 0 {
		expires := now.Add(lifetime).UTC()
		t.ExpiresAt = &expires
	}
	var createdBy *string
	if userID != "" {
		createdBy = &userID
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO api_tokens (name, token_hash, role, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
		name, hashToken(secret), role, createdBy, now, t.ExpiresAt).Scan(&t.ID)
	if err != nil {
		return Token{}, "", fmt.Errorf("create token: %w", err)
	}
	return t, secret, nil
}

// Tokens lists every token, newest first, including revoked ones.
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id::text, t.name, t.role, coalesce(u.email, ''), t.created_at, t.expires_at, t.last_used_at, t.revoked_at
		FROM api_tokens t LEFT JOIN users u ON u.id = t.created_by ORDER BY t.created_at DESC, t.id`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	tokens, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Token, error) {
		var t Token
		err := row.Scan(&t.ID, &t.Name, &t.Role, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt)
		// The API answers times in UTC.
		t.CreatedAt = t.CreatedAt.UTC()
		for _, at := range []*time.Time{t.ExpiresAt, t.LastUsedAt, t.RevokedAt} {
			if at != nil {
				*at = at.UTC()
			}
		}
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return tokens, nil
}

// RevokeToken makes the token stop working. Revoking a token twice is not an
// error. It returns ErrNoToken when there is no token with this identifier.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `UPDATE api_tokens SET revoked_at = coalesce(revoked_at, $2) WHERE id = $1`, id, s.now())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P02" { // not a UUID
		return ErrNoToken
	}
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoToken
	}
	return nil
}

// tokenPrincipal returns who a token stands for. It returns an error that
// wraps access.ErrUnauthenticated when the secret belongs to no token, or to
// one that is revoked or expired.
func (s *Store) tokenPrincipal(ctx context.Context, secret string) (access.Principal, error) {
	denied := fmt.Errorf("no valid token: %w", access.ErrUnauthenticated)
	if !strings.HasPrefix(secret, tokenPrefix) {
		return access.Principal{}, denied
	}
	now := s.now()
	hash := hashToken(secret)
	var (
		p        access.Principal
		stored   string
		lastUsed *time.Time
	)
	err := s.db.QueryRow(ctx, `
		SELECT id::text, name, role, token_hash, last_used_at FROM api_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2)`,
		hash, now).Scan(&p.TokenID, &p.Name, &p.Role, &stored, &lastUsed)
	// The lookup is by the hash of a long random secret, so its timing tells
	// an attacker nothing useful; the comparison is constant-time all the
	// same.
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1) {
		return access.Principal{}, denied
	}
	if err != nil {
		return access.Principal{}, fmt.Errorf("read token: %w", err)
	}
	if lastUsed == nil || now.Sub(*lastUsed) >= lastUsedPrecision {
		if _, err := s.db.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, p.TokenID, now); err != nil {
			return access.Principal{}, fmt.Errorf("record token use: %w", err)
		}
	}
	return p, nil
}

// bearer returns the token of an Authorization header, and whether the
// header holds one.
func bearer(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// tokenRoutes are the routes that manage tokens. All need the admin role: a
// token can carry any role, so whoever may create one may do anything.
func (h *Handler) tokenRoutes() []access.Route {
	return []access.Route{
		{Pattern: "GET " + TokensPath, Level: access.Admin, Handler: http.HandlerFunc(h.handleListTokens)},
		{Pattern: "POST " + TokensPath, Level: access.Admin, Handler: http.HandlerFunc(h.handleCreateToken)},
		{Pattern: "DELETE " + TokensPath + "/{id}", Level: access.Admin, Handler: http.HandlerFunc(h.handleRevokeToken)},
	}
}

func (h *Handler) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.store.Tokens(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "list tokens", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	page, next, field, problem, ok := api.Slice(tokens, r.URL.Query())
	if !ok {
		h.write(w, r, http.StatusBadRequest, api.Error{Error: problem, Field: field})
		return
	}
	h.write(w, r, http.StatusOK, struct {
		Tokens     []Token `json:"tokens"`
		NextCursor string  `json:"next_cursor,omitempty"`
	}{page, next})
}

// createTokenRequest is the body of a request for a new token.
type createTokenRequest struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// ExpiresIn is how long the token is valid, such as 30d. Empty means
	// that it does not expire.
	ExpiresIn string `json:"expires_in"`
}

func (h *Handler) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		h.fail(w, r, http.StatusBadRequest, `the body must be JSON such as {"name": "ci", "role": "member", "expires_in": "90d"}`)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	var lifetime time.Duration
	switch {
	case req.Name == "" || len(req.Name) > maxTokenNameLength:
		h.fail(w, r, http.StatusBadRequest, fmt.Sprintf("name is required, and at most %d characters", maxTokenNameLength))
		return
	case !access.Role(req.Role).Valid():
		h.fail(w, r, http.StatusBadRequest, "role is required: admin or member")
		return
	case req.ExpiresIn != "":
		d, err := config.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 || time.Duration(d) > maxTokenLifetime {
			h.fail(w, r, http.StatusBadRequest, "expires_in must be a duration such as 12h, 30d or 52w, at most 5 years; leave it out for a token that does not expire")
			return
		}
		lifetime = time.Duration(d)
	}
	by, _ := access.FromContext(r.Context())
	t, secret, err := h.store.CreateToken(r.Context(), req.Name, access.Role(req.Role), lifetime, by.UserID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "create token", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	// The secret goes to the caller and nowhere else.
	h.logger.InfoContext(r.Context(), "api token created", slog.String("token_id", t.ID), slog.String("name", t.Name),
		slog.String("role", string(t.Role)), slog.String("by_user", by.UserID), slog.String("by_token", by.TokenID))
	t.CreatedBy = by.Email
	h.write(w, r, http.StatusCreated, struct {
		Token
		// Secret is the token itself. It is shown this once.
		Secret string `json:"token"`
	}{t, secret})
}

func (h *Handler) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.store.RevokeToken(r.Context(), id)
	if errors.Is(err, ErrNoToken) {
		h.fail(w, r, http.StatusNotFound, "no such token")
		return
	}
	if err != nil {
		h.logger.ErrorContext(r.Context(), "revoke token", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	by, _ := access.FromContext(r.Context())
	h.logger.InfoContext(r.Context(), "api token revoked", slog.String("token_id", id), slog.String("by_user", by.UserID), slog.String("by_token", by.TokenID))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
