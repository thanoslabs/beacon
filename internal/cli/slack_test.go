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
	// This configuration has no identity provider, so there are no users
	// and nobody's Slack account is linked. Acknowledging is refused, with
	// a note to the person who pressed, and the log said at the start that
	// it would be.
	if code := fake.Click(t, interactions, signingSecret, msg, "beacon_acknowledge", user); code != http.StatusOK {
		t.Fatalf("click = %d, want 200", code)
	}
	if notes := fake.Responses(); len(notes) != 1 || !strings.Contains(notes[0]["text"].(string), "only Beacon users can acknowledge an alert") ||
		!strings.Contains(notes[0]["text"].(string), "ask whoever runs Beacon for its address") {
		t.Fatalf("answer to an acknowledgement by an account that is not linked = %v", notes)
	}
	if fake.Messages()[0].Updates != 0 {
		t.Errorf("the message changed after a refused press")
	}
	if !strings.Contains(srv.logs.String(), `"level":"WARN","msg":"nobody can acknowledge or snooze from Slack`) {
		t.Errorf("the logs do not warn that nobody can acknowledge from Slack")
	}

	// Alice becomes a Beacon user whose Slack account is linked, as signing
	// in and the lookup by email address would make her.
	pool, err := postgres.Open(context.Background(), postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()
	_, err = pool.Exec(context.Background(), `WITH u AS (INSERT INTO users (email, display_name) VALUES ('alice@example.com', 'alice') RETURNING id)
		INSERT INTO user_identities (user_id, integration_id, provider, external_id, origin, created_at)
		SELECT u.id, i.id, 'slack', 'U0ALICE', 'email', now() FROM u, integrations i WHERE i.name = 'slack'`)
	if err != nil {
		t.Fatalf("link Alice: %v", err)
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

	var spec string
	var sent, events int
	// Slack has the last update a moment before Beacon has recorded that it
	// was delivered, so the records are waited for.
	waitFor(t, "three recorded deliveries", func() bool {
		err = pool.QueryRow(context.Background(), `SELECT
			(SELECT spec::text FROM integrations WHERE name = 'slack'),
			(SELECT count(*) FROM notifications WHERE status = 'sent' AND destination = 'C0ALERTS'),
			(SELECT count(*) FROM alert_events WHERE type = 'notification_sent')`).Scan(&spec, &sent, &events)
		return err != nil || (sent == 3 && events == 3)
	})
	if err != nil {
		t.Errorf("read the recorded deliveries: %v", err)
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

func TestServeRejectsABadDiscordAPIURL(t *testing.T) {
	code, _, stderr := runCLIEnv(t, nil, "serve", "--database-url", "postgres://localhost/beacon", "--discord-api-url", "discord.com/api")
	if code != exitError || !strings.Contains(stderr, "--discord-api-url must be an http or https URL") {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
}

// TestServeWarnsAboutAPolicyThatReachesNobody starts with a policy whose
// targets are people, and no integration that reaches a person. It is
// valid, and the log says that it notifies nobody.
func TestServeWarnsAboutAPolicyThatReachesNobody(t *testing.T) {
	const token = "source-token-marker-7c2e41d0"
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL":        postgrestest.NewURL(t),
		"BEACON_SOURCE_TOKEN":        token,
		"BEACON_HOOK_SIGNING_SECRET": "hook-signing-secret-marker-4d1a",
		"BEACON_CONFIG": writeConfig(t, `sources:
  - name: grafana
    type: grafana
    token: {env: BEACON_SOURCE_TOKEN}
integrations:
  - name: tickets
    type: webhook
    webhook: {url: "https://tickets.example.com/hook?token=query-token-marker-19de", signing_secret: {env: BEACON_HOOK_SIGNING_SECRET}}
escalation_policies:
  - name: default
    steps:
      - notify: [{user: ada@example.com}]
        timeout: 5m
  - name: tickets
    steps:
      - notify: [{integration: tickets}]
        timeout: 5m
routing:
  default: {escalation_policy: default}
`),
	})
	// Every type of source receives alerts.
	if code, body := postJSON(t, srv.baseURL+"/api/v1/ingest/grafana", token, `{"labels": {"alertname": "A"}}`); code != http.StatusBadRequest || body["field"] != "alerts" {
		t.Errorf("a generic payload to a grafana source = %d %v, want 400 naming alerts", code, body)
	}
	logs := srv.logs.String()
	// A webhook reaches no person, so the first policy reaches nobody; the
	// second names the webhook itself and is not warned about.
	if want := `"msg":"this escalation policy can notify nobody: its targets are people, and no integration in the configuration reaches a person; add a Slack, Discord or email integration","policy":"default"`; !strings.Contains(logs, want) {
		t.Errorf("logs do not contain %s:\n%s", want, logs)
	}
	if strings.Count(logs, "can notify nobody") != 1 {
		t.Errorf("the policy that names the webhook is warned about:\n%s", logs)
	}
	// Every type of integration delivers, and the address of a webhook,
	// which may hold a token, is logged by its host alone.
	if strings.Contains(logs, "does not deliver") || !strings.Contains(logs, `"msg":"integration configured","integration":"tickets","type":"webhook","host":"tickets.example.com"`) {
		t.Errorf("logs:\n%s", logs)
	}
	assertNoSecrets(t, "the logs", logs, "query-token-marker-19de", "hook-signing-secret-marker-4d1a")
}
