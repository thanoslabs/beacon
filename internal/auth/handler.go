package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/config"
)

// Paths of the sign-in endpoints.
const (
	LoginPath  = "/auth/login"
	LogoutPath = "/auth/logout"
	// MePath answers who is signed in. It is where the browser lands after
	// signing in, until there is a web interface to land on.
	MePath = "/api/v1/me"
)

// Handler serves the sign-in endpoints and tells who is behind a request.
type Handler struct {
	logger *slog.Logger
	store  *Store
	// settings follow the configuration, and are replaced as a whole when it
	// is reloaded. Nil means that nobody can sign in.
	settings atomic.Pointer[settings]
	// csrf refuses requests that another site made a browser send.
	csrf *http.CrossOriginProtection
}

// settings is the auth block of the configuration, prepared for use.
type settings struct {
	auth config.Auth
	// teams are the teams of the configuration, whose groups decide which
	// teams a person belongs to.
	teams []config.Team
	// secure is whether Beacon is reached over https, and so whether
	// cookies are marked Secure.
	secure bool

	// The provider is discovered on first use, not at start: Beacon must
	// start, and keep handling alerts, while the identity provider is down.
	mu       sync.Mutex
	provider *provider
}

// NewHandler returns a handler with which nobody can sign in until
// Configure is called.
func NewHandler(logger *slog.Logger, store *Store) *Handler {
	return &Handler{logger: logger, store: store, csrf: http.NewCrossOriginProtection()}
}

// Configure makes a the active sign-in settings; nil turns sign-in off.
// teams are the teams of the configuration. It is safe while requests are
// being handled. Sessions that exist stay valid as long as sign-in stays on;
// a changed mapping of groups applies to a person at their next sign-in.
func (h *Handler) Configure(ctx context.Context, a *config.Auth, teams []config.Team) {
	if a == nil {
		h.settings.Store(nil)
		h.logger.WarnContext(ctx, "sign-in is not configured, so nobody can sign in; add an auth block to the configuration")
		return
	}
	s := &settings{auth: *a, teams: teams, secure: strings.HasPrefix(a.PublicURL, "https://")}
	h.settings.Store(s)
	if a.OIDC == nil {
		return
	}
	h.logger.InfoContext(ctx, "sign-in configured", slog.String("issuer", a.OIDC.Issuer), slog.String("client_id", a.OIDC.ClientID),
		slog.String("redirect_uri", s.redirectURL()), slog.Duration("session_lifetime", a.Lifetime()),
		slog.String("groups_claim", a.OIDC.Claim()), slog.Bool("roles_mapped", a.Roles != nil))
	if _, err := s.discovered(ctx); err != nil {
		h.logger.WarnContext(ctx, "the identity provider cannot be reached; sign-in will try again when someone signs in", slog.Any("error", err))
	}
}

// redirectURL is the redirect URI registered with the provider.
func (s *settings) redirectURL() string {
	return strings.TrimRight(s.auth.PublicURL, "/") + CallbackPath
}

// discovered returns the provider, reading its discovery document if that
// has not succeeded yet.
func (s *settings) discovered(ctx context.Context) (*provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider == nil {
		p, err := discover(ctx, *s.auth.OIDC, s.redirectURL())
		if err != nil {
			return nil, err
		}
		s.provider = p
	}
	return s.provider, nil
}

// The cookies. Their names carry the prefixes that make a browser refuse
// them unless they were set over https, when Beacon is reached over https.
func (s *settings) sessionCookie() string {
	if s.secure {
		return "__Host-beacon_session"
	}
	return "beacon_session"
}

func (s *settings) flowCookie() string {
	if s.secure {
		return "__Secure-beacon_sign_in"
	}
	return "beacon_sign_in"
}

// Register attaches the endpoints to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+LoginPath, h.handleLogin)
	mux.HandleFunc("GET "+CallbackPath, h.handleCallback)
	mux.Handle("POST "+LogoutPath, h.RequireSession(http.HandlerFunc(h.handleLogout)))
	mux.Handle("GET "+MePath, h.RequireSession(http.HandlerFunc(h.handleMe)))
}

// Authenticate returns who is behind r, from its session cookie. It returns
// ErrNoSession when there is no valid session.
func (h *Handler) Authenticate(r *http.Request) (access.Principal, error) {
	p, _, err := h.session(r)
	return p, err
}

