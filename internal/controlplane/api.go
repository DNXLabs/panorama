// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/secrets"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/version"
	"github.com/dnxlabs/panorama/internal/websearch"
)

// defaultWindow is how far back the console looks when no range is given.
const defaultWindow = 7 * 24 * time.Hour

// API serves the console's REST surface.
//
// Every route here is authenticated: by a console session, or by the same
// per-user gateway token a coding tool presents, which is what makes the API
// reachable from a script and from a deployment that has no single sign-on yet.
type API struct {
	db     *store.DB
	prices *pricing.Table
	policy *policy.Engine
	log    *slog.Logger

	// integrations resolves what is currently connected. Connections are made
	// in the console, so this is asked each time rather than held: a registry
	// captured at boot would answer for the deployment as it was started, not
	// as it is.
	integrations *integrations.Resolver
	// secrets encrypts what an administrator stores. Keyless on a deployment
	// with no encryption key, which is supported - it simply cannot hold a
	// credential, and says so.
	secrets *secrets.Box
	// providers builds and holds the configured model providers. Which ones
	// exist, and in which region, is data in the providers table; this is how
	// the console reaches them to discover, check and describe them without
	// naming an implementation.
	providers *providers.Registry
	// priceService refreshes rates for a provider whose region an
	// administrator just changed. Nil where nothing may refresh.
	priceService *pricing.Service
	// webSearch holds the backend /mcp searches with, and is reloaded when an
	// administrator saves its configuration. Nil is a deployment with none.
	webSearch *websearch.Service
	// webSearchKind describes the one backend this build offers.
	webSearchKind websearch.Descriptor
	// webSearchDep is the deployment's own gateway, when it named one.
	webSearchDep websearch.Deployment

	// server is held so the handlers that write cookies - token minting does
	// not, but sign-out and the wizard do - can reach the session machinery
	// without a second copy of it.
	server *Server
}

// mountAPI registers the console API.
func (s *Server) mountAPI(api *API) {
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/me":              api.handleMe,
		"GET /api/v1/requests":        api.handleRequests,
		"GET /api/v1/requests/groups": api.handleRequestGroups,
		"GET /api/v1/usage":           api.handleUsage,
		"GET /api/v1/prices":          api.handlePrices,
		"GET /api/v1/policy":          api.handlePolicy,
		"GET /api/v1/work":            api.handleWork,
		"GET /api/v1/sessions":        api.handleSessions,

		"GET /api/v1/auth/sessions":  s.handleListSessions,
		"POST /api/v1/auth/sessions": s.handleRevokeSessions,
	} {
		s.mux.Handle(pattern, s.authenticated(handler))
	}

	// A setup session has to be able to ask what it is and to end itself.
	// Everything else above stays closed to it - listing signed-in browsers
	// reads a user id it does not have, and would panic before it refused.
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/auth/session":  s.handleSession,
		"POST /api/v1/auth/signout": s.handleSignOut,
	} {
		s.mux.Handle(pattern, s.authenticatedAllowing(true, handler))
	}
}

// handleMe reports who the caller acts as, so the console can decide what to
// show before asking for anything.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	writeJSON(w, http.StatusOK, map[string]any{
		"user": renderPrincipal(p),
		// Kept beside the user for the console written against the earlier
		// shape of this endpoint.
		"can_see_everyone": seesEveryone(p.Role),
	})
}

