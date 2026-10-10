// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dnxlabs/panorama/internal/costcenters"
)

// CostCenter is one accounting line.
//
// It holds a plan rather than a limit, and the difference is not cosmetic:
// nothing here is enforced. See the costcenters package for why a cost center
// cannot block a request, and internal/store/migrations for why this is not a
// fourth scope on `budgets`.
type CostCenter struct {
	ID          string
	Code        string
	Name        string
	Owner       string
	Description string

	// PlanNanos is what this line is planned to spend in a calendar month.
	// Nil where nobody has set one, which is not the same as zero - zero is a
	// line that is meant to spend nothing.
	PlanNanos     *int64
	WarnAtPercent int

	AcceptsStamp bool
	Archived     bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StoredRule is an allocation rule with its provenance.
//
// The resolver takes the embedded half and leaves the timestamps behind, the
// same split LoadPolicy makes for a route: nothing that decides an allocation
// has any business knowing when the rule was written.
type StoredRule struct {
	costcenters.Rule
	CreatedAt time.Time
	UpdatedAt time.Time
}

/* ------------------------------------------------------------- reading -- */

const costCenterSelect = `
	SELECT id, code, name, owner, description,
	       plan_nanos, warn_at_percent, accepts_stamp, archived,
	       created_at, updated_at
	FROM cost_centers`

// ListCostCenters reads every accounting line, archived ones included only when
// asked.
//
// Ordered by code, because a chart of accounts is read in the order finance
// keeps it and not in the order somebody happened to create rows.
func (db *DB) ListCostCenters(ctx context.Context, includeArchived bool) ([]CostCenter, error) {
	// Bound as a parameter rather than written into the SQL: SQLite stores a
	// boolean as 0/1 and Postgres as true/false, and a literal would need a
	// dialect branch for a condition the driver already knows how to bind.
	q, args := costCenterSelect, []any{}
	if !includeArchived {
		q += ` WHERE archived = ?`
		args = append(args, false)
	}
	q += ` ORDER BY code`

	rows, err := db.QueryContext(ctx, db.rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list cost centers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]CostCenter, 0, 8)
	for rows.Next() {
		var c CostCenter
		var plan sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Code, &c.Name, &c.Owner, &c.Description,
			&plan, &c.WarnAtPercent, &c.AcceptsStamp, &c.Archived,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan cost center: %w", err)
		}
		if plan.Valid {
			v := plan.Int64
			c.PlanNanos = &v
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCostCenterRules reads the ordered allocation list.
//
// The ordering is (position, id) and it is applied here rather than in the
// resolver, so that the console's tester, the recompute and any future caller
// are all reading one order. A resolver that sorted for itself would be a
// second place precedence was decided.
func (db *DB) ListCostCenterRules(ctx context.Context) ([]StoredRule, error) {
	return listRules(ctx, db.QueryContext)
}

type queryFunc func(ctx context.Context, query string, args ...any) (*sql.Rows, error)

func listRules(ctx context.Context, q queryFunc) ([]StoredRule, error) {
	rows, err := q(ctx, `
		SELECT id, cost_center_id, position, match_on, pattern, created_at, updated_at
		FROM cost_center_rules
		ORDER BY position, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list cost center rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]StoredRule, 0, 16)
	for rows.Next() {
		var r StoredRule
		var match string
		if err := rows.Scan(&r.ID, &r.CostCenterID, &r.Position, &match, &r.Pattern,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan cost center rule: %w", err)
		}
		r.Match = costcenters.Match(match)
		out = append(out, r)
	}
	return out, rows.Err()
}

// StampableCostCenters is the lookup the policy snapshot carries so a stamped
// request costs one map read rather than a query.
//
// Only lines a client may actually name are in it - not archived, and accepting
// stamps - because filtering at load time is what keeps the request path to a
// single condition. See costcenters.Stampable for why collapsing "no such code"
// and "that line refuses stamps" into one absent key is correct rather than
// lossy.
func (db *DB) StampableCostCenters(ctx context.Context) (costcenters.Stampable, error) {
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT code, id FROM cost_centers
		WHERE archived = ? AND accepts_stamp = ?`), false, true)
	if err != nil {
		return nil, fmt.Errorf("store: load stampable cost centers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(costcenters.Stampable, 8)
	for rows.Next() {
		var code, id string
		if err := rows.Scan(&code, &id); err != nil {
			return nil, fmt.Errorf("store: scan stampable cost center: %w", err)
		}
		out[costcenters.NormalizeCode(code)] = id
	}
	return out, rows.Err()
}

/* ------------------------------------------------------------- writing -- */

// SaveCostCenter inserts or updates one line and returns its id.
func (db *DB) SaveCostCenter(ctx context.Context, c CostCenter) (string, error) {
	if c.Code == "" {
		return "", fmt.Errorf("store: %w: a cost center needs a code", ErrInvalidInput)
	}
	if c.Name == "" {
		return "", fmt.Errorf("store: %w: a cost center needs a name", ErrInvalidInput)
	}
	if c.WarnAtPercent <= 0 || c.WarnAtPercent > 100 {
		return "", fmt.Errorf("store: %w: warn_at_percent must be between 1 and 100", ErrInvalidInput)
	}

	now := time.Now().UTC()
	if c.ID == "" {
		c.ID = NewID("cct")
		_, err := db.ExecContext(ctx, db.rebind(`
			INSERT INTO cost_centers (
				id, code, name, owner, description,
				plan_nanos, warn_at_percent, accepts_stamp, archived,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			c.ID, c.Code, c.Name, c.Owner, c.Description,
			nullInt64(c.PlanNanos), c.WarnAtPercent, c.AcceptsStamp, c.Archived, now, now)
		if err != nil {
			return "", fmt.Errorf("store: create cost center %q: %w", c.Code, err)
		}
		return c.ID, nil
	}

	res, err := db.ExecContext(ctx, db.rebind(`
		UPDATE cost_centers SET
			code = ?, name = ?, owner = ?, description = ?,
			plan_nanos = ?, warn_at_percent = ?, accepts_stamp = ?, archived = ?,
			updated_at = ?
		WHERE id = ?`),
		c.Code, c.Name, c.Owner, c.Description,
		nullInt64(c.PlanNanos), c.WarnAtPercent, c.AcceptsStamp, c.Archived, now, c.ID)
	if err != nil {
		return "", fmt.Errorf("store: update cost center %q: %w", c.Code, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return c.ID, nil
}

// ErrCostCenterInUse is returned when deleting a line would erase the
// allocation of spend already recorded against it.
//
// Archiving is the operation that exists for this: it hides the line and
// refuses new rules, and its existing rules keep matching, so a year of
// recorded spend stays where it was. Deleting is available once nothing has
// been billed here.
var ErrCostCenterInUse = errors.New("cost center has recorded spend")

// DeleteCostCenter removes a line, refusing one that has been billed to.
func (db *DB) DeleteCostCenter(ctx context.Context, id string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: delete cost center: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Both halves of the ladder: work items the rules placed here, and requests
	// stamped here directly. Either one means somebody's report has this line
	// on it.
	var used int
	err = tx.QueryRowContext(ctx, db.rebind(`
		SELECT
			(SELECT COUNT(*) FROM work_items WHERE cost_center_id = ?) +
			(SELECT COUNT(*) FROM requests   WHERE cost_center_id = ?)`),
		id, id).Scan(&used)
	if err != nil {
		return false, fmt.Errorf("store: check cost center %s: %w", id, err)
	}
	if used > 0 {
		return false, fmt.Errorf("store: %w", ErrCostCenterInUse)
	}

	res, err := tx.ExecContext(ctx, db.rebind(`DELETE FROM cost_centers WHERE id = ?`), id)
	if err != nil {
		return false, fmt.Errorf("store: delete cost center %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit cost center delete: %w", err)
	}
	return n > 0, nil
}

// SaveCostCenterRule writes one rule and restates every allocation it changes.
//
// The recompute runs in this transaction rather than after it. A rollup that
// read between the two would see a mapping half applied - some work items on
// the new rule and some on the old - and would report a number that never
// existed at any moment.
func (db *DB) SaveCostCenterRule(ctx context.Context, r costcenters.Rule) (string, error) {
	if !r.Match.Valid() {
		return "", fmt.Errorf("store: %w: unknown match %q", ErrInvalidInput, r.Match)
	}
	if err := costcenters.ValidatePattern(r.Pattern); err != nil {
		return "", fmt.Errorf("store: %w: %w", ErrInvalidInput, err)
	}
	if r.CostCenterID == "" {
		return "", fmt.Errorf("store: %w: a rule needs a cost center", ErrInvalidInput)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store: save cost center rule: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// An archived line takes no new rules. Its existing ones keep matching,
	// which is what stops archiving from moving recorded spend.
	var archived bool
	err = tx.QueryRowContext(ctx, db.rebind(
		`SELECT archived FROM cost_centers WHERE id = ?`), r.CostCenterID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read cost center %s: %w", r.CostCenterID, err)
	}
	if archived {
		return "", fmt.Errorf("store: %w: %s is archived", ErrInvalidInput, r.CostCenterID)
	}

	now := time.Now().UTC()
	if r.ID == "" {
		r.ID = NewID("ccr")
		_, err = tx.ExecContext(ctx, db.rebind(`
			INSERT INTO cost_center_rules
				(id, cost_center_id, position, match_on, pattern, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			r.ID, r.CostCenterID, r.Position, string(r.Match), r.Pattern, now, now)
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, db.rebind(`
			UPDATE cost_center_rules SET
				cost_center_id = ?, position = ?, match_on = ?, pattern = ?, updated_at = ?
			WHERE id = ?`),
			r.CostCenterID, r.Position, string(r.Match), r.Pattern, now, r.ID)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return "", ErrNotFound
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("store: write cost center rule %q: %w", r.Pattern, err)
	}

	if _, err := db.reallocate(ctx, tx); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit cost center rule: %w", err)
	}
	return r.ID, nil
}

// DeleteCostCenterRule removes a rule and restates what it was holding.
//
// The work items it claimed become unallocated unless a later rule takes them,
// which is the visible outcome: they reappear in the worklist rather than
// quietly moving somewhere plausible.
func (db *DB) DeleteCostCenterRule(ctx context.Context, id string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: delete cost center rule: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, db.rebind(
		`DELETE FROM cost_center_rules WHERE id = ?`), id)
	if err != nil {
		return false, fmt.Errorf("store: delete cost center rule %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}

	if _, err := db.reallocate(ctx, tx); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit cost center rule delete: %w", err)
	}
	return true, nil
}

// ReorderCostCenterRules writes a new evaluation order and restates every
// allocation it changes.
//
// The whole list in one request, like a team's roster: translating a drag into
// a stream of per-rule updates leaves a half-applied order on the first
// failure, and a half-applied order is an allocation nobody chose.
func (db *DB) ReorderCostCenterRules(ctx context.Context, ids []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: reorder cost center rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, db.rebind(
			`UPDATE cost_center_rules SET position = ?, updated_at = ? WHERE id = ?`),
			i, now, id); err != nil {
			return fmt.Errorf("store: reorder cost center rule %s: %w", id, err)
		}
	}

	if _, err := db.reallocate(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit cost center reorder: %w", err)
	}
	return nil
}

/* -------------------------------------------------------- reallocation -- */

// Reallocate rebuilds every work item's allocation from the current rules and
// reports how many moved.
//
// This is the whole answer to "can a past period be restated". Allocation lives
// on the work item, and a work item is permanent, so recomputing them restates
// every period that ever referenced them - there is no per-period state to
// migrate and no closed book to reopen. A rule corrected in November corrects
// October, which is the deliberate choice: a mis-assignment caught late was a
// mis-assignment all along, and a report that cannot be corrected is one nobody
// trusts.
//
// Called on its own by POST /api/v1/cost-centers/reallocate, and inside the
// transaction of every rule write.
func (db *DB) Reallocate(ctx context.Context) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: reallocate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	moved, err := db.reallocate(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit reallocation: %w", err)
	}
	return moved, nil
}

