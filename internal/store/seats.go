// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// UserSpend is one person's spend over a window.
//
// # Read this before reusing it
//
// Named per-person spend is visible to administrators and to managers of that
// person's teams, and to nobody else: an engineer must never see how they rank
// against the colleague at the next desk, and a budget holder who cannot see
// where the budget went cannot hold it (README, Principles).
//
// So it exists, and it is dangerous in exactly one way: nothing about the query
// knows who is asking. The entitlement is `Visibility`, decided on the server,
// and every caller passes one. Do not add a caller that skips it.
type UserSpend struct {
	UserID      string
	Email       string
	DisplayName string
	Role        string

	Totals Totals

	// Sessions is engagement rather than cost: "spent little across forty
	// conversations" and "spent little across one" are different findings.
	Sessions int64

	// AttributedCost is how much of this person's spend reached a work item.
	// The same column TeamSpend carries and for the same reason: a table that
	// shows spend and coverage side by side should read both from one query.
	//
	// It is not a performance measure and the console does not present it as
	// one. What it answers is whether this account's traffic is landing in the
	// attribution the deployment is being asked to trust - an engineer with no
	// coverage is usually one whose harness is configured differently, not one
	// who is working differently.
	AttributedCost int64
}

// Visibility is who a request may see named spend for.
//
// A value type rather than a role string, because "which teams do they lead" is
// part of the answer and a role alone cannot carry it. It is built once, on the
// server, from the principal - see controlplane.spendVisibility.
type Visibility struct {
	// Everyone is true for an administrator.
	Everyone bool
	// TeamIDs are the teams whose members may be seen. Empty with Everyone
	// false means nobody.
	TeamIDs []string
}

// SeesNobody reports whether this entitlement permits no named spend at all,
// which is what a developer holds.
func (v Visibility) SeesNobody() bool { return !v.Everyone && len(v.TeamIDs) == 0 }

// SpendByUser totals spend per named person, for those the caller may see.
//
// The entitlement is applied in the query rather than by filtering the result,
// so a person the caller may not see is never read out of the database at all.
// Filtering afterwards is the same answer until somebody adds a log line, a
// count, or a "showing 3 of 47" and leaks the shape of what was withheld.
func (db *DB) SpendByUser(ctx context.Context, f RequestFilter, v Visibility) ([]UserSpend, error) {
	if v.SeesNobody() {
		// Not an error and not an empty table: a developer asking this question
		// gets nothing, and the screen says why. Returning early also means the
		// query is never built for a caller who may not have it.
		return nil, nil
	}

	// The same conditions every other rollup uses, so a date range means the
	// same thing here as it does on the usage screen beside it.
	//
	// Deliberately no `attempts > 0`. That predicate belongs to budget
	// enforcement, where a blocked request must not count against the budget
	// that blocked it - Summarize does not apply it, so applying it here would
	// have Analytics and this screen report different request counts for the
	// same window, with nothing on either to explain the gap. Cost is
	// unaffected either way: a request that reached no provider has a null cost.
	where, args := requestConditions(f)
	where = append(where, "r.user_id IS NOT NULL")

	// A manager sees the people on the teams they lead, and nobody else. An IN
	// against a subquery rather than a join, so somebody on two of their teams
	// is one row rather than two.
	if !v.Everyone {
		placeholders := make([]string, len(v.TeamIDs))
		for i, id := range v.TeamIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = append(where, "r.user_id IN (SELECT tm.user_id FROM team_members tm WHERE tm.team_id IN ("+
			strings.Join(placeholders, ", ")+"))")
	}

	// Ordered by cost, which is what makes this a ranking - and is the point.
	// NULLS LAST is spelled per dialect because SQLite sorts NULL first and
	// Postgres last, and an unpriced row at the top of a spend table reads as
	// the most expensive person in the company.
	// No max(started_at) here, for the reason ListUsers records: an aggregate
	// loses the column's declared type, and the SQLite driver then hands back a
	// string that will not scan into a time. Last activity is a fact about a
	// person rather than about a window, and ListUsers already reads it the way
	// that works on both dialects - so the console joins the two rather than
	// this query computing it a second, more fragile way.
	// The sessions join is a LEFT one and adds no condition, so it widens the
	// row without narrowing the set: a person whose traffic was never grouped
	// into a session keeps their spend and reports nothing attributed.
	query := `
		SELECT u.id, u.email, u.display_name, u.role,
		       ` + spendAggregates + `,
		       count(DISTINCT r.session_id),
		       coalesce(sum(CASE WHEN s.work_item_id IS NOT NULL THEN r.cost_nanos ELSE 0 END), 0)
		FROM requests r
		JOIN users u ON u.id = r.user_id
		LEFT JOIN sessions s ON s.id = r.session_id` +
		whereClause(where) + `
		GROUP BY u.id, u.email, u.display_name, u.role
		ORDER BY coalesce(sum(r.cost_nanos), 0) DESC, u.email`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: spend by user: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UserSpend
	for rows.Next() {
		var (
			u    UserSpend
			cost int64
		)
		if err := rows.Scan(&u.UserID, &u.Email, &u.DisplayName, &u.Role,
			&cost, &u.Totals.Requests, &u.Totals.UnpricedBillable,
			&u.Sessions, &u.AttributedCost); err != nil {
			return nil, fmt.Errorf("store: scan user spend: %w", err)
		}
		u.Totals.Cost = pricing.Cost{Total: pricing.Money(cost)}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read user spend: %w", err)
	}
	return out, nil
}