func (h *Handler) session(r *http.Request) (access.Principal, time.Time, error) {
	s := h.settings.Load()
	if s == nil {
		return access.Principal{}, time.Time{}, ErrNoSession
	}
	cookie, err := r.Cookie(s.sessionCookie())
	if err != nil {
		return access.Principal{}, time.Time{}, ErrNoSession
	}
	u, expires, err := h.store.Session(r.Context(), cookie.Value)
	if err != nil {
		return access.Principal{}, time.Time{}, err
	}
	return access.Principal{UserID: u.ID, Email: u.Email, Name: u.DisplayName, Role: u.Role}, expires, nil
}

// RequireSession lets a request through only when it carries a valid
// session, and puts the principal into its context. A request that changes
// something is also refused when another site made the browser send it.
func (h *Handler) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h.csrf.Check(r); err != nil {
			h.fail(w, r, http.StatusForbidden, "the request came from another site")
			return
		}
		p, err := h.Authenticate(r)
		if errors.Is(err, ErrNoSession) {
			h.fail(w, r, http.StatusUnauthorized, "sign in first, at "+LoginPath)
			return
		}
		if err != nil {
			h.logger.ErrorContext(r.Context(), "read session", slog.Any("error", err))
			h.fail(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		next.ServeHTTP(w, r.WithContext(access.WithPrincipal(r.Context(), p)))
	})
}

