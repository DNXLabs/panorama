// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/pricing"
)

// WorkItemRecord is a work item as stored, including whatever a system of
// record has confirmed about it.
type WorkItemRecord struct {
	ID     string
	Kind   string
	Key    string
	Repo   string
	Branch string

	Title      string
	URL        string
	Status     string
	Author     string
	Project    string
	Source     string
	ExternalID string
	Outcome    string

	// FirstSeenAt and LastSeenAt are when Panorama saw this work in traffic,
	// which is a different question from when the work item was opened.
	FirstSeenAt time.Time
	LastSeenAt  time.Time

	CheckedAt   *time.Time
	ConfirmedAt *time.Time
	OpenedAt    *time.Time
	MergedAt    *time.Time
	ClosedAt    *time.Time
}

// Confirmed reports whether a system of record recognised this item.
func (w WorkItemRecord) Confirmed() bool { return w.ConfirmedAt != nil }

// Checked reports whether anything has been asked about it yet. Checked and
// unconfirmed is a real answer - a branch that was never pushed - and reads
// differently from never having asked.
func (w WorkItemRecord) Checked() bool { return w.CheckedAt != nil }

const workItemColumns = `
	w.id, w.kind, w.key, w.repo, w.branch,
	w.title, w.url, w.status, w.author, w.project, w.source, w.external_id, w.outcome,
	w.first_seen_at, w.last_seen_at,
	w.checked_at, w.confirmed_at, w.opened_at, w.merged_at, w.closed_at`

func scanWorkItem(scan func(...any) error) (WorkItemRecord, error) {
	var w WorkItemRecord
	err := scan(&w.ID, &w.Kind, &w.Key, &w.Repo, &w.Branch,
		&w.Title, &w.URL, &w.Status, &w.Author, &w.Project, &w.Source, &w.ExternalID, &w.Outcome,
		&w.FirstSeenAt, &w.LastSeenAt,
		&w.CheckedAt, &w.ConfirmedAt, &w.OpenedAt, &w.MergedAt, &w.ClosedAt)
	return w, err
}

