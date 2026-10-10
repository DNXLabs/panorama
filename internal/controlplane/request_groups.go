// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/dnxlabs/panorama/internal/store"
)

// handleRequestGroups serves the audit trail as the console reads it: one row
// per sitting rather than one per model call.
//
// The same filter as /requests, read by the same filterFrom, so a developer
// is scoped to their own traffic here exactly as there. Expanding a row is
// /requests with `group` set, which keeps the individual calls behind every
// total one click away and keeps the CSV export per request.
func (a *API) handleRequestGroups(w http.ResponseWriter, r *http.Request) {
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

	page, err := a.db.ListRequestGroups(r.Context(), filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeAPIError(w, r, http.StatusBadRequest, "That cursor is not valid.")
			return
		}
		a.fail(w, r, "listing request groups", err)
		return
	}

	groups := make([]map[string]any, 0, len(page.Groups))
	for _, g := range page.Groups {
		groups = append(groups, renderRequestGroup(g))
	}
	body := map[string]any{
		"groups":      groups,
		"next_cursor": page.NextCursor,
	}

	// With the first page only, for the reason handleRequests gives. Both
	// counts travel: the control divides groups, and the footer says how many
	// requests those groups hold, because "12 groups" alone would hide the
	// number an auditor checks against.
	if filter.Cursor == "" {
		groupCounts, gerr := a.db.CountGroupOutcomes(r.Context(), filter)
		if gerr != nil {
			a.fail(w, r, "counting request group outcomes", gerr)
			return
		}
		requestCounts, rerr := a.db.CountOutcomes(r.Context(), filter)
		if rerr != nil {
			a.fail(w, r, "counting request outcomes", rerr)
			return
		}
		body["outcomes"] = renderOutcomes(groupCounts)
		body["request_outcomes"] = renderOutcomes(requestCounts)
	}

	writeJSON(w, http.StatusOK, body)
}

func renderOutcomes(o store.RequestOutcomes) map[string]any {
	return map[string]any{
		"all":     o.All,
		"routed":  o.Routed,
		"blocked": o.Blocked,
		"error":   o.Errored,
	}
}

// leadFirst lists the names a group spans with the one that opened it first,
// then the ends of the range that are not it. The opening call names what the
// sitting was mostly about; the alphabetical first of a range does not.
func leadFirst(lead, low, high string) []string {
	out := []string{}
	for _, name := range []string{lead, low, high} {
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func renderRequestGroup(g store.RequestGroup) map[string]any {
	out := renderTotals(g.Totals)
	out["id"] = g.ID
	out["first_at"] = g.FirstAt.UTC().Format(time.RFC3339Nano)
	out["last_at"] = g.LastAt.UTC().Format(time.RFC3339Nano)
	out["user_email"] = g.UserEmail
	out["tool"] = g.Tool
	// The routes the model calls named, both ends when there were several and
	// how many there were, for the reason models are: a sitting that used the
	// fast model too spent on both.
	out["routes"] = leadFirst(g.LeadRoute, g.RouteLow, g.RouteHigh)
	out["route_count"] = g.Routes
	out["tool_calls"] = g.ToolCalls
	out["provider"] = g.Provider
	out["outcome"] = g.Outcome
	out["attempts"] = g.Attempts
	out["duration_ms"] = g.DurationMS
	out["decisions"] = map[string]int64{
		"refused":      g.Refused,
		"blocked_hard": g.BlockedHard,
		"blocked_soft": g.BlockedSoft,
		"warned":       g.Warned,
	}

	// Both ends when they differ, rather than one of them chosen: a route
	// re-pointed during a sitting served two models, and naming only one
	// would misstate what half the row paid for.
	out["models"] = leadFirst(g.LeadModel, g.ModelLow, g.ModelHigh)

	if g.SessionID != "" {
		out["session_id"] = g.SessionID
	}
	if g.Repo != "" {
		out["repo"] = g.Repo
	}
	if g.Branch != "" {
		out["branch"] = g.Branch
	}
	if g.WorkItemKey != "" {
		out["work_item"] = g.WorkItemKey
		out["rung"] = g.Rung
	}
	if g.ErrorKind != "" {
		out["error_kind"] = g.ErrorKind
	}
	if g.StatusCode != 0 {
		out["status_code"] = g.StatusCode
	}
	return out
}
