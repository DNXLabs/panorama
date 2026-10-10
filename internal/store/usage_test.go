// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// seedRequests writes a small, deliberately mixed history: two users, two
// models, priced and unpriced requests, successes and a failure.
func seedRequests(t *testing.T, db *DB) (alice, bob *User, base time.Time) {
	t.Helper()
	ctx := t.Context()

	var err error
	if alice, err = db.CreateUser(ctx, "alice@example.com", "Alice", RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if bob, err = db.CreateUser(ctx, "bob@example.com", "Bob", RoleAdmin); err != nil {
		t.Fatal(err)
	}

	base = time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	cost := func(total int64) *pricing.Cost {
		return &pricing.Cost{
			Input:      pricing.Money(total / 4),
			Output:     pricing.Money(total / 4),
			CacheRead:  pricing.Money(total / 4),
			CacheWrite: pricing.Money(total / 4),
			Total:      pricing.Money(total),
		}
	}

	records := []RequestRecord{
		{
			RequestID: "r1", UserID: alice.ID, StartedAt: base,
			Surface: "anthropic", Provider: "bedrock",
			ResolvedModel: "us.anthropic.claude-sonnet-5", Tool: "claude-code",
			StatusCode: 200, Decision: DecisionAllowed,
			InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10_000, CacheWriteTokens: 400,
			CacheWrite1hTokens: 100,
			Cost:               cost(4_000_000), CostSource: pricing.SourceAWS,
		},
		{
			RequestID: "r2", UserID: alice.ID, StartedAt: base.Add(time.Minute),
			Surface: "anthropic", Provider: "bedrock",
			ResolvedModel: "us.anthropic.claude-sonnet-5", Tool: "claude-code",
			StatusCode: 200, Decision: DecisionAllowed,
			InputTokens: 200, OutputTokens: 80, CacheReadTokens: 20_000,
			Cost: cost(8_000_000), CostSource: pricing.SourceAWS,
		},
		{
			// Bob's, on another model, and unpriced: no rate was available.
			RequestID: "r3", UserID: bob.ID, StartedAt: base.Add(2 * time.Minute),
			Surface: "anthropic", Provider: "bedrock",
			ResolvedModel: "us.anthropic.claude-experimental", Tool: "codex-cli",
			StatusCode: 200, Decision: DecisionAllowed,
			InputTokens: 500, OutputTokens: 100,
		},
		{
			// A failure that never reached the model: no tokens, no cost, and
			// not a pricing gap either.
			RequestID: "r4", UserID: bob.ID, StartedAt: base.Add(3 * time.Minute),
			Surface: "anthropic", Provider: "bedrock",
			ResolvedModel: "us.anthropic.claude-sonnet-5", Tool: "claude-code",
			StatusCode: 429, Decision: DecisionError, ErrorKind: "throttled",
		},
	}

	for _, r := range records {
		if err := db.RecordRequest(ctx, r); err != nil {
			t.Fatalf("record %s: %v", r.RequestID, err)
		}
	}
	return alice, bob, base
}

func TestRecordRequestRoundTripsCost(t *testing.T) {
	db := migratedSQLite(t)
	seedRequests(t, db)

	page, err := db.ListRequests(t.Context(), RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]LoggedRequest{}
	for _, r := range page.Requests {
		byID[r.RequestID] = r
	}

	priced := byID["r1"]
	if priced.Cost == nil {
		t.Fatal("r1 lost its cost")
	}
	if priced.Cost.Total != 4_000_000 {
		t.Errorf("total = %d, want 4000000", priced.Cost.Total)
	}
	if priced.Cost.Input+priced.Cost.Output+priced.Cost.CacheRead+priced.Cost.CacheWrite != priced.Cost.Total {
		t.Error("the class breakdown does not add up to the stored total")
	}
	if priced.CostSource != pricing.SourceAWS {
		t.Errorf("CostSource = %q, want it recorded", priced.CostSource)
	}
	if priced.CacheWrite1hTokens != 100 {
		t.Errorf("CacheWrite1hTokens = %d, want 100", priced.CacheWrite1hTokens)
	}
	if priced.UserEmail != "alice@example.com" {
		t.Errorf("UserEmail = %q, want it joined from the user", priced.UserEmail)
	}

	// The unpriced request must come back as unknown, not as free.
	if unpriced := byID["r3"]; unpriced.Cost != nil {
		t.Errorf("r3 has a cost of %v; an unpriced request must read back as nil", unpriced.Cost)
	}
}

func TestListRequestsIsNewestFirst(t *testing.T) {
	db := migratedSQLite(t)
	seedRequests(t, db)

	page, err := db.ListRequests(t.Context(), RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 4 {
		t.Fatalf("got %d requests, want 4", len(page.Requests))
	}
	if page.Requests[0].RequestID != "r4" {
		t.Errorf("first row is %s, want the newest (r4)", page.Requests[0].RequestID)
	}
	if page.NextCursor != "" {
		t.Error("a complete page should not offer a cursor")
	}
}

func TestListRequestsPaginatesWithoutSkippingRows(t *testing.T) {
	eachDialect(t, testListRequestsPaginatesWithoutSkippingRows)
}

func testListRequestsPaginatesWithoutSkippingRows(t *testing.T, db *DB) {
	seedRequests(t, db)
	ctx := t.Context()

	var seen []string
	cursor := ""
	for range 10 {
		page, err := db.ListRequests(ctx, RequestFilter{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Requests {
			seen = append(seen, r.RequestID)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}

	want := []string{"r4", "r3", "r2", "r1"}
	if len(seen) != len(want) {
		t.Fatalf("paged through %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paged through %v, want %v", seen, want)
		}
	}
}

func TestListRequestsRejectsABadCursor(t *testing.T) {
	db := migratedSQLite(t)

	// Silently restarting from the top would make a paging client loop forever.
	if _, err := db.ListRequests(t.Context(), RequestFilter{Cursor: "not-a-cursor"}); err == nil {
		t.Error("a malformed cursor must be an error")
	}
}

func TestListRequestsFilters(t *testing.T) {
	db := migratedSQLite(t)
	alice, _, base := seedRequests(t, db)
	ctx := t.Context()

	page, err := db.ListRequests(ctx, RequestFilter{UserID: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 2 {
		t.Errorf("alice has %d requests, want 2", len(page.Requests))
	}
	for _, r := range page.Requests {
		if r.UserID != alice.ID {
			t.Errorf("another user's request leaked into a scoped query: %s", r.RequestID)
		}
	}

	page, err = db.ListRequests(ctx, RequestFilter{Tool: "codex-cli"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 1 || page.Requests[0].RequestID != "r3" {
		t.Errorf("tool filter returned %d rows, want just r3", len(page.Requests))
	}

	page, err = db.ListRequests(ctx, RequestFilter{Since: base.Add(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) != 2 {
		t.Errorf("time filter returned %d rows, want 2", len(page.Requests))
	}
}

// TestSummarizeCountsUnpricedSeparately is the honesty requirement in the
// aggregate: a total that quietly omitted the request it could not price would
// look complete and be wrong.
func TestSummarizeCountsUnpricedSeparately(t *testing.T) {
	eachDialect(t, testSummarizeCountsUnpricedSeparately)
}

func testSummarizeCountsUnpricedSeparately(t *testing.T, db *DB) {
	seedRequests(t, db)

	summary, err := db.Summarize(t.Context(), RequestFilter{})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	total := summary.Totals
	if total.Requests != 4 {
		t.Errorf("Requests = %d, want 4", total.Requests)
	}
	if total.ErrorRequests != 1 {
		t.Errorf("ErrorRequests = %d, want 1", total.ErrorRequests)
	}
	// Two rows have no cost: the unpriced model and the throttled failure.
	if total.UnpricedRequests != 2 {
		t.Errorf("UnpricedRequests = %d, want 2", total.UnpricedRequests)
	}
	// Only one of those actually spent tokens, and that is the one that
	// represents money the total is missing.
	if total.UnpricedBillable != 1 {
		t.Errorf("UnpricedBillable = %d, want 1 - a failure that used no tokens is not a pricing gap", total.UnpricedBillable)
	}

	if total.Cost.Total != 12_000_000 {
		t.Errorf("cost = %d, want 12000000", total.Cost.Total)
	}
	if total.CacheReadTokens != 30_000 {
		t.Errorf("cache reads = %d, want 30000", total.CacheReadTokens)
	}
	if total.CacheWrite1hTokens != 100 {
		t.Errorf("one-hour cache writes = %d, want 100", total.CacheWrite1hTokens)
	}
}

func TestSummarizeGroups(t *testing.T) {
	eachDialect(t, testSummarizeGroups)
}

func testSummarizeGroups(t *testing.T, db *DB) {
	seedRequests(t, db)

	summary, err := db.Summarize(t.Context(), RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}

	// Keyed by provider and model together: the same id on two providers is two
	// targets that bill apart.
	byModel := map[string]Totals{}
	for _, b := range summary.ByModel {
		byModel[b.Key] = b.Totals
	}
	if got := byModel["bedrock/us.anthropic.claude-sonnet-5"]; got.Requests != 3 {
		t.Errorf("sonnet requests = %d, want 3", got.Requests)
	}
	if got := byModel["bedrock/us.anthropic.claude-experimental"]; got.UnpricedBillable != 1 {
		t.Errorf("the unpriced model should report its gap: %+v", got)
	}

	byTool := map[string]Totals{}
	for _, b := range summary.ByTool {
		byTool[b.Key] = b.Totals
	}
	if got := byTool["claude-code"]; got.Requests != 3 {
		t.Errorf("claude-code requests = %d, want 3", got.Requests)
	}

	if len(summary.ByDay) == 0 {
		t.Error("ByDay is empty")
	}
	for _, day := range summary.ByDay {
		if len(day.Key) != 10 {
			t.Errorf("day key %q is not a YYYY-MM-DD date", day.Key)
		}
	}

	// Every breakdown must add up to the same total it was computed from.
	var modelled int64
	for _, b := range summary.ByModel {
		modelled += b.Requests
	}
	if modelled != summary.Totals.Requests {
		t.Errorf("the model breakdown sums to %d requests, the total says %d", modelled, summary.Totals.Requests)
	}
}

func TestSummarizeScopedToOneUser(t *testing.T) {
	db := migratedSQLite(t)
	alice, _, _ := seedRequests(t, db)

	summary, err := db.Summarize(t.Context(), RequestFilter{UserID: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Totals.Requests != 2 {
		t.Errorf("Requests = %d, want alice's 2", summary.Totals.Requests)
	}
	if summary.Totals.Cost.Total != 12_000_000 {
		t.Errorf("cost = %d, want alice's 12000000", summary.Totals.Cost.Total)
	}
	if summary.Totals.UnpricedBillable != 0 {
		t.Errorf("alice has no unpriced requests, got %d", summary.Totals.UnpricedBillable)
	}
}
