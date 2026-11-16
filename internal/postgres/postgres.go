// Package postgres opens the connection pool and applies schema migrations.
// Postgres is the only place Beacon keeps state.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds the database settings.
type Config struct {
	// URL is a Postgres connection string or URL. Pool settings that pgx
	// understands, such as pool_max_conn_lifetime, can be given as URL
	// parameters.
	URL string
	// MaxConns caps the pool size. Zero keeps the value from the URL, or the
	// pgx default when the URL has none.
	MaxConns int
	// ConnectTimeout bounds the first connection attempt made by Open.
	ConnectTimeout time.Duration
}

// DefaultConnectTimeout is used when Config.ConnectTimeout is zero.
const DefaultConnectTimeout = 10 * time.Second

// Open creates a connection pool and checks that the database is reachable.
// The caller must close the pool.
func Open(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if cfg.URL == "" {
		return nil, errors.New("database URL is not set")
	}
	if cfg.MaxConns < 0 {
		return nil, fmt.Errorf("database max connections must not be negative, got %d", cfg.MaxConns)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx already redacts the password in its parse errors.
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = int32(cfg.MaxConns) //nolint:gosec // bounded by flag validation; a pool this large is never configured
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	timeout := cfg.ConnectTimeout
	if timeout == 0 {
		timeout = DefaultConnectTimeout
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}
