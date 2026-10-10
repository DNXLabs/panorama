// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
)

// costCenterOf is the allocation ladder, as SQL, in the one place it is
// written.
//
// A stamp on the request beats the rule that placed its work item, and that
// COALESCE is the whole of the precedence. Every reader goes through this
// constant - the audit filter, the rollup, the gap breakdown and the export -
// because a ladder written twice is a filter that admits a row the total counts
// somewhere else, and nothing about that looks wrong from either side of the
// wire.
//
// A correlated subquery rather than a join, even in the queries that already
// join `work_items`, for exactly that reason: a second form of this expression
// would be a second derivation. Both hops are primary-key lookups over indexed
// columns.
const costCenterOf = `COALESCE(r.cost_center_id, (
	SELECT wi.cost_center_id
	FROM sessions si
	JOIN work_items wi ON wi.id = si.work_item_id
	WHERE si.id = r.session_id))`

// Why a request's spend reached no accounting line.
//
// A partition of the gap rather than a list of notes beside it: the costs sum
// to exactly the unallocated remainder, which is what stops the breakdown from
// becoming a second number to reconcile against the first.
const (
	// UnallocatedNoRule: the work is known and no rule claims it. The
	// actionable one, and the reason there is no fallback rung - this is a
	// worklist with a repository on it, not an accounting mystery.
	UnallocatedNoRule = "no_rule"
	// UnallocatedNoWorkItem: the conversation was never attributed to
	// anything, so there was nothing for a rule to match. This is the
	// attribution gap, reported here in the currency of allocation; /api/v1/work
	// breaks the same spend down by why it could not be attributed.
	UnallocatedNoWorkItem = "no_work_item"
)

// AllocationBucket is one accounting line's share of a window.
type AllocationBucket struct {
	// CostCenterID is empty for the unallocated remainder, which is a row here
	// like any other. It is never omitted and never folded into a total: an
	// honest 70% beats a false 100%.
	CostCenterID string
	Requests     int64
	Sessions     int64
	Cost         int64
	// Stamped is the share of Cost that a client claimed with a header rather
	// than a rule deriving from evidence. Reported separately because the two
	// carry different weight under challenge: a rule is what the gateway
	// observed, a stamp is what the spender asserted.
	Stamped int64
}

// AllocationSpend totals a window by accounting line, unallocated included.
func (db *DB) AllocationSpend(ctx context.Context, f RequestFilter) ([]AllocationBucket, error) {
	where, args := requestConditions(f)

	query := `
		SELECT COALESCE(` + costCenterOf + `, ''),
		       count(*),
		       count(DISTINCT r.session_id),
		       coalesce(sum(r.cost_nanos), 0),
		       coalesce(sum(CASE WHEN r.cost_center_id IS NOT NULL
		                         THEN r.cost_nanos ELSE 0 END), 0)
		FROM requests r` +
		whereClause(where) + `
		GROUP BY COALESCE(` + costCenterOf + `, '')
		ORDER BY 4 DESC`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: allocation spend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AllocationBucket
	for rows.Next() {
		var b AllocationBucket
		if err := rows.Scan(&b.CostCenterID, &b.Requests, &b.Sessions, &b.Cost, &b.Stamped); err != nil {
			return nil, fmt.Errorf("store: scan allocation bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UnallocatedWork is one reason a slice of the gap has no line, and - where
// there is one - the repository somebody would write a rule for.
type UnallocatedWork struct {
	Reason string
	// Repo is the repository the unmatched work belongs to, empty when none was
	// ever observed. It is what makes this a worklist rather than a figure: the
	// console offers "create a rule" against exactly this string.
	Repo     string
	Requests int64
	Sessions int64
	Cost     int64
}

// UnallocatedSpend breaks the gap down by why no rule reached it.
//
// The store reports what it observed and nothing more. "No rule claims this"
// is a fact about the rule table; whether that reads as an oversight or as a
// deliberate choice is an interpretation, and it belongs on the side of the
// wire that has to defend it.
func (db *DB) UnallocatedSpend(ctx context.Context, f RequestFilter) ([]UnallocatedWork, error) {
	where, args := requestConditions(f)
	where = append(where, costCenterOf+" IS NULL")

	// Two reasons, and they cannot overlap: either the session reached a work
	// item or it did not.
	reason := `CASE WHEN s.work_item_id IS NULL
	                THEN '` + UnallocatedNoWorkItem + `'
	                ELSE '` + UnallocatedNoRule + `' END`
	// The repository is only meaningful for the half somebody can act on, so
	// the other half groups under one empty key rather than fragmenting into
	// rows nobody can write a rule against.
	repo := `CASE WHEN s.work_item_id IS NULL THEN ''
	              ELSE coalesce(nullif(w.repo, ''), nullif(s.repo, ''), '') END`

	query := `
		SELECT ` + reason + `, ` + repo + `,
		       count(*),
		       count(DISTINCT r.session_id),
		       coalesce(sum(r.cost_nanos), 0)
		FROM requests r
		LEFT JOIN sessions s ON s.id = r.session_id
		LEFT JOIN work_items w ON w.id = s.work_item_id` +
		whereClause(where) + `
		GROUP BY ` + reason + `, ` + repo + `
		ORDER BY 5 DESC`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: unallocated spend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UnallocatedWork
	for rows.Next() {
		var u UnallocatedWork
		if err := rows.Scan(&u.Reason, &u.Repo, &u.Requests, &u.Sessions, &u.Cost); err != nil {
			return nil, fmt.Errorf("store: scan unallocated work: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RejectedStamp is a cost center code a client sent that named no line.
type RejectedStamp struct {
	Stamp    string
	Requests int64
	Cost     int64
}

// RejectedStamps reports every stamp that resolved to nothing in a window.
//
// Deliberately not part of the unallocated partition, and this is the subtle
// half. A request carrying a misspelt stamp whose work item a rule *does* claim
// is allocated - correctly, on evidence - and so never appears in the gap. A
// deployment could have every stamp in it wrong and a fully allocated report,
// with the only symptom being that nobody's override ever takes effect. So this
// is counted over the whole window rather than over the remainder.
func (db *DB) RejectedStamps(ctx context.Context, f RequestFilter) ([]RejectedStamp, error) {
	where, args := requestConditions(f)
	where = append(where, "r.cost_center_stamp <> ''")

	query := `
		SELECT r.cost_center_stamp, count(*), coalesce(sum(r.cost_nanos), 0)
		FROM requests r` +
		whereClause(where) + `
		GROUP BY r.cost_center_stamp
		ORDER BY 3 DESC`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: rejected stamps: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RejectedStamp
	for rows.Next() {
		var s RejectedStamp
		if err := rows.Scan(&s.Stamp, &s.Requests, &s.Cost); err != nil {
			return nil, fmt.Errorf("store: scan rejected stamp: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
