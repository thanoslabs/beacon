package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type afterCommitKey struct{}

// InTx runs f in a transaction, which commits when f returns nil and rolls
// back otherwise. The context f is given carries the transaction's list of
// things to do once it has committed; see AfterCommit.
func InTx(ctx context.Context, db *pgxpool.Pool, f func(ctx context.Context, tx pgx.Tx) error) error {
	var after []func()
	inner := context.WithValue(ctx, afterCommitKey{}, &after)
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error { return f(inner, tx) }); err != nil {
		return err
	}
	for _, do := range after {
		do()
	}
	return nil
}

// AfterCommit asks for do to be called once the transaction that ctx
// belongs to has committed, and not at all if it rolls back. It is for what
// cannot be undone and must only happen for a change that is real, such as
// counting it in a metric. do must not fail and must not block.
//
// ctx must be the context that InTx handed to its function, or one made
// from it. With any other context there is no transaction to wait for, and
// do is called at once.
func AfterCommit(ctx context.Context, do func()) {
	if after, ok := ctx.Value(afterCommitKey{}).(*[]func()); ok {
		*after = append(*after, do)
		return
	}
	do()
}
