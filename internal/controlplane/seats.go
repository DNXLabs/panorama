// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// spendVisibility decides whose named numbers a caller may see.
//
// The single place that answer is computed, and the reason `filterFrom` is not
// where it lives. Those two questions look alike and are not: `filterFrom`
// decides which *rows* a caller may read, and this decides whether they may see
// those rows broken down by named person. Folding them together is how a
// grouping parameter added later would inherit a developer's own-rows filter
// and quietly return the whole organization.
//
// The rule (README, Principles):
//
//   - an administrator sees everyone;
//   - a manager sees members of the teams they lead - leading is a
//     `team_members.role` of 'lead', not merely belonging to the team;
//   - everybody else sees nobody, including themselves. Their own numbers are
//     on the personal screens, which is where an individual's spend belongs.
func (a *API) spendVisibility(r *http.Request) (store.Visibility, error) {
	p := PrincipalFrom(r.Context())
	if p == nil {
		return store.Visibility{}, nil
	}

	if p.Role == store.RoleAdmin {
		return store.Visibility{Everyone: true}, nil
	}
	if p.Role != store.RoleManager {
		return store.Visibility{}, nil
	}

	teams, err := a.db.LeadTeams(r.Context(), p.UserID)
	if err != nil {
		return store.Visibility{}, err
	}
	return store.Visibility{TeamIDs: teams}, nil
}

// handleSeats serves seat economics, the per-user table, and cache hygiene.
//
// One endpoint rather than three, because they are one screen and one date
// range, and three round trips would let the three figures disagree about which
// window they describe.
func (a *API) handleSeats(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Seat economics and cache hygiene are population figures - they name
	// nobody - so they are computed over the organization rather than over
	// whatever rows the caller may read. A developer opening this screen sees
	// the same cost-per-active-developer their manager does, and no names.
	orgFilter := filter
	orgFilter.UserID = ""

	seats, err := a.db.SeatEconomics(r.Context(), orgFilter)
	if err != nil {
		a.fail(w, r, "computing seat economics", err)
		return
	}

	hygiene, err := a.db.CacheHygieneByTool(r.Context(), orgFilter)
	if err != nil {
		a.fail(w, r, "computing cache hygiene", err)
		return
	}

	visibility, err := a.spendVisibility(r)
	if err != nil {
		a.fail(w, r, "resolving spend visibility", err)
		return
	}
	people, err := a.db.SpendByUser(r.Context(), orgFilter, visibility)
	if err != nil {
		a.fail(w, r, "computing spend by user", err)
		return
	}

	body := map[string]any{
		"since": filter.Since.Format(time.RFC3339),
		"until": filter.Until.Format(time.RFC3339),
		"seats": map[string]any{
			"provisioned": seats.Provisioned,
			"active":      seats.Active,
			"idle":        seats.Provisioned - seats.Active,
			// The distribution, so one outlier is visible without naming them.
			"cost_per_active": renderMoney(seats.CostPerActive),
			"p50":             renderMoney(seats.P50),
			"p90":             renderMoney(seats.P90),
			"max":             renderMoney(seats.Max),
			"total":           renderMoney(seats.Total),
		},
		"cache_hygiene": renderHygiene(hygiene),
		// Said in the payload rather than inferred from an empty list, so the
		// console can tell "nobody spent anything" from "you may not see this".
		"can_see_people": !visibility.SeesNobody(),
		"people":         renderUserSpend(people),
	}
	if visibility.SeesNobody() {
		body["people_note"] = "Per-person spend is for administrators and for managers of a person's teams. " +
			"Your own numbers are on My usage."
	}

	writeJSON(w, http.StatusOK, body)
}

func renderUserSpend(rows []store.UserSpend) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, map[string]any{
			"user_id":      u.UserID,
			"email":        u.Email,
			"display_name": u.DisplayName,
			"role":         u.Role,
			"requests":     u.Totals.Requests,
			"sessions":     u.Sessions,
			"cost":         renderMoney(u.Totals.Cost.Total),
			// What share of it reached a work item, from the same query as the
			// spend it is a share of.
			"attributed_cost": renderMoney(pricing.Money(u.AttributedCost)),
			// Beside every total, as it is everywhere else: a request whose
			// model had no rate is unpriced rather than free.
			"unpriced": u.Totals.UnpricedBillable,
		})
	}
	return out
}

// renderHygiene reports what each harness is doing with the prompt cache, and
// what the uncached ones are costing.
//
// The multiple is computed against what the same traffic would have cost with a
// typical cache hit rate, and it is deliberately not presented as an exact
// saving - the honest claim is "this is the order of what you are leaving on
// the table", not a figure to invoice against.
func renderHygiene(rows []store.CacheHygiene) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		row := map[string]any{
			"tool":              c.Tool,
			"requests":          c.Requests,
			"input_tokens":      c.InputTokens,
			"cache_read_tokens": c.CacheReadTokens,
			"cost":              renderMoney(c.Cost),
			"uncached":          c.Uncached(),
		}
		// The share of context that came from cache. The single number that
		// says whether a harness is configured correctly.
		if total := c.InputTokens + c.CacheReadTokens; total > 0 {
			row["cache_read_share"] = float64(c.CacheReadTokens) / float64(total)
		}
		out = append(out, row)
	}
	return out
}

// renderMoney carries an amount the way every other money field does: exact
// integer nanodollars to compute with, and a decimal string to display.
//
// Never a JSON number for the dollars - a float64 cannot hold a nanodollar
// total exactly, and a cost report that changes when it is parsed is not a cost
// report.
func renderMoney(m pricing.Money) map[string]any {
	return map[string]any{"nanos": int64(m), "usd": m.USD()}
}
