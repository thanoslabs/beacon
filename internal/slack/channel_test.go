package slack_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
	"github.com/thanoslabs/beacon/internal/slack"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

const botToken = "xoxb-test-bot-token-marker-93ab"

func newChannel(apiURL string) *slack.Channel {
	return slack.NewChannel(slack.NewClient(apiURL, config.NewSecret(config.SecretRef{}, botToken)))
}

func message(status alert.Status, destination, previousRef string) notify.Message {
	return notify.Message{
		Notification: notify.Notification{Destination: destination},
		Alerts: []alert.Alert{{
			ID: "a1", Status: status, Severity: alert.SeverityCritical,
			Labels:      map[string]string{"alertname": "DiskFull"},
			Annotations: map[string]string{"summary": "Disk is full"},
			StartedAt:   time.Now(),
		}},
		PreviousRef: previousRef,
	}
}

func TestSendPostsThenUpdatesInPlace(t *testing.T) {
	fake := slacktest.New(t, botToken)
	ch := newChannel(fake.URL)
	ctx := context.Background()

	ref, err := ch.Send(ctx, message(alert.StatusFiring, "#alerts", ""))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref != "C0ALERTS:1700000000.000001" {
		t.Errorf("ref = %q, want the channel ID and the message timestamp", ref)
	}
	posts := fake.Calls("chat.postMessage")
	if len(posts) != 1 || posts[0].Body["channel"] != "alerts" || posts[0].Body["text"] != "[FIRING] critical: Disk is full" {
		t.Fatalf("chat.postMessage calls = %+v", posts)
	}
	if _, hasTS := posts[0].Body["ts"]; hasTS {
		t.Errorf("chat.postMessage was sent a ts")
	}

	ref2, err := ch.Send(ctx, message(alert.StatusAcknowledged, "#alerts", ref))
	if err != nil || ref2 != ref {
		t.Fatalf("second Send = %q, %v; want the same reference", ref2, err)
	}
	updates := fake.Calls("chat.update")
	if len(updates) != 1 || updates[0].Body["channel"] != "C0ALERTS" || updates[0].Body["ts"] != "1700000000.000001" {
		t.Fatalf("chat.update calls = %+v", updates)
	}
	if len(fake.Calls("chat.postMessage")) != 1 {
		t.Errorf("the update posted a second message")
	}
	msgs := fake.Messages()
	if len(msgs) != 1 || msgs[0].Updates != 1 || !strings.Contains(msgs[0].JSON(), "*Acknowledged*") || msgs[0].Text != "[ACKNOWLEDGED] critical: Disk is full" {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestSendToAChannelID(t *testing.T) {
	fake := slacktest.New(t, botToken)
	if _, err := newChannel(fake.URL).Send(context.Background(), message(alert.StatusFiring, "C0ALERTS", "")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := fake.Calls("chat.postMessage")[0].Body["channel"]; got != "C0ALERTS" {
		t.Errorf("channel = %v", got)
	}
}

func TestUpdateOfADeletedMessagePostsAgain(t *testing.T) {
	fake := slacktest.New(t, botToken)
	ch := newChannel(fake.URL)
	ctx := context.Background()
	ref, err := ch.Send(ctx, message(alert.StatusFiring, "alerts", ""))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	fake.Delete("C0ALERTS", "1700000000.000001")

	ref2, err := ch.Send(ctx, message(alert.StatusResolved, "alerts", ref))
	if err != nil {
		t.Fatalf("Send after the message was deleted: %v", err)
	}
	if ref2 == ref || ref2 != "C0ALERTS:1700000000.000002" {
		t.Errorf("ref = %q, want a reference to a new message", ref2)
	}
	if msgs := fake.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0].JSON(), "*Resolved*") {
		t.Errorf("messages = %+v", msgs)
	}

	// Another failure of the update is not papered over with a new message.
	fake.Fail("chat.update", slacktest.Failure{Error: "internal_error"})
	if _, err := ch.Send(ctx, message(alert.StatusResolved, "alerts", ref2)); err == nil {
		t.Error("Send succeeded although chat.update failed")
	}
	if n := len(fake.Calls("chat.postMessage")); n != 2 {
		t.Errorf("%d messages posted, want 2", n)
	}
}

func TestErrorsNeverContainTheToken(t *testing.T) {
	fake := slacktest.New(t, "another-token")
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not slack</html>"))
	}))
	defer garbage.Close()

	tests := []struct {
		name, url, want string
		fail            *slacktest.Failure
	}{
		{name: "rejected token", url: fake.URL, want: "slack chat.postMessage: invalid_auth (the bot token is not valid)"},
		{name: "connection refused", url: closed.URL, want: "connection refused"},
		{name: "not JSON", url: garbage.URL, want: "the response is not JSON"},
		{name: "HTTP 503", url: fake.URL, fail: &slacktest.Failure{Status: 503}, want: "HTTP status 503"},
		{name: "rate limited", url: fake.URL, fail: &slacktest.Failure{Status: 429, RetryAfter: "7"}, want: "rate limited"},
		{name: "not in channel", url: fake.URL, fail: &slacktest.Failure{Error: "not_in_channel"}, want: "invite the app to the channel"},
		{name: "bad URL", url: "http://bad host/", want: "slack chat.postMessage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fail != nil {
				fake.Fail("chat.postMessage", *tt.fail)
			}
			_, err := newChannel(tt.url).Send(context.Background(), message(alert.StatusFiring, "alerts", ""))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Send = %v, want an error containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), botToken) || strings.Contains(err.Error(), "xoxb") {
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
	_, err := newChannel(slow.URL).Send(ctx, message(alert.StatusFiring, "alerts", ""))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("Send = %v after %v, want the context deadline", err, time.Since(start))
	}
}

