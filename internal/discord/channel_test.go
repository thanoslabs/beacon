package discord_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/discord"
	"github.com/thanoslabs/beacon/internal/discord/discordtest"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

const botToken = "discord-test-bot-token-marker-41fa"

// ada is the Discord account of a person.
const ada = "300000000000000001"

func newClient(apiURL string) *discord.Client {
	return discord.NewClient(apiURL, config.NewSecret(config.SecretRef{}, botToken))
}

func newChannel(apiURL string) *discord.Channel { return discord.NewChannel(newClient(apiURL)) }

func message(status alert.Status, destination, previousRef string) notify.Message {
	return notify.Message{
		Notification: notify.Notification{Destination: destination},
		Alerts: []alert.Alert{{
			ID: "a1", GroupID: "3f0e9a0c-6f6e-4a53-9a5e-0d6a4b0c2a11", Status: status, Severity: alert.SeverityCritical,
			Labels:      map[string]string{"alertname": "DiskFull"},
			Annotations: map[string]string{"summary": "Disk is full"},
			StartedAt:   time.Now(),
		}},
		PreviousRef: previousRef,
	}
}

func TestSendPostsThenEditsInPlace(t *testing.T) {
	fake := discordtest.New(t, botToken)
	ch := newChannel(fake.URL)
	ctx := context.Background()

	ref, err := ch.Send(ctx, message(alert.StatusFiring, discordtest.Channel, ""))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := fake.Messages()
	if len(msgs) != 1 || ref != discordtest.Channel+":"+msgs[0].ID || msgs[0].Content != "🔴 **Firing** · critical\n**Disk is full**" {
		t.Fatalf("ref = %q, messages = %+v", ref, msgs)
	}

	ref2, err := ch.Send(ctx, message(alert.StatusAcknowledged, discordtest.Channel, ref))
	if err != nil || ref2 != ref {
		t.Fatalf("second Send = %q, %v; want the same reference", ref2, err)
	}
	if n := len(fake.Calls(discordtest.RouteCreateMessage)); n != 1 {
		t.Errorf("%d messages were posted, want 1", n)
	}
	edits := fake.Calls(discordtest.RouteEditMessage)
	if len(edits) != 1 || edits[0].Path != "/channels/"+discordtest.Channel+"/messages/"+msgs[0].ID {
		t.Fatalf("edits = %+v", edits)
	}
	// An edit sends every part, or Discord would keep what was there.
	for _, part := range []string{"content", "embeds", "components", "allowed_mentions"} {
		if _, sent := edits[0].Body[part]; !sent {
			t.Errorf("the edit does not send %s", part)
		}
	}
	if msgs = fake.Messages(); len(msgs) != 1 || msgs[0].Edits != 1 || !strings.HasPrefix(msgs[0].Content, "🟡 **Acknowledged**") {
		t.Errorf("messages = %+v", msgs)
	}
}

// TestSendToAPerson: a notification for one person goes to the direct
// message channel with them, which is opened first, and updates edit the
// message there.
func TestSendToAPerson(t *testing.T) {
	fake := discordtest.New(t, botToken)
	fake.AddUser(ada)
	ch := newChannel(fake.URL)
	ctx := context.Background()

	page := message(alert.StatusFiring, ada, "")
	page.Notification.UserID, page.Notification.Reason = "user-1", notify.ReasonEscalationStep
	ref, err := ch.Send(ctx, page)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	dm := fake.DM(ada)
	msgs := fake.Messages()
	if dm == "" || len(msgs) != 1 || msgs[0].ChannelID != dm || ref != dm+":"+msgs[0].ID {
		t.Fatalf("ref = %q, direct message channel = %q, messages = %+v", ref, dm, msgs)
	}
	if opened := fake.Calls(discordtest.RouteCreateDM); len(opened) != 1 || opened[0].Body["recipient_id"] != ada {
		t.Errorf("direct message channels opened: %+v", opened)
	}

	update := message(alert.StatusAcknowledged, ada, ref)
	update.Notification.UserID = "user-1"
	if ref2, err := ch.Send(ctx, update); err != nil || ref2 != ref {
		t.Fatalf("update = %q, %v", ref2, err)
	}
	if n := len(fake.Calls(discordtest.RouteCreateDM)); n != 1 {
		t.Errorf("the update opened the direct message channel again")
	}

	// Without the user, the same ID is a channel, and Discord has none.
	if _, err := ch.Send(ctx, message(alert.StatusFiring, ada, "")); err == nil || !strings.Contains(err.Error(), "error code 10003") {
		t.Errorf("Send to a user ID as a channel = %v, want unknown channel", err)
	}
}

