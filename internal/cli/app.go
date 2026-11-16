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

// attachIngest stores the configured sources and serves the ingest endpoint
// for them.
func attachIngest(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, cfg config.Config, alerts *alert.Store, mux *http.ServeMux) error {
	sources := make([]ingest.Source, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		id, err := provision.Ensure(ctx, pool, provision.Sources, s.Name, s)
		if err != nil {
			return err
		}
		sources = append(sources, ingest.Source{ID: id, Source: s})
		logger.InfoContext(ctx, "source configured",
			slog.String("source", s.Name), slog.String("type", s.Type),
			slog.String("path", strings.Replace(ingest.Path, "{source}", s.Name, 1)))
	}
	if len(sources) == 0 {
		logger.WarnContext(ctx, "no source is configured, so every request to the ingest endpoint is refused; set --source-token or "+envSourceToken)
	}
	ingest.New(logger, alerts, sources).Register(mux)
	return nil
}

// attachNotifications stores the configured integrations, gives the pipeline
// a channel for each, and makes every alert notify the configured targets.
func attachNotifications(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, cfg config.Config, alerts *alert.Store, pipeline *notify.Pipeline, slackAPIURL string) error {
	ids := make(map[string]string, len(cfg.Integrations))
	for _, i := range cfg.Integrations {
		id, err := provision.Ensure(ctx, pool, provision.Integrations, i.Name, i)
		if err != nil {
			return err
		}
		ids[i.Name] = id
		pipeline.Register(id, slack.NewChannel(slack.NewClient(slackAPIURL, i.Slack.BotToken)))
		logger.InfoContext(ctx, "integration configured", slog.String("integration", i.Name), slog.String("type", i.Type))
	}

	targets := make([]notify.Target, 0, len(cfg.Notify))
	for _, t := range cfg.Notify {
		targets = append(targets, notify.Target{IntegrationID: ids[t.Integration], Destination: t.Channel})
		logger.InfoContext(ctx, "every alert notifies", slog.String("integration", t.Integration), slog.String("channel", t.Channel))
	}
	if len(targets) == 0 {
		logger.WarnContext(ctx, "Slack is not configured, so alerts are stored but nobody is notified; set --slack-bot-token and --slack-channel")
		return nil
	}
	alerts.Observe(pipeline.NotifyAll(targets))
	return nil
}
