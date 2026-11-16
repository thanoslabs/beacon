package postgres_test

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

func newMigrator(t *testing.T, pool *pgxpool.Pool, fsys fs.FS) *postgres.Migrator {
	t.Helper()
	m, err := postgres.NewMigrator(pool, fsys)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return exists
}

func TestEmbeddedMigrationsAreWellFormed(t *testing.T) {
	entries, err := fs.ReadDir(postgres.Migrations(), ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations are embedded")
	}
	name := regexp.MustCompile(`^\d{5}_[a-z0-9_]+\.sql$`)
	for i, e := range entries {
		if !name.MatchString(e.Name()) {
			t.Errorf("migration %q does not match <5 digits>_<snake_case>.sql", e.Name())
		}
		// Versions are consecutive, so two branches cannot both add the same
		// number without a conflict being noticed here.
		if want := fmt.Sprintf("%05d_", i+1); !strings.HasPrefix(e.Name(), want) {
			t.Errorf("migration %q: want the name to start with %s", e.Name(), want)
		}
		body, err := fs.ReadFile(postgres.Migrations(), e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, marker := range []string{"-- +goose Up", "-- +goose Down"} {
			if !strings.Contains(string(body), marker) {
				t.Errorf("migration %q has no %q section", e.Name(), marker)
			}
		}
	}
}

func TestMigrateEmbedded(t *testing.T) {
	pool := postgrestest.New(t)
	ctx := context.Background()
	m := newMigrator(t, pool, postgres.Migrations())

	before, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, s := range before {
		if s.Applied {
			t.Errorf("migration %s is applied in an empty database", s.Name)
		}
	}

	applied, err := m.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != len(before) {
		t.Errorf("applied %d migrations, want %d", len(applied), len(before))
	}
	if !tableExists(t, pool, "users") {
		t.Error("users table does not exist after migrating")
	}

	// A second run finds nothing to do.
	again, err := m.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Migrate applied %d migrations, want 0", len(again))
	}

	after, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, s := range after {
		if !s.Applied || s.AppliedAt.IsZero() {
			t.Errorf("migration %s: applied=%v at %v, want applied with a time", s.Name, s.Applied, s.AppliedAt)
		}
	}
}

func TestUsersTableConstraints(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO users (email) VALUES ('ada@example.com')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	rejected := map[string]string{
		"duplicate email":         `INSERT INTO users (email) VALUES ('ada@example.com')`,
		"mixed-case email":        `INSERT INTO users (email) VALUES ('Grace@example.com')`,
		"unknown role":            `INSERT INTO users (email, role) VALUES ('grace@example.com', 'owner')`,
		"issuer without subject":  `INSERT INTO users (email, oidc_issuer) VALUES ('grace@example.com', 'https://idp.example.com')`,
		"missing email":           `INSERT INTO users (display_name) VALUES ('Grace')`,
		"subject without issuer":  `INSERT INTO users (email, oidc_subject) VALUES ('grace@example.com', 'abc')`,
		"duplicate oidc identity": `INSERT INTO users (email, oidc_issuer, oidc_subject) VALUES ('a@example.com', 'i', 's'), ('b@example.com', 'i', 's')`,
	}
	for name, stmt := range rejected {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, stmt); err == nil {
				t.Errorf("statement succeeded, want a constraint violation: %s", stmt)
			}
		})
	}
}

// testMigrations is a small migration set used to test the migrator itself,
// independent of the real schema.
func testMigrations(extra ...string) fstest.MapFS {
	fsys := fstest.MapFS{
		"00001_first.sql": {Data: []byte("-- +goose Up\nCREATE TABLE first (id int);\n-- +goose Down\nDROP TABLE first;\n")},
	}
	for i := 0; i+1 < len(extra); i += 2 {
		fsys[extra[i]] = &fstest.MapFile{Data: []byte(extra[i+1])}
	}
	return fsys
}

func TestMigrateAppliesOnlyPending(t *testing.T) {
	pool := postgrestest.New(t)
	ctx := context.Background()

	if _, err := newMigrator(t, pool, testMigrations()).Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	withSecond := testMigrations("00002_second.sql", "-- +goose Up\nCREATE TABLE second (id int);\n-- +goose Down\nDROP TABLE second;\n")
	applied, err := newMigrator(t, pool, withSecond).Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate with a new migration: %v", err)
	}
	if len(applied) != 1 || applied[0].Version != 2 || applied[0].Name != "00002_second.sql" {
		t.Errorf("applied = %+v, want only version 2", applied)
	}
	if !tableExists(t, pool, "second") {
		t.Error("table from the second migration does not exist")
	}
}

func TestMigrateFailureRollsBackAndReports(t *testing.T) {
	pool := postgrestest.New(t)
	ctx := context.Background()

	broken := testMigrations("00002_broken.sql", "-- +goose Up\nCREATE TABLE half_done (id int);\nSELECT * FROM does_not_exist;\n-- +goose Down\n")
	m := newMigrator(t, pool, broken)

	applied, err := m.Migrate(ctx)
	if err == nil {
		t.Fatal("Migrate succeeded, want an error from the broken migration")
	}
	if !strings.Contains(err.Error(), "00002_broken.sql") {
		t.Errorf("error = %q, want it to name the failing migration", err)
	}
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Errorf("applied = %+v, want only version 1", applied)
	}
	if !tableExists(t, pool, "first") {
		t.Error("the migration before the failure was not kept")
	}
	if tableExists(t, pool, "half_done") {
		t.Error("the failed migration was not rolled back")
	}
}

func TestMigrateConcurrently(t *testing.T) {
	// Several replicas starting together must apply each migration once.
	pool := postgrestest.New(t)
	ctx := context.Background()

	const replicas = 4
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for range replicas {
		m := newMigrator(t, pool, testMigrations())
		wg.Go(func() {
			applied, err := m.Migrate(ctx)
			if err != nil {
				t.Errorf("concurrent Migrate: %v", err)
			}
			mu.Lock()
			total += len(applied)
			mu.Unlock()
		})
	}
	wg.Wait()

	if total != 1 {
		t.Errorf("migrations applied across replicas = %d, want 1", total)
	}
}
