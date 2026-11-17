package cli

import (
	"errors"
	"flag"

	"github.com/thanoslabs/beacon/internal/config"
)

// bootstrapSettings stand in for configuration files, which do not exist yet.
// They describe the few provisioned objects the first end-to-end path needs,
// and produce the same config.Config that the YAML loader will produce.
//
// Temporary: remove these settings when beacon serve takes --config.
type bootstrapSettings struct {
	sourceName         string
	sourceToken        string
	slackBotToken      string
	slackSigningSecret string
	slackChannel       string
}

// The environment variables of the bootstrap secrets, as recorded in the
// stored copy of each object in place of the value.
const (
	envSourceToken        = "BEACON_SOURCE_TOKEN"
	envSlackBotToken      = "BEACON_SLACK_BOT_TOKEN"
	envSlackSigningSecret = "BEACON_SLACK_SIGNING_SECRET"
)

// slackIntegrationName is the name of the one Slack integration.
const slackIntegrationName = "slack"

func (b *bootstrapSettings) register(fs *flag.FlagSet, e env) {
	stringSetting(fs, e, &b.sourceName, "source-name", "BEACON_SOURCE_NAME", "default",
		"name of the webhook source, the last segment of its URL /api/v1/ingest/<name>; temporary, until configuration files exist")
	secretSetting(fs, e, &b.sourceToken, "source-token", envSourceToken,
		"token that senders to the webhook source present as a bearer token; without it no source exists; temporary, until configuration files exist")
	secretSetting(fs, e, &b.slackBotToken, "slack-bot-token", envSlackBotToken,
		"bot token of the Slack app that posts alerts; without it nobody is notified; temporary, until configuration files exist")
	secretSetting(fs, e, &b.slackSigningSecret, "slack-signing-secret", envSlackSigningSecret,
		"signing secret of the Slack app, which proves that button presses come from Slack; temporary, until configuration files exist")
	stringSetting(fs, e, &b.slackChannel, "slack-channel", "BEACON_SLACK_CHANNEL", "",
		"Slack channel that is told about every alert, as a channel ID such as C0123456789; temporary, until configuration files exist")
}

// config builds the provisioned objects the settings describe.
func (b bootstrapSettings) config() (config.Config, error) {
	var cfg config.Config
	set := 0
	for _, v := range []string{b.slackBotToken, b.slackSigningSecret, b.slackChannel} {
		if v != "" {
			set++
		}
	}
	if set != 0 && set != 3 {
		return cfg, errors.New("the Slack settings go together: set all of --slack-bot-token (or " + envSlackBotToken +
			"), --slack-signing-secret (or " + envSlackSigningSecret + ") and --slack-channel (or BEACON_SLACK_CHANNEL), or none")
	}
	if b.slackBotToken != "" {
		cfg.Integrations = append(cfg.Integrations, config.Integration{
			Name: slackIntegrationName,
			Type: config.IntegrationTypeSlack,
			Slack: &config.SlackIntegration{
				BotToken:      config.NewSecret(config.SecretRef{Env: envSlackBotToken}, b.slackBotToken),
				SigningSecret: config.NewSecret(config.SecretRef{Env: envSlackSigningSecret}, b.slackSigningSecret),
			},
		})
		cfg.Notify = append(cfg.Notify, config.ChannelTarget{Channel: b.slackChannel, Integration: slackIntegrationName})
	}
	if b.sourceToken != "" {
		cfg.Sources = append(cfg.Sources, config.Source{
			Name:  b.sourceName,
			Type:  config.SourceTypeWebhook,
			Token: config.NewSecret(config.SecretRef{Env: envSourceToken}, b.sourceToken),
		})
	}
	return cfg, cfg.Validate()
}
