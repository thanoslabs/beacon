package e2e_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	"golang.org/x/crypto/bcrypt"

	"github.com/thanoslabs/beacon/internal/auth/oidctest"
	"github.com/thanoslabs/beacon/internal/discord/discordtest"
	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

// The credentials the tests give Beacon. None may appear in its output.
const (
	sourceToken   = "e2e-source-token-marker-a41c7f"
	botToken      = "xoxb-e2e-bot-token-marker-5d20"
	signingSecret = "e2e-signing-secret-marker-9be3"
	discordToken  = "e2e-discord-bot-token-marker-c7d1"
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
// channel that is told about every alert group. A group is told about at
// once and may be updated every second, so the tests need not wait long. Secrets are references, as they
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
  group_wait: 0s
  group_interval: 1s
  default:
    escalation_policy: default
`

// writeConfig writes a configuration, and the secret it reads from a file,
// into a new directory and returns the directory.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"beacon.yaml": yaml, "signing-secret": signingSecret + "\n"} {
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
	// discord is the fake Discord, for the configurations that have a
	// Discord integration.
	discord *discordtest.Server
	db      *pgxpool.Pool
	cmd     *exec.Cmd

	mu   sync.Mutex
	logs strings.Builder
	done chan struct{}
	addr chan string
	// dir holds the configuration the process was started with.
	dir string
}

// start runs the binary against a new database and a new fake Slack, and
// waits until it serves.
func start(t *testing.T) *beacon {
	t.Helper()
	// First, so that the test is skipped before anything is built when there
	// is no database.
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, configuration))
	b.waitUntilServing()
	return b
}

// startSignedIn runs the binary with sign-in through Dex configured, signs
// Ada in, and waits until Beacon has linked the Slack account that has her
// email address. It returns that Slack account and the name Ada has in
// Beacon. Acknowledging from Slack needs such a person: an account that is
// linked to no Beacon user may only resolve.
func startSignedIn(t *testing.T) (b *beacon, ada slacktest.User, name string) {
	t.Helper()
	dbURL := postgrestest.NewURL(t)
	b = launch(t, dbURL, writeConfig(t, signInConfiguration(t)))
	b.waitUntilServing()
	b.slack.AddUser(oidctest.Ada, "U0ADA")
	if code, body := oidctest.SignIn(t, oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), oidctest.Ada, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	b.waitFor("Ada's Slack account to be linked", func() bool {
		err := b.db.QueryRow(context.Background(), `SELECT COALESCE(NULLIF(u.display_name, ''), u.email) FROM user_identities i JOIN users u ON u.id = i.user_id
			WHERE i.external_id = 'U0ADA'`).Scan(&name)
		return err == nil
	})
	return b, slacktest.User{ID: "U0ADA", Username: "ada.in.slack"}, name
}

// launch starts the binary against the database at dbURL with the
// configuration in dir, and returns without waiting for it to serve.
func launch(t *testing.T, dbURL, dir string) *beacon {
	t.Helper()
	bin := build(t)

	b := &beacon{t: t, slack: slacktest.New(t, botToken), discord: discordtest.New(t, discordToken), done: make(chan struct{}), addr: make(chan string, 1), dir: dir}
	b.cmd = exec.Command(bin, "serve")
	b.cmd.Env = []string{
		"BEACON_DATABASE_URL=" + dbURL,
		"BEACON_LISTEN_ADDRESS=127.0.0.1:0",
		"BEACON_CONFIG=" + dir,
		"BEACON_SOURCE_TOKEN=" + sourceToken,
		"BEACON_SLACK_BOT_TOKEN=" + botToken,
		"BEACON_SLACK_API_URL=" + b.slack.URL,
		"BEACON_DISCORD_BOT_TOKEN=" + discordToken,
		"BEACON_DISCORD_API_URL=" + b.discord.URL,
		"BEACON_OIDC_CLIENT_SECRET=" + oidctest.ClientSecret,
		"BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH=" + bootstrapHash(t),
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
				b.addr <- rec.Addr
			}
		}
	}()

	// The database is read to check what Beacon stored; nothing is written.
	b.db, err = postgres.Open(context.Background(), postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(b.db.Close)
	return b
}

// waitUntilServing waits until the process listens.
func (b *beacon) waitUntilServing() {
	b.t.Helper()
	select {
	case a := <-b.addr:
		b.url = "http://" + a
	case <-b.done:
		b.t.Fatalf("beacon exited before it listened:\n%s", b.output())
	case <-time.After(30 * time.Second):
		b.t.Fatalf("beacon did not start listening:\n%s", b.output())
	}
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
	return b.webhookTo("e2e", payload)
}

// webhookTo is webhook for another source with the same token.
func (b *beacon) webhookTo(source, payload string) (int, map[string]string) {
	b.t.Helper()
	var out map[string]string
	return b.deliver(source, payload, &out), out
}

// deliver sends a payload to a source and decodes the response into out.
func (b *beacon) deliver(source, payload string, out any) int {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.url+"/api/v1/ingest/"+source, strings.NewReader(payload))
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
	if err := json.Unmarshal(raw, out); err != nil {
		b.t.Fatalf("webhook answered %d %q, not the expected JSON", resp.StatusCode, raw)
	}
	return resp.StatusCode
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
	// Acknowledging from Slack needs a Beacon user, so the path begins with
	// one: Ada has signed in, and her Slack account is linked.
	b, alice, name := startSignedIn(t)

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
	b.timeline(alertID, 3)

	// 3. Someone acknowledges it from Slack.
	if code := b.click(msg, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("acknowledge from Slack = %d, want 200", code)
	}
	if got := b.status(alertID); got != "acknowledged" {
		t.Fatalf("alert is %s after the acknowledgement, want acknowledged", got)
	}
	msg = b.message("the acknowledgement in Slack", contains("*Acknowledged*", "Acknowledged by "+name))
	if strings.Contains(msg.JSON(), "beacon_acknowledge") || !strings.Contains(msg.JSON(), "beacon_resolve") {
		t.Errorf("the acknowledged message should offer resolve but not acknowledge:\n%s", msg.JSON())
	}

	// Still looking at the old message, she presses acknowledge again.
	// Nothing changes, and Slack is given a note for her.
	stale := slacktest.Message{Channel: msg.Channel, TS: msg.TS, Blocks: firing.Blocks}
	if code := b.click(stale, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("second acknowledge from Slack = %d, want 200", code)
	}
	if notes := b.slack.Responses(); len(notes) != 1 || notes[0]["text"] != "Nothing changed: this alert is already acknowledged." {
		t.Errorf("answer to the second acknowledge = %v", notes)
	}
	// Bob is in the channel and is no Beacon user. He may not snooze: he is
	// told why, and how to get connected, and nothing changes.
	bob := slacktest.User{ID: "U0BOB", Username: "bob"}
	if code := b.click(msg, "beacon_snooze", bob); code != http.StatusOK {
		t.Fatalf("snooze by an account that is not linked = %d, want 200", code)
	}
	notes := b.slack.Responses()
	if text, _ := notes[len(notes)-1]["text"].(string); len(notes) != 2 || !strings.Contains(text, "only Beacon users can snooze an alert") ||
		!strings.Contains(text, "Sign in to Beacon at "+oidctest.PublicURL+" with the same email address as your Slack account") {
		t.Errorf("answer to the snooze by an account that is not linked = %v", notes)
	}
	if got := b.status(alertID); got != "acknowledged" {
		t.Errorf("alert is %s after the refused snooze, want acknowledged", got)
	}

	// 4. The source reports that the problem is over, once the delivery of
	// the acknowledgement is on record too.
	b.timeline(alertID, 5)
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
		"escalated by system",
		"notification_sent by system",
		"acknowledged by user slack:U0ADA",
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
	for _, secret := range []string{sourceToken, botToken, signingSecret, oidctest.ClientSecret} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain the credential %q", secret)
		}
	}
	if !strings.Contains(logs, "beacon stopped") {
		t.Errorf("the logs do not show a clean stop")
	}
}

// TestSlackWithoutSignIn is what Slack can do in an install that has no
// identity provider, such as one started from the Slack example. The alert
// arrives in Slack. Nobody can acknowledge or snooze it from there, because
// nobody is a Beacon user: the press is answered with a note and changes
// nothing, and the log said so at the start. Anybody can resolve it.
func TestSlackWithoutSignIn(t *testing.T) {
	b := start(t)
	if !strings.Contains(b.output(), `"level":"WARN","msg":"nobody can acknowledge or snooze from Slack`) {
		t.Errorf("the logs do not warn that nobody can acknowledge from Slack")
	}
	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	msg := b.message("the alert in Slack", contains("*Firing*", "beacon_acknowledge"))
	b.timeline(alertID, 3)

	alice := slacktest.User{ID: "U0ALICE", Username: "alice"}
	for i, button := range []string{"beacon_acknowledge", "beacon_snooze"} {
		if code := b.click(msg, button, alice); code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", button, code)
		}
		notes := b.slack.Responses()
		if text, _ := notes[len(notes)-1]["text"].(string); len(notes) != i+1 || !strings.Contains(text, "only Beacon users can") ||
			!strings.Contains(text, "ask whoever runs Beacon for its address") || !strings.Contains(text, "You can still resolve the alert from here.") {
			t.Errorf("answer to %s = %v", button, notes)
		}
		if got := b.status(alertID); got != "firing" {
			t.Fatalf("the alert is %s after %s by an account that is not linked, want firing", got, button)
		}
	}
	if code := b.click(msg, "beacon_resolve", alice); code != http.StatusOK {
		t.Fatalf("resolve = %d, want 200", code)
	}
	if got := b.status(alertID); got != "resolved" {
		t.Fatalf("the alert is %s after the resolve, want resolved", got)
	}
	// The fake workspace has no profile for her, so her handle is used.
	b.message("the resolution in Slack", contains("*Resolved*", "Resolved by alice (Slack not connected)"))
	want := []string{"fired by source", "escalated by system", "notification_sent by system", "resolved by user slack:U0ALICE", "notification_sent by system"}
	if got := b.timeline(alertID, len(want)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("timeline:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
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
	// Bob is no Beacon user, so he is told that before anything else.
	if text, _ := responses[0]["text"].(string); len(responses) != 1 || !strings.Contains(text, "Nothing changed: only Beacon users can acknowledge an alert") {
		t.Errorf("answer to the late acknowledge = %v", responses)
	}
	want := []string{"fired by source", "escalated by system", "notification_sent by system", "resolved by source", "notification_sent by system"}
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

// waitForMessages waits until Slack has n messages that all satisfy cond, and
// returns them.
func (b *beacon) waitForMessages(what string, n int, cond func(slacktest.Message) bool) []slacktest.Message {
	b.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		msgs := b.slack.Messages()
		ok := len(msgs) == n
		for _, m := range msgs {
			ok = ok && cond(m)
		}
		if ok {
			return msgs
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s; Slack has: %+v", what, msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// captured reads a payload that a real sender produced, from the fixtures of
// the ingest package.
func captured(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "ingest", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestAlertmanagerSource delivers what a real Alertmanager sent about a group
// of two alerts: firing, repeated, one resolved, both resolved.
func TestAlertmanagerSource(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, strings.Replace(configuration, "type: webhook", "type: alertmanager", 1)))
	b.waitUntilServing()
	deliver := func(fixture string) map[string]int {
		t.Helper()
		var res map[string]any
		if code := b.deliver("e2e", captured(t, fixture), &res); code != http.StatusOK || res["result"] != "accepted" {
			t.Fatalf("%s = %d %v, want 200 accepted", fixture, code, res)
		}
		counts := map[string]int{}
		for k, v := range res {
			if n, ok := v.(float64); ok && n != 0 {
				counts[k] = int(n)
			}
		}
		return counts
	}

	if got := deliver("alertmanager/firing.json"); got["received"] != 2 || got["created"] != 2 {
		t.Fatalf("firing = %v, want two alerts created", got)
	}
	// Both alerts are DiskFull, so they are one group and one message.
	msg := b.message("the group in Slack", contains("*Firing* · critical", "2 alerts: 2 firing", "Disk is full on db1", "Disk is filling up on db2"))
	if msg.Text != "[FIRING] critical: DiskFull (2 alerts)" || !strings.Contains(msg.JSON(), "`instance=db1.example.com:9100`") {
		t.Errorf("message = %s", msg.JSON())
	}

	// The repeat that Alertmanager sends every repeat_interval changes
	// nothing and tells nobody.
	if got := deliver("alertmanager/firing.json"); got["updated"] != 2 || got["created"] != 0 {
		t.Fatalf("repeated firing = %v, want two alerts updated", got)
	}
	if got := deliver("alertmanager/some-resolved.json"); got["updated"] != 1 || got["resolved"] != 1 {
		t.Fatalf("one resolved = %v", got)
	}
	b.message("the partly resolved group in Slack", contains("*Firing* · critical", "2 alerts: 1 firing, 1 resolved"))
	if got := deliver("alertmanager/resolved.json"); got["resolved"] != 1 {
		t.Fatalf("all resolved = %v", got)
	}
	msg = b.message("the resolved group in Slack", contains("*Resolved*", "2 alerts: 2 resolved", "Resolved by the source"))
	if strings.Contains(msg.JSON(), `"actions"`) {
		t.Errorf("the resolved message still has buttons:\n%s", msg.JSON())
	}

	var alerts, groups int
	err := b.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM alerts WHERE status = 'resolved'), (SELECT count(*) FROM alert_groups WHERE status = 'resolved')`).Scan(&alerts, &groups)
	if err != nil || alerts != 2 || groups != 1 {
		t.Errorf("%d resolved alerts in %d resolved groups, %v; want 2 in 1", alerts, groups, err)
	}
	if n := len(b.slack.Calls("chat.postMessage")); n != 1 {
		t.Errorf("%d messages were posted, want 1", n)
	}
	if strings.Contains(b.output(), sourceToken) {
		t.Errorf("the logs contain the source token")
	}
}

// TestGrafanaSource delivers what a real Grafana sent about an alert of a
// rule that is linked to a dashboard panel: firing, then resolved.
func TestGrafanaSource(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, strings.Replace(configuration, "type: webhook", "type: grafana", 1)))
	b.waitUntilServing()

	var res map[string]any
	if code := b.deliver("e2e", captured(t, "grafana/firing.json"), &res); code != http.StatusOK || res["created"] != float64(1) {
		t.Fatalf("firing = %d %v, want one alert created", code, res)
	}
	msg := b.message("the alert in Slack", contains("*Firing* · warning", "Latency is high on api1"))
	for _, want := range []string{
		"http://localhost:13000/alerting/grafana/beacon-demo-latency/view?orgId=1|:mag: Source",
		"|:bar_chart: Dashboard", "viewPanel=1|:chart_with_upwards_trend: Panel", "|:no_bell: Silence at the source",
		"`grafana_folder=Beacon`",
	} {
		if !strings.Contains(msg.JSON(), want) {
			t.Errorf("the message does not contain %q:\n%s", want, msg.JSON())
		}
	}

	if code := b.deliver("e2e", captured(t, "grafana/resolved.json"), &res); code != http.StatusOK || res["resolved"] != float64(1) {
		t.Fatalf("resolved = %d %v, want one alert resolved", code, res)
	}
	b.message("the resolution in Slack", contains("*Resolved*", "Resolved by the source", "|:bar_chart: Dashboard"))
}

// threeSources is the configuration with a source of each type. They share
// one token.
var threeSources = strings.Replace(configuration, "sources:\n", `sources:
  - name: am
    type: alertmanager
    token: {env: BEACON_SOURCE_TOKEN}
  - name: grafana
    type: grafana
    token: {env: BEACON_SOURCE_TOKEN}
`, 1)

// TestConcurrentDeliveries sends the same notification to each type of
// source from several clients at once, over real connections, and then
// resolves it the same way. Each alert exists once, is told to Slack once
// and is resolved once, however the deliveries interleave.
func TestConcurrentDeliveries(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, threeSources))
	b.waitUntilServing()
	const clients, repeats = 8, 4

	storm := func(payloads map[string]string) {
		t.Helper()
		var wg sync.WaitGroup
		for source, payload := range payloads {
			for range clients {
				wg.Go(func() {
					// Each client has its own connections, as separate senders do.
					client := &http.Client{Transport: &http.Transport{}}
					defer client.CloseIdleConnections()
					for range repeats {
						req, err := http.NewRequest(http.MethodPost, b.url+"/api/v1/ingest/"+source, strings.NewReader(payload))
						if err != nil {
							t.Error(err)
							return
						}
						req.Header.Set("Authorization", "Bearer "+sourceToken)
						resp, err := client.Do(req)
						if err != nil {
							t.Errorf("%s: %v", source, err)
							return
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
						if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
							t.Errorf("%s: status %d", source, resp.StatusCode)
						}
					}
				})
			}
		}
		wg.Wait()
	}
	count := func(query string) (n int) {
		t.Helper()
		if err := b.db.QueryRow(context.Background(), query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	storm(map[string]string{"e2e": diskFull, "am": captured(t, "alertmanager/firing.json"), "grafana": captured(t, "grafana/firing.json")})
	if alerts, firing := count(`SELECT count(*) FROM alerts`), count(`SELECT count(*) FROM alerts WHERE status = 'firing'`); alerts != 4 || firing != 4 {
		t.Fatalf("%d alerts, %d firing, after %d deliveries to each source; want the 4 distinct alerts", alerts, firing, clients*repeats)
	}
	// The three DiskFull alerts, from two sources, are one group; Grafana's
	// HighLatency is another. Groups, like alerts, are not duplicated by
	// deliveries that arrive together.
	if groups := count(`SELECT count(*) FROM alert_groups`); groups != 2 {
		t.Fatalf("%d groups, want 2", groups)
	}
	b.waitForMessages("one message per group", 2, func(m slacktest.Message) bool {
		return strings.Contains(m.JSON(), "3 alerts: 3 firing") || strings.Contains(m.JSON(), "Latency is high on api1")
	})

	storm(map[string]string{"e2e": diskFullResolved, "am": captured(t, "alertmanager/resolved.json"), "grafana": captured(t, "grafana/resolved.json")})
	// Alertmanager's last notification resolved one of its two alerts.
	if alerts, resolved := count(`SELECT count(*) FROM alerts`), count(`SELECT count(*) FROM alerts WHERE status = 'resolved'`); alerts != 4 || resolved != 3 {
		t.Fatalf("%d alerts, %d resolved, want 4 and 3", alerts, resolved)
	}
	b.waitForMessages("the resolutions in Slack", 2, func(m slacktest.Message) bool {
		return strings.Contains(m.JSON(), "3 alerts: 1 firing, 2 resolved") || strings.Contains(m.JSON(), "*Resolved*")
	})
	// Each alert fired once and resolved once, whatever was repeated.
	if got := count(`SELECT count(*) FROM alert_events WHERE type IN ('fired', 'resolved')`); got != 7 {
		t.Errorf("%d fired and resolved events, want 7", got)
	}
	if n := len(b.slack.Calls("chat.postMessage")); n != 2 {
		t.Errorf("%d messages were posted to Slack, want 2", n)
	}
}

// groupStates reads the states of the alerts with an alert name, as
// "host=state" in host order.
func (b *beacon) groupStates(alertname string) string {
	b.t.Helper()
	rows, err := b.db.Query(context.Background(), `SELECT (labels->>'host') || '=' || status FROM alerts WHERE labels->>'alertname' = $1 ORDER BY 1`, alertname)
	if err != nil {
		b.t.Fatal(err)
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		b.t.Fatal(err)
	}
	return strings.Join(states, " ")
}

// find waits for the one Slack message that satisfies cond, among others.
func (b *beacon) find(what string, cond func(slacktest.Message) bool) slacktest.Message {
	b.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var found []slacktest.Message
		for _, m := range b.slack.Messages() {
			if cond(m) {
				found = append(found, m)
			}
		}
		if len(found) == 1 {
			return found[0]
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s; Slack has: %+v", what, b.slack.Messages())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGrouping follows a group through the real binary: it forms, gains
// alerts, is acknowledged from Slack as a whole, and resolves, in one Slack
// message throughout.
func TestGrouping(t *testing.T) {
	b, alice, name := startSignedIn(t)
	fire := func(alertname, host string) {
		t.Helper()
		body := fmt.Sprintf(`{"labels": {"alertname": %q, "host": %q, "severity": "warning"}, "annotations": {"summary": %q}}`, alertname, host, alertname+" on "+host)
		if code, res := b.webhook(body); code != http.StatusCreated {
			t.Fatalf("webhook = %d %v", code, res)
		}
	}
	resolve := func(alertname, host string) {
		t.Helper()
		body := fmt.Sprintf(`{"status": "resolved", "labels": {"alertname": %q, "host": %q, "severity": "warning"}}`, alertname, host)
		if code, res := b.webhook(body); code != http.StatusOK || res["result"] != "resolved" {
			t.Fatalf("resolve webhook = %d %v", code, res)
		}
	}
	diskFull := func(substrings ...string) func(slacktest.Message) bool {
		return contains(append([]string{"DiskFull"}, substrings...)...)
	}

	// 1. One alert: the message is about that alert.
	fire("DiskFull", "db1")
	msg := b.find("the first alert in Slack", diskFull("*Firing* · warning", "*DiskFull on db1*"))
	if msg.Text != "[FIRING] warning: DiskFull on db1" {
		t.Errorf("text = %q", msg.Text)
	}

	// 2. A second alert with the same name joins: the same message now
	// shows the group. An alert with another name gets a message of its own.
	fire("DiskFull", "db2")
	fire("HighLoad", "web1")
	msg = b.find("the group in Slack", diskFull("2 alerts: 2 firing", "DiskFull on db1 `host=db1`", "DiskFull on db2 `host=db2`"))
	if msg.Text != "[FIRING] warning: DiskFull (2 alerts)" {
		t.Errorf("text = %q", msg.Text)
	}
	b.find("the other alert in Slack", contains("*HighLoad on web1*"))

	// 3. Acknowledging the group from Slack acknowledges its alerts, and
	// only its alerts.
	if code := b.click(msg, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("acknowledge = %d", code)
	}
	if got := b.groupStates("DiskFull"); got != "db1=acknowledged db2=acknowledged" {
		t.Fatalf("DiskFull alerts = %s, want both acknowledged", got)
	}
	if got := b.groupStates("HighLoad"); got != "web1=firing" {
		t.Errorf("HighLoad alerts = %s, want it untouched", got)
	}
	b.find("the acknowledgement in Slack", diskFull("*Acknowledged*", "2 alerts: 2 acknowledged", "Acknowledged by "+name))

	// 4. A third alert joins the acknowledged group. It is new and firing,
	// and the message says so; but a group is paged once, so the message is
	// edited and nobody is asked again.
	fire("DiskFull", "db3")
	msg = b.find("the new alert in the group", diskFull("*Firing*", "3 alerts: 1 firing, 2 acknowledged"))
	if !strings.Contains(msg.JSON(), "beacon_acknowledge") {
		t.Errorf("the message does not offer to acknowledge the new alert:\n%s", msg.JSON())
	}

	// 5. The source resolves them one by one; the message follows, and ends
	// resolved without buttons.
	resolve("DiskFull", "db3")
	b.find("one resolution in Slack", diskFull("*Acknowledged*", "3 alerts: 2 acknowledged, 1 resolved"))
	resolve("DiskFull", "db1")
	resolve("DiskFull", "db2")
	msg = b.find("the resolved group in Slack", diskFull("*Resolved*", "3 alerts: 3 resolved"))
	if strings.Contains(msg.JSON(), `"actions"`) {
		t.Errorf("the resolved message still has buttons:\n%s", msg.JSON())
	}

	// 6. Two groups, two messages, however many alerts and changes: the
	// alert that joined the acknowledged group paged nobody.
	if n := len(b.slack.Calls("chat.postMessage")); n != 2 {
		t.Errorf("%d messages were posted, want 2", n)
	}
	var pages, restarted int
	if err := b.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM notifications WHERE reason = 'escalation_step'),
		(SELECT count(*) FROM alert_events WHERE type = 'escalated')`).Scan(&pages, &restarted); err != nil || pages != 2 || restarted != 2 {
		t.Errorf("%d pages and %d escalation steps, %v; want one of each per group", pages, restarted, err)
	}
	var open, resolved int
	if err := b.db.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE status = 'open'), count(*) FILTER (WHERE status = 'resolved') FROM alert_groups`).Scan(&open, &resolved); err != nil || open != 1 || resolved != 1 {
		t.Errorf("%d open and %d resolved groups, %v; want 1 and 1", open, resolved, err)
	}

	// 7. The same problem again is a new group with a new message.
	fire("DiskFull", "db1")
	b.find("the new group in Slack", diskFull("*Firing*", "*DiskFull on db1*"))
	if n := len(b.slack.Calls("chat.postMessage")); n != 3 {
		t.Errorf("%d messages were posted, want 3", n)
	}
}

// routingConfiguration routes by two rules and a default, each to a channel
// of its own. The policy "urgent" also names a person and a second step,
// which do nothing until the escalation engine exists.
const routingConfiguration = `sources:
  - name: e2e
    type: webhook
    token: {env: BEACON_SOURCE_TOKEN}
    severity:
      label: priority
      mapping: {P1: critical, P2: warning}
      default: info
integrations:
  - name: slack
    type: slack
    slack:
      bot_token: {env: BEACON_SLACK_BOT_TOKEN}
      signing_secret: {file: signing-secret}
teams:
  - name: dba
  - name: platform
escalation_policies:
  - name: default
    steps:
      - notify: [{channel: C0ALERTS, integration: slack}]
        timeout: 5m
  - name: dba
    steps:
      - notify: [{channel: C0DBA, integration: slack}]
        timeout: 5m
  - name: urgent
    repeat: 1
    steps:
      - notify:
          - user: ada@example.com
          - channel: C0URGENT
            integration: slack
        timeout: 5m
      - notify: [{team: platform}]
        timeout: 5m
routing:
  group_wait: 0s
  group_interval: 1s
  default:
    team: platform
    escalation_policy: default
  rules:
    - name: database
      match: {team: dba}
      team: dba
      service: postgres
      escalation_policy: dba
    - name: pages
      match_re: {priority: "P1|page"}
      escalation_policy: urgent
`

// inChannel matches a message in a channel, or a direct conversation, that
// contains every one of the substrings.
func inChannel(channel string, substrings ...string) func(slacktest.Message) bool {
	all := contains(substrings...)
	return func(m slacktest.Message) bool { return m.Channel == channel && all(m) }
}

// TestRouting sends alerts that match the first rule, a later rule and no
// rule through the real binary, and follows each to its channel.
func TestRouting(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, routingConfiguration))
	b.slack.AddChannel("dba", "C0DBA")
	b.slack.AddChannel("urgent", "C0URGENT")
	b.waitUntilServing()
	fire := func(labels string) string {
		t.Helper()
		code, res := b.webhook(`{"labels": ` + labels + `}`)
		if code != http.StatusCreated {
			t.Fatalf("webhook = %d %v", code, res)
		}
		return res["alert_id"]
	}
	// routed reads what routing stored for an alert.
	routed := func(alertID string) string {
		t.Helper()
		var got string
		err := b.db.QueryRow(context.Background(), `
			SELECT 'rule=' || COALESCE(r.name, '-') || ' team=' || COALESCE(t.name, '-') || ' service=' || COALESCE(a.service, '-') ||
				' policy=' || COALESCE(p.name, '-') || ' severity=' || a.severity
			FROM alerts a JOIN alert_groups g ON g.id = a.group_id
			LEFT JOIN routing_rules r ON r.id = a.routing_rule_id LEFT JOIN teams t ON t.id = a.team_id
			LEFT JOIN escalation_policies p ON p.id = g.escalation_policy_id WHERE a.id = $1`, alertID).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// 1. The first rule. The second would match too; the first one wins.
	first := fire(`{"alertname": "ReplicationLag", "team": "dba", "priority": "P1", "service": "from-the-alert"}`)
	if got, want := routed(first), "rule=database team=dba service=postgres policy=dba severity=critical"; got != want {
		t.Errorf("first rule: %s, want %s", got, want)
	}
	b.find("the database alert in its channel", inChannel("C0DBA", "ReplicationLag"))

	// 2. A later rule, by a regular expression.
	second := fire(`{"alertname": "CheckoutDown", "priority": "page", "service": "checkout"}`)
	if got, want := routed(second), "rule=pages team=- service=checkout policy=urgent severity=info"; got != want {
		t.Errorf("later rule: %s, want %s", got, want)
	}
	b.find("the page in its channel", inChannel("C0URGENT", "CheckoutDown"))

	// 3. No rule: the default. The pattern is anchored, so P10 is not P1.
	third := fire(`{"alertname": "DiskFilling", "priority": "P10"}`)
	if got, want := routed(third), "rule=- team=platform service=- policy=default severity=info"; got != want {
		t.Errorf("default: %s, want %s", got, want)
	}
	b.find("the unmatched alert in the default channel", inChannel("C0ALERTS", "DiskFilling"))
	if got, want := b.metrics("beacon_routing_unmatched_alerts_total"), `beacon_routing_unmatched_alerts_total{source="e2e"} 1`; got != want {
		t.Errorf("metric:\n%s\nwant:\n%s", got, want)
	}

	// Each alert went to one channel. The person in the first step of
	// "urgent" has never signed in, so that target was skipped: the step on
	// the timeline and the log say so.
	if n := len(b.slack.Calls("chat.postMessage")); n != 3 {
		t.Errorf("%d messages were posted, want 3", n)
	}
	var destinations string
	if err := b.db.QueryRow(context.Background(), `SELECT string_agg(destination, ',' ORDER BY destination) FROM notifications`).Scan(&destinations); err != nil || destinations != "C0ALERTS,C0DBA,C0URGENT" {
		t.Errorf("notifications went to %q, %v", destinations, err)
	}
	var skipped string
	err := b.db.QueryRow(context.Background(), `SELECT e.data->'skipped'->0->>'target' || ': ' || (e.data->'skipped'->0->>'reason')
		FROM alert_events e WHERE e.type = 'escalated' AND e.data->>'policy' = 'urgent'`).Scan(&skipped)
	if err != nil || skipped != "user ada@example.com: no user has this email address" {
		t.Errorf("the step of the urgent policy skipped %q, %v; want the person who is not a user", skipped, err)
	}
	if !strings.Contains(b.output(), `"msg":"escalation target not notified"`) {
		t.Errorf("the log does not say that a target was not notified")
	}

	// 4. A reload changes the rules for alerts that arrive afterwards. The
	// alert that is open keeps the route it was given.
	b.reload(strings.Replace(routingConfiguration, "match: {team: dba}", "match: {team: storage}", 1), 1)
	fourth := fire(`{"alertname": "SlowQueries", "team": "dba", "priority": "P2"}`)
	if got, want := routed(fourth), "rule=- team=platform service=- policy=default severity=warning"; got != want {
		t.Errorf("after the reload: %s, want %s", got, want)
	}
	b.find("the alert in the default channel after the reload", inChannel("C0ALERTS", "SlowQueries"))
	if got, want := routed(first), "rule=database team=dba service=postgres policy=dba severity=critical"; got != want {
		t.Errorf("the open alert after the reload: %s, want %s", got, want)
	}
	if got, want := b.metrics("beacon_routing_unmatched_alerts_total"), `beacon_routing_unmatched_alerts_total{source="e2e"} 2`; got != want {
		t.Errorf("metric after the reload: %s", got)
	}

	// 5. A configuration whose rule has a broken pattern is rejected whole,
	// and names the field.
	logs := b.reload(strings.Replace(routingConfiguration, `"P1|page"`, `"P1|(page"`, 1), 2)
	if !strings.Contains(logs, `"object":"routing rule \"pages\"","field":"match_re.priority","problem":"is not a regular expression: missing closing )`) {
		t.Errorf("the rejected pattern is not named in the logs")
	}
	if got := routed(fire(`{"alertname": "StillRouted", "priority": "page"}`)); !strings.HasPrefix(got, "rule=pages ") {
		t.Errorf("after the rejected reload: %s, want the rules that were active", got)
	}
}

