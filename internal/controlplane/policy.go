// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// handlePolicy serves the routing rules and budgets in force.
//
// # Where the line is
//
// A budget is the one place in this product where a number attaches to a named
// individual, so this endpoint is where the second trust commitment has to be
// made concrete rather than restated.
//
// The rule it implements: a person may see the spend of any budget that governs
// them - their own, their teams', the organization's. An administrator may
// additionally see that a per-user budget exists and what its limits are,
// because otherwise nobody could administer them. Nobody, at any role, receives
// another named person's spend from this API. That is what would turn a budget
// list into a leaderboard, and no amount of "but an admin needs it" makes it a
// thing this product does.
func (a *API) handlePolicy(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	set := a.policy.Snapshot()
	teams := set.Teams[p.UserID]
	privileged := seesEveryone(p.Role)

	// Team names, so a team budget reads as "team · Payments" rather than
	// leaving a reader to work out which team an opaque id is. Deliberately
	// only for teams: the equivalent for a user-scoped budget would put a
	// named colleague beside a limit, and the budget's own name is what an
	// administrator gave it for exactly that reason.
	teamNames, err := a.teamNames(r)
	if err != nil {
		a.fail(w, r, "reading teams", err)
		return
	}

	// Names for the people a per-user budget governs, read only for a caller
	// entitled to administer them.
	userNames := map[string]string{}
	if privileged {
		users, uerr := a.db.ListUsers(r.Context())
		if uerr != nil {
			a.fail(w, r, "reading people", uerr)
			return
		}
		for _, u := range users {
			userNames[u.ID] = u.DisplayName
			if u.DisplayName == "" {
				userNames[u.ID] = u.Email
			}
		}
	}

	budgets := make([]map[string]any, 0, len(set.Budgets))
	for _, s := range a.policy.Status() {
		var row map[string]any
		switch {
		case s.Budget.Governs(p.UserID, teams):
			// Their own standing, in full. This is the number the console shows
			// a developer so they are not surprised by a block.
			row = renderBudget(s, true)

		case privileged && s.Budget.Scope != policy.ScopeUser:
			// Org and team budgets are aggregates by construction - a team is
			// the smallest unit a leader is shown.
			row = renderBudget(s, true)

		case privileged:
			// Someone else's personal budget: the limits, so it can be
			// administered, and none of the spend.
			row = renderBudget(s, false)

		default:
			continue
		}

		if s.Budget.Scope == policy.ScopeTeam {
			if name, ok := teamNames[s.Budget.ScopeID]; ok {
				row["scope_label"] = name
			}
		}
		if privileged {
			// Who the budget governs, for the person who administers it. A
			// name beside a limit, never beside a spend figure: the spend half
			// of this row is still withheld for somebody else's personal
			// budget, which is the commitment. Not knowing whose limit it is
			// would make a per-user budget impossible to raise, which is the
			// one thing an administrator is here to do.
			row["scope_id"] = s.Budget.ScopeID
			if s.Budget.Scope == policy.ScopeUser {
				if name, ok := userNames[s.Budget.ScopeID]; ok {
					row["scope_label"] = name
				}
			}
		}
		budgets = append(budgets, row)
	}

	out := map[string]any{
		"budgets": budgets,
		// So the console can say why it is showing what it is showing, rather
		// than looking like it lost some rows.
		"scope": scopeLabel(map[bool]string{true: "", false: p.UserID}[privileged]),
	}

	// Whether this caller may change a budget. The same permission as changing
	// a route, because both decide what a deployment spends - but a different
	// screen, because a limit belongs to the person or team it governs.
	out["can_edit"] = p.Role == store.RoleAdmin

	writeJSON(w, http.StatusOK, out)
}

// teamNames maps team ids onto the names a human recognises.
func (a *API) teamNames(r *http.Request) (map[string]string, error) {
	list, err := a.db.ListTeams(r.Context())
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(list))
	for _, team := range list {
		names[team.ID] = team.Name
	}
	return names, nil
}

// renderBudget turns a budget's standing into JSON.
//
// withSpend is false when the caller may know the budget exists but not what
// has been spent against it. The keys are simply absent in that case rather
// than zeroed: a zero would read as "they have spent nothing", which is a claim
// this response is deliberately not making.
func renderBudget(s policy.Status, withSpend bool) map[string]any {
	out := map[string]any{
		"id":      s.Budget.ID,
		"scope":   s.Budget.Scope,
		"period":  string(s.Budget.Period),
		"name":    s.Budget.Label(),
		"enabled": s.Budget.Enabled,
		"limits": map[string]any{
			"soft":     nanosOrNull(s.Budget.SoftLimit),
			"hard":     nanosOrNull(s.Budget.HardLimit),
			"soft_usd": usdOrNull(s.Budget.SoftLimit),
			"hard_usd": usdOrNull(s.Budget.HardLimit),
		},
		"window_start": s.WindowStart.Format(time.RFC3339),
		"window_end":   s.WindowEnd.Format(time.RFC3339),
	}

	if !withSpend {
		out["spend_visible"] = false
		return out
	}

	out["spend_visible"] = true
	out["state"] = string(s.State)
	out["spent_nanos"] = int64(s.Spend.Cost)
	out["spent_usd"] = s.Spend.Cost.USD()
	out["requests"] = s.Spend.Requests
	// Beside every figure, as everywhere else: what is missing from it.
	out["unpriced_requests"] = s.Spend.Unpriced
	if s.Percent >= 0 {
		out["percent_of_hard_limit"] = s.Percent
	}
	return out
}

