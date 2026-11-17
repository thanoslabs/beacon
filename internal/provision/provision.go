// Package provision reconciles the configuration into Postgres: it makes the
// rows of the provisioned objects match what the files define.
//
// The running process works from the files it loaded. The rows exist so that
// runtime state, such as an alert, has something stable to reference, and so
// that an object which history refers to can still be shown after it has
// left the files. See docs/configuration-and-data-model.md.
package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/config"
)

// Kind is a kind of provisioned object. Its value is the name of the table
// that holds the copies.
type Kind string

// The kinds, in the order they are reconciled: an object comes after the
// objects its row refers to.
const (
	Sources            Kind = "sources"
	Integrations       Kind = "integrations"
	Teams              Kind = "teams"
	Schedules          Kind = "schedules"
	EscalationPolicies Kind = "escalation_policies"
	RoutingRules       Kind = "routing_rules"
)

// Kinds lists every kind in reconciliation order.
var Kinds = []Kind{Sources, Integrations, Teams, Schedules, EscalationPolicies, RoutingRules}

// Action is what reconciliation did to one object.
type Action string

// The actions. An unchanged object is not reported.
const (
	Created Action = "created"
	Updated Action = "updated"
	// Revived is an object that had been removed and is defined again. It
	// keeps its row, so history stays attached to it.
	Revived Action = "revived"
	// Removed is an object that left the files. Its row is kept and marked.
	Removed Action = "removed"
)

// Change is one object that reconciliation touched.
type Change struct {
	Kind   Kind
	Name   string
	Action Action
}

// Result is the outcome of a reconciliation.
type Result struct {
	// IDs maps the name of every object in the configuration to the
	// identifier of its row, by kind.
	IDs map[Kind]map[string]string
	// Changes lists what was created, updated, revived and removed. It is
	// empty when the database already matched the files.
	Changes []Change
}

// lockKey names the advisory lock that serialises reconciliations.
const lockKey = "beacon/reconcile-configuration"

// object is one provisioned object on its way into a row.
type object struct {
	name string
	spec any
	// column and value set one more column of the row, for the kinds that
	// have one: a schedule's team and a routing rule's position.
	column string
	value  any
}

// Reconcile makes the provisioned rows match cfg: objects are created and
// updated, and objects that are no longer defined are marked as removed. A
// removed object keeps its row, so that whatever refers to it still
// resolves; when it is defined again the same row is revived.
//
// cfg must be valid. Everything happens in one transaction, so the database
// never holds half a configuration. Reconcile is safe to repeat, and safe
// from several replicas at once: an advisory lock makes them take turns, and
// the second finds nothing left to do.
func Reconcile(ctx context.Context, db *pgxpool.Pool, cfg config.Config) (Result, error) {
	res := Result{IDs: map[Kind]map[string]string{}}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		for _, kind := range Kinds {
			objects, err := objectsOf(kind, cfg, res.IDs)
			if err != nil {
				return err
			}
			ids, changes, err := reconcileKind(ctx, tx, kind, objects)
			if err != nil {
				return err
			}
			res.IDs[kind] = ids
			res.Changes = append(res.Changes, changes...)
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("reconcile configuration: %w", err)
	}
	return res, nil
}

// objectsOf lists the objects of one kind in cfg. ids holds the rows of the
// kinds reconciled so far.
func objectsOf(kind Kind, cfg config.Config, ids map[Kind]map[string]string) ([]object, error) {
	var out []object
	switch kind {
	case Sources:
		for _, o := range cfg.Sources {
			out = append(out, object{name: o.Name, spec: o})
		}
	case Integrations:
		for _, o := range cfg.Integrations {
			out = append(out, object{name: o.Name, spec: o})
		}
	case Teams:
		for _, o := range cfg.Teams {
			out = append(out, object{name: o.Name, spec: o})
		}
	case Schedules:
		for _, o := range cfg.Schedules {
			team, ok := ids[Teams][o.Team]
			if !ok {
				return nil, fmt.Errorf("schedule %q: team %q is not in the configuration", o.Name, o.Team)
			}
			out = append(out, object{name: o.Name, spec: o, column: "team_id", value: team})
		}
	case EscalationPolicies:
		for _, o := range cfg.EscalationPolicies {
			out = append(out, object{name: o.Name, spec: o})
		}
	case RoutingRules:
		if cfg.Routing != nil {
			for i, o := range cfg.Routing.Rules {
				out = append(out, object{name: o.Name, spec: o, column: "position", value: i})
			}
		}
	}
	return out, nil
}