// LeadTeams returns the teams a person leads, which is what decides whose
// numbers they may see.
func (db *DB) LeadTeams(ctx context.Context, userID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, db.rebind(
		`SELECT team_id FROM team_members WHERE user_id = ? AND role = ?`), userID, TeamRoleLead)
	if err != nil {
		return nil, fmt.Errorf("store: lead teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan lead team: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Seats is the answer to "what are we paying per engineer".
//
// Deliberately not a leaderboard: every figure here is about the population
// rather than about a person, so it is the one part of the analytics a
// developer could be shown without breaking anything. What it reports is
// whether the seats a company is paying for are being used, and how unevenly.
type Seats struct {
	// Provisioned is every active account. Active is the population that could
	// have spent, which is what makes "spent nothing" meaningful.
	Provisioned int64
	// Active is how many of them actually made a request in the window.
	Active int64

	// CostPerActive is total spend divided by Active, and is the headline. It
	// is zero when nobody was active, rather than a division by zero dressed up
	// as a number.
	CostPerActive pricing.Money

	// The distribution, so that one outlier is visible without naming them.
	// P50 and P90 are the spend of the median and ninetieth-percentile active
	// developer; Max is the highest.
	P50 pricing.Money
	P90 pricing.Money
	Max pricing.Money

	Total pricing.Money
}

// SeatEconomics computes the seat figures for a window.
//
// It reads per-user totals and then discards the identities, which is why it
// takes no Visibility: the result names nobody, so there is nobody to be
// entitled to. That is what lets the number appear on a screen a developer can
// open while the table beside it does not.
func (db *DB) SeatEconomics(ctx context.Context, f RequestFilter) (*Seats, error) {
	where, args := requestConditions(f)
	where = append(where, "r.user_id IS NOT NULL")

	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT coalesce(sum(r.cost_nanos), 0)
		FROM requests r`+whereClause(where)+`
		GROUP BY r.user_id
		ORDER BY 1`), args...)
	if err != nil {
		return nil, fmt.Errorf("store: seat economics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var spends []pricing.Money
	for rows.Next() {
		var cost int64
		if err := rows.Scan(&cost); err != nil {
			return nil, fmt.Errorf("store: scan seat spend: %w", err)
		}
		spends = append(spends, pricing.Money(cost))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read seat spend: %w", err)
	}

	var provisioned int64
	if err := db.QueryRowContext(ctx, db.rebind(
		`SELECT count(*) FROM users WHERE status = 'active'`)).Scan(&provisioned); err != nil {
		return nil, fmt.Errorf("store: count seats: %w", err)
	}

	seats := &Seats{Provisioned: provisioned, Active: int64(len(spends))}
	for _, s := range spends {
		seats.Total += s
	}
	if seats.Active > 0 {
		seats.CostPerActive = seats.Total / pricing.Money(seats.Active)
		seats.P50 = percentileOf(spends, 50)
		seats.P90 = percentileOf(spends, 90)
		seats.Max = spends[len(spends)-1]
	}
	return seats, nil
}

// percentileOf reads a percentile out of an ascending slice.
//
// Nearest-rank rather than interpolated: these are amounts somebody actually
// spent, and the ninetieth percentile of a team of eleven should be one of
// their real numbers rather than a weighted average of two of them.
func percentileOf(ascending []pricing.Money, p int) pricing.Money {
	if len(ascending) == 0 {
		return 0
	}
	rank := (p*len(ascending) + 99) / 100 // ceil(p/100 * n)
	if rank < 1 {
		rank = 1
	}
	if rank > len(ascending) {
		rank = len(ascending)
	}
	return ascending[rank-1]
}

// CacheHygiene is one tool or session running without prompt caching.
//
// The most legible number this product can put in front of somebody who signs
// the invoice. Coding agents re-send the whole conversation every turn, so a
// harness that never sets cache_control pays the full input rate for context it
// could have read back at a tenth of it - and the multiple is large, quiet, and
// invisible in a bill that only says "Bedrock".
type CacheHygiene struct {
	// Tool is the harness, as identified from the user agent.
	Tool string

	Requests int64
	// CacheReadTokens is what caching saved. Zero across a whole tool is the
	// flag: it means nothing in that harness is caching at all.
	CacheReadTokens int64
	InputTokens     int64

	Cost pricing.Money
}

// Uncached reports whether this tool read nothing from cache at all.
func (c CacheHygiene) Uncached() bool { return c.CacheReadTokens == 0 && c.Requests > 0 }

// CacheHygieneByTool reports how each harness is using the prompt cache.
//
// Grouped by tool rather than by session, because the finding is about a
// harness and its configuration rather than about a conversation - "Cline is
// uncached" is something an operator can act on, "this session was uncached" is
// something they cannot.
func (db *DB) CacheHygieneByTool(ctx context.Context, f RequestFilter) ([]CacheHygiene, error) {
	where, args := requestConditions(f)

	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT coalesce(nullif(r.tool, ''), 'unidentified'),
		       count(*),
		       coalesce(sum(r.cache_read_tokens), 0),
		       coalesce(sum(r.input_tokens), 0),
		       coalesce(sum(r.cost_nanos), 0)
		FROM requests r`+whereClause(where)+`
		GROUP BY coalesce(nullif(r.tool, ''), 'unidentified')
		ORDER BY 5 DESC`), args...)
	if err != nil {
		return nil, fmt.Errorf("store: cache hygiene: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CacheHygiene
	for rows.Next() {
		var (
			c    CacheHygiene
			cost int64
		)
		if err := rows.Scan(&c.Tool, &c.Requests, &c.CacheReadTokens, &c.InputTokens, &cost); err != nil {
			return nil, fmt.Errorf("store: scan cache hygiene: %w", err)
		}
		c.Cost = pricing.Money(cost)
		out = append(out, c)
	}
	return out, rows.Err()
}