func nanosOrNull(m *pricing.Money) any {
	if m == nil {
		return nil
	}
	return int64(*m)
}

func usdOrNull(m *pricing.Money) any {
	if m == nil {
		return nil
	}
	return m.USD()
}

// budgetRequest is a budget as the console sends it.
//
// Money arrives as text and stays text until it is parsed into nanodollars,
// because JSON turns 800.00 into a float64 that is not 800.00 and a budget a
// rounding error away from what somebody typed is a budget that gets argued
// with. Amount is the same type the policy document uses, so the console and a
// file are parsed by one function.
type budgetRequest struct {
	Scope string `json:"scope"`
	// ScopeID is the user id or the team id this budget governs, and empty for
	// the organization. Ids rather than emails and slugs: the console picks
	// from a list it was served, unlike a file somebody types.
	ScopeID string `json:"scope_id"`
	Period  string `json:"period"`
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`

	SoftLimit policy.Amount `json:"soft_limit"`
	HardLimit policy.Amount `json:"hard_limit"`
}

// handleSaveBudget creates a budget or changes one.
//
// A budget is what a route reads and cannot change: this is the only place a
// limit is set, so raising one is one edit in one screen rather than a change
// that has to be made in two places and kept in step.
func (a *API) handleSaveBudget(w http.ResponseWriter, r *http.Request) {
	var req budgetRequest
	if !decodeBody(w, r, &req) {
		return
	}

	b := policy.Budget{
		Scope:   strings.TrimSpace(req.Scope),
		ScopeID: strings.TrimSpace(req.ScopeID),
		Period:  policy.Period(strings.TrimSpace(req.Period)),
		Name:    strings.TrimSpace(req.Name),
		Enabled: req.Enabled == nil || *req.Enabled,
	}

	for _, field := range []struct {
		name   string
		amount policy.Amount
		dst    **pricing.Money
	}{
		{"soft limit", req.SoftLimit, &b.SoftLimit},
		{"hard limit", req.HardLimit, &b.HardLimit},
	} {
		if !field.amount.Set() {
			continue
		}
		money, err := field.amount.Money()
		if err != nil {
			writeAPIError(w, r, http.StatusBadRequest,
				"The "+field.name+" is not an amount like 250.00.")
			return
		}
		*field.dst = &money
	}

	if err := b.Validate(); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	if msg, err := a.checkBudgetSubject(r.Context(), b); err != nil {
		a.fail(w, r, "checking the budget", err)
		return
	} else if msg != "" {
		writeAPIError(w, r, http.StatusBadRequest, msg)
		return
	}

	id, err := a.db.SaveBudget(r.Context(), b)
	if err != nil {
		a.fail(w, r, "saving the budget", err)
		return
	}

	// Reloaded rather than waited for. Somebody who has just raised a limit for
	// a blocked colleague should not have to explain a thirty-second wait.
	if lerr := a.policy.Load(r.Context()); lerr != nil {
		a.fail(w, r, "reloading policy", lerr)
		return
	}

	a.log.InfoContext(r.Context(), "budget saved",
		"budget", id, "scope", b.Scope, "period", b.Period,
		"actor", actorFor(PrincipalFrom(r.Context())))

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "saved": true})
}

// checkBudgetSubject refuses a budget on somebody who is not here.
//
// A budget whose subject does not exist enforces nothing and looks exactly like
// one that does, which is the worst combination available: an administrator
// believes a limit is in place and no request is ever measured against it.
func (a *API) checkBudgetSubject(ctx context.Context, b policy.Budget) (string, error) {
	switch b.Scope {
	case policy.ScopeUser:
		users, err := a.db.ListUsers(ctx)
		if err != nil {
			return "", err
		}
		for _, u := range users {
			if u.ID == b.ScopeID {
				return "", nil
			}
		}
		return "That person is not in this deployment.", nil

	case policy.ScopeTeam:
		teams, err := a.db.ListTeams(ctx)
		if err != nil {
			return "", err
		}
		for _, t := range teams {
			if t.ID == b.ScopeID {
				return "", nil
			}
		}
		return "That team does not exist.", nil
	}
	return "", nil
}

// handleDeleteBudget removes a budget. Nothing is capped by it from the next
// reload, which happens immediately.
func (a *API) handleDeleteBudget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	removed, err := a.db.DeleteBudget(r.Context(), id)
	if err != nil {
		a.fail(w, r, "removing the budget", err)
		return
	}
	if !removed {
		writeAPIError(w, r, http.StatusNotFound, "There is no budget with that id.")
		return
	}

	if lerr := a.policy.Load(r.Context()); lerr != nil {
		a.fail(w, r, "reloading policy", lerr)
		return
	}

	a.log.InfoContext(r.Context(), "budget removed",
		"budget", id, "actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "removed": true})
}
