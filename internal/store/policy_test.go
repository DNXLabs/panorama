// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

func nanos(n int64) *pricing.Money {
	m := pricing.Money(n)
	return &m
}

// docRoute builds a route as a policy document compiles one: served past a soft
// limit, which is what a file that says nothing about it means.
func docRoute(name, target string) policy.Route {
	return policy.Route{Name: name, Target: providers.ModelRef{Provider: providers.KindBedrock, Model: target}, ServesOverSoft: true}
}

// TestApplyPolicyIsIdempotent is the property that makes a policy file safe to
// re-apply on every deploy.
func TestApplyPolicyIsIdempotent(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		cheap := docRoute("standard", "us.anthropic.claude-sonnet-5")
		cheap.ServesOverSoft = false

		set := policy.Set{
			Routes: []policy.Route{cheap, docRoute("fast", "us.anthropic.claude-haiku-4-5")},
			Budgets: []policy.Budget{
				{Scope: policy.ScopeOrg, Period: policy.PeriodMonth, Name: "deployment",
					Enabled: true, HardLimit: nanos(1_000_000_000_000)},
			},
		}

		added, updated, removed, err := db.ApplyPolicy(t.Context(), set)
		if err != nil {
			t.Fatalf("first apply: %v", err)
		}
		if added != 3 || updated != 0 || removed != 0 {
			t.Errorf("first apply: %d added, %d updated, %d removed; want 3/0/0", added, updated, removed)
		}

		loaded, err := db.LoadPolicy(t.Context())
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(loaded.Routes) != 2 {
			t.Fatalf("%d routes stored, want 2", len(loaded.Routes))
		}
		stored, err := db.ListRoutes(t.Context())
		if err != nil {
			t.Fatalf("list routes: %v", err)
		}
		created := map[string]time.Time{}
		for _, r := range stored {
			created[r.Name] = r.CreatedAt
		}

		// The same document again changes nothing and creates nothing new.
		added, updated, removed, err = db.ApplyPolicy(t.Context(), set)
		if err != nil {
			t.Fatalf("second apply: %v", err)
		}
		if added != 0 || removed != 0 || updated != 3 {
			t.Errorf("second apply: %d added, %d updated, %d removed; want 0/3/0", added, updated, removed)
		}

		after, err := db.ListRoutes(t.Context())
		if err != nil {
			t.Fatalf("list routes: %v", err)
		}
		for _, r := range after {
			// A name is a route's identity across edits, and it is what every
			// request already recorded against it resolves through. Re-applying
			// must update rather than replace.
			if was, ok := created[r.Name]; !ok || !r.CreatedAt.Equal(was) {
				t.Errorf("route %q was recreated on re-apply: created %v, now %v", r.Name, was, r.CreatedAt)
			}
		}
		if len(after) != 2 {
			t.Errorf("%d routes after re-applying the same document, want 2", len(after))
		}
	})
}

func TestApplyPolicyRemovesWhatIsNotInTheFile(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		full := policy.Set{
			Routes: []policy.Route{
				docRoute("keep", "us.anthropic.claude-sonnet-5"),
				docRoute("drop", "us.anthropic.claude-opus-4-6"),
			},
			Budgets: []policy.Budget{
				{Scope: policy.ScopeOrg, Period: policy.PeriodDay, Enabled: true, HardLimit: nanos(1)},
			},
		}
		if _, _, _, err := db.ApplyPolicy(t.Context(), full); err != nil {
			t.Fatalf("apply: %v", err)
		}

		trimmed := policy.Set{Routes: full.Routes[:1]}
		_, _, removed, err := db.ApplyPolicy(t.Context(), trimmed)
		if err != nil {
			t.Fatalf("apply trimmed: %v", err)
		}
		if removed != 2 {
			t.Errorf("removed %d, want 2 (the dropped route and the budget)", removed)
		}

		loaded, err := db.LoadPolicy(t.Context())
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(loaded.Routes) != 1 || loaded.Routes[0].Name != "keep" {
			t.Errorf("routes after trim = %+v", loaded.Routes)
		}
		if len(loaded.Budgets) != 0 {
			t.Errorf("budgets after trim = %+v", loaded.Budgets)
		}

	})
}

