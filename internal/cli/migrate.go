package cli

import (
	"context"
	"fmt"

	"github.com/thanoslabs/beacon/internal/postgres"
)

func runMigrate(ctx context.Context, e env, args []string) error {
	var (
		dbCfg   postgres.Config
		status  bool
		envErrs []error
	)
	fs := newFlagSet(e, "migrate", "Apply database schema migrations")
	databaseSettings(fs, e, &dbCfg, &envErrs)
	fs.BoolVar(&status, "status", false, "list migrations and whether each is applied, without changing anything")
	if err := parseFlags(fs, args, envErrs...); err != nil {
		return err
	}
	if err := checkDatabaseSettings(dbCfg); err != nil {
		return err
	}

	pool, err := postgres.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	migrator, err := postgres.NewMigrator(pool, postgres.Migrations())
	if err != nil {
		return err
	}
	defer migrator.Close() //nolint:errcheck // the pool is closed right after

	if status {
		migrations, err := migrator.Status(ctx)
		if err != nil {
			return err
		}
		for _, m := range migrations {
			state := "pending"
			if m.Applied {
				state = "applied " + m.AppliedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			fmt.Fprintf(e.stdout, "%-40s %s\n", m.Name, state)
		}
		return nil
	}

	applied, err := migrator.Migrate(ctx)
	for _, m := range applied {
		fmt.Fprintf(e.stdout, "applied %s\n", m.Name)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Fprintln(e.stdout, "database schema is up to date")
	}
	return nil
}
