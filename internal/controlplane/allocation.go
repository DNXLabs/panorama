// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"time"

	"github.com/dnxlabs/panorama/internal/store"
)

// handleAllocation answers where a window's money went, and states what it
// could not place.
//
// The gap is part of the answer rather than a second call. A client that had to
// ask twice would eventually render the allocated total on its own, and a
// fully-allocated-looking report with a quarter of the spend missing is the one
// output this feature exists to prevent.
//
// Two figures with different windows sit in one response, and they are labelled
// rather than reconciled. The table is the range the reader asked for; the plan
// comparison is always month to date, because a plan is a monthly figure and
// showing a week's spend as a share of it would read as a line coming in five
// times under budget.
func (a *API) handleAllocation(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	buckets, err := a.db.AllocationSpend(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "totalling allocated spend", err)
		return
	}
	gap, err := a.db.UnallocatedSpend(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "breaking down unallocated spend", err)
		return
	}
	rejected, err := a.db.RejectedStamps(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "counting rejected cost center stamps", err)
		return
	}
	centers, err := a.db.ListCostCenters(r.Context(), true)
	if err != nil {
		a.fail(w, r, "listing cost centers", err)
		return
	}

	// Month to date, on its own window, for the plan comparison alone.
	monthly := filter
	now := time.Now().UTC()
	monthly.Since = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthly.Until = now
	monthBuckets, err := a.db.AllocationSpend(r.Context(), monthly)
	if err != nil {
		a.fail(w, r, "totalling this month's allocated spend", err)
		return
	}
	monthByID := make(map[string]int64, len(monthBuckets))
	for _, b := range monthBuckets {
		monthByID[b.CostCenterID] = b.Cost
	}

	byID := make(map[string]store.AllocationBucket, len(buckets))
	var total, allocated, unallocated int64
	for _, b := range buckets {
		total += b.Cost
		if b.CostCenterID == "" {
			unallocated = b.Cost
			continue
		}
		allocated += b.Cost
		byID[b.CostCenterID] = b
	}

	lines := make([]map[string]any, 0, len(centers))
	for _, c := range centers {
		b := byID[c.ID]
		monthToDate := monthByID[c.ID]

		line := map[string]any{
			"id":              c.ID,
			"code":            c.Code,
			"name":            c.Name,
			"owner":           c.Owner,
			"archived":        c.Archived,
			"accepts_stamp":   c.AcceptsStamp,
			"cost_nanos":      b.Cost,
			"requests":        b.Requests,
			"sessions":        b.Sessions,
			"stamped_nanos":   b.Stamped,
			"plan_nanos":      c.PlanNanos,
			"month_to_date":   monthToDate,
			"warn_at_percent": c.WarnAtPercent,
		}
		// Only computed where a plan exists. A share of nothing is not zero
		// percent, and rendering it as zero would put every unplanned line at
		// the bottom of a sorted column looking comfortable.
		if c.PlanNanos != nil && *c.PlanNanos > 0 {
			line["plan_used_percent"] = float64(monthToDate) / float64(*c.PlanNanos) * 100
			line["over_warn"] = monthToDate*100 >= *c.PlanNanos*int64(c.WarnAtPercent)
		}
		lines = append(lines, line)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"lines": lines,
		"totals": map[string]any{
			"cost_nanos":        total,
			"allocated_nanos":   allocated,
			"unallocated_nanos": unallocated,
		},
		// The gap, partitioned. The rows sum to unallocated_nanos exactly; a
		// breakdown whose parts overlapped or fell short would be a second
		// number to reconcile against the first.
		"unallocated": renderUnallocated(gap),
		// Counted over the whole window rather than over the gap, because a
		// misspelt stamp on work a rule does claim is allocated - correctly -
		// and would never appear in the remainder. A deployment can have every
		// stamp wrong and a fully allocated report.
		"rejected_stamps": renderRejectedStamps(rejected),
		// Allocation warns. Nothing here stopped anybody, and a console that
		// implied otherwise would be promising a control that does not exist.
		"enforced": false,
	})
}

// renderUnallocated names each reason in the words the product uses for it.
//
// The store reported what it observed - a work item was reached, or it was
// not. Calling the first "no rule covers this yet" is an interpretation, and it
// belongs on the side of the wire that has to defend it.
func renderUnallocated(rows []store.UnallocatedWork) []map[string]any {
	labels := map[string]struct{ label, detail string }{
		store.UnallocatedNoRule: {
			"No rule covers this",
			"The work is known and no allocation rule claims it. One rule places it.",
		},
		store.UnallocatedNoWorkItem: {
			"Nothing to allocate against",
			"These conversations were never attributed to a repository or a ticket, " +
				"so there was nothing for a rule to match. See the attribution breakdown.",
		},
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		meta := labels[row.Reason]
		out = append(out, map[string]any{
			"reason":     row.Reason,
			"label":      meta.label,
			"detail":     meta.detail,
			"repo":       row.Repo,
			"requests":   row.Requests,
			"sessions":   row.Sessions,
			"cost_nanos": row.Cost,
			// Only the first kind is something anybody can act on here. The
			// second is an attribution problem wearing an allocation hat, and
			// offering "create a rule" against it would be offering a fix that
			// cannot work.
			"actionable": row.Reason == store.UnallocatedNoRule && row.Repo != "",
		})
	}
	return out
}

func renderRejectedStamps(rows []store.RejectedStamp) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"stamp":      row.Stamp,
			"requests":   row.Requests,
			"cost_nanos": row.Cost,
		})
	}
	return out
}