func TestLoadPolicyRoundTripsEveryField(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		everything := docRoute("everything", "us.anthropic.claude-opus-4-6")
		everything.Description = "the whole shape"
		everything.Users = []string{"usr_1"}
		everything.Teams = []string{"tm_1"}
		everything.Tools = []string{"claude-code"}
		everything.ServesOverSoft = false

		set := policy.Set{
			Routes: []policy.Route{everything},
			Budgets: []policy.Budget{{
				Scope: policy.ScopeUser, ScopeID: "usr_1", Period: policy.PeriodWeek,
				Name: "personal", Enabled: true,
				SoftLimit: nanos(5_000_000_000), HardLimit: nanos(10_000_000_000),
			}},
		}
		if _, _, _, err := db.ApplyPolicy(t.Context(), set); err != nil {
			t.Fatalf("apply: %v", err)
		}

		loaded, err := db.LoadPolicy(t.Context())
		if err != nil {
			t.Fatalf("load: %v", err)
		}

		r := loaded.Routes[0]
		if r.Description != "the whole shape" {
			t.Errorf("description did not round-trip: %q", r.Description)
		}
		if len(r.Users) != 1 || len(r.Teams) != 1 || len(r.Tools) != 1 {
			t.Errorf("conditions did not round-trip: %+v", r)
		}
		if r.Target != providers.MustModelRef("bedrock/us.anthropic.claude-opus-4-6") {
			t.Errorf("target did not round-trip: %q", r.Target)
		}
		if r.ServesOverSoft {
			t.Error("serves_over_soft: false did not round-trip, so the route now serves the " +
				"expensive model all the way to the hard limit")
		}

		b := loaded.Budgets[0]
		if b.SoftLimit == nil || *b.SoftLimit != 5_000_000_000 {
			t.Errorf("soft limit did not round-trip: %v", b.SoftLimit)
		}
		if b.HardLimit == nil || *b.HardLimit != 10_000_000_000 {
			t.Errorf("hard limit did not round-trip: %v", b.HardLimit)
		}
	})
}

// A budget written from a screen and one written from a file are the same row,
// matched on what they govern rather than on an id.
func TestSaveBudgetMatchesOnWhatItGoverns(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		b := policy.Budget{
			Scope: policy.ScopeTeam, ScopeID: "tm_1", Period: policy.PeriodMonth,
			Name: "platform", Enabled: true, HardLimit: nanos(1_000_000_000),
		}

		id, err := db.SaveBudget(t.Context(), b)
		if err != nil {
			t.Fatalf("save: %v", err)
		}

		b.HardLimit = nanos(2_000_000_000)
		again, err := db.SaveBudget(t.Context(), b)
		if err != nil {
			t.Fatalf("raise the limit: %v", err)
		}
		if again != id {
			t.Errorf("raising a limit created a second budget: %s then %s", id, again)
		}

		loaded, err := db.LoadPolicy(t.Context())
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(loaded.Budgets) != 1 || *loaded.Budgets[0].HardLimit != 2_000_000_000 {
			t.Errorf("budgets = %+v, want one raised limit", loaded.Budgets)
		}

		removed, err := db.DeleteBudget(t.Context(), id)
		if err != nil || !removed {
			t.Fatalf("delete = %v %v", removed, err)
		}
		if removed, _ := db.DeleteBudget(t.Context(), id); removed {
			t.Error("deleting a budget twice reported it removed twice")
		}
	})
}

// A budget that enforces nothing is refused rather than stored looking like it
// does something.
func TestSaveBudgetRefusesOneThatEnforcesNothing(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		_, err := db.SaveBudget(t.Context(), policy.Budget{
			Scope: policy.ScopeOrg, Period: policy.PeriodMonth, Enabled: true,
		})
		if err == nil {
			t.Error("a budget with no limits was accepted")
		}
	})
}

