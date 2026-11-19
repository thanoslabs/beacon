package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// TestOverrideIsRecordedOnceWhenTwoReplicasTakeIt plays out a race that
// timing alone seldom produces: a replica finds an override that has begun
// and has no shift, and by the time it holds the override's row, another
// replica has recorded the shift and committed. Under load this once made
// the second replica try to record the override again and fail on the
// unique index. The second one must find the shift and leave it.
func TestOverrideIsRecordedOnceWhenTwoReplicasTakeIt(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	b := begun{starts: now.Add(-time.Hour), ends: now.Add(time.Hour)}
	err := pool.QueryRow(ctx, `
		WITH u AS (INSERT INTO users (email) VALUES ('ken@example.com') RETURNING id),
			t AS (INSERT INTO teams (name) VALUES ('platform') RETURNING id),
			s AS (INSERT INTO schedules (name, team_id) SELECT 'primary', id FROM t RETURNING id)
		INSERT INTO schedule_overrides (schedule_id, user_id, starts_at, ends_at, created_by, created_at)
		SELECT s.id, u.id, $1, $2, u.id, $1 FROM s, u RETURNING id::text, schedule_id::text, user_id::text`, b.starts, b.ends).
		Scan(&b.id, &b.scheduleID, &b.userID)
	if err != nil {
		t.Fatal(err)
	}

	// Both replicas found the override before either recorded it. They
	// take turns at its row, as the lock makes them.
	for replica, want := range []bool{true, false} {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM schedule_overrides WHERE id = $1 FOR UPDATE`, b.id); err != nil {
				return err
			}
			h, recorded, err := recordBegun(ctx, tx, b, now)
			if err != nil {
				return err
			}
			if recorded != want || (recorded && (h.To != "ken@example.com" || h.Schedule != "primary" || !h.At.Equal(b.starts))) {
				t.Errorf("replica %d: recorded = %v with %+v, want %v", replica, recorded, h, want)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("replica %d: %v", replica, err)
		}
	}
	var shifts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM shifts WHERE override_id = $1 AND ends_at IS NULL`, b.id).Scan(&shifts); err != nil || shifts != 1 {
		t.Errorf("%d shifts for the override, %v; want 1", shifts, err)
	}
}
