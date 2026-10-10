// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
)

// LoadPolicy reads the whole policy configuration in one go.
//
// Routes, budgets, and team membership travel together because they are
// evaluated together: a route's rule reacts to a budget's state and its
// conditions reach a person through their teams, so a snapshot assembled from
// three reads taken at three moments could contain a route matching a team that
// the membership half of the snapshot does not yet know about.
func (db *DB) LoadPolicy(ctx context.Context) (policy.Set, error) {
	var set policy.Set

	budgets, err := db.loadBudgets(ctx)
	if err != nil {
		return policy.Set{}, err
	}
	teams, err := db.loadTeamMembership(ctx)
	if err != nil {
		return policy.Set{}, err
	}
	routes, err := db.ListRoutes(ctx)
	if err != nil {
		return policy.Set{}, err
	}
	// Loaded with the rest rather than on its own interval. A stamp lookup that
	// refreshed separately could not disagree with anything here - it decides
	// nothing - but a second snapshot is a second reload loop to reason about,
	// and this one is a map of a few dozen rows.
	stampable, err := db.StampableCostCenters(ctx)
	if err != nil {
		return policy.Set{}, err
	}
	// Provider settings travel with the routes that name them, for the reason
	// the rest of the snapshot is read together: a route and the provider it
	// points at, read at two moments, could disagree about whether that
	// provider exists.
	settings, err := db.ListProviders(ctx)
	if err != nil {
		return policy.Set{}, err
	}

	set.Budgets, set.Teams, set.CostCenters = budgets, teams, stampable
	set.ProviderSettings = settings
	set.Routes = make([]policy.Route, 0, len(routes))
	for _, r := range routes {
		// The engine takes the routing half and leaves the provenance behind.
		// Nothing on the request path has any business knowing who created a
		// route or when.
		set.Routes = append(set.Routes, r.Route)
	}
	set.Index()
	return set, nil
}

