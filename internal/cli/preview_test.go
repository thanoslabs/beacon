package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/schedule"
)

func TestPreviewSettingsErrors(t *testing.T) {
	path := writeConfig(t, "teams:\n  - name: platform\n")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no configuration", []string{"preview", "--database-url", "postgres://localhost/beacon"}, "the configuration path is required"},
		{"no database", []string{"preview", "--config", path}, "the database URL is required"},
		{"weeks", []string{"preview", "--config", path, "--database-url", "postgres://localhost/beacon", "--weeks", "0"}, "--weeks must be between 1 and 104"},
		{"format", []string{"preview", "--config", path, "--database-url", "postgres://localhost/beacon", "--format", "yaml"}, "--format must be text or json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLIEnv(t, nil, tc.args...)
			if code != exitError || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q; want %d and %q", code, stderr, exitError, tc.want)
			}
		})
	}
}

const previewRunning = `teams:
  - name: platform
schedules:
  - name: primary
    team: platform
    time_zone: Europe/London
    rotation:
      members: [ada@example.com, grace@example.com, linus@example.com]
      shift_length: 1d
      handoff: {time: "09:00"}
  - name: leaving
    team: platform
    time_zone: UTC
    rotation:
      members: [ada@example.com]
      shift_length: 1w
      handoff: {day: monday, time: "09:00"}
`

const previewProposed = `teams:
  - name: platform
schedules:
  - name: primary
    team: platform
    time_zone: Europe/London
    rotation:
      members: [ada@example.com, linus@example.com, typo@example.com]
      shift_length: 1d
      handoff: {time: "09:00"}
  - name: arriving
    team: platform
    time_zone: Australia/Sydney
    rotation:
      members: [grace@example.com]
      shift_length: 1w
      handoff: {day: friday, time: "17:00"}
`

// TestPreview runs the preview command against a database in which
// schedules are running, as a check in CI would against the database of a
// running Beacon. It reports what would move and writes nothing.
func TestPreview(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	ctx := context.Background()
	if code, _, stderr := runCLIEnv(t, map[string]string{"BEACON_DATABASE_URL": dbURL}, "migrate"); code != exitOK {
		t.Fatalf("migrate: %s", stderr)
	}
	pool, err := postgres.Open(ctx, postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO users (email) VALUES ('ada@example.com'), ('grace@example.com'), ('linus@example.com')`); err != nil {
		t.Fatal(err)
	}
	// The running configuration, applied as beacon serve applies it.
	running := writeConfig(t, previewRunning)
	cfg, err := config.Read(running, config.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := provision.Reconcile(ctx, pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var provisioned []schedule.Provisioned
	for _, s := range cfg.Schedules {
		provisioned = append(provisioned, schedule.Provisioned{ID: rows.IDs[provision.Schedules][s.Name], Schedule: s})
	}
	if _, err := schedule.NewStore(pool).Apply(ctx, provisioned); err != nil {
		t.Fatal(err)
	}
	dump := func() string {
		rows, err := pool.Query(ctx, `SELECT p::text FROM schedule_positions p ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, "\n")
	}
	before := dump()
	env := map[string]string{"BEACON_DATABASE_URL": dbURL}

	// The same configuration moves nothing.
	code, stdout, stderr := runCLIEnv(t, env, "preview", "--config", running, "--fail-on-moves")
	if code != exitOK || !strings.Contains(stdout, "schedule primary: unchanged.") || !strings.Contains(stdout, "schedule leaving: unchanged.") {
		t.Errorf("preview of the running configuration: exit %d\n%s%s", code, stdout, stderr)
	}

	// The proposed one takes Grace out of the rotation, in which she is
	// next, names someone who is not a user, adds a schedule and drops one.
	proposed := writeConfig(t, previewProposed)
	code, stdout, stderr = runCLIEnv(t, env, "preview", "--config", proposed, "--weeks", "1")
	if code != exitOK {
		t.Fatalf("preview: exit %d\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"schedule primary: changed. The new definition takes over at the next handoff, ",
		"  passed over: typo@example.com is not a user yet\n",
		"  the person on call would change in ",
		": linus@example.com instead of grace@example.com\n",
		"  shifts with this configuration:\n",
		"schedule arriving: new. Its rotation starts when the configuration is applied.\n",
		"schedule leaving: removed. Its shift in progress ends when the configuration is applied; recorded shifts are kept.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the preview does not contain %q:\n%s", want, stdout)
		}
	}
	// Times are shown in the zone of each schedule.
	if !strings.Contains(stdout, " 09:00 GMT") && !strings.Contains(stdout, " 09:00 BST") {
		t.Errorf("the preview does not show London time:\n%s", stdout)
	}

	// As a check in CI: the exit status says that shifts move.
	code, _, stderr = runCLIEnv(t, env, "preview", "--config", proposed, "--fail-on-moves")
	if code != exitError || !strings.Contains(stderr, "2 schedules would have upcoming shifts moved or ended") {
		t.Errorf("preview --fail-on-moves: exit %d, stderr %q", code, stderr)
	}

	code, stdout, stderr = runCLIEnv(t, env, "preview", "--config", proposed, "--format", "json", "--schedule", "primary")
	var out struct {
		Schedules []struct {
			Schedule    string
			Status      string
			TakesOverAt string `json:"takes_over_at"`
			Skipped     []string
			Moves       []struct{ Was, Becomes, Start, End string }
			Shifts      []struct{ User string }
		}
	}
	if err := json.Unmarshal([]byte(stdout), &out); code != exitOK || err != nil {
		t.Fatalf("preview --format json: exit %d, %v\n%s%s", code, err, stdout, stderr)
	}
	if len(out.Schedules) != 1 {
		t.Fatalf("%d schedules in the JSON, want primary alone:\n%s", len(out.Schedules), stdout)
	}
	p := out.Schedules[0]
	if p.Schedule != "primary" || p.Status != "changed" || p.TakesOverAt == "" || len(p.Skipped) != 1 || len(p.Moves) == 0 ||
		p.Moves[0].Was != "grace@example.com" || p.Moves[0].Becomes != "linus@example.com" || len(p.Shifts) == 0 || p.Shifts[0].User != "ada@example.com" {
		t.Errorf("JSON preview of primary: %+v", p)
	}

	if code, _, stderr := runCLIEnv(t, env, "preview", "--config", proposed, "--schedule", "nope"); code != exitError || !strings.Contains(stderr, `no schedule is named "nope"`) {
		t.Errorf("preview of an unknown schedule: exit %d, %q", code, stderr)
	}
	// An invalid configuration is reported as validate reports it.
	bad := writeConfig(t, strings.Replace(previewProposed, "Europe/London", "Europe/Londn", 1))
	if code, _, stderr := runCLIEnv(t, env, "preview", "--config", bad); code != exitError || !strings.Contains(stderr, `time_zone: "Europe/Londn" is not an IANA time zone`) {
		t.Errorf("preview of an invalid configuration: exit %d, %q", code, stderr)
	}

	if after := dump(); after != before {
		t.Errorf("the preview wrote to the positions:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
