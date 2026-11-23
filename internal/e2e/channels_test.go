package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/auth/oidctest"
	"github.com/thanoslabs/beacon/internal/discord/discordtest"
	"github.com/thanoslabs/beacon/internal/email/mailpittest"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/webhook"
)

// discordIntegration is a Discord integration, to go under "integrations:".
// The public key stands for the key of the fake Discord, which launch and
// reload put in its place.
const discordIntegration = `  - name: discord
    type: discord
    discord:
      bot_token: {env: BEACON_DISCORD_BOT_TOKEN}
      public_key: "` + discordKeyPlaceholder + `"
      application_id: "` + discordApplication + `"
`

const (
	discordKeyPlaceholder = "0000000000000000000000000000000000000000000000000000000000000000"
	discordApplication    = "400000000000000001"
)

// withDiscordKey puts the public key of the fake Discord into a
// configuration.
func (b *beacon) withDiscordKey(yaml string) string {
	return strings.ReplaceAll(yaml, discordKeyPlaceholder, b.discord.PublicKey())
}

// discordInteractions is where the fake Discord sends interactions.
func (b *beacon) discordInteractions() string {
	return b.url + "/api/v1/integrations/discord/interactions"
}

// firstSlackTarget matches a channel target of the Slack integration.
var firstSlackTarget = regexp.MustCompile(`          - channel: \S+\n            integration: slack\n`)

// withDiscord returns a configuration with a Discord integration added, and
// with the Slack channel of the first step replaced by the Discord channel.
func withDiscord(t *testing.T, yaml string) string {
	t.Helper()
	out := strings.Replace(yaml, "escalation_policies:\n", discordIntegration+"escalation_policies:\n", 1)
	replaced := false
	out = firstSlackTarget.ReplaceAllStringFunc(out, func(target string) string {
		if replaced {
			return target
		}
		replaced = true
		return "          - channel: \"" + discordtest.Channel + "\"\n            integration: discord\n"
	})
	if !replaced {
		t.Fatal("the configuration has no Slack channel to replace")
	}
	return out
}

