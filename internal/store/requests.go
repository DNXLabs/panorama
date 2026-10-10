// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// Decisions the gateway can record against a request.
const (
	DecisionAllowed = "allowed"
	DecisionRefused = "refused"
	// DecisionBlocked is a budget stopping a request, and it covers both ways
	// that happens: a hard limit, where nothing is served, and a soft limit
	// meeting a route that does not survive one, where the engineer can carry
	// on by naming another route.
	//
	// Not two values, because two columns already tell them apart -
	// `budget_state` is over_hard or over_soft, and `error_kind` carries the
	// policy reason. A third way of saying the same thing would be a third
	// thing to keep in step, and the first screen to read only one of them
	// would report a deployment working as designed as though it were an
	// outage.
	DecisionBlocked = "blocked"
	DecisionError   = "error"
)

// Outcome classes: what the audit trail's filter narrows by, and what its
// column states.
//
// Four decisions, three outcomes. The fold exists because the question a reader
// asks of the ledger is "was this served, refused, or did it fail", and that is
// not the same question as "which of the four words did the gateway record".
// `allowed` is served, under the outcome the console calls `routed`. `blocked`
// and `refused` both mean the provider was never invoked, one by a budget and
// one by the allowlist.
//
// ONE TABLE, BOTH DIRECTIONS. `outcomeDecisions` is what the filter turns into
// SQL and what a count is folded through, so a row the filter admits and a row
// the count includes can never be two different sets.
const (
	OutcomeRouted  = "routed"
	OutcomeBlocked = "blocked"
	OutcomeError   = "error"
)

var outcomeDecisions = map[string][]string{
	OutcomeRouted:  {DecisionAllowed},
	OutcomeBlocked: {DecisionBlocked, DecisionRefused},
	OutcomeError:   {DecisionError},
}

// Outcomes lists the classes in the order the console offers them.
var Outcomes = []string{OutcomeRouted, OutcomeBlocked, OutcomeError}

// OutcomeOf folds a recorded decision into the class the console filters by.
// A decision no class names returns empty, which is a row the counts would not
// add up without - so it is reported rather than silently binned.
func OutcomeOf(decision string) string {
	for outcome, decisions := range outcomeDecisions {
		for _, d := range decisions {
			if d == decision {
				return outcome
			}
		}
	}
	return ""
}

// ValidOutcome reports whether a filter value names a class.
func ValidOutcome(outcome string) bool {
	_, ok := outcomeDecisions[outcome]
	return ok
}

// RequestOutcomes is how the window divides, before the outcome filter is
// applied to it.
//
// Counted rather than derived from a page, because the page holds fifty rows of
// a window that holds weeks: a segmented control reporting "blocked 2" about
// what happened to be loaded, beside a filter that searches the whole window,
// is the same wrong answer the typed filters were moved to the server to avoid.
type RequestOutcomes struct {
	// All is the sum of the three, and is what "showing N of M" measures
	// against when no outcome is selected.
	All     int64
	Routed  int64
	Blocked int64
	Errored int64
}

// Count returns the count for one class, or All for anything else.
func (o RequestOutcomes) Count(outcome string) int64 {
	switch outcome {
	case OutcomeRouted:
		return o.Routed
	case OutcomeBlocked:
		return o.Blocked
	case OutcomeError:
		return o.Errored
	default:
		return o.All
	}
}

