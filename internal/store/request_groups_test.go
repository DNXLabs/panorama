// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// groupingFixture records one sitting's worth of requests and remembers them
// by name, so a test can say which ones belong together.
type groupingFixture struct {
	t    *testing.T
	db   *DB
	base time.Time
	user string
}

func newGroupingFixture(t *testing.T, db *DB) *groupingFixture {
	t.Helper()
	user, err := db.CreateUser(t.Context(), "ana@example.com", "Ana", RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	return &groupingFixture{
		t: t, db: db, user: user.ID,
		// An hour and a half back, so every request is inside the default
		// window and the latest is still in the past. Microseconds rather than
		// whole seconds, the finest precision Postgres keeps, so the idle gap
		// is compared on fractional stamps.
		base: time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Microsecond),
	}
}

// call is one request, described by what the grouping key reads.
type call struct {
	name     string
	at       time.Duration // after the fixture's base
	session  string
	route    string
	decision string
	cost     int64
}

func (f *groupingFixture) record(calls ...call) {
	f.t.Helper()
	for _, c := range calls {
		rec := RequestRecord{
			RequestID: c.name, UserID: f.user, SessionID: c.session,
			StartedAt: f.base.Add(c.at), Surface: "anthropic", Provider: "bedrock",
			RequestedModel: c.route, ResolvedModel: "us.anthropic." + c.route,
			Tool: "claude-code", StatusCode: 200, Decision: c.decision,
			InputTokens: 10, OutputTokens: 5,
		}
		if c.route == toolCallRoute {
			// A search, as the MCP endpoint records one: no conversation of
			// its own, and a request to join the one it was made from.
			rec.Surface, rec.Provider, rec.ResolvedModel = surfaceTool, "bedrock-agentcore", "web-search"
			rec.SessionFromSitting, rec.InputTokens, rec.OutputTokens, rec.WebSearches = true, 0, 0, 1
		}
		if c.decision == DecisionAllowed {
			rec.Attempts = 1
			rec.Cost = &pricing.Cost{Input: pricing.Money(c.cost), Total: pricing.Money(c.cost)}
			rec.CostSource = pricing.SourceSeed
		}
		if err := f.db.RecordRequest(f.t.Context(), rec); err != nil {
			f.t.Fatalf("record %s: %v", c.name, err)
		}
	}
}

// partition reads back which requests share a group, as sorted lists of
// request names, so two groupings can be compared without caring about ids.
func (f *groupingFixture) partition() [][]string {
	f.t.Helper()
	page, err := f.db.ListRequests(f.t.Context(), RequestFilter{Limit: maxRequestPageSize})
	if err != nil {
		f.t.Fatalf("list: %v", err)
	}
	byGroup := map[string][]string{}
	for _, r := range page.Requests {
		if r.GroupID == "" {
			f.t.Errorf("request %s has no group", r.RequestID)
		}
		byGroup[r.GroupID] = append(byGroup[r.GroupID], r.RequestID)
	}
	out := make([][]string, 0, len(byGroup))
	for _, names := range byGroup {
		sort.Strings(names)
		out = append(out, names)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func samePartition(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.Join(a[i], ",") != strings.Join(b[i], ",") {
			return false
		}
	}
	return true
}

// toolCallRoute marks a call in the fixture as a search over MCP.
const toolCallRoute = "web_search"

// sitting is a conversation that exercises every part of the key: a gap
// inside the limit, one outside it, a second route, a tool call, a refusal, a
// second conversation beside the first, and a request recorded after the ones
// that started later than it did.
var sitting = []call{
	{"a1", 0, "s1", "claude-sonnet-5", DecisionAllowed, 100},
	{"a2", 10 * time.Minute, "s1", "claude-sonnet-5", DecisionAllowed, 200},
	// 29 minutes idle: still the same sitting.
	{"a3", 39 * time.Minute, "s1", "claude-sonnet-5", DecisionAllowed, 300},
	// Another route in the same conversation is the same sitting: Claude
	// Code's fast model, called on every turn.
	{"h1", 12 * time.Minute, "s1", "claude-haiku-4-5", DecisionAllowed, 10},
	// A search carries no conversation and joins the one its tool was in.
	{"w1", 12*time.Minute + 30*time.Second, "", toolCallRoute, DecisionAllowed, 7},
	// A refusal never joins a served total.
	{"r1", 11 * time.Minute, "s1", "claude-sonnet-5", DecisionRefused, 0},
	{"r2", 13 * time.Minute, "s1", "claude-sonnet-5", DecisionBlocked, 0},
	// A second conversation on the same route, at the same time.
	{"b1", 5 * time.Minute, "s2", "claude-sonnet-5", DecisionAllowed, 50},
	// 36 minutes after a3: a new sitting.
	{"a4", 75 * time.Minute, "s1", "claude-sonnet-5", DecisionAllowed, 400},
	// Started before a4 and recorded after it, as a long call would be.
	{"a5", 72 * time.Minute, "s1", "claude-sonnet-5", DecisionAllowed, 500},
}

var sittingGroups = [][]string{
	{"a1", "a2", "a3", "h1", "w1"},
	{"a4", "a5"},
	{"b1"},
	{"r1", "r2"},
}

func TestRequestsAreGroupedBySitting(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		f := newGroupingFixture(t, db)
		f.record(sitting...)

		if got := f.partition(); !samePartition(got, sittingGroups) {
			t.Errorf("groups = %v, want %v", got, sittingGroups)
		}
	})
}