func (db *DB) loadBudgets(ctx context.Context) ([]policy.Budget, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, scope, scope_id, period, name, enabled,
		       soft_limit_nanos, hard_limit_nanos
		FROM budgets
		ORDER BY scope, scope_id, period`)
	if err != nil {
		return nil, fmt.Errorf("store: load budgets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []policy.Budget
	for rows.Next() {
		var (
			b      policy.Budget
			period string
			soft   sql.NullInt64
			hard   sql.NullInt64
		)
		if err := rows.Scan(&b.ID, &b.Scope, &b.ScopeID, &period, &b.Name, &b.Enabled,
			&soft, &hard); err != nil {
			return nil, fmt.Errorf("store: scan budget: %w", err)
		}

		b.Period = policy.Period(period)
		b.SoftLimit = money(soft)
		b.HardLimit = money(hard)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read budgets: %w", err)
	}
	return out, nil
}

// loadTeamMembership reads which teams each user belongs to.
//
// Whole-table rather than per-user: this runs once per reload, and a
// single-organization deployment's membership is small enough that reading it
// entirely is cheaper than the query planning for the alternative.
func (db *DB) loadTeamMembership(ctx context.Context) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT user_id, team_id FROM team_members`)
	if err != nil {
		return nil, fmt.Errorf("store: load team membership: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]string)
	for rows.Next() {
		var userID, teamID string
		if err := rows.Scan(&userID, &teamID); err != nil {
			return nil, fmt.Errorf("store: scan team membership: %w", err)
		}
		out[userID] = append(out[userID], teamID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read team membership: %w", err)
	}
	return out, nil
}

// SpendSince totals spend per subject of one scope since a time.
//
// # Read this before reusing it
//
// The user-scoped form groups by user. It exists to enforce a per-user budget,
// it is called only by the policy engine, and its result never leaves the
// process - it carries no entitlement check because it has no caller that needs
// one. Per-user numbers a person will look at come from `SpendByUser`, which
// does carry one. Do not serve this map to a handler.
//
// Only requests that reached a provider count - `attempts > 0`. The in-memory
// ledger applies the same test, and the two have to agree or a reconcile will
// contradict the counting it is supposed to correct.
func (db *DB) SpendSince(ctx context.Context, scope string, since time.Time) (map[string]policy.Spend, error) {
	var query string
	args := []any{since.UTC()}

	// The scope is an allowlist, not interpolated input: each arm is a literal
	// in this file.
	switch scope {
	case policy.ScopeOrg:
		query = `
			SELECT '', ` + spendAggregates + `
			FROM requests r
			WHERE r.attempts > 0 AND r.started_at >= ?`

	case policy.ScopeUser:
		query = `
			SELECT coalesce(r.user_id, ''), ` + spendAggregates + `
			FROM requests r
			WHERE r.attempts > 0 AND r.started_at >= ? AND r.user_id IS NOT NULL
			GROUP BY r.user_id`

	case policy.ScopeTeam:
		// Two populations reach a team, and they are unioned rather than
		// merged into one join on purpose.
		//
		// A person's request reaches a team through membership; a service
		// token's request carries its team on the row, because a pipeline has
		// no user to look one up with.
		//
		// The arms are made disjoint here rather than assumed to be: the second
		// requires user_id IS NULL, so a row carrying both can only ever be
		// counted once even though nothing in the data plane writes one. The
		// alternative - trusting the writer - is how a UNION becomes a double
		// count the first time some other code path fills in both columns.
		// Expressing it as a single join with a COALESCE has the same fault in
		// a form that is harder to see.
		//
		// A user on two teams still contributes their spend to both. That is
		// the honest answer to "what did this team spend", and it means the sum
		// of team budgets can exceed the organization's - a fact about shared
		// people, not an error to normalise away.
		query = `
			SELECT r.team_id, ` + spendAggregates + `
			FROM (
				SELECT tm.team_id AS team_id, rq.cost_nanos,
				       rq.input_tokens, rq.output_tokens,
				       rq.cache_read_tokens, rq.cache_write_tokens, rq.web_searches
				FROM requests rq
				JOIN team_members tm ON tm.user_id = rq.user_id
				WHERE rq.attempts > 0 AND rq.started_at >= ?
				UNION ALL
				SELECT rq.team_id AS team_id, rq.cost_nanos,
				       rq.input_tokens, rq.output_tokens,
				       rq.cache_read_tokens, rq.cache_write_tokens, rq.web_searches
				FROM requests rq
				WHERE rq.attempts > 0 AND rq.started_at >= ?
				  AND rq.team_id IS NOT NULL AND rq.user_id IS NULL
			) r
			GROUP BY r.team_id`
		args = append(args, since.UTC())

	default:
		return nil, fmt.Errorf("store: %w: unknown budget scope %q", ErrInvalidInput, scope)
	}

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: spend since: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]policy.Spend)
	for rows.Next() {
		var (
			key   string
			spend policy.Spend
			cost  int64
		)
		if err := rows.Scan(&key, &cost, &spend.Requests, &spend.Unpriced); err != nil {
			return nil, fmt.Errorf("store: scan spend: %w", err)
		}
		spend.Cost = pricing.Money(cost)
		out[key] = spend
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read spend: %w", err)
	}
	return out, nil
}

// spendAggregates is the shared aggregate for every scope, so a budget means
// the same thing whichever population it governs.
//
// Unpriced counts only requests that used tokens or ran a search. A request that reached the
// provider and failed before producing one has no missing money to report, and
// counting it would make every budget message overstate its own uncertainty.
const spendAggregates = `
	coalesce(sum(r.cost_nanos), 0),
	count(*),
	coalesce(sum(CASE WHEN r.cost_nanos IS NULL
	                   AND (r.input_tokens + r.output_tokens + r.cache_read_tokens + r.cache_write_tokens + r.web_searches) > 0
	                  THEN 1 ELSE 0 END), 0)`

// ApplyPolicy replaces the stored policy with the given set, in one transaction.
//
// Routes and budgets are matched to existing rows by their stable identity - a
// route's name, a budget's scope and period - rather than deleted and
// recreated. Recreating them would mint new ids for rows nothing changed, and
// anything recorded against the old id would point at one that no longer
// exists.
// Applying the same document twice therefore changes nothing.
func (db *DB) ApplyPolicy(ctx context.Context, set policy.Set) (added, updated, removed int, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: apply policy: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	routeAdd, routeUpd, routeDel, err := db.applyRoutes(ctx, tx, set.Routes, now)
	if err != nil {
		return 0, 0, 0, err
	}
	budgetAdd, budgetUpd, budgetDel, err := db.applyBudgets(ctx, tx, set.Budgets, now)
	if err != nil {
		return 0, 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("store: commit policy: %w", err)
	}
	return routeAdd + budgetAdd, routeUpd + budgetUpd, routeDel + budgetDel, nil
}