// WorkItemsToConfirm lists what is worth asking a system of record about.
//
// Two kinds of row qualify, and nothing else does:
//
//   - work someone actually spent money on, which is the only work these
//     numbers have anything to say about, and
//   - the far end of a link, so that a ticket named by a confirmed pull request
//     gets a title even though no session was attributed to it directly.
//
// Never-checked rows come first, then the stalest. The ordering matters because
// this list is truncated: a deployment that has been running for a month has
// more work items than one sync should ask about, and the useful half of the
// answer is the half nobody has ever looked up.
func (db *DB) WorkItemsToConfirm(ctx context.Context, staleBefore time.Time, limit int) ([]integrations.Ref, error) {
	if limit <= 0 {
		limit = 200
	}

	// A NULL checked_at has to sort first on both dialects, and they disagree
	// about where NULLs land by default. Ordering on the expression rather than
	// on the column sidesteps the difference instead of relying on it.
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT w.kind, w.key, w.repo, w.branch
		FROM work_items w
		WHERE (w.checked_at IS NULL OR w.checked_at < ?)
		  AND (EXISTS (SELECT 1 FROM sessions s WHERE s.work_item_id = w.id)
		       OR EXISTS (SELECT 1 FROM work_item_links l WHERE l.to_id = w.id))
		ORDER BY CASE WHEN w.checked_at IS NULL THEN 0 ELSE 1 END,
		         w.checked_at,
		         w.last_seen_at DESC
		LIMIT ?`), staleBefore.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list work items to confirm: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []integrations.Ref
	for rows.Next() {
		var ref integrations.Ref
		if err := rows.Scan(&ref.Kind, &ref.Key, &ref.Repo, &ref.Branch); err != nil {
			return nil, fmt.Errorf("store: scan work item ref: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read work item refs: %w", err)
	}
	return out, nil
}

// ConfirmResult is what one pass of confirmation did, for the integrations
// screen and the log line.
type ConfirmResult struct {
	Checked   int
	Confirmed int
	Linked    int
}

// ConfirmWorkItems records the answer to one round of lookups.
//
// checked is every reference that was asked about; found is the subset a source
// recognised. Both are needed: marking only the found ones would leave the
// unrecognised ones permanently at the front of the queue, and the sync would
// ask about the same never-pushed branch forever.
//
// The whole round is one transaction. A partial application would leave some
// items checked and others not, and the next pass would silently ask a
// different question.
func (db *DB) ConfirmWorkItems(ctx context.Context, source string, checked []integrations.Ref, found []integrations.WorkItem, at time.Time) (ConfirmResult, error) {
	var result ConfirmResult
	at = at.UTC()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("store: confirm work items: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, ref := range checked {
		res, err := tx.ExecContext(ctx, db.rebind(
			`UPDATE work_items SET checked_at = ? WHERE kind = ? AND key = ?`),
			at, ref.Kind, ref.Key)
		if err != nil {
			return result, fmt.Errorf("store: mark work item checked: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			result.Checked += int(n)
		}
	}

	for _, item := range found {
		if err := item.Validate(); err != nil {
			// A source returning something malformed is a bug in the source, and
			// writing it would corrupt the one table the outcome numbers read.
			return result, fmt.Errorf("store: %w", err)
		}

		id, err := db.applyConfirmation(ctx, tx, source, item, at)
		if err != nil {
			return result, err
		}
		result.Confirmed++

		for _, link := range item.Links {
			linked, err := db.linkWorkItems(ctx, tx, id, item.Kind, link, source, at)
			if err != nil {
				return result, err
			}
			if linked {
				result.Linked++
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("store: commit confirmations: %w", err)
	}
	return result, nil
}

// applyConfirmation writes what a source said onto the work item, creating the
// row if the item is one Panorama had not seen in traffic.
//
// Text fields overwrite, because the source is authoritative for them and a
// renamed pull request should show its new title. first_seen_at does not,
// because it is Panorama's own observation and no integration knows it.
func (db *DB) applyConfirmation(ctx context.Context, tx *sql.Tx, source string, item integrations.WorkItem, at time.Time) (string, error) {
	_, err := tx.ExecContext(ctx, db.rebind(`
		INSERT INTO work_items (
			id, kind, key, repo, branch, title, url, status, author, project,
			source, external_id, outcome,
			first_seen_at, last_seen_at, checked_at, confirmed_at,
			opened_at, merged_at, closed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, key) DO UPDATE SET
			repo   = CASE WHEN excluded.repo   <> '' THEN excluded.repo   ELSE work_items.repo   END,
			branch = CASE WHEN excluded.branch <> '' THEN excluded.branch ELSE work_items.branch END,
			title  = CASE WHEN excluded.title  <> '' THEN excluded.title  ELSE work_items.title  END,
			url    = CASE WHEN excluded.url    <> '' THEN excluded.url    ELSE work_items.url    END,
			status = CASE WHEN excluded.status <> '' THEN excluded.status ELSE work_items.status END,
			author = CASE WHEN excluded.author <> '' THEN excluded.author ELSE work_items.author END,
			project = CASE WHEN excluded.project <> '' THEN excluded.project ELSE work_items.project END,
			external_id = CASE WHEN excluded.external_id <> '' THEN excluded.external_id ELSE work_items.external_id END,
			outcome = excluded.outcome,
			source  = excluded.source,
			checked_at   = excluded.checked_at,
			confirmed_at = excluded.confirmed_at,
			opened_at = coalesce(excluded.opened_at, work_items.opened_at),
			merged_at = coalesce(excluded.merged_at, work_items.merged_at),
			closed_at = coalesce(excluded.closed_at, work_items.closed_at)`),
		NewID("wrk"), item.Kind, item.Key, item.Repo, item.Branch,
		item.Title, item.URL, item.Status, item.Author, item.Project,
		source, item.ExternalID, string(item.Outcome),
		at, at, at, at,
		nullTime(item.CreatedAt), item.MergedAt, item.ClosedAt)
	if err != nil {
		return "", fmt.Errorf("store: confirm work item %s: %w", item.Key, err)
	}

	var id string
	err = tx.QueryRowContext(ctx, db.rebind(
		`SELECT id FROM work_items WHERE kind = ? AND key = ?`), item.Kind, item.Key).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: read confirmed work item: %w", err)
	}
	return id, nil
}

// linkWorkItems records that one work item names another, creating a
// placeholder row for the far end if traffic has never mentioned it.
//
// The placeholder carries the reporting source but no confirmed_at: GitHub
// naming PAY-1183 is evidence that PAY-1183 exists, not evidence of what Jira
// says about it. The next Jira pass fills it in.
func (db *DB) linkWorkItems(ctx context.Context, tx *sql.Tx, fromID, fromKind string, link integrations.Link, source string, at time.Time) (bool, error) {
	_, err := tx.ExecContext(ctx, db.rebind(`
		INSERT INTO work_items (id, kind, key, source, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, key) DO UPDATE SET last_seen_at = excluded.last_seen_at`),
		NewID("wrk"), link.Kind, link.Key, source, at, at)
	if err != nil {
		return false, fmt.Errorf("store: record linked work item %s: %w", link.Key, err)
	}

	var toID string
	err = tx.QueryRowContext(ctx, db.rebind(
		`SELECT id FROM work_items WHERE kind = ? AND key = ?`), link.Kind, link.Key).Scan(&toID)
	if err != nil {
		return false, fmt.Errorf("store: read linked work item: %w", err)
	}
	if toID == fromID {
		// A source naming itself. Harmless, and a self-link would make the
		// outcome join resolve an item through itself.
		return false, nil
	}

	_, err = tx.ExecContext(ctx, db.rebind(`
		INSERT INTO work_item_links (from_id, to_id, relation, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (from_id, to_id) DO UPDATE SET relation = excluded.relation`),
		fromID, toID, relationFor(fromKind, link.Kind), at)
	if err != nil {
		return false, fmt.Errorf("store: link work items: %w", err)
	}
	return true, nil
}

// relationFor names the relation from the two kinds, so the console does not
// have to re-derive it and word it differently.
func relationFor(from, to string) string {
	switch {
	case from == "branch" && to == "pull_request":
		return "opened_as"
	case from == "pull_request" && to == "ticket":
		return "implements"
	default:
		return "relates"
	}
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// IntegrationState is the health of one connected system.
type IntegrationState struct {
	Kind     string
	Provider string
	Detail   string
	Status   string

	LastError   string
	LastErrorAt *time.Time

	LastCheckedAt *time.Time
	LastSyncedAt  *time.Time

	LastCheckedItems   int64
	LastConfirmedItems int64

	UpdatedAt time.Time
}

// Integration status values.
const (
	IntegrationUnknown = "unknown"
	IntegrationOK      = "ok"
	IntegrationError   = "error"
)

// RegisterIntegration records that a source is configured, without claiming
// anything about whether it works.
//
// The row exists from startup so the console can show "connected, never run",
// which is a different thing from a screen that is empty because nothing is
// configured. Re-registering keeps the health fields: a restart is not evidence
// that the last failure is over.
func (db *DB) RegisterIntegration(ctx context.Context, kind, provider, detail string) error {
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO integrations (kind, provider, detail, status, created_at, updated_at)
		VALUES (?, ?, ?, 'unknown', ?, ?)
		ON CONFLICT (kind) DO UPDATE SET
			provider   = excluded.provider,
			detail     = excluded.detail,
			updated_at = excluded.updated_at`),
		kind, provider, detail, now, now)
	if err != nil {
		return fmt.Errorf("store: register integration %s: %w", kind, err)
	}
	return nil
}

