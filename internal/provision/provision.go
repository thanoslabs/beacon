// Package provision copies provisioned objects into Postgres, so that runtime
// rows such as alerts have something stable to reference.
//
// This is the smallest piece of what configuration reconciliation will do: it
// makes sure an object has a row. It does not mark objects that left the
// configuration as removed, and it does not revive removed rows.
package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind is a kind of provisioned object. Its value is the name of the table
// that holds the copies.
type Kind string

// The kinds that have tables.
const (
	Sources      Kind = "sources"
	Integrations Kind = "integrations"
)

// Ensure makes sure the object of this kind and name has a row holding spec,
// and returns the row's identifier. spec is stored as JSON, so it must encode
// secrets as references and never as values; config.Secret does.
//
// It is safe when several replicas start together.
func Ensure(ctx context.Context, db *pgxpool.Pool, kind Kind, name string, spec any) (id string, err error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode %s %q: %w", kind, name, err)
	}
	sum := sha256.Sum256(encoded)
	table := string(kind)
	// The table name comes from the constants above, never from input.
	err = db.QueryRow(ctx, `
		INSERT INTO `+table+` AS t (name, spec, spec_hash) VALUES ($1, $2, $3)
		ON CONFLICT (name) WHERE removed_at IS NULL DO UPDATE SET
			spec = EXCLUDED.spec,
			spec_hash = EXCLUDED.spec_hash,
			updated_at = CASE WHEN t.spec_hash = EXCLUDED.spec_hash THEN t.updated_at ELSE now() END
		RETURNING id::text`, name, encoded, hex.EncodeToString(sum[:])).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store %s %q: %w", kind, name, err)
	}
	return id, nil
}
