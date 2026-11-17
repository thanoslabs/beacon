package e2e_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

// The credentials the tests give Beacon. None may appear in its output.
const (
	sourceToken   = "e2e-source-token-marker-a41c7f"
	botToken      = "xoxb-e2e-bot-token-marker-5d20"
	signingSecret = "e2e-signing-secret-marker-9be3"
)

var (
	buildOnce sync.Once
	binary    string
	errBuild  error
)

// build compiles the beacon binary once for all tests.
func build(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "beacon-e2e-")
		if err != nil {
			errBuild = err
			return
		}
		binary = filepath.Join(dir, "beacon")
		out, err := exec.Command("go", "build", "-o", binary, "github.com/thanoslabs/beacon/cmd/beacon").CombinedOutput()
		if err != nil {
			errBuild = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if errBuild != nil {
		t.Fatalf("build beacon: %v", errBuild)
	}
	return binary
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binary != "" {
		_ = os.RemoveAll(filepath.Dir(binary))
	}
	os.Exit(code)
}

// configuration is what the tests provision: one webhook source, one Slack
// integration, and a default escalation policy whose first step names the
// channel that is told about every alert. Secrets are references, as they
// must be; the values are in the process environment.
const configuration = `sources:
  - name: e2e
    type: webhook
    token: {env: BEACON_SOURCE_TOKEN}
integrations:
  - name: slack
    type: slack
    slack:
      bot_token: {env: BEACON_SLACK_BOT_TOKEN}
      signing_secret: {file: signing-secret}
escalation_policies:
  - name: default
    steps:
      - notify:
          - channel: C0ALERTS
            integration: slack
        timeout: 5m
routing:
  default:
    escalation_policy: default
`

// writeConfig writes the configuration, and the secret it reads from a file,
// into a new directory and returns the directory.
func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"beacon.yaml": configuration, "signing-secret": signingSecret + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// beacon is a running beacon serve process and the things around it.
type beacon struct {
	t     *testing.T
	url   string
	slack *slacktest.Server
	db    *pgxpool.Pool
	cmd   *exec.Cmd

	mu   sync.Mutex
	logs strings.Builder
	done chan struct{}
}

// start runs the binary against a new database and a new fake Slack, and
// waits until it serves.
func start(t *testing.T) *beacon {
	t.Helper()
	// First, so that the test is skipped before anything is built when there
	// is no database.
	dbURL := postgrestest.NewURL(t)
	bin := build(t)

	b := &beacon{t: t, slack: slacktest.New(t, botToken), done: make(chan struct{})}
	b.cmd = exec.Command(bin, "serve")
	b.cmd.Env = []string{
		"BEACON_DATABASE_URL=" + dbURL,
		"BEACON_LISTEN_ADDRESS=127.0.0.1:0",
		"BEACON_CONFIG=" + writeConfig(t),
		"BEACON_SOURCE_TOKEN=" + sourceToken,
		"BEACON_SLACK_BOT_TOKEN=" + botToken,
		"BEACON_SLACK_API_URL=" + b.slack.URL,
	}
	stderr, err := b.cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := b.cmd.Start(); err != nil {
		t.Fatalf("start beacon: %v", err)
	}
	t.Cleanup(func() {
		if b.cmd.ProcessState == nil {
			_ = b.cmd.Process.Kill()
			<-b.done
			_ = b.cmd.Wait()
		}
		if t.Failed() {
			t.Logf("beacon logs:\n%s", b.output())
		}
	})

	// The address is in the "http server listening" log record.
	addr := make(chan string, 1)
	go func() {
		defer close(b.done)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			b.mu.Lock()
			b.logs.WriteString(line + "\n")
			b.mu.Unlock()
			var rec struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "http server listening" {
				addr <- rec.Addr
			}
		}
	}()
	select {
	case a := <-addr:
		b.url = "http://" + a
	case <-b.done:
		t.Fatalf("beacon exited before it listened:\n%s", b.output())
	case <-time.After(30 * time.Second):
		t.Fatalf("beacon did not start listening:\n%s", b.output())
	}

	// The database is read to check what Beacon stored; nothing is written.
	b.db, err = postgres.Open(context.Background(), postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(b.db.Close)
	return b
}

func (b *beacon) output() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logs.String()
}

// terminate sends SIGTERM, as an orchestrator would, and returns the exit
// code.
func (b *beacon) terminate() int {
	b.t.Helper()
	if err := b.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		b.t.Fatalf("signal beacon: %v", err)
	}
	select {
	case <-b.done:
	case <-time.After(30 * time.Second):
		b.t.Fatalf("beacon did not stop after SIGTERM:\n%s", b.output())
	}
	err := b.cmd.Wait()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		b.t.Fatalf("wait for beacon: %v", err)
	}
	return b.cmd.ProcessState.ExitCode()
}

