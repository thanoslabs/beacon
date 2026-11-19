package slack_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/slack"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

const signingSecret = "signing-secret-marker-4be1"

var (
	alice = slacktest.User{ID: "U0ALICE", Username: "alice"}
	bob   = slacktest.User{ID: "U0BOB", Username: "bob"}
)

// actionsFixture is the pipeline fixture plus the interactions endpoint.
type actionsFixture struct {
	*pipelineFixture
	url          string
	logs         *bytes.Buffer
	interactions *slack.Interactions
}

func newActionsFixture(t *testing.T) *actionsFixture {
	t.Helper()
	f := &actionsFixture{pipelineFixture: newPipelineFixture(t), logs: &bytes.Buffer{}}
	mux := http.NewServeMux()
	secrets := map[string]config.Secret{"slack": config.NewSecret(config.SecretRef{}, signingSecret)}
	f.interactions = slack.NewInteractions(slog.New(slog.NewJSONHandler(f.logs, nil)), f.alerts, secrets)
	f.interactions.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/api/v1/integrations/slack/interactions"
	return f
}

// posted fires an alert and returns it with its Slack message.
func (f *actionsFixture) posted(t *testing.T) (alert.Alert, slacktest.Message) {
	t.Helper()
	a := f.fire(t)
	f.attempts(t)
	msgs := f.fake.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want 1", len(msgs))
	}
	return a, msgs[0]
}

func (f *actionsFixture) status(t *testing.T, id string) alert.Alert {
	t.Helper()
	a, err := f.alerts.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return a
}