// discordMessage waits until a message in the fake Discord satisfies cond
// and returns it.
func (b *beacon) discordMessage(what string, cond func(discordtest.Message) bool) discordtest.Message {
	b.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		msgs := b.discord.Messages()
		for _, m := range msgs {
			if cond(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s; Discord has: %+v", what, msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func discordContains(substrings ...string) func(discordtest.Message) bool {
	return func(m discordtest.Message) bool {
		for _, s := range substrings {
			if !strings.Contains(m.JSON(), s) {
				return false
			}
		}
		return true
	}
}

// reloadAddsDiscord reloads the running process with a Discord integration
// added to yaml, as the n-th reload, and checks that an alert that fires
// afterwards is posted to Discord.
func reloadAddsDiscord(b *beacon, yaml string, n int) {
	b.t.Helper()
	logs := b.reload(withDiscord(b.t, yaml), n)
	if !strings.Contains(logs, `"msg":"integration configured","integration":"discord","type":"discord"`) {
		b.t.Errorf("the logs do not show the Discord integration")
	}
	if code, res := b.webhookTo("second", `{"labels": {"alertname": "ToDiscord", "severity": "warning"}}`); code != http.StatusCreated {
		b.t.Fatalf("webhook = %d %v", code, res)
	}
	b.discordMessage("the alert in Discord after the reload", discordContains("**Firing** · warning", "ToDiscord"))
}

// TestDiscordChannel: an alert group is posted to a Discord channel, and
// the message is edited in place when the source resolves the alert.
func TestDiscordChannel(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	b := launch(t, dbURL, writeConfig(t, withDiscord(t, configuration)))
	b.waitUntilServing()

	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	msg := b.discordMessage("the alert in Discord", discordContains("**Firing** · critical", "Disk is full on db1"))
	if msg.ChannelID != discordtest.Channel {
		t.Errorf("the message went to %s", msg.ChannelID)
	}
	for _, want := range []string{"`alertname=DiskFull` `host=db1` `severity=critical`", "[📖 Runbook](https://runbooks.example.com/disk-full)"} {
		if !strings.Contains(msg.JSON(), want) {
			t.Errorf("the message does not contain %q:\n%s", want, msg.JSON())
		}
	}
	// The delivery is on record before the source reports again.
	b.timeline(alertID, 3)

	if code, res := b.webhook(diskFullResolved); code != http.StatusOK {
		t.Fatalf("resolve webhook = %d %v", code, res)
	}
	msg = b.discordMessage("the resolution in Discord", discordContains("**Resolved**", "Resolved by the source"))
	if msgs := b.discord.Messages(); len(msgs) != 1 || msg.Edits != 1 {
		t.Errorf("Discord has %d messages and the first has %d edits, want one message edited once", len(msgs), msg.Edits)
	}
	var ref string
	if err := b.db.QueryRow(context.Background(), `SELECT DISTINCT external_ref FROM notifications`).Scan(&ref); err != nil || ref != discordtest.Channel+":"+msg.ID {
		t.Errorf("external_ref = %q, %v", ref, err)
	}
	if len(b.slack.Messages()) != 0 {
		t.Errorf("Slack was sent a message for a policy that names Discord only")
	}

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	if strings.Contains(b.output(), discordToken) {
		t.Error("the logs contain the Discord bot token")
	}
}

// TestDiscordFirstPath is the first path with Discord in place of Slack,
// and with a person in it: Ada signs in, links her Discord account herself,
// is paged in a direct message, and acknowledges from there. Someone who is
// no Beacon user may resolve and nothing else.
func TestDiscordFirstPath(t *testing.T) {
	oidctest.Issuer(t)
	dbURL := postgrestest.NewURL(t)
	cfg := strings.Replace(signInConfiguration(t), "escalation_policies:\n", discordIntegration+"escalation_policies:\n", 1)
	cfg = strings.Replace(cfg, "          - channel: C0ALERTS\n            integration: slack\n",
		"          - channel: \""+discordtest.Channel+"\"\n            integration: discord\n          - user: "+oidctest.Ada+"\n            via: [discord]\n", 1)
	if !strings.Contains(cfg, "via: [discord]") {
		t.Fatal("the configuration has no step to change")
	}
	b := launch(t, dbURL, writeConfig(t, cfg))
	b.waitUntilServing()
	ctx := context.Background()

	// Beacon registers the link command by itself.
	b.waitFor("the link command to be registered", func() bool { return len(b.discord.Commands(discordApplication)) == 1 })
	if cmd := b.discord.Commands(discordApplication)[0]; cmd["name"] != "beacon-link" {
		t.Errorf("command = %v", cmd)
	}

	// 1. Ada signs in and asks Beacon for a link code.
	browser := oidctest.Browser(t, strings.TrimPrefix(b.url, "http://"))
	if code, body := oidctest.SignIn(t, browser, oidctest.Ada, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	code, body := do(t, browser, http.MethodPost, "/api/v1/me/identities/discord/link-code")
	var issued struct{ Code, Command string }
	if err := json.Unmarshal([]byte(body), &issued); err != nil || code != http.StatusCreated || issued.Code == "" || issued.Command != "/beacon-link code: "+issued.Code {
		t.Fatalf("link code = %d %s", code, body)
	}

	// 2. In Discord, she gives the code to Beacon. A code that is wrong
	// links nobody.
	ada := discordtest.User{ID: "300000000000000001", Username: "ada.in.discord", GlobalName: "Ada in Discord"}
	b.discord.AddUser(ada.ID)
	if got := b.discord.Send(t, b.discordInteractions(), b.discord.CommandBody(t, "beacon-link", map[string]string{"code": "AAAA-BBBB-CCCC-DDDD"}, ada)); !strings.Contains(got.Content, "the code is not valid") {
		t.Fatalf("a wrong code = %+v", got)
	}
	got := b.discord.Send(t, b.discordInteractions(), b.discord.CommandBody(t, "beacon-link", map[string]string{"code": issued.Code}, ada))
	if got.Type != 4 || !got.Ephemeral || !strings.HasPrefix(got.Content, "Connected: this Discord account now belongs to ") {
		t.Fatalf("the link command = %+v", got)
	}
	if code, body := do(t, browser, http.MethodGet, "/api/v1/me/identities"); code != http.StatusOK ||
		!strings.Contains(body, `"integration":"discord","provider":"discord","external_id":"`+ada.ID+`","origin":"self"`) {
		t.Fatalf("identities = %d %s", code, body)
	}
	var name string
	if err := b.db.QueryRow(ctx, `SELECT COALESCE(NULLIF(display_name, ''), email) FROM users WHERE email = $1`, oidctest.Ada).Scan(&name); err != nil {
		t.Fatal(err)
	}
	// The code worked once.
	if got := b.discord.Send(t, b.discordInteractions(), b.discord.CommandBody(t, "beacon-link", map[string]string{"code": issued.Code}, ada)); !strings.Contains(got.Content, "the code is not valid") {
		t.Errorf("the code a second time = %+v", got)
	}

	// 3. An alert fires. The channel is told, and Ada is paged in a direct
	// message.
	status, res := b.webhook(diskFull)
	if status != http.StatusCreated {
		t.Fatalf("webhook = %d %v", status, res)
	}
	alertID := res["alert_id"]
	inChannel := b.discordMessage("the alert in the channel", func(m discordtest.Message) bool {
		return m.ChannelID == discordtest.Channel && strings.Contains(m.JSON(), "**Firing** · critical")
	})
	page := b.discordMessage("the page in Ada's direct messages", func(m discordtest.Message) bool {
		return m.ChannelID != discordtest.Channel && strings.Contains(m.JSON(), "Disk is full on db1")
	})
	if dm := b.discord.DM(ada.ID); dm == "" || page.ChannelID != dm {
		t.Errorf("the page went to %s, want Ada's direct message channel %q", page.ChannelID, dm)
	}
	if !strings.Contains(inChannel.JSON(), `"name":"Paged","value":"\u003c@`+ada.ID+`\u003e"`) {
		t.Errorf("the message in the channel does not say who was paged: %s", inChannel.JSON())
	}
	// Both deliveries are on record before anyone acts.
	b.timeline(alertID, 4)

	// 4. Bob is in the channel and is no Beacon user. He may not acknowledge.
	bob := discordtest.User{ID: "300000000000000002", Username: "bob"}
	if got := b.discord.Send(t, b.discordInteractions(), b.discord.PressBody(t, inChannel, "beacon_acknowledge", bob, false)); got.Type != 4 || !got.Ephemeral ||
		!strings.Contains(got.Content, "only Beacon users can acknowledge an alert") || !strings.Contains(got.Content, "Sign in to Beacon at "+oidctest.PublicURL) {
		t.Fatalf("acknowledge by an account that is not linked = %+v", got)
	}
	if got := b.status(alertID); got != "firing" {
		t.Fatalf("the alert is %s after the refused press", got)
	}

	// 5. Ada acknowledges from her direct message. Both messages show it.
	if got := b.discord.Send(t, b.discordInteractions(), b.discord.PressBody(t, page, "beacon_acknowledge", ada, true)); got.Status != http.StatusOK || got.Type != 6 {
		t.Fatalf("acknowledge from Discord = %+v", got)
	}
	if got := b.status(alertID); got != "acknowledged" {
		t.Fatalf("the alert is %s after the acknowledgement", got)
	}
	for _, where := range []string{discordtest.Channel, page.ChannelID} {
		m := b.discordMessage("the acknowledgement in "+where, func(m discordtest.Message) bool {
			return m.ChannelID == where && strings.Contains(m.JSON(), "Acknowledged by "+name)
		})
		if strings.Contains(m.JSON(), "beacon_acknowledge") || !strings.Contains(m.JSON(), "beacon_resolve") {
			t.Errorf("the acknowledged message should offer resolve but not acknowledge: %s", m.JSON())
		}
	}
	b.timeline(alertID, 7)

	// 6. Bob resolves, which anyone who sees the message may.
	if got := b.discord.Send(t, b.discordInteractions(), b.discord.PressBody(t, inChannel, "beacon_resolve", bob, false)); got.Type != 6 {
		t.Fatalf("resolve by an account that is not linked = %+v", got)
	}
	final := b.discordMessage("the resolution in the channel", func(m discordtest.Message) bool {
		return m.ChannelID == discordtest.Channel && strings.Contains(m.JSON(), `Resolved by bob \\(Discord not connected\\)`)
	})
	if len(final.Components) != 0 || len(b.discord.Messages()) != 2 {
		t.Errorf("want two messages, edited in place, and no buttons on a resolved one: %+v", b.discord.Messages())
	}

	want := []string{
		"fired by source",
		"escalated by system",
		"notification_sent by system",
		"notification_sent by system",
		"acknowledged by user discord:" + ada.ID,
		"notification_sent by system",
		"notification_sent by system",
		"resolved by user discord:" + bob.ID,
		"notification_sent by system",
		"notification_sent by system",
	}
	if got := b.timeline(alertID, len(want)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("timeline:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// The acknowledgement is Ada's, the Beacon user.
	var actor string
	if err := b.db.QueryRow(ctx, `SELECT u.email FROM alert_events e JOIN users u ON u.id = e.actor_id WHERE e.type = 'acknowledged'`).Scan(&actor); err != nil || actor != oidctest.Ada {
		t.Errorf("the acknowledgement is attributed to %q, %v", actor, err)
	}

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	for _, secret := range []string{discordToken, issued.Code, strings.ReplaceAll(issued.Code, "-", "")} {
		if strings.Contains(b.output(), secret) {
			t.Errorf("the logs contain %q", secret)
		}
	}
}

// withEmail returns a configuration with an email integration that sends
// through the Mailpit of compose.yaml, and with the Slack channel of the
// first step replaced by targets, which are lines of a notify list.
func withEmail(t *testing.T, server *mailpittest.Server, yaml, targets string) string {
	t.Helper()
	integration := fmt.Sprintf(`  - name: email
    type: email
    email: {host: %q, port: %d, from: beacon@example.invalid, tls: none, username: %s, password: {env: BEACON_SMTP_PASSWORD}}
`, server.Host, server.Port, mailpittest.Username)
	out := strings.Replace(yaml, "escalation_policies:\n", integration+"escalation_policies:\n", 1)
	replaced := false
	out = firstSlackTarget.ReplaceAllStringFunc(out, func(target string) string {
		if replaced {
			return target
		}
		replaced = true
		return targets
	})
	if !replaced {
		t.Fatal("the configuration has no Slack channel to replace")
	}
	return out
}

// mail waits until Mailpit holds a message for the recipient whose subject
// contains every one of the substrings, and returns it.
func mail(t *testing.T, server *mailpittest.Server, to string, subject ...string) mailpittest.Message {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		msgs := server.Messages(t, to)
	next:
		for _, m := range msgs {
			for _, s := range subject {
				if !strings.Contains(m.Subject, s) {
					continue next
				}
			}
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("no message for %s with %q in its subject; Mailpit has %d for that address", to, subject, len(msgs))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reloadAddsEmail reloads the running process with an email integration
// added to yaml, as the n-th reload, and checks that an alert that fires
// afterwards is sent to the real SMTP server.
func reloadAddsEmail(b *beacon, yaml string, n int) {
	b.t.Helper()
	server := mailpittest.New(b.t)
	to := mailpittest.Address(b.t, "reload")
	logs := b.reload(withEmail(b.t, server, yaml, "          - channel: "+to+"\n            integration: email\n"), n)
	if !strings.Contains(logs, `"msg":"integration configured","integration":"email","type":"email"`) {
		b.t.Errorf("the logs do not show the email integration")
	}
	if code, res := b.webhookTo("second", `{"labels": {"alertname": "ToEmail", "severity": "warning"}}`); code != http.StatusCreated {
		b.t.Fatalf("webhook = %d %v", code, res)
	}
	mail(b.t, server, to, "[FIRING] warning: ToEmail")
}

// TestEmailChannel: an alert group is sent as email through a real SMTP
// server, to a shared address and to a person at the address they have in
// Beacon, and every change is a further message in the same conversation.
func TestEmailChannel(t *testing.T) {
	oidctest.Issuer(t)
	server := mailpittest.New(t)
	dbURL := postgrestest.NewURL(t)
	shared := mailpittest.Address(t, "team")
	// Ada's address is the one she has at the identity provider, which
	// other tests use too: this test's messages are told apart by the name
	// of its alert.
	name := "Mail" + strings.ToUpper(strings.TrimSuffix(strings.TrimPrefix(shared, "team-"), "@example.invalid"))
	cfg := withEmail(t, server, signInConfiguration(t),
		"          - channel: "+shared+"\n            integration: email\n          - user: "+oidctest.Ada+"\n            via: [email]\n")
	b := launch(t, dbURL, writeConfig(t, cfg))
	b.waitUntilServing()
	// Ada becomes a user by signing in. Nothing has to be linked: her
	// address is where email reaches her.
	if code, body := oidctest.SignIn(t, oidctest.Browser(t, strings.TrimPrefix(b.url, "http://")), oidctest.Ada, oidctest.Password); code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}

	code, res := b.webhook(`{"labels": {"alertname": "` + name + `", "host": "db1", "severity": "critical"},
		"annotations": {"summary": "Disk is full on db1 (` + name + `)", "runbook_url": "https://runbooks.example.com/disk-full"}}`)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	subject := "critical: Disk is full on db1 (" + name + ")"
	first := mail(t, server, shared, "[FIRING] "+subject)
	page := mail(t, server, oidctest.Ada, "[FIRING] "+subject)
	if first.From != "beacon@example.invalid" || len(page.To) != 1 || page.To[0] != oidctest.Ada {
		t.Errorf("from %q, the page went to %v", first.From, page.To)
	}
	for _, want := range []string{
		"FIRING · critical", "  alertname = " + name, "  host = db1", "  severity = critical", "Status: Firing since ",
		"Paged: ", "Open in Beacon: " + oidctest.PublicURL + "/alerts/" + alertID, "Runbook: https://runbooks.example.com/disk-full",
	} {
		if !strings.Contains(first.Text, want) {
			t.Errorf("the message does not contain %q:\n%s", want, first.Text)
		}
	}
	b.timeline(alertID, 4)

	// The source resolves the alert: a second message, which answers the
	// first.
	if code, res := b.webhook(`{"status": "resolved", "labels": {"alertname": "` + name + `", "host": "db1", "severity": "critical"}}`); code != http.StatusOK {
		t.Fatalf("resolve webhook = %d %v", code, res)
	}
	resolved := mail(t, server, shared, "[RESOLVED] "+subject)
	if !strings.Contains(resolved.Text, "Status: Resolved by the source") || !strings.Contains(resolved.Text, "Sent by Beacon because this alert is resolved.") {
		t.Errorf("the resolution:\n%s", resolved.Text)
	}
	if id := first.Headers["Message-Id"]; len(id) != 1 || len(resolved.Headers["In-Reply-To"]) != 1 || resolved.Headers["In-Reply-To"][0] != id[0] {
		t.Errorf("the resolution does not answer the first message: %v after %v", resolved.Headers["In-Reply-To"], id)
	}
	mail(t, server, oidctest.Ada, "[RESOLVED] "+subject)
	var sent int
	b.waitFor("four recorded deliveries", func() bool {
		err := b.db.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE status = 'sent'`).Scan(&sent)
		return err == nil && sent == 4
	})

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	if strings.Contains(b.output(), mailpittest.Password) {
		t.Error("the logs contain the SMTP password")
	}
}

// hookReceiver is the other end of an outbound webhook: it keeps the
// payloads whose signature verifies, and counts the requests whose
// signature does not.
type hookReceiver struct {
	*httptest.Server
	mu       sync.Mutex
	payloads []webhook.Payload
	keys     []string
	unsigned int
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	r := &hookReceiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		// As a receiver would: the signature over the timestamp and the raw
		// body, and a timestamp that is recent.
		if !webhook.Verify(hookSecret, req.Header.Get(webhook.HeaderTimestamp), req.Header.Get(webhook.HeaderSignature), body, time.Now(), 5*time.Minute) {
			r.unsigned++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var p webhook.Payload
		if err := json.Unmarshal(body, &p); err != nil {
			r.unsigned++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.payloads = append(r.payloads, p)
		r.keys = append(r.keys, req.Header.Get(webhook.HeaderIdempotencyKey))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.Close)
	return r
}

// payload waits until the receiver holds a verified payload that satisfies
// cond, and returns it.
func (r *hookReceiver) payload(t *testing.T, what string, cond func(webhook.Payload) bool) webhook.Payload {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r.mu.Lock()
		for _, p := range r.payloads {
			if cond(p) {
				r.mu.Unlock()
				return p
			}
		}
		n, unsigned := len(r.payloads), r.unsigned
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; the receiver has %d payloads and refused %d requests", what, n, unsigned)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// withWebhook returns a configuration with an outbound webhook that posts
// to url, and with the Slack channel of the first step replaced by it.
func withWebhook(t *testing.T, yaml, url string) string {
	t.Helper()
	integration := fmt.Sprintf(`  - name: tickets
    type: webhook
    webhook:
      url: %q
      signing_secret: {env: BEACON_HOOK_SIGNING_SECRET}
      headers: {X-Team: platform}
`, url)
	out := strings.Replace(yaml, "escalation_policies:\n", integration+"escalation_policies:\n", 1)
	replaced := false
	out = firstSlackTarget.ReplaceAllStringFunc(out, func(target string) string {
		if replaced {
			return target
		}
		replaced = true
		return "          - integration: tickets\n"
	})
	if !replaced {
		t.Fatal("the configuration has no Slack channel to replace")
	}
	return out
}

// reloadAddsWebhook reloads the running process with an outbound webhook
// added to yaml, as the n-th reload, and checks that an alert that fires
// afterwards is posted to it.
func reloadAddsWebhook(b *beacon, yaml string, n int) {
	b.t.Helper()
	r := newHookReceiver(b.t)
	logs := b.reload(withWebhook(b.t, yaml, r.URL+"/hook"), n)
	if !strings.Contains(logs, `"msg":"integration configured","integration":"tickets","type":"webhook"`) {
		b.t.Errorf("the logs do not show the webhook integration")
	}
	if code, res := b.webhookTo("second", `{"labels": {"alertname": "ToWebhook", "severity": "warning"}}`); code != http.StatusCreated {
		b.t.Fatalf("webhook = %d %v", code, res)
	}
	r.payload(b.t, "the alert at the receiver after the reload", func(p webhook.Payload) bool {
		return p.Group.Title == "ToWebhook" && p.Group.Status == "firing"
	})
}

// TestOutboundWebhook: an alert group is posted to an address as signed
// JSON when it fires and when it resolves, and the receiver can verify
// every request.
func TestOutboundWebhook(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	r := newHookReceiver(t)
	b := launch(t, dbURL, writeConfig(t, withWebhook(t, configuration, r.URL+"/hooks/beacon?token=e2e-query-token-marker-52aa")))
	b.waitUntilServing()

	code, res := b.webhook(diskFull)
	if code != http.StatusCreated {
		t.Fatalf("webhook = %d %v", code, res)
	}
	alertID := res["alert_id"]
	fired := r.payload(t, "the firing group", func(p webhook.Payload) bool { return p.Group.Status == "firing" })
	if fired.Version != "1" || fired.Reason != "escalation_step" || fired.Group.Severity != "critical" || fired.Group.Title != "Disk is full on db1" ||
		len(fired.Alerts) != 1 || fired.Alerts[0].ID != alertID || fired.Alerts[0].Labels["host"] != "db1" ||
		fired.Alerts[0].Annotations["runbook_url"] != "https://runbooks.example.com/disk-full" || fired.Group.Counts["firing"] != 1 {
		t.Errorf("payload = %+v", fired)
	}
	b.timeline(alertID, 3)

	if code, res := b.webhook(diskFullResolved); code != http.StatusOK {
		t.Fatalf("resolve webhook = %d %v", code, res)
	}
	resolved := r.payload(t, "the resolved group", func(p webhook.Payload) bool { return p.Group.Status == "resolved" })
	if resolved.Reason != "resolved" || resolved.Alerts[0].ResolvedAt == nil || resolved.LastChange == nil || resolved.LastChange.ActorName != "the source" ||
		resolved.Group.ID != fired.Group.ID || resolved.ID == fired.ID {
		t.Errorf("payload = %+v", resolved)
	}
	r.mu.Lock()
	if r.unsigned != 0 || len(r.payloads) != 2 || r.keys[0] != fired.ID || r.keys[1] != resolved.ID {
		t.Errorf("%d requests did not verify; %d payloads with the keys %v", r.unsigned, len(r.payloads), r.keys)
	}
	r.mu.Unlock()
	var destinations string
	if err := b.db.QueryRow(context.Background(), `SELECT string_agg(DISTINCT destination, ',') FROM notifications WHERE status = 'sent'`).Scan(&destinations); err != nil || destinations != "tickets" {
		t.Errorf("destinations = %q, %v; want the name of the integration, never its address", destinations, err)
	}

	if code := b.terminate(); code != 0 {
		t.Errorf("exit code after SIGTERM = %d, want 0", code)
	}
	for _, secret := range []string{hookSecret, "e2e-query-token-marker-52aa"} {
		if strings.Contains(b.output(), secret) {
			t.Errorf("the logs contain %q", secret)
		}
	}
}
