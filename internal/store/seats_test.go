// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// seed puts three people with different spend into two teams and returns them.
func seedSpend(t *testing.T, db *DB) (alice, bob, carol *User, platform, other *Team) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()

	var err error
	if alice, err = db.CreateUser(ctx, "alice@example.com", "Alice", RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if bob, err = db.CreateUser(ctx, "bob@example.com", "Bob", RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if carol, err = db.CreateUser(ctx, "carol@example.com", "Carol", RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if platform, err = db.CreateTeam(ctx, "Platform", "platform", ""); err != nil {
		t.Fatal(err)
	}
	if other, err = db.CreateTeam(ctx, "Other", "other", ""); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		team *Team
		user *User
	}{{platform, alice}, {platform, bob}, {other, carol}} {
		if aerr := db.AddTeamMember(ctx, m.team.ID, m.user.ID, TeamRoleMember); aerr != nil {
			t.Fatal(aerr)
		}
	}

	// Alice 3, Bob 2, Carol 1 - so ordering by cost is unambiguous.
	for _, s := range []struct {
		user *User
		n    int
	}{{alice, 3}, {bob, 2}, {carol, 1}} {
		for i := range s.n {
			writeRequest(t, db, s.user.ID, s.user.Email+string(rune('a'+i)), now,
				&pricing.Cost{Input: 1_000_000, Total: 1_000_000}, 1)
		}
	}
	return alice, bob, carol, platform, other
}

// TestSpendByUserRespectsItsEntitlement is the whole of the 19 Aug amendment,
// in one test. A developer sees nobody; a manager sees the teams they lead and
// nobody else; an administrator sees everyone.
func TestSpendByUserRespectsItsEntitlement(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		alice, bob, carol, platform, _ := seedSpend(t, db)
		filter := RequestFilter{Since: time.Now().UTC().Add(-time.Hour)}

		t.Run("a developer sees nobody", func(t *testing.T) {
			rows, err := db.SpendByUser(t.Context(), filter, Visibility{})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Errorf("a developer got %d named rows; the amendment permits none", len(rows))
			}
		})

		t.Run("an administrator sees everyone", func(t *testing.T) {
			rows, err := db.SpendByUser(t.Context(), filter, Visibility{Everyone: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 {
				t.Fatalf("got %d rows, want 3", len(rows))
			}
			// Ordered by cost, because that is what makes it useful to a budget
			// holder - and the reason it was refused before the amendment.
			if rows[0].UserID != alice.ID || rows[2].UserID != carol.ID {
				t.Errorf("rows are not ordered by spend: %v, %v, %v",
					rows[0].Email, rows[1].Email, rows[2].Email)
			}
			if rows[0].Totals.Cost.Total != pricing.Money(3_000_000) {
				t.Errorf("alice cost = %d, want 3000000", rows[0].Totals.Cost.Total)
			}
		})

		t.Run("a manager sees the teams they lead and nobody else", func(t *testing.T) {
			rows, err := db.SpendByUser(t.Context(), filter, Visibility{TeamIDs: []string{platform.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 {
				t.Fatalf("got %d rows, want the two on the team they lead", len(rows))
			}
			seen := map[string]bool{}
			for _, r := range rows {
				seen[r.UserID] = true
			}
			if !seen[alice.ID] || !seen[bob.ID] {
				t.Error("a team lead cannot see their own team")
			}
			if seen[carol.ID] {
				t.Error("a team lead can see somebody on a team they do not lead")
			}
		})
	})
}

// TestSpendByUserCountsSomebodyOnTwoTeamsOnce. The entitlement is an IN against
// a subquery for exactly this reason.
func TestSpendByUserCountsSomebodyOnTwoTeamsOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		alice, _, _, platform, other := seedSpend(t, db)
		if err := db.AddTeamMember(t.Context(), other.ID, alice.ID, TeamRoleMember); err != nil {
			t.Fatal(err)
		}

		rows, err := db.SpendByUser(t.Context(),
			RequestFilter{Since: time.Now().UTC().Add(-time.Hour)},
			Visibility{TeamIDs: []string{platform.ID, other.ID}})
		if err != nil {
			t.Fatal(err)
		}

		count := 0
		for _, r := range rows {
			if r.UserID == alice.ID {
				count++
				if r.Totals.Cost.Total != pricing.Money(3_000_000) {
					t.Errorf("alice cost = %d, want 3000000 - counted twice",
						r.Totals.Cost.Total)
				}
			}
		}
		if count != 1 {
			t.Errorf("alice appears %d times for a lead of both her teams, want 1", count)
		}
	})
}