// TestReloadChangesGrouping: the labels that define a group follow the
// configuration. Alerts that arrive after a reload are grouped the new way;
// the groups that exist stay as they are.
func TestReloadChangesGrouping(t *testing.T) {
	b := start(t)
	fire := func(alertname, host string) {
		t.Helper()
		if code, res := b.webhook(fmt.Sprintf(`{"labels": {"alertname": %q, "host": %q}}`, alertname, host)); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("webhook = %d %v", code, res)
		}
	}
	groups := func() string {
		t.Helper()
		rows, err := b.db.Query(context.Background(), `SELECT string_agg(a.labels->>'alertname' || '@' || (a.labels->>'host'), '+' ORDER BY a.labels->>'alertname', a.labels->>'host')
			FROM alerts a GROUP BY a.group_id ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, " | ")
	}

	fire("DiskFull", "db1")
	fire("DiskFull", "db2")
	fire("HighLoad", "db1")
	if got, want := groups(), "DiskFull@db1+DiskFull@db2 | HighLoad@db1"; got != want {
		t.Fatalf("groups by alertname = %s, want %s", got, want)
	}

	logs := b.reload(strings.Replace(configuration, "routing:\n", "routing:\n  group_by: [host]\n", 1), 1)
	if !strings.Contains(logs, `"msg":"alerts are routed and grouped","rules":0,"group_by":["host"]`) {
		t.Errorf("the new grouping is not in the logs")
	}
	fire("DiskFull", "db3")
	fire("HighLoad", "db3")
	fire("HighLoad", "db1")
	// db3's two alerts share a group now. HighLoad@db1 is open already and
	// stays in its group.
	if got, want := groups(), "DiskFull@db1+DiskFull@db2 | DiskFull@db3+HighLoad@db3 | HighLoad@db1"; got != want {
		t.Errorf("groups after the reload = %s, want %s", got, want)
	}
}

// TestGroupWaitSurvivesARestart stops the process while a group waits for
// its first notification. The wait is a row in Postgres, not a timer in the
// process: two new processes start on the database, and between them they
// send the notification once.
func TestGroupWaitSurvivesARestart(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	cfg := strings.Replace(configuration, "group_wait: 0s", "group_wait: 3s", 1)
	first := launch(t, dbURL, writeConfig(t, cfg))
	first.waitUntilServing()
	if code, res := first.webhook(diskFull); code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	if code := first.terminate(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	sentByFirst := len(first.slack.Messages())

	replicas := []*beacon{launch(t, dbURL, writeConfig(t, cfg)), launch(t, dbURL, writeConfig(t, cfg))}
	for _, r := range replicas {
		r.waitUntilServing()
	}
	total := func() int { return sentByFirst + len(replicas[0].slack.Messages()) + len(replicas[1].slack.Messages()) }
	deadline := time.Now().Add(30 * time.Second)
	for total() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no process sent the notification of the group that was waiting")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The notification is on record as sent, once, and nothing else is due.
	var sent, due int
	for {
		err := first.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM notifications WHERE status = 'sent'),
			(SELECT count(*) FROM notifications WHERE status = 'pending') + (SELECT count(*) FROM alert_groups WHERE notify_at IS NOT NULL)`).Scan(&sent, &due)
		if err != nil {
			t.Fatal(err)
		}
		if (sent > 0 && due == 0) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sent != 1 || due != 0 || total() != 1 {
		t.Errorf("%d notifications sent, %d due, %d messages in Slack; want 1, 0 and 1", sent, due, total())
	}
}

