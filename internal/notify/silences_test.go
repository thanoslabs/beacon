package notify_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/silence"
)

// silenceFixture is a group fixture whose pipeline listens to silences.
type silenceFixture struct {
	*groupFixture
	silences *silence.Store
	author   access.Principal
}

func newSilenceFixture(t *testing.T) *silenceFixture {
	t.Helper()
	f := &silenceFixture{groupFixture: newGroupFixture(t)}
	f.silences = silence.NewStore(f.pool)
	f.silences.SetClock(f.clock.Now)
	f.silences.OnExpire(f.alerts.WakeGroupsWaitingFor)
	f.p.SetSilences(f.silences)
	f.author = access.Principal{Email: "ada@example.com", Role: access.RoleMember}
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO users (email) VALUES ($1) RETURNING id::text`, f.author.Email).Scan(&f.author.UserID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return f
}

// silence creates a silence for a host, from now, for the duration.
func (f *silenceFixture) silence(host string, d time.Duration) silence.Silence {
	f.t.Helper()
	s, err := f.silences.Create(context.Background(), silence.New{
		Matchers: []silence.Matcher{{Label: "host", Operator: silence.Equal, Value: host}},
		EndsAt:   f.clock.Now().Add(d), Comment: "maintenance on " + host,
	}, f.author)
	if err != nil {
		f.t.Fatalf("create silence: %v", err)
	}
	return s
}

// groupEvents lists the events about the group itself, as "type" or
// "type until=...".
func (f *silenceFixture) groupEvents(groupID string) string {
	f.t.Helper()
	events, err := f.alerts.GroupTimeline(context.Background(), groupID)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.AlertID == "" {
			out = append(out, string(e.Type))
		}
	}
	return strings.Join(out, ",")
}

// TestSilencedAlertIsRecordedAndNotifiesNobody is the heart of it: an alert
// that arrives under a silence is stored like any other and tells nobody,
// until the silence ends.
func TestSilencedAlertIsRecordedAndNotifiesNobody(t *testing.T) {
	f := newSilenceFixture(t)
	ctx := context.Background()
	s := f.silence("db1", 2*time.Hour)

	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	if n := f.tick(); n != 1 {
		t.Fatalf("%d groups were looked at, want 1", n)
	}
	f.expect("under the silence", a.GroupID, "")
	// The alert is there, firing, with its timeline.
	got, err := f.alerts.Get(ctx, a.ID)
	if err != nil || got.Status != alert.StatusFiring {
		t.Fatalf("the silenced alert = %+v, %v; want it stored and firing", got, err)
	}
	g, err := f.alerts.Group(ctx, a.GroupID)
	if err != nil || g.NotifiedAt != nil || g.NotifyAt == nil || !g.NotifyAt.Equal(s.EndsAt) || g.SilenceID != s.ID {
		t.Fatalf("group = %+v, %v; want it not notified and due when the silence ends", g, err)
	}
	if got := f.groupEvents(a.GroupID); got != "notification_silenced" {
		t.Errorf("group events = %q, want the timeline to say that a silence held the notification back", got)
	}

	// Repeats, and an hour, change nothing; the timeline does not repeat
	// itself either.
	for range 3 {
		f.clock.Advance(20 * time.Minute)
		f.join("db1")
		f.tick()
	}
	f.expect("an hour into the silence", a.GroupID, "")
	if got := f.groupEvents(a.GroupID); got != "notification_silenced" {
		t.Errorf("group events = %q", got)
	}

	// The silence ends and the alert still fires: now people are told.
	f.clock.Advance(time.Hour)
	if n := f.tick(); n != 1 {
		t.Fatalf("%d groups were notified when the silence ended, want 1", n)
	}
	f.expect("the silence ended", a.GroupID, "escalation_step")
	if g, _ := f.alerts.Group(ctx, a.GroupID); g.NotifiedAt == nil || g.NotifyAt != nil || g.SilenceID != "" {
		t.Errorf("group after the silence = %+v", g)
	}
}

func TestSilencedAlertThatResolvesIsNeverTold(t *testing.T) {
	f := newSilenceFixture(t)
	f.silence("db1", 2*time.Hour)
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.clock.Advance(time.Hour)
	f.resolveFromSource("db1")
	f.clock.Advance(24 * time.Hour)
	if n := f.tick(); n != 0 {
		t.Errorf("%d groups were due after the silenced alert resolved", n)
	}
	f.expect("resolved under the silence", a.GroupID, "")
}

// TestExpiringASilenceTellsWhatItHeldBack: ending a silence early does not
// leave its alerts quiet until the end it used to have.
func TestExpiringASilenceTellsWhatItHeldBack(t *testing.T) {
	f := newSilenceFixture(t)
	s := f.silence("db1", 8*time.Hour)
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.expect("under the silence", a.GroupID, "")

	f.clock.Advance(10 * time.Minute)
	if _, err := f.silences.Expire(context.Background(), s.ID); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n := f.tick(); n != 1 {
		t.Fatalf("%d groups were notified after the silence was expired, want 1", n)
	}
	f.expect("the silence was expired", a.GroupID, "escalation_step")
}

// TestSilenceCoversPartOfAGroup: the alerts of a group that are not silenced
// are told about as usual, and the silenced ones are left out of it.
func TestSilenceCoversPartOfAGroup(t *testing.T) {
	f := newSilenceFixture(t)
	var deliveries []string
	f.p.Register(f.integrationID, channelFunc(func(_ context.Context, m notify.Message) (string, error) {
		var hosts []string
		for _, a := range m.Alerts {
			hosts = append(hosts, a.Labels["host"]+"="+string(a.Status))
		}
		// Alerts that began at the same moment come in no particular order.
		slices.Sort(hosts)
		deliveries = append(deliveries, fmt.Sprintf("%s %s", m.Notification.Reason, strings.Join(hosts, " ")))
		return "msg", nil
	}))
	stop := f.run(f.p)
	delivered := func(n int) {
		t.Helper()
		f.waitFor(fmt.Sprintf("%d deliveries", n), func() bool {
			var sent int
			if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE status = 'sent'`).Scan(&sent); err != nil {
				t.Fatal(err)
			}
			return sent == n
		})
	}

	s := f.silence("db2", 2*time.Hour)
	a := f.join("db1")
	f.join("db2")
	f.clock.Advance(30 * time.Second)
	f.tick()
	delivered(1)

	// Another silenced alert joins, and the first silenced one resolves:
	// none of it is news.
	f.silence("db3", 3*time.Hour)
	f.clock.Advance(10 * time.Minute)
	f.join("db3")
	f.tick()
	f.clock.Advance(10 * time.Minute)
	f.resolveFromSource("db2")
	f.tick()
	f.expect("changes under silences", a.GroupID, "escalation_step")
	g, err := f.alerts.Group(context.Background(), a.GroupID)
	if err != nil || g.NotifyAt == nil || g.SilenceID == s.ID {
		t.Fatalf("group = %+v, %v; want it to wait for the silence of db3, the one that still holds an open alert", g, err)
	}

	// An alert that no silence covers joins: an update, without the
	// silenced ones.
	f.clock.Advance(10 * time.Minute)
	f.join("db4")
	f.tick()
	delivered(2)

	// db3's silence ends while it still fires: it is news now.
	f.clock.Advance(3 * time.Hour)
	f.tick()
	delivered(3)

	// Everything resolves. The group is over, and everyone who was told
	// hears it.
	for _, host := range []string{"db1", "db3", "db4"} {
		f.resolveFromSource(host)
	}
	f.clock.Advance(time.Hour)
	f.tick()
	delivered(4)
	stop()

	want := []string{
		"escalation_step db1=firing",
		"group_update db1=firing db4=firing",
		// db2 resolved under its silence, which is over by now, so the
		// message lists it; nobody was told about it on its own.
		"group_update db1=firing db2=resolved db3=firing db4=firing",
		"resolved db1=resolved db2=resolved db3=resolved db4=resolved",
	}
	if fmt.Sprint(deliveries) != fmt.Sprint(want) {
		t.Errorf("deliveries:\n%s\nwant:\n%s", strings.Join(deliveries, "\n"), strings.Join(want, "\n"))
	}
}

