// Package postgrestest gives tests a real, empty Postgres database.
//
// Tests that use it need a running Postgres and its URL in the
// BEACON_TEST_DATABASE_URL environment variable. The Compose file in the
// repository root provides one:
//
//	docker compose up -d --wait postgres
//	export BEACON_TEST_DATABASE_URL='postgres://beacon:beacon@localhost:5432/beacon?sslmode=disable'
//
// Without the variable the tests are skipped, except in CI, where a missing
// database is a failure.
package postgrestest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/postgres"
)

// EnvURL names the environment variable that holds the URL of the Postgres
// server to run tests against. The user must be allowed to create databases.
const EnvURL = "BEACON_TEST_DATABASE_URL"

// NewURL creates an empty database for one test and returns its URL. The
// database is dropped when the test ends. Each call gets its own database, so
// tests can run in parallel.
func NewURL(t testing.TB) string {
	t.Helper()

	base := os.Getenv(EnvURL)
	if base == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set; database tests must not be skipped in CI", EnvURL)
		}
		t.Skipf("%s is not set; skipping test that needs Postgres", EnvURL)
	}

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvURL, err)
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate database name: %v", err)
	}
	name := "beacon_test_" + hex.EncodeToString(suffix)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvURL, err)
	}
	defer admin.Close(ctx) //nolint:errcheck // nothing useful to do if closing the admin connection fails
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("connect to drop test database: %v", err)
			return
		}
		defer conn.Close(ctx) //nolint:errcheck // see above
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	u.Path = "/" + name
	return u.String()
}

// New creates an empty database for one test and returns a pool connected to
// it. The pool is closed and the database dropped when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()

	dbURL := NewURL(t)
	pool, err := postgres.Open(context.Background(), postgres.Config{URL: dbURL})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	// Registered after the drop in NewURL, so it runs first.
	t.Cleanup(pool.Close)
	return pool
}

// NewMigrated is like New, and also applies every embedded migration.
func NewMigrated(t testing.TB) *pgxpool.Pool {
	t.Helper()

	pool := New(t)
	m, err := postgres.NewMigrator(pool, postgres.Migrations())
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}
	defer m.Close() //nolint:errcheck // the pool is closed by the test cleanup
	if _, err := m.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return pool
}