func TestLoadPolicyKeepsNullLimitsNull(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		set := policy.Set{Budgets: []policy.Budget{{
			Scope: policy.ScopeOrg, Period: policy.PeriodDay, Enabled: true,
			SoftLimit: nanos(1_000_000_000),
		}}}
		if _, _, _, err := db.ApplyPolicy(t.Context(), set); err != nil {
			t.Fatalf("apply: %v", err)
		}

		loaded, err := db.LoadPolicy(t.Context())
		if err != nil {
			t.Fatalf("load: %v", err)
		}

		// Absent limits stay absent rather than becoming zero, because zero
		// means "may spend nothing" and absent means "no ceiling".
		b := loaded.Budgets[0]
		if b.HardLimit != nil {
			t.Errorf("an unset hard limit came back as %v, which would block at that amount", b.HardLimit)
		}
	})
}

// TestSpendSinceCountsOnlyRequestsThatReachedAProvider is the invariant that
// stops a blocked user being held down by their own blocked requests.
func TestSpendSinceCountsOnlyRequestsThatReachedAProvider(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		now := time.Now().UTC()
		cost := &pricing.Cost{Input: 1_000_000, Total: 1_000_000}

		// Two real requests.
		writeRequest(t, db, user.ID, "ok-1", now, cost, 1)
		writeRequest(t, db, user.ID, "ok-2", now, cost, 1)
		// One the gateway answered itself: blocked by a budget.
		writeRequest(t, db, user.ID, "blocked", now, nil, 0)

		spend, err := db.SpendSince(t.Context(), policy.ScopeUser, now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("spend: %v", err)
		}

		got := spend[user.ID]
		if got.Requests != 2 {
			t.Errorf("requests = %d, want 2 - a blocked request counted against the budget that blocked it", got.Requests)
		}
		if got.Cost != 2_000_000 {
			t.Errorf("cost = %d, want 2000000", got.Cost)
		}
	})
}

func TestSpendSinceByScope(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		alice, err := db.CreateUser(t.Context(), "alice@example.com", "Alice", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		bob, err := db.CreateUser(t.Context(), "bob@example.com", "Bob", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		team, err := db.CreateTeam(t.Context(), "Platform", "platform", "")
		if err != nil {
			t.Fatal(err)
		}
		if aerr := db.AddTeamMember(t.Context(), team.ID, alice.ID, TeamRoleLead); aerr != nil {
			t.Fatal(aerr)
		}
		if aerr := db.AddTeamMember(t.Context(), team.ID, bob.ID, TeamRoleMember); aerr != nil {
			t.Fatal(aerr)
		}

		now := time.Now().UTC()
		since := now.Add(-time.Hour)
		writeRequest(t, db, alice.ID, "a1", now, &pricing.Cost{Total: 3_000}, 1)
		writeRequest(t, db, bob.ID, "b1", now, &pricing.Cost{Total: 7_000}, 1)

		org, err := db.SpendSince(t.Context(), policy.ScopeOrg, since)
		if err != nil {
			t.Fatal(err)
		}
		if org[""].Cost != 10_000 {
			t.Errorf("org spend = %d, want 10000", org[""].Cost)
		}

		teams, err := db.SpendSince(t.Context(), policy.ScopeTeam, since)
		if err != nil {
			t.Fatal(err)
		}
		if teams[team.ID].Cost != 10_000 {
			t.Errorf("team spend = %d, want 10000 (both members)", teams[team.ID].Cost)
		}

		users, err := db.SpendSince(t.Context(), policy.ScopeUser, since)
		if err != nil {
			t.Fatal(err)
		}
		if users[alice.ID].Cost != 3_000 || users[bob.ID].Cost != 7_000 {
			t.Errorf("per-user spend = %v", users)
		}
	})
}

// TestSpendSinceCountsUnpricedSeparately: a budget enforced against an
// incomplete figure has to know it is incomplete.
func TestSpendSinceCountsUnpricedSeparately(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		now := time.Now().UTC()
		writeRequest(t, db, user.ID, "priced", now, &pricing.Cost{Total: 500}, 1)
		writeRequest(t, db, user.ID, "unpriced", now, nil, 1)

		spend, err := db.SpendSince(t.Context(), policy.ScopeOrg, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}

		got := spend[""]
		if got.Requests != 2 {
			t.Errorf("requests = %d, want 2", got.Requests)
		}
		if got.Unpriced != 1 {
			t.Errorf("unpriced = %d, want 1", got.Unpriced)
		}
		if got.Cost != 500 {
			t.Errorf("cost = %d, want 500 - an unpriced request must not contribute zero silently", got.Cost)
		}
	})
}

