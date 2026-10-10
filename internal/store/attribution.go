// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
)

// Session is one conversation, with whatever it has been attributed to.
type Session struct {
	ID          string
	Fingerprint string
	Method      string
	UserID      string
	UserEmail   string
	Tool        string

	StartedAt  time.Time
	LastSeenAt time.Time
	Requests   int64

	Repo   string
	Branch string

	// WorkItemKey is empty for a session nothing could be attributed to, which
	// is a real and reportable state rather than a failure.
	WorkItemKey  string
	WorkItemKind string
	Rung         string
	Confidence   string
	Evidence     string

	// Cost is what the whole conversation cost, in nanodollars. A developer
	// looking at their own sessions is asking "what did that piece of work
	// cost me", and the answer is the conversation rather than the part of it
	// that happens to fall inside the window they are looking at.
	Cost int64
}

// Attributed reports whether this session is tied to a work item.
func (s Session) Attributed() bool { return s.WorkItemKey != "" }

// Describe renders the session's attribution the way the console shows it:
// "PAY-1183 - branch name (structural, high)".
func (s Session) Describe() string {
	if !s.Attributed() {
		return "unattributed"
	}
	if s.Evidence == "" {
		return s.WorkItemKey
	}
	return s.WorkItemKey + " - " + s.Evidence + " (" + s.Rung + ", " + s.Confidence + ")"
}

// Observation is what one request revealed about the conversation it belongs
// to.
type Observation struct {
	Fingerprint string
	Method      string
	UserID      string
	// ServiceTokenID is set instead of UserID when the turn came from a team's
	// service token. Exactly one of the two is ever populated, and a session
	// records whichever it was, so CI conversations are attributable to the
	// credential that ran them rather than to nobody.
	ServiceTokenID string
	Tool           string
	At             time.Time
	Signals        attribution.Signals
}

