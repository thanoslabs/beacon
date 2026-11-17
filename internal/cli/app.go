package cli

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/ingest"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/slack"
)

// reconcile makes the provisioned rows in the database match cfg, and logs
// what changed.
func reconcile(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, cfg config.Config) (provision.Result, error) {
	res, err := provision.Reconcile(ctx, pool, cfg)
	if err != nil {
		return res, err
	}
	for _, c := range res.Changes {
		logger.InfoContext(ctx, "provisioned object "+string(c.Action), slog.String("kind", string(c.Kind)), slog.String("name", c.Name))
	}
	logger.InfoContext(ctx, "configuration reconciled", slog.Int("changes", len(res.Changes)))
	return res, nil
}

// attachIngest serves the ingest endpoint for the configured sources. ids
// maps each source to its row.
func attachIngest(ctx context.Context, logger *slog.Logger, cfg config.Config, ids map[string]string, alerts *alert.Store, mux *http.ServeMux) {
	sources := make([]ingest.Source, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		id := ids[s.Name]
		if s.Type != config.SourceTypeWebhook {
			logger.WarnContext(ctx, "this type of source does not receive alerts yet, so its endpoint refuses every request",
				slog.String("source", s.Name), slog.String("type", s.Type))
			continue
		}
		sources = append(sources, ingest.Source{ID: id, Source: s})
		logger.InfoContext(ctx, "source configured",
			slog.String("source", s.Name), slog.String("type", s.Type),
			slog.String("path", strings.Replace(ingest.Path, "{source}", s.Name, 1)))
	}
	if len(sources) == 0 {
		logger.WarnContext(ctx, "no source is configured, so every request to the ingest endpoint is refused; add a source of type webhook to the configuration")
	}
	ingest.New(logger, alerts, sources).Register(mux)
}

// attachNotifications gives the pipeline a channel for each configured
// integration, serves the endpoint that Slack sends button presses to, and
// makes every alert notify the configured targets. rows maps each
// integration to its row.
func attachNotifications(ctx context.Context, logger *slog.Logger, cfg config.Config, rows map[string]string, alerts *alert.Store, pipeline *notify.Pipeline, mux *http.ServeMux, slackAPIURL string) {
	ids := make(map[string]string, len(cfg.Integrations))
	signingSecrets := make(map[string]config.Secret, len(cfg.Integrations))
	for _, i := range cfg.Integrations {
		id := rows[i.Name]
		if i.Type != config.IntegrationTypeSlack {
			logger.WarnContext(ctx, "this type of integration does not deliver yet, so nothing is sent through it",
				slog.String("integration", i.Name), slog.String("type", i.Type))
			continue
		}
		ids[i.Name] = id
		pipeline.Register(id, slack.NewChannel(slack.NewClient(slackAPIURL, i.Slack.BotToken)))
		signingSecrets[i.Name] = i.Slack.SigningSecret
		logger.InfoContext(ctx, "integration configured", slog.String("integration", i.Name), slog.String("type", i.Type),
			slog.String("interactions_path", strings.Replace(slack.InteractionsPath, "{name}", i.Name, 1)))
	}
	slack.NewInteractions(logger, alerts, signingSecrets).Register(mux)

	var targets []notify.Target
	for _, t := range cfg.EveryAlertTargets() {
		id, ok := ids[t.Integration]
		if !ok {
			continue // warned about above
		}
		targets = append(targets, notify.Target{IntegrationID: id, Destination: t.Channel})
		logger.InfoContext(ctx, "every alert notifies", slog.String("integration", t.Integration), slog.String("channel", t.Channel))
	}
	if len(targets) == 0 {
		logger.WarnContext(ctx, "alerts are stored but nobody is notified: the first step of the default escalation policy names no channel of a Slack integration")
		return
	}
	alerts.Observe(pipeline.NotifyAll(targets))
}