// row is what reconciliation needs to know about a stored object.
type row struct {
	id          string
	hash        string
	removed     bool
	provisioned bool
}

// reconcileKind makes the rows of one kind match objects.
func reconcileKind(ctx context.Context, tx pgx.Tx, kind Kind, objects []object) (map[string]string, []Change, error) {
	// The table and column names come from the constants in this file, never
	// from input.
	table := string(kind)

	// One row per name: a removed row is revived rather than joined by a
	// second one, so this map cannot lose a row.
	existing := map[string]row{}
	rows, err := tx.Query(ctx, `SELECT name, id::text, spec_hash, removed_at IS NOT NULL, provisioned FROM `+table+` ORDER BY removed_at NULLS FIRST`)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", table, err)
	}
	for rows.Next() {
		var name string
		var r row
		if err := rows.Scan(&name, &r.id, &r.hash, &r.removed, &r.provisioned); err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", table, err)
		}
		if _, seen := existing[name]; !seen {
			existing[name] = r
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", table, err)
	}

	ids := make(map[string]string, len(objects))
	var changes []Change
	for _, o := range objects {
		// The stored form holds secret references, never values;
		// config.Secret encodes itself that way.
		spec, err := json.Marshal(o.spec)
		if err != nil {
			return nil, nil, fmt.Errorf("encode %s %q: %w", table, o.name, err)
		}
		sum := sha256.New()
		sum.Write(spec)
		if o.column != "" {
			fmt.Fprintf(sum, "\x00%s=%v", o.column, o.value)
		}
		hash := hex.EncodeToString(sum.Sum(nil))

		old, found := existing[o.name]
		switch {
		case found && !old.provisioned:
			return nil, nil, fmt.Errorf("%s %q exists and is not managed by configuration; remove it or choose another name", table, o.name)
		case !found:
			columns, placeholders, args := "name, spec, spec_hash", "$1, $2, $3", []any{o.name, spec, hash}
			if o.column != "" {
				columns, placeholders, args = columns+", "+o.column, placeholders+", $4", append(args, o.value)
			}
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO `+table+` (`+columns+`) VALUES (`+placeholders+`) RETURNING id::text`, args...).Scan(&id); err != nil {
				return nil, nil, fmt.Errorf("create %s %q: %w", table, o.name, err)
			}
			ids[o.name] = id
			changes = append(changes, Change{Kind: kind, Name: o.name, Action: Created})
		case old.removed || old.hash != hash:
			set, args := "spec = $2, spec_hash = $3, updated_at = now(), removed_at = NULL", []any{old.id, spec, hash}
			if o.column != "" {
				set, args = set+", "+o.column+" = $4", append(args, o.value)
			}
			if _, err := tx.Exec(ctx, `UPDATE `+table+` SET `+set+` WHERE id = $1`, args...); err != nil {
				return nil, nil, fmt.Errorf("update %s %q: %w", table, o.name, err)
			}
			ids[o.name] = old.id
			action := Updated
			if old.removed {
				action = Revived
			}
			changes = append(changes, Change{Kind: kind, Name: o.name, Action: action})
		default:
			ids[o.name] = old.id
		}
	}

	for _, name := range slices.Sorted(maps.Keys(existing)) {
		old := existing[name]
		if _, wanted := ids[name]; wanted || old.removed || !old.provisioned {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE `+table+` SET removed_at = now(), updated_at = now() WHERE id = $1`, old.id); err != nil {
			return nil, nil, fmt.Errorf("remove %s %q: %w", table, name, err)
		}
		changes = append(changes, Change{Kind: kind, Name: name, Action: Removed})
	}
	return ids, changes, nil
}
