// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// RequestGroup is one row of the audit trail: the requests one conversation
// made with one outcome, model calls and tool calls together, with no idle gap longer than
// GroupIdleGap between them. See groupOf for how a request joins one.
//
// Every figure is over the requests the filter admitted, not over the whole
// group. A group straddling the edge of the window reports the part inside
// it, and a model filter narrows a group to the requests that ran on that
// model, so a row's total and the requests it expands to are always the same
// set.
type RequestGroup struct {
	// ID is the id of the group's first request.
	ID string

	FirstAt time.Time
	LastAt  time.Time

	UserID    string
	UserEmail string
	SessionID string
	Tool      string
	// RouteLow and RouteHigh are the least and greatest route the group's
	// model calls named. A sitting may use more than one: Claude Code sends
	// background calls to its fast model, and an engineer may switch. Both
	// are empty for a group of tool calls alone.
	RouteLow  string
	RouteHigh string
	// Routes is how many routes the model calls named.
	Routes int64
	// LeadRoute and LeadModel are the route and model of the group's first
	// request, which opened the sitting: in a Claude Code conversation that is
	// the main model's turn rather than one of the fast model's background
	// calls, so it is the one a reader should see named first. Empty when the
	// group opened with a tool call.
	LeadRoute string
	LeadModel string

	Provider string
	// ToolCalls is how many of the group's requests were tool calls (a
	// search over MCP) rather than model calls.
	ToolCalls int64
	// ModelLow and ModelHigh are the least and greatest model that served the
	// group's requests. They differ only when the route was pointed at a
	// different model while the sitting was going on, which the console says
	// rather than choosing one of them.
	ModelLow  string
	ModelHigh string
	// Outcome is the class every request in the group belongs to.
	Outcome string

	// What the class is made of, because one class covers words that mean
	// opposite things to the reader: over a hard limit nothing runs, past a
	// soft one the engineer names another route.
	Refused     int64
	BlockedHard int64
	BlockedSoft int64
	// Warned counts served requests made past a soft limit.
	Warned int64
	// ErrorKind and StatusCode are the greatest of each, enough to say what
	// kind of failure a group of errors was; the expansion has every one.
	ErrorKind  string
	StatusCode int
	Attempts   int64
	DurationMS int64

	Repo        string
	Branch      string
	WorkItemKey string
	Rung        string

	Totals
}

// surfaceTool is the surface a tool call is recorded on. The data plane names
// it; the store only needs to tell a tool call from a model call.
const surfaceTool = "mcp"

// RequestGroupPage is one page of the grouped trail.
type RequestGroupPage struct {
	Groups []RequestGroup
	// NextCursor is empty at the end of the trail.
	NextCursor string
}

// outcomeExpr folds a request's decision into its class, in SQL, from the same
// table OutcomeOf reads. A decision no class names folds to NULL.
//
// The decisions are package constants and are written into the statement
// rather than bound: the expression is grouped by, and Postgres will not match
// a GROUP BY to a select list whose parameters are numbered differently.
func outcomeExpr() string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, outcome := range Outcomes {
		b.WriteString(" WHEN r.decision IN ('")
		b.WriteString(strings.Join(outcomeDecisions[outcome], "', '"))
		b.WriteString("') THEN '")
		b.WriteString(outcome)
		b.WriteString("'")
	}
	b.WriteString(" END")
	return b.String()
}