// RecordIntegrationCheck records the result of a health check.
func (db *DB) RecordIntegrationCheck(ctx context.Context, kind string, checkErr error) error {
	now := time.Now().UTC()

	status, message := IntegrationOK, ""
	if checkErr != nil {
		status, message = IntegrationError, checkErr.Error()
	}

	// The previous error text survives a success, so an intermittent failure is
	// still readable once the next check passes. Only last_error_at moves.
	query := `
		UPDATE integrations
		SET status = ?, last_checked_at = ?, updated_at = ?`
	args := []any{status, now, now}
	if checkErr != nil {
		query += `, last_error = ?, last_error_at = ?`
		args = append(args, message, now)
	}
	query += ` WHERE kind = ?`
	args = append(args, kind)

	if _, err := db.ExecContext(ctx, db.rebind(query), args...); err != nil {
		return fmt.Errorf("store: record integration check: %w", err)
	}
	return nil
}

// RecordIntegrationSync records what a confirmation pass did.
func (db *DB) RecordIntegrationSync(ctx context.Context, kind string, result ConfirmResult) error {
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx, db.rebind(`
		UPDATE integrations
		SET last_synced_at = ?, last_checked_items = ?, last_confirmed_items = ?, updated_at = ?
		WHERE kind = ?`),
		now, result.Checked, result.Confirmed, now, kind)
	if err != nil {
		return fmt.Errorf("store: record integration sync: %w", err)
	}
	return nil
}

