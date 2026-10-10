// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dnxlabs/panorama/internal/costcenters"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/store"
)

// The cost center surface: the chart of accounts, the rules that fill it, and
// what a window actually landed where.
//
// Two things about the shape are deliberate.
//
// Reading is for leaders and writing is for administrators, because a chart of
// accounts is somebody's finance data and a manager reading their own team's
// line has a use for it; a developer does not, and is served none of it. The
// role check is the server's - the sidebar hiding a screen is not what enforces
// this.
//
// And the gap is returned by the same handler as the totals, never by a second
// call somebody could forget to make. Unallocated spend is a row of the answer
// here, with its own reasons and the repositories behind them, because a client
// that had to ask twice would eventually render one number without the other.

// handleCostCenters returns the chart of accounts with its rules.
//
// The rules travel with the lines rather than under their own endpoint: an
// ordered list is only readable as a whole - which rule wins is a property of
// the sequence, not of any row in it - and two calls could return a list and an
// order taken a keystroke apart.
func (a *API) handleCostCenters(w http.ResponseWriter, r *http.Request) {
	centers, err := a.db.ListCostCenters(r.Context(), true)
	if err != nil {
		a.fail(w, r, "listing cost centers", err)
		return
	}
	rules, err := a.db.ListCostCenterRules(r.Context())
	if err != nil {
		a.fail(w, r, "listing allocation rules", err)
		return
	}

	resolver := make([]costcenters.Rule, len(rules))
	for i, rule := range rules {
		resolver[i] = rule.Rule
	}
	// Reported rather than refused, and reported against the list as a whole.
	// A rule stranded under a catch-all is legal, saves cleanly and does
	// nothing, which is exactly the failure somebody needs told about rather
	// than prevented from creating.
	stranded := map[int]bool{}
	for _, i := range costcenters.Unreachable(resolver) {
		stranded[i] = true
	}
	// The weaker claim, kept separate from the exact one. A repository
	// catch-all does not make a later ticket rule impossible, only very nearly
	// so, and saying "never matches" about it would be a warning somebody
	// eventually catches out and thereafter ignores.
	shadowed := map[int]bool{}
	for _, i := range costcenters.Shadowed(resolver) {
		shadowed[i] = true
	}

	out := make([]map[string]any, 0, len(rules))
	for i, rule := range rules {
		out = append(out, map[string]any{
			"id":             rule.ID,
			"cost_center_id": rule.CostCenterID,
			"position":       rule.Position,
			"match_on":       string(rule.Match),
			"pattern":        rule.Pattern,
			"unreachable":    stranded[i],
			"shadowed":       shadowed[i] && !stranded[i],
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cost_centers": renderCostCenters(centers),
		"rules":        out,
		// Said on the wire rather than assumed by each client: allocation
		// warns, and the thing that stops an account is a budget on a person or
		// a team. A console that implied otherwise would be promising a control
		// this product deliberately does not have.
		"enforced": false,
	})
}

func renderCostCenters(centers []store.CostCenter) []map[string]any {
	out := make([]map[string]any, 0, len(centers))
	for _, c := range centers {
		row := map[string]any{
			"id":              c.ID,
			"code":            c.Code,
			"name":            c.Name,
			"owner":           c.Owner,
			"description":     c.Description,
			"warn_at_percent": c.WarnAtPercent,
			"accepts_stamp":   c.AcceptsStamp,
			"archived":        c.Archived,
			// Null rather than zero where nobody has set one. Zero is a line
			// meant to spend nothing, which is a different statement.
			"plan_nanos": c.PlanNanos,
		}
		out = append(out, row)
	}
	return out
}

type costCenterRequest struct {
	Code          string        `json:"code"`
	Name          string        `json:"name"`
	Owner         string        `json:"owner"`
	Description   string        `json:"description"`
	Plan          policy.Amount `json:"plan"`
	WarnAtPercent *int          `json:"warn_at_percent"`
	AcceptsStamp  *bool         `json:"accepts_stamp"`
	Archived      *bool         `json:"archived"`
}

// apply folds a request onto a line, leaving anything absent as it was.
func (req costCenterRequest) apply(c store.CostCenter) (store.CostCenter, error) {
	c.Code = strings.TrimSpace(req.Code)
	c.Name = strings.TrimSpace(req.Name)
	c.Owner = strings.TrimSpace(req.Owner)
	c.Description = req.Description

	if req.Plan == "" {
		c.PlanNanos = nil
	} else {
		money, err := req.Plan.Money()
		if err != nil {
			return c, err
		}
		v := int64(money)
		c.PlanNanos = &v
	}

	c.WarnAtPercent = 80
	if req.WarnAtPercent != nil {
		c.WarnAtPercent = *req.WarnAtPercent
	}
	c.AcceptsStamp = true
	if req.AcceptsStamp != nil {
		c.AcceptsStamp = *req.AcceptsStamp
	}
	if req.Archived != nil {
		c.Archived = *req.Archived
	}
	return c, nil
}

func (a *API) handleSaveCostCenter(w http.ResponseWriter, r *http.Request) {
	var req costCenterRequest
	if !decodeBody(w, r, &req) {
		return
	}

	center, err := req.apply(store.CostCenter{ID: r.PathValue("id")})
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	id, err := a.db.SaveCostCenter(r.Context(), center)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeAPIError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "There is no cost center with that id.")
			return
		}
		a.fail(w, r, "saving the cost center", err)
		return
	}

	// The snapshot carries which lines may be stamped, so a line created,
	// archived or switched to refuse stamps has to reach the gateway before the
	// next request rather than at the next reload. Same reload every other
	// write path performs, for the same reason.
	if lerr := a.policy.Load(r.Context()); lerr != nil {
		a.fail(w, r, "reloading policy", lerr)
		return
	}

	a.log.InfoContext(r.Context(), "cost center saved",
		"cost_center", id, "code", center.Code, "actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (a *API) handleDeleteCostCenter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	removed, err := a.db.DeleteCostCenter(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrCostCenterInUse) {
			// 409 rather than 400: nothing about the request is malformed, and
			// the same request succeeds once the line holds no spend. The
			// message names the way out, because archiving is the operation
			// somebody actually wants here.
			writeAPIError(w, r, http.StatusConflict,
				"Spend is already recorded against this cost center. Archive it instead - "+
					"an archived line keeps its history and its rules, and takes no new ones.")
			return
		}
		a.fail(w, r, "removing the cost center", err)
		return
	}
	if !removed {
		writeAPIError(w, r, http.StatusNotFound, "There is no cost center with that id.")
		return
	}

	if lerr := a.policy.Load(r.Context()); lerr != nil {
		a.fail(w, r, "reloading policy", lerr)
		return
	}

	a.log.InfoContext(r.Context(), "cost center removed",
		"cost_center", id, "actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "removed": true})
}

