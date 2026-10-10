-- SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
-- SPDX-License-Identifier: AGPL-3.0-only

-- Panorama's schema, Postgres dialect.
--
-- One baseline, kept in step with migrations/sqlite/00001_initial.sql: the
-- two dialects hold the same tables, columns, constraints and indexes, and
-- differ only where SQLite and Postgres spell a thing differently. A change
-- to the schema is a new numbered migration in both directories, never an
-- edit to this file.
--
-- Money is BIGINT nanodollars, never a float, so a column of costs sums to
-- the same figure in any order. Case-insensitive uniqueness is a unique index
-- on lower(), where SQLite says COLLATE NOCASE.

-- +goose Up

-- People. Panorama does not own identities: a row is created by the first
-- SSO sign-in (or by `panorama token create`), and the identity provider stays
-- the record of who somebody is.
CREATE TABLE users (
    id           TEXT PRIMARY KEY,
    -- Subject claim from the identity provider. Null until the user first
    -- signs in through SSO, so an admin can pre-create accounts.
    external_id  TEXT,
    email        TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    -- Roles are an allowlist. Group-to-role mapping happens at sign-in.
    role         TEXT NOT NULL DEFAULT 'developer'
                 CHECK (role IN ('admin', 'manager', 'developer')),
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'disabled')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Who decided the role. 'sso' is recomputed from group membership at every
    -- sign-in, which is how removing somebody from a group removes their
    -- access. 'manual' is a role an administrator pinned, and the mapping
    -- leaves it alone until somebody hands the account back: otherwise the
    -- account that owns a fresh deployment would be demoted by its own second
    -- sign-in.
    role_source  TEXT NOT NULL DEFAULT 'sso'
                 CHECK (role_source IN ('sso', 'manual'))
);

CREATE UNIQUE INDEX users_email_key ON users (lower(email));
CREATE UNIQUE INDEX users_external_id_key ON users (external_id) WHERE external_id IS NOT NULL;

