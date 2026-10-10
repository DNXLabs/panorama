// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// handleWork serves spend rolled up against the work it paid for.
//
// This is the endpoint the product is named for. Two things about its shape are
// commitments rather than choices:
//
// Unattributed spend is a row, not an omission. The bucket whose key is empty
// is returned like any other, labelled, with its cost intact, and the coverage
// block says what share of the total was placed. An honest seventy per cent
// beats a false hundred, and the way to keep that true is to make the gap a
// value the caller has to render rather than a subtraction they might not do.
//
// Nothing here groups by user. Work items are the unit; a developer sees their
// own work, a leader sees the organization's, and neither can ask this endpoint
// which named person spent the most on anything.
func (a *API) handleWork(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	items, err := a.db.SummarizeWork(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "rolling up work items", err)
		return
	}
	repos, err := a.db.SummarizeRepos(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "rolling up repositories", err)
		return
	}
	coverage, err := a.db.Coverage(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "computing attribution coverage", err)
		return
	}
	unplaced, err := a.db.Unplaced(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "breaking down unplaced spend", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"since":         filter.Since.Format(time.RFC3339),
		"until":         filter.Until.Format(time.RFC3339),
		"scope":         scopeLabel(filter.UserID),
		"by_work_item":  renderWorkBuckets(items),
		"by_repository": renderWorkBuckets(repos),
		"coverage":      renderCoverage(coverage),
		// Why the coverage gap is a gap, as a partition of it rather than as a
		// note beside it. The rows sum to `coverage.cost_nanos -
		// coverage.attributed_cost_nanos`; a client that renders one figure
		// and not the other is rendering half of the product's central claim.
		"unplaced": renderUnplaced(unplaced),
	})
}

// renderUnplaced names each reason in the words the product uses for it.
//
// The store reports what it observed - no repository, no branch, no work item -
// and the label is chosen here, because a label is an interpretation and the
// interpretation belongs on the side of the wire that has to defend it. "No
// repository was seen" is a fact; "exploratory" is a reading of that fact, and
// it is a fair one but it is not something SQL established.
func renderUnplaced(rows []store.UnplacedSpend) []map[string]any {
	labels := map[string][2]string{
		store.UnplacedNoRepository: {
			"Exploratory sessions",
			"no repository open at the time of the request",
		},
		store.UnplacedNoBranch: {
			"Work outside a branch",
			"a repository, but no branch checked out",
		},
		store.UnplacedNoWorkItem: {
			"Branch matched no work item",
			"the branch name matched no work item",
		},
	}

	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		label := labels[u.Reason]
		out = append(out, map[string]any{
			"reason":     u.Reason,
			"label":      label[0],
			"detail":     label[1],
			"requests":   u.Requests,
			"sessions":   u.Sessions,
			"cost_nanos": u.Cost,
		})
	}
	return out
}

// handleSessions serves recent conversations and what each was attributed to.
//
// It is where the ladder becomes inspectable. A cost figure attached to
// PAY-1183 is only trustworthy if you can ask why, and the answer - "branch
// name, structural, high" - is a row in this response rather than a claim made
// by a screen.
func (a *API) handleSessions(w http.ResponseWriter, r *http.Request) {
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

	sessions, err := a.db.ListSessions(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "listing sessions", err)
		return
	}

	out := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, renderSession(s))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": out,
		"scope":    scopeLabel(filter.UserID),
	})
}

func renderSession(s store.Session) map[string]any {
	out := map[string]any{
		"id":         s.ID,
		"user_email": s.UserEmail,
		"tool":       s.Tool,
		// How the conversation was recognised, because an inferred grouping and
		// one the client stated are different levels of certainty and the
		// difference should not have to be inferred in turn.
		"method":       s.Method,
		"started_at":   s.StartedAt.UTC().Format(time.RFC3339),
		"last_seen_at": s.LastSeenAt.UTC().Format(time.RFC3339),
		"requests":     s.Requests,
		"repo":         s.Repo,
		"branch":       s.Branch,
		"attributed":   s.Attributed(),
		// The one-line form the console shows verbatim, so the phrasing of the
		// product's central claim lives in one place rather than in every
		// client that renders it.
		"describe": s.Describe(),
		// What the conversation cost. Integer nanodollars and a decimal string,
		// like every other money field - never a JSON number.
		"cost": renderMoney(pricing.Money(s.Cost)),
	}

	if s.Attributed() {
		out["work_item"] = map[string]any{
			"key":        s.WorkItemKey,
			"kind":       s.WorkItemKind,
			"rung":       s.Rung,
			"confidence": s.Confidence,
			"evidence":   s.Evidence,
		}
	}
	return out
}

func renderWorkBuckets(buckets []store.WorkBucket) []map[string]any {
	out := make([]map[string]any, 0, len(buckets))
	for _, b := range buckets {
		row := renderTotals(b.Totals)
		row["key"] = b.Key
		row["kind"] = b.Kind
		row["repo"] = b.Repo
		row["sessions"] = b.Sessions
		row["attributed"] = b.Attributed()
		// The attributed half of the group's spend. On a work-item row it equals
		// the total by definition; on a repository or a project it is the share
		// worth reading, and both come from the same query as the total so the
		// two can never be measured over different windows.
		row["attributed_cost_nanos"] = b.AttributedCost
		if b.MergedPRs > 0 {
			row["merged_prs"] = b.MergedPRs
		}
		if b.Rung != "" {
			row["rung"] = b.Rung
		}
		if !b.Attributed() {
			// Named in the payload rather than left to each client to invent a
			// label for. A row that some screens call "unattributed", some call
			// "other", and one forgets to render at all is how the honest number
			// stops being shown.
			row["label"] = "Unattributed"
		}
		// What a system of record said, when one was asked. Omitted rather than
		// sent empty, so a client can tell "GitHub says this is open" from
		// "nothing has been asked about this" - which is the difference between
		// a status and the absence of an integration.
		if b.Title != "" {
			row["title"] = b.Title
		}
		if b.Status != "" {
			row["status"] = b.Status
		}
		if b.Outcome != "" {
			row["outcome"] = b.Outcome
		}
		if b.URL != "" {
			row["url"] = b.URL
		}
		if !b.LastSeenAt.IsZero() {
			row["last_seen_at"] = b.LastSeenAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}

// renderCoverage states how much of the period could be placed, in both
// requests and money, alongside the other gap the product always declares.
func renderCoverage(c *store.AttributionCoverage) map[string]any {
	out := map[string]any{
		"requests":              c.Requests,
		"attributed_requests":   c.AttributedRequests,
		"unattributed_requests": c.Requests - c.AttributedRequests,
		"cost_nanos":            c.Cost,
		"attributed_cost_nanos": c.AttributedCost,
		// The two gaps travel together. Attribution coverage of 100% still
		// leaves a hole if some of that spend could not be priced, and a caller
		// reading one figure should not have to go looking for the other.
		"unpriced_requests": c.UnpricedRequests,
	}

	if c.Cost > 0 {
		out["attributed_cost_percent"] = c.AttributedCost * 100 / c.Cost
	}
	if c.Requests > 0 {
		out["attributed_requests_percent"] = c.AttributedRequests * 100 / c.Requests
	}
	return out
}
