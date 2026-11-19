package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// Lookup finds accounts in a chat service by email address.
type Lookup interface {
	// LookupByEmail returns the identifier of the account with the address.
	// found is false when the service has no such account, which is not an
	// error. An error must not contain a credential.
	LookupByEmail(ctx context.Context, email string) (externalID string, found bool, err error)
}

// Integration is an integration through which people have accounts.
type Integration struct {
	// ID is the identifier of the integration's row, and Name its name.
	ID   string
	Name string
	// Provider is the chat service.
	Provider string
	// Lookup finds accounts by email address. It is nil for a service in
	// which people link their accounts themselves.
	Lookup Lookup
	// LinkCommand is what a person types in the service to give Beacon a
	// link code, such as "/beacon-link code:", for a service in which people
	// link their accounts themselves.
	LinkCommand string
}

// How often the linker asks, and how much at a time.
const (
	// DefaultInterval is how often a process looks for users without an
	// identity.
	DefaultInterval = 10 * time.Second
	// retryNotFound is how long to wait before asking again for a user the
	// service did not know. People join a workspace after they join Beacon,
	// so the question is worth repeating, but not often.
	retryNotFound = time.Hour
	// retryError is how long to wait after a lookup that failed.
	retryError = 2 * time.Minute
	// claimFor is how long a lookup in progress is hidden from other
	// replicas.
	claimFor = 2 * time.Minute
	// batch bounds the lookups of one pass for one integration. Slack
	// allows about fifty a minute.
	batch = 20
)

// Linker finds the accounts of users in the services that can be asked by
// email address, and stores them. Whom it has asked about is kept in
// Postgres, so replicas do not repeat each other and a restart does not
// start over.
type Linker struct {
	store  *Store
	logger *slog.Logger
	// integrations follows the configuration, and is replaced as a whole
	// when it is reloaded.
	integrations atomic.Pointer[[]Integration]
	unlinked     *prometheus.GaugeVec
	// kick asks Run to look now.
	kick chan struct{}
}

// NewLinker returns a linker that stores what it finds in store.
func NewLinker(store *Store, logger *slog.Logger) *Linker {
	l := &Linker{store: store, logger: logger, kick: make(chan struct{}, 1), unlinked: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "beacon_users_without_identity",
		Help: "Users who have no linked account in the chat service of an integration, by integration, as of the last time this process looked. Such a person cannot be reached directly through that integration.",
	}, []string{"integration"})}
	l.SetIntegrations(nil)
	return l
}

// RegisterMetrics registers the linker's metrics on reg.
func (l *Linker) RegisterMetrics(reg prometheus.Registerer) { reg.MustRegister(l.unlinked) }

// SetIntegrations replaces the integrations. It is safe while the linker
// runs.
func (l *Linker) SetIntegrations(integrations []Integration) {
	integrations = slices.Clone(integrations)
	l.integrations.Store(&integrations)
	l.unlinked.Reset()
}

// Integrations returns the integrations, by name.
func (l *Linker) Integrations() map[string]Integration {
	out := map[string]Integration{}
	for _, in := range *l.integrations.Load() {
		out[in.Name] = in
	}
	return out
}

// Kick makes Run look for users without an identity now, without waiting
// for its interval: after a sign-in, when there may be a new user. It never
// blocks.
func (l *Linker) Kick() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Run calls LinkMissing every interval, and whenever Kick asks, until ctx is
// cancelled.
func (l *Linker) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-l.kick:
		}
		if _, err := l.LinkMissing(ctx); err != nil && ctx.Err() == nil {
			l.logger.ErrorContext(ctx, "link identities", slog.Any("error", err))
		}
	}
}

// candidate is a user to ask a service about.
type candidate struct {
	userID string
	email  string
}

// LinkMissing asks each service that can be asked for the account of every
// user who has none there, and reports how many it linked. A user the
// service does not know is asked for again after an hour, or at once when
// their email address has changed.
//
// It is safe from several replicas at once: a user is claimed, in Postgres,
// before the service is asked, and a claimed user is skipped by the others.
func (l *Linker) LinkMissing(ctx context.Context) (linked int, err error) {
	var errs []error
	for _, in := range *l.integrations.Load() {
		if in.Lookup != nil {
			n, err := l.link(ctx, in)
			linked += n
			if err != nil {
				errs = append(errs, fmt.Errorf("integration %q: %w", in.Name, err))
			}
		}
		if err := l.count(ctx, in); err != nil {
			errs = append(errs, fmt.Errorf("integration %q: %w", in.Name, err))
		}
	}
	return linked, errors.Join(errs...)
}

