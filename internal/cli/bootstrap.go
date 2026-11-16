package cli

import (
	"flag"

	"github.com/thanoslabs/beacon/internal/config"
)

// bootstrapSettings stand in for configuration files, which do not exist yet.
// They describe the few provisioned objects the first end-to-end path needs,
// and produce the same config.Config that the YAML loader will produce.
//
// Temporary: remove these settings when beacon serve takes --config.
type bootstrapSettings struct {
	sourceName  string
	sourceToken string
}

// The environment variables of the bootstrap secrets, as recorded in the
// stored copy of each object in place of the value.
const envSourceToken = "BEACON_SOURCE_TOKEN"

func (b *bootstrapSettings) register(fs *flag.FlagSet, e env) {
	stringSetting(fs, e, &b.sourceName, "source-name", "BEACON_SOURCE_NAME", "default",
		"name of the webhook source, the last segment of its URL /api/v1/ingest/<name>; temporary, until configuration files exist")
	secretSetting(fs, e, &b.sourceToken, "source-token", envSourceToken,
		"token that senders to the webhook source present as a bearer token; without it no source exists; temporary, until configuration files exist")
}

// config builds the provisioned objects the settings describe.
func (b bootstrapSettings) config() config.Config {
	var cfg config.Config
	if b.sourceToken != "" {
		cfg.Sources = append(cfg.Sources, config.Source{
			Name:  b.sourceName,
			Type:  config.SourceTypeWebhook,
			Token: config.NewSecret(config.SecretRef{Env: envSourceToken}, b.sourceToken),
		})
	}
	return cfg
}