func TestSpendSinceRejectsAnUnknownScope(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if _, err := db.SpendSince(t.Context(), "everyone", time.Now()); err == nil {
			t.Error("an unknown scope was accepted")
		}
	})
}

// writeRequest records one audit row for the spend tests.
func writeRequest(t *testing.T, db *DB, userID, requestID string, at time.Time, cost *pricing.Cost, attempts int) {
	t.Helper()

	rec := RequestRecord{
		RequestID: requestID, UserID: userID, StartedAt: at,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "claude-sonnet-5", ResolvedModel: "us.anthropic.claude-sonnet-5",
		Tool: "claude-code", StatusCode: 200, Decision: DecisionAllowed,
		InputTokens: 100, OutputTokens: 50,
		Cost: cost, Attempts: attempts,
	}
	if cost != nil {
		rec.CostSource = pricing.SourceSeed
	}
	if attempts == 0 {
		rec.Decision = DecisionBlocked
		rec.StatusCode = 403
		rec.InputTokens, rec.OutputTokens = 0, 0
	}
	if err := db.RecordRequest(t.Context(), rec); err != nil {
		t.Fatalf("record %s: %v", requestID, err)
	}
}

func TestTeams(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		// A slug is derived when one is not given, because most operators do
		// not want to invent one.
		team, err := db.CreateTeam(t.Context(), "Platform Engineering", "", "keeps the lights on")
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		if team.Slug != "platform-engineering" {
			t.Errorf("slug = %q, want platform-engineering", team.Slug)
		}

		found, err := db.TeamBySlug(t.Context(), "platform-engineering")
		if err != nil || found.ID != team.ID {
			t.Fatalf("lookup by slug: %v %+v", err, found)
		}

		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		if aerr := db.AddTeamMember(t.Context(), team.ID, user.ID, TeamRoleMember); aerr != nil {
			t.Fatalf("add member: %v", aerr)
		}
		// Adding twice changes the role rather than failing, so a policy script
		// is re-runnable.
		if aerr := db.AddTeamMember(t.Context(), team.ID, user.ID, TeamRoleLead); aerr != nil {
			t.Fatalf("re-add member: %v", aerr)
		}

		n, err := db.TeamMemberCount(t.Context(), team.ID)
		if err != nil || n != 1 {
			t.Errorf("member count = %d (%v), want 1", n, err)
		}

		membership, err := db.loadTeamMembership(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(membership[user.ID]) != 1 || membership[user.ID][0] != team.ID {
			t.Errorf("membership = %v", membership)
		}

		if rerr := db.RemoveTeamMember(t.Context(), team.ID, user.ID); rerr != nil {
			t.Fatal(rerr)
		}
		if n, _ := db.TeamMemberCount(t.Context(), team.ID); n != 0 {
			t.Errorf("member count after removal = %d", n)
		}
	})
}

func TestCreateTeamRejectsABadSlug(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if _, err := db.CreateTeam(t.Context(), "Platform", "Platform Team!", ""); err == nil {
			t.Error("a slug with spaces and punctuation was accepted")
		}
		if _, err := db.CreateTeam(t.Context(), "", "platform", ""); err == nil {
			t.Error("a team with no name was accepted")
		}
	})
}

