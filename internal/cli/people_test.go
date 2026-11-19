package cli

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/identity"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
	"github.com/thanoslabs/beacon/internal/schedule"
)

// TestWhereAnIntegrationReachesAPerson: a chat integration reaches a person
// at the account that is linked to them, and an email integration at the
// address they have in Beacon, which nobody needs to link.
func TestWhereAnIntegrationReachesAPerson(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	slackID := provisiontest.Row(t, pool, provision.Integrations, "slack")
	emailID := provisiontest.Row(t, pool, provision.Integrations, "email")
	p := &people{schedules: schedule.NewStore(pool), identities: store}

	var ada, gone, bootstrap string
	err := pool.QueryRow(ctx, `WITH a AS (INSERT INTO users (email, oidc_issuer, oidc_subject) VALUES ('ada@example.com', 'https://idp.example.com', 'ada') RETURNING id),
		g AS (INSERT INTO users (email, oidc_issuer, oidc_subject, disabled_at) VALUES ('gone@example.com', 'https://idp.example.com', 'gone', now()) RETURNING id),
		b AS (INSERT INTO users (email) VALUES ('bootstrap-admin@beacon.invalid') RETURNING id)
		SELECT a.id::text, g.id::text, b.id::text FROM a, g, b`).Scan(&ada, &gone, &bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(ctx, pool, ada, slackID, identity.ProviderSlack, "U0ADA", identity.OriginEmail); err != nil {
		t.Fatal(err)
	}
	address := func(integration, user string) (string, bool) {
		t.Helper()
		var out string
		var found bool
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) (err error) {
			out, found, err = p.Address(ctx, tx, integration, user)
			return err
		})
		if err != nil {
			t.Fatalf("Address: %v", err)
		}
		return out, found
	}

	// Before the configuration says which integrations send email, an
	// integration reaches only linked accounts.
	if got, found := address(emailID, ada); found {
		t.Errorf("address through an integration nobody is linked with = %q", got)
	}
	p.SetAddressed(map[string]bool{emailID: true})
	if got, found := address(emailID, ada); !found || got != "ada@example.com" {
		t.Errorf("Ada by email = %q, %v", got, found)
	}
	if got, found := address(slackID, ada); !found || got != "U0ADA" {
		t.Errorf("Ada in Slack = %q, %v", got, found)
	}
	// A disabled user and the bootstrap admin are not written to.
	for name, user := range map[string]string{"a disabled user": gone, "the bootstrap admin": bootstrap} {
		if got, found := address(emailID, user); found {
			t.Errorf("%s by email = %q, want nobody", name, got)
		}
	}
}