func TestAPersonWhoRefusesDirectMessages(t *testing.T) {
	fake := discordtest.New(t, botToken)
	fake.AddUser(ada)
	fake.BlockDMs(ada)
	page := message(alert.StatusFiring, ada, "")
	page.Notification.UserID = "user-1"
	_, err := newChannel(fake.URL).Send(context.Background(), page)
	if err == nil || !strings.Contains(err.Error(), "error code 50007 (the person does not accept direct messages from the bot") {
		t.Fatalf("Send = %v", err)
	}
	if permanent, _ := notify.Classify(err); !permanent {
		t.Errorf("the failure would be retried")
	}
}

// TestEscalationStepSaysTheAlertAgain: an edit notifies nobody, so a step
// that reaches a destination again posts a new message and turns the old
// one into a pointer.
func TestEscalationStepSaysTheAlertAgain(t *testing.T) {
	fake := discordtest.New(t, botToken)
	ch := newChannel(fake.URL)
	ctx := context.Background()
	ref, err := ch.Send(ctx, message(alert.StatusFiring, discordtest.Channel, ""))
	if err != nil {
		t.Fatal(err)
	}
	again := message(alert.StatusFiring, discordtest.Channel, ref)
	again.Notification.Reason = notify.ReasonEscalationStep
	ref2, err := ch.Send(ctx, again)
	if err != nil || ref2 == ref {
		t.Fatalf("Send = %q, %v; want a new message", ref2, err)
	}
	msgs := fake.Messages()
	if len(msgs) != 2 || discordtest.Channel+":"+msgs[1].ID != ref2 {
		t.Fatalf("messages = %+v", msgs)
	}
	if old := msgs[0]; old.Edits != 1 || !strings.HasPrefix(old.Content, "⬇️ Beacon sent this alert again") || !strings.HasSuffix(old.Content, "**Disk is full**") ||
		len(old.Embeds) != 0 || len(old.Components) != 0 {
		t.Errorf("the old message = %+v", old)
	}
}

func TestEditOfADeletedMessagePostsAgain(t *testing.T) {
	fake := discordtest.New(t, botToken)
	ch := newChannel(fake.URL)
	ctx := context.Background()
	ref, err := ch.Send(ctx, message(alert.StatusFiring, discordtest.Channel, ""))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	fake.Delete(discordtest.Channel, strings.TrimPrefix(ref, discordtest.Channel+":"))

	ref2, err := ch.Send(ctx, message(alert.StatusResolved, discordtest.Channel, ref))
	if err != nil || ref2 == ref {
		t.Fatalf("Send after the message was deleted = %q, %v", ref2, err)
	}
	if msgs := fake.Messages(); len(msgs) != 1 || !strings.HasPrefix(msgs[0].Content, "🟢 **Resolved**") {
		t.Errorf("messages = %+v", msgs)
	}

	// Another failure of the edit is not papered over with a new message.
	fake.Fail(discordtest.RouteEditMessage, discordtest.Failure{Status: 500})
	if _, err := ch.Send(ctx, message(alert.StatusResolved, discordtest.Channel, ref2)); err == nil {
		t.Error("Send succeeded although the edit failed")
	}
	if n := len(fake.Calls(discordtest.RouteCreateMessage)); n != 2 {
		t.Errorf("%d messages posted, want 2", n)
	}
}

func TestErrorsNeverContainTheToken(t *testing.T) {
	fake := discordtest.New(t, "another-token")
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not discord</html>"))
	}))
	defer garbage.Close()

	tests := []struct {
		name, url, destination, want string
		fail                         *discordtest.Failure
	}{
		{name: "rejected token", url: fake.URL, want: "discord create message: HTTP status 401 (the bot token is not valid)"},
		{name: "connection refused", url: closed.URL, want: "connection refused"},
		{name: "not JSON", url: garbage.URL, want: "the response is not the expected JSON"},
		{name: "HTTP 503", url: fake.URL, fail: &discordtest.Failure{Status: 503}, want: "HTTP status 503"},
		{name: "rate limited", url: fake.URL, fail: &discordtest.Failure{Status: 429, RetryAfter: "7"}, want: "rate limited"},
		{name: "missing access", url: fake.URL, fail: &discordtest.Failure{Status: 403, Code: 50001}, want: "error code 50001 (the bot cannot see the channel"},
		{name: "bad URL", url: "http://bad host/", want: "discord create message"},
		{name: "a name, not an ID", url: fake.URL, destination: "#alerts", want: "use the numeric ID of the channel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fail != nil {
				fake.Fail(discordtest.RouteCreateMessage, *tt.fail)
			}
			destination := discordtest.Channel
			if tt.destination != "" {
				destination = tt.destination
			}
			_, err := newChannel(tt.url).Send(context.Background(), message(alert.StatusFiring, destination, ""))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Send = %v, want an error containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), botToken) || strings.Contains(err.Error(), "marker") {
				t.Errorf("the error contains the bot token: %v", err)
			}
		})
	}
}