func TestListRequestGroupsTotalsAndPages(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		f := newGroupingFixture(t, db)
		f.record(sitting...)

		page, err := db.ListRequestGroups(ctx, RequestFilter{})
		if err != nil {
			t.Fatalf("list groups: %v", err)
		}
		if len(page.Groups) != len(sittingGroups) {
			t.Fatalf("%d groups, want %d", len(page.Groups), len(sittingGroups))
		}

		// Most recently active first: the second sitting ended last.
		top := page.Groups[0]
		if top.Requests != 2 || top.Cost.Total != 900 {
			t.Errorf("top group = %d requests, %d nanos; want a4 and a5 at 900", top.Requests, top.Cost.Total)
		}
		if !top.FirstAt.Equal(f.base.Add(72*time.Minute)) || !top.LastAt.Equal(f.base.Add(75*time.Minute)) {
			t.Errorf("top group spans %s to %s, want 72m to 75m after base", top.FirstAt, top.LastAt)
		}
		if top.RouteLow != "claude-sonnet-5" || top.Routes != 1 || top.ToolCalls != 0 || top.Outcome != OutcomeRouted {
			t.Errorf("top group routes %q (%d), %d tool calls, outcome %q", top.RouteLow, top.Routes, top.ToolCalls, top.Outcome)
		}

		var first *RequestGroup
		for i := range page.Groups {
			if page.Groups[i].FirstAt.Equal(f.base) {
				first = &page.Groups[i]
			}
		}
		if first == nil {
			t.Fatal("no group for the first sitting")
		}
		if first.Routes != 2 || first.RouteLow != "claude-haiku-4-5" || first.RouteHigh != "claude-sonnet-5" ||
			first.ToolCalls != 1 || first.WebSearches != 1 || first.SessionID != "s1" {
			t.Errorf("first sitting = routes %q..%q (%d), %d tool calls, %d searches; want both routes and the search",
				first.RouteLow, first.RouteHigh, first.Routes, first.ToolCalls, first.WebSearches)
		}
		if first.ModelLow != "us.anthropic.claude-haiku-4-5" || first.ModelHigh != "us.anthropic.claude-sonnet-5" {
			t.Errorf("the search's model leaked into the sitting's models: %q..%q", first.ModelLow, first.ModelHigh)
		}

		var refusals *RequestGroup
		for i := range page.Groups {
			if page.Groups[i].Outcome == OutcomeBlocked {
				refusals = &page.Groups[i]
			}
		}
		if refusals == nil {
			t.Fatal("no blocked group")
		}
		if refusals.Refused != 1 || refusals.BlockedHard != 1 || refusals.Attempts != 0 {
			t.Errorf("blocked group = refused %d, hard %d, attempts %d; want 1, 1, 0",
				refusals.Refused, refusals.BlockedHard, refusals.Attempts)
		}

		// The totals are the window's: a window that cuts the first sitting
		// in half reports the half inside it.
		cut, err := db.ListRequestGroups(ctx, RequestFilter{
			Since: f.base.Add(5 * time.Minute), Until: f.base.Add(20 * time.Minute),
			Outcome: OutcomeRouted, Tool: "claude-code",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range cut.Groups {
			if g.SessionID == "s1" && (g.Requests != 3 || g.Cost.Total != 217) {
				t.Errorf("the cut sitting reports %d requests at %d nanos, want a2, h1 and w1 at 217", g.Requests, g.Cost.Total)
			}
		}

		// Expanding a group is the request log narrowed to it.
		inside, err := db.ListRequests(ctx, RequestFilter{Group: top.ID})
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(inside.Requests)) != top.Requests {
			t.Errorf("group expands to %d requests, the row says %d", len(inside.Requests), top.Requests)
		}

		// Pages of two cover every group once.
		seen := map[string]bool{}
		cursor := ""
		for range 10 {
			p, perr := db.ListRequestGroups(ctx, RequestFilter{Limit: 2, Cursor: cursor})
			if perr != nil {
				t.Fatalf("page: %v", perr)
			}
			for _, g := range p.Groups {
				if seen[g.ID] {
					t.Errorf("group %s on two pages", g.ID)
				}
				seen[g.ID] = true
			}
			if cursor = p.NextCursor; cursor == "" {
				break
			}
		}
		if len(seen) != len(sittingGroups) {
			t.Errorf("paging reached %d groups, want %d", len(seen), len(sittingGroups))
		}

		counts, err := db.CountGroupOutcomes(ctx, RequestFilter{})
		if err != nil {
			t.Fatal(err)
		}
		// r1 and r2 are one group holding a refusal and a block; counted per
		// decision it would be two.
		if counts.Routed != 3 || counts.Blocked != 1 || counts.Errored != 0 || counts.All != 4 {
			t.Errorf("group counts = %+v, want routed 3, blocked 1, all 4", counts)
		}
	})
}

// A developer who sends a colleague's group id gets nothing: the group is a
// filter, ANDed with the scope, and never a scope of its own.
func TestAGroupIdDoesNotWidenTheScope(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		f := newGroupingFixture(t, db)
		f.record(sitting[0])

		bo, err := db.CreateUser(ctx, "bo@example.com", "Bo", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		page, err := db.ListRequestGroups(ctx, RequestFilter{})
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := db.ListRequests(ctx, RequestFilter{UserID: bo.ID, Group: page.Groups[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(theirs.Requests) != 0 {
			t.Errorf("another user's group id returned %d requests", len(theirs.Requests))
		}
	})
}