type ruleRequest struct {
	ID           string `json:"id"`
	CostCenterID string `json:"cost_center_id"`
	MatchOn      string `json:"match_on"`
	Pattern      string `json:"pattern"`
	Position     int    `json:"position"`
}

func (a *API) handleSaveCostCenterRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if !decodeBody(w, r, &req) {
		return
	}

	id, err := a.db.SaveCostCenterRule(r.Context(), costcenters.Rule{
		ID:           req.ID,
		CostCenterID: req.CostCenterID,
		Position:     req.Position,
		Match:        costcenters.Match(req.MatchOn),
		Pattern:      strings.TrimSpace(req.Pattern),
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeAPIError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "There is no cost center with that id.")
			return
		}
		a.fail(w, r, "saving the allocation rule", err)
		return
	}

	a.log.InfoContext(r.Context(), "allocation rule saved",
		"rule", id, "match_on", req.MatchOn, "pattern", req.Pattern,
		"actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (a *API) handleDeleteCostCenterRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	removed, err := a.db.DeleteCostCenterRule(r.Context(), id)
	if err != nil {
		a.fail(w, r, "removing the allocation rule", err)
		return
	}
	if !removed {
		writeAPIError(w, r, http.StatusNotFound, "There is no allocation rule with that id.")
		return
	}

	a.log.InfoContext(r.Context(), "allocation rule removed",
		"rule", id, "actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "removed": true})
}

type orderRequest struct {
	IDs []string `json:"ids"`
}

// handleOrderCostCenterRules writes a whole new evaluation order.
//
// The list entire, like a team's roster and for the same reason: translating a
// drag into a stream of per-rule updates leaves a half-applied order on the
// first failure, and a half-applied order allocates money nobody chose.
func (a *API) handleOrderCostCenterRules(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := a.db.ReorderCostCenterRules(r.Context(), req.IDs); err != nil {
		a.fail(w, r, "reordering the allocation rules", err)
		return
	}

	a.log.InfoContext(r.Context(), "allocation rules reordered",
		"rules", len(req.IDs), "actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"ordered": len(req.IDs)})
}

// handleReallocate restates every allocation from the current rules.
//
// Every rule write already does this inside its own transaction, so this exists
// for the cases a rule write does not cover: a database restored from a backup
// taken mid-edit, an operator who has just changed rules through SQL, or simply
// somebody who wants to be certain before signing off a period. It is
// idempotent, and it reports how many work items moved so that "nothing
// changed" is an answer rather than an absence of one.
func (a *API) handleReallocate(w http.ResponseWriter, r *http.Request) {
	moved, err := a.db.Reallocate(r.Context())
	if err != nil {
		a.fail(w, r, "reallocating recorded work", err)
		return
	}

	a.log.InfoContext(r.Context(), "work reallocated",
		"moved", moved, "actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"moved": moved})
}

type allocationTestRequest struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	Repo string `json:"repo"`
}

// handleTestAllocation answers "which rule claims this?" before anybody finds
// out from a restated report.
//
// This is the price of an ordered rule list, paid rather than argued away. It
// calls the same resolver the recompute calls - a second implementation here,
// however small, would eventually promise an allocation the recompute did not
// deliver.
func (a *API) handleTestAllocation(w http.ResponseWriter, r *http.Request) {
	var req allocationTestRequest
	if !decodeBody(w, r, &req) {
		return
	}

	rules, err := a.db.ListCostCenterRules(r.Context())
	if err != nil {
		a.fail(w, r, "listing allocation rules", err)
		return
	}
	resolver := make([]costcenters.Rule, len(rules))
	for i, rule := range rules {
		resolver[i] = rule.Rule
	}

	kind := req.Kind
	if kind == "" && req.Key != "" {
		kind = costcenters.KindTicket
	}

	matched, ok := costcenters.Resolve(resolver, costcenters.Subject{
		Kind: kind,
		Key:  strings.TrimSpace(req.Key),
		Repo: strings.TrimSpace(req.Repo),
	})
	if !ok {
		// Not an error, and said in the words the product uses for it. This is
		// the answer a worklist entry is made of.
		writeJSON(w, http.StatusOK, map[string]any{"allocated": false})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"allocated":      true,
		"cost_center_id": matched.CostCenterID,
		"rule_id":        matched.ID,
		"match_on":       string(matched.Match),
		"pattern":        matched.Pattern,
	})
}