// handleLogin starts a sign-in and sends the browser to the provider.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	s := h.settings.Load()
	if s == nil || s.auth.OIDC == nil {
		h.fail(w, r, http.StatusNotFound, "sign-in through an identity provider is not configured")
		return
	}
	p, err := s.discovered(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "the identity provider cannot be reached", slog.Any("error", err))
		h.fail(w, r, http.StatusServiceUnavailable, "the identity provider cannot be reached; try again shortly")
		return
	}
	// State, nonce and PKCE verifier are all fresh and unguessable.
	f := flow{state: newToken(), nonce: newToken(), verifier: newToken()}
	if err := h.store.startFlow(r.Context(), f); err != nil {
		h.logger.ErrorContext(r.Context(), "start sign-in", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	// The cookie ties the sign-in to this browser: a callback is accepted
	// only from the browser that started, so nobody can be signed in to an
	// account they did not choose by being sent a link.
	http.SetCookie(w, &http.Cookie{
		Name: s.flowCookie(), Value: f.state, Path: "/auth/", MaxAge: int(flowLifetime.Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, p.authURL(f), http.StatusFound)
}

// providerErrorRE matches the error codes of OAuth 2.0, which is all that is
// repeated from a callback.
var providerErrorRE = regexp.MustCompile(`^[a-z_]{1,64}$`)

// handleCallback completes a sign-in when the provider sends the browser
// back.
func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := h.settings.Load()
	if s == nil || s.auth.OIDC == nil {
		h.fail(w, r, http.StatusNotFound, "sign-in through an identity provider is not configured")
		return
	}
	// The sign-in cookie has done its work whatever happens next.
	http.SetCookie(w, &http.Cookie{Name: s.flowCookie(), Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})

	query := r.URL.Query()
	state := query.Get("state")
	cookie, err := r.Cookie(s.flowCookie())
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		h.fail(w, r, http.StatusBadRequest, "this sign-in was not started in this browser, or it has expired; start again at "+LoginPath)
		return
	}
	f, err := h.store.takeFlow(ctx, state)
	if errors.Is(err, ErrNoFlow) {
		h.fail(w, r, http.StatusBadRequest, "this sign-in has expired or was already completed; start again at "+LoginPath)
		return
	}
	if err != nil {
		h.logger.ErrorContext(ctx, "complete sign-in", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if code := query.Get("error"); code != "" {
		if !providerErrorRE.MatchString(code) {
			code = "invalid"
		}
		h.logger.WarnContext(ctx, "the identity provider refused the sign-in", slog.String("provider_error", code))
		h.fail(w, r, http.StatusUnauthorized, "the identity provider refused the sign-in: "+code)
		return
	}

	p, err := s.discovered(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "the identity provider cannot be reached", slog.Any("error", err))
		h.fail(w, r, http.StatusServiceUnavailable, "the identity provider cannot be reached; try again shortly")
		return
	}
	id, claims, err := p.exchange(ctx, query.Get("code"), f)
	var rejected *errRejected
	if errors.As(err, &rejected) {
		h.logger.WarnContext(ctx, "sign-in refused", slog.String("reason", rejected.reason))
		h.fail(w, r, http.StatusForbidden, rejected.reason)
		return
	}
	if err != nil {
		// Neither the code nor any token is part of these errors.
		h.logger.WarnContext(ctx, "sign-in failed", slog.Any("error", err))
		h.fail(w, r, http.StatusUnauthorized, "the sign-in could not be verified; start again at "+LoginPath)
		return
	}

	// Role and teams are decided afresh from the groups in this token, so
	// that leaving a group at the provider takes effect here.
	groups := groupsOf(claims, s.auth.OIDC.Claim())
	g, ok := grant(s.auth.Roles, s.teams, groups)
	if !ok {
		if _, err := h.store.Revoke(ctx, id.Issuer, id.Subject); err != nil {
			h.logger.ErrorContext(ctx, "complete sign-in", slog.Any("error", err))
			h.fail(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		h.logger.WarnContext(ctx, "sign-in refused", slog.String("reason", "in no group that is mapped to a role"),
			slog.String("email", id.Email), slog.Int("groups", len(groups)))
		h.fail(w, r, http.StatusForbidden, "you are not in a group that has access to Beacon; ask an administrator of Beacon")
		return
	}

	u, created, err := h.store.SignIn(ctx, id, g)
	switch {
	case errors.Is(err, ErrEmailTaken), errors.Is(err, ErrUserDisabled):
		h.logger.WarnContext(ctx, "sign-in refused", slog.String("reason", err.Error()), slog.String("email", id.Email))
		h.fail(w, r, http.StatusForbidden, err.Error()+"; ask an administrator of Beacon")
		return
	case err != nil:
		h.logger.ErrorContext(ctx, "complete sign-in", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	token, expires, err := h.store.CreateSession(ctx, u.ID, s.auth.Lifetime())
	if err != nil {
		h.logger.ErrorContext(ctx, "complete sign-in", slog.Any("error", err))
		h.fail(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	// HttpOnly keeps the token from scripts, Secure from plain http, and
	// SameSite=Lax from requests that other sites make. Lax, not Strict:
	// the browser arrives here by a redirect from the provider's site, and
	// must send the cookie on the request that follows.
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookie(), Value: token, Path: "/", Expires: expires, MaxAge: int(s.auth.Lifetime().Seconds()),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	h.logger.InfoContext(ctx, "user signed in", slog.String("user_id", u.ID), slog.String("email", u.Email),
		slog.String("role", string(u.Role)), slog.Any("teams", g.Teams), slog.Int("groups", len(groups)), slog.Bool("first_sign_in", created))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, MePath, http.StatusSeeOther)
}

// handleLogout ends the session.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	s := h.settings.Load()
	if s == nil {
		h.fail(w, r, http.StatusUnauthorized, "sign in first, at "+LoginPath)
		return
	}
	if cookie, err := r.Cookie(s.sessionCookie()); err == nil {
		if err := h.store.EndSession(r.Context(), cookie.Value); err != nil {
			h.logger.ErrorContext(r.Context(), "sign out", slog.Any("error", err))
			h.fail(w, r, http.StatusInternalServerError, "internal error")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: s.sessionCookie(), Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	if p, ok := access.FromContext(r.Context()); ok {
		h.logger.InfoContext(r.Context(), "user signed out", slog.String("user_id", p.UserID))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// me is the body of MePath.
type me struct {
	ID          string      `json:"id"`
	Email       string      `json:"email"`
	DisplayName string      `json:"display_name"`
	Role        access.Role `json:"role"`
	// SessionExpiresAt is when the person has to sign in again.
	SessionExpiresAt *time.Time `json:"session_expires_at,omitempty"`
}

// handleMe answers who is signed in.
func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := access.FromContext(r.Context())
	body := me{ID: p.UserID, Email: p.Email, DisplayName: p.Name, Role: p.Role}
	if _, expires, err := h.session(r); err == nil {
		body.SessionExpiresAt = &expires
	}
	h.write(w, r, http.StatusOK, body)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h.write(w, r, status, map[string]string{"error": msg})
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}

// RunCleanup deletes expired sessions and sign-ins until ctx is cancelled.
func (s *Store) RunCleanup(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.DeleteExpired(ctx); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "delete expired sessions", slog.Any("error", err))
			}
		}
	}
}
