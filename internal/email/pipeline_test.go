package email_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/email"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

// TestPipelineRetriesEmailFailures runs the real pipeline with the email
// channel against a real database: a temporary refusal is tried again, a
// final one is not, and each later message about the group answers the one
// before.
func TestPipelineRetriesEmailFailures(t *testing.T) {
	tests := []struct {
		name    string
		replies map[string]string
		want    []string
		sent    int
	}{
		{"delivered", nil, []string{"success"}, 1},
		{"a mailbox that does not exist fails at once", map[string]string{"RCPT": "550 5.1.1 no such user"},
			[]string{"permanent_error: email: RCPT TO: the SMTP server answered 550 5.1.1 no such user"}, 0},
		{"a temporary refusal is tried until the limit", map[string]string{"MAIL": "451 4.3.0 try again later"},
			[]string{"retryable_error: email: MAIL FROM: the SMTP server answered 451 4.3.0 try again later",
				"retryable_error: email: MAIL FROM: the SMTP server answered 451 4.3.0 try again later",
				"retryable_error: email: MAIL FROM: the SMTP server answered 451 4.3.0 try again later"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := postgrestest.NewMigrated(t)
			ctx := context.Background()
			alerts := alert.NewStore(pool)
			source := provisiontest.Row(t, pool, provision.Sources, "test")
			integration := provisiontest.Row(t, pool, provision.Integrations, "email")
			policy := provisiontest.Row(t, pool, provision.EscalationPolicies, "default")
			fake := newFakeSMTP(t)
			for k, v := range tt.replies {
				fake.replies[k] = v
			}
			ch := email.NewChannel(config.EmailIntegration{Host: "127.0.0.1", Port: fake.start(), From: "beacon@example.invalid", TLS: config.EmailTLSNone}, "")

			p := notify.New(pool, alerts, slog.New(slog.DiscardHandler), notify.Config{
				PollInterval: 10 * time.Millisecond, MaxAttempts: 3, BackoffBase: 20 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
			})
			p.Register(integration, ch)
			p.SetPolicies(map[string]notify.Policy{policy: {Name: "default", Steps: []notify.Step{{
				Targets: []notify.StepTarget{notify.ChannelTarget(integration, "email", "oncall@example.invalid")}, Timeout: 1000 * time.Hour,
			}}}})
			alerts.Observe(p.Observe)
			runCtx, cancel := context.WithCancel(ctx)
			var running sync.WaitGroup
			running.Go(func() { p.Run(runCtx) })
			running.Go(func() { p.RunGroups(runCtx, 10*time.Millisecond) })
			t.Cleanup(func() {
				cancel()
				running.Wait()
			})
			settled := func() []string {
				t.Helper()
				deadline := time.Now().Add(20 * time.Second)
				for {
					var pending int
					if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM notifications WHERE status = 'pending') +
						(SELECT count(*) FROM alert_groups WHERE notify_at IS NOT NULL)`).Scan(&pending); err != nil {
						t.Fatal(err)
					}
					if pending == 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("notifications are still pending")
					}
					time.Sleep(10 * time.Millisecond)
				}
				rows, _ := pool.Query(ctx, `SELECT outcome || COALESCE(': ' || error, '') FROM notification_attempts ORDER BY id`)
				out, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					t.Fatal(err)
				}
				return out
			}

			a, _, err := alerts.Fire(ctx, alert.Firing{
				SourceID: source, Fingerprint: "fp", Severity: alert.SeverityCritical,
				Labels: map[string]string{"alertname": "DiskFull"}, Annotations: map[string]string{"summary": "Disk is full"},
				Group: alert.Grouping{Key: "fp", EscalationPolicyID: policy},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := settled(); strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("attempts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if n := len(fake.received()); n != tt.sent {
				t.Fatalf("%d messages were taken by the server, want %d", n, tt.sent)
			}
			if tt.sent == 0 {
				return
			}

			// An email cannot be edited: the acknowledgement is a second
			// message, which answers the first.
			if _, err := alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.ChatUser("slack", "U1", "alice"), alert.Options{}); err != nil {
				t.Fatal(err)
			}
			settled()
			msgs := fake.received()
			if len(msgs) != 2 {
				t.Fatalf("%d messages after the acknowledgement, want 2", len(msgs))
			}
			var first string
			if err := pool.QueryRow(ctx, `SELECT external_ref FROM notifications ORDER BY created_at, id LIMIT 1`).Scan(&first); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(msgs[0], "Message-ID: "+first) || !strings.Contains(msgs[1], "In-Reply-To: "+first) ||
				!strings.Contains(msgs[1], "Subject: [ACKNOWLEDGED] critical: Disk is full") || !strings.Contains(msgs[1], "Acknowledged by alice") {
				t.Errorf("the second message does not answer the first (%s):\n%s", first, msgs[1])
			}
		})
	}
}
