package cli

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

// waitFor polls until cond is true.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeNotifiesSlack(t *testing.T) {
	const (
		sourceToken = "source-token-marker-7c2e41d0"
		botToken    = "xoxb-bot-token-marker-55d1"
	)
	fake := slacktest.New(t, botToken)
	dbURL := postgrestest.NewURL(t)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL":    dbURL,
		"BEACON_SOURCE_TOKEN":    sourceToken,
		"BEACON_SLACK_BOT_TOKEN": botToken,
		"BEACON_SLACK_CHANNEL":   "C0ALERTS",
		"BEACON_SLACK_API_URL":   fake.URL,
	})
	url := srv.baseURL + "/api/v1/ingest/default"

	code, body := postJSON(t, url, sourceToken, `{
		"labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"},
		"annotations": {"summary": "Disk is full on db1", "runbook_url": "https://runbooks.example.com/disk"}}`)
	if code != http.StatusCreated {
		t.Fatalf("fire = %d %v", code, body)
	}
	waitFor(t, "the Slack message", func() bool { return len(fake.Messages()) == 1 })
	msg := fake.Messages()[0]
	if msg.Channel != "C0ALERTS" || msg.Text != "[FIRING] critical: Disk is full on db1" || !strings.Contains(msg.JSON(), "Runbook") {
		t.Errorf("message = %+v", msg)
	}

	code, body = postJSON(t, url, sourceToken, `{"status": "resolved", "labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"}}`)
	if code != http.StatusOK || body["result"] != "resolved" {
		t.Fatalf("resolve = %d %v", code, body)
	}
	waitFor(t, "the message to be updated", func() bool { return fake.Messages()[0].Updates == 1 })
	if msgs := fake.Messages(); len(msgs) != 1 || msgs[0].Text != "[RESOLVED] critical: Disk is full on db1" || !strings.Contains(msgs[0].JSON(), "Resolved by the source") {
		t.Errorf("messages after the resolve = %+v", msgs)
	}

	pool, err := postgres.Open(context.Background(), postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()
	var spec string
	var sent, events int
	err = pool.QueryRow(context.Background(), `SELECT
		(SELECT spec::text FROM integrations WHERE name = 'slack'),
		(SELECT count(*) FROM notifications WHERE status = 'sent' AND destination = 'C0ALERTS'),
		(SELECT count(*) FROM alert_events WHERE type = 'notification_sent')`).Scan(&spec, &sent, &events)
	if err != nil || sent != 2 || events != 2 {
		t.Errorf("sent notifications = %d, timeline events = %d, %v; want 2 of each", sent, events, err)
	}
	assertNoSecrets(t, "the stored integration", spec, botToken)
	if !strings.Contains(spec, "BEACON_SLACK_BOT_TOKEN") {
		t.Errorf("the stored integration does not name where its token comes from: %s", spec)
	}

	if code := srv.stop(t); code != exitOK {
		t.Errorf("exit code = %d", code)
	}
	assertNoSecrets(t, "the logs", srv.logs.String(), botToken, sourceToken)
}

// TestSlackFailureDoesNotLeakTheToken makes Slack unreachable and rejecting,
// which is when error text is built, logged and stored.
func TestSlackFailureDoesNotLeakTheToken(t *testing.T) {
	const (
		sourceToken = "source-token-marker-7c2e41d0"
		botToken    = "xoxb-bot-token-marker-55d1"
	)
	fake := slacktest.New(t, "a-different-token")
	dbURL := postgrestest.NewURL(t)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL":    dbURL,
		"BEACON_SOURCE_TOKEN":    sourceToken,
		"BEACON_SLACK_BOT_TOKEN": botToken,
		"BEACON_SLACK_CHANNEL":   "C0ALERTS",
		"BEACON_SLACK_API_URL":   fake.URL,
		"BEACON_LOG_LEVEL":       "debug",
	})
	if code, body := postJSON(t, srv.baseURL+"/api/v1/ingest/default", sourceToken, `{"labels": {"alertname": "DiskFull"}}`); code != http.StatusCreated {
		t.Fatalf("fire = %d %v", code, body)
	}

	pool, err := postgres.Open(context.Background(), postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()
	var stored string
	waitFor(t, "the failed attempt", func() bool {
		err := pool.QueryRow(context.Background(), `SELECT a.error || ' ' || e.data::text FROM notification_attempts a, alert_events e
			WHERE e.type = 'notification_failed'`).Scan(&stored)
		return err == nil
	})
	if !strings.Contains(stored, "invalid_auth") {
		t.Errorf("stored error = %s, want invalid_auth", stored)
	}
	assertNoSecrets(t, "the stored attempt and event", stored, botToken)
	srv.stop(t)
	assertNoSecrets(t, "the logs", srv.logs.String(), botToken, sourceToken)
}

func TestServeSlackSettingsErrors(t *testing.T) {
	const dbURL = "postgres://localhost/beacon"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"token without channel", []string{"--slack-bot-token", "xoxb-some-token"}, "the Slack settings need both a bot token and a channel"},
		{"channel without token", []string{"--slack-channel", "C0ALERTS"}, "the Slack settings need both a bot token and a channel"},
		{"bad API URL", []string{"--slack-api-url", "slack.com/api"}, "--slack-api-url must be an http or https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLIEnv(t, nil, append([]string{"serve", "--database-url", dbURL}, tt.args...)...)
			if code != exitError || !strings.Contains(stderr, tt.want) {
				t.Errorf("exit %d, stderr = %q; want an error containing %q", code, stderr, tt.want)
			}
			assertNoSecrets(t, "stderr", stderr, "xoxb-some-token")
		})
	}
}