// rows lists the rows of a provisioned table as "name" or "name (removed)".
func (b *beacon) rows(table string) string {
	b.t.Helper()
	rows, err := b.db.Query(context.Background(), `SELECT name || CASE WHEN removed_at IS NULL THEN '' ELSE ' (removed)' END FROM `+table+` ORDER BY name`)
	if err != nil {
		b.t.Fatalf("read %s: %v", table, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		b.t.Fatalf("read %s: %v", table, err)
	}
	return strings.Join(names, ", ")
}

// TestReplicasStartTogether starts several processes at once against one
// empty database, as a deployment with replicas does. They all migrate and
// reconcile the same configuration; every object must end up with one row,
// created by exactly one of them, and every process must serve.
func TestReplicasStartTogether(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	dir := writeConfig(t, configuration)
	const replicas = 5

	var all []*beacon
	for range replicas {
		all = append(all, launch(t, dbURL, dir))
	}
	created := 0
	for _, b := range all {
		b.waitUntilServing()
		created += strings.Count(b.output(), `"msg":"provisioned object created"`)
	}
	if created != 3 {
		t.Errorf("the replicas created %d objects between them, want 3: each object once", created)
	}
	b := all[0]
	for table, want := range map[string]string{"sources": "e2e", "integrations": "slack", "escalation_policies": "default", "teams": ""} {
		if got := b.rows(table); got != want {
			t.Errorf("%s = %q, want %q", table, got, want)
		}
	}
	// Every replica serves the source, and they agree on its row: an alert
	// sent to one is updated, not duplicated, by the next.
	for i, replica := range all {
		code, res := replica.webhook(diskFull)
		if want := map[bool]int{true: http.StatusCreated, false: http.StatusOK}[i == 0]; code != want {
			t.Errorf("replica %d: webhook = %d %v, want %d", i, code, res, want)
		}
	}
}

// TestConfigurationChangesBetweenStarts edits the files between two runs:
// one object is changed, one removed and one added. The second run must
// bring the rows in line, and keep the removed source for the alert that
// came from it.
func TestConfigurationChangesBetweenStarts(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	first := launch(t, dbURL, writeConfig(t, configuration))
	first.waitUntilServing()
	code, res := first.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	if code := first.terminate(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}

	edited := strings.NewReplacer("name: e2e", "name: renamed", "timeout: 5m", "timeout: 10m").Replace(configuration) +
		"teams:\n  - name: platform\n"
	second := launch(t, dbURL, writeConfig(t, edited))
	second.waitUntilServing()

	for table, want := range map[string]string{"sources": "e2e (removed), renamed", "integrations": "slack", "escalation_policies": "default", "teams": "platform"} {
		if got := second.rows(table); got != want {
			t.Errorf("%s = %q, want %q", table, got, want)
		}
	}
	logs := second.output()
	for _, want := range []string{
		`"msg":"provisioned object removed","kind":"sources","name":"e2e"`,
		`"msg":"provisioned object created","kind":"sources","name":"renamed"`,
		`"msg":"provisioned object updated","kind":"escalation_policies","name":"default"`,
		`"msg":"provisioned object created","kind":"teams","name":"platform"`,
		`"msg":"configuration reconciled","changes":4`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs do not contain %s", want)
		}
	}
	// The alert still points at the source it came from.
	var source string
	err := second.db.QueryRow(context.Background(), `SELECT s.name FROM alerts a JOIN sources s ON s.id = a.source_id WHERE a.id = $1`, alertID).Scan(&source)
	if err != nil || source != "e2e" {
		t.Errorf("the alert's source = %q, %v; want e2e", source, err)
	}
	// The removed source no longer accepts alerts.
	if code, _ := second.webhook(diskFull); code != http.StatusUnauthorized {
		t.Errorf("webhook to the removed source = %d, want 401", code)
	}
}

