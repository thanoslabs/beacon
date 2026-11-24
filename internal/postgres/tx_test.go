package postgres_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// TestAfterCommit: what is asked for inside a transaction happens once the
// transaction has committed, in the order it was asked for, and does not
// happen at all when the transaction rolls back.
func TestAfterCommit(t *testing.T) {
	pool := postgrestest.New(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE things (name text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM things`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	var done []string
	err := postgres.InTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO things VALUES ('a')`); err != nil {
			return err
		}
		postgres.AfterCommit(ctx, func() {
			// The row is there for everyone by now.
			if count() != 1 {
				t.Error("the function ran before the commit")
			}
			done = append(done, "first")
		})
		// A context made from the transaction's belongs to it too.
		type key struct{}
		postgres.AfterCommit(context.WithValue(ctx, key{}, 1), func() { done = append(done, "second") })
		if len(done) != 0 {
			t.Error("a function ran inside the transaction")
		}
		return nil
	})
	if err != nil || !slices.Equal(done, []string{"first", "second"}) {
		t.Fatalf("after a commit: %v, %v", done, err)
	}

	// A transaction that fails after asking does none of it.
	done = nil
	failure := errors.New("no")
	err = postgres.InTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO things VALUES ('b')`); err != nil {
			return err
		}
		postgres.AfterCommit(ctx, func() { done = append(done, "rolled back") })
		return failure
	})
	if !errors.Is(err, failure) || len(done) != 0 || count() != 1 {
		t.Errorf("after a rollback: %v, %v, %d rows", done, err, count())
	}
	// So does one whose commit the database refuses.
	err = postgres.InTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		postgres.AfterCommit(ctx, func() { done = append(done, "refused") })
		_, _ = tx.Exec(ctx, `INSERT INTO things VALUES ('a')`) // a duplicate: the transaction is aborted
		return nil
	})
	if err == nil || len(done) != 0 {
		t.Errorf("after a refused commit: %v, %v", done, err)
	}

	// Outside a transaction there is nothing to wait for.
	postgres.AfterCommit(ctx, func() { done = append(done, "at once") })
	if !slices.Equal(done, []string{"at once"}) {
		t.Errorf("outside a transaction: %v", done)
	}
}
