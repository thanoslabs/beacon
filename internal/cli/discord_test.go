package cli

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/thanoslabs/beacon/internal/identity"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
)

type noLookup struct{}

func (noLookup) LookupByEmail(context.Context, string) (string, bool, error) { return "", false, nil }

// TestALinkCodeOnlyWorksWhereItWasIssued: a code is redeemed for the
// integration named in the path of the request, which must be one in which
// people link their accounts themselves. Any other name is a code that is
// not valid, and uses nothing up.
func TestALinkCodeOnlyWorksWhereItWasIssued(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	store := identity.NewStore(pool)
	linker := identity.NewLinker(store, slog.New(slog.DiscardHandler))
	discordID := provisiontest.Row(t, pool, provision.Integrations, "discord")
	slackID := provisiontest.Row(t, pool, provision.Integrations, "slack")
	linker.SetIntegrations([]identity.Integration{
		{ID: discordID, Name: "discord", Provider: identity.ProviderDiscord},
		{ID: slackID, Name: "slack", Provider: identity.ProviderSlack, Lookup: noLookup{}},
	})
	p := &provisioner{pool: pool, identities: store, linker: linker}

	var ada string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, display_name, oidc_issuer, oidc_subject) VALUES ('ada@example.com', 'Ada', 'https://idp.example.com', 'ada') RETURNING id::text`).Scan(&ada); err != nil {
		t.Fatal(err)
	}
	code, _, err := store.StartLink(ctx, ada, discordID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"slack", "nope", ""} {
		if _, err := p.CompleteLink(ctx, name, "300000000000000001", code); !errors.Is(err, identity.ErrBadCode) {
			t.Errorf("CompleteLink through %q = %v, want the code refused", name, err)
		}
	}
	if name, err := p.CompleteLink(ctx, "discord", "300000000000000001", code); err != nil || name != "Ada" {
		t.Fatalf("CompleteLink = %q, %v", name, err)
	}
	if id, name, found, err := p.UserFor(ctx, "discord", "300000000000000001"); err != nil || !found || id != ada || name != "Ada" {
		t.Errorf("UserFor = %q %q %v %v", id, name, found, err)
	}
}