// ListRequestGroups reads the grouped trail, most recently active first.
//
// Grouped at read time over the filtered requests, not read from a table of
// groups, so that the filters mean what they mean on every other reading of
// the request log: requestConditions is applied to requests, and the group is
// whatever of it survived. Membership is what was decided at write time.
func (db *DB) ListRequestGroups(ctx context.Context, f RequestFilter) (*RequestGroupPage, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = defaultRequestPageSize
	case limit > maxRequestPageSize:
		limit = maxRequestPageSize
	}

	where, args := requestConditions(f)

	var having string
	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
		having = ` HAVING (max(r.started_at) < ? OR (max(r.started_at) = ? AND r.group_id < ?))`
		args = append(args, at, at, id)
	}
	args = append(args, limit+1)

	// Joined per request rather than once per group, so that the filters and
	// the aggregates share one FROM and `aggregates` is used as it is
	// everywhere else. The joins are one-to-one, and within a group the user
	// and the session are the same on every row, so max() reads the value.
	query := `
		SELECT r.group_id, min(r.started_at), max(r.started_at),
		       max(coalesce(r.user_id, '')), max(coalesce(u.email, '')),
		       max(r.session_id), max(r.tool),
		       coalesce(min(CASE WHEN r.surface <> '` + surfaceTool + `' THEN r.requested_model END), ''),
		       coalesce(max(CASE WHEN r.surface <> '` + surfaceTool + `' THEN r.requested_model END), ''),
		       count(DISTINCT CASE WHEN r.surface <> '` + surfaceTool + `' THEN r.requested_model END),
		       coalesce(max(CASE WHEN r.surface <> '` + surfaceTool + `' THEN r.provider END), max(r.provider)),
		       coalesce(sum(CASE WHEN r.surface = '` + surfaceTool + `' THEN 1 ELSE 0 END), 0),
		       coalesce((SELECT f.requested_model FROM requests f WHERE f.id = r.group_id AND f.surface <> '` + surfaceTool + `'), ''),
		       coalesce((SELECT f.resolved_model FROM requests f WHERE f.id = r.group_id AND f.surface <> '` + surfaceTool + `'), ''),
		       coalesce(min(CASE WHEN r.surface <> '` + surfaceTool + `' THEN r.resolved_model END), ''),
		       coalesce(max(CASE WHEN r.surface <> '` + surfaceTool + `' THEN r.resolved_model END), ''),
		       max(` + outcomeExpr() + `),
		       coalesce(sum(CASE WHEN r.decision = 'refused' THEN 1 ELSE 0 END), 0),
		       coalesce(sum(CASE WHEN r.decision = 'blocked' AND r.budget_state <> 'over_soft' THEN 1 ELSE 0 END), 0),
		       coalesce(sum(CASE WHEN r.decision = 'blocked' AND r.budget_state = 'over_soft' THEN 1 ELSE 0 END), 0),
		       coalesce(sum(CASE WHEN r.decision = 'allowed' AND r.budget_state = 'over_soft' THEN 1 ELSE 0 END), 0),
		       max(r.error_kind), max(r.status_code),
		       coalesce(sum(r.attempts), 0), coalesce(sum(r.duration_ms), 0),
		       max(coalesce(s.repo, '')), max(coalesce(s.branch, '')),
		       max(coalesce(w.key, '')), max(coalesce(s.rung, '')),` + aggregates + `
		FROM requests r
		LEFT JOIN users u ON u.id = r.user_id
		LEFT JOIN sessions s ON s.id = r.session_id
		LEFT JOIN work_items w ON w.id = s.work_item_id` +
		whereClause(where) + `
		GROUP BY r.group_id` + having + `
		ORDER BY max(r.started_at) DESC, r.group_id DESC
		LIMIT ?`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list request groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := &RequestGroupPage{}
	for rows.Next() {
		var (
			g       RequestGroup
			first   aggregateTime
			last    aggregateTime
			outcome sql.NullString
		)
		dst := []any{
			&g.ID, &first, &last,
			&g.UserID, &g.UserEmail,
			&g.SessionID, &g.Tool, &g.RouteLow, &g.RouteHigh, &g.Routes, &g.Provider, &g.ToolCalls, &g.LeadRoute, &g.LeadModel,
			&g.ModelLow, &g.ModelHigh,
			&outcome,
			&g.Refused, &g.BlockedHard, &g.BlockedSoft, &g.Warned,
			&g.ErrorKind, &g.StatusCode,
			&g.Attempts, &g.DurationMS,
			&g.Repo, &g.Branch, &g.WorkItemKey, &g.Rung,
		}
		b, serr := scanTotals(func(totals ...any) error { return rows.Scan(append(dst, totals...)...) }, nil)
		if serr != nil {
			return nil, fmt.Errorf("store: scan request group: %w", serr)
		}
		g.Totals = b.Totals
		g.FirstAt, g.LastAt = first.Time, last.Time
		g.Outcome = outcome.String
		page.Groups = append(page.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read request groups: %w", err)
	}

	if len(page.Groups) > limit {
		last := page.Groups[limit-1]
		page.Groups = page.Groups[:limit]
		page.NextCursor = encodeCursor(last.LastAt, last.ID)
	}
	return page, nil
}

// CountGroupOutcomes divides the window's groups by outcome, ignoring the
// filter's own outcome, as CountOutcomes does for requests.
//
// Folded in SQL rather than counted per decision and folded here: a group can
// hold both `blocked` and `refused` requests, and counting distinct groups per
// decision would count it twice under one class.
func (db *DB) CountGroupOutcomes(ctx context.Context, f RequestFilter) (RequestOutcomes, error) {
	f.Outcome = ""
	f.Cursor = ""
	where, args := requestConditions(f)

	query := `SELECT ` + outcomeExpr() + `, count(DISTINCT r.group_id) FROM requests r` +
		whereClause(where) + ` GROUP BY ` + outcomeExpr()

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return RequestOutcomes{}, fmt.Errorf("store: count group outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out RequestOutcomes
	for rows.Next() {
		var outcome sql.NullString
		var n int64
		if err := rows.Scan(&outcome, &n); err != nil {
			return RequestOutcomes{}, fmt.Errorf("store: scan group outcome count: %w", err)
		}
		switch outcome.String {
		case OutcomeRouted:
			out.Routed = n
		case OutcomeBlocked:
			out.Blocked = n
		case OutcomeError:
			out.Errored = n
		default:
			// A decision written by a newer build, left out of all four for
			// the reason CountOutcomes gives.
			continue
		}
	}
	if err := rows.Err(); err != nil {
		return RequestOutcomes{}, fmt.Errorf("store: read group outcome counts: %w", err)
	}
	out.All = out.Routed + out.Blocked + out.Errored
	return out, nil
}

// aggregateTime scans min() or max() of a timestamp column.
//
// An aggregate loses the column's declared type, so the SQLite driver hands
// back the stored text rather than a time. Postgres keeps the type. The SQLite
// driver writes a time as time.Time.String does, which is the first layout; the
// others are the ones it reads, in case a deployment's rows were written by
// something else. Anything unparseable is an error rather than a zero time,
// because a group dated 0001-01-01 would sort to the bottom of the trail and
// look like nothing happened.
type aggregateTime struct{ time.Time }

var aggregateTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999-07:00",
	time.RFC3339Nano,
}

func (t *aggregateTime) Scan(src any) error {
	var text string
	switch v := src.(type) {
	case time.Time:
		t.Time = v.UTC()
		return nil
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return fmt.Errorf("store: a timestamp aggregate came back as %T", src)
	}
	// time.Time.String appends the monotonic reading when there is one.
	if at := strings.Index(text, " m="); at >= 0 {
		text = text[:at]
	}
	for _, layout := range aggregateTimeLayouts {
		if at, err := time.Parse(layout, text); err == nil {
			t.Time = at.UTC()
			return nil
		}
	}
	return fmt.Errorf("store: %q is not a timestamp this store writes", text)
}