// TestSeatEconomicsNamesNobody is why this figure can appear on a screen the
// per-user table cannot.
func TestSeatEconomicsNamesNobody(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		seedSpend(t, db)
		// A fourth account that never spent: a provisioned seat nobody used,
		// which is the finding the number exists to surface.
		if _, err := db.CreateUser(t.Context(), "dormant@example.com", "Dormant", RoleDeveloper); err != nil {
			t.Fatal(err)
		}

		seats, err := db.SeatEconomics(t.Context(), RequestFilter{Since: time.Now().UTC().Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}

		if seats.Provisioned != 4 {
			t.Errorf("provisioned = %d, want 4", seats.Provisioned)
		}
		if seats.Active != 3 {
			t.Errorf("active = %d, want 3 - the dormant seat is the point", seats.Active)
		}
		if seats.Total != pricing.Money(6_000_000) {
			t.Errorf("total = %d, want 6000000", seats.Total)
		}
		if seats.CostPerActive != pricing.Money(2_000_000) {
			t.Errorf("cost per active = %d, want 2000000", seats.CostPerActive)
		}
		// Spends ascending are 1, 2, 3. Nearest-rank: p50 of 3 is the 2nd, p90
		// the 3rd - real amounts somebody spent rather than an interpolation.
		if seats.P50 != pricing.Money(2_000_000) {
			t.Errorf("p50 = %d, want 2000000", seats.P50)
		}
		if seats.P90 != pricing.Money(3_000_000) || seats.Max != pricing.Money(3_000_000) {
			t.Errorf("p90/max = %d/%d, want 3000000", seats.P90, seats.Max)
		}
	})
}

func TestSeatEconomicsWithNobodyActive(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		if _, err := db.CreateUser(t.Context(), "nobody@example.com", "Nobody", RoleDeveloper); err != nil {
			t.Fatal(err)
		}

		seats, err := db.SeatEconomics(t.Context(), RequestFilter{Since: time.Now().UTC().Add(-time.Hour)})
		if err != nil {
			t.Fatalf("an idle deployment must not error: %v", err)
		}
		if seats.Active != 0 || seats.CostPerActive != 0 {
			t.Errorf("seats = %+v, want zero rather than a division by zero", seats)
		}
	})
}

// TestCacheHygieneFindsTheUncachedHarness. The number that says "this harness
// is costing you several times what it should".
func TestCacheHygieneFindsTheUncachedHarness(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()
		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		// One harness reads from cache, the other never does.
		for i, tc := range []struct {
			tool      string
			cacheRead int64
		}{{"claude-code", 90_000}, {"claude-code", 90_000}, {"some-cli", 0}, {"some-cli", 0}} {
			if rerr := db.RecordRequest(ctx, RequestRecord{
				RequestID: tc.tool + string(rune('a'+i)), UserID: user.ID, StartedAt: now,
				Surface: "anthropic", Provider: "bedrock",
				RequestedModel: "m", ResolvedModel: "m", Tool: tc.tool,
				StatusCode: 200, Decision: DecisionAllowed, Attempts: 1,
				InputTokens: 10_000, CacheReadTokens: tc.cacheRead,
				Cost: &pricing.Cost{Total: 1_000_000}, CostSource: pricing.SourceSeed,
			}); rerr != nil {
				t.Fatal(rerr)
			}
		}

		rows, err := db.CacheHygieneByTool(ctx, RequestFilter{Since: now.Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}

		byTool := map[string]CacheHygiene{}
		for _, r := range rows {
			byTool[r.Tool] = r
		}
		if !byTool["some-cli"].Uncached() {
			t.Error("the harness that read nothing from cache is not flagged")
		}
		if byTool["claude-code"].Uncached() {
			t.Error("a harness that is caching was flagged as uncached")
		}
		if byTool["claude-code"].CacheReadTokens != 180_000 {
			t.Errorf("cache reads = %d, want 180000", byTool["claude-code"].CacheReadTokens)
		}
	})
}

func TestLeadTeamsIsOnlyWhatTheyLead(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		alice, _, _, platform, other := seedSpend(t, db)

		// A member of one, a lead of the other.
		if err := db.AddTeamMember(t.Context(), other.ID, alice.ID, TeamRoleLead); err != nil {
			t.Fatal(err)
		}

		teams, err := db.LeadTeams(t.Context(), alice.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(teams) != 1 || teams[0] != other.ID {
			t.Errorf("lead teams = %v, want just the one she leads (%s); membership is not leadership",
				teams, other.ID)
		}
		if teams[0] == platform.ID {
			t.Error("plain membership was treated as leadership")
		}
	})
}
