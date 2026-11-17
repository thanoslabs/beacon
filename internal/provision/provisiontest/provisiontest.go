// Package provisiontest gives tests provisioned rows to reference.
package provisiontest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/provision"
)

// Row inserts an empty object of the given kind, which must have no columns
// beyond the common ones, and returns the identifier of its row.
func Row(t testing.TB, pool *pgxpool.Pool, kind provision.Kind, name string) string {
	t.Helper()
	var id string
	// The table name is one of the package's constants.
	if err := pool.QueryRow(context.Background(), `INSERT INTO `+string(kind)+` (name) VALUES ($1) RETURNING id::text`, name).Scan(&id); err != nil {
		t.Fatalf("insert into %s: %v", kind, err)
	}
	return id
}
