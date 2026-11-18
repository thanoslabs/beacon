package auth

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/config"
)

// TestGroupsOf covers the shapes in which providers send roles and groups.
func TestGroupsOf(t *testing.T) {
	tests := []struct {
		name   string
		claims string
		claim  string
		want   string
	}{
		{"a list", `{"groups": ["a", "b"]}`, "groups", "a,b"},
		{"a single string", `{"groups": "a"}`, "groups", "a"},
		{"another claim", `{"roles": ["admin"], "groups": ["a"]}`, "roles", "admin"},
		{"nested, as Keycloak sends realm roles", `{"realm_access": {"roles": ["admin", "user"]}}`, "realm_access.roles", "admin,user"},
		{"nested two levels", `{"resource_access": {"beacon": {"roles": ["admin"]}}}`, "resource_access.beacon.roles", "admin"},
		{"a claim named by a URL, as Auth0 requires", `{"https://example.com/groups": ["a"]}`, "https://example.com/groups", "a"},
		{"a name with dots wins over a path", `{"a.b": ["flat"], "a": {"b": ["nested"]}}`, "a.b", "flat"},
		{"missing", `{"email": "ada@example.com"}`, "groups", ""},
		{"missing nested", `{"realm_access": {}}`, "realm_access.roles", ""},
		{"a path through something that is not an object", `{"realm_access": "x"}`, "realm_access.roles", ""},
		{"empty list", `{"groups": []}`, "groups", ""},
		{"null", `{"groups": null}`, "groups", ""},
		{"empty strings and other types are left out", `{"groups": ["a", "", 5, true, null, ["b"], {"c": 1}]}`, "groups", "a"},
		{"a number", `{"groups": 5}`, "groups", ""},
		{"an object", `{"groups": {"a": true}}`, "groups", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var claims map[string]any
			if err := json.Unmarshal([]byte(tt.claims), &claims); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(groupsOf(claims, tt.claim), ","); got != tt.want {
				t.Errorf("groupsOf = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGrant(t *testing.T) {
	both := &config.RoleMapping{Admin: []string{"admins", "owners"}, Member: []string{"users"}}
	adminOnly := &config.RoleMapping{Admin: []string{"admins"}}
	teams := []config.Team{{Name: "platform", Groups: []string{"platform-engineers", "sre"}}, {Name: "data", Groups: []string{"data"}}, {Name: "listed-only"}}
	tests := []struct {
		name      string
		roles     *config.RoleMapping
		groups    []string
		wantRole  access.Role
		wantTeams string
		refused   bool
	}{
		{name: "no mapping: everyone is a member", groups: []string{"admins"}, wantRole: access.RoleMember},
		{name: "no mapping and no groups", wantRole: access.RoleMember},
		{name: "an admin group", roles: both, groups: []string{"owners"}, wantRole: access.RoleAdmin},
		{name: "admin wins over member", roles: both, groups: []string{"users", "admins"}, wantRole: access.RoleAdmin},
		{name: "a member group", roles: both, groups: []string{"users", "sre"}, wantRole: access.RoleMember, wantTeams: "platform"},
		{name: "in no mapped group", roles: both, groups: []string{"sre", "data"}, refused: true},
		{name: "no groups at all", roles: both, refused: true},
		{name: "names are compared exactly", roles: both, groups: []string{"Admins", "admins "}, refused: true},
		{name: "only admins mapped: the rest are members", roles: adminOnly, groups: []string{"data", "sre"}, wantRole: access.RoleMember, wantTeams: "platform,data"},
		{name: "only admins mapped, an admin", roles: adminOnly, groups: []string{"admins"}, wantRole: access.RoleAdmin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, ok := grant(tt.roles, teams, tt.groups)
			if ok == tt.refused || g.Role != tt.wantRole || strings.Join(g.Teams, ",") != tt.wantTeams {
				t.Errorf("grant = %+v, %v; want role %q, teams %q, refused %v", g, ok, tt.wantRole, tt.wantTeams, tt.refused)
			}
		})
	}
}