// AttributeSession records a turn against its conversation and updates what
// that conversation is working on.
//
// It runs off the request path, in the same detached goroutine that prices and
// records the request, because it writes to the database and nothing that
// writes to the database may sit between a model's token and the client.
//
// The attribution is only replaced by strictly better evidence - see
// attribution.Supersedes. A session that has been placed does not wander.
func (db *DB) AttributeSession(ctx context.Context, obs Observation) (*Session, error) {
	if obs.Fingerprint == "" {
		return nil, fmt.Errorf("store: %w: a session needs a fingerprint", ErrInvalidInput)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: attribute session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	session, err := db.upsertSession(ctx, tx, obs)
	if err != nil {
		return nil, err
	}

	candidate, found := attribution.Attribute(obs.Signals)
	current := attribution.Attribution{
		Kind: session.WorkItemKind,
		Key:  session.WorkItemKey,
		Rung: attribution.Rung(session.Rung),
	}

	if found && candidate.Supersedes(current) {
		if err := db.linkWork(ctx, tx, session, candidate, obs.At); err != nil {
			return nil, err
		}
		session.WorkItemKey = candidate.Key
		session.WorkItemKind = candidate.Kind
		session.Rung = string(candidate.Rung)
		session.Confidence = string(candidate.Confidence)
		session.Evidence = candidate.Evidence
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit attribution: %w", err)
	}
	return session, nil
}

// upsertSession records the turn and returns the conversation's current state.
func (db *DB) upsertSession(ctx context.Context, tx *sql.Tx, obs Observation) (*Session, error) {
	// The observed repo and branch fill in blanks rather than overwrite. A turn
	// whose body happened not to include the environment block must not erase
	// what an earlier turn established.
	_, err := tx.ExecContext(ctx, db.rebind(`
		INSERT INTO sessions (
			id, fingerprint, method, user_id, service_token_id, tool,
			started_at, last_seen_at, requests, repo, branch
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (fingerprint) DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			requests     = sessions.requests + 1,
			repo   = CASE WHEN sessions.repo   = '' THEN excluded.repo   ELSE sessions.repo   END,
			branch = CASE WHEN sessions.branch = '' THEN excluded.branch ELSE sessions.branch END`),
		NewID("ses"), obs.Fingerprint, obs.Method, nullIfEmpty(obs.UserID), obs.ServiceTokenID, obs.Tool,
		obs.At, obs.At, obs.Signals.RepoName(), obs.Signals.Branch.Value)
	if err != nil {
		return nil, fmt.Errorf("store: upsert session: %w", err)
	}

	row := tx.QueryRowContext(ctx, db.rebind(`
		SELECT s.id, s.fingerprint, s.method, coalesce(s.user_id, ''), s.tool,
		       s.started_at, s.last_seen_at, s.requests, s.repo, s.branch,
		       coalesce(w.key, ''), coalesce(w.kind, ''),
		       s.rung, s.confidence, coalesce(a.evidence, '')
		FROM sessions s
		LEFT JOIN work_items w ON w.id = s.work_item_id
		LEFT JOIN attributions a ON a.session_id = s.id
		WHERE s.fingerprint = ?`), obs.Fingerprint)

	var s Session
	err = row.Scan(&s.ID, &s.Fingerprint, &s.Method, &s.UserID, &s.Tool,
		&s.StartedAt, &s.LastSeenAt, &s.Requests, &s.Repo, &s.Branch,
		&s.WorkItemKey, &s.WorkItemKind, &s.Rung, &s.Confidence, &s.Evidence)
	if err != nil {
		return nil, fmt.Errorf("store: read session: %w", err)
	}
	return &s, nil
}

// linkWork upserts the work item and points the session at it.
func (db *DB) linkWork(ctx context.Context, tx *sql.Tx, s *Session, a attribution.Attribution, at time.Time) error {
	source := "inferred"
	if a.Rung == attribution.RungStamp {
		source = "stamp"
	}

	workItemID, err := db.upsertWorkItem(ctx, tx, a, source, at)
	if err != nil {
		return err
	}

	// Place it against the allocation rules in the same transaction that
	// created it. Waiting for the next rule write would leave a repository
	// seen for the first time sitting unallocated until somebody happened to
	// edit a rule - indistinguishable, on the worklist, from a repository no
	// rule covers, which is the one thing that list has to be right about.
	if aerr := db.allocateWorkItem(ctx, tx, workItemID); aerr != nil {
		return aerr
	}

	_, err = tx.ExecContext(ctx, db.rebind(`
		INSERT INTO attributions (
			id, session_id, work_item_id, rung, confidence, evidence, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET
			work_item_id = excluded.work_item_id,
			rung         = excluded.rung,
			confidence   = excluded.confidence,
			evidence     = excluded.evidence,
			updated_at   = excluded.updated_at`),
		NewID("att"), s.ID, workItemID, string(a.Rung), string(a.Confidence), a.Evidence, at, at)
	if err != nil {
		return fmt.Errorf("store: record attribution: %w", err)
	}

	// Denormalised onto the session so the usage rollups are one join deep.
	_, err = tx.ExecContext(ctx, db.rebind(`
		UPDATE sessions SET work_item_id = ?, rung = ?, confidence = ? WHERE id = ?`),
		workItemID, string(a.Rung), string(a.Confidence), s.ID)
	if err != nil {
		return fmt.Errorf("store: link session to work item: %w", err)
	}
	return nil
}

// upsertWorkItem inserts a work item or refreshes the one already there.
//
// Fields fill in blanks rather than overwrite, and the source is not downgraded:
// once an integration has confirmed a work item in M5, a later inferred sighting
// must not quietly demote it back to a guess.
func (db *DB) upsertWorkItem(ctx context.Context, tx *sql.Tx, a attribution.Attribution, source string, at time.Time) (string, error) {
	_, err := tx.ExecContext(ctx, db.rebind(`
		INSERT INTO work_items (id, kind, key, repo, branch, source, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, key) DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			repo   = CASE WHEN work_items.repo   = '' THEN excluded.repo   ELSE work_items.repo   END,
			branch = CASE WHEN work_items.branch = '' THEN excluded.branch ELSE work_items.branch END`),
		NewID("wrk"), a.Kind, a.Key, a.Repo, a.Branch, source, at, at)
	if err != nil {
		return "", fmt.Errorf("store: upsert work item: %w", err)
	}

	var id string
	err = tx.QueryRowContext(ctx, db.rebind(
		`SELECT id FROM work_items WHERE kind = ? AND key = ?`), a.Kind, a.Key).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: read work item: %w", err)
	}
	return id, nil
}

// SessionByFingerprint reads one conversation, or ErrNotFound.
func (db *DB) SessionByFingerprint(ctx context.Context, fingerprint string) (*Session, error) {
	row := db.QueryRowContext(ctx, db.rebind(sessionSelect+` WHERE s.fingerprint = ?`), fingerprint)

	s, err := scanSession(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: session by fingerprint: %w", err)
	}
	return &s, nil
}

