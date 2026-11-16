package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// embeddedMigrations holds the schema migrations that ship in the binary.
//
//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// Migrations returns the migrations embedded in the binary.
func Migrations() fs.FS {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		// The directory is embedded at compile time, so this cannot fail.
		panic(err)
	}
	return sub
}

// Migration describes one schema migration and whether it has been applied.
type Migration struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

// Migrator applies versioned SQL migrations.
type Migrator struct {
	provider *goose.Provider
}

// NewMigrator returns a migrator for the migrations in fsys, which must hold
// goose SQL files named <version>_<name>.sql at its root. Pass Migrations()
// for the embedded set.
//
// Migrate takes a Postgres advisory lock, so several replicas that start at
// the same time apply each migration once.
func NewMigrator(pool *pgxpool.Pool, fsys fs.FS) (*Migrator, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("create migration lock: %w", err)
	}
	// goose works through database/sql. This wraps the pool and opens no
	// connections of its own; closing it does not close the pool.
	db := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithSessionLocker(locker),
		goose.WithTableName("schema_migrations"),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	return &Migrator{provider: provider}, nil
}

// Close releases the migrator. It does not close the pool.
func (m *Migrator) Close() error {
	return m.provider.Close()
}

// Migrate applies every pending migration in version order and returns the
// ones it applied. Each migration runs in its own transaction.
func (m *Migrator) Migrate(ctx context.Context) ([]Migration, error) {
	results, err := m.provider.Up(ctx)

	// When a migration fails, goose reports the ones applied before it, and
	// the one that failed, inside the error.
	var partial *goose.PartialError
	if errors.As(err, &partial) {
		results = partial.Applied
		if partial.Failed != nil && partial.Failed.Source != nil {
			err = fmt.Errorf("%s: %w", migrationName(partial.Failed.Source.Path), partial.Err)
		}
	}

	applied := make([]Migration, 0, len(results))
	for _, r := range results {
		applied = append(applied, Migration{
			Version: r.Source.Version,
			Name:    migrationName(r.Source.Path),
			Applied: true,
		})
	}
	if err != nil {
		return applied, fmt.Errorf("apply migrations: %w", err)
	}
	return applied, nil
}

// Status lists every known migration, in version order, with whether it has
// been applied.
func (m *Migrator) Status(ctx context.Context) ([]Migration, error) {
	statuses, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("read migration status: %w", err)
	}
	out := make([]Migration, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, Migration{
			Version:   s.Source.Version,
			Name:      migrationName(s.Source.Path),
			Applied:   s.State == goose.StateApplied,
			AppliedAt: s.AppliedAt,
		})
	}
	return out, nil
}

// migrationName returns the file name of a migration without its directory.
func migrationName(p string) string {
	return path.Base(p)
}
