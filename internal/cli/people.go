package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/identity"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/schedule"
)

// people answers the escalation engine's questions about people from the
// packages that know: schedules say who is on call, and identities say
// where an integration reaches a person. It implements notify.People.
type people struct {
	schedules  *schedule.Store
	identities *identity.Store
}

// OnCall returns who is on call for a schedule now. A schedule that has
// left the configuration has nobody.
func (p people) OnCall(ctx context.Context, tx pgx.Tx, scheduleID string) ([]notify.Person, error) {
	on, err := p.schedules.OnCall(ctx, tx, scheduleID)
	if errors.Is(err, schedule.ErrNotFound) || (err == nil && on.UserID == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	person := notify.Person{UserID: on.UserID, Email: on.Email}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(display_name, ''), email) FROM users WHERE id = $1`, on.UserID).Scan(&person.Name); err != nil {
		return nil, fmt.Errorf("read the user on call: %w", err)
	}
	return []notify.Person{person}, nil
}

// TeamMembers returns the members of a team, whichever way they became
// members, by email address.
func (p people) TeamMembers(ctx context.Context, tx pgx.Tx, teamID string) ([]notify.Person, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT u.id::text, u.email, COALESCE(NULLIF(u.display_name, ''), u.email) FROM team_members m JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1 AND u.disabled_at IS NULL ORDER BY u.email`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read team members: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (notify.Person, error) {
		var person notify.Person
		return person, r.Scan(&person.UserID, &person.Email, &person.Name)
	})
}

// User returns the user with an email address.
func (p people) User(ctx context.Context, tx pgx.Tx, email string) (notify.Person, bool, error) {
	person := notify.Person{Email: email}
	err := tx.QueryRow(ctx, `SELECT id::text, COALESCE(NULLIF(display_name, ''), email) FROM users WHERE email = $1 AND disabled_at IS NULL`, email).Scan(&person.UserID, &person.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return notify.Person{}, false, nil
	}
	if err != nil {
		return notify.Person{}, false, fmt.Errorf("read user: %w", err)
	}
	return person, true, nil
}

// Address returns the account through which an integration reaches a
// person directly.
func (p people) Address(ctx context.Context, tx pgx.Tx, integrationID, userID string) (string, bool, error) {
	return p.identities.Address(ctx, tx, integrationID, userID)
}
