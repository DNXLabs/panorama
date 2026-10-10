// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// exportRowCap bounds an export.
//
// A CSV endpoint that streams an unbounded audit trail is a way to make one
// request hold a database cursor for an hour. The cap is stated in the file
// itself when it bites, because a truncated export that does not say so is
// worse than no export.
const exportRowCap = 50_000

// mountExports registers the CSV downloads.
//
// Separate from the JSON API because the response is a file rather than a
// document, and because a browser fetches it by navigating - which means it
// carries the session cookie and nothing else. They are reads, so no CSRF token
// applies.
func (s *Server) mountExports(api *API) {
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/export/requests.csv":   api.handleExportRequests,
		"GET /api/v1/export/usage.csv":      api.handleExportUsage,
		"GET /api/v1/export/work.csv":       api.handleExportWork,
		"GET /api/v1/export/allocation.csv": api.handleExportAllocation,
	} {
		s.mux.Handle(pattern, s.authenticated(handler))
	}
}

// csvWriter starts a download and returns a writer for it.
func csvWriter(w http.ResponseWriter, name string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	// The file is data, not markup, and a browser that decided otherwise would
	// be running someone's branch name as HTML.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	return csv.NewWriter(w)
}

// handleExportRequests writes the audit trail as CSV.
//
// Metadata only, exactly as the API and the console show it. There is no column
// here for a prompt or a response because there is no such column anywhere, and
// an export is the place someone would most expect to find one.
func (a *API) handleExportRequests(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	filter.Limit = maxExportPage

	out := csvWriter(w, "panorama-requests.csv")
	defer out.Flush()

	// Costs are written as exact decimal strings rather than as numbers. A
	// spreadsheet that reads 0.000031 as a float and sums a column of them
	// disagrees with itself depending on the order; the nanodollar column beside
	// it is the one to compute with.
	header := []string{
		"started_at", "request_id", "user_email", "tool", "surface",
		"requested_model", "resolved_model", "decision", "status_code",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "cache_write_1h_tokens",
		"cost_nanos", "cost_usd", "priced", "session_id", "repo", "branch", "work_item", "rung",
		"cost_center", "cost_center_stamped",
		"budget_state", "attempts", "duration_ms",
		// Last, so a spreadsheet built on the columns before it still lines up.
		"web_searches",
	}
	if werr := out.Write(header); werr != nil {
		return
	}

	written := 0
	cursor := ""
	for written < exportRowCap {
		filter.Cursor = cursor
		page, perr := a.db.ListRequests(r.Context(), filter)
		if perr != nil {
			a.log.ErrorContext(r.Context(), "exporting requests", "error", perr)
			return
		}
		if len(page.Requests) == 0 {
			break
		}

		for _, req := range page.Requests {
			row := []string{
				req.StartedAt.UTC().Format(time.RFC3339), req.RequestID, req.UserEmail,
				req.Tool, req.Surface, req.RequestedModel, req.ResolvedModel,
				req.Decision, strconv.Itoa(req.StatusCode),
				strconv.FormatInt(req.InputTokens, 10), strconv.FormatInt(req.OutputTokens, 10),
				strconv.FormatInt(req.CacheReadTokens, 10), strconv.FormatInt(req.CacheWriteTokens, 10),
				strconv.FormatInt(req.CacheWrite1hTokens, 10),
				"", "", strconv.FormatBool(req.Cost != nil),
				req.SessionID, req.Repo, req.Branch, req.WorkItemKey, req.Rung,
				req.CostCenterCode, strconv.FormatBool(req.CostCenterStamped),
				req.BudgetState,
				strconv.Itoa(req.Attempts), strconv.FormatInt(req.Duration.Milliseconds(), 10),
				strconv.FormatInt(req.WebSearches, 10),
			}
			if req.Cost != nil {
				row[14] = strconv.FormatInt(int64(req.Cost.Total), 10)
				row[15] = req.Cost.Total.USD()
			}
			if werr := out.Write(row); werr != nil {
				return
			}
			written++
			if written >= exportRowCap {
				break
			}
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if written >= exportRowCap {
		// Said in the file rather than only in a header, because the header is
		// gone by the time anyone opens the spreadsheet.
		_ = out.Write([]string{"# truncated at " + strconv.Itoa(exportRowCap) +
			" rows; narrow the date range to export the rest"})
	}
}

// maxExportPage is the page size the export pulls in. Larger than the console's
// because nobody is looking at the latency.
const maxExportPage = 500

// handleExportUsage writes the aggregate view.
func (a *API) handleExportUsage(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	summary, err := a.db.Summarize(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "exporting usage", err)
		return
	}

	out := csvWriter(w, "panorama-usage.csv")
	defer out.Flush()

	if werr := out.Write([]string{
		"grouping", "key", "requests", "error_requests", "unpriced_requests",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
		"cost_nanos", "cost_usd",
	}); werr != nil {
		return
	}

	groups := []struct {
		name    string
		buckets []store.Bucket
	}{
		{"day", summary.ByDay},
		{"model", summary.ByModel},
		{"tool", summary.ByTool},
	}
	for _, g := range groups {
		for _, b := range g.buckets {
			if werr := out.Write(totalsRow(g.name, b.Key, b.Totals)); werr != nil {
				return
			}
		}
	}
	// The totals row is written last and labelled, so a reader who sorts the
	// file does not end up with it in the middle pretending to be a group.
	_ = out.Write(totalsRow("total", "", summary.Totals))
}

// handleExportWork writes spend against the work it paid for, unattributed
// remainder included.
func (a *API) handleExportWork(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	items, err := a.db.SummarizeWork(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "exporting work", err)
		return
	}

	out := csvWriter(w, "panorama-work.csv")
	defer out.Flush()

	if werr := out.Write([]string{
		"work_item", "kind", "repo", "rung", "sessions", "requests",
		"cost_nanos", "cost_usd", "unpriced_requests",
	}); werr != nil {
		return
	}

	for _, b := range items {
		key := b.Key
		if key == "" {
			// The unattributed row is exported like any other, and named. An
			// export that dropped it would be the false hundred per cent this
			// product exists not to publish.
			key = "(unattributed)"
		}
		if werr := out.Write([]string{
			key, b.Kind, b.Repo, b.Rung,
			strconv.FormatInt(b.Sessions, 10), strconv.FormatInt(b.Requests, 10),
			strconv.FormatInt(int64(b.Cost.Total), 10), b.Cost.Total.USD(),
			strconv.FormatInt(b.UnpricedRequests, 10),
		}); werr != nil {
			return
		}
	}
}