// webhook sends a payload to the source, as a monitoring system would, and
// returns the status and the decoded response.
func (b *beacon) webhook(payload string) (int, map[string]string) {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.url+"/api/v1/ingest/e2e", strings.NewReader(payload))
	if err != nil {
		b.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sourceToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatalf("send webhook: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		b.t.Fatalf("webhook answered %d %q, not a JSON object", resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

// click presses a button on a Slack message as a Slack user.
func (b *beacon) click(msg slacktest.Message, actionID string, user slacktest.User) int {
	b.t.Helper()
	return b.slack.Click(b.t, b.url+"/api/v1/integrations/slack/interactions", signingSecret, msg, actionID, user)
}

// message waits until the one Slack message satisfies cond and returns it.
func (b *beacon) message(what string, cond func(slacktest.Message) bool) slacktest.Message {
	b.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		msgs := b.slack.Messages()
		if len(msgs) > 1 {
			b.t.Fatalf("Slack has %d messages, want one message updated in place", len(msgs))
		}
		if len(msgs) == 1 && cond(msgs[0]) {
			return msgs[0]
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s; Slack has: %+v", what, msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// status reads the state of an alert from the database.
func (b *beacon) status(alertID string) string {
	b.t.Helper()
	var status string
	if err := b.db.QueryRow(context.Background(), `SELECT status FROM alerts WHERE id = $1`, alertID).Scan(&status); err != nil {
		b.t.Fatalf("read alert: %v", err)
	}
	return status
}

// timeline reads the alert's timeline as "type by actor" lines, waiting until
// it has the wanted number of events: the last notification is recorded just
// after Slack receives it.
func (b *beacon) timeline(alertID string, want int) []string {
	b.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		rows, err := b.db.Query(context.Background(), `
			SELECT e.type || ' by ' || e.actor_type || COALESCE(' ' || e.actor_provider || ':' || e.actor_external_id, '')
			FROM alert_events e JOIN alerts a ON a.group_id = e.alert_group_id
			WHERE a.id = $1 ORDER BY e.id`, alertID)
		if err != nil {
			b.t.Fatalf("read timeline: %v", err)
		}
		events, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			b.t.Fatalf("read timeline: %v", err)
		}
		if len(events) >= want || time.Now().After(deadline) {
			return events
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const diskFull = `{
	"labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"},
	"annotations": {"summary": "Disk is full on db1", "runbook_url": "https://runbooks.example.com/disk-full"}
}`

const diskFullResolved = `{"status": "resolved", "labels": {"alertname": "DiskFull", "host": "db1", "severity": "critical"}}`

func contains(substrings ...string) func(slacktest.Message) bool {
	return func(m slacktest.Message) bool {
		for _, s := range substrings {
			if !strings.Contains(m.JSON(), s) {
				return false
			}
		}
		return true
	}
}

// TestFirstPath is the first complete path through Beacon: an alert arrives
// by webhook, a message goes to Slack, someone acknowledges it from Slack,
// and the source resolves it.
func TestFirstPath(t *testing.T) {
	b := start(t)
	alice := slacktest.User{ID: "U0ALICE", Username: "alice"}

	// 1. The source reports the alert.
	code, res := b.webhook(diskFull)
	if code != http.StatusCreated || res["status"] != "firing" {
		t.Fatalf("webhook = %d %v, want 201 and a firing alert", code, res)
	}
	alertID := res["alert_id"]

	// 2. Slack receives one message about it, with the buttons.
	msg := b.message("the alert in Slack", contains("*Firing* · critical", "Disk is full on db1", "beacon_acknowledge"))
	firing := msg
	if msg.Channel != "C0ALERTS" || msg.Text != "[FIRING] critical: Disk is full on db1" {
		t.Errorf("message = %+v", msg)
	}
	for _, want := range []string{"`alertname=DiskFull` `host=db1` `severity=critical`", "https://runbooks.example.com/disk-full|:book: Runbook", "beacon_snooze", "beacon_resolve"} {
		if !strings.Contains(msg.JSON(), want) {
			t.Errorf("the message does not contain %q:\n%s", want, msg.JSON())
		}
	}

	// Slack has the message a moment before Beacon has recorded that it was
	// delivered. The press must come after that, as a person's would, or
	// the two events can reach the timeline in either order.
	b.timeline(alertID, 2)

	// 3. Someone acknowledges it from Slack.
	if code := b.click(msg, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("acknowledge from Slack = %d, want 200", code)
	}
	if got := b.status(alertID); got != "acknowledged" {
		t.Fatalf("alert is %s after the acknowledgement, want acknowledged", got)
	}
	msg = b.message("the acknowledgement in Slack", contains("*Acknowledged*", "Acknowledged by alice"))
	if strings.Contains(msg.JSON(), "beacon_acknowledge") || !strings.Contains(msg.JSON(), "beacon_resolve") {
		t.Errorf("the acknowledged message should offer resolve but not acknowledge:\n%s", msg.JSON())
	}

	// Someone else, still looking at the old message, presses acknowledge
	// too. Nothing changes, and Slack is given a note for that person.
	bob := slacktest.User{ID: "U0BOB", Username: "bob"}
	stale := slacktest.Message{Channel: msg.Channel, TS: msg.TS, Blocks: firing.Blocks}
	if code := b.click(stale, "beacon_acknowledge", bob); code != http.StatusOK {
		t.Fatalf("second acknowledge from Slack = %d, want 200", code)
	}
	if notes := b.slack.Responses(); len(notes) != 1 || notes[0]["text"] != "Nothing changed: this alert is already acknowledged." {
		t.Errorf("answer to the second acknowledge = %v", notes)
	}

	// 4. The source reports that the problem is over, once the delivery of
	// the acknowledgement is on record too.
	b.timeline(alertID, 4)
	code, res = b.webhook(diskFullResolved)
	if code != http.StatusOK || res["result"] != "resolved" || res["alert_id"] != alertID {
		t.Fatalf("resolve webhook = %d %v, want the alert resolved", code, res)
	}
	msg = b.message("the resolution in Slack", contains("*Resolved*", "Resolved by the source"))
	if msg.Updates != 2 || msg.Text != "[RESOLVED] critical: Disk is full on db1" || strings.Contains(msg.JSON(), `"actions"`) {
		t.Errorf("final message: %d updates, text %q, want two updates and no buttons:\n%s", msg.Updates, msg.Text, msg.JSON())
	}

	// 5. The final state and the whole story are in the database.
	if got := b.status(alertID); got != "resolved" {
		t.Errorf("final state = %s, want resolved", got)
	}
	want := []string{
		"fired by source",
		"notification_sent by system",
		"acknowledged by user slack:U0ALICE",
		"notification_sent by system",
		"resolved by source",
		"notification_sent by system",
	}
	if got := b.timeline(alertID, len(want)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("timeline:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if n := len(b.slack.Calls("chat.postMessage")); n != 1 {
		t.Errorf("%d messages were posted, want 1", n)
	}

	// 6. The process stops cleanly and never printed a credential.
	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	logs := b.output()
	for _, secret := range []string{sourceToken, botToken, signingSecret} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain the credential %q", secret)
		}
	}
	if !strings.Contains(logs, "beacon stopped") {
		t.Errorf("the logs do not show a clean stop")
	}
}

// TestSourceResolvesAnAlertNobodyTook covers a resolve sent by the source
// while the alert is still firing, and a late button press on the message.
func TestSourceResolvesAnAlertNobodyTook(t *testing.T) {
	b := start(t)

	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	firing := b.message("the alert in Slack", contains("*Firing*"))

	// The source repeats the alert, as monitoring systems do. Nothing new is
	// posted and the alert is the same one.
	if code, res := b.webhook(diskFull); code != http.StatusOK || res["result"] != "updated" || res["alert_id"] != alertID {
		t.Fatalf("repeated webhook = %d %v, want the same alert updated", code, res)
	}

	if code, res := b.webhook(diskFullResolved); code != http.StatusOK || res["result"] != "resolved" {
		t.Fatalf("resolve webhook = %d %v", code, res)
	}
	resolved := b.message("the resolution in Slack", contains("*Resolved*", "Resolved by the source"))
	if resolved.Updates != 1 || strings.Contains(resolved.JSON(), `"actions"`) {
		t.Errorf("resolved message: %d updates, want one and no buttons:\n%s", resolved.Updates, resolved.JSON())
	}
	if got := b.status(alertID); got != "resolved" {
		t.Errorf("final state = %s, want resolved", got)
	}

	// Someone who still sees the old message presses acknowledge.
	bob := slacktest.User{ID: "U0BOB", Username: "bob"}
	if code := b.click(firing, "beacon_acknowledge", bob); code != http.StatusOK {
		t.Fatalf("late acknowledge = %d, want 200", code)
	}
	responses := b.slack.Responses()
	if len(responses) != 1 || responses[0]["text"] != "Nothing changed: this alert is already resolved." {
		t.Errorf("answer to the late acknowledge = %v", responses)
	}
	want := []string{"fired by source", "notification_sent by system", "resolved by source", "notification_sent by system"}
	if got := b.timeline(alertID, len(want)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("timeline:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// A source that sends the resolve again is answered without an error,
	// and the problem coming back opens a new alert with a new message.
	if code, res := b.webhook(diskFullResolved); code != http.StatusOK || res["result"] != "not_found" {
		t.Errorf("second resolve = %d %v, want 200 not_found", code, res)
	}
	code, res = b.webhook(diskFull)
	if code != http.StatusCreated || res["alert_id"] == alertID {
		t.Fatalf("firing again = %d %v, want a new alert", code, res)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(b.slack.Messages()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("Slack has %d messages, want a second one for the new alert", len(b.slack.Messages()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
