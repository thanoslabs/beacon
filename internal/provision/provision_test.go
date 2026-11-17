package provision_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
)

func TestEnsure(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	const token = "a-very-secret-token-value"
	src := config.Source{Name: "prod", Type: config.SourceTypeWebhook, Token: config.NewSecret(config.SecretRef{Env: "BEACON_SOURCE_TOKEN"}, token)}

	id, err := provision.Ensure(ctx, pool, provision.Sources, src.Name, src)
	if err != nil || id == "" {
		t.Fatalf("Ensure = %q, %v", id, err)
	}
	var spec string
	var updated time.Time
	if err := pool.QueryRow(ctx, `SELECT spec::text, updated_at FROM sources WHERE id = $1`, id).Scan(&spec, &updated); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if strings.Contains(spec, token) {
		t.Errorf("the stored spec contains the token: %s", spec)
	}
	if !strings.Contains(spec, `"env": "BEACON_SOURCE_TOKEN"`) || !strings.Contains(spec, `"type": "webhook"`) {
		t.Errorf("spec = %s, want the type and the secret reference", spec)
	}

	// The same object again keeps the row untouched.
	again, err := provision.Ensure(ctx, pool, provision.Sources, src.Name, src)
	if err != nil || again != id {
		t.Fatalf("second Ensure = %q, %v; want %q", again, err, id)
	}
	var updatedAgain time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM sources WHERE id = $1`, id).Scan(&updatedAgain); err != nil || !updatedAgain.Equal(updated) {
		t.Errorf("updated_at moved from %v to %v for an unchanged object (%v)", updated, updatedAgain, err)
	}

	// A changed object keeps its identifier and gets the new spec.
	src.Labels = map[string]string{"environment": "production"}
	changed, err := provision.Ensure(ctx, pool, provision.Sources, src.Name, src)
	if err != nil || changed != id {
		t.Fatalf("Ensure after a change = %q, %v; want %q", changed, err, id)
	}
	if err := pool.QueryRow(ctx, `SELECT spec::text FROM sources WHERE id = $1`, id).Scan(&spec); err != nil || !strings.Contains(spec, "production") {
		t.Errorf("spec = %s, %v; want the new labels", spec, err)
	}

	other, err := provision.Ensure(ctx, pool, provision.Sources, "staging", src)
	if err != nil || other == id {
		t.Errorf("Ensure for another name = %q, %v; want a different row", other, err)
	}
}

func TestEnsureConcurrently(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			id, err := provision.Ensure(context.Background(), pool, provision.Sources, "prod", map[string]string{"type": "webhook"})
			if err != nil {
				t.Errorf("Ensure: %v", err)
			}
			ids[i] = id
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent Ensure returned different rows: %v", ids)
		}
	}
}