// ListIntegrations reads the state of every configured system.
func (db *DB) ListIntegrations(ctx context.Context) ([]IntegrationState, error) {
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT kind, provider, detail, status, last_error, last_error_at,
		       last_checked_at, last_synced_at, last_checked_items, last_confirmed_items,
		       updated_at
		FROM integrations
		ORDER BY kind`))
	if err != nil {
		return nil, fmt.Errorf("store: list integrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []IntegrationState
	for rows.Next() {
		var s IntegrationState
		if err := rows.Scan(&s.Kind, &s.Provider, &s.Detail, &s.Status,
			&s.LastError, &s.LastErrorAt, &s.LastCheckedAt, &s.LastSyncedAt,
			&s.LastCheckedItems, &s.LastConfirmedItems, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan integration: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read integrations: %w", err)
	}
	return out, nil
}

// outcomeJoin resolves the work item whose outcome a request's spend belongs
// to.
//
// A session is usually attributed to a branch, and a branch has no outcome of
// its own - it is the pull request it was opened as that merged or did not. So
// the resolution follows exactly one link, `opened_as`, and no further.
//
// One hop rather than a general graph walk is deliberate. Two hops would carry
// a branch's spend through its pull request to a ticket, and the ticket is not
// what "cost per merged pull request" is counting. The chain is still shown in
// full on the work item itself; it is only the outcome rollup that stops here.
const outcomeJoin = `
	FROM requests r
	LEFT JOIN sessions s ON s.id = r.session_id
	LEFT JOIN work_items w ON w.id = s.work_item_id
	LEFT JOIN work_item_links l ON l.from_id = w.id AND l.relation = 'opened_as'
	LEFT JOIN work_items pr ON pr.id = l.to_id`

// outcomeItem is the resolved item's column, given a column name.
func outcomeItem(column string) string {
	return `CASE WHEN w.kind = 'branch' AND pr.id IS NOT NULL THEN pr.` + column + ` ELSE coalesce(w.` + column + `, '') END`
}

// OutcomeBucket is spend grouped by what became of the work.
type OutcomeBucket struct {
	// Outcome is "", "open", "landed", or "abandoned". The empty one is work
	// nothing has confirmed, and it is a row rather than an omission for the
	// same reason unattributed spend is.
	Outcome   string
	WorkItems int64
	Totals
}

// OutcomeSummary answers what the money bought.
type OutcomeSummary struct {
	ByOutcome []OutcomeBucket

	// LandedPullRequests and LandedPullRequestCost are the two halves of cost
	// per merged pull request, returned separately so the caller divides them
	// rather than receiving a number it cannot check.
	//
	// The definition is stated because more than one is defensible: spend
	// recorded inside the window, attributed to pull requests that have landed,
	// over the number of such pull requests. A pull request merged inside the
	// window whose spend was incurred before it is therefore counted with only
	// the spend the window holds.
	LandedPullRequests    int64
	LandedPullRequestCost pricing.Money

	// Attributed reports whether any of this is meaningful yet. A deployment
	// with no integration connected gets every row in the empty outcome, and
	// the console must say why rather than showing a zero.
	Confirmed bool
}

// Outcomes rolls spend up by what became of the work it paid for.
func (db *DB) Outcomes(ctx context.Context, f RequestFilter) (*OutcomeSummary, error) {
	where, args := requestConditions(f)

	item := outcomeItem("outcome")
	query := `
		SELECT ` + item + `,
		       count(DISTINCT CASE WHEN w.id IS NULL THEN NULL
		                           WHEN w.kind = 'branch' AND pr.id IS NOT NULL THEN pr.id
		                           ELSE w.id END),` +
		aggregates + outcomeJoin + whereClause(where) + `
		GROUP BY ` + item + `
		ORDER BY ` + item

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: roll up outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	summary := &OutcomeSummary{}
	for rows.Next() {
		var b OutcomeBucket
		scan := func(dst ...any) error {
			return rows.Scan(append([]any{&b.Outcome, &b.WorkItems}, dst...)...)
		}
		bucket, serr := scanTotals(scan, nil)
		if serr != nil {
			return nil, fmt.Errorf("store: scan outcome bucket: %w", serr)
		}
		b.Totals = bucket.Totals
		if b.Outcome != "" {
			summary.Confirmed = true
		}
		summary.ByOutcome = append(summary.ByOutcome, b)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read outcome buckets: %w", rerr)
	}

	landedKind := `CASE WHEN w.kind = 'branch' AND pr.id IS NOT NULL THEN pr.kind ELSE coalesce(w.kind, '') END`
	landedID := `CASE WHEN w.kind = 'branch' AND pr.id IS NOT NULL THEN pr.id ELSE w.id END`

	prWhere := append(append([]string{}, where...),
		item+` = 'landed'`, landedKind+` = 'pull_request'`)

	err = db.QueryRowContext(ctx, db.rebind(`
		SELECT count(DISTINCT `+landedID+`), coalesce(sum(r.cost_nanos), 0)`+
		outcomeJoin+whereClause(prWhere)), args...).
		Scan(&summary.LandedPullRequests, &summary.LandedPullRequestCost)
	if err != nil {
		return nil, fmt.Errorf("store: cost per landed pull request: %w", err)
	}

	return summary, nil
}

// TeamSpend is one team's aggregate.
//
// A team is the smallest unit a leader is shown, which is the second trust
// stance expressed as a query: there is no by-user equivalent of this function,
// and adding one would be the change that turns analytics into a leaderboard.
type TeamSpend struct {
	TeamID  string
	Slug    string
	Name    string
	Members int64
	// AttributedCost is how much of this team's spend reached a work item.
	// Carried on the row rather than computed beside it, so a table showing
	// spend and coverage in two columns reads both from one query and cannot
	// show a share of a total it did not measure.
	AttributedCost int64
	Totals
}

// SummarizeTeams rolls spend up by team.
//
// A person on two teams contributes to both, so these rows can sum to more than
// the organization's total. That is the honest arithmetic - the alternative is
// picking one team per person and quietly under-reporting the other - and the
// caller is told when it applies so the screen can say so rather than leaving
// someone to notice the bars do not add up.
func (db *DB) SummarizeTeams(ctx context.Context, f RequestFilter) ([]TeamSpend, bool, error) {
	where, args := requestConditions(f)

	// The sessions join is a LEFT one and carries no condition, so it adds a
	// column without narrowing a row: a team whose traffic was never grouped
	// into a session still reports its spend, with nothing attributed.
	query := `
		SELECT t.id, t.slug, t.name,
		       (SELECT count(*) FROM team_members m2 WHERE m2.team_id = t.id),
		       coalesce(sum(CASE WHEN s.work_item_id IS NOT NULL THEN r.cost_nanos ELSE 0 END), 0),` +
		aggregates + `
		FROM requests r
		JOIN team_members m ON m.user_id = r.user_id
		JOIN teams t ON t.id = m.team_id
		LEFT JOIN sessions s ON s.id = r.session_id` +
		whereClause(where) + `
		GROUP BY t.id, t.slug, t.name
		ORDER BY t.name`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: roll up teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TeamSpend
	for rows.Next() {
		var s TeamSpend
		scan := func(dst ...any) error {
			return rows.Scan(append(
				[]any{&s.TeamID, &s.Slug, &s.Name, &s.Members, &s.AttributedCost}, dst...)...)
		}
		bucket, serr := scanTotals(scan, nil)
		if serr != nil {
			return nil, false, fmt.Errorf("store: scan team spend: %w", serr)
		}
		s.Totals = bucket.Totals
		out = append(out, s)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, false, fmt.Errorf("store: read team spend: %w", rerr)
	}

	var overlapping bool
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM team_members GROUP BY user_id HAVING count(*) > 1)`).
		Scan(&overlapping)
	if err != nil {
		return nil, false, fmt.Errorf("store: check overlapping teams: %w", err)
	}

	return out, overlapping, nil
}