// handleSession answers who is signed in and hands the page its CSRF token.
//
// The token is served here rather than in a cookie the page reads. Panorama
// sends no CORS headers at all, so a cross-origin page cannot read this
// response - which makes the value exactly what a CSRF token has to be: one
// only a same-origin page can obtain.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	body := map[string]any{
		"user":           renderPrincipal(p),
		"sso_configured": s.cfg.OIDC.Configured(),
		// Which build is running. Served here rather than from /settings
		// because the console footer names it on every screen and /settings is
		// closed to a developer - a footer that read "v2.4.1" for a manager and
		// nothing for the engineer beside them is a support conversation about
		// which of them is on the old version.
		"version": version.Get().Version,
		// Where the running build's source is, at its commit. The footer
		// links it on every screen, for everybody signed in, because AGPL-3.0
		// section 13 is an offer to every user of the deployment and not to
		// its administrators.
		"source": version.Get().Source,
		// Whether this deployment serves command-line sign-in. Here for the
		// same reason the version is: My setup and the approval screen both
		// need it, one of them is a developer's screen, and /settings is
		// closed to a developer. One fact on one response, so the two screens
		// cannot disagree about whether `panorama login` works.
		"cli_sign_in": s.cliSignInOn(r.Context()),
	}

	// A setup session gets its own token from its own table. Answered first,
	// because startSetupSession cleared the console cookie and there is nothing
	// else this browser could be holding.
	if p.IsSetup() {
		if value := s.readCookie(r, setupCookie); value != "" {
			if _, session, err := s.db.AuthenticateSetupSession(r.Context(), auth.Hash(value)); err == nil {
				body["csrf_token"] = session.CSRFToken
				body["session_expires_at"] = session.ExpiresAt.UTC().Format(time.RFC3339)
			}
		}
		writeJSON(w, http.StatusOK, body)
		return
	}

	// A gateway token carries no session and therefore no CSRF token. Saying so
	// explicitly stops a console from waiting for one that will never arrive.
	if value := s.readCookie(r, sessionCookie); value != "" {
		if _, session, err := s.db.AuthenticateConsoleSession(r.Context(), auth.Hash(value)); err == nil {
			body["csrf_token"] = session.CSRFToken
			body["session_expires_at"] = session.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}

	writeJSON(w, http.StatusOK, body)
}

// handleSignOut ends the current session.
func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if PrincipalFrom(r.Context()).IsSetup() {
		s.endSetupSession(w, r)
		return
	}
	if err := s.signOut(w, r); err != nil {
		s.fail(w, r, "signing out", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

// handleListSessions lists the caller's own signed-in browsers.
//
// Their own, always: there is no parameter that widens it. Whose browsers are
// open where is not a thing one colleague gets to look at about another, and
// the way to keep that true is not to build the query.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	sessions, err := s.db.ListConsoleSessions(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, "listing sessions", err)
		return
	}

	out := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, map[string]any{
			"id":           session.ID,
			"created_at":   session.CreatedAt.UTC().Format(time.RFC3339),
			"last_seen_at": session.LastSeenAt.UTC().Format(time.RFC3339),
			"expires_at":   session.ExpiresAt.UTC().Format(time.RFC3339),
			"user_agent":   session.UserAgent,
			"source_ip":    session.SourceIP,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// handleRevokeSessions signs the caller out of every browser, including this
// one.
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	if err := s.db.RevokeUserConsoleSessions(r.Context(), p.UserID); err != nil {
		s.fail(w, r, "revoking sessions", err)
		return
	}
	s.clearCookie(w, sessionCookie)
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

// handleRequests serves the request log.
func (a *API) handleRequests(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	limit, err := intParam(r, "limit")
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	filter.Limit = limit
	filter.Cursor = r.URL.Query().Get("cursor")

	page, err := a.db.ListRequests(r.Context(), filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeAPIError(w, r, http.StatusBadRequest, "That cursor is not valid.")
			return
		}
		a.fail(w, r, "listing requests", err)
		return
	}

	requests := make([]map[string]any, 0, len(page.Requests))
	for _, req := range page.Requests {
		requests = append(requests, renderRequest(req))
	}
	body := map[string]any{
		"requests":    requests,
		"next_cursor": page.NextCursor,
	}

	// How the window divides, sent with the first page and not recomputed while
	// somebody pages through it. The counts describe the window rather than the
	// page, so they do not change as more rows load, and a `GROUP BY` over
	// weeks of the largest table in the schema is not worth running again for
	// each fifty rows.
	if filter.Cursor == "" {
		outcomes, cerr := a.db.CountOutcomes(r.Context(), filter)
		if cerr != nil {
			a.fail(w, r, "counting request outcomes", cerr)
			return
		}
		body["outcomes"] = renderOutcomes(outcomes)
	}

	writeJSON(w, http.StatusOK, body)
}

// handleUsage serves aggregated spend.
func (a *API) handleUsage(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	summary, err := a.db.Summarize(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "summarizing usage", err)
		return
	}

	body := map[string]any{
		"since":    filter.Since.Format(time.RFC3339),
		"until":    filter.Until.Format(time.RFC3339),
		"scope":    scopeLabel(filter.UserID),
		"totals":   renderTotals(summary.Totals),
		"by_model": renderBuckets(summary.ByModel),
		"by_tool":  renderBuckets(summary.ByTool),
		"by_day":   renderBuckets(summary.ByDay),
	}

	// Teams are only meaningful for someone looking at the organization. A
	// developer asking for their own usage would get one row that is just their
	// own total under a team's name, which is a leaderboard with one entry.
	if filter.UserID == "" {
		teams, overlapping, terr := a.db.SummarizeTeams(r.Context(), filter)
		if terr != nil {
			a.fail(w, r, "rolling up teams", terr)
			return
		}
		rows := make([]map[string]any, 0, len(teams))
		for _, t := range teams {
			row := renderTotals(t.Totals)
			row["key"] = t.Slug
			row["name"] = t.Name
			row["members"] = t.Members
			// The attributed share, beside the spend it is a share of, so a
			// table showing both columns never divides one query's numerator
			// by another query's denominator.
			row["attributed_cost_nanos"] = t.AttributedCost
			rows = append(rows, row)
		}
		body["by_team"] = rows
		// Stated rather than left for someone to notice that the bars do not
		// add up to the total.
		body["teams_overlap"] = overlapping
		if overlapping {
			body["teams_note"] = "Some engineers are on more than one team. Their spend appears under each, so these rows sum to more than the total."
		}
	}

	writeJSON(w, http.StatusOK, body)
}

