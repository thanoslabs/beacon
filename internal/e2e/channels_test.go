package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/auth/oidctest"
	"github.com/thanoslabs/beacon/internal/discord/discordtest"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
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
