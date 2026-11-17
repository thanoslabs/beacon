package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/ingest"
	"github.com/thanoslabs/beacon/internal/logging"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/server"
	"github.com/thanoslabs/beacon/internal/slack"
	"github.com/thanoslabs/beacon/internal/version"
)

// snoozeExpiryInterval is how often the server looks for snoozes that have run
// out, and so the longest a snoozed alert stays quiet past its time.
const snoozeExpiryInterval = 5 * time.Second

// sessionCleanupInterval is how often expired sessions are deleted. They are
// refused from the moment they expire; this only keeps the table small.
const sessionCleanupInterval = 5 * time.Minute

func runServe(ctx context.Context, e env, args []string) error {
	var (
		dbCfg     postgres.Config
		srvCfg    server.Config
		logLevel  string
		logFormat string
		migrate   bool
		cfgPath   string
		notifyCfg notify.Config
		// slackAPIURL is a process setting, not part of an integration: it
		// exists so that tests can point Beacon at a fake Slack.
		slackAPIURL string
		envErrs     []error
	)
	fs := newFlagSet(e, "serve", "Run the Beacon server")
	stringSetting(fs, e, &srvCfg.Addr, "listen-address", "BEACON_LISTEN_ADDRESS", server.DefaultAddr,
		"address to serve HTTP on")
	durationSetting(fs, e, &srvCfg.ShutdownTimeout, "shutdown-timeout", "BEACON_SHUTDOWN_TIMEOUT", server.DefaultShutdownTimeout,
		"how long in-flight requests get to finish after a termination signal", &envErrs)
	boolSetting(fs, e, &migrate, "migrate", "BEACON_MIGRATE", true,
		"apply pending database migrations before serving", &envErrs)
	stringSetting(fs, e, &logLevel, "log-level", "BEACON_LOG_LEVEL", "info",
		"log level: debug, info, warn or error")
	stringSetting(fs, e, &logFormat, "log-format", "BEACON_LOG_FORMAT", logging.FormatJSON,
		"log format: json or text")
	intSetting(fs, e, &notifyCfg.Workers, "notification-workers", "BEACON_NOTIFICATION_WORKERS", notify.DefaultWorkers,
		"how many notifications this process delivers at a time", &envErrs)
	stringSetting(fs, e, &slackAPIURL, "slack-api-url", "BEACON_SLACK_API_URL", slack.DefaultAPIURL,
		"base URL of the Slack Web API; change it only to test against a fake Slack")
	databaseSettings(fs, e, &dbCfg, &envErrs)
	configSetting(fs, e, &cfgPath)
	if err := parseFlags(fs, args, envErrs...); err != nil {
		return err
	}

	if err := checkDatabaseSettings(dbCfg); err != nil {
		return err
	}
	if _, err := url.ParseRequestURI(slackAPIURL); err != nil || !strings.HasPrefix(slackAPIURL, "http") {
		return fmt.Errorf("--slack-api-url must be an http or https URL, got %q", slackAPIURL)
	}
	if srvCfg.ShutdownTimeout <= 0 {
		return fmt.Errorf("--shutdown-timeout must be positive, got %s", srvCfg.ShutdownTimeout)
	}
	if notifyCfg.Workers < 1 || notifyCfg.Workers > 64 {
		return fmt.Errorf("--notification-workers must be between 1 and 64, got %d", notifyCfg.Workers)
	}
	// Logs go to stderr, so stdout stays free for command output.
	logger, err := logging.New(e.stderr, logLevel, logFormat)
	if err != nil {
		return err
	}

	// The configuration is read before anything is started, so that a mistake
	// in it stops the start instead of leaving half a system running.
	cfg, err := readConfig(e, cfgPath)
	if err != nil {
		return err
	}
	if cfgPath == "" {
		logger.WarnContext(ctx, "no configuration: there are no sources and nobody is notified; set --config or BEACON_CONFIG")
	}

	info := version.Get()
	logger.InfoContext(ctx, "starting beacon",
		slog.String("version", info.Version),
		slog.String("commit", info.Commit),
		slog.String("go_version", info.GoVersion),
	)

	pool, err := postgres.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.InfoContext(ctx, "connected to database", slog.Int("max_conns", int(pool.Config().MaxConns)))

	if migrate {
		if err := migrateOnStart(ctx, logger, pool); err != nil {
			return err
		}
	}

	registry := server.NewRegistry()
	srv := server.New(srvCfg, logger, pool, registry)
	if err := srv.Listen(); err != nil {
		return err
	}

	// The parts that follow the configuration start empty. The provisioner
	// fills them now, and again on every reload.
	alerts := alert.NewStore(pool)
	sessions := auth.NewStore(pool)
	pipeline := notify.New(pool, alerts, logger, notifyCfg)
	alerts.Observe(pipeline.NotifyAll(nil))
	prov := &provisioner{
		logger: logger, pool: pool, path: cfgPath, getenv: e.getenv, slackAPIURL: slackAPIURL,
		ingest:       ingest.New(logger, alerts, nil),
		interactions: slack.NewInteractions(logger, alerts, nil),
		pipeline:     pipeline,
		auth:         auth.NewHandler(logger, sessions),
	}
	prov.registerMetrics(registry)
	prov.ingest.Register(srv.Mux())
	prov.interactions.Register(srv.Mux())
	prov.auth.Register(srv.Mux())
	// The provisioned rows are made to match the files before anything that
	// refers to them is served.
	if err := prov.apply(ctx, cfg); err != nil {
		return err
	}

	// SIGHUP asks for the configuration to be read again.
	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	defer signal.Stop(hangup)

	// Background work stops with the server: it watches the same context.
	var background sync.WaitGroup
	background.Go(func() { pipeline.Run(ctx) })
	background.Go(func() { alerts.RunSnoozeExpiry(ctx, logger, snoozeExpiryInterval) })
	background.Go(func() { sessions.RunCleanup(ctx, logger, sessionCleanupInterval) })
	background.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hangup:
				// The outcome is logged and counted by reload.
				_ = prov.reload(ctx, "signal")
			}
		}
	})

	err = srv.Serve(ctx)
	background.Wait()
	if err != nil {
		return err
	}
	logger.Info("beacon stopped")
	return nil
}

// migrateOnStart applies pending migrations. It is safe when several replicas
// start together: the migrator serialises them with an advisory lock.
func migrateOnStart(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool) error {
	migrator, err := postgres.NewMigrator(pool, postgres.Migrations())
	if err != nil {
		return err
	}
	defer migrator.Close() //nolint:errcheck // the pool outlives the migrator and is closed by the caller

	start := time.Now()
	applied, err := migrator.Migrate(ctx)
	for _, m := range applied {
		logger.InfoContext(ctx, "applied migration", slog.String("migration", m.Name))
	}
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "database schema is up to date",
		slog.Int("applied", len(applied)),
		slog.Duration("duration", time.Since(start)),
	)
	return nil
}
