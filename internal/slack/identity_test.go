package slack_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/slack"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

func TestLookupByEmail(t *testing.T) {
	fake := slacktest.New(t, botToken)
	fake.AddUser("ada@example.com", "U0ADA")
	client := slack.NewClient(fake.URL, config.NewSecret(config.SecretRef{}, botToken))
	ctx := context.Background()

	id, found, err := client.LookupByEmail(ctx, "ada@example.com")
	if id != "U0ADA" || !found || err != nil {
		t.Fatalf("LookupByEmail = %q, %v, %v", id, found, err)
	}
	// The address is an argument of the request, in the query, as the
	// method is documented.
	calls := fake.Calls("users.lookupByEmail")
	if len(calls) != 1 || calls[0].Body["email"] != "ada@example.com" {
		t.Errorf("users.lookupByEmail calls = %+v", calls)
	}

	// An address with characters that mean something in a query.
	fake.AddUser("ada+oncall&x=1@example.com", "U0PLUS")
	if id, found, err := client.LookupByEmail(ctx, "ada+oncall&x=1@example.com"); id != "U0PLUS" || !found || err != nil {
		t.Errorf("LookupByEmail of an address with a plus = %q, %v, %v", id, found, err)
	}

	// Nobody has the address, or the member was deactivated: not an error.
	if id, found, err := client.LookupByEmail(ctx, "nobody@example.com"); id != "" || found || err != nil {
		t.Errorf("LookupByEmail(unknown) = %q, %v, %v", id, found, err)
	}
	fake.RemoveUser("ada@example.com")
	if _, found, err := client.LookupByEmail(ctx, "ada@example.com"); found || err != nil {
		t.Errorf("LookupByEmail(deactivated) = %v, %v", found, err)
	}
}

func TestLookupByEmailFailures(t *testing.T) {
	fake := slacktest.New(t, botToken)
	fake.AddUser("ada@example.com", "U0ADA")
	ctx := context.Background()

	// The app lacks the scope.
	fake.Fail("users.lookupByEmail", slacktest.Failure{Error: "missing_scope"})
	client := slack.NewClient(fake.URL, config.NewSecret(config.SecretRef{}, botToken))
	_, found, err := client.LookupByEmail(ctx, "ada@example.com")
	var apiErr *slack.APIError
	if found || !errors.As(err, &apiErr) || apiErr.Code != "missing_scope" || !strings.Contains(err.Error(), "users:read.email") {
		t.Errorf("LookupByEmail without the scope = %v, %v", found, err)
	}
	// Rate limited, and unavailable.
	fake.Fail("users.lookupByEmail", slacktest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "30"}, slacktest.Failure{Status: http.StatusServiceUnavailable})
	for range 2 {
		if _, found, err := client.LookupByEmail(ctx, "ada@example.com"); found || err == nil {
			t.Errorf("LookupByEmail while Slack fails = %v, %v", found, err)
		}
	}

	// A wrong token, and a Slack that cannot be reached: neither error
	// holds the token or the address that was asked for.
	const wrong = "xoxb-wrong-token-marker-77aa"
	for name, c := range map[string]*slack.Client{
		"wrong token": slack.NewClient(fake.URL, config.NewSecret(config.SecretRef{}, wrong)),
		"unreachable": slack.NewClient("http://127.0.0.1:1", config.NewSecret(config.SecretRef{}, wrong)),
	} {
		_, found, err := c.LookupByEmail(ctx, "secret-address@example.com")
		if found || err == nil || strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), "secret-address") {
			t.Errorf("%s: LookupByEmail = %v, %v", name, found, err)
		}
	}
}

// directory answers who a Slack account belongs to.
type directory map[string][2]string

func (d directory) UserFor(_ context.Context, integration, externalID string) (userID, name string, found bool, err error) {
	if integration != "slack" {
		return "", "", false, errors.New("asked about another integration")
	}
	u, ok := d[externalID]
	return u[0], u[1], ok, nil
}

type brokenDirectory struct{}

func (brokenDirectory) UserFor(context.Context, string, string) (string, string, bool, error) {
	return "", "", false, errors.New("the database is away")
}