func TestAcknowledgeFromSlack(t *testing.T) {
	f := newActionsFixture(t)
	a, msg := f.posted(t)

	if code := f.fake.Click(t, f.url, signingSecret, msg, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("click = %d, want 200", code)
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusAcknowledged {
		t.Fatalf("status = %s, want acknowledged", got.Status)
	}

	// The timeline names the Slack user, in the form identity linking uses.
	events, err := f.alerts.Timeline(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	var ack *alert.Event
	for i := range events {
		if events[i].Type == alert.EventAcknowledged {
			ack = &events[i]
		}
	}
	if ack == nil {
		t.Fatalf("no acknowledged event on the timeline: %+v", events)
	}
	want := alert.Actor{Type: alert.ActorUser, Provider: "slack", ExternalID: "U0ALICE", Name: "alice"}
	if ack.Actor != want {
		t.Errorf("actor = %+v, want %+v", ack.Actor, want)
	}

	// The message is updated in place and offers what is still possible.
	f.attempts(t)
	msgs := f.fake.Messages()
	if len(msgs) != 1 || msgs[0].Updates != 1 {
		t.Fatalf("%d messages with %d updates, want the one message updated once", len(msgs), msgs[0].Updates)
	}
	updated := msgs[0].JSON()
	if !strings.Contains(updated, "Acknowledged by alice") || strings.Contains(updated, "beacon_acknowledge") ||
		!strings.Contains(updated, "beacon_snooze") || !strings.Contains(updated, "beacon_resolve") {
		t.Errorf("updated message: %s", updated)
	}
	if n := len(f.fake.Responses()); n != 0 {
		t.Errorf("%d messages were sent to the response URL for an action that worked", n)
	}
}

func TestSnoozeAndResolveFromSlack(t *testing.T) {
	f := newActionsFixture(t)
	a, msg := f.posted(t)

	before := time.Now()
	if code := f.fake.Click(t, f.url, signingSecret, msg, "beacon_snooze", bob); code != http.StatusOK {
		t.Fatalf("snooze = %d", code)
	}
	got := f.status(t, a.ID)
	if got.Status != alert.StatusSnoozed || got.SnoozedUntil == nil {
		t.Fatalf("status = %s until %v, want snoozed", got.Status, got.SnoozedUntil)
	}
	if d := got.SnoozedUntil.Sub(before); d < time.Hour-time.Second || d > time.Hour+time.Minute {
		t.Errorf("snoozed for %v, want an hour", d)
	}
	f.attempts(t)
	msg = f.fake.Messages()[0]
	if !strings.Contains(msg.JSON(), "Snoozed by bob until") {
		t.Errorf("message after snooze: %s", msg.JSON())
	}

	if code := f.fake.Click(t, f.url, signingSecret, msg, "beacon_resolve", alice); code != http.StatusOK {
		t.Fatalf("resolve = %d", code)
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusResolved {
		t.Fatalf("status = %s, want resolved", got.Status)
	}
	f.attempts(t)
	msg = f.fake.Messages()[0]
	if !strings.Contains(msg.JSON(), "Resolved by alice") || strings.Contains(msg.JSON(), `"actions"`) {
		t.Errorf("message after resolve should name alice and have no buttons: %s", msg.JSON())
	}
}

// TestActingOnAnAlertThatAlreadyChanged presses buttons on a stale copy of the
// message, as two people looking at the same alert would.
func TestActingOnAnAlertThatAlreadyChanged(t *testing.T) {
	f := newActionsFixture(t)
	a, stale := f.posted(t)

	if code := f.fake.Click(t, f.url, signingSecret, stale, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("first click = %d", code)
	}
	if code := f.fake.Click(t, f.url, signingSecret, stale, "beacon_acknowledge", bob); code != http.StatusOK {
		t.Fatalf("second click = %d, want 200 so Slack shows no error", code)
	}
	responses := f.fake.Responses()
	if len(responses) != 1 {
		t.Fatalf("%d responses, want one telling bob what happened", len(responses))
	}
	if r := responses[0]; r["text"] != "Nothing changed: this alert is already acknowledged." || r["response_type"] != "ephemeral" || r["replace_original"] != false {
		t.Errorf("response = %v", r)
	}

	// Bob's click changed nothing and is not on the timeline.
	events, err := f.alerts.Timeline(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	for _, e := range events {
		if e.Actor.ExternalID == "U0BOB" {
			t.Errorf("the refused action is on the timeline: %+v", e)
		}
	}

	if _, _, err := f.alerts.ResolveFromSource(context.Background(), f.source, "fp", time.Time{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for action, want := range map[string]string{
		"beacon_acknowledge": "Nothing changed: this alert is already resolved.",
		"beacon_snooze":      "Nothing changed: this alert is already resolved.",
		"beacon_resolve":     "Nothing changed: this alert is already resolved.",
	} {
		if code := f.fake.Click(t, f.url, signingSecret, stale, action, bob); code != http.StatusOK {
			t.Errorf("%s on a resolved alert = %d", action, code)
		}
		all := f.fake.Responses()
		if got := all[len(all)-1]["text"]; got != want {
			t.Errorf("%s on a resolved alert: response = %q, want %q", action, got, want)
		}
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusResolved {
		t.Errorf("status = %s, want resolved", got.Status)
	}
}

func TestActionOnAnUnknownAlert(t *testing.T) {
	f := newActionsFixture(t)
	_, msg := f.posted(t)
	for _, value := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid", "", "'; DROP TABLE alerts; --"} {
		forged := msg
		forged.Blocks = []map[string]any{{"type": "actions", "elements": []any{map[string]any{"action_id": "beacon_resolve", "value": value}}}}
		before := len(f.fake.Responses())
		if code := f.fake.Click(t, f.url, signingSecret, forged, "beacon_resolve", alice); code != http.StatusOK {
			t.Errorf("value %q: click = %d, want 200", value, code)
		}
		all := f.fake.Responses()
		if len(all) != before+1 || all[len(all)-1]["text"] != "Beacon does not know this alert any more." {
			t.Errorf("value %q: responses = %v", value, all[before:])
		}
	}
}

// send posts a body to the interactions endpoint with the given headers.
func send(t *testing.T, url string, body []byte, timestamp, signature string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if timestamp != "" {
		req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	}
	if signature != "" {
		req.Header.Set("X-Slack-Signature", signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	return resp.StatusCode
}

func TestSignatureVerification(t *testing.T) {
	f := newActionsFixture(t)
	a, msg := f.posted(t)
	body := f.fake.InteractionBody(t, msg, "beacon_resolve", alice)
	now := time.Now()
	ts, sig := slacktest.Sign(signingSecret, now, body)
	_, wrongSecret := slacktest.Sign("another-secret", now, body)
	oldTS, oldSig := slacktest.Sign(signingSecret, now.Add(-6*time.Minute), body)
	futureTS, futureSig := slacktest.Sign(signingSecret, now.Add(6*time.Minute), body)
	tampered := bytes.Replace(body, []byte("U0ALICE"), []byte("U0EVIL0"), 1)

	refused := []struct {
		name          string
		url           string
		body          []byte
		ts, signature string
	}{
		{"no headers", f.url, body, "", ""},
		{"no signature", f.url, body, ts, ""},
		{"no timestamp", f.url, body, "", sig},
		{"signed with another secret", f.url, body, ts, wrongSecret},
		{"body changed after signing", f.url, tampered, ts, sig},
		{"timestamp changed after signing", f.url, body, oldTS, sig},
		{"signed six minutes ago", f.url, body, oldTS, oldSig},
		{"signed six minutes ahead", f.url, body, futureTS, futureSig},
		{"timestamp is not a number", f.url, body, "now", sig},
		{"signature without its version", f.url, body, ts, strings.TrimPrefix(sig, "v0=")},
		{"signature in upper case", f.url, body, ts, "v0=" + strings.ToUpper(strings.TrimPrefix(sig, "v0="))},
		{"another integration's path", strings.Replace(f.url, "/slack/", "/other/", 1), body, ts, sig},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			if code := send(t, tt.url, tt.body, tt.ts, tt.signature); code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", code)
			}
			if got := f.status(t, a.ID); got.Status != alert.StatusFiring {
				t.Fatalf("a refused request changed the alert to %s", got.Status)
			}
		})
	}

	// The same request with its own signature is accepted, which shows that
	// the cases above were refused for their signatures alone.
	if code := send(t, f.url, body, ts, sig); code != http.StatusOK {
		t.Fatalf("correctly signed request = %d, want 200", code)
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusResolved {
		t.Errorf("status = %s, want resolved", got.Status)
	}

	logs := f.logs.String()
	for _, secret := range []string{signingSecret, sig, strings.TrimPrefix(sig, "v0="), "response/"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs contain %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, "slack request refused") {
		t.Errorf("refused requests are not logged:\n%s", logs)
	}
}

func TestInteractionsBeaconDoesNotUse(t *testing.T) {
	f := newActionsFixture(t)
	a, _ := f.posted(t)
	post := func(form string) int {
		ts, sig := slacktest.Sign(signingSecret, time.Now(), []byte(form))
		return send(t, f.url, []byte(form), ts, sig)
	}
	accepted := map[string]string{
		"another kind of interaction": `payload={"type":"view_submission","user":{"id":"U1"}}`,
		"a button that is not ours":   `payload={"type":"block_actions","user":{"id":"U1"},"actions":[{"action_id":"other_app","value":"x"}]}`,
		"no actions":                  `payload={"type":"block_actions","user":{"id":"U1"}}`,
	}
	for name, form := range accepted {
		if code := post(form); code != http.StatusOK {
			t.Errorf("%s = %d, want 200", name, code)
		}
	}
	for name, form := range map[string]string{"no payload": `x=1`, "payload is not JSON": `payload=hello`, "not a form": `%zz`} {
		if code := post(form); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusFiring {
		t.Errorf("status = %s, want firing", got.Status)
	}
}

// brokenAlerts stands in for a database that is down.
type brokenAlerts struct{}

func (brokenAlerts) ApplyToGroup(context.Context, string, alert.Action, alert.Actor, alert.Options) ([]alert.Alert, error) {
	return nil, errors.New("connection refused to db.internal")
}

func TestStoreFailureAnswersTheUser(t *testing.T) {
	fake := slacktest.New(t, botToken)
	mux := http.NewServeMux()
	secrets := map[string]config.Secret{"slack": config.NewSecret(config.SecretRef{}, signingSecret)}
	slack.NewInteractions(slog.New(slog.DiscardHandler), brokenAlerts{}, secrets).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	msg := slacktest.Message{Channel: "C0ALERTS", TS: "1.2", Blocks: []map[string]any{{"type": "actions", "elements": []any{
		map[string]any{"action_id": "beacon_acknowledge", "value": "0f8b1c1e-6a0e-4b0e-9a57-0d7f6d4f3c11"}}}}}
	if code := fake.Click(t, srv.URL+"/api/v1/integrations/slack/interactions", signingSecret, msg, "beacon_acknowledge", alice); code != http.StatusInternalServerError {
		t.Errorf("click = %d, want 500", code)
	}
	responses := fake.Responses()
	if len(responses) != 1 || !strings.Contains(responses[0]["text"].(string), "could not apply that") || strings.Contains(responses[0]["text"].(string), "db.internal") {
		t.Errorf("responses = %v, want one that does not repeat the cause", responses)
	}
}