// handlePrices serves the rate table, so an unexpected cost can be traced to
// the rate that produced it rather than argued about.
func (a *API) handlePrices(w http.ResponseWriter, r *http.Request) {
	entries := a.prices.All()

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		row := map[string]any{
			"provider": e.Provider,
			"model":    e.Model,
			"scope":    string(e.Scope),
			"currency": e.Currency,
			"source":   e.Source,
			"region":   e.Region,
			"rates": map[string]string{
				"input":          e.Rates.Input.USDPerMillion(),
				"output":         e.Rates.Output.USDPerMillion(),
				"cache_read":     e.Rates.CacheRead.USDPerMillion(),
				"cache_write":    e.Rates.CacheWrite.USDPerMillion(),
				"cache_write_1h": e.Rates.CacheWrite1h.USDPerMillion(),
			},
		}
		if !e.AsOf.IsZero() {
			row["as_of"] = e.AsOf.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"prices": out,
		"count":  len(out),
		"unit":   "US dollars per million tokens",
	})
}

// filterFrom builds a request filter, enforcing what the caller may see.
//
// The visibility rule lives here, in one place, on the server. A developer sees
// their own traffic and nobody else's; an admin or manager sees the whole audit
// trail.
//
// Named per-user grouping exists (README, Principles), but it is not reachable from
// here: this function decides which *rows* a
// caller may see, and the entitlement to see them broken down by named person
// is a separate check (`spendVisibility`). Keeping the two apart is what stops
// a future grouping parameter from inheriting a developer's own-rows filter and
// quietly returning everyone.
func (a *API) filterFrom(r *http.Request) (store.RequestFilter, error) {
	p := PrincipalFrom(r.Context())
	q := r.URL.Query()

	// The typed filters narrow and nothing more. They are read before the
	// scope is pinned and are ANDed with it, so no combination of them widens
	// what this caller may see: a developer who types a colleague's address
	// gets their own traffic filtered to nothing, never the colleague's.
	filter := store.RequestFilter{
		Model:  q.Get("model"),
		Tool:   q.Get("tool"),
		User:   q.Get("user"),
		Repo:   q.Get("repo"),
		Branch: q.Get("branch"),
		// Allocation narrows like everything else here. A developer who names a
		// cost center sees their own traffic within it and never anybody
		// else's, because this is read before the scope below is pinned and is
		// ANDed with it.
		CostCenter:  q.Get("cost_center"),
		Unallocated: q.Get("cost_center") == "none",
		// One group of the audit trail, which is how a row expands. An id
		// rather than a scope: it is ANDed with the scope pinned below, so a
		// developer who sends a colleague's group id gets nothing back.
		Group: q.Get("group"),
	}
	// "none" is the remainder rather than a line with that id, so it must not
	// also be sent as one - a filter matching both would return nothing and
	// look like an empty window.
	if filter.Unallocated {
		filter.CostCenter = ""
	}

	// An allowlist, and a 400 for anything outside it. Ignoring an outcome
	// nobody recognises would answer a narrowed question with the whole window
	// and label it as narrowed, which on an audit trail is the one wrong answer
	// that matters.
	if v := q.Get("outcome"); v != "" {
		if !store.ValidOutcome(v) {
			return store.RequestFilter{}, fmt.Errorf("outcome: %q is not one of %s", v, strings.Join(store.Outcomes, ", "))
		}
		filter.Outcome = v
	}

	if !seesEveryone(p.Role) || q.Get("scope") == "me" {
		filter.UserID = p.UserID
	}

	now := time.Now().UTC()
	var err error

	filter.Since = now.Add(-defaultWindow)
	if v := q.Get("since"); v != "" {
		if filter.Since, err = parseWhen(v, now); err != nil {
			return store.RequestFilter{}, fmt.Errorf("since: %w", err)
		}
	}
	filter.Until = now
	if v := q.Get("until"); v != "" {
		if filter.Until, err = parseWhen(v, now); err != nil {
			return store.RequestFilter{}, fmt.Errorf("until: %w", err)
		}
	}
	if !filter.Until.After(filter.Since) {
		return store.RequestFilter{}, errors.New("the time range ends before it starts")
	}

	return filter, nil
}