// TestActionsAreAttributedToTheLinkedUser presses buttons as two Slack
// accounts: one that is linked to a Beacon user and one that is not. The
// first is recorded as that user, who also becomes the one who acknowledged
// the alert; the second acts all the same, and is recorded as the Slack
// account.
func TestActionsAreAttributedToTheLinkedUser(t *testing.T) {
	f := newActionsFixture(t)
	var ada string
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO users (email, display_name) VALUES ('ada@example.com', 'Ada Lovelace') RETURNING id::text`).Scan(&ada); err != nil {
		t.Fatal(err)
	}
	f.interactions.SetIdentities(directory{"U0ALICE": {ada, "Ada Lovelace"}})
	a, msg := f.posted(t)

	if code := f.fake.Click(t, f.url, signingSecret, msg, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("click = %d, want 200", code)
	}
	last := func(typ alert.EventType) alert.Actor {
		t.Helper()
		events, err := f.alerts.Timeline(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("Timeline: %v", err)
		}
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type == typ {
				return events[i].Actor
			}
		}
		t.Fatalf("no %s event on the timeline", typ)
		return alert.Actor{}
	}
	// The user, and the account they acted from.
	if got, want := last(alert.EventAcknowledged), (alert.Actor{Type: alert.ActorUser, ID: ada, Provider: "slack", ExternalID: "U0ALICE", Name: "Ada Lovelace"}); got != want {
		t.Errorf("actor = %+v, want %+v", got, want)
	}
	var by string
	if err := f.pool.QueryRow(context.Background(), `SELECT COALESCE(acknowledged_by::text, '') FROM alerts WHERE id = $1`, a.ID).Scan(&by); err != nil || by != ada {
		t.Errorf("acknowledged_by = %q, %v; want Ada", by, err)
	}
	f.attempts(t)
	if updated := f.fake.Messages()[0].JSON(); !strings.Contains(updated, "Acknowledged by Ada Lovelace") {
		t.Errorf("the message does not name the Beacon user: %s", updated)
	}

	// Bob is linked to nobody.
	if code := f.fake.Click(t, f.url, signingSecret, f.fake.Messages()[0], "beacon_resolve", bob); code != http.StatusOK {
		t.Fatalf("click = %d, want 200", code)
	}
	if got, want := last(alert.EventResolved), (alert.Actor{Type: alert.ActorUser, Provider: "slack", ExternalID: "U0BOB", Name: "bob"}); got != want {
		t.Errorf("actor = %+v, want %+v", got, want)
	}
	if !strings.Contains(f.logs.String(), `"msg":"slack action by an account that is not linked to a Beacon user; the timeline records the Slack account","integration":"slack","slack_user":"U0BOB"`) {
		t.Errorf("the logs do not mention the account that is not linked:\n%s", f.logs)
	}
}

// TestActionWorksWhenTheLookupFails must not lose a button press because
// identities cannot be read.
func TestActionWorksWhenTheLookupFails(t *testing.T) {
	f := newActionsFixture(t)
	f.interactions.SetIdentities(brokenDirectory{})
	a, msg := f.posted(t)
	if code := f.fake.Click(t, f.url, signingSecret, msg, "beacon_acknowledge", alice); code != http.StatusOK {
		t.Fatalf("click = %d, want 200", code)
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusAcknowledged {
		t.Errorf("status = %s, want acknowledged", got.Status)
	}
	if !strings.Contains(f.logs.String(), `"level":"ERROR","msg":"look up the user of a slack account"`) {
		t.Errorf("the failed lookup was not logged:\n%s", f.logs)
	}
}

// TestDirectMessage posts to a member by user ID, which is how a person is
// paged: Slack opens the conversation between the app and the member, and
// later notifications update the message there.
func TestDirectMessage(t *testing.T) {
	fake := slacktest.New(t, botToken)
	fake.AddUser("ada@example.com", "U0ADA")
	ch := newChannel(fake.URL)
	ctx := context.Background()

	ref, err := ch.Send(ctx, message(alert.StatusFiring, "U0ADA", ""))
	if err != nil || ref != slacktest.DM("U0ADA")+":1700000000.000001" {
		t.Fatalf("Send to a member = %q, %v", ref, err)
	}
	if _, err := ch.Send(ctx, message(alert.StatusAcknowledged, "U0ADA", ref)); err != nil {
		t.Fatalf("update of a direct message: %v", err)
	}
	msgs := fake.Messages()
	if len(msgs) != 1 || msgs[0].Channel != "D0ADA" || msgs[0].Updates != 1 {
		t.Errorf("messages = %+v, want one in the direct conversation, updated once", msgs)
	}
	// Someone who is not a member cannot be written to, and trying again
	// would not help.
	_, err = ch.Send(ctx, message(alert.StatusFiring, "U0NOBODY", ""))
	var apiErr *slack.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "channel_not_found" {
		t.Errorf("Send to an unknown member = %v", err)
	}
}

// TestEscalationStepIsSaidAgain: when an escalation step reaches a
// destination that already has the group's message, the alert is posted
// again, because an edit notifies nobody. The old message becomes a pointer
// without buttons, and updates follow the new one.
func TestEscalationStepIsSaidAgain(t *testing.T) {
	fake := slacktest.New(t, botToken)
	ch := newChannel(fake.URL)
	ctx := context.Background()

	first, err := ch.Send(ctx, message(alert.StatusFiring, "#alerts", ""))
	if err != nil {
		t.Fatal(err)
	}
	again := message(alert.StatusFiring, "#alerts", first)
	again.Notification.Reason = notify.ReasonEscalationStep
	second, err := ch.Send(ctx, again)
	if err != nil || second == first || !strings.HasPrefix(second, "C0ALERTS:") {
		t.Fatalf("Send of a step = %q after %q, %v; want another message", second, first, err)
	}
	msgs := fake.Messages()
	if len(msgs) != 2 {
		t.Fatalf("%d messages, want 2", len(msgs))
	}
	if old := msgs[0]; old.Updates != 1 || !strings.Contains(old.JSON(), "Beacon sent this alert again") || strings.Contains(old.JSON(), "beacon_acknowledge") ||
		old.Text != "Sent again below: [FIRING] critical: Disk is full" {
		t.Errorf("the old message: %q %s", old.Text, old.JSON())
	}
	if fresh := msgs[1]; fresh.Updates != 0 || !strings.Contains(fresh.JSON(), "beacon_acknowledge") || fresh.Text != "[FIRING] critical: Disk is full" {
		t.Errorf("the new message: %q %s", fresh.Text, fresh.JSON())
	}

	// An update goes to the new message, in place.
	if ref, err := ch.Send(ctx, message(alert.StatusAcknowledged, "#alerts", second)); err != nil || ref != second {
		t.Fatalf("update = %q, %v", ref, err)
	}
	if msgs := fake.Messages(); len(msgs) != 2 || msgs[1].Updates != 1 || msgs[0].Updates != 1 {
		t.Errorf("after the update: %+v", msgs)
	}

	// The old message is gone: the step is delivered all the same.
	channelID, ts, _ := strings.Cut(second, ":")
	fake.Delete(channelID, ts)
	again = message(alert.StatusFiring, "#alerts", second)
	again.Notification.Reason = notify.ReasonEscalationStep
	if third, err := ch.Send(ctx, again); err != nil || third == second {
		t.Errorf("Send of a step whose old message was deleted = %q, %v", third, err)
	}
	// And a step that cannot be posted is a failure, with nothing edited.
	fake.Fail("chat.postMessage", slacktest.Failure{Status: http.StatusServiceUnavailable})
	updates := len(fake.Calls("chat.update"))
	if _, err := ch.Send(ctx, again); err == nil || len(fake.Calls("chat.update")) != updates {
		t.Errorf("a step that Slack refused: %v, with %d more edits", err, len(fake.Calls("chat.update"))-updates)
	}
}

// escalated is a step on the timeline, as the engine writes it and the
// database returns it.
func escalated(notified, skipped []map[string]any) alert.Event {
	list := func(in []map[string]any) []any {
		out := make([]any, 0, len(in))
		for _, m := range in {
			out = append(out, m)
		}
		return out
	}
	return alert.Event{Type: notify.EventEscalated, Actor: alert.System(), Data: map[string]any{"notified": list(notified), "skipped": list(skipped)}}
}

// TestMessageNamesWhoWasPaged: the group's message says whom the policy has
// paged so far. Someone with a linked Slack account is mentioned. Someone
// who was to be paged and has none is named in plain text with "(Slack not
// connected)", never as a mention.
func TestMessageNamesWhoWasPaged(t *testing.T) {
	fake := slacktest.New(t, botToken)
	ch := newChannel(fake.URL)
	const here, elsewhere = "integration-here", "integration-elsewhere"
	paged := func(events ...alert.Event) string {
		t.Helper()
		m := message(alert.StatusFiring, "#alerts", "")
		m.Notification.IntegrationID = here
		m.Events = events
		if _, err := ch.Send(context.Background(), m); err != nil {
			t.Fatalf("Send: %v", err)
		}
		msgs := fake.Messages()
		for _, b := range msgs[len(msgs)-1].Blocks {
			elements, _ := b["elements"].([]any)
			for _, e := range elements {
				if text, _ := e.(map[string]any)["text"].(string); strings.Contains(text, "Paged:") {
					return text
				}
			}
		}
		return ""
	}

	// A group that only told channels names nobody.
	if got := paged(escalated([]map[string]any{{"target": "channel #alerts", "integration_id": here, "destination": "#alerts"}}, nil)); got != "" {
		t.Errorf("a step without people: %q", got)
	}
	if got := paged(); got != "" {
		t.Errorf("no step at all: %q", got)
	}

	first := escalated(
		[]map[string]any{{"target": "schedule primary", "user": "ada@example.com", "name": "Ada Lovelace", "integration_id": here, "destination": "U0ADA"}},
		nil)
	second := escalated(
		[]map[string]any{
			// Ada again, by the team: named once.
			{"target": "team platform", "user": "ada@example.com", "name": "Ada Lovelace", "integration_id": here, "destination": "U0ADA"},
			// Paged through another workspace: no mention here.
			{"target": "team platform", "user": "ken@example.com", "name": "Ken T", "integration_id": elsewhere, "destination": "U0KEN"},
			{"target": "channel #alerts", "integration_id": here, "destination": "#alerts"},
		},
		[]map[string]any{
			{"target": "team platform", "user": "grace@example.com", "name": "Grace <!channel> Hopper", "integration_id": here,
				"reason": "the person has no linked account for this integration", "not_connected": "Slack", "shown_as": "Grace Hopper (Slack not connected)"},
			// Skipped for another reason: not a person to name.
			{"target": "schedule empty", "reason": "the schedule has nobody on call: a gap in coverage"},
			// Not connected in two services.
			{"target": "user linus@example.com", "user": "linus@example.com", "name": "Linus", "not_connected": "Slack"},
			{"target": "user linus@example.com", "user": "linus@example.com", "name": "Linus", "not_connected": "Discord"},
			// A destination that is not a Slack user ID is never made a mention.
		})
	want := ":pager: Paged: <@U0ADA>, Ken T, Grace &lt;!channel&gt; Hopper (Slack not connected), Linus (Slack, Discord not connected)"
	if got := paged(first, second); got != want {
		t.Errorf("paged line:\n got %s\nwant %s", got, want)
	}
	// After the first step alone.
	if got := paged(first); got != ":pager: Paged: <@U0ADA>" {
		t.Errorf("after the first step: %q", got)
	}
	// Something that is not a user ID in the destination stays text.
	odd := escalated([]map[string]any{{"user": "x@example.com", "name": "X", "integration_id": here, "destination": "!channel"}}, nil)
	if got := paged(odd); got != ":pager: Paged: X" {
		t.Errorf("a destination that is not a user ID: %q", got)
	}
	// Many people fit in one line.
	var many []map[string]any
	for i := range 30 {
		many = append(many, map[string]any{"user": strings.Repeat("u", i+1) + "@example.com", "name": strings.Repeat("N", 300), "not_connected": "Slack"})
	}
	if got := paged(escalated(nil, many)); !strings.HasSuffix(got, ", and 10 more") || len(got) > 3000 {
		t.Errorf("thirty people: %d characters, ending %q", len(got), got[max(len(got)-20, 0):])
	}
}