// A conversation's cost, as a correlated subquery rather than a join.
//
// A GROUP BY over the join would have to group by every session column selected
// above, which on Postgres is a dozen names that must be kept in step with the
// SELECT list. `requests.session_id` is indexed and this list is bounded by a
// LIMIT, so the subquery is one index lookup per returned row.
//
// It is the whole conversation's cost rather than the window's. A session is a
// conversation, not a time slice: showing "$4.20" against one that started
// yesterday and cost $96 in total, because the window opened at midnight, is a
// figure nobody asked for. The window decides which sessions are listed.
const sessionCost = `(SELECT coalesce(sum(rq.cost_nanos), 0) FROM requests rq WHERE rq.session_id = s.id)`

const sessionSelect = `
	SELECT s.id, s.fingerprint, s.method, coalesce(s.user_id, ''), coalesce(u.email, ''), s.tool,
	       s.started_at, s.last_seen_at, s.requests, s.repo, s.branch,
	       coalesce(w.key, ''), coalesce(w.kind, ''),
	       s.rung, s.confidence, coalesce(a.evidence, ''),
	       ` + sessionCost + `
	FROM sessions s
	LEFT JOIN users u ON u.id = s.user_id
	LEFT JOIN work_items w ON w.id = s.work_item_id
	LEFT JOIN attributions a ON a.session_id = s.id`

func scanSession(scan func(...any) error) (Session, error) {
	var s Session
	err := scan(&s.ID, &s.Fingerprint, &s.Method, &s.UserID, &s.UserEmail, &s.Tool,
		&s.StartedAt, &s.LastSeenAt, &s.Requests, &s.Repo, &s.Branch,
		&s.WorkItemKey, &s.WorkItemKind, &s.Rung, &s.Confidence, &s.Evidence,
		&s.Cost)
	return s, err
}

