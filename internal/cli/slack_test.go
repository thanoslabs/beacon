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
		sourceToken   = "source-token-marker-7c2e41d0"
		botToken      = "xoxb-bot-token-marker-55d1"
		signingSecret = "signing-secret-marker-e07b"
	)
	fake := slacktest.New(t, botToken)
	dbURL := postgrestest.NewURL(t)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL":         dbURL,
		"BEACON_SOURCE_TOKEN":         sourceToken,
		"BEACON_SLACK_BOT_TOKEN":      botToken,
		"BEACON_SLACK_SIGNING_SECRET": signingSecret,
		"BEACON_CONFIG":               writeConfig(t, slackConfig),
		"BEACON_SLACK_API_URL":        fake.URL,
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

	// A button press is refused unless Slack signed it with the app's secret.
	interactions := srv.baseURL + "/api/v1/integrations/slack/interactions"
	user := slacktest.User{ID: "U0ALICE", Username: "alice"}
	if code := fake.Click(t, interactions, "not-the-signing-secret", msg, "beacon_acknowledge", user); code != http.StatusUnauthorized {
		t.Errorf("click signed with the wrong secret = %d, want 401", code)
	}
	if code := fake.Click(t, interactions, signingSecret, msg, "beacon_acknowledge", user); code != http.StatusOK {
		t.Fatalf("click = %d, want 200", code)
	}
	waitFor(t, "the message to show the acknowledgement", func() bool { return fake.Messages()[0].Updates == 1 })
	if got := fake.Messages()[0].JSON(); !strings.Contains(got, "Acknowledged by alice") {
		t.Errorf("message after the click: %s", got)
	}

	code, body = postJSON(t, url, sourceToken, `{"status": "resolved", "labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"}}`)
	if code != http.StatusOK || body["result"] != "resolved" {
		t.Fatalf("resolve = %d %v", code, body)
	}
	waitFor(t, "the message to be updated", func() bool { return fake.Messages()[0].Updates == 2 })
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
	if err != nil || sent != 3 || events != 3 {
		t.Errorf("sent notifications = %d, timeline events = %d, %v; want 3 of each", sent, events, err)
	}
	assertNoSecrets(t, "the stored integration", spec, botToken, signingSecret)
	if !strings.Contains(spec, "BEACON_SLACK_BOT_TOKEN") {
		t.Errorf("the stored integration does not name where its token comes from: %s", spec)
	}

	if code := srv.stop(t); code != exitOK {
		t.Errorf("exit code = %d", code)
	}
	assertNoSecrets(t, "the logs", srv.logs.String(), botToken, sourceToken, signingSecret)
}

// TestSlackFailureDoesNotLeakTheToken makes Slack unreachable and rejecting,
// which is when error text is built, logged and stored.
func TestSlackFailureDoesNotLeakTheToken(t *testing.T) {
	const (
		sourceToken   = "source-token-marker-7c2e41d0"
		botToken      = "xoxb-bot-token-marker-55d1"
		signingSecret = "signing-secret-marker-e07b"
	)
	fake := slacktest.New(t, "a-different-token")
	dbURL := postgrestest.NewURL(t)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL":         dbURL,
		"BEACON_SOURCE_TOKEN":         sourceToken,
		"BEACON_SLACK_BOT_TOKEN":      botToken,
		"BEACON_SLACK_SIGNING_SECRET": signingSecret,
		"BEACON_CONFIG":               writeConfig(t, slackConfig),
		"BEACON_SLACK_API_URL":        fake.URL,
		"BEACON_LOG_LEVEL":            "debug",
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
	assertNoSecrets(t, "the logs", srv.logs.String(), botToken, sourceToken, signingSecret)
}

func TestServeRejectsABadSlackAPIURL(t *testing.T) {
	code, _, stderr := runCLIEnv(t, nil, "serve", "--database-url", "postgres://localhost/beacon", "--slack-api-url", "slack.com/api")
	if code != exitError || !strings.Contains(stderr, "--slack-api-url must be an http or https URL") {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
}

// TestServeWarnsAboutWhatIsNotImplemented starts with source and integration
// types that are valid in the files but have no behaviour yet. They are
// stored, and the log says that they do nothing.
func TestServeWarnsAboutWhatIsNotImplemented(t *testing.T) {
	const token = "source-token-marker-7c2e41d0"
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL": postgrestest.NewURL(t),
		"BEACON_SOURCE_TOKEN": token,
		"BEACON_CONFIG": writeConfig(t, `sources:
  - name: am
    type: alertmanager
    token: {env: BEACON_SOURCE_TOKEN}
integrations:
  - name: tickets
    type: webhook
    webhook: {url: "https://tickets.example.com/hook"}
escalation_policies:
  - name: default
    steps:
      - notify: [{channel: tickets, integration: tickets}]
        timeout: 5m
routing:
  default: {escalation_policy: default}
`),
	})
	if code, _ := postJSON(t, srv.baseURL+"/api/v1/ingest/am", token, `{"labels": {"alertname": "A"}}`); code != http.StatusUnauthorized {
		t.Errorf("POST to an alertmanager source = %d, want 401", code)
	}
	logs := srv.logs.String()
	for _, want := range []string{"this type of source does not receive alerts yet", "this type of integration does not deliver yet", "alerts are stored but nobody is notified"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs do not contain %q:\n%s", want, logs)
		}
	}
}