// pipelineFixture runs the real pipeline with the Slack channel against the
// fake Slack and a real database.
type pipelineFixture struct {
	pool   *pgxpool.Pool
	alerts *alert.Store
	fake   *slacktest.Server
	source string
}

func newPipelineFixture(t *testing.T) *pipelineFixture {
	t.Helper()
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	f := &pipelineFixture{pool: pool, alerts: alert.NewStore(pool), fake: slacktest.New(t, botToken)}
	f.source = provisiontest.Row(t, pool, provision.Sources, "test")
	integration := provisiontest.Row(t, pool, provision.Integrations, "slack")

	p := notify.New(pool, f.alerts, slog.New(slog.DiscardHandler), notify.Config{
		PollInterval: 10 * time.Millisecond, MaxAttempts: 3, BackoffBase: 20 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
	})
	p.Register(integration, newChannel(f.fake.URL))
	f.alerts.Observe(p.NotifyAll([]notify.Target{{IntegrationID: integration, Destination: "#alerts"}}))

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return f
}

func (f *pipelineFixture) fire(t *testing.T) alert.Alert {
	t.Helper()
	a, _, err := f.alerts.Fire(context.Background(), alert.Firing{
		SourceID: f.source, Fingerprint: "fp", Severity: alert.SeverityCritical,
		Labels:      map[string]string{"alertname": "DiskFull", "host": "db1"},
		Annotations: map[string]string{"summary": "Disk is full", "runbook_url": "https://runbooks.example.com/disk"},
	})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	return a
}