CREATE TABLE teams (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX teams_slug_key ON teams (slug);

CREATE TABLE team_members (
    team_id    TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member'
               CHECK (role IN ('lead', 'member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (team_id, user_id)
);

CREATE INDEX team_members_user_idx ON team_members (user_id);

-- Deployment-wide switches and facts an administrator sets from a screen.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- What administrators and the system did. Never a request or response body.
CREATE TABLE audit_events (
    id            TEXT PRIMARY KEY,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_type    TEXT NOT NULL
                  CHECK (actor_type IN ('user', 'token', 'system')),
    -- Null for system actors, and set null rather than cascaded when a user is
    -- deleted: the trail must outlive the account.
    actor_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
    action        TEXT NOT NULL,
    target_type   TEXT NOT NULL DEFAULT '',
    target_id     TEXT NOT NULL DEFAULT '',
    source_ip     TEXT NOT NULL DEFAULT '',
    user_agent    TEXT NOT NULL DEFAULT '',
    -- JSON object of action-specific detail.
    metadata      JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX audit_events_occurred_at_idx ON audit_events (occurred_at);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_user_id, occurred_at);

-- What `panorama login` leaves on a laptop. Long lived and deliberately not
-- spendable: it exchanges for a short-lived gateway token, so a grant that
-- leaks cannot buy model calls. Stored as a SHA-256, never in the clear.
CREATE TABLE cli_grants (
    id TEXT PRIMARY KEY,

    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The machine, as the CLI reported it. A developer with a laptop and a
    -- desktop needs to be able to tell which one to revoke.
    name TEXT NOT NULL DEFAULT '',

    prefix     TEXT NOT NULL,
    token_hash TEXT NOT NULL,

    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX cli_grants_hash_key ON cli_grants (token_hash);
CREATE INDEX cli_grants_user_idx ON cli_grants (user_id);

-- What a coding tool holds. Stored as a SHA-256, never in the clear.
CREATE TABLE gateway_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Operator-supplied label, e.g. "laptop" or "ci".
    name         TEXT NOT NULL DEFAULT '',
    -- Leading characters of the token, displayed so a user can tell their
    -- tokens apart without the secret being recoverable.
    prefix       TEXT NOT NULL,
    token_hash   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    -- Which grant minted it, when one did. Null for a token minted in the
    -- console and pasted by hand. Its purpose is revocation: ending a grant
    -- must end the tokens it produced, or signing a lost laptop out would
    -- leave its last token working until it expired on its own.
    grant_id     TEXT REFERENCES cli_grants (id) ON DELETE SET NULL
);

CREATE UNIQUE INDEX gateway_tokens_hash_key ON gateway_tokens (token_hash);
CREATE INDEX gateway_tokens_user_idx ON gateway_tokens (user_id);
CREATE INDEX gateway_tokens_grant_idx ON gateway_tokens (grant_id) WHERE grant_id IS NOT NULL;

-- What a pipeline holds. A table of its own rather than a nullable user_id on
-- gateway_tokens: it belongs to a team, bills that team's budget, and
-- survives the engineer who created it.
CREATE TABLE service_tokens (
    id TEXT PRIMARY KEY,

    -- The principal, so it cascades. created_by is provenance and is nulled
    -- when that account goes.
    team_id    TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    created_by TEXT REFERENCES users (id) ON DELETE SET NULL,

    name TEXT NOT NULL DEFAULT '',

    prefix     TEXT NOT NULL,
    token_hash TEXT NOT NULL,

    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX service_tokens_hash_key ON service_tokens (token_hash);
CREATE INDEX service_tokens_team_idx ON service_tokens (team_id);

-- An accounting line spend is allocated to, by what it bought rather than by
-- who spent it. A cost center carries a plan that warns; it is not a budget
-- and never blocks a request.
CREATE TABLE cost_centers (
    id TEXT PRIMARY KEY,

    -- The key the finance system already uses, and the column a downstream
    -- export is joined on. It is the administrator's string rather than one
    -- Panorama invented, which is why it is unique and why it is not the
    -- primary key: a chart of accounts gets renumbered, and an id that changed
    -- would orphan every rule pointing at it.
    --
    -- Unique on lower(code) because "cc-100" and "CC-100" are one account,
    -- and two rows for one account are two answers to what a line cost.
    code TEXT NOT NULL,
    name TEXT NOT NULL,

    -- Who to ask when a line runs hot. Free text on purpose: the owner is
    -- frequently a distribution list or a department that holds no account on
    -- this deployment, and a foreign key onto `users` would refuse exactly the
    -- person a CFO wants named.
    owner       TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',

    -- What this line is planned to spend in a calendar month, in nanodollars.
    -- Null where nobody has set one, which is the honest state and not zero.
    --
    -- Monthly and only monthly. A budget carries three windows because a daily
    -- ceiling is a circuit breaker against a runaway agent; nothing here breaks
    -- a circuit, and finance closes on a month.
    plan_nanos BIGINT,
    -- The share of the plan at which this line is worth raising. Percent of
    -- plan; only read when plan_nanos is set.
    warn_at_percent INTEGER NOT NULL DEFAULT 80,

    -- Whether `x-panorama-cost-center` may name this line.
    --
    -- Default on, because the header is how the cases no pattern can express
    -- get allocated at all, and a line nobody can stamp is a line that only
    -- ever collects what a rule happened to catch. Turned off for a line whose
    -- integrity matters more than its coverage: a stamp is the spender's claim
    -- about where their own cost belongs, and a capitalised project or a
    -- customer-billable engagement is exactly the line an administrator wants
    -- unreachable by assertion.
    --
    -- A refused stamp does not refuse the request and does not silently vanish.
    -- The request falls through to the rules - evidence is still evidence - and
    -- the rejected code is recorded so it surfaces in the worklist rather than
    -- being discovered a quarter later.
    accepts_stamp BOOLEAN NOT NULL DEFAULT TRUE,

    -- A closed account still owns its history.
    --
    -- Archiving hides it from every picker and refuses new rules, and its
    -- existing rules keep matching - which is what stops the archive from
    -- silently moving a year of recorded spend into the unallocated bucket.
    -- Deleting is the operation that does move it, and is refused by the write
    -- path once anything has been billed here.
    archived BOOLEAN NOT NULL DEFAULT FALSE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX cost_centers_code_key ON cost_centers (lower(code));

-- The ordered list that maps a repository or a ticket prefix to a cost
-- center. Ticket, then repository, then unallocated: there is no rung that
-- falls back to the spender's team.
CREATE TABLE cost_center_rules (
    id TEXT PRIMARY KEY,

    cost_center_id TEXT NOT NULL REFERENCES cost_centers (id) ON DELETE CASCADE,

    -- Evaluation order, lowest first. Not unique: ties break by id so the order
    -- is deterministic, and an administrator who cares says so with distinct
    -- positions.
    position INTEGER NOT NULL DEFAULT 0,

    -- What the pattern is matched against. Not named `match`, which is
    -- reserved in standard SQL and would need quoting on one of the two
    -- dialects but not the other.
    --
    -- 'repo' alone is insufficient and the pair is not arbitrary: a company
    -- with a monorepo has one repository, so repository rules alone would give
    -- it exactly one cost center. Ticket rules rescue that, and they need no
    -- integration - a branch named `feat/PAY-1183-x` is attributed to the
    -- ticket `PAY-1183` structurally, long before anybody connects a tracker.
    match_on TEXT NOT NULL CHECK (match_on IN ('ticket', 'repo')),

    -- The whole syntax is `*`, standing for any run of characters including
    -- none, matched without regard to case. Unlike path.Match's, this `*`
    -- crosses `/`: a pattern of `*payments*` meant to catch
    -- `acme/payments-api` and silently matching nothing is the kind of
    -- surprise that misfiles a quarter.
    pattern TEXT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One rule per pattern and kind, across every cost center. Two cost centers
-- claiming `acme/payments-*` is not a precedence question to be settled by
-- position - it is somebody's mistake, and the ordered list would resolve it
-- silently and permanently.
CREATE UNIQUE INDEX cost_center_rules_pattern_key
    ON cost_center_rules (match_on, lower(pattern));
CREATE INDEX cost_center_rules_center_idx ON cost_center_rules (cost_center_id);
CREATE INDEX cost_center_rules_order_idx ON cost_center_rules (position, id);

-- The audit trail: one row per request, the largest table in the schema.
-- Metadata only. No prompt or response body is stored here or anywhere else.
CREATE TABLE requests (
    id         TEXT PRIMARY KEY,
    -- The X-Request-Id echoed to the client, so an engineer quoting an error
    -- can be found here.
    request_id TEXT NOT NULL,

    -- Null when the request failed before authenticating, and set null rather
    -- than cascaded on delete: the record outlives the account.
    user_id  TEXT REFERENCES users (id) ON DELETE SET NULL,
    token_id TEXT REFERENCES gateway_tokens (id) ON DELETE SET NULL,

    started_at TIMESTAMPTZ NOT NULL,
    -- Total wall time from accept to last byte.
    duration_ms BIGINT NOT NULL DEFAULT 0,
    -- Time to first token, in microseconds. The gateway holds itself to a
    -- sub-10ms latency budget, so millisecond resolution would round the whole
    -- budget away.
    ttft_us BIGINT,

    -- Which client-facing API this came in on: anthropic, openai, or mcp for
    -- a web search made through the gateway's tool.
    surface  TEXT NOT NULL,
    provider TEXT NOT NULL DEFAULT '',

    -- The route the client named, and the provider-native model it became.
    -- Both are needed to explain a bill.
    requested_model TEXT NOT NULL DEFAULT '',
    resolved_model  TEXT NOT NULL DEFAULT '',

    streaming   BOOLEAN NOT NULL DEFAULT FALSE,
    status_code INTEGER NOT NULL DEFAULT 0,

    -- What the gateway did. `refused` is a name nobody routed, `blocked` is a
    -- budget (budget_state says which line), `error` is a failure.
    decision TEXT NOT NULL DEFAULT 'allowed'
             CHECK (decision IN ('allowed', 'refused', 'blocked', 'error')),
    -- Coarse failure class for triage. Never a provider message verbatim.
    error_kind TEXT NOT NULL DEFAULT '',

    -- All four token classes. Cache reads bill at a fraction of the input rate,
    -- so collapsing them would overstate coding-agent cost several-fold.
    input_tokens       BIGINT NOT NULL DEFAULT 0,
    output_tokens      BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens  BIGINT NOT NULL DEFAULT 0,
    cache_write_tokens BIGINT NOT NULL DEFAULT 0,

    -- The coding tool, derived from the user agent.
    tool       TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    client_ip  TEXT NOT NULL DEFAULT '',

    -- The portion of cache_write_tokens written with a one-hour time to live.
    -- The remainder is the five-minute cache. They are priced differently, so
    -- the split has to be recorded at request time - it cannot be recovered
    -- later.
    cache_write_1h_tokens BIGINT NOT NULL DEFAULT 0,

    -- Cost in nanodollars. Null, not zero, when the request could not be
    -- priced: no rate for the model, or a token class with no published rate.
    -- Zero would fold unknown spend into the totals as free; null keeps it
    -- countable and visible, the same stance the product takes toward
    -- unattributed spend. Includes web_search_cost_nanos.
    cost_nanos BIGINT,
    -- The same total broken out by class. Stored rather than recomputed
    -- because it is the evidence for the headline claim: showing that caching
    -- saved 90% on a request has to be checkable against the row, not
    -- re-derived from rates that have since changed.
    input_cost_nanos       BIGINT,
    output_cost_nanos      BIGINT,
    cache_read_cost_nanos  BIGINT,
    cache_write_cost_nanos BIGINT,
    -- Which price source produced the cost above, empty when unpriced.
    cost_source TEXT NOT NULL DEFAULT '',

    -- The budget state at evaluation: ok, over_soft, over_hard, or empty when
    -- no budget governs this request.
    budget_state TEXT NOT NULL DEFAULT '',
    -- How many times a provider was actually invoked: 0 when the gateway
    -- answered on its own (blocked, refused, or a body it could not read),
    -- otherwise 1.
    --
    -- Zero is load-bearing rather than incidental. It is the one predicate for
    -- "did this request consume provider capacity", which is exactly what a
    -- budget counts. Without it, the in-memory ledger and the query that
    -- reconciles it would each have to re-derive that from `decision`, and two
    -- derivations of the same rule drift - which here would mean a blocked
    -- user staying blocked because their blocked requests counted toward the
    -- limit that blocked them.
    attempts INTEGER NOT NULL DEFAULT 1,

    -- Which conversation a request belongs to.
    --
    -- Deliberately not a foreign key, and deliberately the only attribution
    -- column on this table. Not a key, because the audit row must survive a
    -- session being removed. Only one, because attribution is a property of
    -- the session: when a session's attribution is upgraded, every request in
    -- it is meant to move with it, and a work_item_id copied onto each row
    -- would freeze the old answer on the requests that came before.
    session_id TEXT NOT NULL DEFAULT '',

    -- Which team to bill, when there is no user to reach one through. Team
    -- spend is otherwise derived by joining `team_members` on `user_id`, and a
    -- service request has none, so without this its cost would be counted at
    -- the organization level and nowhere else. Null for every request a person
    -- made: those resolve their team through membership, so that a change of
    -- team moves history with it.
    team_id TEXT,
    -- Which service token served the request, when one did. Not a foreign key:
    -- a leaked CI token is revoked and deleted, and the record of what it did
    -- must not go with it.
    service_token_id TEXT NOT NULL DEFAULT '',

    -- The stamped cost center override, resolved. Null on almost every row,
    -- which is why it is a column rather than a table. The rollup reads
    -- COALESCE(requests.cost_center_id, work_items.cost_center_id), and that
    -- COALESCE is the whole of the precedence between a stamp and a rule. SET
    -- NULL rather than cascade: deleting a cost center moves recorded spend to
    -- unallocated, where it is visible.
    cost_center_id TEXT
        REFERENCES cost_centers (id) ON DELETE SET NULL,
    -- A stamp that did not resolve, kept verbatim for diagnosis: a code that
    -- named no cost center, or named one with accepts_stamp off. Ignoring it
    -- would be a fallback that masks an error; refusing the request would
    -- break a coding session over an accounting label.
    cost_center_stamp TEXT NOT NULL DEFAULT '',

    -- The sitting this request belongs to: the id of its first request.
    -- A sitting is one principal's conversation in one tool with one outcome
    -- class, with no more than thirty idle minutes between one request and
    -- the next (store.GroupIdleGap). Decided when the request is written and
    -- stored, because an idle gap compares neighbours and every filter and
    -- page of the trail would otherwise recompute it over the largest table.
    group_id TEXT NOT NULL DEFAULT '',

    -- How many web searches this row paid for, and what they cost. A search
    -- is a row of its own on the surface 'mcp', so it is in every total,
    -- budget and export through the queries that already exist.
    web_searches          BIGINT NOT NULL DEFAULT 0,
    web_search_cost_nanos BIGINT
);

CREATE INDEX requests_started_at_idx ON requests (started_at);
CREATE INDEX requests_user_idx ON requests (user_id, started_at);
CREATE INDEX requests_model_idx ON requests (resolved_model, started_at);
CREATE INDEX requests_session_idx ON requests (session_id);
CREATE INDEX requests_team_idx ON requests (team_id, started_at) WHERE team_id IS NOT NULL;
CREATE INDEX requests_cost_center_idx ON requests (cost_center_id, started_at);
-- The trail groups by this and the expansion of a sitting reads by it.
CREATE INDEX requests_group_idx ON requests (group_id, started_at);
-- What the write path looks up to find the sitting a request joins.
CREATE INDEX requests_group_key_idx ON requests (session_id, started_at);

CREATE TABLE budgets (
    id TEXT PRIMARY KEY,

    -- Which population this budget governs. The scope is polymorphic, so there
    -- is no foreign key: the referent is a team for 'team', a user for 'user',
    -- and the whole deployment for 'org'. A budget whose subject has been
    -- deleted stays visible rather than vanishing - an orphan an operator can
    -- see beats one the loader silently skips.
    scope    TEXT NOT NULL CHECK (scope IN ('org', 'team', 'user')),
    -- Empty for the org scope, which has exactly one subject.
    scope_id TEXT NOT NULL DEFAULT '',

    -- The window the limits apply to, always in UTC: a calendar day, a week
    -- beginning Monday, or a calendar month. Spend resets when the window
    -- rolls over - nothing is carried forward, and nothing is pro-rated.
    period TEXT NOT NULL CHECK (period IN ('day', 'week', 'month')),

    -- Money limits, in nanodollars. Null means "no limit of this kind".
    --
    -- Crossing the soft limit warns and closes the routes marked as not
    -- serving past one; crossing the hard limit blocks. Both are optional and
    -- independent: a soft-only budget is an early-warning system that never
    -- blocks anyone, which is the right first posture for a deployment that
    -- has not yet learned what its normal spend looks like.
    soft_limit_nanos BIGINT,
    hard_limit_nanos BIGINT,

    -- Operator-facing label, quoted back in the block message so an engineer
    -- who hits a limit knows which one.
    name TEXT NOT NULL DEFAULT '',

    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One budget per scope and period. Two budgets on the same user for the same
-- window would make "which limit did I hit" unanswerable.
CREATE UNIQUE INDEX budgets_scope_key ON budgets (scope, scope_id, period);

-- A session is one conversation, grouped across the turns that make it up.
--
-- Coding agents re-send the whole conversation every turn, so the turns of one
-- session share an opening. That opening, hashed with the principal, is the
-- fingerprint when no client header names the session - which is what lets a
-- deployment attribute work with no client configuration at all.
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,

    -- The grouping key. Always includes the principal, so two people whose
    -- conversations happen to open identically are never merged.
    fingerprint TEXT NOT NULL,
    -- How the fingerprint was derived, because the methods have different
    -- reliability and the difference should be visible rather than assumed.
    method TEXT NOT NULL DEFAULT '',

    user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
    tool    TEXT NOT NULL DEFAULT '',

    started_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    requests     INTEGER NOT NULL DEFAULT 0,

    -- What this session is working on, once known. Denormalised from
    -- `attributions` so the usage rollups are one join rather than two.
    work_item_id TEXT,
    -- Which rung of the ladder produced it, and how much to believe it.
    rung       TEXT NOT NULL DEFAULT '',
    confidence TEXT NOT NULL DEFAULT '',

    -- The structural facts observed, kept for display: "PAY-1183 - branch name
    -- (structural, high)" needs the branch as well as the ticket.
    repo   TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',

    -- Whose conversation this is, when it is not a person's. A service token
    -- has no user, so its id is mixed into the fingerprint instead, and this
    -- column records which - otherwise every pipeline in the deployment would
    -- share one session.
    service_token_id TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX sessions_fingerprint_key ON sessions (fingerprint);
CREATE INDEX sessions_user_idx ON sessions (user_id, last_seen_at);
CREATE INDEX sessions_work_item_idx ON sessions (work_item_id);

-- A unit of work money can be attributed to: a branch, a pull request, or a
-- ticket.
--
-- Created by inference - a branch seen in a request body is a work item the
-- moment it is seen, with no integration configured. An integration confirms
-- it and fills in the title, status and URL; the row is the same row,
-- upgraded in place.
CREATE TABLE work_items (
    id TEXT PRIMARY KEY,

    kind TEXT NOT NULL CHECK (kind IN ('branch', 'pull_request', 'ticket')),
    -- The natural identifier, unique within its kind: "PAY-1183" for a ticket,
    -- "acme/payments-api#412" for a pull request, "acme/payments-api@feat/x"
    -- for a branch.
    key TEXT NOT NULL,

    repo   TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',

    -- Filled by an integration. Empty until then, and empty is honest: an
    -- inferred branch has no title until something is asked.
    title  TEXT NOT NULL DEFAULT '',
    url    TEXT NOT NULL DEFAULT '',
    -- The source's own word, for display: "Merged", "In review", "Won't Do".
    status TEXT NOT NULL DEFAULT '',

    -- Where the row came from. 'inferred' is the gateway's own reading of a
    -- request; the others mean an integration confirmed it.
    source TEXT NOT NULL DEFAULT 'inferred'
           CHECK (source IN ('inferred', 'stamp', 'github', 'jira')),

    -- When Panorama saw it in traffic.
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,

    -- What a system of record said about it. All empty for a deployment with
    -- nothing connected, and that is the supported configuration rather than a
    -- degraded one: attribution works with no integration at all. What is
    -- missing without one is the outcome half - cost per merged pull request
    -- cannot be computed from traffic alone.
    author  TEXT NOT NULL DEFAULT '',
    project TEXT NOT NULL DEFAULT '',
    -- The canonical lifecycle position, for metrics. Separate from `status` on
    -- purpose: "Merged", "Closed", "Done", "Released" and "Won't Do" are five
    -- words and three outcomes, and a metric grouped on the source's word
    -- would gain a sixth category the day somebody renames a workflow state.
    -- The empty string is "nothing has been asked yet", a bucket of its own
    -- rather than folded into `open`.
    outcome TEXT NOT NULL DEFAULT ''
      CHECK (outcome IN ('', 'open', 'landed', 'abandoned')),
    -- The source's identifier, kept so a later call can address the item
    -- without reconstructing its key from parts.
    external_id TEXT NOT NULL DEFAULT '',
    -- When the item was last asked about, and when a source last recognised
    -- it. checked_at with no confirmed_at means "we asked and GitHub has never
    -- heard of this branch" - the honest answer for a branch that was never
    -- pushed, which must not read as an integration failure.
    checked_at   TIMESTAMPTZ,
    confirmed_at TIMESTAMPTZ,
    -- The work item's own lifecycle, as the source reports it. Distinct from
    -- first_seen_at and last_seen_at: a ticket opened in March and first
    -- worked on in August has both, and they mean different things.
    opened_at TIMESTAMPTZ,
    merged_at TIMESTAMPTZ,
    closed_at TIMESTAMPTZ,

    -- Where the allocation lands. Derived, never authored: recomputed from the
    -- cost center rules in the same transaction as any rule write. On the work
    -- item rather than on `requests`, which is one pattern match per
    -- conversation instead of one per request. SET NULL rather than cascade:
    -- deleting a cost center must not delete the record of work that was done.
    cost_center_id TEXT
    REFERENCES cost_centers (id) ON DELETE SET NULL,
    -- Which rule claimed it, so an allocation can say why: "CC-100 - matched
    -- acme/payments-*" is what lets somebody challenge a line and get an
    -- answer. Written in the same statement as cost_center_id.
    cost_center_rule_id TEXT
    REFERENCES cost_center_rules (id) ON DELETE SET NULL
);

CREATE UNIQUE INDEX work_items_key ON work_items (kind, key);
CREATE INDEX work_items_outcome_idx ON work_items (kind, outcome);
CREATE INDEX work_items_project_idx ON work_items (project) WHERE project <> '';
CREATE INDEX work_items_cost_center_idx ON work_items (cost_center_id);

-- The link, with its provenance.
--
-- One row per session: the current answer, upgraded in place when better
-- evidence arrives. A session is attributed once and inherits across turns
-- rather than being re-guessed per request, so this table grows with
-- conversations rather than with traffic.
CREATE TABLE attributions (
    id TEXT PRIMARY KEY,

    session_id   TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    work_item_id TEXT NOT NULL REFERENCES work_items (id) ON DELETE CASCADE,

    -- The rung of the provenance ladder. Higher always wins, and the order is
    -- fixed: a stamp beats structure, structure beats content. Content is never
    -- promoted above structure no matter how confident it looks, because a
    -- ticket key mentioned in a sentence is a mention and a branch name is a
    -- fact.
    rung TEXT NOT NULL CHECK (rung IN ('stamp', 'structural', 'content')),
    confidence TEXT NOT NULL CHECK (confidence IN ('high', 'medium', 'low')),

    -- What kind of thing was matched - "branch name", "environment block",
    -- "gh pr view output". A label, never the matched text: this table is
    -- covered by the same promise as everything else, that no body content is
    -- stored anywhere.
    evidence TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX attributions_session_key ON attributions (session_id);
CREATE INDEX attributions_work_item_idx ON attributions (work_item_id);

-- The relation between units of work. A branch is opened as a pull request;
-- a pull request implements a ticket. Recorded as a chain rather than
-- collapsed, because collapsing it at write time would mean re-attributing a
-- session on an integration's say-so, which is the one thing confirmation is
-- not allowed to do.
CREATE TABLE work_item_links (
    from_id TEXT NOT NULL REFERENCES work_items (id) ON DELETE CASCADE,
    to_id   TEXT NOT NULL REFERENCES work_items (id) ON DELETE CASCADE,

    -- Derived from the two kinds at write time, so the console does not have to
    -- re-derive it and get the wording subtly different.
    relation TEXT NOT NULL DEFAULT 'relates'
             CHECK (relation IN ('opened_as', 'implements', 'relates')),

    created_at TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (from_id, to_id)
);

CREATE INDEX work_item_links_to_idx ON work_item_links (to_id);

-- A connected system of record, one row per kind: its connection and its
-- health on the same row, because they share a lifetime and "when did this
-- last work" is read at the same moment as "what is it pointed at".
CREATE TABLE integrations (
    kind TEXT PRIMARY KEY CHECK (kind IN ('github', 'jira')),

    -- Which implementation is behind it: 'github', 'jira', or 'mock'. Shown in
    -- the console, because a number that came out of a fixtures file must never
    -- look like one that came out of GitHub.
    provider TEXT NOT NULL,
    -- The source's own description of itself, e.g. the fixtures file a mock
    -- read.
    detail TEXT NOT NULL DEFAULT '',

    status TEXT NOT NULL DEFAULT 'unknown'
           CHECK (status IN ('unknown', 'ok', 'error')),
    -- The last failure, kept after it clears so an intermittent problem is
    -- still visible once the next check succeeds.
    last_error    TEXT NOT NULL DEFAULT '',
    last_error_at TIMESTAMPTZ,

    last_checked_at TIMESTAMPTZ,
    last_synced_at  TIMESTAMPTZ,

    -- What the last run did, so the screen can say something specific.
    last_checked_items   INTEGER NOT NULL DEFAULT 0,
    last_confirmed_items INTEGER NOT NULL DEFAULT 0,

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,

    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    -- Non-secret connection settings, kept in the clear so an operator can
    -- read them back out of the console to check them. The app id and
    -- installation id identify an installation; neither authenticates
    -- anything on its own.
    app_id          TEXT NOT NULL DEFAULT '',
    installation_id TEXT NOT NULL DEFAULT '',
    api_base_url    TEXT NOT NULL DEFAULT '',
    -- The part that authenticates: a GitHub App private key, or a personal
    -- access token. Sealed with the deployment's key before it arrives here,
    -- and never returned by any API - a credential that can be read back out
    -- of the console is a credential the console can leak.
    secret_ciphertext TEXT NOT NULL DEFAULT '',
    -- Which kind of credential the ciphertext holds, so the source knows what
    -- it is decrypting without trying to parse it.
    secret_kind TEXT NOT NULL DEFAULT ''
      CHECK (secret_kind IN ('', 'app_key', 'token')),
    -- Who connected it and when. Nulled rather than cascaded when that account
    -- goes, because the connection outlives the person who made it.
    configured_at TIMESTAMPTZ,
    configured_by TEXT REFERENCES users (id) ON DELETE SET NULL
);

-- What a browser holds. Stored as a SHA-256, never in the clear.
CREATE TABLE console_sessions (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,

    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- The value the page must echo back in a header on every state-changing
    -- request.
    --
    -- Cookie authentication is ambient: the browser attaches it to a request
    -- the page did not make. SameSite closes most of that, but it is one
    -- attribute on one cookie and browsers have disagreed about its edges more
    -- than once, so the header is required as well. It is not a secret - the
    -- page reads it - it is a thing a cross-origin caller cannot set.
    csrf_token TEXT NOT NULL,

    created_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,

    -- Shown on the sessions list so a user can recognise the browser they are
    -- about to sign out.
    user_agent TEXT NOT NULL DEFAULT '',
    source_ip  TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX console_sessions_token_key ON console_sessions (token_hash);
CREATE INDEX console_sessions_user_idx ON console_sessions (user_id);
CREATE INDEX console_sessions_expiry_idx ON console_sessions (expires_at);

-- An OIDC sign-in in progress. Short-lived and single-use: a state that can be
-- redeemed twice is a state that can be replayed.
CREATE TABLE oidc_flows (
    state    TEXT PRIMARY KEY,
    nonce    TEXT NOT NULL,
    verifier TEXT NOT NULL,

    -- Where to land after sign-in. Validated as a same-origin path before it is
    -- stored; an open redirect on a login endpoint is how a phishing page
    -- borrows your domain.
    redirect_to TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX oidc_flows_expiry_idx ON oidc_flows (expires_at);

-- The group-to-role mapping. Data rather than configuration, because the names
-- of a company's groups are organizational data an administrator edits from a
-- screen. The strongest matching group wins.
CREATE TABLE oidc_group_roles (
    group_name TEXT PRIMARY KEY,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'manager', 'developer')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

-- The group names identity providers have actually sent, offered as a
-- picklist. Names only: membership stays the provider's record.
CREATE TABLE oidc_observed_groups (
    group_name TEXT PRIMARY KEY,

    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,

    -- Sign-ins that carried this group. Ordering by it puts the groups a
    -- deployment actually uses above the ones one contractor was in once.
    seen_count BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX oidc_observed_groups_last_seen_idx ON oidc_observed_groups (last_seen_at);

-- Counters that cannot be derived from anything else: how many sign-ins
-- carried no groups at all is the question worth answering, and `users`
-- counts people rather than sign-ins. One is a directory nobody has finished
-- configuring; all of them is a groups claim that does not exist. A table
-- rather than a settings row so the increment is one atomic statement.
CREATE TABLE oidc_signin_counters (
    name  TEXT PRIMARY KEY,
    count BIGINT NOT NULL DEFAULT 0
);

-- What a redeemed setup code opens: not an account, a session that may
-- configure the identity connection, the model providers and the
-- administrators list, and nothing else, for an hour.
CREATE TABLE setup_sessions (
    id TEXT PRIMARY KEY,

    -- The cookie's SHA-256, exactly as with a console session or a gateway
    -- token. Nothing here is a working credential if the database leaks.
    token_hash TEXT NOT NULL,

    -- Echoed back in a header on every write, for the same reason a console
    -- session carries one: cookie authentication is ambient.
    csrf_token TEXT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,

    -- Recorded because a setup session is the most privileged thing that can
    -- happen to this deployment's identity configuration, and the audit
    -- question afterwards is where it came from.
    source_ip TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX setup_sessions_token_key ON setup_sessions (token_hash);
CREATE INDEX setup_sessions_expiry_idx ON setup_sessions (expires_at);

-- The claim names ID tokens have carried, with two counts. Names are schema;
-- values are somebody's email, subject and group membership, and there is
-- nowhere here to put one. "The groups claim is empty" has two opposite
-- causes, and this is what tells them apart.
CREATE TABLE oidc_observed_claims (
    claim_name TEXT PRIMARY KEY,

    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,

    -- Sign-ins whose ID token carried this claim. A claim present on some
    -- sign-ins and not others is its own diagnosis: Entra drops the groups
    -- claim for a user in more than 200 groups, so the claim that is there for
    -- most people and missing for one is not a misconfiguration.
    seen_count BIGINT NOT NULL DEFAULT 0,

    -- Sign-ins where the claim was present but carried nothing - an empty
    -- array, an empty string, null. This is the state that looks like success
    -- and is not.
    empty_count BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX oidc_observed_claims_last_seen_idx ON oidc_observed_claims (last_seen_at);

-- The routes: everything a client may ask for. A name absent from this table
-- is refused, and a route becomes exactly one model.
CREATE TABLE model_routes (
    -- What the client sends, verbatim: "claude-sonnet-5", "fast".
    name TEXT PRIMARY KEY,

    description TEXT NOT NULL DEFAULT '',

    -- Who created it, kept for the audit trail. A setup session has no user
    -- row, so this is a label rather than a reference.
    enabled_by TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,

    -- Who the route exists for: user ids, team ids, tools. Empty lists admit
    -- everybody, and every non-empty list must match.
    --
    -- JSON arrays rather than a junction table, and the reason is the failure
    -- mode rather than convenience. Rows referencing `users` would have to
    -- cascade, and a cascade here *widens* a route: delete the one person a
    -- route names and the condition disappears, leaving a route that admits
    -- everybody. A list of ids that no longer resolve simply never matches,
    -- which fails closed.
    match_users TEXT NOT NULL DEFAULT '[]',
    match_teams TEXT NOT NULL DEFAULT '[]',
    match_tools TEXT NOT NULL DEFAULT '[]',
    -- The model it becomes, as a provider reference
    -- ("bedrock/global.anthropic.claude-sonnet-5"). The write path refuses an
    -- empty one, because a route with nowhere to send a request would admit it
    -- and then have nothing to do with it.
    --
    -- Deliberately not a foreign key onto provider_models: the catalogue is a
    -- cache the discovery job rebuilds, so a cascade would let an AWS-side
    -- change delete an administrator's decision and a restrictive constraint
    -- would let it block one.
    target_model TEXT NOT NULL DEFAULT '',
    -- Whether this route is still served once a budget governing the caller
    -- has passed its soft limit. True by default: a soft limit warns and
    -- serves. False is the deliberate mark on the expensive route, and makes
    -- an engineer switch rather than changing their model for them.
    serves_over_soft BOOLEAN NOT NULL DEFAULT TRUE
);

-- A device-authorization flow in progress (RFC 8628), with Panorama as its
-- own authorization server. Approval requires a console session, which
-- requires the identity provider; what this table holds is the few minutes
-- between a CLI asking and a human agreeing. Single-use, like `oidc_flows`.
CREATE TABLE cli_authorizations (
    id TEXT PRIMARY KEY,

    -- The CLI's secret, hashed. It polls with this and nothing else.
    device_code_hash TEXT NOT NULL,

    -- The part a human reads off one screen and types into another. Short, and
    -- drawn from an alphabet with I, L, O, 0 and 1 removed, because it is
    -- transcribed by eye. It is not a secret: approving it requires a signed-in
    -- session, and the device code is what actually collects the grant.
    user_code TEXT NOT NULL,

    -- What asked, and from where. Shown on the approval screen so the person
    -- clicking Allow is deciding about something described rather than about an
    -- opaque code.
    client_name TEXT NOT NULL DEFAULT '',
    source_ip   TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,

    -- The polling floor the CLI is told to respect, in seconds. Stored rather
    -- than assumed so slow_down can raise it for one client without changing a
    -- constant for everybody.
    interval_seconds BIGINT NOT NULL DEFAULT 5,

    -- When the CLI last asked. RFC 8628 answers a client that polls faster than
    -- its interval with slow_down rather than with the grant, and that judgment
    -- is per device code: rate limiting by address would punish everyone behind
    -- one office NAT for one impatient client.
    last_polled_at TIMESTAMPTZ,

    -- Set when a human decides. Exactly one of these is ever non-null.
    user_id     TEXT REFERENCES users (id) ON DELETE CASCADE,
    approved_at TIMESTAMPTZ,
    denied_at   TIMESTAMPTZ,

    -- Set when the CLI collects the grant. The row survives its own redemption
    -- so a second poll is answered "already claimed" rather than by a second
    -- grant.
    claimed_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX cli_authorizations_device_key ON cli_authorizations (device_code_hash);
CREATE UNIQUE INDEX cli_authorizations_user_code_key ON cli_authorizations (user_code);
CREATE INDEX cli_authorizations_expiry_idx ON cli_authorizations (expires_at);

-- What a provider can serve, as its discovery job last found it. A cache, keyed
-- by provider and id, replaced per provider.
CREATE TABLE provider_models (
    -- Which provider this identifier is valid on. Not a foreign key onto
    -- `providers`: this is a cache the discovery job rebuilds, and a cascade
    -- from configuration into it, or a restriction from it onto configuration,
    -- would let one decide the other.
    provider TEXT NOT NULL,

    -- The identifier the provider is given, inference-profile prefix included.
    id TEXT NOT NULL,

    -- The bare model underneath, which carries the price.
    foundation_model_id TEXT NOT NULL,

    -- Who made the model - "Anthropic", "OpenAI" - as the provider names them.
    vendor       TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',

    -- The inference-profile prefix as itself, or '' for a bare id. Always ''
    -- on bedrock-mantle, which has no profiles.
    geography TEXT NOT NULL DEFAULT '',

    -- The region the provider was asked. A bare id is invoked there, so for a
    -- provider without profiles this is the residency answer.
    region TEXT NOT NULL DEFAULT '',

    -- The regions a profile may route to, comma-separated, as the provider
    -- enumerated them. Empty for global.
    regions TEXT NOT NULL DEFAULT '',

    streaming        BOOLEAN NOT NULL DEFAULT TRUE,
    lifecycle        TEXT NOT NULL DEFAULT '',
    requires_profile BOOLEAN NOT NULL DEFAULT FALSE,

    -- The client APIs this target can be served on, comma-separated:
    -- anthropic-messages, openai-chat, openai-responses. Empty is a model this
    -- build can list and cannot serve - browse-only in the console, and refused
    -- as a route's model. Neither endpoint publishes this; it is the provider's
    -- table (internal/providers/bedrock/apis.go), recorded where every reader
    -- already looks.
    apis TEXT NOT NULL DEFAULT '',

    -- The provider's own word for a model it lists and cannot invoke right
    -- now, and why. Empty for an ordinary, invocable model. Kept rather than
    -- filtered, so "why is this model missing" is answered in the console.
    status        TEXT NOT NULL DEFAULT '',
    status_reason TEXT NOT NULL DEFAULT '',

    -- The data-retention modes the provider allows for this model,
    -- comma-separated, as it names them. A compliance column: some models
    -- require retention for abuse review, and a deployment that promises
    -- otherwise needs to see which.
    retention TEXT NOT NULL DEFAULT '',

    -- What the model accepts and produces, zero when unrecorded. No provider
    -- publishes these; they come from the hand-maintained table in
    -- internal/providers/bedrock/windows.go, and a wrong window is worse than
    -- an absent one.
    context_window    INTEGER NOT NULL DEFAULT 0,
    max_output_tokens INTEGER NOT NULL DEFAULT 0,

    discovered_at TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (provider, id)
);

CREATE INDEX provider_models_fm_idx  ON provider_models (foundation_model_id);
CREATE INDEX provider_models_geo_idx ON provider_models (provider, geography);

-- A model provider: a kind, a region and a switch. One row per kind.
CREATE TABLE providers (
    -- The provider, and the identity. The CHECK is the same allowlist the code
    -- holds (providers.Kinds): a row naming anything else would be a provider
    -- nothing can build, and the process refuses to start with one. Adding a
    -- kind rebuilds this table, which is safe because nothing references it.
    kind TEXT PRIMARY KEY CHECK (kind IN ('bedrock', 'bedrock-mantle')),

    -- Whether requests may be sent to it. Turning a provider off that a route
    -- uses is refused at the write path, naming the routes, so a disabled row
    -- is one nothing depends on.
    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    -- Where this provider is invoked, e.g. us-east-1.
    region TEXT NOT NULL,

    -- What corrects this build's tables for this provider, as JSON: the beta
    -- flags to drop, context-window overrides, and which APIs a model family
    -- speaks. Every key exists because the provider does not publish the fact.
    options TEXT NOT NULL DEFAULT '{}',

    -- When a live invocation last succeeded against this row, and against
    -- which model. Cleared when the region changes: a verification is a
    -- statement about one region.
    verified_at    TIMESTAMPTZ,
    verified_model TEXT NOT NULL DEFAULT '',

    -- Who last changed it, for the audit trail. A setup session has no user
    -- row, so this is a label rather than a reference.
    configured_at TIMESTAMPTZ NOT NULL,
    configured_by TEXT NOT NULL DEFAULT ''
);

-- Rates per model, seeded from the compiled-in table and refreshed from the
-- AWS Price List API.
CREATE TABLE price_table (
    provider TEXT NOT NULL,
    -- The foundation-model id, with no inference-profile prefix:
    -- anthropic.claude-sonnet-5, not us.anthropic.claude-sonnet-5.
    model    TEXT NOT NULL,
    -- Which of the model's two published rate sets this row holds. AWS prices
    -- a model differently through a geographic inference profile and through
    -- the global one, by about 10%, which is why the scope is part of the key
    -- rather than normalised away.
    scope    TEXT NOT NULL CHECK (scope IN ('regional', 'global')),

    -- Nanodollars per million tokens. Zero means "not published", which is not
    -- the same as free: a request using a class with a zero rate is recorded
    -- unpriced rather than costed at nothing.
    input_rate          BIGINT NOT NULL DEFAULT 0,
    output_rate         BIGINT NOT NULL DEFAULT 0,
    cache_read_rate     BIGINT NOT NULL DEFAULT 0,
    -- The five-minute cache, which is the default and carries nearly all
    -- traffic.
    cache_write_rate    BIGINT NOT NULL DEFAULT 0,
    -- The one-hour cache, separately published at roughly 1.6x the
    -- five-minute rate.
    cache_write_1h_rate BIGINT NOT NULL DEFAULT 0,

    currency TEXT NOT NULL DEFAULT 'USD',
    -- Where the rate came from, so a shipped estimate is never mistaken for a
    -- fetched price. Config beats the pricing API beats the compiled-in seed,
    -- which beats a rate read from a model card.
    source   TEXT NOT NULL CHECK (source IN ('seed', 'config', 'aws-pricing-api', 'aws-model-card')),
    -- Which region's catalogue this rate was read from. Provenance, not part
    -- of the key.
    region   TEXT NOT NULL DEFAULT '',

    -- When the price took effect, as published.
    as_of      TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL,

    -- The rates that replace this row's own above a prompt size, as JSON:
    -- [{"above_input_tokens": 272000, "input": 440000000, ...}], in the same
    -- units, and '' for a model with one price at every size. JSON rather than
    -- a child table, because a tier is never queried on its own: the whole
    -- row is read into memory, and nothing on the request path touches the
    -- database.
    tiers      TEXT NOT NULL DEFAULT '',

    PRIMARY KEY (provider, model, scope)
);

-- The web search backend, one row per kind.
CREATE TABLE web_search (
    kind TEXT PRIMARY KEY CHECK (kind IN ('bedrock-agentcore')),

    -- Off until an administrator turns it on. A search sends text a model
    -- wrote from somebody's conversation to a backend, which is a decision to
    -- make rather than a default to inherit.
    enabled BOOLEAN NOT NULL DEFAULT FALSE,

    -- The AgentCore gateway's MCP URL. Its host names the gateway and the
    -- region, and nothing but a host of that shape is accepted, because the
    -- gateway is called with a signature made from the deployment's role.
    gateway_url TEXT NOT NULL,

    -- What one search costs, in nanodollars. Null means the rate AWS
    -- published, which this build carries; a figure here overrides it.
    price_nanos BIGINT,

    -- When a check last reached the gateway and found its web search tool.
    -- Cleared when the gateway changes: a check is a statement about one.
    verified_at TIMESTAMPTZ,

    configured_at TIMESTAMPTZ NOT NULL,
    configured_by TEXT NOT NULL DEFAULT ''
);

-- +goose Down

DROP TABLE web_search;
DROP TABLE price_table;
DROP TABLE providers;
DROP TABLE provider_models;
DROP TABLE cli_authorizations;
DROP TABLE model_routes;
DROP TABLE oidc_observed_claims;
DROP TABLE setup_sessions;
DROP TABLE oidc_signin_counters;
DROP TABLE oidc_observed_groups;
DROP TABLE oidc_group_roles;
DROP TABLE oidc_flows;
DROP TABLE console_sessions;
DROP TABLE integrations;
DROP TABLE work_item_links;
DROP TABLE attributions;
DROP TABLE work_items;
DROP TABLE sessions;
DROP TABLE budgets;
DROP TABLE requests;
DROP TABLE cost_center_rules;
DROP TABLE cost_centers;
DROP TABLE service_tokens;
DROP TABLE gateway_tokens;
DROP TABLE cli_grants;
DROP TABLE audit_events;
DROP TABLE settings;
DROP TABLE team_members;
DROP TABLE teams;
DROP TABLE users;
