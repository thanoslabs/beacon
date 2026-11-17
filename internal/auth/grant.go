package auth

import (
	"slices"
	"strings"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/config"
)

// Grant is what a sign-in gives a person: their role and the teams they
// belong to through their identity provider groups. It is worked out again
// at every sign-in, from the groups in that sign-in's token.
type Grant struct {
	Role access.Role
	// Teams are the names of the teams whose groups the person is in.
	Teams []string
}

// groupsOf returns the roles or groups that the claim of this name carries.
//
// The claim is looked up by its whole name first, since claim names may be
// URLs with dots in them, and then as a path of nested objects, such as
// realm_access.roles. Its value is a list of strings or a single string;
// anything else gives no groups.
func groupsOf(claims map[string]any, claim string) []string {
	value, ok := claims[claim]
	if !ok {
		var at any = claims
		for part := range strings.SplitSeq(claim, ".") {
			object, isObject := at.(map[string]any)
			if !isObject {
				return nil
			}
			at = object[part]
		}
		value = at
	}
	switch v := value.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []any:
		var groups []string
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				groups = append(groups, s)
			}
		}
		return groups
	}
	return nil
}

// grant decides what a person in these groups gets. ok is false when the
// configuration maps groups to roles and the person is in none of them: they
// have no access.
func grant(roles *config.RoleMapping, teams []config.Team, groups []string) (g Grant, ok bool) {
	in := func(list []string) bool {
		return slices.ContainsFunc(list, func(name string) bool { return slices.Contains(groups, name) })
	}
	switch {
	case roles == nil:
		g.Role = access.RoleMember
	case in(roles.Admin):
		g.Role = access.RoleAdmin
	case len(roles.Member) == 0 || in(roles.Member):
		g.Role = access.RoleMember
	default:
		return Grant{}, false
	}
	for _, t := range teams {
		if in(t.Groups) {
			g.Teams = append(g.Teams, t.Name)
		}
	}
	return g, true
}