// ListSessions reads recent conversations, newest activity first.
//
// This is where the product's promise to show its working is kept: every row
// carries the work item, the rung, the confidence, and the evidence, so
// "PAY-1183 - branch name (structural, high)" is a fact read out of the
// database rather than a claim made by a screen.
func (db *DB) ListSessions(ctx context.Context, f RequestFilter) ([]Session, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = defaultRequestPageSize
	case limit > maxRequestPageSize:
		limit = maxRequestPageSize
	}

	var (
		where []string
		args  []any
	)
	if f.UserID != "" {
		where = append(where, "s.user_id = ?")
		args = append(args, f.UserID)
	}
	// The typed filters are applied here too. A filter this function accepted
	// and ignored would be a screen quietly answering a different question from
	// the one it was asked; the select already joins `users`, and the repo and
	// branch are columns on the session itself.
	if f.User != "" {
		where = append(where, contains("u.email"))
		args = append(args, pattern(f.User))
	}
	if f.Repo != "" {
		where = append(where, contains("s.repo"))
		args = append(args, pattern(f.Repo))
	}
	if f.Branch != "" {
		where = append(where, contains("s.branch"))
		args = append(args, pattern(f.Branch))
	}
	if !f.Since.IsZero() {
		where = append(where, "s.last_seen_at >= ?")
		args = append(args, f.Since.UTC())
	}
	if !f.Until.IsZero() {
		where = append(where, "s.last_seen_at < ?")
		args = append(args, f.Until.UTC())
	}
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, db.rebind(
		sessionSelect+whereClause(where)+` ORDER BY s.last_seen_at DESC, s.id DESC LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Session
	for rows.Next() {
		s, serr := scanSession(rows.Scan)
		if serr != nil {
			return nil, fmt.Errorf("store: scan session: %w", serr)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read sessions: %w", err)
	}
	return out, nil
}

// WorkBucket is spend rolled up against one unit of work.
//
// The bucket whose Key is empty is unattributed spend. It is not filtered out
// anywhere, and every caller shows it: an honest seventy per cent beats a false
// hundred, and a rollup that quietly drops what it could not place is the false
// hundred.
type WorkBucket struct {
	Key  string
	Kind string
	Repo string
	// Rung is the best provenance any session contributing to this bucket had.
	Rung     string
	Sessions int64
	Totals

	// What a system of record said about this item, when one has been asked.
	// Empty on an inferred row, and empty is the honest answer rather than a
	// placeholder: a branch nobody has pushed has no title, and inventing one
	// from the branch name would present a guess in the column a reader goes to
	// for a fact. Only SummarizeWork fills these - a repository and a project
	// are groupings rather than items and have none of them.
	Title  string
	Status string
	// Outcome is '', 'open', 'landed' or 'abandoned' - the three the metrics
	// read, as against `Status`, which is whatever word the source used.
	Outcome string
	URL     string
	// LastSeenAt is when traffic last touched this item. Zero when nothing has.
	LastSeenAt time.Time

	// AttributedCost is how much of this bucket's spend reached a work item.
	// Equal to the total on a work-item row by definition; the column exists for
	// the repository and project rollups, where a group holds both attributed
	// and unattributed traffic and the share is the thing worth reading.
	AttributedCost int64

	// MergedPRs is how many merged pull requests this item is worth, resolved
	// through exactly one link - the same one hop the outcome rollup follows,
	// and for the same reason. A branch counts the pull request it was opened
	// as; a ticket counts the pull requests that implement it; a pull request
	// counts itself. Two hops would carry a branch's spend to a ticket, which
	// is not what cost per merged pull request counts.
	MergedPRs int64
}

// Attributed reports whether this bucket is real work rather than the
// unattributed remainder.
func (b WorkBucket) Attributed() bool { return b.Key != "" }

// Confirmed reports whether a system of record has been asked about this item.
func (b WorkBucket) Confirmed() bool { return b.Title != "" || b.Outcome != "" }

// rungRank scores provenance inside SQL so the best one in a group survives the
// aggregation. Lexicographic ordering would put "structural" above "stamp",
// which is exactly backwards.
const rungRank = `CASE s.rung WHEN 'stamp' THEN 3 WHEN 'structural' THEN 2 WHEN 'content' THEN 1 ELSE 0 END`

// attributedCost is the attributed half of a group's spend, and is the same
// predicate Coverage uses - so a repository's share and the deployment's share
// are the same arithmetic rather than two readings of it.
const attributedCost = `coalesce(sum(CASE WHEN s.work_item_id IS NOT NULL THEN r.cost_nanos ELSE 0 END), 0)`

// mergedPRs counts the merged pull requests one work item is worth.
//
// Three arms, one hop each, and they are disjoint by kind: a branch reaches its
// pull request forwards through `opened_as`, a ticket is reached backwards by
// the pull requests that `implements` it, and a pull request is itself. Nothing
// walks further - a branch's pull request's ticket is two hops, and counting it
// would file one merge under two items.
const mergedPRs = `(
	  (SELECT count(*) FROM work_item_links fl
	     JOIN work_items fp ON fp.id = fl.to_id
	    WHERE fl.from_id = w.id AND fl.relation = 'opened_as'
	      AND fp.kind = 'pull_request' AND fp.merged_at IS NOT NULL)
	+ (SELECT count(*) FROM work_item_links bl
	     JOIN work_items bp ON bp.id = bl.from_id
	    WHERE bl.to_id = w.id AND bl.relation = 'implements'
	      AND bp.kind = 'pull_request' AND bp.merged_at IS NOT NULL)
	+ (CASE WHEN w.kind = 'pull_request' AND w.merged_at IS NOT NULL THEN 1 ELSE 0 END)
	)`

// SummarizeWork rolls spend up by work item.
//
// It carries the item's own record - title, status, outcome, url, when it was
// last touched - because a table of work items that showed only keys sent a
// reader to a second screen to find out what `acme/panorama@feat/x` was, and
// those columns are already on the row this query joins. They are grouping
// columns rather than aggregates: one work item has exactly one title, so
// adding them changes nothing about how the spend is grouped.
func (db *DB) SummarizeWork(ctx context.Context, f RequestFilter) ([]WorkBucket, error) {
	where, args := requestConditions(f)

	// The item's own columns, listed once so the SELECT and the GROUP BY cannot
	// drift - a column in one and not the other is either a syntax error on
	// Postgres or, on SQLite, an arbitrary row's value silently chosen.
	//
	// `w.id` is in the group even though (kind, key) already identifies a row:
	// the merged-pull-request subqueries correlate on it, and a column a
	// subquery reads has to be grouped or Postgres refuses the statement.
	item := `w.id, coalesce(w.key, ''), coalesce(w.kind, ''), coalesce(w.repo, ''),
	         coalesce(w.title, ''), coalesce(w.status, ''), coalesce(w.outcome, ''),
	         coalesce(w.url, ''), w.last_seen_at, w.kind, w.merged_at`

	query := `
		SELECT ` + item + `,
		       ` + attributedCost + `,
		       coalesce(` + mergedPRs + `, 0),
		       max(` + rungRank + `),
		       count(DISTINCT r.session_id),` +
		aggregates + `
		FROM requests r
		LEFT JOIN sessions s ON s.id = r.session_id
		LEFT JOIN work_items w ON w.id = s.work_item_id` +
		whereClause(where) + `
		GROUP BY ` + item + `
		ORDER BY coalesce(w.key, '')`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: roll up work items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkBucket
	for rows.Next() {
		var (
			b         WorkBucket
			id        sql.NullString
			rank      int
			lastSeen  sql.NullTime
			groupKind sql.NullString
			mergedAt  sql.NullTime
		)
		scan := func(dst ...any) error {
			return rows.Scan(append([]any{
				&id, &b.Key, &b.Kind, &b.Repo, &b.Title, &b.Status, &b.Outcome, &b.URL, &lastSeen,
				&groupKind, &mergedAt,
				&b.AttributedCost, &b.MergedPRs,
				&rank, &b.Sessions,
			}, dst...)...)
		}
		bucket, serr := scanTotals(scan, nil)
		if serr != nil {
			return nil, fmt.Errorf("store: scan work bucket: %w", serr)
		}
		b.Totals = bucket.Totals
		b.Rung = rungFromRank(rank)
		if lastSeen.Valid {
			b.LastSeenAt = lastSeen.Time
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read work buckets: %w", err)
	}
	return out, nil
}

// SummarizeRepos rolls spend up by repository, which is the coarser view a
// leader asks for first.
func (db *DB) SummarizeRepos(ctx context.Context, f RequestFilter) ([]WorkBucket, error) {
	where, args := requestConditions(f)

	// The repository comes from the session rather than the work item, so that
	// a session with a known repository but no work item still lands under its
	// repository instead of in the unattributed bucket.
	query := `
		SELECT coalesce(s.repo, ''), '', coalesce(s.repo, ''),
		       ` + attributedCost + `,
		       max(` + rungRank + `),
		       count(DISTINCT r.session_id),` +
		aggregates + `
		FROM requests r
		LEFT JOIN sessions s ON s.id = r.session_id` +
		whereClause(where) + `
		GROUP BY coalesce(s.repo, '')
		ORDER BY coalesce(s.repo, '')`

	return db.workBuckets(ctx, query, args)
}

func (db *DB) workBuckets(ctx context.Context, query string, args []any) ([]WorkBucket, error) {
	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: roll up work: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WorkBucket
	for rows.Next() {
		var (
			b    WorkBucket
			rank int
		)
		scan := func(dst ...any) error {
			return rows.Scan(append(
				[]any{&b.Key, &b.Kind, &b.Repo, &b.AttributedCost, &rank, &b.Sessions}, dst...)...)
		}
		bucket, serr := scanTotals(scan, nil)
		if serr != nil {
			return nil, fmt.Errorf("store: scan work bucket: %w", serr)
		}
		b.Totals = bucket.Totals
		b.Rung = rungFromRank(rank)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read work buckets: %w", err)
	}
	return out, nil
}

func rungFromRank(rank int) string {
	switch rank {
	case 3:
		return "stamp"
	case 2:
		return "structural"
	case 1:
		return "content"
	default:
		return ""
	}
}

// AttributionCoverage is how much of a period's spend could be placed.
//
// It exists so the number is computed once, in SQL, and shown identically
// everywhere. A dashboard that lets each screen work out its own coverage is a
// dashboard whose screens disagree.
type AttributionCoverage struct {
	Requests           int64
	AttributedRequests int64
	Cost               int64
	AttributedCost     int64
	// UnpricedRequests is the other gap, carried alongside so that a coverage
	// figure is never mistaken for completeness in the other dimension.
	UnpricedRequests int64
}

// Coverage reports the attributed share of a period.
func (db *DB) Coverage(ctx context.Context, f RequestFilter) (*AttributionCoverage, error) {
	where, args := requestConditions(f)

	query := `
		SELECT count(*),
		       coalesce(sum(CASE WHEN s.work_item_id IS NOT NULL THEN 1 ELSE 0 END), 0),
		       coalesce(sum(r.cost_nanos), 0),
		       coalesce(sum(CASE WHEN s.work_item_id IS NOT NULL THEN r.cost_nanos ELSE 0 END), 0),
		       coalesce(sum(CASE WHEN r.cost_nanos IS NULL
		                          AND (r.input_tokens + r.output_tokens + r.cache_read_tokens + r.cache_write_tokens + r.web_searches) > 0
		                         THEN 1 ELSE 0 END), 0)
		FROM requests r
		LEFT JOIN sessions s ON s.id = r.session_id` + whereClause(where)

	var c AttributionCoverage
	err := db.QueryRowContext(ctx, db.rebind(query), args...).Scan(
		&c.Requests, &c.AttributedRequests, &c.Cost, &c.AttributedCost, &c.UnpricedRequests)
	if err != nil {
		return nil, fmt.Errorf("store: attribution coverage: %w", err)
	}
	return &c, nil
}

// Reasons a request's spend could not be placed against a work item.
//
// They are a partition of the unattributed remainder rather than a list of
// things that might be true of it: every unattributed request matches exactly
// one, and their costs sum to the coverage gap. That is the whole point - the
// product's claim is that the gap is stated rather than absorbed, and a
// breakdown whose parts overlap or fall short is a second number to reconcile
// against the first.
//
// The order is the ladder's, read downwards: the further a conversation got
// before the trail went cold, the closer its reason is to something an operator
// can act on. Nothing further up is inferred - a session with no repository is
// not assumed to be exploratory, it is one that revealed no repository, which
// is what the label says.
const (
	// UnplacedNoRepository: nothing in the conversation revealed a repository.
	// Exploratory work, a question about an error, a session that never
	// touched a checkout.
	UnplacedNoRepository = "no_repository"
	// UnplacedNoBranch: a repository was seen, no branch was. Work in a
	// checkout that is sitting on the default branch, most often.
	UnplacedNoBranch = "no_branch"
	// UnplacedNoWorkItem: a branch was seen and nothing could be made of it.
	// This is the one a naming convention fixes, and the only one of the three
	// where the deployment has already done everything it can.
	UnplacedNoWorkItem = "no_work_item"
)

// UnplacedSpend is one reason, with what it cost.
type UnplacedSpend struct {
	Reason   string
	Requests int64
	Sessions int64
	Cost     int64
}

// Unplaced breaks the unattributed remainder down by why it could not be
// placed.
//
// Read entirely from what the session recorded, and therefore honest about the
// only thing it could be dishonest about: these are not guesses at intent. A
// session with no repository is reported as having revealed no repository, not
// as "exploratory" - the console chooses that word, and it is the console's to
// defend.
//
// Requests that were never grouped into a session at all fall under
// UnplacedNoRepository, which is correct rather than convenient: a turn with no
// session revealed no repository either.
func (db *DB) Unplaced(ctx context.Context, f RequestFilter) ([]UnplacedSpend, error) {
	where, args := requestConditions(f)
	// The remainder, and only the remainder. Coverage's attributed half is the
	// complement of exactly this predicate, which is what makes the two add up.
	where = append(where, "s.work_item_id IS NULL")

	reason := `CASE
	             WHEN s.id IS NULL OR coalesce(s.repo, '') = '' THEN '` + UnplacedNoRepository + `'
	             WHEN coalesce(s.branch, '') = '' THEN '` + UnplacedNoBranch + `'
	             ELSE '` + UnplacedNoWorkItem + `'
	           END`

	query := `
		SELECT ` + reason + `,
		       count(*),
		       count(DISTINCT r.session_id),
		       coalesce(sum(r.cost_nanos), 0)
		FROM requests r
		LEFT JOIN sessions s ON s.id = r.session_id` +
		whereClause(where) + `
		GROUP BY ` + reason + `
		ORDER BY 4 DESC`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: unplaced spend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UnplacedSpend
	for rows.Next() {
		var u UnplacedSpend
		if err := rows.Scan(&u.Reason, &u.Requests, &u.Sessions, &u.Cost); err != nil {
			return nil, fmt.Errorf("store: scan unplaced spend: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read unplaced spend: %w", err)
	}
	return out, nil
}
