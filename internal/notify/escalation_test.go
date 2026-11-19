package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

// escalationFixture is a silence fixture whose policy has three steps and
// runs twice: #alerts, five minutes, #team, ten minutes, #managers, fifteen
// minutes, and all of that once more.
type escalationFixture struct {
	*silenceFixture
}

func newEscalationFixture(t *testing.T) *escalationFixture {
	t.Helper()
	f := &escalationFixture{silenceFixture: newSilenceFixture(t)}
	f.silences.OnExpire(f.p.WakeEscalationsWaitingFor)
	f.p.SetPolicies(f.threeSteps())
	return f
}

func (f *fixture) threeSteps() map[string]notify.Policy {
	return map[string]notify.Policy{f.policyID: {Name: "default", Repeat: 1, Steps: []notify.Step{
		f.step(5*time.Minute, "#alerts"), f.step(10*time.Minute, "#team"), f.step(15*time.Minute, "#managers"),
	}}}
}

// open fires an alert for a host into the DiskFull group and lets the
// group's wait pass, so that the first step has run. It returns the group.
func (f *escalationFixture) open(hosts ...string) string {
	f.t.Helper()
	var group string
	for _, host := range hosts {
		group = f.join(host).GroupID
	}
	f.clock.Advance(30 * time.Second)
	if n := f.tick(); n != 1 {
		f.t.Fatalf("%d groups notified, want 1", n)
	}
	return group
}

// escalate lets the engine run what is due and returns how many escalations
// it dealt with.
func (f *escalationFixture) escalate() int {
	f.t.Helper()
	n, err := f.p.EscalateDue(context.Background())
	if err != nil {
		f.t.Fatalf("EscalateDue: %v", err)
	}
	return n
}

// after moves the clock and lets the engine run.
func (f *escalationFixture) after(d time.Duration) int {
	f.t.Helper()
	f.clock.Advance(d)
	return f.escalate()
}