// TestAlertSilencedAfterPeopleWereTold: a silence that is created later does
// not take an alert out of the message people already have, and its
// resolution still reaches them.
func TestAlertSilencedAfterPeopleWereTold(t *testing.T) {
	f := newSilenceFixture(t)
	ctx := context.Background()
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.sent()
	f.expect("before the silence", a.GroupID, "escalation_step")

	f.silence("db1", 4*time.Hour)
	// The source repeats the alert; nothing is told.
	f.clock.Advance(time.Hour)
	f.join("db1")
	f.tick()
	f.expect("silenced after the fact", a.GroupID, "escalation_step")

	// Someone acknowledges it from the message they have: that is answered.
	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.ChatUser("slack", "U1", "bob"), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	f.expect("acknowledged under the silence", a.GroupID, "escalation_step,group_update")
	f.sent()

	// It resolves under the silence: the people who have the message see it
	// end.
	f.clock.Advance(time.Hour)
	f.resolveFromSource("db1")
	f.tick()
	f.expect("resolved under the silence", a.GroupID, "escalation_step,group_update,resolved")
}

// TestPendingAndExpiredSilencesDoNothing: only a silence in force counts.
func TestPendingAndExpiredSilencesDoNothing(t *testing.T) {
	f := newSilenceFixture(t)
	ctx := context.Background()
	over := f.silence("db1", time.Hour)
	if _, err := f.silences.Expire(ctx, over.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.silences.Create(ctx, silence.New{
		Matchers: []silence.Matcher{{Label: "host", Operator: silence.Equal, Value: "db1"}},
		StartsAt: f.clock.Now().Add(time.Hour), EndsAt: f.clock.Now().Add(2 * time.Hour), Comment: "tonight",
	}, f.author)
	if err != nil {
		t.Fatal(err)
	}
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.expect("with an expired and a pending silence", a.GroupID, "escalation_step")
}

// TestSilenceByPattern: matchers of every kind decide through the pipeline
// as they do alone.
func TestSilenceByPattern(t *testing.T) {
	f := newSilenceFixture(t)
	_, err := f.silences.Create(context.Background(), silence.New{
		Matchers: []silence.Matcher{
			{Label: "alertname", Operator: silence.Equal, Value: "DiskFull"},
			{Label: "host", Operator: silence.Matches, Value: "db[0-9]"},
			{Label: "host", Operator: silence.NotEqual, Value: "db7"},
		},
		EndsAt: f.clock.Now().Add(time.Hour), Comment: "the small databases",
	}, f.author)
	if err != nil {
		t.Fatal(err)
	}
	a := f.join("db1")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.expect("db1 matches every matcher", a.GroupID, "")
	// db10 is not db[0-9] as a whole, so it is not silenced: the group is
	// told about, with that alert.
	f.join("db10")
	f.clock.Advance(30 * time.Second)
	f.tick()
	f.expect("db10 does not match the anchored pattern", a.GroupID, "escalation_step")
}
