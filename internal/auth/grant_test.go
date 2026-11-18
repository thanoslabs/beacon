package auth_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/auth/oidctest"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/provision"
)

// mappedTeams are the teams the mapping tests work with.
var mappedTeams = []config.Team{
	{Name: "platform", Groups: []string{"platform-engineers"}},
	{Name: "data", Groups: []string{"data-engineers"}},
	{Name: "listed", Members: []string{"ada@example.com"}},
}

// provisionTeams gives the teams their rows.
func (b *beacon) provisionTeams(teams []config.Team) {
	b.t.Helper()
	if _, err := provision.Reconcile(context.Background(), b.pool, config.Config{Teams: teams}); err != nil {
		b.t.Fatalf("Reconcile: %v", err)
	}
}

// memberships lists a user's teams as "team/origin", in order.
func (b *beacon) memberships(email string) string {
	b.t.Helper()
	var out string
	err := b.pool.QueryRow(context.Background(), `
		SELECT coalesce(string_agg(t.name || '/' || m.origin, ' ' ORDER BY t.name, m.origin), '')
		FROM team_members m JOIN teams t ON t.id = m.team_id JOIN users u ON u.id = m.user_id WHERE u.email = $1`, email).Scan(&out)
	if err != nil {
		b.t.Fatalf("read memberships: %v", err)
	}
	return out
}

func (b *beacon) role(email string) string {
	b.t.Helper()
	var role string
	if err := b.pool.QueryRow(context.Background(), `SELECT role FROM users WHERE email = $1`, email).Scan(&role); err != nil {
		b.t.Fatalf("read role of %s: %v", email, err)
	}
	return role
}