// existingRouteNames reads which routes are stored, so an applied document can
// tell an edit from a creation and a deletion from an omission.
func existingRouteNames(ctx context.Context, tx *sql.Tx) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM model_routes`)
	if err != nil {
		return nil, fmt.Errorf("store: read existing routes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan existing route: %w", err)
		}
		out[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read existing routes: %w", err)
	}
	return out, nil
}

// applyRoutes writes the document's routes, and removes the ones it does not
// mention.
//
// A policy file is the whole policy: a route absent from it is a route
// somebody deleted, and leaving it behind would mean a document could only ever
// add. That is the same rule the budgets half applies, and it is why applying a
// document is safe to do from CI on every deploy.
func (db *DB) applyRoutes(ctx context.Context, tx *sql.Tx, routes []policy.Route, now time.Time) (added, updated, removed int, err error) {
	existing, err := existingRouteNames(ctx, tx)
	if err != nil {
		return 0, 0, 0, err
	}

	keep := make(map[string]struct{}, len(routes))
	for _, r := range routes {
		if verr := r.Validate(); verr != nil {
			return 0, 0, 0, fmt.Errorf("store: route %q: %w", r.Name, verr)
		}
		keep[r.Name] = struct{}{}

		users, merr := encodeList(r.Users)
		if merr != nil {
			return 0, 0, 0, fmt.Errorf("store: encode the user condition for %q: %w", r.Name, merr)
		}
		teams, merr := encodeList(r.Teams)
		if merr != nil {
			return 0, 0, 0, fmt.Errorf("store: encode the team condition for %q: %w", r.Name, merr)
		}
		tools, merr := encodeList(r.Tools)
		if merr != nil {
			return 0, 0, 0, fmt.Errorf("store: encode the tool condition for %q: %w", r.Name, merr)
		}
		if _, ok := existing[r.Name]; ok {
			if _, uerr := tx.ExecContext(ctx, db.rebind(`
				UPDATE model_routes SET
					description = ?, match_users = ?, match_teams = ?, match_tools = ?,
					target_model = ?, serves_over_soft = ?,
					updated_at = ?
				WHERE name = ?`),
				r.Description, users, teams, tools,
				r.Target.String(), r.ServesOverSoft, now, r.Name); uerr != nil {
				return 0, 0, 0, fmt.Errorf("store: update route %q: %w", r.Name, uerr)
			}
			updated++
		} else {
			if _, ierr := tx.ExecContext(ctx, db.rebind(`
				INSERT INTO model_routes (name, description, match_users, match_teams, match_tools,
				                          target_model, serves_over_soft,
				                          enabled_by, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				r.Name, r.Description, users, teams, tools,
				r.Target.String(), r.ServesOverSoft, "policy file", now, now); ierr != nil {
				return 0, 0, 0, fmt.Errorf("store: insert route %q: %w", r.Name, ierr)
			}
			added++
		}
	}

	for name := range existing {
		if _, ok := keep[name]; ok {
			continue
		}
		if _, derr := tx.ExecContext(ctx, db.rebind(`DELETE FROM model_routes WHERE name = ?`), name); derr != nil {
			return 0, 0, 0, fmt.Errorf("store: delete route %q: %w", name, derr)
		}
		removed++
	}

	return added, updated, removed, nil
}

