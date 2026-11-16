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
	"github.com/thanoslabs/beacon/internal/provision"
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