// people limits a read of users u to the ones who can have an identity:
// enabled, and signed in through the identity provider. The bootstrap admin
// is an account, not a person with an address.
const people = `u.disabled_at IS NULL AND u.oidc_subject IS NOT NULL`

func (l *Linker) link(ctx context.Context, in Integration) (linked int, err error) {
	now := l.store.now()
	rows, err := l.store.db.Query(ctx, `
		INSERT INTO identity_lookups (user_id, integration_id, email, outcome, next_check_at)
		SELECT u.id, $1, u.email, 'pending', $3 FROM users u
		WHERE `+people+`
			AND NOT EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = u.id AND i.integration_id = $1)
			AND NOT EXISTS (SELECT 1 FROM identity_lookups k WHERE k.user_id = u.id AND k.integration_id = $1 AND k.email = u.email AND k.next_check_at > $2)
		ORDER BY u.created_at, u.id LIMIT $4
		ON CONFLICT (user_id, integration_id) DO UPDATE SET email = EXCLUDED.email, outcome = 'pending', next_check_at = EXCLUDED.next_check_at
			WHERE identity_lookups.next_check_at <= $2 OR identity_lookups.email <> EXCLUDED.email
		RETURNING user_id::text, email`, in.ID, now, now.Add(claimFor), batch)
	if err != nil {
		return 0, fmt.Errorf("claim lookups: %w", err)
	}
	claimed, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (candidate, error) {
		var c candidate
		return c, r.Scan(&c.userID, &c.email)
	})
	if err != nil {
		return 0, fmt.Errorf("claim lookups: %w", err)
	}
	for i, c := range claimed {
		if ctx.Err() != nil {
			return linked, ctx.Err()
		}
		log := l.logger.With(slog.String("integration", in.Name), slog.String("provider", in.Provider), slog.String("user_id", c.userID), slog.String("email", c.email))
		externalID, found, err := in.Lookup.LookupByEmail(ctx, c.email)
		switch {
		case err != nil:
			// The service is unwell or is rate limiting: everything that is
			// left of this pass waits, so that it is not asked in vain.
			rest := make([]string, 0, len(claimed)-i)
			for _, r := range claimed[i:] {
				rest = append(rest, r.userID)
			}
			if _, dbErr := l.store.db.Exec(ctx, `UPDATE identity_lookups SET outcome = 'error', next_check_at = $3 WHERE integration_id = $1 AND user_id = ANY($2)`,
				in.ID, rest, l.store.now().Add(retryError)); dbErr != nil {
				return linked, fmt.Errorf("record failed lookup: %w", dbErr)
			}
			return linked, fmt.Errorf("look up %s: %w", c.email, err)
		case !found:
			log.WarnContext(ctx, "no account in the chat service has this user's email address, so they cannot be reached directly through this integration; Beacon asks again in an hour")
			if _, err := l.store.db.Exec(ctx, `UPDATE identity_lookups SET outcome = 'not_found', next_check_at = $3 WHERE integration_id = $1 AND user_id = $2`,
				in.ID, c.userID, l.store.now().Add(retryNotFound)); err != nil {
				return linked, fmt.Errorf("record lookup: %w", err)
			}
		default:
			err := l.store.Link(ctx, l.store.db, c.userID, in.ID, in.Provider, externalID, OriginEmail)
			if errors.Is(err, ErrTaken) {
				log.ErrorContext(ctx, "the chat account with this user's email address is linked to another user already; unlink it there first", slog.String("external_id", externalID))
				if _, err := l.store.db.Exec(ctx, `UPDATE identity_lookups SET outcome = 'error', next_check_at = $3 WHERE integration_id = $1 AND user_id = $2`,
					in.ID, c.userID, l.store.now().Add(retryNotFound)); err != nil {
					return linked, fmt.Errorf("record lookup: %w", err)
				}
				continue
			}
			if err != nil {
				return linked, err
			}
			linked++
			log.InfoContext(ctx, "user linked to their chat account by email address", slog.String("external_id", externalID))
		}
	}
	return linked, nil
}

// count sets the metric of users without an identity for one integration.
func (l *Linker) count(ctx context.Context, in Integration) error {
	var n int
	err := l.store.db.QueryRow(ctx, `SELECT count(*) FROM users u WHERE `+people+`
		AND NOT EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = u.id AND i.integration_id = $1)`, in.ID).Scan(&n)
	if err != nil {
		return fmt.Errorf("count users without an identity: %w", err)
	}
	l.unlinked.WithLabelValues(in.Name).Set(float64(n))
	return nil
}