// reallocate is the pass itself, inside a caller's transaction.
//
// The matching happens in Go rather than in SQL, and deliberately: the glob is
// one implementation in the resolver, which is what the console's tester calls
// too. Two matchers - one here in each dialect's SQL, one in Go - would answer
// differently the first time somebody wrote a pattern with a slash in it, and
// the console would promise an allocation the recompute did not deliver.
//
// The read is the whole of work_items, which is bounded by distinct branches,
// pull requests and tickets rather than by traffic: a busy deployment has
// thousands of rows here and millions in `requests`, which is the reason the
// allocation lives on this side of the join.
func (db *DB) reallocate(ctx context.Context, tx *sql.Tx) (int, error) {
	rules, err := listRules(ctx, tx.QueryContext)
	if err != nil {
		return 0, err
	}
	resolver := make([]costcenters.Rule, len(rules))
	for i, r := range rules {
		resolver[i] = r.Rule
	}

	type change struct{ id, center, rule string }

	// The scan is its own scope so the cursor is closed before anything is
	// written: the reads have to finish before the updates start, and a
	// deferred Close in this function would run after them.
	changes, err := func() ([]change, error) {
		rows, qerr := tx.QueryContext(ctx, `
			SELECT id, kind, key, repo, cost_center_id, cost_center_rule_id
			FROM work_items`)
		if qerr != nil {
			return nil, fmt.Errorf("store: read work items for reallocation: %w", qerr)
		}
		defer func() { _ = rows.Close() }()

		var out []change
		for rows.Next() {
			var id, kind, key, repo string
			var center, rule sql.NullString
			if serr := rows.Scan(&id, &kind, &key, &repo, &center, &rule); serr != nil {
				return nil, fmt.Errorf("store: scan work item for reallocation: %w", serr)
			}

			var wantCenter, wantRule string
			if matched, ok := costcenters.Resolve(resolver, costcenters.Subject{
				Kind: kind, Key: key, Repo: repo,
			}); ok {
				wantCenter, wantRule = matched.CostCenterID, matched.ID
			}

			if center.String != wantCenter || rule.String != wantRule {
				out = append(out, change{id, wantCenter, wantRule})
			}
		}
		return out, rows.Err()
	}()
	if err != nil {
		return 0, err
	}

	for _, c := range changes {
		if _, err := tx.ExecContext(ctx, db.rebind(`
			UPDATE work_items SET cost_center_id = ?, cost_center_rule_id = ?
			WHERE id = ?`),
			nullString(c.center), nullString(c.rule), c.id); err != nil {
			return 0, fmt.Errorf("store: reallocate work item %s: %w", c.id, err)
		}
	}
	return len(changes), nil
}