// SummarizeProjects rolls spend up by the project a work item belongs to - a
// Jira project or epic, or the repository GitHub filed it under.
//
// It resolves through the same single link the outcome rollup does, and for the
// same reason: a branch has no project of its own, and the pull request it was
// opened as does. Grouping on the branch's own empty project would put real
// work in a "no project" bucket beside work that genuinely has none.
func (db *DB) SummarizeProjects(ctx context.Context, f RequestFilter) ([]WorkBucket, error) {
	where, args := requestConditions(f)

	project := outcomeItem("project")
	repo := outcomeItem("repo")

	query := `
		SELECT ` + project + `, '', ` + repo + `,
		       ` + attributedCost + `,
		       max(` + rungRank + `),
		       count(DISTINCT r.session_id),` +
		aggregates + outcomeJoin +
		whereClause(where) + `
		GROUP BY ` + project + `, ` + repo + `
		ORDER BY ` + project

	return db.workBuckets(ctx, query, args)
}

// WorkItemDetail is one work item with the chain it belongs to.
type WorkItemDetail struct {
	WorkItemRecord
	// Links are the items this one names, and the items that name it, so the
	// drill-in can show the whole chain from either end.
	Links    []WorkItemLink
	Sessions []Session
}

// WorkItemLink is one edge of the chain.
type WorkItemLink struct {
	WorkItemRecord
	Relation string
	// Direction is "to" for an item this one names and "from" for one that
	// names it.
	Direction string
}