// seesEveryone reports whether a role may look beyond its own traffic.
func seesEveryone(role string) bool {
	return role == store.RoleAdmin || role == store.RoleManager
}

func scopeLabel(userID string) string {
	if userID == "" {
		return "organization"
	}
	return "self"
}

// parseWhen accepts an absolute RFC 3339 timestamp or an age like "24h" or
// "7d", which is what a dashboard actually asks for.
func parseWhen(v string, now time.Time) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, v); err == nil {
		return at.UTC(), nil
	}

	// "7d" is not a Go duration but is the natural way to ask for a week.
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a timestamp or an age like 24h or 7d", v)
	}
	if d < 0 {
		d = -d
	}
	return now.Add(-d), nil
}

func intParam(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: %q is not a positive whole number", name, v)
	}
	return n, nil
}

// renderRequest turns an audit row into JSON.
//
// Money crosses the wire twice: as exact integer nanodollars, which is what
// anything computing with it should read, and as a decimal string for display.
// It never crosses as a JSON number - a float64 cannot hold a nanodollar total
// exactly, and a cost report that changes when it is parsed is not a cost
// report.
func renderRequest(r store.LoggedRequest) map[string]any {
	out := map[string]any{
		"id":              r.ID,
		"group_id":        r.GroupID,
		"request_id":      r.RequestID,
		"started_at":      r.StartedAt.UTC().Format(time.RFC3339Nano),
		"duration_ms":     r.Duration.Milliseconds(),
		"surface":         r.Surface,
		"provider":        r.Provider,
		"requested_model": r.RequestedModel,
		"resolved_model":  r.ResolvedModel,
		"streaming":       r.Streaming,
		"status_code":     r.StatusCode,
		"decision":        r.Decision,
		"error_kind":      r.ErrorKind,
		"tool":            r.Tool,
		"user_email":      r.UserEmail,
		"tokens": map[string]int64{
			"input":          r.InputTokens,
			"output":         r.OutputTokens,
			"cache_read":     r.CacheReadTokens,
			"cache_write":    r.CacheWriteTokens,
			"cache_write_1h": r.CacheWrite1hTokens,
		},
		// Explicitly present and false rather than absent, so a reader of the
		// log never has to wonder whether a missing cost means free.
		"priced": r.Cost != nil,
	}

	if r.TimeToFirstToken > 0 {
		out["ttft_ms"] = float64(r.TimeToFirstToken.Microseconds()) / 1000
	}
	if r.SessionID != "" {
		out["session_id"] = r.SessionID
	}
	// The work the request paid for, read through its session. Present on the
	// row rather than left to a second call against /sessions, so a console
	// filtering by repository shows the repository it filtered on instead of a
	// dash for every request whose session fell outside that call's own page.
	if r.Repo != "" {
		out["repo"] = r.Repo
	}
	if r.Branch != "" {
		out["branch"] = r.Branch
	}
	if r.CostCenterCode != "" {
		out["cost_center"] = r.CostCenterCode
		// Said on the row rather than left to be inferred from a second call.
		// A line reached by a client's header and one derived from a rule are
		// answerable to different standards, and the audit trail is where
		// somebody goes to challenge a number.
		out["cost_center_stamped"] = r.CostCenterStamped
	}
	if r.WorkItemKey != "" {
		out["work_item"] = r.WorkItemKey
		out["rung"] = r.Rung
	}
	if r.BudgetState != "" {
		out["budget_state"] = r.BudgetState
	}
	// Whether a provider was invoked at all: zero for a request the gateway
	// answered on its own, one for anything that reached a model. It is the
	// test a budget counts a request by, so it is on every row.
	out["attempts"] = r.Attempts
	if r.WebSearches > 0 {
		out["web_searches"] = r.WebSearches
	}
	if r.Cost != nil {
		out["cost"] = renderCost(*r.Cost)
		out["cost_source"] = r.CostSource
	}
	return out
}