func TestSendHonoursTheContext(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := newChannel(slow.URL).Send(ctx, message(alert.StatusFiring, discordtest.Channel, ""))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("Send = %v after %v, want the context deadline", err, time.Since(start))
	}
}

// TestRateLimitsAreRespected: Discord says in its answers when a route is
// used up, and asks not to be sent requests that it will refuse. Beacon
// holds such a request back and tells the pipeline how long to wait.
func TestRateLimitsAreRespected(t *testing.T) {
	fake := discordtest.New(t, botToken)
	fake.AddChannel("100000000000000002")
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	client := newClient(fake.URL)
	client.SetClock(func() time.Time { return now })
	ch := discord.NewChannel(client)
	ctx := context.Background()
	posted := func() int { return len(fake.Calls(discordtest.RouteCreateMessage)) }

	// The answer to a delivered message says the route is used up for 2.5 s.
	fake.Exhaust(discordtest.RouteCreateMessage, "2.5")
	if _, err := ch.Send(ctx, message(alert.StatusFiring, discordtest.Channel, "")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, err := ch.Send(ctx, message(alert.StatusFiring, discordtest.Channel, ""))
	if err == nil || !strings.Contains(err.Error(), "not sent, the rate limit Discord reported is still in force") || posted() != 1 {
		t.Fatalf("Send while the route is used up = %v after %d requests, want it held back", err, posted())
	}
	if _, wait := notify.Classify(err); wait != 2500*time.Millisecond {
		t.Errorf("the pipeline is asked to wait %v, want 2.5s", wait)
	}
	// Limits are per channel: another channel is not held up.
	if _, err := ch.Send(ctx, message(alert.StatusFiring, "100000000000000002", "")); err != nil {
		t.Errorf("Send to another channel: %v", err)
	}
	now = now.Add(2500 * time.Millisecond)
	if _, err := ch.Send(ctx, message(alert.StatusFiring, discordtest.Channel, "")); err != nil {
		t.Errorf("Send after the reset: %v", err)
	}

	// A 429 names the wait, with decimals, and a global one stops every
	// route.
	sent := posted()
	fake.Fail(discordtest.RouteCreateMessage, discordtest.Failure{Status: 429, RetryAfter: "64.57", Global: true})
	_, err = ch.Send(ctx, message(alert.StatusFiring, discordtest.Channel, ""))
	if _, wait := notify.Classify(err); err == nil || wait != 64570*time.Millisecond {
		t.Fatalf("Send = %v, wait %v; want the 64.57 seconds Discord asked for", err, wait)
	}
	if _, err := ch.Send(ctx, message(alert.StatusFiring, "100000000000000002", "")); err == nil || posted() != sent+1 {
		t.Errorf("Send to another channel during a global limit = %v after %d more requests, want it held back", err, posted()-sent)
	}
	now = now.Add(65 * time.Second)
	if _, err := ch.Send(ctx, message(alert.StatusFiring, "100000000000000002", "")); err != nil {
		t.Errorf("Send after the global limit: %v", err)
	}
}

// pipelineFixture runs the real pipeline with the Discord channel against
// the fake Discord and a real database.
type pipelineFixture struct {
	pool   *pgxpool.Pool
	alerts *alert.Store
	fake   *discordtest.Server
	source string
	policy string
}

func newPipelineFixture(t *testing.T) *pipelineFixture {
	t.Helper()
	pool := postgrestest.NewMigrated(t)
	f := &pipelineFixture{pool: pool, alerts: alert.NewStore(pool), fake: discordtest.New(t, botToken)}
	f.source = provisiontest.Row(t, pool, provision.Sources, "test")
	integration := provisiontest.Row(t, pool, provision.Integrations, "discord")

	p := notify.New(pool, f.alerts, slog.New(slog.DiscardHandler), notify.Config{
		PollInterval: 10 * time.Millisecond, MaxAttempts: 3, BackoffBase: 20 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
	})
	p.Register(integration, newChannel(f.fake.URL))
	f.policy = provisiontest.Row(t, pool, provision.EscalationPolicies, "default")
	p.SetPolicies(map[string]notify.Policy{f.policy: {Name: "default", Steps: []notify.Step{{
		Targets: []notify.StepTarget{notify.ChannelTarget(integration, "discord", discordtest.Channel)}, Timeout: 1000 * time.Hour,
	}}}})
	f.alerts.Observe(p.Observe)

	runCtx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { p.Run(runCtx) })
	running.Go(func() { p.RunGroups(runCtx, 10*time.Millisecond) })
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	return f
}

