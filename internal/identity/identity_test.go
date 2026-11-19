package identity_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/identity"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *identity.Store
	clock *clock
	// slack and discord are the identifiers of two integrations.
	slack, discord string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, pool: postgrestest.NewMigrated(t), clock: &clock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}}
	f.store = identity.NewStore(f.pool)
	f.store.SetClock(f.clock.Now)
	f.slack = provisiontest.Row(t, f.pool, provision.Integrations, "slack")
	f.discord = provisiontest.Row(t, f.pool, provision.Integrations, "discord")
	return f
}

// user creates a user who signed in through the identity provider, and
// returns their identifier.
func (f *fixture) user(email string) string {
	f.t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(), `INSERT INTO users (email, display_name, oidc_issuer, oidc_subject) VALUES ($1, $2, 'https://idp.example.com', $1) RETURNING id::text`,
		email, strings.ToUpper(email[:1])+strings.Split(email, "@")[0][1:]).Scan(&id)
	if err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func TestLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada, grace := f.user("ada@example.com"), f.user("grace@example.com")

	if _, found, err := f.store.Address(ctx, f.pool, f.slack, ada); found || err != nil {
		t.Fatalf("Address before any link = found %v, %v", found, err)
	}
	if err := f.store.Link(ctx, f.pool, ada, f.slack, identity.ProviderSlack, "U0ADA", identity.OriginEmail); err != nil {
		t.Fatalf("Link: %v", err)
	}
	// Both directions.
	if address, found, err := f.store.Address(ctx, f.pool, f.slack, ada); address != "U0ADA" || !found || err != nil {
		t.Errorf("Address = %q, %v, %v", address, found, err)
	}
	if u, found, err := f.store.UserFor(ctx, f.pool, f.slack, "U0ADA"); u != (identity.User{ID: ada, Email: "ada@example.com", Name: "Ada"}) || !found || err != nil {
		t.Errorf("UserFor = %+v, %v, %v", u, found, err)
	}
	// An account means something only within its integration.
	if _, found, _ := f.store.UserFor(ctx, f.pool, f.discord, "U0ADA"); found {
		t.Error("the Slack account was found through the Discord integration")
	}
	if _, found, _ := f.store.Address(ctx, f.pool, f.discord, ada); found {
		t.Error("Ada has an address with the Discord integration")
	}

	// An account is one person's.
	if err := f.store.Link(ctx, f.pool, grace, f.slack, identity.ProviderSlack, "U0ADA", identity.OriginEmail); !errors.Is(err, identity.ErrTaken) {
		t.Errorf("linking Ada's account to Grace = %v, want ErrTaken", err)
	}
	if u, _, _ := f.store.UserFor(ctx, f.pool, f.slack, "U0ADA"); u.ID != ada {
		t.Errorf("after the refused link the account belongs to %s", u.Email)
	}
	// Linking again is harmless, and a new account replaces the old one.
	if err := f.store.Link(ctx, f.pool, ada, f.slack, identity.ProviderSlack, "U0ADA", identity.OriginEmail); err != nil {
		t.Errorf("linking the same account again: %v", err)
	}
	if err := f.store.Link(ctx, f.pool, ada, f.slack, identity.ProviderSlack, "U0ADA2", identity.OriginSelf); err != nil {
		t.Errorf("linking another account: %v", err)
	}
	if _, found, _ := f.store.UserFor(ctx, f.pool, f.slack, "U0ADA"); found {
		t.Error("the replaced account still belongs to someone")
	}
	if err := f.store.Link(ctx, f.pool, ada, f.discord, identity.ProviderDiscord, "1234567890", identity.OriginSelf); err != nil {
		t.Fatalf("Link(discord): %v", err)
	}

	list, err := f.store.Identities(ctx, ada)
	if err != nil || len(list) != 2 || list[0].Integration != "discord" || list[0].Provider != "discord" || list[0].ExternalID != "1234567890" ||
		list[1].Integration != "slack" || list[1].ExternalID != "U0ADA2" || list[1].Origin != "self" || !list[1].LinkedAt.Equal(f.clock.Now()) {
		t.Errorf("Identities = %+v, %v", list, err)
	}

	// A disabled user is nobody.
	f.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, ada)
	if _, found, _ := f.store.UserFor(ctx, f.pool, f.slack, "U0ADA2"); found {
		t.Error("the account of a disabled user was attributed to them")
	}
	f.exec(`UPDATE users SET disabled_at = NULL WHERE id = $1`, ada)

	if removed, err := f.store.Unlink(ctx, ada, f.slack); !removed || err != nil {
		t.Errorf("Unlink = %v, %v", removed, err)
	}
	if removed, err := f.store.Unlink(ctx, ada, f.slack); removed || err != nil {
		t.Errorf("Unlink twice = %v, %v", removed, err)
	}
	if _, found, _ := f.store.Address(ctx, f.pool, f.slack, ada); found {
		t.Error("the unlinked account is still an address")
	}
}

