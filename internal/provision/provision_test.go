package provision_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
)

const secretValue = "provision-secret-marker-4b1e"

// example loads the example configuration that the config package tests
// with: two sources, four integrations, a team, a schedule, a policy and a
// routing rule.
func example(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Read("../config/testdata/example", config.LoadOptions{Getenv: func(string) string { return secretValue }})
	if err != nil {
		t.Fatalf("read the example configuration: %v", err)
	}
	return cfg
}

func reconcile(t *testing.T, pool *pgxpool.Pool, cfg config.Config) provision.Result {
	t.Helper()
	res, err := provision.Reconcile(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// changes writes what a reconciliation did, one "kind name action" per item.
func changes(res provision.Result) string {
	var out []string
	for _, c := range res.Changes {
		out = append(out, fmt.Sprintf("%s %s %s", c.Kind, c.Name, c.Action))
	}
	return strings.Join(out, ", ")
}

// stored is one row of a provisioned table.
type stored struct {
	id      string
	spec    string
	updated time.Time
	removed bool
}

func read(t *testing.T, pool *pgxpool.Pool, kind provision.Kind, name string) stored {
	t.Helper()
	var s stored
	err := pool.QueryRow(context.Background(), `SELECT id::text, spec::text, updated_at, removed_at IS NOT NULL FROM `+string(kind)+` WHERE name = $1 AND provisioned`, name).
		Scan(&s.id, &s.spec, &s.updated, &s.removed)
	if err != nil {
		t.Fatalf("read %s %q: %v", kind, name, err)
	}
	return s
}

func count(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestReconcile(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	cfg := example(t)

	// Create: every object gets a row.
	first := reconcile(t, pool, cfg)
	want := "sources prod-alertmanager created, sources grafana created, integrations slack created, integrations email created, " +
		"integrations discord created, integrations ticketing created, teams platform created, schedules platform-primary created, " +
		"escalation_policies platform-default created, routing_rules platform-critical created"
	if got := changes(first); got != want {
		t.Errorf("first reconciliation:\n%s\nwant:\n%s", got, want)
	}
	for _, kind := range provision.Kinds {
		if n := count(t, pool, `SELECT count(*) FROM `+string(kind)+` WHERE provisioned AND removed_at IS NULL`); n != len(first.IDs[kind]) || n == 0 {
			t.Errorf("%s: %d rows, %d identifiers", kind, n, len(first.IDs[kind]))
		}
	}
	slack := read(t, pool, provision.Integrations, "slack")
	if slack.id != first.IDs[provision.Integrations]["slack"] || !strings.Contains(slack.spec, `"bot_token": {"env": "BEACON_SLACK_BOT_TOKEN"}`) {
		t.Errorf("stored integration = %+v", slack)
	}
	// No secret value is stored anywhere.
	for _, kind := range provision.Kinds {
		if n := count(t, pool, `SELECT count(*) FROM `+string(kind)+` WHERE spec::text LIKE '%`+secretValue+`%' OR spec::text LIKE '%grafana-token-from-a-file%'`); n != 0 {
			t.Errorf("%s stores a secret value", kind)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM schedules s JOIN teams t ON t.id = s.team_id WHERE s.name = 'platform-primary' AND t.name = 'platform'`); n != 1 {
		t.Error("the schedule does not reference its team")
	}

	// Repeat: nothing changes, and no row is written.
	if got := changes(reconcile(t, pool, cfg)); got != "" {
		t.Errorf("second reconciliation changed: %s", got)
	}
	if again := read(t, pool, provision.Integrations, "slack"); !again.updated.Equal(slack.updated) {
		t.Error("an unchanged object was written again")
	}

	// Update: only the edited objects are written.
	cfg.Teams[0].DisplayName = "Platform Engineering"
	cfg.Routing.Rules = append([]config.RoutingRule{{Name: "first", Match: map[string]string{"team": "data"}, EscalationPolicy: "platform-default"}}, cfg.Routing.Rules...)
	if got, want := changes(reconcile(t, pool, cfg)), "teams platform updated, routing_rules first created, routing_rules platform-critical updated"; got != want {
		t.Errorf("after an edit: %s, want %s", got, want)
	}
	team := read(t, pool, provision.Teams, "platform")
	if !strings.Contains(team.spec, "Platform Engineering") || team.id != first.IDs[provision.Teams]["platform"] {
		t.Errorf("updated team = %+v", team)
	}
	// The rule that moved down keeps its row and takes its new position.
	if n := count(t, pool, `SELECT count(*) FROM routing_rules WHERE (name, position) IN (('first', 0), ('platform-critical', 1))`); n != 2 {
		t.Error("routing rule positions do not follow the file")
	}

	// Remove: the rows stay, marked.
	cfg.Sources = cfg.Sources[:1]
	cfg.Schedules = nil
	cfg.EscalationPolicies[0].Steps[0].Notify = cfg.EscalationPolicies[0].Steps[0].Notify[1:]
	if got, want := changes(reconcile(t, pool, cfg)), "sources grafana removed, schedules platform-primary removed, escalation_policies platform-default updated"; got != want {
		t.Errorf("after a removal: %s, want %s", got, want)
	}
	gone := read(t, pool, provision.Sources, "grafana")
	if !gone.removed || gone.id != first.IDs[provision.Sources]["grafana"] {
		t.Errorf("removed source = %+v", gone)
	}
	if got := changes(reconcile(t, pool, cfg)); got != "" {
		t.Errorf("reconciling again after a removal changed: %s", got)
	}

	// Return: the same rows are revived, so history stays attached.
	back := example(t)
	if got := changes(reconcile(t, pool, back)); !strings.Contains(got, "sources grafana revived") || !strings.Contains(got, "schedules platform-primary revived") || !strings.Contains(got, "routing_rules first removed") {
		t.Errorf("after the objects returned: %s", got)
	}
	if revived := read(t, pool, provision.Sources, "grafana"); revived.removed || revived.id != gone.id {
		t.Errorf("revived source = %+v, want the row %s again", revived, gone.id)
	}
	if n := count(t, pool, `SELECT count(*) FROM sources`); n != 2 {
		t.Errorf("%d source rows, want 2: a revived object must not get a second row", n)
	}
}

// TestRemovedObjectKeepsItsHistory removes a source that an alert refers to.
// The alert and its timeline must still resolve to the source.
func TestRemovedObjectKeepsItsHistory(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	cfg := example(t)
	sourceID := reconcile(t, pool, cfg).IDs[provision.Sources]["grafana"]

	a, _, err := alert.NewStore(pool).Fire(ctx, alert.Firing{SourceID: sourceID, Fingerprint: "f1", Severity: alert.SeverityWarning, Labels: map[string]string{"alertname": "A"}})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	cfg.Sources = cfg.Sources[:1]
	reconcile(t, pool, cfg)

	var name string
	var removed bool
	err = pool.QueryRow(ctx, `SELECT s.name, s.removed_at IS NOT NULL FROM alerts a JOIN sources s ON s.id = a.source_id WHERE a.id = $1`, a.ID).Scan(&name, &removed)
	if err != nil || name != "grafana" || !removed {
		t.Errorf("the alert's source = %q, removed %v, %v; want the removed source grafana", name, removed, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM alert_events`); n != 1 {
		t.Errorf("%d timeline events, want 1", n)
	}
}

// TestReconcileConcurrently runs many reconciliations of the same
// configuration at once, as replicas that start together do. Each object
// must be created exactly once.
func TestReconcileConcurrently(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	cfg := example(t)
	const workers = 12

	var wg sync.WaitGroup
	results := make([]provision.Result, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Go(func() {
			<-start
			results[i], errs[i] = provision.Reconcile(context.Background(), pool, cfg)
		})
	}
	close(start)
	wg.Wait()

	created := 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		created += len(results[i].Changes)
		for _, kind := range provision.Kinds {
			for name, id := range results[0].IDs[kind] {
				if results[i].IDs[kind][name] != id {
					t.Errorf("worker %d has another row for %s %q", i, kind, name)
				}
			}
		}
	}
	if created != 10 {
		t.Errorf("%d changes across all workers, want 10: one creation per object", created)
	}
	for _, kind := range provision.Kinds {
		if n, want := count(t, pool, `SELECT count(*) FROM `+string(kind)), len(results[0].IDs[kind]); n != want {
			t.Errorf("%s: %d rows, want %d", kind, n, want)
		}
	}
}

// TestReconcileIsAllOrNothing fails a reconciliation half way. Nothing of it
// may be left behind.
func TestReconcileIsAllOrNothing(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	cfg := example(t)
	reconcile(t, pool, cfg)

	// Validation would refuse this; here it stands for any failure after
	// earlier kinds were written.
	cfg.Sources[0].Labels = map[string]string{"changed": "yes"}
	cfg.Teams = append(cfg.Teams, config.Team{Name: "data"})
	cfg.Schedules[0].Team = "nobody"
	if _, err := provision.Reconcile(context.Background(), pool, cfg); err == nil || !strings.Contains(err.Error(), `team "nobody"`) {
		t.Fatalf("Reconcile = %v, want an error about the team", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM teams WHERE name = 'data'`); n != 0 {
		t.Error("a team from the failed reconciliation was stored")
	}
	if src := read(t, pool, provision.Sources, "prod-alertmanager"); strings.Contains(src.spec, "changed") {
		t.Error("a source from the failed reconciliation was stored")
	}
}

// TestReconcileLeavesOtherRowsAlone checks that only rows marked as
// provisioned are managed.
func TestReconcileLeavesOtherRowsAlone(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO teams (name, provisioned) VALUES ('by-hand', false)`); err != nil {
		t.Fatal(err)
	}
	if got := changes(reconcile(t, pool, config.Config{})); got != "" {
		t.Errorf("reconciling an empty configuration changed: %s", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM teams WHERE name = 'by-hand' AND removed_at IS NULL`); n != 1 {
		t.Error("a row that is not provisioned was removed")
	}
	_, err := provision.Reconcile(ctx, pool, config.Config{Teams: []config.Team{{Name: "by-hand"}}})
	if err == nil || !strings.Contains(err.Error(), "not managed by configuration") {
		t.Errorf("taking over a row that is not provisioned: %v", err)
	}
}

// members lists the memberships as "team/email/origin".
func members(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var out string
	err := pool.QueryRow(context.Background(), `
		SELECT coalesce(string_agg(t.name || '/' || u.email || '/' || m.origin, ' ' ORDER BY t.name, u.email, m.origin), '')
		FROM team_members m JOIN teams t ON t.id = m.team_id JOIN users u ON u.id = m.user_id`).Scan(&out)
	if err != nil {
		t.Fatalf("read team members: %v", err)
	}
	return out
}

func addUser(t *testing.T, pool *pgxpool.Pool, email string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO users (email) VALUES ($1)`, email); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func skipped(res provision.Result) string {
	var out []string
	for _, s := range res.Skipped {
		out = append(out, s.Team+"/"+s.Email)
	}
	return strings.Join(out, " ")
}

// TestReconcileTeamMembers follows a team's members list through the life of
// a configuration: unknown users are skipped without stopping anything, and
// join once they exist.
func TestReconcileTeamMembers(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	addUser(t, pool, "ada@example.com")
	cfg := config.Config{
		Sources: []config.Source{{Name: "prod", Type: config.SourceTypeWebhook}},
		Teams: []config.Team{
			// Addresses are compared as users store them: lowercase.
			{Name: "platform", Members: []string{"Ada@Example.com", "grace@example.com", "linus@example.com"}},
			{Name: "data", Members: []string{"grace@example.com"}},
			{Name: "empty"},
		},
	}

	// Unknown users are skipped and reported, and the rest is applied: the
	// known member, the other teams and the other kinds.
	res := reconcile(t, pool, cfg)
	if got := members(t, pool); got != "platform/ada@example.com/config" {
		t.Errorf("members = %q", got)
	}
	if got, want := skipped(res), "platform/grace@example.com platform/linus@example.com data/grace@example.com"; got != want {
		t.Errorf("skipped = %q, want %q", got, want)
	}
	if res.MembersAdded != 1 || res.MembersRemoved != 0 || len(res.IDs[provision.Sources]) != 1 || len(res.IDs[provision.Teams]) != 3 {
		t.Errorf("result = %+v", res)
	}

	// Repeating changes nothing.
	if res := reconcile(t, pool, cfg); res.MembersAdded != 0 || res.MembersRemoved != 0 || len(res.Skipped) != 3 {
		t.Errorf("second reconciliation = %+v", res)
	}

	// Once the user exists, the next reconciliation adds them everywhere
	// they are listed.
	addUser(t, pool, "grace@example.com")
	res = reconcile(t, pool, cfg)
	if got := members(t, pool); got != "data/grace@example.com/config platform/ada@example.com/config platform/grace@example.com/config" {
		t.Errorf("members = %q", got)
	}
	if res.MembersAdded != 2 || skipped(res) != "platform/linus@example.com" {
		t.Errorf("after the user appeared: added %d, skipped %q", res.MembersAdded, skipped(res))
	}
	if got, err := provision.SkippedMembers(context.Background(), pool, cfg.Teams); err != nil || len(got) != 1 || got[0].Email != "linus@example.com" {
		t.Errorf("SkippedMembers = %+v, %v", got, err)
	}

	// A membership that came from the identity provider belongs to sign-in,
	// and must survive whatever the members list says.
	if _, err := pool.Exec(context.Background(), `INSERT INTO team_members (team_id, user_id, origin)
		SELECT t.id, u.id, 'group' FROM teams t, users u WHERE t.name IN ('platform', 'empty') AND u.email = 'ada@example.com'`); err != nil {
		t.Fatal(err)
	}

	// Removed from one list, and a whole list emptied.
	cfg.Teams[0].Members = []string{"grace@example.com"}
	cfg.Teams[1].Members = nil
	res = reconcile(t, pool, cfg)
	if got := members(t, pool); got != "empty/ada@example.com/group platform/ada@example.com/group platform/grace@example.com/config" {
		t.Errorf("members after the lists changed = %q", got)
	}
	if res.MembersRemoved != 2 || len(res.Skipped) != 0 {
		t.Errorf("after the lists changed: removed %d, skipped %q", res.MembersRemoved, skipped(res))
	}

	// A team that leaves the files loses its configured members, and its
	// row stays.
	cfg.Teams = cfg.Teams[1:]
	res = reconcile(t, pool, cfg)
	if got := members(t, pool); got != "empty/ada@example.com/group platform/ada@example.com/group" {
		t.Errorf("members after the team was removed = %q", got)
	}
	if res.MembersRemoved != 1 || !read(t, pool, provision.Teams, "platform").removed {
		t.Errorf("after the team was removed: %+v", res)
	}
}

// TestReconcileTeamMembersConcurrently reconciles the same members from many
// replicas at once.
func TestReconcileTeamMembersConcurrently(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	addUser(t, pool, "ada@example.com")
	addUser(t, pool, "grace@example.com")
	cfg := config.Config{Teams: []config.Team{{Name: "platform", Members: []string{"ada@example.com", "grace@example.com", "nobody@example.com"}}}}

	var wg sync.WaitGroup
	results := make([]provision.Result, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = provision.Reconcile(context.Background(), pool, cfg) })
	}
	wg.Wait()
	added := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		added += results[i].MembersAdded
		if skipped(results[i]) != "platform/nobody@example.com" {
			t.Errorf("worker %d skipped %q", i, skipped(results[i]))
		}
	}
	if added != 2 || members(t, pool) != "platform/ada@example.com/config platform/grace@example.com/config" {
		t.Errorf("added %d in total, members %q", added, members(t, pool))
	}
}
