package discord_test

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
	"github.com/thanoslabs/beacon/internal/discord"
	"github.com/thanoslabs/beacon/internal/discord/discordtest"
	"github.com/thanoslabs/beacon/internal/identity"
)

// Alice and Bob have Discord accounts that are linked to Beacon users.
// Carol is in the server and is nobody in Beacon.
var (
	alice = discordtest.User{ID: "300000000000000011", Username: "alice", GlobalName: "Alice A."}
	bob   = discordtest.User{ID: "300000000000000012", Username: "bob"}
	carol = discordtest.User{ID: "300000000000000013", Username: "carol.c", GlobalName: "Carol C.", Nick: "Carol *the* Guest"}
)

// directory is the identities the handler is given: which Discord accounts
// belong to which users, and which codes link an account to whom.
type directory struct {
	users map[string][2]string
	// codes maps a link code to the user who asked for it, by identifier
	// and name, and taken lists the accounts that belong to someone else.
	codes map[string][2]string
	taken map[string]bool
	err   error
}

func (d *directory) UserFor(_ context.Context, integration, externalID string) (string, string, bool, error) {
	if d.err != nil {
		return "", "", false, d.err
	}
	u, ok := d.users[externalID]
	return u[0], u[1], ok && integration == "discord", nil
}

func (d *directory) CompleteLink(_ context.Context, integration, externalID, code string) (string, error) {
	u, ok := d.codes[code]
	switch {
	case d.err != nil:
		return "", d.err
	case !ok || integration != "discord":
		return "", identity.ErrBadCode
	}
	// A code works once, whatever comes of it.
	delete(d.codes, code)
	if d.taken[externalID] {
		return "", identity.ErrTaken
	}
	d.users[externalID] = u
	return u[1], nil
}

// actionsFixture is the pipeline fixture plus the interactions endpoint.
type actionsFixture struct {
	*pipelineFixture
	url  string
	logs *bytes.Buffer
	dir  *directory
	// aliceID and bobID are the Beacon users of Alice and Bob.
	aliceID, bobID string
}

func newActionsFixture(t *testing.T) *actionsFixture {
	t.Helper()
	f := &actionsFixture{pipelineFixture: newPipelineFixture(t), logs: &bytes.Buffer{}}
	h := discord.NewInteractions(slog.New(slog.NewJSONHandler(f.logs, nil)), f.alerts)
	h.SetPublicKeys(map[string]string{"discord": f.fake.PublicKey(), "broken": "not a key"})
	h.SetSignInURL("https://beacon.example.com")
	err := f.pool.QueryRow(context.Background(), `WITH a AS (INSERT INTO users (email) VALUES ('alice@example.com') RETURNING id),
		b AS (INSERT INTO users (email) VALUES ('bob@example.com') RETURNING id) SELECT a.id::text, b.id::text FROM a, b`).Scan(&f.aliceID, &f.bobID)
	if err != nil {
		t.Fatalf("create users: %v", err)
	}
	f.dir = &directory{users: map[string][2]string{alice.ID: {f.aliceID, "alice"}, bob.ID: {f.bobID, "bob"}}, codes: map[string][2]string{}, taken: map[string]bool{}}
	h.SetIdentities(f.dir)
	mux := http.NewServeMux()
	mux.Handle("POST "+discord.InteractionsPath, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/api/v1/integrations/discord/interactions"
	return f
}

// posted fires an alert and returns it with its Discord message.
func (f *actionsFixture) posted(t *testing.T) (alert.Alert, discordtest.Message) {
	t.Helper()
	a := f.fire(t)
	f.attempts(t)
	msgs := f.fake.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want 1", len(msgs))
	}
	return a, msgs[0]
}

// press presses a button of the message as a person in the server.
func (f *actionsFixture) press(t *testing.T, m discordtest.Message, button string, user discordtest.User) discordtest.Answer {
	t.Helper()
	return f.fake.Send(t, f.url, f.fake.PressBody(t, m, button, user, false))
}

func (f *actionsFixture) status(t *testing.T, id string) alert.Alert {
	t.Helper()
	a, err := f.alerts.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return a
}