// renderCost renders a cost breakdown.
func renderCost(c pricing.Cost) map[string]any {
	return map[string]any{
		"total_nanos": int64(c.Total),
		"total_usd":   c.Total.USD(),
		"by_class": map[string]any{
			"input":       map[string]any{"nanos": int64(c.Input), "usd": c.Input.USD()},
			"output":      map[string]any{"nanos": int64(c.Output), "usd": c.Output.USD()},
			"cache_read":  map[string]any{"nanos": int64(c.CacheRead), "usd": c.CacheRead.USD()},
			"cache_write": map[string]any{"nanos": int64(c.CacheWrite), "usd": c.CacheWrite.USD()},
			"web_search":  map[string]any{"nanos": int64(c.WebSearch), "usd": c.WebSearch.USD()},
		},
	}
}

// renderTotals renders an aggregate.
//
// unpriced_requests travels beside the cost in every response, and the console
// shows it. An honest total with a stated gap beats a confident one that quietly
// omits what it could not price.
func renderTotals(t store.Totals) map[string]any {
	return map[string]any{
		"requests":          t.Requests,
		"error_requests":    t.ErrorRequests,
		"unpriced_requests": t.UnpricedRequests,
		"unpriced_billable": t.UnpricedBillable,
		"tokens": map[string]int64{
			"input":          t.InputTokens,
			"output":         t.OutputTokens,
			"cache_read":     t.CacheReadTokens,
			"cache_write":    t.CacheWriteTokens,
			"cache_write_1h": t.CacheWrite1hTokens,
		},
		// Searches made through the gateway's MCP tool, which are billed per
		// search and so are counted beside the tokens rather than in them.
		"web_searches": t.WebSearches,
		"cost":         renderCost(t.Cost),
	}
}

func renderBuckets(buckets []store.Bucket) []map[string]any {
	out := make([]map[string]any, 0, len(buckets))
	for _, b := range buckets {
		row := renderTotals(b.Totals)
		row["key"] = b.Key
		out = append(out, row)
	}
	return out
}

// fail records the cause server-side and tells the client only what it can act
// on: a request id to quote.
func (a *API) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.log.ErrorContext(r.Context(), what,
		"error", err,
		"request_id", RequestIDFrom(r.Context()),
		"path", r.URL.Path,
	)
	writeAPIError(w, r, http.StatusInternalServerError,
		"Panorama could not complete that request. Quote the request id if you report it.")
}

// authenticated wraps a console handler so it only runs for a signed-in
// browser or a valid gateway token.
//
// A setup principal is refused here, so every route is closed to it by
// default and only the ones that name store.RoleSetup in requireRole open.
// The alternative - letting it through and checking later - would mean every
// handler added afterwards is reachable by a credential that was never meant to
// see it, which is exactly the failure requireRole exists to prevent.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return s.authenticatedAllowing(false, next)
}

