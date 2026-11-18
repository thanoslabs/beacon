package access

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Level is who may use a route.
type Level int

// The levels. The zero value is the strictest, so a route whose level was
// forgotten is closed to everyone but admins rather than open.
const (
	// Admin routes need the admin role.
	Admin Level = iota
	// Member routes need a signed-in user or an API token, with either
	// role.
	Member
	// Public routes are reachable without a session. Use it only for
	// endpoints that must be, such as probes, and for endpoints that check
	// a credential of their own, such as a source's token.
	Public
)

func (l Level) String() string {
	switch l {
	case Admin:
		return "admin"
	case Member:
		return "member"
	case Public:
		return "public"
	}
	return "unknown"
}

// Route is an HTTP route and who may use it.
type Route struct {
	// Pattern is a net/http.ServeMux pattern with a method, such as
	// "GET /api/v1/me".
	Pattern string
	Level   Level
	Handler http.Handler
}

// ErrUnauthenticated is what an Authenticator returns, alone or wrapped,
// when a request carries no valid credential.
var ErrUnauthenticated = errors.New("not authenticated")

// Authenticator tells who is behind a request.
type Authenticator interface {
	// Authenticate returns the principal of r, or an error that wraps
	// ErrUnauthenticated when r carries no valid credential.
	Authenticate(r *http.Request) (Principal, error)
}

// Guard enforces the level of routes.
type Guard struct {
	logger *slog.Logger
	auth   Authenticator
	// csrf refuses requests that another site made a browser send. A
	// session cookie travels with such a request; this is what stops it.
	csrf *http.CrossOriginProtection
}

// NewGuard returns a guard that asks auth who is behind each request. With
// a nil auth nobody is ever authenticated, so only public routes work.
func NewGuard(logger *slog.Logger, auth Authenticator) *Guard {
	return &Guard{logger: logger, auth: auth, csrf: http.NewCrossOriginProtection()}
}

// Wrap returns the route's handler behind the checks of its level. A public
// route is returned as it is. Every other route answers 401 without a valid
// credential, 403 to a request that changes something and was sent by
// another site, and 403 to a member on an admin route; the handler then
// finds the principal in the request's context.
func (g *Guard) Wrap(route Route) http.Handler {
	if route.Level == Public {
		return route.Handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := g.csrf.Check(r); err != nil {
			g.refuse(w, r, http.StatusForbidden, "the request came from another site")
			return
		}
		if g.auth == nil {
			g.unauthenticated(w, r)
			return
		}
		p, err := g.auth.Authenticate(r)
		if errors.Is(err, ErrUnauthenticated) {
			g.unauthenticated(w, r)
			return
		}
		if err != nil {
			g.logger.ErrorContext(r.Context(), "authenticate request", slog.Any("error", err))
			g.refuse(w, r, http.StatusInternalServerError, "internal error")
			return
		}
		// Anything that is not explicitly open to members needs an admin.
		if route.Level != Member && p.Role != RoleAdmin {
			g.refuse(w, r, http.StatusForbidden, "this needs the admin role")
			return
		}
		next := route.Handler
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func (g *Guard) unauthenticated(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="beacon"`)
	g.refuse(w, r, http.StatusUnauthorized, "authentication is required: sign in at /auth/login")
}

func (g *Guard) refuse(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": msg}); err != nil {
		g.logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}