// pages lists the destinations of the group's escalation notifications, in
// the order they were queued.
func (f *fixture) pages(groupID string) string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT destination FROM notifications
		WHERE alert_group_id = $1 AND reason = 'escalation_step' ORDER BY created_at, destination`, groupID)
	if err != nil {
		f.t.Fatalf("read notifications: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		f.t.Fatalf("read notifications: %v", err)
	}
	return strings.Join(got, " ")
}

// state reads an escalation as "status step pass", with " due in <d>" while
// its timer runs.
func (f *escalationFixture) state(groupID string) string {
	f.t.Helper()
	var (
		status     string
		step, pass int
		next       *time.Time
	)
	err := f.pool.QueryRow(context.Background(), `SELECT status, step_index, pass, next_step_at FROM escalations WHERE alert_group_id = $1`, groupID).Scan(&status, &step, &pass, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return "none"
	}
	if err != nil {
		f.t.Fatalf("read escalation: %v", err)
	}
	out := fmt.Sprintf("%s step %d pass %d", status, step, pass)
	if next != nil {
		out += " due in " + next.Sub(f.clock.Now()).String()
	}
	return out
}

// steps lists the escalation events on a group's timeline, as
// "pass.step>destinations", and "exhausted" for the end.
func (f *escalationFixture) steps(groupID string) string {
	f.t.Helper()
	events, err := f.alerts.GroupTimeline(context.Background(), groupID)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		switch e.Type {
		case notify.EventEscalated:
			if e.Actor.Type != alert.ActorSystem || e.AlertID != "" {
				f.t.Errorf("escalated event by %s about alert %q, want the system and the group", e.Actor.Type, e.AlertID)
			}
			var to []string
			for _, n := range e.Data["notified"].([]any) {
				to = append(to, n.(map[string]any)["destination"].(string))
			}
			out = append(out, fmt.Sprintf("%v.%v>%s", e.Data["pass"], e.Data["step"], strings.Join(to, "+")))
		case notify.EventEscalationExhausted:
			out = append(out, "exhausted")
		}
	}
	return strings.Join(out, " ")
}

func (f *escalationFixture) check(what, groupID, pages, state string) {
	f.t.Helper()
	if got := f.pages(groupID); got != pages {
		f.t.Errorf("%s: pages = %q, want %q", what, got, pages)
	}
	if got := f.state(groupID); got != state {
		f.t.Errorf("%s: escalation = %q, want %q", what, got, state)
	}
}

// TestEscalationRunsEveryStepAndRepeats follows a group that nobody
// responds to through its whole policy: each step when the timeout of the
// one before has passed, not sooner; then all steps once more; then the end.
// Every step is on the timeline.
func TestEscalationRunsEveryStepAndRepeats(t *testing.T) {
	f := newEscalationFixture(t)
	g := f.open("db1")
	f.check("the first step runs with the first notification", g, "#alerts", "active step 0 pass 0 due in 5m0s")

	if n := f.after(4*time.Minute + 59*time.Second); n != 0 {
		t.Errorf("a step ran a second before its time")
	}
	f.check("before the timeout", g, "#alerts", "active step 0 pass 0 due in 1s")

	for _, step := range []struct {
		wait  time.Duration
		pages string
		state string
	}{
		{time.Second, "#alerts #team", "active step 1 pass 0 due in 10m0s"},
		{10 * time.Minute, "#alerts #team #managers", "active step 2 pass 0 due in 15m0s"},
		// The policy starts again.
		{15 * time.Minute, "#alerts #team #managers #alerts", "active step 0 pass 1 due in 5m0s"},
		{5 * time.Minute, "#alerts #team #managers #alerts #team", "active step 1 pass 1 due in 10m0s"},
		{10 * time.Minute, "#alerts #team #managers #alerts #team #managers", "active step 2 pass 1 due in 15m0s"},
		// And gives up.
		{15 * time.Minute, "#alerts #team #managers #alerts #team #managers", "exhausted step 2 pass 1"},
	} {
		if n := f.after(step.wait); n != 1 {
			t.Fatalf("%d escalations dealt with after %s, want 1", n, step.wait)
		}
		f.check("after "+step.wait.String(), g, step.pages, step.state)
		// Looking again changes nothing.
		if n := f.escalate(); n != 0 {
			t.Errorf("the same step ran twice")
		}
	}
	if n := f.after(24 * time.Hour); n != 0 {
		t.Errorf("an exhausted escalation ran again")
	}
	if got, want := f.steps(g), "1.1>#alerts 1.2>#team 1.3>#managers 2.1>#alerts 2.2>#team 2.3>#managers exhausted"; got != want {
		t.Errorf("timeline:\n got %s\nwant %s", got, want)
	}

	// What an event holds.
	events, _ := f.alerts.GroupTimeline(context.Background(), g)
	var second, last alert.Event
	for _, e := range events {
		if e.Type == notify.EventEscalated && e.Data["step"] == float64(2) && e.Data["pass"] == float64(1) {
			second = e
		}
		if e.Type == notify.EventEscalationExhausted {
			last = e
		}
	}
	data, _ := json.Marshal(second.Data)
	if want := `{"notified":[{"destination":"#team","integration":"chat","target":"channel #team"}],"pass":1,"passes":2,"policy":"default","skipped":[],"step":2,"steps":3,"timeout":"10m"}`; string(data) != want {
		t.Errorf("escalated event:\n got %s\nwant %s", data, want)
	}
	if !second.OccurredAt.Equal(time.Date(2026, 3, 1, 12, 5, 30, 0, time.UTC)) {
		t.Errorf("the second step is recorded at %s", second.OccurredAt)
	}
	if reason, _ := last.Data["reason"].(string); reason != `every step of escalation policy "default" ran, 2 times, and nobody responded` {
		t.Errorf("exhausted event: %v", last.Data)
	}
}

// TestRespondingStopsEscalation: acknowledging, snoozing and resolving each
// stop the steps, at the moment of the change, whoever makes it.
func TestRespondingStopsEscalation(t *testing.T) {
	ctx := context.Background()
	someone := alert.ChatUser("slack", "U0ADA", "ada")
	for name, respond := range map[string]func(f *escalationFixture, g string) string{
		"acknowledged": func(f *escalationFixture, g string) string {
			if _, err := f.alerts.ApplyToGroup(ctx, g, alert.ActionAcknowledge, someone, alert.Options{}); err != nil {
				t.Fatal(err)
			}
			return "acknowledged step 0 pass 0"
		},
		"snoozed": func(f *escalationFixture, g string) string {
			if _, err := f.alerts.ApplyToGroup(ctx, g, alert.ActionSnooze, someone, alert.Options{SnoozeUntil: f.clock.Now().Add(72 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			return "acknowledged step 0 pass 0"
		},
		"resolved by a person": func(f *escalationFixture, g string) string {
			if _, err := f.alerts.ApplyToGroup(ctx, g, alert.ActionResolve, someone, alert.Options{}); err != nil {
				t.Fatal(err)
			}
			return "resolved step 0 pass 0"
		},
		"resolved by the source": func(f *escalationFixture, _ string) string {
			f.resolveFromSource("db1")
			return "resolved step 0 pass 0"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEscalationFixture(t)
			g := f.open("db1")
			f.clock.Advance(2 * time.Minute)
			want := respond(f, g)
			// The timer is gone the moment the alert is taken, not when
			// the worker next looks.
			f.check("at once", g, "#alerts", want)
			for range 5 {
				if n := f.after(20 * time.Minute); n != 0 {
					t.Errorf("an escalation was dealt with after the response")
				}
			}
			f.check("a long time later", g, "#alerts", want)
			if got := f.steps(g); got != "1.1>#alerts" {
				t.Errorf("timeline: %s", got)
			}
		})
	}
}

// TestEscalationGoesOnWhileAnAlertIsFiring: a group of two alerts of which
// one is acknowledged still has one that nobody took.
func TestEscalationGoesOnWhileAnAlertIsFiring(t *testing.T) {
	f := newEscalationFixture(t)
	ctx := context.Background()
	g := f.open("db1", "db2")
	alerts, err := f.alerts.GroupAlerts(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	// The two alerts began at the same moment, so their order is not fixed.
	db1 := alerts[slices.IndexFunc(alerts, func(a alert.Alert) bool { return a.Labels["host"] == "db1" })]
	if _, err := f.alerts.Apply(ctx, db1.ID, alert.ActionAcknowledge, alert.System(), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.check("one of two acknowledged", g, "#alerts", "active step 0 pass 0 due in 5m0s")
	f.after(5 * time.Minute)
	f.check("the next step", g, "#alerts #team", "active step 1 pass 0 due in 10m0s")

	// The other one is resolved by its source: nothing is firing.
	f.resolveFromSource("db2")
	f.check("nothing firing", g, "#alerts #team", "acknowledged step 1 pass 0")
	if n := f.after(time.Hour); n != 0 {
		t.Error("a step ran with nothing firing")
	}
}

// TestEscalationStartsAgainWhenASnoozeEnds: an escalation that stopped
// because the alerts were snoozed starts from its first step when the
// snooze runs out. Being asked again is what a snooze is for.
func TestEscalationStartsAgainWhenASnoozeEnds(t *testing.T) {
	f := newEscalationFixture(t)
	ctx := context.Background()
	g := f.open("db1")
	f.after(5 * time.Minute)
	f.check("the second step ran", g, "#alerts #team", "active step 1 pass 0 due in 10m0s")

	if _, err := f.alerts.ApplyToGroup(ctx, g, alert.ActionSnooze, alert.ChatUser("slack", "U0ADA", "ada"), alert.Options{SnoozeUntil: f.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.check("snoozed", g, "#alerts #team", "acknowledged step 1 pass 0")
	if n := f.after(59 * time.Minute); n != 0 {
		t.Error("a step ran during the snooze")
	}

	// The snooze ends and nobody has done anything about the alert.
	f.clock.Advance(time.Minute)
	if n, err := f.alerts.ExpireSnoozes(ctx); n != 1 || err != nil {
		t.Fatalf("ExpireSnoozes = %d, %v", n, err)
	}
	f.check("the snooze ended", g, "#alerts #team", "active step -1 pass 0 due in 0s")
	f.escalate()
	f.check("from the first step again", g, "#alerts #team #alerts", "active step 0 pass 0 due in 5m0s")
	f.after(5 * time.Minute)
	f.check("and on", g, "#alerts #team #alerts #team", "active step 1 pass 0 due in 10m0s")

	if got, want := f.steps(g), "1.1>#alerts 1.2>#team 1.1>#alerts 1.2>#team"; got != want {
		t.Errorf("timeline:\n got %s\nwant %s", got, want)
	}
}

// TestAcknowledgedGroupIsNeverPagedAgain: one page per alert group. Alerts
// that join a group after it was acknowledged page nobody, however many
// join and however long they fire. The destinations that were told get the
// group's update, which edits their message, and nothing else.
func TestAcknowledgedGroupIsNeverPagedAgain(t *testing.T) {
	f := newEscalationFixture(t)
	ctx := context.Background()
	g := f.open("db1")
	f.after(5 * time.Minute)
	f.sent()
	if _, err := f.alerts.ApplyToGroup(ctx, g, alert.ActionAcknowledge, alert.ChatUser("slack", "U0ADA", "ada"), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.sent()
	f.check("acknowledged", g, "#alerts #team", "acknowledged step 1 pass 0")
	updates := func() string {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT destination FROM notifications WHERE alert_group_id = $1 AND reason = 'group_update' AND status = 'pending' ORDER BY destination`, g)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, " ")
	}

	for i, host := range []string{"db2", "db3", "db4"} {
		f.clock.Advance(7 * time.Minute)
		joined := f.join(host)
		if joined.GroupID != g || joined.Status != alert.StatusFiring {
			t.Fatalf("%s joined group %s as %s, want the acknowledged group, firing", host, joined.GroupID, joined.Status)
		}
		// Not at once, and not later.
		if n := f.escalate(); n != 0 {
			t.Errorf("alert %d that joined the acknowledged group ran an escalation step", i+1)
		}
		f.check("after "+host+" joined", g, "#alerts #team", "acknowledged step 1 pass 0")
		// The people who were told see the new alert in their message.
		f.tick()
		if got := updates(); got != "#alerts #team" {
			t.Errorf("after %s joined, updates are waiting for %q, want both destinations that were told", host, got)
		}
		f.sent()
	}
	for range 6 {
		if n := f.after(4 * time.Hour); n != 0 {
			t.Error("an escalation step ran for the acknowledged group")
		}
	}
	f.check("a day later, with three alerts firing", g, "#alerts #team", "acknowledged step 1 pass 0")
	if got, want := f.steps(g), "1.1>#alerts 1.2>#team"; got != want {
		t.Errorf("timeline: %s, want %s", got, want)
	}
	var pages int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE alert_group_id = $1 AND reason = 'escalation_step'`, g).Scan(&pages); err != nil || pages != 2 {
		t.Errorf("%d pages for the group, %v; want the 2 from before it was acknowledged", pages, err)
	}

	// The same holds for a group whose policy ran out.
	other := f.fireWith("Other", f.policyID)
	f.tick()
	for range 7 {
		f.after(15 * time.Minute)
	}
	if got := f.state(other.GroupID); !strings.HasPrefix(got, "exhausted") {
		t.Fatalf("the other group's escalation is %s, want exhausted", got)
	}
	before := f.pages(other.GroupID)
	if _, _, err := f.alerts.Fire(ctx, alert.Firing{SourceID: f.sourceID, Fingerprint: "other-2", Severity: alert.SeverityWarning,
		Labels: map[string]string{"alertname": "Other"}, Group: alert.Grouping{Key: "Other", EscalationPolicyID: f.policyID}}); err != nil {
		t.Fatal(err)
	}
	if n := f.after(time.Hour); n != 0 || f.pages(other.GroupID) != before {
		t.Error("an alert that joined a group whose policy had run out paged someone")
	}
}

// TestGroupTakenBeforeItsFirstNotification: someone acknowledges during the
// group's wait. The group is still told about, once, and nothing follows.
func TestGroupTakenBeforeItsFirstNotification(t *testing.T) {
	f := newEscalationFixture(t)
	g := f.join("db1").GroupID
	if _, err := f.alerts.ApplyToGroup(context.Background(), g, alert.ActionAcknowledge, alert.System(), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.check("acknowledged before anyone was told", g, "#alerts", "acknowledged step 0 pass 0")
	if n := f.after(time.Hour); n != 0 {
		t.Error("a step ran for a group nobody needs to respond to")
	}
}

// replica returns another pipeline on the same database with the same
// clock and policy, as another process has.
func (f *escalationFixture) replica() *notify.Pipeline {
	store := alert.NewStore(f.pool)
	store.SetClock(f.clock.Now)
	p := notify.New(f.pool, store, slog.New(slog.DiscardHandler), fastConfig())
	p.SetClock(f.clock.Now)
	p.Register(f.integrationID, undelivered)
	p.SetPolicies(f.threeSteps())
	p.SetSilences(f.silences)
	return p
}

// TestEscalationTimersSurviveARestartAndFireOnce: the timer of a step is a
// column in Postgres. The process that ran the first step is gone when the
// second falls due; several new ones start at the same moment, and exactly
// one of them runs it. The same again for the third step.
func TestEscalationTimersSurviveARestartAndFireOnce(t *testing.T) {
	f := newEscalationFixture(t)
	var groups []string
	for i := range 12 {
		a, _, err := f.alerts.Fire(context.Background(), alert.Firing{
			SourceID: f.sourceID, Fingerprint: fmt.Sprint(i), Severity: alert.SeverityCritical, Labels: map[string]string{"alertname": fmt.Sprint("A", i)},
			Group: alert.Grouping{Key: fmt.Sprint(i), EscalationPolicyID: f.policyID},
		})
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, a.GroupID)
	}
	if n := f.tick(); n != 12 {
		t.Fatalf("%d groups notified, want 12", n)
	}

	for round, want := range []string{"#alerts #team", "#alerts #team #managers"} {
		// Nothing runs for a while, and longer than the timeout.
		f.clock.Advance(time.Duration(round+1) * 17 * time.Minute)
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			total int
		)
		for range 6 {
			p := f.replica()
			wg.Go(func() {
				n, err := p.EscalateDue(context.Background())
				if err != nil {
					t.Errorf("EscalateDue: %v", err)
				}
				mu.Lock()
				total += n
				mu.Unlock()
			})
		}
		wg.Wait()
		// A replica may have found nothing left while another still held a
		// row; a last look picks up whatever was held at that moment.
		total += f.escalate()
		if total != 12 {
			t.Errorf("round %d: %d steps ran, want 12", round, total)
		}
		for _, g := range groups {
			if got := f.pages(g); got != want {
				t.Errorf("round %d: pages of a group = %q, want %q", round, got, want)
			}
		}
	}
}

// TestEscalationRespectsSilences: a silence keeps Beacon from asking more
// people about an alert. The step waits for the silence and runs when it
// ends, or at once when it is expired; an alert that is not silenced keeps
// its group escalating.
func TestEscalationRespectsSilences(t *testing.T) {
	f := newEscalationFixture(t)
	ctx := context.Background()
	g := f.open("db1")
	f.clock.Advance(time.Minute)
	s := f.silence("db1", 2*time.Hour)

	// The second step is due and does not run.
	if n := f.after(4 * time.Minute); n != 1 {
		t.Fatalf("%d escalations looked at, want 1", n)
	}
	f.check("silenced", g, "#alerts", "active step 0 pass 0 due in 1h56m0s")
	if got := f.groupEvents(g); got != "escalated,notification_silenced" {
		t.Errorf("events = %s", got)
	}
	if n := f.after(time.Hour); n != 0 {
		t.Error("a step ran under the silence")
	}

	// The silence is expired early: the step runs at the next look.
	if _, err := f.silences.Expire(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.escalate(); n != 1 {
		t.Fatalf("%d steps ran after the silence was expired, want 1", n)
	}
	f.check("after the silence was expired", g, "#alerts #team", "active step 1 pass 0 due in 10m0s")

	// Another silence, left to run out.
	f.silence("db1", 30*time.Minute)
	f.after(10 * time.Minute)
	f.check("silenced again", g, "#alerts #team", "active step 1 pass 0 due in 20m0s")
	if n := f.after(19 * time.Minute); n != 0 {
		t.Error("a step ran under the second silence")
	}
	f.after(time.Minute)
	f.check("the silence ran out", g, "#alerts #team #managers", "active step 2 pass 0 due in 15m0s")
	if got := f.groupEvents(g); got != "escalated,notification_silenced,escalated,notification_silenced,escalated" {
		t.Errorf("events = %s", got)
	}

	// A second alert joins that no silence covers. With one alert silenced
	// and one not, the group goes on escalating.
	f.silence("db1", 10*time.Hour)
	f.join("db2")
	f.after(15 * time.Minute)
	f.check("one of two alerts silenced", g, "#alerts #team #managers #alerts", "active step 0 pass 1 due in 5m0s")
}

// directory is the people the fixture's steps can name.
type directory struct {
	onCall  map[string][]notify.Person
	members map[string][]notify.Person
	users   map[string]notify.Person
	// addresses maps "integration/user" to where the integration reaches
	// the user.
	addresses map[string]string
	// fail makes the next lookups of addresses fail.
	fail int
}

func (d *directory) OnCall(_ context.Context, _ pgx.Tx, scheduleID string) ([]notify.Person, error) {
	return d.onCall[scheduleID], nil
}

func (d *directory) TeamMembers(_ context.Context, _ pgx.Tx, teamID string) ([]notify.Person, error) {
	return d.members[teamID], nil
}

func (d *directory) User(_ context.Context, _ pgx.Tx, email string) (notify.Person, bool, error) {
	u, ok := d.users[email]
	return u, ok, nil
}

func (d *directory) Address(_ context.Context, _ pgx.Tx, integrationID, userID string) (string, bool, error) {
	if d.fail > 0 {
		d.fail--
		return "", false, errors.New("the database is away")
	}
	address, ok := d.addresses[integrationID+"/"+userID]
	return address, ok, nil
}

// TestPeopleTargets runs a step whose targets are people: whoever is on
// call for a schedule, the members of a team, and a user. Each is notified
// where the integration reaches them. Someone who cannot be reached is
// skipped, logged and named on the timeline, and does not hold up the rest.
func TestPeopleTargets(t *testing.T) {
	f := newEscalationFixture(t)
	ctx := context.Background()
	logs := &bytes.Buffer{}
	p := notify.New(f.pool, f.alerts, slog.New(slog.NewJSONHandler(logs, nil)), fastConfig())
	p.SetClock(f.clock.Now)
	p.Register(f.integrationID, undelivered)
	email := provisiontest.Row(t, f.pool, provision.Integrations, "email")

	user := func(address string) notify.Person {
		var id string
		if err := f.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id::text`, address).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return notify.Person{UserID: id, Email: address}
	}
	grace, linus, ken := user("grace@example.com"), user("linus@example.com"), user("ken@example.com")
	chat := []notify.Via{{IntegrationID: f.integrationID, Name: "chat"}}
	p.SetPeople(&directory{
		onCall:  map[string][]notify.Person{"schedule-primary": {grace}},
		members: map[string][]notify.Person{"team-platform": {grace, linus, ken}},
		users:   map[string]notify.Person{"ken@example.com": ken},
		// Linus has no account in the chat service.
		addresses: map[string]string{f.integrationID + "/" + grace.UserID: "U0GRACE", f.integrationID + "/" + ken.UserID: "U0KEN"},
	})
	p.SetPolicies(map[string]notify.Policy{f.policyID: {Name: "people", Steps: []notify.Step{{Timeout: time.Hour, Targets: []notify.StepTarget{
		{Kind: notify.TargetSchedule, Name: "primary", ID: "schedule-primary", Via: chat},
		// Grace is on call and in the team: she is told once.
		{Kind: notify.TargetTeam, Name: "platform", ID: "team-platform", Via: chat},
		{Kind: notify.TargetSchedule, Name: "empty", ID: "schedule-empty", Via: chat},
		{Kind: notify.TargetTeam, Name: "nobody", ID: "team-nobody", Via: chat},
		{Kind: notify.TargetUser, Name: "stranger@example.com", Via: chat},
		// Through an integration that does not deliver, as well as one that does.
		{Kind: notify.TargetUser, Name: "ken@example.com", Via: []notify.Via{{IntegrationID: email, Name: "email"}, chat[0]}},
		{Kind: notify.TargetUser, Name: "ken@example.com"},
		notify.ChannelTarget(f.integrationID, "chat", "#alerts"),
		notify.ChannelTarget(email, "email", "oncall@example.com"),
	}}}}})

	g := f.join("db1").GroupID
	f.clock.Advance(30 * time.Second)
	if n, err := p.NotifyDueGroups(ctx); n != 1 || err != nil {
		t.Fatalf("NotifyDueGroups = %d, %v", n, err)
	}

	rows, err := f.pool.Query(ctx, `SELECT n.destination || COALESCE(' for ' || u.email, '') FROM notifications n LEFT JOIN users u ON u.id = n.user_id
		WHERE n.alert_group_id = $1 ORDER BY n.destination`, g)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if got, want := strings.Join(queued, ", "), "#alerts, U0GRACE for grace@example.com, U0KEN for ken@example.com"; err != nil || got != want {
		t.Errorf("notifications:\n got %s\nwant %s (%v)", got, want, err)
	}

	events, _ := f.alerts.GroupTimeline(ctx, g)
	var data map[string]any
	for _, e := range events {
		if e.Type == notify.EventEscalated {
			data = e.Data
		}
	}
	list := func(key string) string {
		var out []string
		for _, item := range data[key].([]any) {
			m := item.(map[string]any)
			parts := []string{m["target"].(string)}
			for _, k := range []string{"user", "integration", "destination", "reason"} {
				if v, ok := m[k].(string); ok && v != "" {
					parts = append(parts, v)
				}
			}
			out = append(out, strings.Join(parts, " | "))
		}
		return strings.Join(out, "\n")
	}
	if got, want := list("notified"), strings.Join([]string{
		"schedule primary | grace@example.com | chat | U0GRACE",
		"team platform | ken@example.com | chat | U0KEN",
		"channel #alerts | chat | #alerts",
	}, "\n"); got != want {
		t.Errorf("notified:\n%s\nwant:\n%s", got, want)
	}
	if got, want := list("skipped"), strings.Join([]string{
		"team platform | linus@example.com | chat | the person has no linked account for this integration",
		"schedule empty | the schedule has nobody on call: a gap in coverage",
		"team nobody | the team has no members who are users",
		"user stranger@example.com | no user has this email address",
		"user ken@example.com | ken@example.com | email | the integration does not deliver notifications",
		"user ken@example.com | no integration in the configuration can reach a person directly",
		"channel oncall@example.com | email | the integration does not deliver notifications",
	}, "\n"); got != want {
		t.Errorf("skipped:\n%s\nwant:\n%s", got, want)
	}
	// A schedule with nobody on call is a warning, never an error.
	if want := `"level":"WARN","msg":"escalation target not notified","group_id":"` + g + `","policy":"people","step":1,"target":"schedule empty","user":"","integration":"","reason":"the schedule has nobody on call: a gap in coverage"`; !strings.Contains(logs.String(), want) {
		t.Errorf("the logs do not contain %s:\n%s", want, logs)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a step with people who cannot be reached logged an error:\n%s", logs)
	}
	// The gap is in the log, with who and why.
	if want := `"level":"WARN","msg":"escalation target not notified","group_id":"` + g + `","policy":"people","step":1,"target":"team platform","user":"linus@example.com","integration":"chat","reason":"the person has no linked account for this integration"`; !strings.Contains(logs.String(), want) {
		t.Errorf("the logs do not contain %s:\n%s", want, logs)
	}
}