func (f *pipelineFixture) fire(t *testing.T) alert.Alert {
	t.Helper()
	a, _, err := f.alerts.Fire(context.Background(), alert.Firing{
		SourceID: f.source, Fingerprint: "fp", Severity: alert.SeverityCritical,
		Labels:      map[string]string{"alertname": "DiskFull", "host": "db1"},
		Annotations: map[string]string{"summary": "Disk is full", "runbook_url": "https://runbooks.example.com/disk"},
		Group:       alert.Grouping{Key: "fp", EscalationPolicyID: f.policy},
	})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	return a
}

// attempts returns the outcome and error of every attempt, in order, once
// no notification is due or pending any more.
func (f *pipelineFixture) attempts(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var pending int
		if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM notifications WHERE status = 'pending') +
			(SELECT count(*) FROM alert_groups WHERE notify_at IS NOT NULL)`).Scan(&pending); err != nil {
			t.Fatalf("count: %v", err)
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("notifications are still pending")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := f.pool.Query(ctx, `SELECT outcome || COALESCE(': ' || error, '') FROM notification_attempts ORDER BY id`)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	return out
}

func TestPipelinePostsAndEditsTheMessage(t *testing.T) {
	f := newPipelineFixture(t)
	ctx := context.Background()
	a := f.fire(t)
	f.attempts(t)

	msgs := f.fake.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages after firing, want 1", len(msgs))
	}
	for _, want := range []string{"🔴 **Firing** · critical", "**Disk is full**", "`alertname=DiskFull` `host=db1`", "[📖 Runbook](https://runbooks.example.com/disk)"} {
		if !strings.Contains(msgs[0].JSON(), want) {
			t.Errorf("the message does not contain %q:\n%s", want, msgs[0].JSON())
		}
	}

	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.ChatUser("discord", ada, "alice"), alert.Options{}); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	f.attempts(t)
	if msgs = f.fake.Messages(); len(msgs) != 1 || msgs[0].Edits != 1 || !strings.Contains(msgs[0].JSON(), "Acknowledged by alice") {
		t.Fatalf("after acknowledging: %d messages:\n%s", len(msgs), msgs[0].JSON())
	}

	if _, _, err := f.alerts.ResolveFromSource(ctx, f.source, "fp", time.Time{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	f.attempts(t)
	if msgs = f.fake.Messages(); len(msgs) != 1 || msgs[0].Edits != 2 || !strings.Contains(msgs[0].JSON(), "Resolved by the source") {
		t.Fatalf("after resolving: %d messages:\n%s", len(msgs), msgs[0].JSON())
	}
	var ref string
	if err := f.pool.QueryRow(ctx, `SELECT DISTINCT external_ref FROM notifications`).Scan(&ref); err != nil || ref != discordtest.Channel+":"+msgs[0].ID {
		t.Errorf("external_ref = %q, %v; want every notification to reference the one message", ref, err)
	}
}

func TestPipelineRetriesDiscordFailures(t *testing.T) {
	tests := []struct {
		name     string
		failures []discordtest.Failure
		want     []string
		messages int
	}{
		{
			name:     "server error then success",
			failures: []discordtest.Failure{{Status: 502}},
			want:     []string{"retryable_error: discord create message: HTTP status 502", "success"},
			messages: 1,
		},
		{
			name:     "unknown channel fails at once",
			failures: []discordtest.Failure{{Status: 404, Code: 10003}},
			want:     []string{"permanent_error: discord create message: HTTP status 404, error code 10003 (the channel does not exist, or the bot cannot see it; use the channel's numeric ID)"},
		},
		{
			name:     "a token that is not valid fails at once",
			failures: []discordtest.Failure{{Status: 401}},
			want:     []string{"permanent_error: discord create message: HTTP status 401 (the bot token is not valid)"},
		},
		{
			name:     "gives up after the limit",
			failures: []discordtest.Failure{{Status: 500}, {Status: 500}, {Status: 500}},
			want:     []string{"retryable_error: discord create message: HTTP status 500", "retryable_error: discord create message: HTTP status 500", "retryable_error: discord create message: HTTP status 500"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPipelineFixture(t)
			f.fake.Fail(discordtest.RouteCreateMessage, tt.failures...)
			f.fire(t)
			got := f.attempts(t)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("attempts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if n := len(f.fake.Messages()); n != tt.messages {
				t.Errorf("%d messages in Discord, want %d", n, tt.messages)
			}
			for _, a := range got {
				if strings.Contains(a, botToken) {
					t.Errorf("a stored error contains the bot token: %s", a)
				}
			}
		})
	}
}