// allocateWorkItem places one work item, inside the transaction that wrote it.
//
// Called from the attribution path so a repository seen for the first time is
// allocated immediately rather than at the next rule write - otherwise a new
// repository would sit unallocated until somebody happened to edit a rule, and
// would look like a gap in the rules when it was only a gap in timing.
// The subject is read back rather than passed in. upsertWorkItem fills blanks
// rather than overwriting, so the repository the caller observed is not
// necessarily the repository the row now holds, and allocating on the caller's
// copy would place a work item somewhere its own record disagrees with.
func (db *DB) allocateWorkItem(ctx context.Context, tx *sql.Tx, id string) error {
	var kind, key, repo string
	err := tx.QueryRowContext(ctx, db.rebind(
		`SELECT kind, key, repo FROM work_items WHERE id = ?`), id).Scan(&kind, &key, &repo)
	if err != nil {
		return fmt.Errorf("store: read work item %s for allocation: %w", id, err)
	}

	rules, err := listRules(ctx, tx.QueryContext)
	if err != nil {
		return err
	}
	resolver := make([]costcenters.Rule, len(rules))
	for i, r := range rules {
		resolver[i] = r.Rule
	}

	var center, rule string
	if matched, ok := costcenters.Resolve(resolver,
		costcenters.Subject{Kind: kind, Key: key, Repo: repo}); ok {
		center, rule = matched.CostCenterID, matched.ID
	}

	if _, err := tx.ExecContext(ctx, db.rebind(`
		UPDATE work_items SET cost_center_id = ?, cost_center_rule_id = ? WHERE id = ?`),
		nullString(center), nullString(rule), id); err != nil {
		return fmt.Errorf("store: allocate work item %s: %w", id, err)
	}
	return nil
}

/* ------------------------------------------------------------- helpers -- */

func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullString writes an empty id as SQL NULL, because the columns it feeds are
// foreign keys: an empty string is not a cost center anybody could look up, and
// the reference would be refused.
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