// TestStepThatFailsIsTriedAgain: a step that cannot be run changes nothing
// and is tried again half a minute later, without holding up the others.
func TestStepThatFailsIsTriedAgain(t *testing.T) {
	f := newEscalationFixture(t)
	ctx := context.Background()
	var ada notify.Person
	ada.Email = "oncall@example.com"
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id::text`, ada.Email).Scan(&ada.UserID); err != nil {
		t.Fatal(err)
	}
	d := &directory{onCall: map[string][]notify.Person{"s": {ada}}, addresses: map[string]string{f.integrationID + "/" + ada.UserID: "U0ADA"}}
	f.p.SetPeople(d)
	f.p.SetPolicies(map[string]notify.Policy{f.policyID: {Name: "default", Steps: []notify.Step{
		f.step(5*time.Minute, "#alerts"),
		{Timeout: 5 * time.Minute, Targets: []notify.StepTarget{{Kind: notify.TargetSchedule, Name: "primary", ID: "s", Via: []notify.Via{{IntegrationID: f.integrationID, Name: "chat"}}}}},
	}}})
	g := f.open("db1")

	d.fail = 1
	f.clock.Advance(5 * time.Minute)
	if _, err := f.p.EscalateDue(ctx); err == nil || !strings.Contains(err.Error(), "the database is away") {
		t.Fatalf("EscalateDue = %v, want the failure", err)
	}
	f.check("after the failure", g, "#alerts", "active step 0 pass 0 due in 30s")
	if got := f.steps(g); got != "1.1>#alerts" {
		t.Errorf("a failed step is on the timeline: %s", got)
	}
	if n := f.after(29 * time.Second); n != 0 {
		t.Error("the failed step was tried again at once")
	}
	f.after(time.Second)
	f.check("tried again", g, "#alerts U0ADA", "active step 1 pass 0 due in 5m0s")
}

// TestPolicyChangesWhileEscalating: a reload changes the policy for the
// escalations in progress too.
func TestPolicyChangesWhileEscalating(t *testing.T) {
	f := newEscalationFixture(t)
	g := f.open("db1")
	f.after(5 * time.Minute)
	f.after(10 * time.Minute)
	f.check("the third step ran", g, "#alerts #team #managers", "active step 2 pass 0 due in 15m0s")

	// The policy now has one step and repeats three times.
	f.p.SetPolicies(map[string]notify.Policy{f.policyID: {Name: "default", Repeat: 3, Steps: []notify.Step{f.step(time.Minute, "#oncall")}}})
	f.after(15 * time.Minute)
	f.check("the new policy, next pass", g, "#alerts #team #managers #oncall", "active step 0 pass 1 due in 1m0s")
	f.after(time.Minute)
	f.after(time.Minute)
	f.check("its last pass", g, "#alerts #team #managers #oncall #oncall #oncall", "active step 0 pass 3 due in 1m0s")

	// The policy leaves the configuration.
	f.p.SetPolicies(nil)
	f.after(time.Minute)
	f.check("without the policy", g, "#alerts #team #managers #oncall #oncall #oncall", "exhausted step 0 pass 3")
	events, _ := f.alerts.GroupTimeline(context.Background(), g)
	if last := events[len(events)-1]; last.Type != notify.EventEscalationExhausted || last.Data["reason"] != "the escalation policy of the group is no longer in the configuration" {
		t.Errorf("last event = %s %v", last.Type, last.Data)
	}
}

// TestUpdatesReachEveryoneWhoWasPaged: the destinations of later steps are
// told about changes like the first one.
func TestUpdatesReachEveryoneWhoWasPaged(t *testing.T) {
	f := newEscalationFixture(t)
	g := f.open("db1")
	f.after(5 * time.Minute)
	f.sent()
	if _, err := f.alerts.ApplyToGroup(context.Background(), g, alert.ActionAcknowledge, alert.ChatUser("slack", "U0ADA", "ada"), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.pool.Query(context.Background(), `SELECT destination FROM notifications WHERE alert_group_id = $1 AND reason = 'group_update' ORDER BY destination`, g)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(updated, []string{"#alerts", "#team"}) {
		t.Errorf("updates went to %v, %v; want both destinations that were paged", updated, err)
	}
}