// CountOutcomes divides the window by outcome, ignoring the filter's own
// outcome so that every option in the console's control carries the count it
// would produce.
func (db *DB) CountOutcomes(ctx context.Context, f RequestFilter) (RequestOutcomes, error) {
	f.Outcome = ""
	f.Cursor = ""
	where, args := requestConditions(f)

	query := `SELECT r.decision, count(*) FROM requests r` + whereClause(where) + ` GROUP BY r.decision`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return RequestOutcomes{}, fmt.Errorf("store: count outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out RequestOutcomes
	for rows.Next() {
		var decision string
		var n int64
		if err := rows.Scan(&decision, &n); err != nil {
			return RequestOutcomes{}, fmt.Errorf("store: scan outcome count: %w", err)
		}
		switch OutcomeOf(decision) {
		case OutcomeRouted:
			out.Routed += n
		case OutcomeBlocked:
			out.Blocked += n
		case OutcomeError:
			out.Errored += n
		default:
			// A decision written by a newer build than this one is reading.
			// Counted into All alone would make the three stop summing to it,
			// so it is not counted at all and the discrepancy stays in the
			// table rather than in the control.
			continue
		}
	}
	if err := rows.Err(); err != nil {
		return RequestOutcomes{}, fmt.Errorf("store: read outcome counts: %w", err)
	}
	out.All = out.Routed + out.Blocked + out.Errored
	return out, nil
}

// defaultRequestPageSize is how many rows a request-log page holds when the
// caller does not say.
const defaultRequestPageSize = 50

// maxRequestPageSize caps a caller-supplied page size, so one console request
// cannot ask the database for the entire audit trail.
const maxRequestPageSize = 500

// RequestRecord is the metadata captured for one proxied request.
//
// There is no field here for a prompt or a response, and adding one is not a
// change that gets merged. The audit trail is metadata.
type RequestRecord struct {
	RequestID string

	UserID  string
	TokenID string
	// TeamID and ServiceTokenID are set only for a request a team's service
	// token made. A person's request leaves both empty and reaches its teams
	// through membership, so that moving somebody between teams moves their
	// history with them; a pipeline has no membership to reach through, so its
	// team is recorded on the row.
	TeamID         string
	ServiceTokenID string

	StartedAt time.Time
	Duration  time.Duration
	// TimeToFirstToken is zero for non-streaming requests and for requests that
	// failed before any token arrived.
	TimeToFirstToken time.Duration

	Surface  string
	Provider string

	RequestedModel string
	ResolvedModel  string

	Streaming  bool
	StatusCode int

	Decision  string
	ErrorKind string

	// BudgetState is how this request stood against the budgets governing it:
	// ok, over_soft, over_hard, or empty when no budget governs it.
	BudgetState string
	// Attempts is how many times a provider was actually invoked: zero or one.
	// Zero means the gateway answered on its own, and is the test for whether
	// this request counts against a budget.
	Attempts int

	// SessionFromSitting is set on a call that carries no conversation of its
	// own (a tool call over MCP): it takes the session sittingSession finds,
	// so it is grouped and attributed with the turns that asked for it.
	SessionFromSitting bool
	// SessionID is the conversation this request is a turn of, empty when none
	// could be identified. Attribution hangs off the session rather than off
	// this row, so that upgrading a session's attribution moves every one of its
	// requests with it.
	SessionID string

	// CostCenterID is the line a client named with x-panorama-cost-center, and
	// is empty on almost every request. It is the top rung of allocation: the
	// rollups read COALESCE(requests.cost_center_id, work_items.cost_center_id),
	// so a stamp overrides whatever the rules would have decided.
	//
	// Recorded here rather than on the work item because a work item is shared
	// - one repository is one row, across every person who touches it - so a
	// caller's override written there would move a colleague's spend to a line
	// they never named.
	CostCenterID string
	// CostCenterStamp is what a client sent that resolved to nothing: an
	// unknown code, or a line that refuses stamps. Kept verbatim because both
	// are invisible from the client's side - the request succeeded and the
	// money went somewhere else - and counted so they surface as a worklist
	// item rather than as a quarter-end surprise.
	CostCenterStamp string

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CacheWrite1hTokens is the portion of CacheWriteTokens written with a
	// one-hour TTL, which carries a higher rate.
	CacheWrite1hTokens int64

	// WebSearches is how many searches this request paid for. A search made
	// through the gateway's MCP tool is a row of its own with no tokens and
	// one search.
	WebSearches int64

	// Cost is nil when the request could not be priced, which is recorded as
	// null rather than zero. An unpriced request is countable and visible in
	// every total; a zero-cost one silently understates the bill.
	Cost *pricing.Cost
	// CostSource names where the rate came from, empty when unpriced.
	CostSource string

	Tool      string
	UserAgent string
	ClientIP  string
}

// Subject identifies the principal a request acted as, for grouping that must
// never merge two of them.
//
// A person is their user id. A service token is its own id, prefixed so it can
// never collide with one: session fingerprints mix this in so that two
// conversations opening identically are kept apart, and a pipeline with no user
// id would otherwise contribute an empty string - merging every CI job in the
// deployment into a single session.
func (r RequestRecord) Subject() string {
	if r.ServiceTokenID != "" {
		return "svc:" + r.ServiceTokenID
	}
	return r.UserID
}

// Usage returns the record's token accounting in the form the pricing engine
// takes.
func (r RequestRecord) Usage() pricing.Usage {
	return pricing.Usage{
		InputTokens:        r.InputTokens,
		OutputTokens:       r.OutputTokens,
		CacheReadTokens:    r.CacheReadTokens,
		CacheWriteTokens:   r.CacheWriteTokens,
		CacheWrite1hTokens: r.CacheWrite1hTokens,
	}
}

// RecordRequest writes one request's metadata.
func (db *DB) RecordRequest(ctx context.Context, r RequestRecord) error {
	// Null rather than zero when there was no first token: "not measured" and
	// "arrived instantly" are different facts and analytics must not average
	// them together.
	var ttft any
	if r.TimeToFirstToken > 0 {
		ttft = r.TimeToFirstToken.Microseconds()
	}

	// Same reasoning for cost: null is "we do not know what this cost", which
	// is a number the product is required to show honestly rather than hide.
	var total, input, output, cacheRead, cacheWrite, webSearch any
	if r.Cost != nil {
		total = int64(r.Cost.Total)
		input = int64(r.Cost.Input)
		output = int64(r.Cost.Output)
		cacheRead = int64(r.Cost.CacheRead)
		cacheWrite = int64(r.Cost.CacheWrite)
		webSearch = int64(r.Cost.WebSearch)
	}

	if r.SessionFromSitting && r.SessionID == "" {
		session, serr := db.sittingSession(ctx, r)
		if serr != nil {
			return serr
		}
		r.SessionID = session
	}

	id := NewID("req")
	group, err := db.groupOf(ctx, r)
	if err != nil {
		return err
	}
	if group == "" {
		group = id
	}

	_, err = db.ExecContext(ctx, db.rebind(`
		INSERT INTO requests (
			id, group_id, request_id, user_id, token_id,
			started_at, duration_ms, ttft_us,
			surface, provider, requested_model, resolved_model,
			streaming, status_code, decision, error_kind,
			budget_state, attempts, session_id,
			team_id, service_token_id,
			cost_center_id, cost_center_stamp,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			cache_write_1h_tokens, web_searches,
			cost_nanos, input_cost_nanos, output_cost_nanos,
			cache_read_cost_nanos, cache_write_cost_nanos, web_search_cost_nanos, cost_source,
			tool, user_agent, client_ip
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, group, r.RequestID, nullIfEmpty(r.UserID), nullIfEmpty(r.TokenID),
		r.StartedAt, r.Duration.Milliseconds(), ttft,
		r.Surface, r.Provider, r.RequestedModel, r.ResolvedModel,
		r.Streaming, r.StatusCode, r.Decision, r.ErrorKind,
		r.BudgetState, r.Attempts, r.SessionID,
		nullIfEmpty(r.TeamID), r.ServiceTokenID,
		nullIfEmpty(r.CostCenterID), r.CostCenterStamp,
		r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens,
		r.CacheWrite1hTokens, r.WebSearches,
		total, input, output, cacheRead, cacheWrite, webSearch, r.CostSource,
		r.Tool, r.UserAgent, r.ClientIP)
	if err != nil {
		return fmt.Errorf("store: record request: %w", err)
	}
	return nil
}

// GroupIdleGap is how long a group of requests may sit idle before the next
// request opens a new one.
//
// Thirty minutes is longer than anybody pauses between two prompts and shorter
// than a meeting or lunch, so one row of the trail reads as one sitting.
const GroupIdleGap = 30 * time.Minute

// groupOf finds the group a request joins, or returns empty when it opens one.
//
// A group is the requests that share a principal, a conversation, a tool and
// an outcome class, with no more than GroupIdleGap between one and the next.
// The conversation already fixes the repository, the branch and the work item,
// and the outcome is in the key so that a run of refusals never hides inside a
// served total.
//
// The route is not in it. Claude Code sends its background calls to its fast
// model on every turn, so a key with the route split one sitting into two rows,
// one of them small calls nobody chose; the trail shows the routes a group
// used instead, which keeps a deliberate switch visible.
//
// The request joins as it is written, so the trail's row for a sitting grows
// while the sitting is still going rather than appearing once it has ended.
//
// The window looks both ways because a request is recorded when it finishes
// and not when it starts: a long call that began at ten and ended at quarter
// to eleven is written after the calls that began in between, and belongs with
// them.
//
// Two requests opening the same group at the same instant on Postgres can each
// find nothing and open one apiece. That is two rows where there would have
// been one, with every request still in exactly one of them, and it is not
// worth a lock on the write that follows every model call.
func (db *DB) groupOf(ctx context.Context, r RequestRecord) (string, error) {
	decisions, ok := outcomeDecisions[OutcomeOf(r.Decision)]
	if !ok {
		// A decision no class names is a row of its own rather than one guessed
		// into a class: OutcomeOf's contract is that such a row is reported,
		// and on the trail a group is what gets reported.
		return "", nil
	}

	where := []string{
		"r.session_id = ?", "r.tool = ?", "r.service_token_id = ?",
		"r.started_at >= ?", "r.started_at <= ?",
		"r.decision IN (" + strings.TrimSuffix(strings.Repeat("?,", len(decisions)), ",") + ")",
	}
	args := []any{
		r.SessionID, r.Tool, r.ServiceTokenID,
		r.StartedAt.Add(-GroupIdleGap).UTC(), r.StartedAt.Add(GroupIdleGap).UTC(),
	}
	for _, d := range decisions {
		args = append(args, d)
	}
	if r.UserID == "" {
		where = append(where, "r.user_id IS NULL")
	} else {
		where = append(where, "r.user_id = ?")
		args = append(args, r.UserID)
	}

	var group string
	err := db.QueryRowContext(ctx, db.rebind(`SELECT r.group_id FROM requests r`+whereClause(where)+`
		ORDER BY r.started_at DESC, r.id DESC LIMIT 1`), args...).Scan(&group)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("store: find request group: %w", err)
	}
	return group, nil
}

// sittingSession is the conversation a call that carries none was most likely
// made from: the one its principal's same tool was last in, within
// GroupIdleGap before it. Only before: the call that asked for the tool
// started before the tool ran.
//
// An inference, and stated as one. A tool call over MCP carries no
// conversation id from any client this was checked against (Claude Code,
// opencode), only the tool's own user agent, so a search could otherwise only
// be a row of its own with no repository. Somebody running two conversations
// in the same tool at once can have a search land in the other one; the cost
// is a search's, and the alternative is every search unattributed.
func (db *DB) sittingSession(ctx context.Context, r RequestRecord) (string, error) {
	where := []string{
		"r.session_id <> ''", "r.tool = ?", "r.service_token_id = ?",
		"r.started_at >= ?", "r.started_at <= ?",
	}
	args := []any{r.Tool, r.ServiceTokenID, r.StartedAt.Add(-GroupIdleGap).UTC(), r.StartedAt.UTC()}
	if r.UserID == "" {
		where = append(where, "r.user_id IS NULL")
	} else {
		where = append(where, "r.user_id = ?")
		args = append(args, r.UserID)
	}
	var session string
	err := db.QueryRowContext(ctx, db.rebind(`SELECT r.session_id FROM requests r`+whereClause(where)+`
		ORDER BY r.started_at DESC, r.id DESC LIMIT 1`), args...).Scan(&session)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("store: find the sitting of a tool call: %w", err)
	}
	return session, nil
}

// CountRequests returns how many requests have been recorded.
func (db *DB) CountRequests(ctx context.Context) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM requests`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count requests: %w", err)
	}
	return n, nil
}

// LatestRequest returns the most recently started request, or ErrNotFound.
func (db *DB) LatestRequest(ctx context.Context) (*RequestRecord, error) {
	page, err := db.ListRequests(ctx, RequestFilter{Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(page.Requests) == 0 {
		return nil, ErrNotFound
	}
	rec := page.Requests[0].RequestRecord
	return &rec, nil
}

// RequestFilter narrows the request log.
//
// The user filter is an equality match: it is how a developer sees their own
// history and how an admin looks up a specific request. Grouping by named user
// is a separate function with its own entitlement check (`SpendByUser`), and
// this filter must never grow a "group by" of its own - one query that both
// narrows and groups is one an entitlement check can be forgotten on.
type RequestFilter struct {
	// UserID restricts to one user. Empty means every user, which only an
	// admin or manager is allowed to ask for.
	UserID string
	// User, Model, Repo and Branch are what a filter box holds: text somebody
	// typed, matched as a case-insensitive substring. Every one of them
	// narrows and none of them widens - they are ANDed with UserID, so a
	// developer who types a colleague's address still sees only their own
	// traffic.
	//
	// User matches the account's email. A request made by a service token has
	// no account, so it matches no user text at all, which is the honest answer
	// rather than a blank that would match everything.
	User string
	// Since and Until bound started_at; zero means unbounded.
	Since time.Time
	Until time.Time
	// Model matches resolved_model - what actually ran, not the route that
	// chose it. A substring rather than an exact id, because that is what a
	// filter box means, with one consequence worth stating: a bare
	// foundation-model id is contained in both its `us.` and its `global.`
	// profile id, so typing it matches both. Those are two catalogue entries
	// billing about 10% apart, and the model each row shows is what tells them
	// apart.
	Model string
	// Tool matches the coding tool exactly, e.g. claude-code. It comes from a
	// fixed list rather than from typing, so there is nothing to search.
	Tool string
	// Outcome narrows to one of the three classes in `outcomeDecisions`, and
	// is empty for all of them. Like the typed filters it only ever narrows,
	// and it is applied here rather than over a loaded page so that the count
	// beside each option, the table, its next page and the CSV export are all
	// answering about the same window.
	Outcome string
	// Repo and Branch match the session the request is a turn of. A request
	// belonging to no session has neither, and matches neither.
	Repo   string
	Branch string
	// CostCenter narrows to one accounting line, by id, through the same
	// expression the rollups group by. An id rather than typed text: a cost
	// center is chosen from a list the console already holds, and a substring
	// match on a code would quietly select CC-1000 for somebody who typed
	// CC-100.
	CostCenter string
	// Unallocated narrows to the remainder - the spend no rule and no stamp
	// reached. It is a separate flag rather than a sentinel value of
	// CostCenter, because "" already means "every line" and one field cannot
	// mean both without the day somebody forgets which.
	Unallocated bool
	// Group narrows to the requests of one group of the audit trail, by id.
	// It is how a group expands, and it narrows like everything else here, so
	// a group id is never a way into somebody else's traffic.
	Group string
	// Limit is the page size.
	Limit int
	// Cursor continues a previous page.
	Cursor string
}

// LoggedRequest is a request as read back out of the audit trail.
type LoggedRequest struct {
	ID string
	// GroupID is the audit trail group the request was written into.
	GroupID string
	RequestRecord
	// UserEmail is empty when the request was never authenticated or the
	// account has since been deleted.
	UserEmail string

	// What the conversation this request belongs to was working on, read
	// through its session at query time rather than copied onto the row.
	//
	// That is the whole reason `requests` carries a session id and no
	// attribution of its own: when a session's attribution is upgraded - a
	// ticket key appears, an integration confirms a branch - every request in
	// it moves with it, and a copy taken at write time would freeze the old
	// answer on every turn that came before the upgrade. Reading it here also
	// means the rows a repository filter returns and the repository each row
	// shows come from one query rather than from two that can disagree.
	Repo        string
	Branch      string
	WorkItemKey string
	Rung        string
	// CostCenterCode is the accounting line this request was allocated to,
	// empty when none reached it. Resolved through the same ladder the rollups
	// group by, in the query that returned the row, for the same reason Repo
	// is: a second, separately limited call would disagree with the filter.
	CostCenterCode string
	// CostCenterStamped says the line came from the client's own header rather
	// than from a rule. Shown because the two are answerable to different
	// standards: one is what the gateway observed, the other is what the
	// spender asserted.
	CostCenterStamped bool
}

// RequestPage is one page of the request log.
type RequestPage struct {
	Requests []LoggedRequest
	// NextCursor is empty at the end of the log.
	NextCursor string
}

// ListRequests reads the request log, newest first.
//
// Pagination is by keyset rather than OFFSET: the log grows continuously while
// someone is paging through it, and an offset would silently skip or repeat
// rows as new requests arrive at the top.
func (db *DB) ListRequests(ctx context.Context, f RequestFilter) (*RequestPage, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = defaultRequestPageSize
	case limit > maxRequestPageSize:
		limit = maxRequestPageSize
	}

	where, args := requestConditions(f)

	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, err
		}
		where = append(where, "(r.started_at < ? OR (r.started_at = ? AND r.id < ?))")
		args = append(args, at, at, id)
	}

	// One row more than asked for, to learn whether another page exists without
	// a second count query.
	args = append(args, limit+1)

	query := `
		SELECT r.id, r.group_id, r.request_id, coalesce(r.user_id, ''), coalesce(r.token_id, ''),
		       coalesce(u.email, ''),
		       r.started_at, r.duration_ms, r.ttft_us,
		       r.surface, r.provider, r.requested_model, r.resolved_model,
		       r.streaming, r.status_code, r.decision, r.error_kind,
		       r.budget_state, r.attempts, r.session_id,
		       r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens,
		       r.cache_write_1h_tokens, r.web_searches,
		       r.cost_nanos, r.input_cost_nanos, r.output_cost_nanos,
		       r.cache_read_cost_nanos, r.cache_write_cost_nanos, r.web_search_cost_nanos, r.cost_source,
		       r.tool, r.user_agent, r.client_ip,
		       coalesce(s.repo, ''), coalesce(s.branch, ''),
		       coalesce(w.key, ''), coalesce(s.rung, ''),
		       coalesce(cc.code, ''),
		       CASE WHEN r.cost_center_id IS NOT NULL THEN 1 ELSE 0 END
		FROM requests r
		LEFT JOIN users u ON u.id = r.user_id
		LEFT JOIN sessions s ON s.id = r.session_id
		LEFT JOIN work_items w ON w.id = s.work_item_id
		-- Joined through the ladder constant rather than through w.cost_center_id,
		-- even though w is right there. The two are provably the same here and
		-- would not stay that way: one of them is what every other reader uses.
		LEFT JOIN cost_centers cc ON cc.id = ` + costCenterOf +
		whereClause(where) + `
		ORDER BY r.started_at DESC, r.id DESC
		LIMIT ?`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list requests: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := &RequestPage{}
	for rows.Next() {
		lr, serr := scanLoggedRequest(rows)
		if serr != nil {
			return nil, serr
		}
		page.Requests = append(page.Requests, lr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read requests: %w", err)
	}

	if len(page.Requests) > limit {
		last := page.Requests[limit-1]
		page.Requests = page.Requests[:limit]
		page.NextCursor = encodeCursor(last.StartedAt, last.ID)
	}
	return page, nil
}

// scanLoggedRequest reads one row of the request log.
func scanLoggedRequest(rows *sql.Rows) (LoggedRequest, error) {
	var (
		lr         LoggedRequest
		durationMS int64
		ttftUS     sql.NullInt64
		total      sql.NullInt64
		input      sql.NullInt64
		output     sql.NullInt64
		cacheRead  sql.NullInt64
		cacheWrite sql.NullInt64
		webSearch  sql.NullInt64
	)

	err := rows.Scan(&lr.ID, &lr.GroupID, &lr.RequestID, &lr.UserID, &lr.TokenID, &lr.UserEmail,
		&lr.StartedAt, &durationMS, &ttftUS,
		&lr.Surface, &lr.Provider, &lr.RequestedModel, &lr.ResolvedModel,
		&lr.Streaming, &lr.StatusCode, &lr.Decision, &lr.ErrorKind,
		&lr.BudgetState, &lr.Attempts, &lr.SessionID,
		&lr.InputTokens, &lr.OutputTokens, &lr.CacheReadTokens, &lr.CacheWriteTokens,
		&lr.CacheWrite1hTokens, &lr.WebSearches,
		&total, &input, &output, &cacheRead, &cacheWrite, &webSearch, &lr.CostSource,
		&lr.Tool, &lr.UserAgent, &lr.ClientIP,
		&lr.Repo, &lr.Branch, &lr.WorkItemKey, &lr.Rung,
		&lr.CostCenterCode, &lr.CostCenterStamped)
	if err != nil {
		return LoggedRequest{}, fmt.Errorf("store: scan request: %w", err)
	}

	lr.Duration = time.Duration(durationMS) * time.Millisecond
	if ttftUS.Valid {
		lr.TimeToFirstToken = time.Duration(ttftUS.Int64) * time.Microsecond
	}
	if total.Valid {
		lr.Cost = &pricing.Cost{
			Input:      pricing.Money(input.Int64),
			Output:     pricing.Money(output.Int64),
			CacheRead:  pricing.Money(cacheRead.Int64),
			CacheWrite: pricing.Money(cacheWrite.Int64),
			WebSearch:  pricing.Money(webSearch.Int64),
			Total:      pricing.Money(total.Int64),
		}
	}
	return lr, nil
}

// Totals is aggregated usage over a set of requests.
//
// UnpricedRequests sits beside Cost on purpose, and every caller is expected to
// show both. A total that silently omits the requests it could not price looks
// authoritative and is wrong; the same stance the product takes toward
// unattributed spend applies to unpriced spend.
type Totals struct {
	Requests         int64
	ErrorRequests    int64
	UnpricedRequests int64
	// UnpricedBillable counts requests that used tokens but could not be
	// priced. It excludes the failures that never reached a model, which have
	// no cost to be missing.
	UnpricedBillable int64

	InputTokens        int64
	OutputTokens       int64
	CacheReadTokens    int64
	CacheWriteTokens   int64
	CacheWrite1hTokens int64
	// WebSearches is how many searches the requests paid for.
	WebSearches int64

	Cost pricing.Cost
}

// Bucket is one row of a grouped usage breakdown.
type Bucket struct {
	Key string
	Totals
}

// Summary is the usage overview the console renders.
type Summary struct {
	Totals  Totals
	ByModel []Bucket
	ByTool  []Bucket
	ByDay   []Bucket
}

// aggregates are the columns every usage query selects, so the totals and each
// breakdown are computed identically and cannot drift apart.
const aggregates = `
	count(*),
	coalesce(sum(CASE WHEN r.decision = 'error' THEN 1 ELSE 0 END), 0),
	coalesce(sum(CASE WHEN r.cost_nanos IS NULL THEN 1 ELSE 0 END), 0),
	coalesce(sum(CASE WHEN r.cost_nanos IS NULL
	                   AND (r.input_tokens + r.output_tokens + r.cache_read_tokens + r.cache_write_tokens + r.web_searches) > 0
	                  THEN 1 ELSE 0 END), 0),
	coalesce(sum(r.input_tokens), 0),
	coalesce(sum(r.output_tokens), 0),
	coalesce(sum(r.cache_read_tokens), 0),
	coalesce(sum(r.cache_write_tokens), 0),
	coalesce(sum(r.cache_write_1h_tokens), 0),
	coalesce(sum(r.cost_nanos), 0),
	coalesce(sum(r.input_cost_nanos), 0),
	coalesce(sum(r.output_cost_nanos), 0),
	coalesce(sum(r.cache_read_cost_nanos), 0),
	coalesce(sum(r.cache_write_cost_nanos), 0),
	coalesce(sum(r.web_searches), 0),
	coalesce(sum(r.web_search_cost_nanos), 0)`

// scanTotals reads the aggregate columns in the order they are selected above.
func scanTotals(scan func(...any) error, key *string) (Bucket, error) {
	var b Bucket
	dst := []any{
		&b.Requests, &b.ErrorRequests, &b.UnpricedRequests, &b.UnpricedBillable,
		&b.InputTokens, &b.OutputTokens, &b.CacheReadTokens, &b.CacheWriteTokens,
		&b.CacheWrite1hTokens,
		&b.Cost.Total, &b.Cost.Input, &b.Cost.Output, &b.Cost.CacheRead, &b.Cost.CacheWrite,
		&b.WebSearches, &b.Cost.WebSearch,
	}
	if key != nil {
		dst = append([]any{key}, dst...)
	}
	if err := scan(dst...); err != nil {
		return Bucket{}, err
	}
	if key != nil {
		b.Key = *key
	}
	return b, nil
}

// Summarize aggregates usage over the filtered requests.
func (db *DB) Summarize(ctx context.Context, f RequestFilter) (*Summary, error) {
	where, args := requestConditions(f)
	clause := whereClause(where)

	var (
		summary Summary
		err     error
	)

	row := db.QueryRowContext(ctx, db.rebind(`SELECT`+aggregates+` FROM requests r`+clause), args...)
	total, err := scanTotals(row.Scan, nil)
	if err != nil {
		return nil, fmt.Errorf("store: summarize requests: %w", err)
	}
	summary.Totals = total.Totals

	// By provider and model together, written as the reference a route uses.
	// The same id on two providers is two targets that bill apart, and a
	// breakdown that summed them would be a line nobody could act on. A request
	// that reached no model keeps the empty key it always had.
	if summary.ByModel, err = db.bucketBy(ctx, modelRefExpr, clause, args); err != nil {
		return nil, err
	}
	if summary.ByTool, err = db.bucketBy(ctx, "r.tool", clause, args); err != nil {
		return nil, err
	}
	if summary.ByDay, err = db.bucketBy(ctx, db.dayExpr(), clause, args); err != nil {
		return nil, err
	}
	return &summary, nil
}

// modelRefExpr is a request's model as provider/model, or empty for a request
// that never reached one.
const modelRefExpr = `CASE WHEN r.resolved_model = '' THEN '' ELSE r.provider || '/' || r.resolved_model END`

// bucketBy runs the shared aggregate grouped by one expression.
func (db *DB) bucketBy(ctx context.Context, groupExpr, clause string, args []any) ([]Bucket, error) {
	// groupExpr is one of this package's own literals, never caller input.
	query := `SELECT ` + groupExpr + `,` + aggregates + `
		FROM requests r` + clause + `
		GROUP BY ` + groupExpr + `
		ORDER BY ` + groupExpr

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: group usage by %s: %w", groupExpr, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Bucket
	for rows.Next() {
		var key string
		b, serr := scanTotals(rows.Scan, &key)
		if serr != nil {
			return nil, fmt.Errorf("store: scan usage bucket: %w", serr)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read usage buckets: %w", err)
	}
	return out, nil
}

// dayExpr is the one place the two dialects genuinely diverge: extracting a
// calendar day out of a timestamp has no portable spelling.
func (db *DB) dayExpr() string {
	if db.IsSQLite() {
		// Timestamps are stored as RFC 3339 text, so the date is the first ten
		// characters.
		return "substr(r.started_at, 1, 10)"
	}
	return "to_char(r.started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
}

// requestConditions turns a filter into SQL conditions and their arguments.
func requestConditions(f RequestFilter) (where []string, args []any) {
	if f.UserID != "" {
		where = append(where, "r.user_id = ?")
		args = append(args, f.UserID)
	}
	if !f.Since.IsZero() {
		where = append(where, "r.started_at >= ?")
		args = append(args, f.Since.UTC())
	}
	if !f.Until.IsZero() {
		where = append(where, "r.started_at < ?")
		args = append(args, f.Until.UTC())
	}
	if f.User != "" {
		// A subquery rather than a join, because these conditions are shared by
		// every query over the request log and only some of them join `users`.
		// One derivation of a filter is the point: a second place to decide
		// what a typed name matches is a second place for it to mean something
		// slightly different.
		where = append(where, "r.user_id IN (SELECT id FROM users WHERE "+contains("email")+")")
		args = append(args, pattern(f.User))
	}
	if f.Model != "" {
		where = append(where, contains("r.resolved_model"))
		args = append(args, pattern(f.Model))
	}
	if f.Tool != "" {
		where = append(where, "r.tool = ?")
		args = append(args, f.Tool)
	}
	if decisions, ok := outcomeDecisions[f.Outcome]; ok {
		// Expanded into placeholders rather than interpolated, even though
		// every value is a constant of this package: a query built by
		// concatenation is one somebody widens later without noticing what it
		// was.
		where = append(where, "r.decision IN ("+strings.TrimSuffix(strings.Repeat("?,", len(decisions)), ",")+")")
		for _, d := range decisions {
			args = append(args, d)
		}
	}
	if f.CostCenter != "" {
		// Through the same expression the rollups group by, so an option that
		// says 4 cannot return 3 rows. It narrows like every other filter here
		// - ANDed with the scope that filterFrom pinned first - so it is never
		// a way to reach somebody else's traffic.
		where = append(where, costCenterOf+" = ?")
		args = append(args, f.CostCenter)
	}
	if f.Unallocated {
		where = append(where, costCenterOf+" IS NULL")
	}
	if f.Group != "" {
		where = append(where, "r.group_id = ?")
		args = append(args, f.Group)
	}
	if f.Repo != "" {
		where = append(where, "r.session_id IN (SELECT id FROM sessions WHERE "+contains("repo")+")")
		args = append(args, pattern(f.Repo))
	}
	if f.Branch != "" {
		where = append(where, "r.session_id IN (SELECT id FROM sessions WHERE "+contains("branch")+")")
		args = append(args, pattern(f.Branch))
	}
	return where, args
}

// contains renders a case-insensitive substring match on one column: what a
// filter box someone types into means.
//
// The column is folded here and the typed text is folded by pattern, rather
// than reaching for ILIKE or a column collation, because one statement runs on
// both SQLite and Postgres and LIKE is case-sensitive on exactly one of them.
// SQLite's lower() folds ASCII only, so a filter is case-blind over the
// alphabet these values are actually written in and case-sensitive outside it -
// which is a worse match, never a wrong row.
//
// The escape clause is what keeps a branch called `feat/50%-done` a search for
// itself rather than a search for everything.
func contains(column string) string {
	return `lower(` + column + `) LIKE ? ESCAPE '\'`
}

// likeEscaper neutralises the two wildcards and the escape character itself,
// so nothing a person types is read as a pattern.
var likeEscaper = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)

// pattern turns typed text into the LIKE argument contains expects.
func pattern(v string) string {
	return "%" + likeEscaper.Replace(strings.ToLower(v)) + "%"
}

func whereClause(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

// encodeCursor packs the sort key of the last row on a page.
func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + " " + id))
}

// decodeCursor unpacks a cursor, rejecting anything malformed rather than
// silently restarting from the top of the log.
func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: %w: not a cursor", ErrInvalidInput)
	}
	stamp, id, found := strings.Cut(string(raw), " ")
	if !found || id == "" {
		return time.Time{}, "", fmt.Errorf("store: %w: malformed cursor", ErrInvalidInput)
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: %w: cursor has no timestamp", ErrInvalidInput)
	}
	return at, id, nil
}

// nullIfEmpty keeps foreign keys null rather than storing an empty string that
// would violate the reference.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