// reload replaces the configuration file, sends SIGHUP, as an operator
// would, and waits for the n-th outcome in the log.
func (b *beacon) reload(yaml string, n int) (logs string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.dir, "beacon.yaml"), []byte(yaml), 0o600); err != nil {
		b.t.Fatalf("write configuration: %v", err)
	}
	if err := b.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		b.t.Fatalf("signal beacon: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		logs = b.output()
		if strings.Count(logs, `"msg":"configuration reloaded"`)+strings.Count(logs, `"msg":"configuration reload failed`) >= n {
			return logs
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("no outcome of reload %d in the logs:\n%s", n, logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// metrics returns the lines of the metrics endpoint that start with prefix.
func (b *beacon) metrics(prefix string) string {
	b.t.Helper()
	resp, err := http.Get(b.url + "/metrics")
	if err != nil {
		b.t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("read metrics: %v", err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// TestReload changes the configuration of a running process three times: a
// valid change, an invalid one, and a valid one again. The process keeps
// serving throughout; the invalid configuration is rejected as a whole and
// changes nothing.
func TestReload(t *testing.T) {
	b := start(t)
	if got, want := b.metrics("beacon_config_reloads_total"), `beacon_config_reloads_total{result="failure"} 0`+"\n"+`beacon_config_reloads_total{result="success"} 0`; got != want {
		t.Errorf("metrics at start:\n%s", got)
	}
	if got := b.metrics("beacon_config_last_reload_successful"); got != "beacon_config_last_reload_successful 1" {
		t.Errorf("metrics at start: %s", got)
	}

	// 1. A valid change: a second source, and the channel by another name
	// (the fake Slack knows one channel, by its ID and by the name alerts).
	second := strings.Replace(configuration, "sources:\n", "sources:\n  - name: second\n    type: webhook\n    token: {env: BEACON_SOURCE_TOKEN}\n", 1)
	second = strings.Replace(second, "channel: C0ALERTS", "channel: alerts", 1)
	if code, _ := b.webhookTo("second", diskFull); code != http.StatusUnauthorized {
		t.Fatalf("webhook to a source that does not exist yet = %d, want 401", code)
	}
	logs := b.reload(second, 1)
	if !strings.Contains(logs, `"msg":"configuration reloaded","trigger":"signal"`) || !strings.Contains(logs, `"msg":"provisioned object created","kind":"sources","name":"second"`) {
		t.Fatalf("the reload is not in the logs:\n%s", logs)
	}
	code, res := b.webhookTo("second", diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook to the new source = %d %v", code, res)
	}
	b.message("the alert in Slack", contains("*Firing*"))
	var destination string
	if err := b.db.QueryRow(context.Background(), `SELECT destination FROM notifications`).Scan(&destination); err != nil || destination != "alerts" {
		t.Errorf("the alert was sent to %q, %v; want the channel of the reloaded configuration", destination, err)
	}

	// 2. An invalid change: the first source is gone, and so is the policy
	// that routing names. Nothing of it may take effect.
	broken := strings.Replace(second, "  - name: e2e\n    type: webhook\n    token: {env: BEACON_SOURCE_TOKEN}\n", "", 1)
	broken = strings.Replace(broken, "escalation_policy: default", "escalation_policy: missing", 1)
	logs = b.reload(broken, 2)
	for _, want := range []string{
		`"msg":"configuration reload failed; the previous configuration stays active"`,
		`"msg":"configuration problem"`, `"object":"routing","field":"default.escalation_policy","problem":"escalation policy \"missing\" does not exist"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs do not contain %s", want)
		}
	}
	if code, res := b.webhook(diskFullResolved); code != http.StatusOK {
		t.Errorf("webhook to the source that the rejected configuration removed = %d %v, want it still served", code, res)
	}
	if got := b.rows("sources"); got != "e2e, second" {
		t.Errorf("sources after the rejected reload = %q", got)
	}
	if got, want := b.metrics("beacon_config_"), `beacon_config_last_reload_successful 0`; !strings.Contains(got, want) ||
		!strings.Contains(got, `beacon_config_reloads_total{result="failure"} 1`) || !strings.Contains(got, `beacon_config_reloads_total{result="success"} 1`) {
		t.Errorf("metrics after the rejected reload:\n%s", got)
	}

	// 3. Fixed: the removal now takes effect.
	fixed := strings.Replace(broken, "escalation_policy: missing", "escalation_policy: default", 1)
	b.reload(fixed, 3)
	if code, _ := b.webhook(diskFull); code != http.StatusUnauthorized {
		t.Errorf("webhook to the removed source = %d, want 401", code)
	}
	if got := b.rows("sources"); got != "e2e (removed), second" {
		t.Errorf("sources after the reload = %q", got)
	}
	if got := b.metrics("beacon_config_"); !strings.Contains(got, "beacon_config_last_reload_successful 1") || !strings.Contains(got, `beacon_config_reloads_total{result="success"} 2`) {
		t.Errorf("metrics after the last reload:\n%s", got)
	}

	// 4. The escalation engine follows the configuration too. The policy
	// gets a second step and a short timeout; a group that opens afterwards
	// is escalated by the policy as it is now.
	b.slack.AddChannel("second-line", "C0SECOND")
	twoSteps := strings.Replace(fixed, "        timeout: 5m\n", "        timeout: 1s\n      - notify:\n          - channel: C0SECOND\n            integration: slack\n        timeout: 5m\n", 1)
	if twoSteps == fixed {
		t.Fatal("the configuration has no step to change")
	}
	logs = b.reload(twoSteps, 4)
	if !strings.Contains(logs, `"msg":"escalation policy configured","policy":"default","steps":2,"repeat":0`) {
		t.Errorf("the logs do not show the policy with two steps")
	}
	if code, res := b.webhookTo("second", `{"labels": {"alertname": "AfterReload", "severity": "critical"}}`); code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	b.find("the first step of the reloaded policy", inChannel("C0ALERTS", "AfterReload"))
	b.find("the second step of the reloaded policy", inChannel("C0SECOND", "AfterReload"))

	// 5. So do the channels: an integration of another service that arrives
	// with a reload delivers from then on.
	reloadAddsDiscord(b, twoSteps, 5)

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	for _, secret := range []string{sourceToken, botToken, signingSecret, discordToken} {
		if strings.Contains(b.output(), secret) {
			t.Errorf("the logs contain the credential %q", secret)
		}
	}
}

// TestReloadReadsSecretFilesAgain rotates a secret that is kept in a file.
func TestReloadReadsSecretFilesAgain(t *testing.T) {
	b := start(t)
	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	msg := b.message("the alert in Slack", contains("*Firing*"))

	const rotated = "e2e-rotated-signing-secret-17aa"
	if err := os.WriteFile(filepath.Join(b.dir, "signing-secret"), []byte(rotated+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.reload(configuration, 1)
	alice := slacktest.User{ID: "U0ALICE", Username: "alice"}
	if code := b.click(msg, "beacon_acknowledge", alice); code != http.StatusUnauthorized {
		t.Errorf("a press signed with the old secret = %d, want 401", code)
	}
	if code := b.slack.Click(t, b.url+"/api/v1/integrations/slack/interactions", rotated, msg, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Errorf("a press signed with the new secret = %d, want 200", code)
	}
	if strings.Contains(b.output(), rotated) {
		t.Error("the logs contain the rotated secret")
	}
}

// signInConfiguration adds sign-in through the Dex of compose.yaml to the
// configuration.
func signInConfiguration(t *testing.T) string {
	t.Helper()
	return configuration + `auth:
  public_url: ` + oidctest.PublicURL + `
  session_lifetime: 1h
  oidc:
    issuer: ` + oidctest.Issuer(t) + `
    client_id: ` + oidctest.ClientID + `
    client_secret: {env: BEACON_OIDC_CLIENT_SECRET}
    scopes: [openid, profile, email, groups]
  roles:
    admin: [beacon-admins]
    member: [platform-engineers]
teams:
  - name: platform
    groups: [platform-engineers]
`
}

// do sends a request to Beacon with the browser and returns the status and
// the body.
func do(t *testing.T, browser *http.Client, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, oidctest.PublicURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// TestSignIn signs a person in to the real binary through a real identity
// provider, uses the session, and signs out.
func TestSignIn(t *testing.T) {
	issuer := oidctest.Issuer(t)
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, signInConfiguration(t)))
	b.waitUntilServing()
	browser := oidctest.Browser(t, strings.TrimPrefix(b.url, "http://"))

	if code, body := do(t, browser, http.MethodGet, "/api/v1/me"); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/me before sign-in = %d %s, want 401", code, body)
	}
	code, body := oidctest.SignIn(t, browser, oidctest.Grace, oidctest.Password)
	if code != http.StatusOK || !strings.Contains(body, `"email":"grace@example.com"`) || !strings.Contains(body, `"role":"member"`) {
		t.Fatalf("after sign-in = %d %s", code, body)
	}
	var stored string
	err := b.db.QueryRow(context.Background(), `SELECT oidc_issuer FROM users WHERE email = 'grace@example.com' AND last_sign_in_at IS NOT NULL`).Scan(&stored)
	if err != nil || stored != issuer {
		t.Errorf("the stored user has issuer %q, %v; want %q", stored, err, issuer)
	}

	// Roles and teams follow the groups that Dex puts in the token: Ada is
	// in beacon-admins and platform-engineers, Grace in platform-engineers,
	// and Linus in neither, so he has no access.
	if code, body := oidctest.SignIn(t, oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), oidctest.Ada, oidctest.Password); code != http.StatusOK || !strings.Contains(body, `"role":"admin"`) {
		t.Errorf("Ada after sign-in = %d %s, want an admin", code, body)
	}
	if code, body := oidctest.SignIn(t, oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), oidctest.Linus, oidctest.Password); code != http.StatusForbidden {
		t.Errorf("Linus after sign-in = %d %s, want 403", code, body)
	}
	var members string
	err = b.db.QueryRow(context.Background(), `
		SELECT coalesce(string_agg(u.email || ' ' || m.origin, ', ' ORDER BY u.email), '')
		FROM team_members m JOIN teams t ON t.id = m.team_id JOIN users u ON u.id = m.user_id WHERE t.name = 'platform'`).Scan(&members)
	if err != nil || members != "ada@example.com group, grace@example.com group" {
		t.Errorf("members of the team = %q, %v", members, err)
	}

	// The session survives a reload of the same configuration, and the
	// webhook still needs no session, only its token.
	b.reload(signInConfiguration(t), 1)
	if code, body := do(t, browser, http.MethodGet, "/api/v1/me"); code != http.StatusOK {
		t.Errorf("GET /api/v1/me after a reload = %d %s", code, body)
	}
	if code, res := b.webhook(diskFull); code != http.StatusCreated {
		t.Errorf("webhook = %d %v", code, res)
	}

	if code, body := do(t, browser, http.MethodPost, "/auth/logout"); code != http.StatusNoContent {
		t.Errorf("sign-out = %d %s, want 204", code, body)
	}
	if code, _ := do(t, browser, http.MethodGet, "/api/v1/me"); code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/me after sign-out = %d, want 401", code)
	}

	// Taking sign-in out of the configuration turns it off.
	b.reload(configuration, 2)
	if code, _ := do(t, browser, http.MethodGet, "/auth/login"); code != http.StatusNotFound {
		t.Errorf("GET /auth/login without an auth block = %d, want 404", code)
	}

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	logs := b.output()
	for _, secret := range []string{oidctest.ClientSecret, sourceToken, botToken, signingSecret} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain the credential %q", secret)
		}
	}
	for _, cookie := range browser.Jar.Cookies(&url.URL{Scheme: "http", Host: "127.0.0.1"}) {
		if cookie.Value != "" && strings.Contains(logs, cookie.Value) {
			t.Errorf("the logs contain the value of the cookie %s", cookie.Name)
		}
	}
}

// TestTeamMembersFromConfiguration lists people in a team before they have
// ever signed in. They are skipped, logged and counted, and nothing else is
// held up; each joins the team at their first sign-in.
func TestTeamMembersFromConfiguration(t *testing.T) {
	withMembers := strings.Replace(signInConfiguration(t), "  - name: platform\n", "  - name: platform\n    members: [grace@example.com, nobody@example.com]\n", 1)
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, withMembers))
	b.waitUntilServing()

	// Both are unknown at the start. That is logged as an error for each,
	// and the rest of the configuration works.
	logs := b.output()
	for _, email := range []string{"grace@example.com", "nobody@example.com"} {
		if want := `"level":"ERROR","msg":"team member skipped: no user has this email address; they join the team when they first sign in","team":"platform","email":"` + email + `"`; !strings.Contains(logs, want) {
			t.Errorf("the logs do not contain %s", want)
		}
	}
	if got := b.metrics("beacon_team_members_skipped"); got != `beacon_team_members_skipped{team="platform"} 2` {
		t.Errorf("metric at start: %s", got)
	}
	if code, res := b.webhook(diskFull); code != http.StatusCreated {
		t.Errorf("webhook = %d %v", code, res)
	}

	// Grace signs in for the first time, and is a member at once.
	browser := oidctest.Browser(t, strings.TrimPrefix(b.url, "http://"))
	if code, body := oidctest.SignIn(t, browser, oidctest.Grace, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	members := func() string {
		t.Helper()
		var out string
		err := b.db.QueryRow(context.Background(), `
			SELECT coalesce(string_agg(u.email || ' ' || m.origin, ', ' ORDER BY u.email, m.origin), '')
			FROM team_members m JOIN teams t ON t.id = m.team_id JOIN users u ON u.id = m.user_id WHERE t.name = 'platform'`).Scan(&out)
		if err != nil {
			t.Fatalf("read members: %v", err)
		}
		return out
	}
	// From the list, and from her identity provider group as well.
	if got := members(); got != "grace@example.com config, grace@example.com group" {
		t.Errorf("members after Grace signed in = %q", got)
	}
	if got := b.metrics("beacon_team_members_skipped"); got != `beacon_team_members_skipped{team="platform"} 1` {
		t.Errorf("metric after Grace signed in: %s", got)
	}

	// Taking her off the list removes that membership at the next reload,
	// and leaves the one from her group.
	b.reload(strings.Replace(withMembers, "grace@example.com, ", "", 1), 1)
	if got := members(); got != "grace@example.com group" {
		t.Errorf("members after the list changed = %q", got)
	}
}

// scheduleConfiguration adds a daily rotation of Grace and Ada to the
// configuration with sign-in.
func scheduleConfiguration(t *testing.T) string {
	t.Helper()
	return signInConfiguration(t) + `schedules:
  - name: primary
    team: platform
    time_zone: Australia/Sydney
    rotation:
      members: [grace@example.com, ada@example.com]
      shift_length: 1d
      handoff:
        time: "09:00"
`
}

// onCall returns who the position of a schedule says is on call, and how
// many shifts it has recorded.
func (b *beacon) onCall(name string) (email string, shifts int) {
	b.t.Helper()
	err := b.db.QueryRow(context.Background(), `
		SELECT COALESCE(u.email, ''), (SELECT count(*) FROM shifts f WHERE f.schedule_id = s.id AND f.kind = 'rotation')
		FROM schedules s JOIN schedule_positions p ON p.schedule_id = s.id LEFT JOIN users u ON u.id = p.user_id
		WHERE s.name = $1`, name).Scan(&email, &shifts)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		b.t.Fatalf("read the position of %s: %v", name, err)
	}
	return email, shifts
}

// TestScheduleRotation runs a schedule in the real binary. Its members have
// not signed in when it starts, so nobody is on call and the logs say so;
// the first of them to sign in takes the shift, and a restart in the middle
// of that shift leaves it alone.
func TestScheduleRotation(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	dir := writeConfig(t, scheduleConfiguration(t))
	b := launch(t, dbURL, dir)
	b.waitUntilServing()

	if who, shifts := b.onCall("primary"); who != "" || shifts != 0 {
		t.Fatalf("before anyone signed in, %q is on call with %d shifts", who, shifts)
	}
	logs := b.output()
	for _, email := range []string{"grace@example.com", "ada@example.com"} {
		if want := `"msg":"rotation member skipped: no user has this email address; they take their place in the rotation once they have signed in","schedule":"primary","email":"` + email + `"`; !strings.Contains(logs, want) {
			t.Errorf("the logs do not contain %s", want)
		}
	}
	if got := b.metrics("beacon_schedule_members_skipped"); got != `beacon_schedule_members_skipped{schedule="primary"} 2` {
		t.Errorf("metric at start: %s", got)
	}
	// Nobody on call is a gap in coverage, which is a warning.
	if want := `"level":"WARN","msg":"gap in coverage: the schedule has nobody on call`; !strings.Contains(logs, want) {
		t.Errorf("the logs do not contain %s", want)
	}

	// Ada is second in the rotation and the first to be a user.
	if code, body := oidctest.SignIn(t, oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), oidctest.Ada, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		who, shifts := b.onCall("primary")
		if who == oidctest.Ada && shifts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q is on call with %d shifts, want Ada with one\n%s", who, shifts, b.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
	var started time.Time
	if err := b.db.QueryRow(context.Background(), `SELECT starts_at FROM shifts WHERE ends_at IS NULL AND kind = 'rotation'`).Scan(&started); err != nil {
		t.Fatalf("read the shift: %v", err)
	}

	// Grace covers for an hour, through the API. Her override is recorded as
	// a shift beside Ada's, and removing it ends that shift.
	admin := oidctest.Browser(t, strings.TrimPrefix(b.url, "http://"))
	if code, body := oidctest.SignIn(t, admin, oidctest.Ada, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	member := oidctest.Browser(t, strings.TrimPrefix(b.url, "http://"))
	if code, body := oidctest.SignIn(t, member, oidctest.Grace, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	if code, body := b.api(oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), "POST", "/api/v1/schedules/primary/overrides", "", `{"user": "grace@example.com", "duration": "1h"}`); code != http.StatusUnauthorized {
		t.Errorf("an override without a session = %d %v, want 401", code, body)
	}
	code, override := b.api(member, "POST", "/api/v1/schedules/primary/overrides", "", `{"user": "grace@example.com", "duration": "1h", "reason": "e2e"}`)
	if code != http.StatusCreated || override["state"] != "active" || override["created_by"] != "grace@example.com" {
		t.Fatalf("create override = %d %v", code, override)
	}
	overrideShifts := func() (open, closed int) {
		t.Helper()
		err := b.db.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE f.ends_at IS NULL), count(*) FILTER (WHERE f.ends_at IS NOT NULL)
			FROM shifts f JOIN users u ON u.id = f.user_id WHERE f.kind = 'override' AND u.email = 'grace@example.com'`).Scan(&open, &closed)
		if err != nil {
			t.Fatalf("read shifts: %v", err)
		}
		return open, closed
	}
	if open, closed := overrideShifts(); open != 1 || closed != 0 {
		t.Errorf("%d open and %d closed override shifts after the override began, want 1 and 0", open, closed)
	}
	// Who is on call, for the schedule and for its team: Grace, covering Ada.
	onCallNow := func(path string) map[string]any {
		t.Helper()
		code, body := b.api(member, "GET", path, "", "")
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %v", path, code, body)
		}
		if list, ok := body["on_call"].([]any); ok && len(list) == 1 {
			return list[0].(map[string]any)
		}
		on, _ := body["on_call"].(map[string]any)
		return on
	}
	for _, path := range []string{"/api/v1/schedules/primary", "/api/v1/teams/platform/on-call"} {
		if on := onCallNow(path); on["user"] != "grace@example.com" || on["rotation_user"] != "ada@example.com" || on["override"] == nil {
			t.Errorf("GET %s during the override: on call %v", path, on)
		}
	}
	code, timeline := b.api(member, "GET", "/api/v1/schedules/primary/shifts", "", "")
	if upcoming, _ := timeline["shifts"].([]any); code != http.StatusOK || len(upcoming) < 10 || upcoming[0].(map[string]any)["user"] != "grace@example.com" || upcoming[1].(map[string]any)["user"] != "ada@example.com" {
		t.Errorf("GET shifts = %d %v", code, timeline)
	}
	// A second override over the same time lies on top of the first: Ada
	// takes the next half hour back. Removing hers reveals Grace's.
	code, top := b.api(admin, "POST", "/api/v1/schedules/primary/overrides", "", `{"user": "ada@example.com", "duration": "30m"}`)
	if code != http.StatusCreated {
		t.Fatalf("an overlapping override = %d %v, want 201", code, top)
	}
	if on := onCallNow("/api/v1/schedules/primary"); on["user"] != "ada@example.com" || on["override"] == nil {
		t.Errorf("on call under two overrides: %v, want the one created last", on)
	}
	if code, body := b.api(member, "DELETE", "/api/v1/schedules/primary/overrides/"+top["id"].(string), "", ""); code != http.StatusNoContent {
		t.Errorf("remove the override on top = %d %v, want 204", code, body)
	}
	if on := onCallNow("/api/v1/schedules/primary"); on["user"] != "grace@example.com" {
		t.Errorf("on call after the override on top was removed: %v, want Grace again", on)
	}
	if code, body := b.api(admin, "DELETE", "/api/v1/schedules/primary/overrides/"+override["id"].(string), "", ""); code != http.StatusNoContent {
		t.Errorf("remove override = %d %v, want 204", code, body)
	}
	if open, closed := overrideShifts(); open != 0 || closed != 1 {
		t.Errorf("%d open and %d closed override shifts after the override was removed, want 0 and 1", open, closed)
	}
	if on := onCallNow("/api/v1/schedules/primary"); on["user"] != "ada@example.com" || on["override"] != nil {
		t.Errorf("on call after the override was removed: %v", on)
	}

	// A restart in the middle of the shift.
	if code := b.terminate(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	b = launch(t, dbURL, dir)
	b.waitUntilServing()
	var again time.Time
	err := b.db.QueryRow(context.Background(), `SELECT starts_at FROM shifts WHERE ends_at IS NULL AND kind = 'rotation'`).Scan(&again)
	if who, shifts := b.onCall("primary"); err != nil || who != oidctest.Ada || shifts != 1 || !again.Equal(started) {
		t.Errorf("after a restart %q is on call with %d shifts, the open one since %s (%v); want Ada with the shift from %s", who, shifts, again, err, started)
	}
	// Both members are users by now.
	if got := b.metrics("beacon_schedule_members_skipped"); got != `beacon_schedule_members_skipped{schedule="primary"} 0` {
		t.Errorf("metric after the restart: %s", got)
	}

	// The definition changes in the files while Ada is on call. The reload
	// stores the change and moves nothing: it waits for the handoff.
	changed := strings.Replace(scheduleConfiguration(t), "members: [grace@example.com, ada@example.com]", "members: [ada@example.com, linus@example.com]", 1)
	logs = b.reload(changed, 1)
	if !strings.Contains(logs, `"msg":"schedule definition changed: the rotation in force runs until the change takes over, at the next handoff","schedule":"primary","takes_over_at":"`) {
		t.Errorf("the logs do not say that the definition changed:\n%s", logs)
	}
	var inForce, pending string
	err = b.db.QueryRow(context.Background(), `SELECT definition->'members'->>0, COALESCE(pending_definition->'members'->>1, '') FROM schedule_positions`).Scan(&inForce, &pending)
	if err != nil || inForce != "grace@example.com" || pending != "linus@example.com" {
		t.Errorf("in force begins with %q and the pending rotation has %q second (%v); want the old one in force and the new one pending", inForce, pending, err)
	}
	err = b.db.QueryRow(context.Background(), `SELECT starts_at FROM shifts WHERE ends_at IS NULL AND kind = 'rotation'`).Scan(&again)
	if who, shifts := b.onCall("primary"); err != nil || who != oidctest.Ada || shifts != 1 || !again.Equal(started) {
		t.Errorf("after the definition changed %q is on call with %d shifts, the open one since %s (%v)", who, shifts, again, err)
	}

	// The schedule leaves the configuration: its shift ends and is kept.
	logs = b.reload(signInConfiguration(t), 2)
	if !strings.Contains(logs, `"msg":"schedule ended: it left the configuration; its recorded shifts are kept","schedule":"primary"`) {
		t.Errorf("the logs do not say that the schedule ended:\n%s", logs)
	}
	var open, closed int
	err = b.db.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE ends_at IS NULL), count(*) FILTER (WHERE ends_at IS NOT NULL) FROM shifts WHERE kind = 'rotation'`).Scan(&open, &closed)
	if who, _ := b.onCall("primary"); err != nil || open != 0 || closed != 1 || who != "" {
		t.Errorf("after the schedule was removed: %d open and %d closed shifts, %q on call (%v)", open, closed, who, err)
	}
}

// waitFor polls until cond holds, and fails the test with what if it does
// not within half a minute.
func (b *beacon) waitFor(what string, cond func() bool) {
	b.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s\n%s", what, b.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSlackIdentity links people to their Slack accounts in the real
// binary. Ada has an account in the workspace with her address: Beacon
// finds it after she signs in, shows it to her, and attributes what she
// does in Slack to her. Grace has none: that is logged and counted.
func TestSlackIdentity(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, signInConfiguration(t)))
	b.waitUntilServing()
	b.slack.AddUser(oidctest.Ada, "U0ADA")

	admin := oidctest.Browser(t, strings.TrimPrefix(b.url, "http://"))
	if code, body := oidctest.SignIn(t, admin, oidctest.Ada, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	identities := func(client *http.Client) []any {
		t.Helper()
		code, body := b.api(client, "GET", "/api/v1/me/identities", "", "")
		list, _ := body["identities"].([]any)
		if code != http.StatusOK || list == nil {
			t.Fatalf("GET /api/v1/me/identities = %d %v", code, body)
		}
		return list
	}
	b.waitFor("Ada's Slack account to be linked", func() bool { return len(identities(admin)) == 1 })
	if got := identities(admin)[0].(map[string]any); got["integration"] != "slack" || got["provider"] != "slack" || got["external_id"] != "U0ADA" || got["origin"] != "email" {
		t.Errorf("Ada's identity = %v", got)
	}

	// Grace is not in the workspace.
	member := oidctest.Browser(t, strings.TrimPrefix(b.url, "http://"))
	if code, body := oidctest.SignIn(t, member, oidctest.Grace, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	b.waitFor("the gap to be logged", func() bool {
		return strings.Contains(b.output(), `"msg":"no account in the chat service has this user's email address, so they cannot be reached directly through this integration; Beacon asks again in an hour","integration":"slack","provider":"slack"`)
	})
	if len(identities(member)) != 0 {
		t.Errorf("Grace has identities: %v", identities(member))
	}
	b.waitFor("the metric of users without an identity", func() bool {
		return b.metrics("beacon_users_without_identity") == `beacon_users_without_identity{integration="slack"} 1`
	})

	// Ada acknowledges from Slack, under whatever name she has there. The
	// timeline and the alert name the Beacon user.
	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	msg := b.message("the alert", contains("Disk is full on db1"))
	if code := b.click(msg, "beacon_acknowledge", slacktest.User{ID: "U0ADA", Username: "ada.in.slack"}); code != http.StatusOK {
		t.Fatalf("click = %d", code)
	}
	var (
		actor, name string
		by          bool
	)
	err := b.db.QueryRow(context.Background(), `
		SELECT u.email, COALESCE(a.acknowledged_by = u.id, false), e.data->>'actor_name'
		FROM alert_events e JOIN users u ON u.id = e.actor_id JOIN alerts a ON a.id = e.alert_id
		WHERE e.type = 'acknowledged' AND e.actor_provider = 'slack' AND e.actor_external_id = 'U0ADA'`).Scan(&actor, &by, &name)
	if err != nil || actor != oidctest.Ada || !by || name == "" || name == "ada.in.slack" {
		t.Errorf("the acknowledgement is by %q (acknowledged_by matches: %v, name %q), %v; want the user Ada", actor, by, name, err)
	}
	b.message("the acknowledgement by the Beacon user", contains("Acknowledged by "+name))

	// Someone in the channel who is no Beacon user may resolve and nothing
	// else. Snoozing is refused with a note; resolving is recorded as the
	// Slack account, under the name the workspace has for it.
	guest := slacktest.User{ID: "U0GUEST", Username: "guest"}
	b.slack.SetProfile(guest.ID, slacktest.Profile{DisplayName: "Gus Guest", Handle: "guest"})
	if code := b.click(b.slack.Messages()[0], "beacon_snooze", guest); code != http.StatusOK {
		t.Fatalf("click = %d", code)
	}
	if notes := b.slack.Responses(); len(notes) != 1 || !strings.Contains(notes[0]["text"].(string), "only Beacon users can snooze an alert") {
		t.Errorf("answer to the snooze by the guest = %v", notes)
	}
	if code := b.click(b.slack.Messages()[0], "beacon_resolve", guest); code != http.StatusOK {
		t.Fatalf("click = %d", code)
	}
	b.message("the resolution", contains("Resolved by Gus Guest (Slack not connected)"))
	var guestName string
	if err := b.db.QueryRow(context.Background(), `SELECT data->>'actor_name' FROM alert_events WHERE type = 'resolved' AND actor_external_id = 'U0GUEST'`).Scan(&guestName); err != nil || guestName != "Gus Guest (Slack not connected)" {
		t.Errorf("the timeline names the guest %q, %v", guestName, err)
	}
	var unlinked int
	err = b.db.QueryRow(context.Background(), `SELECT count(*) FROM alert_events WHERE type = 'resolved' AND actor_id IS NULL AND actor_external_id = 'U0GUEST'`).Scan(&unlinked)
	if err != nil || unlinked != 1 {
		t.Errorf("%d resolved events by the unlinked account, %v; want 1", unlinked, err)
	}

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	if logs := b.output(); strings.Contains(logs, botToken) || strings.Contains(logs, signingSecret) {
		t.Error("the logs contain a Slack credential")
	}
}

// escalationConfiguration is sign-in, a daily rotation of Ada and Grace,
// and a policy whose first step pages whoever is on call and whose second,
// three seconds later, tells the whole team and a channel. The second step
// has a long timeout, so that nothing the test does not cause can follow it.
func escalationConfiguration(t *testing.T) string {
	t.Helper()
	cfg := strings.Replace(signInConfiguration(t), `    steps:
      - notify:
          - channel: C0ALERTS
            integration: slack
        timeout: 5m
`, `    steps:
      - notify:
          - schedule: primary
        timeout: 3s
      - notify:
          - team: platform
          - channel: C0ALERTS
            integration: slack
        timeout: 10m
`, 1)
	if cfg == signInConfiguration(t) {
		t.Fatal("the configuration has no policy to change")
	}
	return cfg + `schedules:
  - name: primary
    team: platform
    time_zone: UTC
    rotation:
      members: [ada@example.com, grace@example.com]
      shift_length: 1d
      handoff:
        time: "09:00"
`
}

// escalationSteps reads the escalation events of the one group as
// "pass.step: who was notified", and the state of its escalation.
func (b *beacon) escalationSteps() (steps []string, status string) {
	b.t.Helper()
	rows, err := b.db.Query(context.Background(), `
		SELECT (e.data->>'pass') || '.' || (e.data->>'step') || ': ' || COALESCE((
			SELECT string_agg((n->>'target') || COALESCE(' ' || (n->>'user'), ''), ', ' ORDER BY (n->>'target'), (n->>'user'))
			FROM jsonb_array_elements(e.data->'notified') n), 'nobody')
		FROM alert_events e WHERE e.type = 'escalated' ORDER BY e.id`)
	if err != nil {
		b.t.Fatalf("read escalation events: %v", err)
	}
	if steps, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		b.t.Fatalf("read escalation events: %v", err)
	}
	err = b.db.QueryRow(context.Background(), `SELECT status || CASE WHEN next_step_at IS NULL THEN '' ELSE ', timer running' END FROM escalations`).Scan(&status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		b.t.Fatalf("read escalation: %v", err)
	}
	return steps, status
}

// TestFirstPathWithEscalation is the first path with people in it: an alert
// comes in, the person on call is paged in a direct message, nobody
// responds, the policy's second step tells the team and a channel, a team
// member acknowledges from Slack, which stops the escalation, and the
// source resolves the alert.
func TestFirstPathWithEscalation(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, escalationConfiguration(t)))
	b.waitUntilServing()
	b.slack.AddUser(oidctest.Ada, "U0ADA")
	b.slack.AddUser(oidctest.Grace, "U0GRACE")

	// Ada and Grace sign in. Beacon finds their Slack accounts, and Ada,
	// first in the rotation, is on call.
	for _, who := range []string{oidctest.Ada, oidctest.Grace} {
		if code, body := oidctest.SignIn(t, oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), who, oidctest.Password); code != http.StatusOK {
			t.Fatalf("sign-in of %s = %d %s", who, code, body)
		}
		if who == oidctest.Ada {
			// Ada is a user before Grace is, so the rotation starts with her.
			b.waitFor("Ada to be on call", func() bool { who, _ := b.onCall("primary"); return who == oidctest.Ada })
		}
	}
	b.waitFor("both Slack accounts to be linked", func() bool {
		var n int
		err := b.db.QueryRow(context.Background(), `SELECT count(*) FROM user_identities`).Scan(&n)
		return err == nil && n == 2
	})

	// 1. The alert. The first step pages the person on call, and only her.
	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	page := b.find("the page to the person on call", inChannel(slacktest.DM("U0ADA"), "*Firing*", "Disk is full on db1", "beacon_acknowledge"))
	if page.Text != "[FIRING] critical: Disk is full on db1" {
		t.Errorf("page = %+v", page)
	}

	// 2. Nobody responds. Three seconds later the second step tells the
	// team, which is Ada and Grace, and the channel.
	fromGrace := b.find("the second step to the other team member", inChannel(slacktest.DM("U0GRACE"), "*Firing*", "Disk is full on db1"))
	// The message in the channel says who was paged, as mentions.
	b.find("the second step to the channel", inChannel("C0ALERTS", "*Firing*", "Disk is full on db1", "Paged: ", `\u003c@U0ADA\u003e`, `\u003c@U0GRACE\u003e`))
	// Ada is asked again in a new message; the first became a pointer to it.
	b.waitFor("Ada to be asked again", func() bool {
		var firing, pointers int
		for _, m := range b.slack.Messages() {
			if m.Channel == slacktest.DM("U0ADA") && strings.Contains(m.JSON(), "*Firing*") {
				firing++
			}
			if m.Channel == slacktest.DM("U0ADA") && strings.Contains(m.JSON(), "Beacon sent this alert again") {
				pointers++
			}
		}
		return firing == 1 && pointers == 1
	})
	steps, status := b.escalationSteps()
	if want := []string{"1.1: schedule primary ada@example.com", "1.2: channel C0ALERTS, team platform ada@example.com, team platform grace@example.com"}; strings.Join(steps, "\n") != strings.Join(want, "\n") || status != "active, timer running" {
		t.Fatalf("escalation: %s\n  %s\nwant active with\n  %s", status, strings.Join(steps, "\n  "), strings.Join(want, "\n  "))
	}
	// Each page names the person it is for.
	var forUsers string
	err := b.db.QueryRow(context.Background(), `SELECT string_agg(n.destination || '=' || COALESCE(u.email, '-'), ' ' ORDER BY n.destination)
		FROM notifications n LEFT JOIN users u ON u.id = n.user_id WHERE n.reason = 'escalation_step'`).Scan(&forUsers)
	if want := "C0ALERTS=- U0ADA=ada@example.com U0ADA=ada@example.com U0GRACE=grace@example.com"; err != nil || forUsers != want {
		t.Errorf("pages: %s, %v\nwant:  %s", forUsers, err, want)
	}

	// 3. Grace acknowledges from her direct message. That is recorded as
	// her, shown everywhere the group was told about, and stops the
	// escalation: its timer is gone.
	if code := b.click(fromGrace, "beacon_acknowledge", slacktest.User{ID: "U0GRACE", Username: "grace.in.slack"}); code != http.StatusOK {
		t.Fatalf("acknowledge = %d", code)
	}
	if got := b.status(alertID); got != "acknowledged" {
		t.Fatalf("the alert is %s, want acknowledged", got)
	}
	var by string
	if err := b.db.QueryRow(context.Background(), `SELECT u.email FROM alerts a JOIN users u ON u.id = a.acknowledged_by WHERE a.id = $1`, alertID).Scan(&by); err != nil || by != oidctest.Grace {
		t.Errorf("acknowledged by %q, %v; want Grace", by, err)
	}
	if _, status := b.escalationSteps(); status != "acknowledged" {
		t.Errorf("escalation after the acknowledgement: %s, want it stopped with no timer", status)
	}
	for _, channel := range []string{slacktest.DM("U0ADA"), slacktest.DM("U0GRACE"), "C0ALERTS"} {
		b.find("the acknowledgement in "+channel, inChannel(channel, "*Acknowledged*", "Acknowledged by "))
	}

	// 4. The source resolves the alert.
	if code, res := b.webhook(diskFullResolved); code != http.StatusOK {
		t.Fatalf("resolve webhook = %d %v", code, res)
	}
	for _, channel := range []string{slacktest.DM("U0ADA"), slacktest.DM("U0GRACE"), "C0ALERTS"} {
		b.find("the resolution in "+channel, inChannel(channel, "*Resolved*", "Resolved by the source"))
	}
	steps, status = b.escalationSteps()
	if len(steps) != 2 || status != "resolved" {
		t.Errorf("at the end the escalation is %s after %d steps, want resolved after 2:\n  %s", status, len(steps), strings.Join(steps, "\n  "))
	}

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	logs := b.output()
	for _, secret := range []string{sourceToken, botToken, signingSecret, oidctest.ClientSecret} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain the credential %q", secret)
		}
	}
}

// TestUnlinkedPeopleStayVisible pages a team of which one member has no
// Slack account. She cannot be notified, and is still named wherever Beacon
// says who was paged: in the channel's message as plain text with "(Slack
// not connected)", next to a real mention of her colleague, and on the
// timeline.
func TestUnlinkedPeopleStayVisible(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, escalationConfiguration(t)))
	b.waitUntilServing()
	// Ada is in the workspace. Grace is not.
	b.slack.AddUser(oidctest.Ada, "U0ADA")
	for _, who := range []string{oidctest.Ada, oidctest.Grace} {
		if code, body := oidctest.SignIn(t, oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), who, oidctest.Password); code != http.StatusOK {
			t.Fatalf("sign-in of %s = %d %s", who, code, body)
		}
		if who == oidctest.Ada {
			b.waitFor("Ada to be on call", func() bool { who, _ := b.onCall("primary"); return who == oidctest.Ada })
		}
	}
	b.waitFor("Ada's Slack account to be linked", func() bool {
		var n int
		err := b.db.QueryRow(context.Background(), `SELECT count(*) FROM user_identities`).Scan(&n)
		return err == nil && n == 1
	})
	var grace string
	if err := b.db.QueryRow(context.Background(), `SELECT COALESCE(NULLIF(display_name, ''), email) FROM users WHERE email = $1`, oidctest.Grace).Scan(&grace); err != nil {
		t.Fatal(err)
	}

	if code, res := b.webhook(diskFull); code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	// The second step tells the team and the channel.
	msg := b.find("the channel's message", inChannel("C0ALERTS", "Disk is full on db1", "Paged: "))
	// The blocks are compared as JSON, in which the angle brackets of a
	// mention are escaped.
	if want := `Paged: \u003c@U0ADA\u003e, ` + grace + " (Slack not connected)"; !strings.Contains(msg.JSON(), want) {
		t.Errorf("the channel's message does not contain %q:\n%s", want, msg.JSON())
	}
	// Grace is not mentioned and got no message.
	if strings.Contains(msg.JSON(), "U0GRACE") {
		t.Errorf("the message mentions an account that is not linked:\n%s", msg.JSON())
	}
	for _, m := range b.slack.Messages() {
		if m.Channel != "C0ALERTS" && m.Channel != slacktest.DM("U0ADA") {
			t.Errorf("a message went to %s", m.Channel)
		}
	}
	var shown, reason string
	err := b.db.QueryRow(context.Background(), `SELECT s->>'shown_as', s->>'reason' FROM alert_events e, jsonb_array_elements(e.data->'skipped') s
		WHERE e.type = 'escalated' AND s->>'user' = $1`, oidctest.Grace).Scan(&shown, &reason)
	if err != nil || shown != grace+" (Slack not connected)" || reason != "the person has no linked account for this integration" {
		t.Errorf("on the timeline Grace is shown as %q, skipped because %q, %v", shown, reason, err)
	}
}

// TestEscalationSurvivesARestart stops the process between two steps of a
// policy. The timer of the second step is in Postgres: two processes that
// start afterwards find it, and exactly one of them runs the step. The
// policy then runs out, which is recorded once.
func TestEscalationSurvivesARestart(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	// Six seconds leave the first process time to stop before the second
	// step is due, also on a slow machine.
	cfg := strings.Replace(configuration, "        timeout: 5m\n", "        timeout: 6s\n      - notify:\n          - channel: C0SECOND\n            integration: slack\n        timeout: 2s\n", 1)
	first := launch(t, dbURL, writeConfig(t, cfg))
	first.waitUntilServing()
	if code, res := first.webhook(diskFull); code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	first.message("the first step", contains("Disk is full on db1"))
	if code := first.terminate(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if steps, status := first.escalationSteps(); len(steps) != 1 || status != "active, timer running" {
		t.Fatalf("when the process stopped: %s after %v", status, steps)
	}

	replicas := []*beacon{launch(t, dbURL, writeConfig(t, cfg)), launch(t, dbURL, writeConfig(t, cfg))}
	for _, r := range replicas {
		r.slack.AddChannel("second-line", "C0SECOND")
	}
	for _, r := range replicas {
		r.waitUntilServing()
	}
	first.waitFor("the policy to run out", func() bool {
		var n int
		err := first.db.QueryRow(context.Background(), `SELECT count(*) FROM alert_events WHERE type = 'escalation_exhausted'`).Scan(&n)
		return err == nil && n > 0
	})
	steps, status := first.escalationSteps()
	if want := "1.1: channel C0ALERTS | 1.2: channel C0SECOND"; strings.Join(steps, " | ") != want || status != "exhausted" {
		t.Errorf("escalation: %s after %q; want exhausted after %q", status, strings.Join(steps, " | "), want)
	}
	var exhausted, pages int
	err := first.db.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM alert_events WHERE type = 'escalation_exhausted'),
		(SELECT count(*) FROM notifications WHERE reason = 'escalation_step')`).Scan(&exhausted, &pages)
	if err != nil || exhausted != 1 || pages != 2 {
		t.Errorf("%d exhausted events and %d pages, %v; want 1 and 2", exhausted, pages, err)
	}
	// The second step reached Slack once, through one of the two.
	first.waitFor("the second step in Slack", func() bool {
		return len(replicas[0].slack.Calls("chat.postMessage"))+len(replicas[1].slack.Calls("chat.postMessage")) >= 1
	})
	if n := len(replicas[0].slack.Calls("chat.postMessage")) + len(replicas[1].slack.Calls("chat.postMessage")); n != 1 {
		t.Errorf("the second step was posted %d times, want once", n)
	}
}

// TestRolesAreEnforced signs in a member and an admin through the identity
// provider and sends both to a route that only an admin may use.
func TestRolesAreEnforced(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, signInConfiguration(t)))
	b.waitUntilServing()
	addr := strings.TrimPrefix(b.url, "http://")
	const reload = "/api/v1/config/reload"

	stranger := oidctest.Browser(t, addr)
	if code, body := do(t, stranger, http.MethodPost, reload); code != http.StatusUnauthorized {
		t.Errorf("reload without a session = %d %s, want 401", code, body)
	}

	// Grace is a member: she may ask who she is, and may not reload.
	member := oidctest.Browser(t, addr)
	if code, body := oidctest.SignIn(t, member, oidctest.Grace, oidctest.Password); code != http.StatusOK || !strings.Contains(body, `"role":"member"`) {
		t.Fatalf("Grace after sign-in = %d %s", code, body)
	}
	if code, body := do(t, member, http.MethodPost, reload); code != http.StatusForbidden || !strings.Contains(body, "needs the admin role") {
		t.Errorf("reload as a member = %d %s, want 403", code, body)
	}
	if strings.Contains(b.output(), `"msg":"configuration reloaded"`) {
		t.Error("a member reloaded the configuration")
	}

	// Ada is an admin.
	admin := oidctest.Browser(t, addr)
	if code, body := oidctest.SignIn(t, admin, oidctest.Ada, oidctest.Password); code != http.StatusOK || !strings.Contains(body, `"role":"admin"`) {
		t.Fatalf("Ada after sign-in = %d %s", code, body)
	}
	if code, body := do(t, admin, http.MethodPost, reload); code != http.StatusOK || !strings.Contains(body, `"result":"reloaded"`) {
		t.Errorf("reload as an admin = %d %s, want 200", code, body)
	}
	if !strings.Contains(b.output(), `"msg":"configuration reloaded","trigger":"api"`) {
		t.Error("the reload by the admin is not in the logs")
	}

	// An invalid configuration is refused through the endpoint as it is by
	// the signal, and the answer says what is wrong.
	broken := strings.Replace(signInConfiguration(t), "escalation_policy: default", "escalation_policy: missing", 1)
	if err := os.WriteFile(filepath.Join(b.dir, "beacon.yaml"), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, admin, http.MethodPost, reload)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"field":"default.escalation_policy"`) || !strings.Contains(body, "the previous configuration stays active") {
		t.Errorf("reload of an invalid configuration = %d %s, want 422 with the problem", code, body)
	}
	if code, res := b.webhook(diskFull); code != http.StatusCreated {
		t.Errorf("webhook after the rejected reload = %d %v", code, res)
	}
	// The admin's session is still good, and a member still cannot reload.
	if code, _ := do(t, admin, http.MethodGet, "/api/v1/me"); code != http.StatusOK {
		t.Errorf("GET /api/v1/me as the admin = %d", code)
	}
}

// bootstrapPassword is the password of the bootstrap admin in these tests.
const bootstrapPassword = "e2e-bootstrap-password-marker-7f3c"

var (
	hashOnce     sync.Once
	hashedSecret string
)

// bootstrapHash returns the bcrypt hash of bootstrapPassword, which is what
// Beacon is given.
func bootstrapHash(t *testing.T) string {
	t.Helper()
	hashOnce.Do(func() {
		hash, err := bcrypt.GenerateFromPassword([]byte(bootstrapPassword), 10)
		if err != nil {
			t.Fatalf("hash the bootstrap password: %v", err)
		}
		hashedSecret = string(hash)
	})
	return hashedSecret
}

// api sends a request with a JSON body and an optional API token, and
// returns the status and the decoded body.
func (b *beacon) api(client *http.Client, method, path, token, body string) (int, map[string]any) {
	b.t.Helper()
	req, err := http.NewRequest(method, oidctest.PublicURL+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatalf("read %s: %v", path, err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// TestBootstrapAdminAndTokens is the path of a new install with no identity
// provider: the bootstrap admin signs in, creates tokens for automation, and
// is then turned off. A member token must not be able to do what only an
// admin may.
func TestBootstrapAdminAndTokens(t *testing.T) {
	withBootstrap := configuration + `auth:
  public_url: ` + oidctest.PublicURL + `
  bootstrap_admin:
    password_hash: {env: BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH}
`
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, withBootstrap))
	b.waitUntilServing()
	addr := strings.TrimPrefix(b.url, "http://")
	browser := oidctest.Browser(t, addr)
	// A client with no cookies, for requests that carry only a token.
	bare := oidctest.Browser(t, addr)
	bare.Jar = nil

	// There is no identity provider, and the wrong password gets nowhere.
	if code, _ := b.api(browser, "GET", "/auth/login", "", ""); code != http.StatusNotFound {
		t.Errorf("GET /auth/login without a provider = %d, want 404", code)
	}
	if code, _ := b.api(browser, "POST", "/auth/bootstrap", "", `{"password": "not-the-password"}`); code != http.StatusUnauthorized {
		t.Errorf("bootstrap sign-in with a wrong password = %d, want 401", code)
	}
	code, who := b.api(browser, "POST", "/auth/bootstrap", "", `{"password": "`+bootstrapPassword+`"}`)
	if code != http.StatusOK || who["role"] != "admin" {
		t.Fatalf("bootstrap sign-in = %d %v", code, who)
	}

	// The admin creates a token of each role.
	create := func(name, role string) string {
		t.Helper()
		code, created := b.api(browser, "POST", "/api/v1/tokens", "", `{"name": "`+name+`", "role": "`+role+`"}`)
		token, _ := created["token"].(string)
		if code != http.StatusCreated || token == "" {
			t.Fatalf("create the %s token = %d %v", role, code, created)
		}
		return token
	}
	memberToken, adminToken := create("dashboard", "member"), create("deploy", "admin")

	// The member token is good for what a member may do, and for nothing
	// that needs an admin.
	if code, who := b.api(bare, "GET", "/api/v1/me", memberToken, ""); code != http.StatusOK || who["role"] != "member" || who["display_name"] != "dashboard" {
		t.Errorf("GET /api/v1/me with the member token = %d %v", code, who)
	}
	for _, denied := range [][3]string{
		{"POST", "/api/v1/config/reload", ""},
		{"GET", "/api/v1/tokens", ""},
		{"POST", "/api/v1/tokens", `{"name": "more", "role": "admin"}`},
	} {
		if code, body := b.api(bare, denied[0], denied[1], memberToken, denied[2]); code != http.StatusForbidden {
			t.Errorf("%s %s with the member token = %d %v, want 403", denied[0], denied[1], code, body)
		}
	}
	// The admin token does all of it.
	if code, body := b.api(bare, "POST", "/api/v1/config/reload", adminToken, ""); code != http.StatusOK {
		t.Errorf("reload with the admin token = %d %v", code, body)
	}
	code, list := b.api(bare, "GET", "/api/v1/tokens", adminToken, "")
	tokens, _ := list["tokens"].([]any)
	if code != http.StatusOK || len(tokens) != 2 {
		t.Fatalf("list with the admin token = %d %v", code, list)
	}
	// No credential at all, and one that was never issued.
	for _, token := range []string{"", "bcn_never-issued"} {
		if code, _ := b.api(bare, "POST", "/api/v1/config/reload", token, ""); code != http.StatusUnauthorized {
			t.Errorf("reload with the token %q = %d, want 401", token, code)
		}
	}

	// Revoking the member token ends its access.
	var memberID string
	for _, item := range tokens {
		if tok := item.(map[string]any); tok["name"] == "dashboard" {
			memberID, _ = tok["id"].(string)
		}
	}
	if code, _ := b.api(bare, "DELETE", "/api/v1/tokens/"+memberID, adminToken, ""); code != http.StatusNoContent {
		t.Errorf("revoke = %d, want 204", code)
	}
	if code, _ := b.api(bare, "GET", "/api/v1/me", memberToken, ""); code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/me with the revoked token = %d, want 401", code)
	}

	// The bootstrap admin is taken out of the configuration, and the admin
	// token reloads it. The account's session ends, signing in is gone, and
	// the token keeps working: automation does not depend on the account.
	if err := os.WriteFile(filepath.Join(b.dir, "beacon.yaml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := b.api(bare, "POST", "/api/v1/config/reload", adminToken, ""); code != http.StatusOK {
		t.Fatalf("reload without the bootstrap admin = %d %v", code, body)
	}
	if code, _ := b.api(browser, "GET", "/api/v1/me", "", ""); code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/me with the bootstrap admin's session after it was turned off = %d, want 401", code)
	}
	if code, _ := b.api(browser, "POST", "/auth/bootstrap", "", `{"password": "`+bootstrapPassword+`"}`); code != http.StatusNotFound {
		t.Errorf("bootstrap sign-in after it was turned off = %d, want 404", code)
	}
	if code, _ := b.api(bare, "GET", "/api/v1/me", adminToken, ""); code != http.StatusOK {
		t.Errorf("GET /api/v1/me with the admin token after the reload = %d, want 200", code)
	}

	// The tokens are stored as hashes, and nothing secret was logged.
	var stored int
	if err := b.db.QueryRow(context.Background(), `SELECT count(*) FROM api_tokens WHERE token_hash IN ($1, $2)`, memberToken, adminToken).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("%d tokens are stored as they are, %v", stored, err)
	}
	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	logs := b.output()
	for name, secret := range map[string]string{"the member token": memberToken, "the admin token": adminToken, "the bootstrap password": bootstrapPassword,
		"the bootstrap password hash": bootstrapHash(t), "a wrong password": "not-the-password", "the source token": sourceToken} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain %s", name)
		}
	}
}

// bootstrapConfiguration is the test configuration with the bootstrap admin
// as the way to sign in.
const bootstrapConfiguration = configuration + `auth:
  public_url: ` + oidctest.PublicURL + `
  bootstrap_admin:
    password_hash: {env: BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH}
`

// signedIn starts Beacon with the bootstrap admin and returns a browser that
// is signed in as that admin, a client without cookies, and a member token.
func signedIn(t *testing.T) (b *beacon, admin, bare *http.Client, memberToken string) {
	t.Helper()
	dbURL := postgrestest.NewURL(t)
	b = launch(t, dbURL, writeConfig(t, bootstrapConfiguration))
	b.waitUntilServing()
	addr := strings.TrimPrefix(b.url, "http://")
	admin = oidctest.Browser(t, addr)
	bare = oidctest.Browser(t, addr)
	bare.Jar = nil
	if code, who := b.api(admin, "POST", "/auth/bootstrap", "", `{"password": "`+bootstrapPassword+`"}`); code != http.StatusOK || who["role"] != "admin" {
		t.Fatalf("bootstrap sign-in = %d %v", code, who)
	}
	code, created := b.api(admin, "POST", "/api/v1/tokens", "", `{"name": "responder", "role": "member"}`)
	memberToken, _ = created["token"].(string)
	if code != http.StatusCreated || memberToken == "" {
		t.Fatalf("create the member token = %d %v", code, created)
	}
	return b, admin, bare, memberToken
}

// TestSilencesAPI creates, reads and expires silences through the real
// binary, as a member with a token and as an admin with a session.
func TestSilencesAPI(t *testing.T) {
	b, admin, bare, memberToken := signedIn(t)
	const request = `{"matchers": [{"label": "alertname", "operator": "=", "value": "DiskFull"}], "duration": "1h", "comment": "replacing the disk"}`

	// Nobody without a credential, and nobody with one that was never issued.
	for _, probe := range [][2]string{{"GET", "/api/v1/silences"}, {"POST", "/api/v1/silences"}, {"GET", "/api/v1/silences/x"}, {"DELETE", "/api/v1/silences/x"}} {
		for _, token := range []string{"", "bcn_never-issued"} {
			if code, body := b.api(bare, probe[0], probe[1], token, request); code != http.StatusUnauthorized {
				t.Errorf("%s %s with the token %q = %d %v, want 401", probe[0], probe[1], token, code, body)
			}
		}
	}
	// The token of a source is not a credential for the API.
	if code, _ := b.api(bare, "POST", "/api/v1/silences", sourceToken, request); code != http.StatusUnauthorized {
		t.Errorf("create with the source token = %d, want 401", code)
	}

	// A member creates one with a token: the token is the author.
	code, byMember := b.api(bare, "POST", "/api/v1/silences", memberToken, request)
	if code != http.StatusCreated || byMember["created_by_token"] != "responder" || byMember["created_by"] != nil || byMember["state"] != "active" {
		t.Fatalf("create as a member = %d %v", code, byMember)
	}
	// An admin creates one in the browser: the user is the author.
	code, byAdmin := b.api(admin, "POST", "/api/v1/silences", "", `{"matchers": [{"label": "team", "operator": "=~", "value": "dba|storage"}], "duration": "30m", "comment": "migration"}`)
	if code != http.StatusCreated || byAdmin["created_by"] != "bootstrap-admin@beacon.invalid" || byAdmin["created_by_token"] != nil {
		t.Fatalf("create as an admin = %d %v", code, byAdmin)
	}
	// A request that is not a silence is refused, with the field at fault.
	if code, body := b.api(bare, "POST", "/api/v1/silences", memberToken, `{"matchers": [{"label": "a", "operator": "=~", "value": "(x"}], "duration": "1h", "comment": "x"}`); code != http.StatusBadRequest || body["field"] != "matchers[0].value" {
		t.Errorf("a broken pattern = %d %v, want 400 at matchers[0].value", code, body)
	}

	// Both see both, and the member may end the admin's.
	for who, client := range map[string]*http.Client{"member": bare, "admin": admin} {
		token := map[string]string{"member": memberToken}[who]
		code, list := b.api(client, "GET", "/api/v1/silences", token, "")
		if silences, _ := list["silences"].([]any); code != http.StatusOK || len(silences) != 2 {
			t.Errorf("list as the %s = %d %v, want both silences", who, code, list)
		}
	}
	adminsID, _ := byAdmin["id"].(string)
	if code, body := b.api(bare, "DELETE", "/api/v1/silences/"+adminsID, memberToken, ""); code != http.StatusNoContent {
		t.Fatalf("a member expires the admin's silence = %d %v, want 204", code, body)
	}
	if code, got := b.api(admin, "GET", "/api/v1/silences/"+adminsID, "", ""); code != http.StatusOK || got["state"] != "expired" {
		t.Errorf("the expired silence = %d %v", code, got)
	}
	code, list := b.api(bare, "GET", "/api/v1/silences", memberToken, "")
	if silences, _ := list["silences"].([]any); code != http.StatusOK || len(silences) != 1 {
		t.Errorf("list after the expiry = %d %v, want the one that is left", code, list)
	}

	// What is stored: the matchers, exactly one author each, and a period
	// that the expiry cut short. The periods are compared with each other, not
	// with the database's clock, which need not agree with Beacon's.
	rows, err := b.db.Query(context.Background(), `SELECT comment || ': ' || matchers::text || ' user=' || (created_by IS NOT NULL)::text || ' token=' || (created_by_token IS NOT NULL)::text ||
		' cut_short=' || (ends_at - starts_at < interval '30 minutes')::text FROM silences ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := pgx.CollectRows(rows, pgx.RowTo[string])
	want := []string{
		`replacing the disk: [{"label": "alertname", "value": "DiskFull", "operator": "="}] user=false token=true cut_short=false`,
		`migration: [{"label": "team", "value": "dba|storage", "operator": "=~"}] user=true token=false cut_short=true`,
	}
	if err != nil || strings.Join(stored, "\n") != strings.Join(want, "\n") {
		t.Errorf("stored:\n%s\n%v\nwant:\n%s", strings.Join(stored, "\n"), err, strings.Join(want, "\n"))
	}
	for _, secret := range []string{memberToken, sourceToken} {
		if strings.Contains(b.output(), secret) {
			t.Errorf("the logs contain a credential")
		}
	}
}

// TestSilences follows alerts under a silence through the real binary: an
// alert that arrives while a silence matches it is stored and tells nobody;
// the rest of its group is told about without it; and when the silence is
// expired, or runs out, what it held back is told.
func TestSilences(t *testing.T) {
	b, admin, bare, memberToken := signedIn(t)
	fire := func(alertname, host string) string {
		t.Helper()
		body := fmt.Sprintf(`{"labels": {"alertname": %q, "host": %q}, "annotations": {"summary": %q}}`, alertname, host, alertname+" on "+host)
		code, res := b.webhook(body)
		if code != http.StatusCreated {
			t.Fatalf("webhook = %d %v", code, res)
		}
		return res["alert_id"]
	}
	silenceHost := func(client *http.Client, token, host, duration string) string {
		t.Helper()
		code, created := b.api(client, "POST", "/api/v1/silences", token,
			fmt.Sprintf(`{"matchers": [{"label": "host", "operator": "=", "value": %q}], "duration": %q, "comment": "work on %s"}`, host, duration, host))
		if code != http.StatusCreated {
			t.Fatalf("create silence = %d %v", code, created)
		}
		return created["id"].(string)
	}
	// heldBack waits until Beacon has looked at the alert's group and found
	// that a silence keeps its notification back.
	heldBack := func(alertID, silenceID string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			var waitsFor string
			err := b.db.QueryRow(context.Background(), `SELECT COALESCE(g.silence_id::text, '') FROM alerts a JOIN alert_groups g ON g.id = a.group_id WHERE a.id = $1`, alertID).Scan(&waitsFor)
			if err != nil {
				t.Fatal(err)
			}
			if waitsFor == silenceID {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the group of the alert waits for %q, want the silence %s", waitsFor, silenceID)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// 1. A member silences db1. An alert from db1 is recorded, and Slack
	// hears nothing.
	silenceID := silenceHost(bare, memberToken, "db1", "1h")
	db1 := fire("DiskFull", "db1")
	heldBack(db1, silenceID)
	if got := b.status(db1); got != "firing" {
		t.Errorf("the silenced alert is %s, want it recorded as firing", got)
	}
	if msgs := b.slack.Messages(); len(msgs) != 0 {
		t.Fatalf("Slack was told about a silenced alert: %+v", msgs)
	}
	if got := b.timeline(db1, 2); strings.Join(got, ",") != "fired by source,notification_silenced by system" {
		t.Errorf("timeline = %v, want it to say that a silence held the notification back", got)
	}

	// 2. An alert of the same group that the silence does not match is
	// told about, alone.
	fire("DiskFull", "db2")
	msg := b.message("the alert that is not silenced", contains("*Firing*", "*DiskFull on db2*"))
	if strings.Contains(msg.JSON(), "db1") {
		t.Errorf("the message shows the silenced alert:\n%s", msg.JSON())
	}

	// 3. An admin expires the silence. db1 still fires, so it is news now:
	// the message shows both.
	if code, body := b.api(admin, "DELETE", "/api/v1/silences/"+silenceID, "", ""); code != http.StatusNoContent {
		t.Fatalf("expire = %d %v", code, body)
	}
	b.message("both alerts once the silence is expired", contains("2 alerts: 2 firing", "DiskFull on db1", "DiskFull on db2"))

	// 4. A silence that runs out by itself: the alert is told about when it
	// ends, by the same timer that would have told it at once.
	short := silenceHost(admin, "", "web1", "3s")
	web1 := fire("HighLoad", "web1")
	heldBack(web1, short)
	b.find("the alert when its silence ran out", contains("*HighLoad on web1*"))
	var early bool
	if err := b.db.QueryRow(context.Background(), `SELECT g.notified_at < s.ends_at FROM alerts a JOIN alert_groups g ON g.id = a.group_id, silences s WHERE a.id = $1 AND s.id = $2`, web1, short).Scan(&early); err != nil || early {
		t.Errorf("the group was notified before its silence ended: %v, %v", early, err)
	}
	if n := len(b.slack.Calls("chat.postMessage")); n != 2 {
		t.Errorf("%d messages were posted, want 2", n)
	}
}
