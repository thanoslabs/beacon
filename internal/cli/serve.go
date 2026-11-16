package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/logging"
	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/server"
	"github.com/thanoslabs/beacon/internal/version"
)

// snoozeExpiryInterval is how often the server looks for snoozes that have run
// out, and so the longest a snoozed alert stays quiet past its time.
const snoozeExpiryInterval = 5 * time.Second

func runServe(ctx context.Context, e env, args []string) error {
	var (
		dbCfg     postgres.Config
		srvCfg    server.Config
		logLevel  string
		logFormat string
		migrate   bool
		envErrs   []error
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
	databaseSettings(fs, e, &dbCfg, &envErrs)
	if err := parseFlags(fs, args, envErrs...); err != nil {
		return err
	}

	if err := checkDatabaseSettings(dbCfg); err != nil {
		return err
	}
	if srvCfg.ShutdownTimeout <= 0 {
		return fmt.Errorf("--shutdown-timeout must be positive, got %s", srvCfg.ShutdownTimeout)
	}
	// Logs go to stderr, so stdout stays free for command output.
	logger, err := logging.New(e.stderr, logLevel, logFormat)
	if err != nil {
		return err
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

	srv := server.New(srvCfg, logger, pool, server.NewRegistry())
	if err := srv.Listen(); err != nil {
		return err
	}

	// Background work stops with the server: it watches the same context.
	alerts := alert.NewStore(pool)
	var background sync.WaitGroup
	background.Go(func() { alerts.RunSnoozeExpiry(ctx, logger, snoozeExpiryInterval) })

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