// TestTeamSpendIncludesServiceTokens is the assertion the service-token design
// most easily fails silently.
//
// Team spend reaches a team by joining team_members on requests.user_id. A
// pipeline has no user id, so before the union arm existed its cost was counted
// at the organization level and nowhere else - a team could run its whole
// budget through CI and see nothing against it.
func TestTeamSpendIncludesServiceTokens(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()
		cost := &pricing.Cost{Input: 1_000_000, Total: 1_000_000}

		team, err := db.CreateTeam(ctx, "Platform", "platform", "")
		if err != nil {
			t.Fatal(err)
		}
		other, err := db.CreateTeam(ctx, "Other", "other", "")
		if err != nil {
			t.Fatal(err)
		}
		alice, err := db.CreateUser(ctx, "alice@example.com", "Alice", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		if aerr := db.AddTeamMember(ctx, team.ID, alice.ID, TeamRoleMember); aerr != nil {
			t.Fatal(aerr)
		}

		// One request from a person on the team.
		writeRequest(t, db, alice.ID, "human", now, cost, 1)

		// One from the team's pipeline: no user at all.
		if rerr := db.RecordRequest(ctx, RequestRecord{
			RequestID: "ci", TeamID: team.ID, ServiceTokenID: "svt_example",
			StartedAt: now, Surface: "anthropic", Provider: "bedrock",
			RequestedModel: "claude-sonnet-5", ResolvedModel: "us.anthropic.claude-sonnet-5",
			Tool: "claude-code", StatusCode: 200, Decision: DecisionAllowed,
			InputTokens: 100, OutputTokens: 50,
			Cost: cost, CostSource: pricing.SourceSeed, Attempts: 1,
		}); rerr != nil {
			t.Fatalf("record ci request: %v", rerr)
		}

		spend, err := db.SpendSince(ctx, policy.ScopeTeam, now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("SpendSince: %v", err)
		}

		got := spend[team.ID]
		if got.Requests != 2 {
			t.Errorf("team requests = %d, want 2 - the pipeline's request is missing "+
				"from its own team's spend", got.Requests)
		}
		if got.Cost != pricing.Money(2_000_000) {
			t.Errorf("team cost = %d, want 2000000", got.Cost)
		}

		// The union must not leak across teams, and must not double count the
		// human request by matching both arms.
		if s, ok := spend[other.ID]; ok && s.Requests != 0 {
			t.Errorf("unrelated team picked up %d requests", s.Requests)
		}

		// The organization total is the ground truth both arms must agree with.
		orgSpend, err := db.SpendSince(ctx, policy.ScopeOrg, now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("SpendSince org: %v", err)
		}
		if orgSpend[""].Requests != 2 {
			t.Errorf("org requests = %d, want 2", orgSpend[""].Requests)
		}
	})
}

// TestServiceTokenSpendIsNotCountedTwice guards the specific failure a single
// join with a COALESCE would produce: a row satisfying both branches.
func TestServiceTokenSpendIsNotCountedTwice(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()
		cost := &pricing.Cost{Input: 1_000_000, Total: 1_000_000}

		team, err := db.CreateTeam(ctx, "Platform", "platform", "")
		if err != nil {
			t.Fatal(err)
		}
		alice, err := db.CreateUser(ctx, "alice@example.com", "Alice", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		if aerr := db.AddTeamMember(ctx, team.ID, alice.ID, TeamRoleMember); aerr != nil {
			t.Fatal(aerr)
		}

		// A row carrying both a user on the team and that same team directly.
		// Nothing in the product writes one - the data plane sets one or the
		// other - but if it ever did, it must still count once.
		if rerr := db.RecordRequest(ctx, RequestRecord{
			RequestID: "both", UserID: alice.ID, TeamID: team.ID,
			StartedAt: now, Surface: "anthropic", Provider: "bedrock",
			RequestedModel: "claude-sonnet-5", ResolvedModel: "us.anthropic.claude-sonnet-5",
			Tool: "claude-code", StatusCode: 200, Decision: DecisionAllowed,
			InputTokens: 100, OutputTokens: 50,
			Cost: cost, CostSource: pricing.SourceSeed, Attempts: 1,
		}); rerr != nil {
			t.Fatalf("record: %v", rerr)
		}

		spend, err := db.SpendSince(ctx, policy.ScopeTeam, now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("SpendSince: %v", err)
		}
		if got := spend[team.ID].Requests; got != 1 {
			t.Errorf("team requests = %d, want 1 - a row carrying both a team member "+
				"and a direct team was counted by both arms of the union", got)
		}
		// The org arm is the invariant that matters: it counts rows, once.
		orgSpend, err := db.SpendSince(ctx, policy.ScopeOrg, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if orgSpend[""].Requests != 1 {
			t.Errorf("org requests = %d, want 1", orgSpend[""].Requests)
		}
	})
}
