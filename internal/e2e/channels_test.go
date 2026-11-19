package e2e_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/discord/discordtest"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// discordIntegration is a Discord integration, to go under "integrations:".
// The public key is added by the tests that send interactions.
const discordIntegration = `  - name: discord
    type: discord
    discord:
      bot_token: {env: BEACON_DISCORD_BOT_TOKEN}
      public_key: "0000000000000000000000000000000000000000000000000000000000000000"
      application_id: "400000000000000001"
`

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