// SaveBudget creates a budget or changes one, matched on what it governs.
//
// The identity is the scope, its subject and the period - not a generated id -
// because that is what makes a budget one thing. Two budgets on the same person
// for the same window would make "which limit did I hit" unanswerable, so the
// unique index refuses them and this updates rather than inserting a second.
//
// The id is returned because the caller has just been given the row's identity
// for the first time: a console creating a budget needs somewhere to send the
// next edit.
func (db *DB) SaveBudget(ctx context.Context, b policy.Budget) (string, error) {
	if err := b.Validate(); err != nil {
		return "", fmt.Errorf("store: %w: %w", ErrInvalidInput, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store: save budget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	existing, err := existingBudgetIDs(ctx, tx)
	if err != nil {
		return "", err
	}

	id, found := existing[budgetKey{b.Scope, b.ScopeID, string(b.Period)}]
	if found {
		if _, uerr := tx.ExecContext(ctx, db.rebind(`
			UPDATE budgets SET
				name = ?, enabled = ?,
				soft_limit_nanos = ?, hard_limit_nanos = ?,
				updated_at = ?
			WHERE id = ?`),
			b.Name, b.Enabled,
			nullMoney(b.SoftLimit), nullMoney(b.HardLimit),
			now, id); uerr != nil {
			return "", fmt.Errorf("store: update budget %q: %w", b.Label(), uerr)
		}
	} else {
		id = NewID("bdg")
		if ierr := insertBudget(ctx, tx, db, id, b, now); ierr != nil {
			return "", ierr
		}
	}

	if cerr := tx.Commit(); cerr != nil {
		return "", fmt.Errorf("store: commit budget %q: %w", b.Label(), cerr)
	}
	return id, nil
}

// DeleteBudget removes a budget and reports whether it was there.
func (db *DB) DeleteBudget(ctx context.Context, id string) (bool, error) {
	res, err := db.ExecContext(ctx, db.rebind(`DELETE FROM budgets WHERE id = ?`), id)
	if err != nil {
		return false, fmt.Errorf("store: delete budget %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete budget %s: %w", id, err)
	}
	return n > 0, nil
}

// insertBudget writes a new budget row. Shared by the single-budget write and
// the document apply, so a budget created from a screen and one created from a
// file are the same row.
func insertBudget(ctx context.Context, tx *sql.Tx, db *DB, id string, b policy.Budget, now time.Time) error {
	_, err := tx.ExecContext(ctx, db.rebind(`
		INSERT INTO budgets (
			id, scope, scope_id, period, name, enabled,
			soft_limit_nanos, hard_limit_nanos,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, b.Scope, b.ScopeID, string(b.Period), b.Name, b.Enabled,
		nullMoney(b.SoftLimit), nullMoney(b.HardLimit),
		now, now)
	if err != nil {
		return fmt.Errorf("store: insert budget %q: %w", b.Label(), err)
	}
	return nil
}

// budgetKey is a budget's stable identity: what it governs, over what window.
type budgetKey struct{ scope, scopeID, period string }

func existingBudgetIDs(ctx context.Context, tx *sql.Tx) (map[budgetKey]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, scope, scope_id, period FROM budgets`)
	if err != nil {
		return nil, fmt.Errorf("store: read existing budgets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[budgetKey]string)
	for rows.Next() {
		var id string
		var k budgetKey
		if err := rows.Scan(&id, &k.scope, &k.scopeID, &k.period); err != nil {
			return nil, fmt.Errorf("store: scan existing budget: %w", err)
		}
		out[k] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read existing budgets: %w", err)
	}
	return out, nil
}

func (db *DB) applyBudgets(ctx context.Context, tx *sql.Tx, budgets []policy.Budget, now time.Time) (added, updated, removed int, err error) {
	existing, err := existingBudgetIDs(ctx, tx)
	if err != nil {
		return 0, 0, 0, err
	}

	keep := make(map[budgetKey]struct{}, len(budgets))
	for _, b := range budgets {
		k := budgetKey{b.Scope, b.ScopeID, string(b.Period)}
		keep[k] = struct{}{}

		if id, ok := existing[k]; ok {
			_, err = tx.ExecContext(ctx, db.rebind(`
				UPDATE budgets SET
					name = ?, enabled = ?,
					soft_limit_nanos = ?, hard_limit_nanos = ?,
					updated_at = ?
				WHERE id = ?`),
				b.Name, b.Enabled,
				nullMoney(b.SoftLimit), nullMoney(b.HardLimit),
				now, id)
			if err != nil {
				return 0, 0, 0, fmt.Errorf("store: update budget %q: %w", b.Label(), err)
			}
			updated++
			continue
		}

		if ierr := insertBudget(ctx, tx, db, NewID("bdg"), b, now); ierr != nil {
			return 0, 0, 0, ierr
		}
		added++
	}

	for k, id := range existing {
		if _, ok := keep[k]; ok {
			continue
		}
		if _, err = tx.ExecContext(ctx, db.rebind(`DELETE FROM budgets WHERE id = ?`), id); err != nil {
			return 0, 0, 0, fmt.Errorf("store: delete %s budget: %w", k.scope, err)
		}
		removed++
	}

	return added, updated, removed, nil
}

// money converts a nullable rate column into an optional amount.
func money(v sql.NullInt64) *pricing.Money {
	if !v.Valid {
		return nil
	}
	m := pricing.Money(v.Int64)
	return &m
}

func nullMoney(m *pricing.Money) any {
	if m == nil {
		return nil
	}
	return int64(*m)
}