// actorOf returns who caused the last event of a type on the timeline.
func (f *actionsFixture) actorOf(t *testing.T, alertID string, typ alert.EventType) alert.Actor {
	t.Helper()
	events, err := f.alerts.Timeline(context.Background(), alertID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			return events[i].Actor
		}
	}
	t.Fatalf("no %s event on the timeline: %+v", typ, events)
	return alert.Actor{}
}

// TestDiscordChecksTheEndpoint: Discord accepts an Interactions Endpoint
// URL only when it answers a PING with a PONG and refuses a request whose
// signature is wrong with 401.
func TestDiscordChecksTheEndpoint(t *testing.T) {
	f := newActionsFixture(t)
	ping := f.fake.PingBody()
	if got := f.fake.Send(t, f.url, ping); got.Status != http.StatusOK || got.Type != 1 {
		t.Fatalf("answer to a PING = %+v, want a PONG", got)
	}

	other := discordtest.New(t, botToken)
	timestamp, signature := f.fake.Sign(time.Now(), ping)
	_, otherSignature := other.Sign(time.Now(), ping)
	a, msg := f.posted(t)
	press := f.fake.PressBody(t, msg, "beacon_resolve", alice, false)
	pressTimestamp, pressSignature := f.fake.Sign(time.Now(), press)
	tests := []struct {
		name, url            string
		body                 []byte
		timestamp, signature string
	}{
		{"signed with another key", f.url, ping, timestamp, otherSignature},
		{"no signature", f.url, ping, timestamp, ""},
		{"no timestamp", f.url, ping, "", signature},
		{"another timestamp", f.url, ping, timestamp + "0", signature},
		{"a signature that is not hexadecimal", f.url, ping, timestamp, strings.Repeat("z", 128)},
		{"a short signature", f.url, ping, timestamp, signature[:64]},
		{"a body that was changed", f.url, bytes.Replace(press, []byte("beacon_resolve"), []byte("beacon_snooze"), 1), pressTimestamp, pressSignature},
		{"an integration that does not exist", strings.Replace(f.url, "/discord/", "/nope/", 1), ping, timestamp, signature},
		{"an integration whose key is not a key", strings.Replace(f.url, "/discord/", "/broken/", 1), ping, timestamp, signature},
	}
	for _, tt := range tests {
		if got := discordtest.Deliver(t, tt.url, tt.body, tt.timestamp, tt.signature); got.Status != http.StatusUnauthorized {
			t.Errorf("%s: answered %+v, want 401", tt.name, got)
		}
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusFiring {
		t.Errorf("the alert is %s after refused requests", got.Status)
	}
	if !strings.Contains(f.logs.String(), "discord request refused: the signature is missing or wrong") {
		t.Errorf("the refusals are not in the log")
	}
	// What is signed and is not an interaction Beacon uses.
	for _, body := range []string{`not json`, `{"type": 5, "data": {}}`} {
		ts, sig := f.fake.Sign(time.Now(), []byte(body))
		if got := discordtest.Deliver(t, f.url, []byte(body), ts, sig); got.Status != http.StatusBadRequest {
			t.Errorf("%s: answered %+v, want 400", body, got)
		}
	}
}

func TestAcknowledgeFromDiscord(t *testing.T) {
	f := newActionsFixture(t)
	a, msg := f.posted(t)
	if got := msg.JSON(); !strings.Contains(got, `"custom_id":"beacon_acknowledge:`+a.GroupID+`"`) || !strings.Contains(got, "beacon_snooze:") || !strings.Contains(got, "beacon_resolve:") {
		t.Fatalf("the message does not offer the three actions: %s", got)
	}

	// The answer says nothing: the message is edited through the pipeline.
	if got := f.press(t, msg, "beacon_acknowledge", alice); got.Status != http.StatusOK || got.Type != 6 {
		t.Fatalf("answer = %+v, want a deferred update", got)
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusAcknowledged {
		t.Fatalf("status = %s, want acknowledged", got.Status)
	}
	// The timeline names the Beacon user and the Discord account she acted
	// from.
	want := alert.Actor{Type: alert.ActorUser, ID: f.aliceID, Provider: "discord", ExternalID: alice.ID, Name: "alice"}
	if got := f.actorOf(t, a.ID, alert.EventAcknowledged); got != want {
		t.Errorf("actor = %+v, want %+v", got, want)
	}

	// The message is edited in place and offers what is still possible.
	f.attempts(t)
	msgs := f.fake.Messages()
	if len(msgs) != 1 || msgs[0].Edits != 1 {
		t.Fatalf("%d messages with %d edits, want the one message edited once", len(msgs), msgs[0].Edits)
	}
	if updated := msgs[0].JSON(); !strings.Contains(updated, "Acknowledged by alice") || strings.Contains(updated, "beacon_acknowledge") ||
		!strings.Contains(updated, "beacon_snooze") || !strings.Contains(updated, "beacon_resolve") {
		t.Errorf("updated message: %s", updated)
	}
}

func TestSnoozeAndResolveFromDiscord(t *testing.T) {
	f := newActionsFixture(t)
	a, msg := f.posted(t)

	before := time.Now()
	if got := f.press(t, msg, "beacon_snooze", bob); got.Type != 6 {
		t.Fatalf("snooze = %+v", got)
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

	// From a direct message, where Discord sends the person as a user and
	// not as a member.
	if got := f.fake.Send(t, f.url, f.fake.PressBody(t, msg, "beacon_resolve", alice, true)); got.Type != 6 {
		t.Fatalf("resolve = %+v", got)
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusResolved {
		t.Fatalf("status = %s, want resolved", got.Status)
	}
	f.attempts(t)
	msg = f.fake.Messages()[0]
	if !strings.Contains(msg.JSON(), "Resolved by alice") || len(msg.Components) != 0 {
		t.Errorf("message after resolve should name alice and have no buttons: %s", msg.JSON())
	}
}

// TestOnlyALinkedAccountMayAcknowledge: an account that belongs to no
// Beacon user may resolve and nothing else, and is told why.
func TestOnlyALinkedAccountMayAcknowledge(t *testing.T) {
	f := newActionsFixture(t)
	a, msg := f.posted(t)

	for _, button := range []string{"beacon_acknowledge", "beacon_snooze"} {
		got := f.press(t, msg, button, carol)
		action := strings.TrimPrefix(button, "beacon_")
		want := "Nothing changed: only Beacon users can " + action + " an alert, and your Discord account is not connected to one. " +
			"Sign in to Beacon at https://beacon.example.com, ask it for a link code for Discord, and give the code to Beacon here with /beacon-link. " +
			"You can still resolve the alert from here."
		if got.Type != 4 || !got.Ephemeral || got.Content != want {
			t.Errorf("%s by an account that is not linked = %+v\nwant the note %q", button, got, want)
		}
	}
	if got := f.status(t, a.ID); got.Status != alert.StatusFiring {
		t.Fatalf("the alert is %s after refused presses", got.Status)
	}

	// When it cannot be checked whether an account is linked, nobody is
	// guessed to be: acknowledging is refused, and resolving works.
	f.dir.err = errors.New("the database is away")
	if got := f.press(t, msg, "beacon_acknowledge", alice); got.Type != 4 || !got.Ephemeral || got.Content != "Nothing changed: Beacon could not check who you are just now. Please try again." {
		t.Errorf("acknowledge while identities cannot be read = %+v", got)
	}
	f.dir.err = nil

	// Carol resolves. She is named as the server names her, with the note
	// that she is not connected, and by her Discord account.
	if got := f.press(t, msg, "beacon_resolve", carol); got.Type != 6 {
		t.Fatalf("resolve by an account that is not linked = %+v", got)
	}
	want := alert.Actor{Type: alert.ActorUser, Provider: "discord", ExternalID: carol.ID, Name: "Carol *the* Guest (Discord not connected)"}
	if got := f.actorOf(t, a.ID, alert.EventResolved); got != want {
		t.Errorf("actor = %+v, want %+v", got, want)
	}
	f.attempts(t)
	if got := f.fake.Messages()[0].JSON(); !strings.Contains(got, `Resolved by Carol \\*the\\* Guest \\(Discord not connected\\)`) {
		t.Errorf("the message does not name who resolved: %s", got)
	}
}

// TestActingOnAnAlertThatAlreadyChanged presses buttons on a stale copy of
// the message, as two people looking at the same alert would.
func TestActingOnAnAlertThatAlreadyChanged(t *testing.T) {
	f := newActionsFixture(t)
	a, stale := f.posted(t)

	if got := f.press(t, stale, "beacon_acknowledge", alice); got.Type != 6 {
		t.Fatalf("first press = %+v", got)
	}
	if got := f.press(t, stale, "beacon_acknowledge", bob); got.Type != 4 || !got.Ephemeral || got.Content != "Nothing changed: this alert is already acknowledged." {
		t.Fatalf("second press = %+v", got)
	}
	if got := f.actorOf(t, a.ID, alert.EventAcknowledged); got.ExternalID != alice.ID {
		t.Errorf("the refused press is on the timeline: %+v", got)
	}
	if _, _, err := f.alerts.ResolveFromSource(context.Background(), f.source, "fp", time.Time{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, button := range []string{"beacon_acknowledge", "beacon_snooze", "beacon_resolve"} {
		if got := f.press(t, stale, button, bob); got.Content != "Nothing changed: this alert is already resolved." {
			t.Errorf("%s on a resolved alert = %+v", button, got)
		}
	}

	// A button of a group that is gone, and one that is not Beacon's.
	gone := stale
	gone.Components = []map[string]any{{"type": 1, "components": []any{
		map[string]any{"type": 2, "custom_id": "beacon_resolve:11111111-2222-4333-8444-555555555555"},
		map[string]any{"type": 2, "custom_id": "beacon_acknowledge:not-a-group"},
		map[string]any{"type": 2, "custom_id": "something_else:" + a.GroupID},
	}}}
	for button, want := range map[string]string{
		"beacon_resolve":     "Beacon does not know this alert any more.",
		"beacon_acknowledge": "Beacon does not know this alert any more.",
		"something_else":     "Beacon does not know this button.",
	} {
		if got := f.press(t, gone, button, alice); got.Type != 4 || !got.Ephemeral || got.Content != want {
			t.Errorf("%s = %+v, want %q", button, got, want)
		}
	}
}

// TestLinkCommand: a person gives Beacon the code it issued them, from
// inside Discord, and the account that sent it becomes theirs.
func TestLinkCommand(t *testing.T) {
	f := newActionsFixture(t)
	a, msg := f.posted(t)
	command := func(user discordtest.User, name string, options map[string]string) discordtest.Answer {
		t.Helper()
		got := f.fake.Send(t, f.url, f.fake.CommandBody(t, name, options, user))
		if got.Type != 4 || !got.Ephemeral {
			t.Errorf("/%s %v was answered with %+v, want a note for the person alone", name, options, got)
		}
		return got
	}

	var carolID string
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO users (email) VALUES ('carol@example.com') RETURNING id::text`).Scan(&carolID); err != nil {
		t.Fatal(err)
	}
	f.dir.codes["GOODCODE"] = [2]string{carolID, "Carol <Lovelace>"}
	if got := command(carol, "beacon-link", map[string]string{"code": "WRONGCODE"}); got.Content != "Nothing changed: the code is not valid; ask Beacon for a new one." {
		t.Errorf("a wrong code = %+v", got)
	}
	if got := command(carol, "beacon-link", nil); got.Content != "Nothing changed: the code is not valid; ask Beacon for a new one." {
		t.Errorf("no code = %+v", got)
	}
	if got := command(carol, "something-else", map[string]string{"code": "GOODCODE"}); got.Content != "Beacon does not know this command." {
		t.Errorf("another command = %+v", got)
	}
	if got := f.press(t, msg, "beacon_acknowledge", carol); got.Type != 4 {
		t.Fatalf("Carol could acknowledge before she linked her account: %+v", got)
	}

	got := command(carol, "beacon-link", map[string]string{"code": "GOODCODE"})
	if want := `Connected: this Discord account now belongs to Carol \<Lovelace\> in Beacon. You can acknowledge and snooze alerts from Discord, and Beacon can page you here.`; got.Content != want {
		t.Errorf("answer = %q, want %q", got.Content, want)
	}
	// From now on she may acknowledge.
	if got := f.press(t, msg, "beacon_acknowledge", carol); got.Type != 6 {
		t.Fatalf("acknowledge after linking = %+v", got)
	}
	if got := f.actorOf(t, a.ID, alert.EventAcknowledged); got.ID != carolID || got.ExternalID != carol.ID {
		t.Errorf("actor = %+v", got)
	}

	// An account that is someone else's is not taken over.
	f.dir.codes["SECOND"], f.dir.taken[bob.ID] = [2]string{carolID, "Dave"}, true
	if got := command(bob, "beacon-link", map[string]string{"code": "SECOND"}); !strings.HasPrefix(got.Content, "Nothing changed: this Discord account is connected to another Beacon user already.") {
		t.Errorf("an account that is taken = %+v", got)
	}
	f.dir.err = errors.New("the database is away")
	if got := command(carol, "beacon-link", map[string]string{"code": "THIRD"}); !strings.HasPrefix(got.Content, "Beacon could not connect your account just now.") {
		t.Errorf("a failure = %+v", got)
	}
	// No code reaches the log.
	for _, code := range []string{"GOODCODE", "WRONGCODE", "SECOND", "THIRD"} {
		if strings.Contains(f.logs.String(), code) {
			t.Errorf("the log contains the code %s:\n%s", code, f.logs)
		}
	}
}

// TestCommandsAreRegistered: Beacon registers the link command for each
// Discord application, once, and tries again later when Discord refuses.
func TestCommandsAreRegistered(t *testing.T) {
	fake := discordtest.New(t, botToken)
	logs := &bytes.Buffer{}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c := discord.NewCommands(slog.New(slog.NewJSONHandler(logs, nil)))
	c.SetClock(func() time.Time { return now })
	const app = "400000000000000001"
	apps := []discord.Application{{Integration: "discord", ID: app, Client: newClient(fake.URL)}}
	c.SetApplications(apps)
	ctx := context.Background()
	calls := func() int { return len(fake.Calls(discordtest.RouteRegisterCommand)) }

	fake.Fail(discordtest.RouteRegisterCommand, discordtest.Failure{Status: 401}, discordtest.Failure{Status: 503})
	if n := c.Register(ctx); n != 0 || calls() != 1 {
		t.Fatalf("registered %d after %d calls, want a failure", n, calls())
	}
	if !strings.Contains(logs.String(), `"msg":"register the link command with Discord; people cannot link their Discord accounts until it is registered","integration":"discord","command":"/beacon-link","next_attempt_in":"1m0s"`) ||
		strings.Contains(logs.String(), botToken) {
		t.Errorf("logs: %s", logs)
	}
	// Not again before the wait is over, and then twice as long.
	now = now.Add(59 * time.Second)
	if c.Register(ctx); calls() != 1 {
		t.Fatalf("tried again after 59 seconds")
	}
	now = now.Add(time.Second)
	if c.Register(ctx); calls() != 2 || !strings.Contains(logs.String(), `"next_attempt_in":"2m0s"`) {
		t.Fatalf("%d calls after a minute, logs: %s", calls(), logs)
	}
	// A reload is a reason to try at once.
	c.SetApplications(apps)
	if n := c.Register(ctx); n != 1 || calls() != 3 {
		t.Fatalf("registered %d after %d calls, want success at the third", n, calls())
	}
	commands := fake.Commands(app)
	if len(commands) != 1 || commands[0]["name"] != "beacon-link" {
		t.Fatalf("commands = %+v", commands)
	}
	options, _ := commands[0]["options"].([]any)
	if option, _ := options[0].(map[string]any); len(options) != 1 || option["name"] != "code" || option["required"] != true || option["type"] != float64(3) {
		t.Errorf("options = %+v", options)
	}
	// Once registered, it is left alone.
	now = now.Add(24 * time.Hour)
	c.SetApplications(apps)
	if n := c.Register(ctx); n != 0 || calls() != 3 {
		t.Errorf("registered again: %d calls", calls())
	}
}
