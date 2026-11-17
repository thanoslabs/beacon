// Package access holds who is making a request and what they may do. It
// depends on nothing else in Beacon, so every package can use it.
package access

import "context"

// Role is what a user or an API token is allowed to do.
type Role string

// The roles. An admin may do everything a member may.
const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleMember }

// Principal is the authenticated party behind a request.
type Principal struct {
	// UserID is the user's row. It is empty for an API token.
	UserID string
	Email  string
	Name   string
	Role   Role
}

type contextKey struct{}

// WithPrincipal returns a context that carries p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// FromContext returns the principal of the request, if it has one.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	return p, ok
}