// TestSelfLink follows the flow with which a person links an account
// themselves: Beacon gives the signed-in user a code, and the account that
// presents the code from inside the chat service becomes theirs.
func TestSelfLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada, grace := f.user("ada@example.com"), f.user("grace@example.com")

	code, expires, err := f.store.StartLink(ctx, ada, f.discord)
	if err != nil || len(code) != 16 || !expires.Equal(f.clock.Now().Add(10*time.Minute)) {
		t.Fatalf("StartLink = %q, %s, %v", code, expires, err)
	}
	// The code itself is not stored.
	var stored int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM identity_link_codes WHERE code_hash = $1`, code).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("the code is stored as it is (%d rows, %v)", stored, err)
	}

	// Not through another integration, and not a guess.
	for name, try := range map[string][2]string{"another integration": {f.slack, code}, "a wrong code": {f.discord, "AAAAAAAAAAAAAAAA"}, "no code": {f.discord, ""}} {
		if _, err := f.store.CompleteLink(ctx, try[0], identity.ProviderDiscord, "111", try[1]); !errors.Is(err, identity.ErrBadCode) {
			t.Errorf("%s: %v, want ErrBadCode", name, err)
		}
	}
	// As a person types it: in lower case, in groups.
	typed := strings.ToLower(code[:4] + "-" + code[4:8] + " " + code[8:12] + "-" + code[12:])
	u, err := f.store.CompleteLink(ctx, f.discord, identity.ProviderDiscord, "111", typed)
	if err != nil || u.ID != ada || u.Email != "ada@example.com" {
		t.Fatalf("CompleteLink = %+v, %v", u, err)
	}
	if address, found, _ := f.store.Address(ctx, f.pool, f.discord, ada); address != "111" || !found {
		t.Errorf("Address after the link = %q, %v", address, found)
	}
	list, _ := f.store.Identities(ctx, ada)
	if len(list) != 1 || list[0].Origin != identity.OriginSelf {
		t.Errorf("Identities = %+v", list)
	}
	// Once.
	if _, err := f.store.CompleteLink(ctx, f.discord, identity.ProviderDiscord, "222", code); !errors.Is(err, identity.ErrBadCode) {
		t.Errorf("using a code twice = %v, want ErrBadCode", err)
	}

	// A new code replaces the one before it, and a code expires.
	first, _, _ := f.store.StartLink(ctx, grace, f.discord)
	second, _, err := f.store.StartLink(ctx, grace, f.discord)
	if err != nil || first == second {
		t.Fatalf("second StartLink = %q after %q, %v", second, first, err)
	}
	if _, err := f.store.CompleteLink(ctx, f.discord, identity.ProviderDiscord, "333", first); !errors.Is(err, identity.ErrBadCode) {
		t.Errorf("a replaced code = %v, want ErrBadCode", err)
	}
	f.clock.Advance(10 * time.Minute)
	if _, err := f.store.CompleteLink(ctx, f.discord, identity.ProviderDiscord, "333", second); !errors.Is(err, identity.ErrBadCode) {
		t.Errorf("an expired code = %v, want ErrBadCode", err)
	}

	// An account that is someone else's cannot be claimed, and the attempt
	// uses the code up.
	third, _, _ := f.store.StartLink(ctx, grace, f.discord)
	if _, err := f.store.CompleteLink(ctx, f.discord, identity.ProviderDiscord, "111", third); !errors.Is(err, identity.ErrTaken) {
		t.Errorf("claiming Ada's account = %v, want ErrTaken", err)
	}
	if _, err := f.store.CompleteLink(ctx, f.discord, identity.ProviderDiscord, "333", third); !errors.Is(err, identity.ErrBadCode) {
		t.Errorf("the code of a refused link was still good: %v", err)
	}
	if u, _, _ := f.store.UserFor(ctx, f.pool, f.discord, "111"); u.ID != ada {
		t.Errorf("Ada's account now belongs to %q", u.Email)
	}
}

// directory is a chat service that can be asked for accounts by email
// address.
type directory struct {
	mu       sync.Mutex
	accounts map[string]string
	asked    []string
	// fail makes the lookup of an address fail.
	fail map[string]error
}

func (d *directory) LookupByEmail(_ context.Context, email string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, email)
	if err := d.fail[email]; err != nil {
		return "", false, err
	}
	id, ok := d.accounts[email]
	return id, ok, nil
}

// take returns the addresses asked for since the last call.
func (d *directory) take() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := strings.Join(d.asked, " ")
	d.asked = nil
	return out
}

func (d *directory) set(email, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.accounts[email] = id
}

func (f *fixture) linker(d *directory) *identity.Linker {
	l := identity.NewLinker(f.store, slog.New(slog.DiscardHandler))
	l.SetIntegrations([]identity.Integration{
		{ID: f.slack, Name: "slack", Provider: identity.ProviderSlack, Lookup: d},
		// Nobody can be looked up in Discord.
		{ID: f.discord, Name: "discord", Provider: identity.ProviderDiscord},
	})
	return l
}

func (f *fixture) pass(l *identity.Linker) int {
	f.t.Helper()
	n, err := l.LinkMissing(context.Background())
	if err != nil {
		f.t.Fatalf("LinkMissing: %v", err)
	}
	return n
}

// without reads the metric of users without an identity, as
// "integration=count" in the order of the integrations' names.
func without(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, family := range families {
		if family.GetName() != "beacon_users_without_identity" {
			continue
		}
		for _, m := range family.GetMetric() {
			out = append(out, fmt.Sprintf("%s=%v", m.GetLabel()[0].GetValue(), m.GetGauge().GetValue()))
		}
	}
	return strings.Join(out, " ")
}

// TestLinkerFindsAccountsByEmail runs the linker over users of whom the
// chat service knows some. It asks once for each, remembers whom the
// service did not know, and asks for them again an hour later, or at once
// when their address changes.
func TestLinkerFindsAccountsByEmail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada, grace := f.user("ada@example.com"), f.user("grace@example.com")
	f.exec(`UPDATE users SET disabled_at = now() WHERE id = $1`, f.user("gone@example.com"))
	// The bootstrap admin is an account without a person behind an address.
	f.exec(`INSERT INTO users (email, role) VALUES ('bootstrap-admin@beacon.invalid', 'admin')`)

	d := &directory{accounts: map[string]string{"ada@example.com": "U0ADA", "gone@example.com": "U0GONE", "bootstrap-admin@beacon.invalid": "U0BOOT"}}
	l := f.linker(d)
	reg := prometheus.NewRegistry()
	l.RegisterMetrics(reg)

	if n := f.pass(l); n != 1 {
		t.Errorf("the first pass linked %d, want 1", n)
	}
	if asked := d.take(); asked != "ada@example.com grace@example.com" {
		t.Errorf("the first pass asked for %q", asked)
	}
	if u, found, _ := f.store.UserFor(ctx, f.pool, f.slack, "U0ADA"); !found || u.ID != ada {
		t.Errorf("Ada's account belongs to %+v, %v", u, found)
	}
	list, _ := f.store.Identities(ctx, ada)
	if len(list) != 1 || list[0].Origin != identity.OriginEmail || list[0].Provider != identity.ProviderSlack {
		t.Errorf("Identities = %+v", list)
	}
	if got := without(t, reg); got != "discord=2 slack=1" {
		t.Errorf("beacon_users_without_identity: %s", got)
	}

	// Nobody is asked about again for an hour.
	for range 3 {
		f.clock.Advance(19 * time.Minute)
		if n := f.pass(l); n != 0 {
			t.Errorf("a pass linked %d", n)
		}
	}
	if asked := d.take(); asked != "" {
		t.Errorf("within the hour the service was asked for %q", asked)
	}
	// Grace joins the workspace. After the hour she is found.
	d.set("grace@example.com", "U0GRACE")
	f.clock.Advance(4 * time.Minute)
	if n := f.pass(l); n != 1 || d.take() != "grace@example.com" {
		t.Errorf("after an hour the pass linked %d", n)
	}
	if address, found, _ := f.store.Address(ctx, f.pool, f.slack, grace); address != "U0GRACE" || !found {
		t.Errorf("Grace's address = %q, %v", address, found)
	}
	if got := without(t, reg); got != "discord=2 slack=0" {
		t.Errorf("beacon_users_without_identity: %s", got)
	}

	// A new user is asked for at the next pass. So is one whose address
	// changed since the service did not know them.
	linus := f.user("linus@example.com")
	f.pass(l)
	if asked := d.take(); asked != "linus@example.com" {
		t.Errorf("after a new user the service was asked for %q", asked)
	}
	f.exec(`UPDATE users SET email = 'linus@example.org' WHERE id = $1`, linus)
	d.set("linus@example.org", "U0LINUS")
	if n := f.pass(l); n != 1 || d.take() != "linus@example.org" {
		t.Errorf("after the address changed the pass linked %d", n)
	}
	// Someone who is linked is never asked for again.
	f.clock.Advance(48 * time.Hour)
	f.pass(l)
	if asked := d.take(); asked != "" {
		t.Errorf("with everyone linked the service was asked for %q", asked)
	}
}

// TestLinkerDoesNotStealAnAccount finds, by email address, an account that
// another user has linked already.
func TestLinkerDoesNotStealAnAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ada, grace := f.user("ada@example.com"), f.user("grace@example.com")
	if err := f.store.Link(ctx, f.pool, ada, f.slack, identity.ProviderSlack, "U0SHARED", identity.OriginSelf); err != nil {
		t.Fatal(err)
	}
	d := &directory{accounts: map[string]string{"grace@example.com": "U0SHARED"}}
	l := f.linker(d)
	if n := f.pass(l); n != 0 {
		t.Errorf("the pass linked %d", n)
	}
	if u, _, _ := f.store.UserFor(ctx, f.pool, f.slack, "U0SHARED"); u.ID != ada {
		t.Errorf("the account now belongs to %s", u.Email)
	}
	if _, found, _ := f.store.Address(ctx, f.pool, f.slack, grace); found {
		t.Error("Grace was given Ada's account")
	}
	f.pass(l)
	if asked := d.take(); asked != "grace@example.com" {
		t.Errorf("the service was asked for %q, want Grace once", asked)
	}
}

// TestLinkerBacksOffWhenTheServiceFails stops a pass at the first failure
// and leaves everyone it had not reached to be asked for in a few minutes.
func TestLinkerBacksOffWhenTheServiceFails(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		f.user(name + "@example.com")
		f.clock.Advance(time.Second)
	}
	// The users were created a second apart in the database's time, not the
	// test's; order them by address.
	f.exec(`UPDATE users SET created_at = '2026-01-01'::timestamptz + (ascii(email) || ' seconds')::interval`)
	d := &directory{
		accounts: map[string]string{"a@example.com": "U0A", "b@example.com": "U0B", "c@example.com": "U0C", "d@example.com": "U0D"},
		fail:     map[string]error{"b@example.com": errors.New("slack users.lookupByEmail: rate limited")},
	}
	l := f.linker(d)
	n, err := l.LinkMissing(context.Background())
	if n != 1 || err == nil || !strings.Contains(err.Error(), `integration "slack": look up b@example.com: slack users.lookupByEmail: rate limited`) {
		t.Fatalf("LinkMissing = %d, %v", n, err)
	}
	if asked := d.take(); asked != "a@example.com b@example.com" {
		t.Errorf("the failed pass asked for %q", asked)
	}
	// Not at once.
	f.clock.Advance(time.Minute)
	if n, _ := l.LinkMissing(context.Background()); n != 0 || d.take() != "" {
		t.Errorf("a minute later the pass linked %d", n)
	}
	// The service recovers.
	d.fail = nil
	f.clock.Advance(time.Minute)
	if n := f.pass(l); n != 3 {
		t.Errorf("after the service recovered the pass linked %d, want 3", n)
	}
	if asked := d.take(); asked != "b@example.com c@example.com d@example.com" {
		t.Errorf("the pass asked for %q", asked)
	}
}

// TestLinkersShareTheWork runs several linkers at once, as replicas do, and
// several passes each. The service is asked once about each user.
func TestLinkersShareTheWork(t *testing.T) {
	f := newFixture(t)
	d := &directory{accounts: map[string]string{}}
	for i := range 30 {
		email := fmt.Sprintf("user%02d@example.com", i)
		f.user(email)
		// Half of them are in the workspace.
		if i%2 == 0 {
			d.accounts[email] = fmt.Sprintf("U%02d", i)
		}
	}
	var wg sync.WaitGroup
	for range 5 {
		l := f.linker(d)
		wg.Go(func() {
			for range 4 {
				if _, err := l.LinkMissing(context.Background()); err != nil {
					t.Errorf("LinkMissing: %v", err)
				}
			}
		})
	}
	wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := map[string]int{}
	for _, email := range d.asked {
		seen[email]++
	}
	if len(seen) != 30 || len(d.asked) != 30 {
		t.Errorf("the service was asked %d times about %d users, want 30 and 30", len(d.asked), len(seen))
	}
	var linked int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_identities`).Scan(&linked); err != nil || linked != 15 {
		t.Errorf("%d identities, %v; want 15", linked, err)
	}
}