func (s *Server) authenticatedAllowing(setup bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := s.authenticateRequest(w, r)
		if err != nil {
			s.rejectCredential(w, r, err)
			return
		}
		if principal.IsSetup() && !setup {
			s.log.WarnContext(r.Context(), "console request refused",
				"reason", "setup session outside deployment configuration",
				"path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusForbidden,
				"A setup session may only configure this deployment. Finish setting it up, then sign in normally.")
			return
		}
		// A service token is refused here rather than by a role list, because
		// there is no route it should ever reach and a role list only refuses
		// what it does not name. It is a gateway credential: it buys model
		// calls, it acts as a team rather than a person, and half this API
		// would read a user id it does not have.
		//
		// Refusing it in the one place every console route passes through is
		// what makes a handler added tomorrow closed by default. RoleSetup
		// cannot follow this pattern - it has routes it must reach - which is
		// why the two are checked separately rather than folded together.
		if principal.IsService() {
			s.log.WarnContext(r.Context(), "console request refused",
				"reason", "service token outside the gateway",
				"teamId", principal.TeamID,
				"path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
			writeAPIError(w, r, http.StatusForbidden,
				"A service token may only call the gateway. Sign in to use the console.")
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
	})
}

// requireRole wraps a handler so it only runs for a role that may do the thing.
//
// Every write in the console API goes through here. Checking the role in the
// handler would work exactly as well right up until the day someone adds a
// handler and forgets.
//
// Naming store.RoleSetup both admits the setup principal past
// authentication and allows its role, so a route's whole intent is one list.
func (s *Server) requireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, role := range roles {
		allowed[role] = true
	}

	return func(next http.Handler) http.Handler {
		return s.authenticatedAllowing(allowed[store.RoleSetup], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil || !allowed[p.Role] {
				s.log.WarnContext(r.Context(), "console request refused",
					"reason", "role", "path", r.URL.Path,
					"request_id", RequestIDFrom(r.Context()))
				writeAPIError(w, r, http.StatusForbidden,
					"Your role does not allow that. Ask a Panorama administrator.")
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// rejectCredential answers a failed authentication.
//
// A rejected credential and a broken database are different problems, logged
// differently and answered differently - but the client is told nothing that
// helps it guess which.
func (s *Server) rejectCredential(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errCSRF):
		// Deliberately not 401. The credential was fine; it is the request that
		// was not, and answering 401 would send a signed-in console to the
		// sign-in page over what is usually a stale tab.
		s.log.WarnContext(r.Context(), "console request refused",
			"reason", "csrf", "path", r.URL.Path,
			"request_id", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusForbidden,
			"That request could not be verified. Reload the console and try again.")

	case errors.Is(err, errNoCredential):
		writeAPIError(w, r, http.StatusUnauthorized, "Sign in to Panorama.")

	case errors.Is(err, store.ErrSetupRejected):
		s.log.WarnContext(r.Context(), "setup session rejected",
			"reason", "unknown, expired, or revoked",
			"request_id", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusUnauthorized,
			"That setup session has ended. Issue another with `panorama admin setup`.")

	case errors.Is(err, store.ErrSessionRejected):
		s.log.WarnContext(r.Context(), "console session rejected",
			"reason", "unknown, expired, revoked, or user disabled",
			"request_id", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusUnauthorized, "Your session has ended. Sign in again.")

	case errors.Is(err, store.ErrTokenRejected):
		s.log.WarnContext(r.Context(), "console token rejected",
			"reason", "unknown, revoked, expired, or user disabled",
			"request_id", RequestIDFrom(r.Context()))
		writeAPIError(w, r, http.StatusUnauthorized,
			"That token is not valid. It may have been revoked or expired.")

	default:
		s.log.ErrorContext(r.Context(), "authenticating console request", "error", err)
		writeAPIError(w, r, http.StatusServiceUnavailable,
			"Panorama could not verify your credentials right now.")
	}
}

// writeAPIError answers with a console-shaped error.
func writeAPIError(w http.ResponseWriter, r *http.Request, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error":      message,
		"request_id": RequestIDFrom(r.Context()),
	})
}