// WorkItem reads one work item by its natural key.
func (db *DB) WorkItem(ctx context.Context, kind, key string) (*WorkItemRecord, error) {
	row := db.QueryRowContext(ctx, db.rebind(
		`SELECT`+workItemColumns+` FROM work_items w WHERE w.kind = ? AND w.key = ?`), kind, key)

	w, err := scanWorkItem(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: work item: %w", err)
	}
	return &w, nil
}

// WorkItemLinks reads the chain around one work item, in both directions.
func (db *DB) WorkItemLinks(ctx context.Context, id string) ([]WorkItemLink, error) {
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT`+workItemColumns+`, l.relation, 'to'
		FROM work_item_links l JOIN work_items w ON w.id = l.to_id
		WHERE l.from_id = ?
		UNION ALL
		SELECT`+workItemColumns+`, l.relation, 'from'
		FROM work_item_links l JOIN work_items w ON w.id = l.from_id
		WHERE l.to_id = ?`), id, id)
	if err != nil {
		return nil, fmt.Errorf("store: work item links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkItemLink
	for rows.Next() {
		var link WorkItemLink
		scan := func(dst ...any) error {
			return rows.Scan(append(dst, &link.Relation, &link.Direction)...)
		}
		rec, serr := scanWorkItem(scan)
		if serr != nil {
			return nil, fmt.Errorf("store: scan work item link: %w", serr)
		}
		link.WorkItemRecord = rec
		out = append(out, link)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read work item links: %w", rerr)
	}
	return out, nil
}