func totalsRow(grouping, key string, t store.Totals) []string {
	return []string{
		grouping, key,
		strconv.FormatInt(t.Requests, 10),
		strconv.FormatInt(t.ErrorRequests, 10),
		strconv.FormatInt(t.UnpricedRequests, 10),
		strconv.FormatInt(t.InputTokens, 10),
		strconv.FormatInt(t.OutputTokens, 10),
		strconv.FormatInt(t.CacheReadTokens, 10),
		strconv.FormatInt(t.CacheWriteTokens, 10),
		strconv.FormatInt(int64(t.Cost.Total), 10),
		t.Cost.Total.USD(),
	}
}

// handleExportAllocation writes the window by accounting line.
//
// This is the file that leaves the product. It is keyed on `code` - the string
// the finance system already uses - rather than on an id Panorama invented,
// because the join happens in somebody else's ledger and an id they have never
// seen is not a key.
//
// Three properties are load-bearing, and each is one somebody would notice only
// after posting the numbers:
//
//   - The unallocated remainder is a row, named, with its cost intact. An
//     export that dropped it would balance perfectly and be wrong by whatever
//     it left out, which is the failure this whole feature exists to prevent.
//   - `stamped_nanos` separates what a client asserted from what a rule derived
//     from evidence. Under challenge those two carry different weight, and a
//     single total cannot be defended twice.
//   - Costs are exact decimal strings beside their nanodollars, for the reason
//     every other export here does it: a column of floats sums differently
//     depending on its order.
func (a *API) handleExportAllocation(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	buckets, err := a.db.AllocationSpend(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "exporting allocation", err)
		return
	}
	centers, err := a.db.ListCostCenters(r.Context(), true)
	if err != nil {
		a.fail(w, r, "exporting allocation", err)
		return
	}
	byID := make(map[string]store.CostCenter, len(centers))
	for _, c := range centers {
		byID[c.ID] = c
	}

	out := csvWriter(w, "panorama-allocation.csv")
	defer out.Flush()

	if werr := out.Write([]string{
		"code", "name", "owner", "requests", "sessions",
		"cost_nanos", "cost_usd", "stamped_nanos", "stamped_usd", "plan_nanos",
	}); werr != nil {
		return
	}

	for _, b := range buckets {
		code, name, owner, plan := "(unallocated)", "No accounting line", "", ""
		if c, ok := byID[b.CostCenterID]; ok {
			code, name, owner = c.Code, c.Name, c.Owner
			if c.PlanNanos != nil {
				plan = strconv.FormatInt(*c.PlanNanos, 10)
			}
		}

		if werr := out.Write([]string{
			code, name, owner,
			strconv.FormatInt(b.Requests, 10),
			strconv.FormatInt(b.Sessions, 10),
			strconv.FormatInt(b.Cost, 10), pricing.Money(b.Cost).USD(),
			strconv.FormatInt(b.Stamped, 10), pricing.Money(b.Stamped).USD(),
			plan,
		}); werr != nil {
			return
		}
	}
}