// TestRolesAndTeamsFollowTheGroups signs the same person in several times
// with different groups in the token. Role and teams must follow each time,
// and leaving every mapped group must take access away.
func TestRolesAndTeamsFollowTheGroups(t *testing.T) {
	f := newFakeProvider(t)
	a := fakeAuth(f, "http://127.0.0.1:18080")
	a.OIDC.GroupsClaim = "roles"
	a.Roles = &config.RoleMapping{Admin: []string{"beacon-admins"}, Member: []string{"beacon-users"}}
	b := newBeacon(t, a)
	b.provisionTeams(mappedTeams)
	b.handler.Configure(context.Background(), a, mappedTeams)

	// signIn completes a sign-in whose token carries these groups in the
	// configured claim, and returns the status, the error and the session.
	signIn := func(subject, email string, groups ...string) (int, string, *http.Cookie) {
		t.Helper()
		at := begin(t, b, f)
		c := at.claims()
		c["sub"], c["email"], c["roles"] = subject, email, groups
		// The default claim must be ignored when another one is configured.
		c["groups"] = []string{"beacon-admins"}
		at.grant("code", sign(t, f.key, c))
		status, msg, cookies := at.callback(url.Values{"code": {"code"}, "state": {at.state}}, at.cookie)
		for _, cookie := range cookies {
			if cookie.Name == "beacon_session" {
				return status, msg, cookie
			}
		}
		return status, msg, nil
	}
	me := func(session *http.Cookie) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, b.server.URL+auth.MePath, nil)
		req.AddCookie(session)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	const ada = "ada@example.com"

	// 1. An admin group and a team group. Ada is also on the members list of
	// the team "listed"; that membership comes from the configuration and
	// must stay whatever her groups are.
	status, msg, first := signIn("ada", ada, "beacon-admins", "platform-engineers", "unrelated")
	if status != http.StatusSeeOther || b.role(ada) != "admin" || b.memberships(ada) != "listed/config platform/group" {
		t.Fatalf("first sign-in = %d %q, role %s, teams %q", status, msg, b.role(ada), b.memberships(ada))
	}
	// 2. No longer an admin, and in another team.
	status, msg, second := signIn("ada", ada, "beacon-users", "data-engineers")
	if status != http.StatusSeeOther || b.role(ada) != "member" || b.memberships(ada) != "data/group listed/config" {
		t.Fatalf("second sign-in = %d %q, role %s, teams %q", status, msg, b.role(ada), b.memberships(ada))
	}
	// The earlier session is still valid, and now has the new role: the role
	// belongs to the user, not to the session.
	if me(first) != http.StatusOK || me(second) != http.StatusOK {
		t.Error("the sessions did not survive a later sign-in")
	}

	// 3. In no mapped group any more: no access, and what was given is
	// taken back.
	status, msg, session := signIn("ada", ada, "data-engineers")
	if status != http.StatusForbidden || !strings.Contains(msg, "not in a group that has access") || session != nil {
		t.Fatalf("sign-in without a mapped group = %d %q", status, msg)
	}
	if me(first) != http.StatusUnauthorized || me(second) != http.StatusUnauthorized {
		t.Error("the sessions outlived the person's access")
	}
	if got := b.memberships(ada); got != "listed/config" {
		t.Errorf("teams after access was removed = %q, want only those from the configuration", got)
	}
	if n := b.count(`SELECT count(*) FROM users WHERE email = $1`, ada); n != 1 {
		t.Error("the user row must stay, for the history that refers to it")
	}

	// 4. Back in a group: access returns.
	if status, msg, _ := signIn("ada", ada, "beacon-users", "platform-engineers"); status != http.StatusSeeOther || b.memberships(ada) != "listed/config platform/group" {
		t.Fatalf("sign-in after returning = %d %q, teams %q", status, msg, b.memberships(ada))
	}

	// Still with access, but in no team's group any more: the last group
	// membership goes, and the ones from the configuration stay.
	if status, msg, _ := signIn("ada", ada, "beacon-users"); status != http.StatusSeeOther || b.memberships(ada) != "listed/config" {
		t.Fatalf("sign-in with a role and no team group = %d %q, teams %q", status, msg, b.memberships(ada))
	}
	if status, msg, _ := signIn("ada", ada, "beacon-users", "platform-engineers"); status != http.StatusSeeOther {
		t.Fatalf("sign-in = %d %q", status, msg)
	}

	// Someone who was never in a mapped group gets no user at all.
	if status, _, _ := signIn("eve", "eve@example.com", "platform-engineers"); status != http.StatusForbidden {
		t.Errorf("sign-in of a stranger = %d, want 403", status)
	}
	if n := b.count(`SELECT count(*) FROM users WHERE email = 'eve@example.com'`); n != 0 {
		t.Error("a refused stranger got a user")
	}

	// A team that left the configuration gives no membership, and a changed
	// mapping applies at the next sign-in.
	b.provisionTeams(mappedTeams[1:])
	a.Roles = &config.RoleMapping{Admin: []string{"beacon-users"}}
	b.handler.Configure(context.Background(), a, mappedTeams[1:])
	if status, msg, _ := signIn("ada", ada, "beacon-users", "platform-engineers", "data-engineers"); status != http.StatusSeeOther || b.role(ada) != "admin" || b.memberships(ada) != "data/group listed/config" {
		t.Fatalf("sign-in after the configuration changed = %d %q, role %s, teams %q", status, msg, b.role(ada), b.memberships(ada))
	}
	// With only admins mapped, everyone else who can sign in is a member.
	if status, _, _ := signIn("grace", "grace@example.com"); status != http.StatusSeeOther || b.role("grace@example.com") != "member" {
		t.Errorf("sign-in without groups when only admins are mapped = %d", status)
	}
}