func TestIdentitiesAPI(t *testing.T) {
	f := newFixture(t)
	ada := f.user("ada@example.com")
	if err := f.store.Link(context.Background(), f.pool, ada, f.slack, identity.ProviderSlack, "U0ADA", identity.OriginEmail); err != nil {
		t.Fatal(err)
	}
	routes := identity.NewHandler(slog.New(slog.DiscardHandler), f.store).Routes()
	if len(routes) != 1 || routes[0].Pattern != "GET /api/v1/me/identities" || routes[0].Level != access.Member {
		t.Fatalf("routes = %+v", routes)
	}
	get := func(p access.Principal) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, identity.Path, nil)
		routes[0].Handler.ServeHTTP(rec, req.WithContext(access.WithPrincipal(req.Context(), p)))
		if rec.Code != http.StatusOK {
			t.Errorf("GET = %d %s", rec.Code, rec.Body)
		}
		return strings.TrimSpace(rec.Body.String())
	}
	if got, want := get(access.Principal{UserID: ada}), `{"identities":[{"integration":"slack","provider":"slack","external_id":"U0ADA","origin":"email","linked_at":"2026-03-01T12:00:00Z"}]}`; got != want {
		t.Errorf("Ada's identities = %s", got)
	}
	// Someone with none, and a token, which is not a person.
	for _, p := range []access.Principal{{UserID: f.user("grace@example.com")}, {TokenID: "00000000-0000-4000-8000-000000000000"}} {
		if got := get(p); got != `{"identities":[]}` {
			t.Errorf("identities of %+v = %s", p, got)
		}
	}
}