// attempts returns the outcome and error of every attempt, in order, once
// there are no pending notifications left.
func (f *pipelineFixture) attempts(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var pending int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE status = 'pending'`).Scan(&pending); err != nil {
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

func TestPipelinePostsAndUpdatesTheMessage(t *testing.T) {
	f := newPipelineFixture(t)
	ctx := context.Background()
	a := f.fire(t)
	f.attempts(t)

	msgs := f.fake.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages after firing, want 1", len(msgs))
	}
	for _, want := range []string{"*Firing* · critical", "*Disk is full*", "`alertname=DiskFull` `host=db1`", "\\u003chttps://runbooks.example.com/disk|:book: Runbook\\u003e"} {
		if !strings.Contains(msgs[0].JSON(), want) {
			t.Errorf("the message does not contain %q:\n%s", want, msgs[0].JSON())
		}
	}

	if _, err := f.alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.ChatUser("slack", "U1", "alice"), alert.Options{}); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	f.attempts(t)
	msgs = f.fake.Messages()
	if len(msgs) != 1 || msgs[0].Updates != 1 || !strings.Contains(msgs[0].JSON(), "Acknowledged by alice") {
		t.Fatalf("after acknowledging: %d messages, %d updates:\n%s", len(msgs), msgs[0].Updates, msgs[0].JSON())
	}

	if _, _, err := f.alerts.ResolveFromSource(ctx, f.source, "fp", time.Time{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	f.attempts(t)
	msgs = f.fake.Messages()
	if len(msgs) != 1 || msgs[0].Updates != 2 || !strings.Contains(msgs[0].JSON(), "Resolved by the source") || msgs[0].Text != "[RESOLVED] critical: Disk is full" {
		t.Fatalf("after resolving: %d messages, %d updates:\n%s", len(msgs), msgs[0].Updates, msgs[0].JSON())
	}

	var ref string
	if err := f.pool.QueryRow(ctx, `SELECT DISTINCT external_ref FROM notifications`).Scan(&ref); err != nil || ref != "C0ALERTS:"+msgs[0].TS {
		t.Errorf("external_ref = %q, %v; want every notification to reference the one message", ref, err)
	}
}

func TestPipelineRetriesSlackFailures(t *testing.T) {
	tests := []struct {
		name     string
		failures []slacktest.Failure
		want     []string
		messages int
	}{
		{
			name:     "server error then success",
			failures: []slacktest.Failure{{Status: 502}},
			want:     []string{"retryable_error: slack chat.postMessage: HTTP status 502", "success"},
			messages: 1,
		},
		{
			name:     "temporary error code then success",
			failures: []slacktest.Failure{{Error: "internal_error"}, {Error: "ratelimited"}},
			want:     []string{"retryable_error: slack chat.postMessage: internal_error", "retryable_error: slack chat.postMessage: ratelimited", "success"},
			messages: 1,
		},
		{
			name:     "unknown channel fails at once",
			failures: []slacktest.Failure{{Error: "channel_not_found"}},
			want:     []string{"permanent_error: slack chat.postMessage: channel_not_found (check the channel; a private channel must be given by its ID and the app must be invited to it)"},
		},
		{
			name:     "revoked token fails at once",
			failures: []slacktest.Failure{{Error: "token_revoked"}},
			want:     []string{"permanent_error: slack chat.postMessage: token_revoked"},
		},
		{
			name:     "gives up after the limit",
			failures: []slacktest.Failure{{Status: 500}, {Status: 500}, {Status: 500}},
			want:     []string{"retryable_error: slack chat.postMessage: HTTP status 500", "retryable_error: slack chat.postMessage: HTTP status 500", "retryable_error: slack chat.postMessage: HTTP status 500"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPipelineFixture(t)
			f.fake.Fail("chat.postMessage", tt.failures...)
			f.fire(t)
			got := f.attempts(t)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("attempts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if n := len(f.fake.Messages()); n != tt.messages {
				t.Errorf("%d messages in Slack, want %d", n, tt.messages)
			}
			for _, a := range got {
				if strings.Contains(a, botToken) {
					t.Errorf("a stored error contains the bot token: %s", a)
				}
			}
		})
	}
}

func TestPipelineWaitsAsLongAsSlackAsks(t *testing.T) {
	f := newPipelineFixture(t)
	f.fake.Fail("chat.postMessage", slacktest.Failure{Status: 429, RetryAfter: "120"})
	start := time.Now()
	f.fire(t)

	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	var next time.Time
	var attempts int
	for attempts == 0 {
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM notification_attempts`).Scan(&attempts); err != nil {
			t.Fatalf("count: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("no attempt was made")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var status, outcome string
	if err := f.pool.QueryRow(ctx, `SELECT n.status, n.next_attempt_at, a.outcome || ': ' || a.error FROM notifications n JOIN notification_attempts a ON a.notification_id = n.id`).Scan(&status, &next, &outcome); err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if status != "pending" || outcome != "retryable_error: slack chat.postMessage: rate limited" {
		t.Errorf("status = %s, attempt = %s", status, outcome)
	}
	if wait := next.Sub(start); wait < 119*time.Second || wait > 125*time.Second {
		t.Errorf("the next attempt is in %v, want the 120 seconds Slack asked for", wait)
	}
}