// TestGroupsFromDex maps the groups that a real provider puts in its ID
// token: Dex sends a "groups" list when the groups scope is asked for.
func TestGroupsFromDex(t *testing.T) {
	a := dexAuth(t)
	a.OIDC.Scopes = []string{"openid", "profile", "email", "groups"}
	a.Roles = &config.RoleMapping{Admin: []string{"beacon-admins"}, Member: []string{"platform-engineers"}}
	b := newBeacon(t, a)
	b.provisionTeams(mappedTeams)
	b.handler.Configure(context.Background(), a, mappedTeams)

	// Ada is in beacon-admins and platform-engineers.
	code, body := oidctest.SignIn(t, oidctest.Browser(t, b.addr()), oidctest.Ada, oidctest.Password)
	if code != http.StatusOK || !strings.Contains(body, `"role":"admin"`) || b.memberships(oidctest.Ada) != "listed/config platform/group" {
		t.Errorf("Ada: %d %s, teams %q\nlogs:\n%s", code, body, b.memberships(oidctest.Ada), b.logs)
	}
	// Grace is in platform-engineers only.
	code, body = oidctest.SignIn(t, oidctest.Browser(t, b.addr()), oidctest.Grace, oidctest.Password)
	if code != http.StatusOK || !strings.Contains(body, `"role":"member"`) || b.memberships(oidctest.Grace) != "platform/group" {
		t.Errorf("Grace: %d %s, teams %q", code, body, b.memberships(oidctest.Grace))
	}
	// Linus is in no group.
	code, body = oidctest.SignIn(t, oidctest.Browser(t, b.addr()), oidctest.Linus, oidctest.Password)
	if code != http.StatusForbidden || !strings.Contains(body, "not in a group that has access") {
		t.Errorf("Linus: %d %s, want 403", code, body)
	}
	if n := b.count(`SELECT count(*) FROM users`); n != 2 {
		t.Errorf("%d users, want Ada and Grace", n)
	}

	// Without the groups scope Dex sends no groups, so nobody is mapped:
	// the scope is part of the setup, and the refusal says what is missing.
	a.OIDC.Scopes = nil
	b.handler.Configure(context.Background(), a, mappedTeams)
	if code, _ := oidctest.SignIn(t, oidctest.Browser(t, b.addr()), oidctest.Ada, oidctest.Password); code != http.StatusForbidden {
		t.Errorf("Ada without the groups scope: %d, want 403", code)
	}
}

// TestListedMembersJoinWhenTheySignIn covers the person whom a team's
// members list names before they have ever signed in: reconciliation had to
// skip them, and the sign-in that creates the user adds them.
func TestListedMembersJoinWhenTheySignIn(t *testing.T) {
	f := newFakeProvider(t)
	a := fakeAuth(f, "http://127.0.0.1:18080")
	teams := []config.Team{
		{Name: "platform", Members: []string{"Ada@Example.com", "grace@example.com"}, Groups: []string{"platform-engineers"}},
		{Name: "data", Members: []string{"ada.lovelace@example.com"}},
		{Name: "other", Members: []string{"grace@example.com"}},
	}
	b := newBeacon(t, a)
	b.provisionTeams(teams)
	b.handler.Configure(context.Background(), a, teams)
	hooks := 0
	b.handler.AfterSignIn(func(context.Context) { hooks++ })

	signIn := func(email string, groups ...string) {
		t.Helper()
		at := begin(t, b, f)
		c := at.claims()
		c["sub"], c["email"], c["groups"] = "ada", email, groups
		at.grant("code", sign(t, f.key, c))
		if status, msg, _ := at.callback(url.Values{"code": {"code"}, "state": {at.state}}, at.cookie); status != http.StatusSeeOther {
			t.Fatalf("sign-in = %d %q", status, msg)
		}
	}

	// Listed and in the team's group: a member twice over, once per origin.
	signIn("ada@example.com", "platform-engineers")
	if got := b.memberships("ada@example.com"); got != "platform/config platform/group" {
		t.Errorf("memberships after the first sign-in = %q", got)
	}
	// Leaving the group removes only the membership that came from it.
	signIn("ada@example.com")
	if got := b.memberships("ada@example.com"); got != "platform/config" {
		t.Errorf("memberships after leaving the group = %q", got)
	}
	// The provider changes the address: the memberships follow the lists.
	signIn("ada.lovelace@example.com")
	if got := b.memberships("ada.lovelace@example.com"); got != "data/config" {
		t.Errorf("memberships after the address changed = %q", got)
	}
	if hooks != 3 {
		t.Errorf("the hook ran %d times, want once per sign-in", hooks)
	}
}
