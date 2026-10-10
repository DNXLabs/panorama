// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"testing"

	"github.com/dnxlabs/panorama/internal/store"
)

// TestPerUserSpendIsGatedByRole is the per-person spend rule (README, Principles)
// enforced over HTTP, which is where it actually has to hold.
//
// The store test proves the query respects an entitlement. This proves the
// right entitlement is computed from the caller - including the half that
// matters most, which is that a developer asking directly gets nothing rather
// than getting their own row back.
func TestPerUserSpendIsGatedByRole(t *testing.T) {
	f := newConsole(t)
	now := timeNow()

	team, err := f.db.CreateTeam(t.Context(), "Platform", "platform", "")
	if err != nil {
		t.Fatal(err)
	}
	lead, leadToken := f.newUser(t, "lead@example.com", store.RoleManager)
	stranger, strangerToken := f.newUser(t, "stranger@example.com", store.RoleManager)

	// The lead leads the team; the developer is on it; the stranger leads
	// nothing.
	if aerr := f.db.AddTeamMember(t.Context(), team.ID, lead.ID, store.TeamRoleLead); aerr != nil {
		t.Fatal(aerr)
	}
	if aerr := f.db.AddTeamMember(t.Context(), team.ID, f.developer.ID, store.TeamRoleMember); aerr != nil {
		t.Fatal(aerr)
	}
	_ = stranger

	f.record(t, f.developer.ID, "dev-1", "us.anthropic.claude-sonnet-5", money(3_000_000), now)
	f.record(t, f.admin.ID, "admin-1", "us.anthropic.claude-sonnet-5", money(1_000_000), now)

	emails := func(body map[string]any) []string {
		rows, _ := body["people"].([]any)
		var out []string
		for _, r := range rows {
			row, _ := r.(map[string]any)
			if email, ok := row["email"].(string); ok {
				out = append(out, email)
			}
		}
		return out
	}

	t.Run("an administrator sees everyone", func(t *testing.T) {
		rec, body := f.get(t, "/api/v1/seats", f.adminToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if body["can_see_people"] != true {
			t.Error("an administrator was told they cannot see per-person spend")
		}
		if len(emails(body)) != 2 {
			t.Errorf("saw %v, want both people who spent", emails(body))
		}
	})

	t.Run("a team lead sees their team and nobody else", func(t *testing.T) {
		rec, body := f.get(t, "/api/v1/seats", leadToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		got := emails(body)
		if len(got) != 1 || got[0] != f.developer.Email {
			t.Errorf("saw %v, want only the developer on the team they lead", got)
		}
	})

	t.Run("a manager who leads nothing sees nobody", func(t *testing.T) {
		rec, body := f.get(t, "/api/v1/seats", strangerToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := emails(body); len(got) != 0 {
			t.Errorf("saw %v; leading no team entitles them to nobody", got)
		}
		if body["can_see_people"] != false {
			t.Error("a manager leading nothing was told they can see per-person spend")
		}
	})

	t.Run("a developer sees nobody, including themselves", func(t *testing.T) {
		rec, body := f.get(t, "/api/v1/seats", f.devToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := emails(body); len(got) != 0 {
			t.Errorf("a developer saw %v; their own numbers belong on My usage", got)
		}
		if body["people_note"] == nil {
			t.Error("the response does not say why the table is empty")
		}
	})
}

// TestSeatEconomicsAreVisibleToEveryone. They name nobody, so a developer sees
// the same population figures their manager does - and the same window.
func TestSeatEconomicsAreVisibleToEveryone(t *testing.T) {
	f := newConsole(t)
	now := timeNow()

	f.record(t, f.developer.ID, "dev-1", "us.anthropic.claude-sonnet-5", money(3_000_000), now)
	f.record(t, f.admin.ID, "admin-1", "us.anthropic.claude-sonnet-5", money(1_000_000), now)

	for _, tc := range []struct{ name, token string }{
		{"admin", f.adminToken},
		{"developer", f.devToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := f.get(t, "/api/v1/seats", tc.token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			seats, _ := body["seats"].(map[string]any)
			if seats == nil {
				t.Fatal("no seat figures in the response")
			}
			active, _ := seats["active"].(float64)
			if active != 2 {
				t.Errorf("active = %v, want 2", seats["active"])
			}
			// A developer must get the organization's figure rather than one
			// silently narrowed to their own traffic, or two people reading the
			// same screen would see different numbers.
			cost, _ := seats["cost_per_active"].(map[string]any)
			nanos, _ := cost["nanos"].(float64)
			if nanos != 2_000_000 {
				t.Errorf("cost per active = %v, want 2000000 for every reader", cost["nanos"])
			}
		})
	}
}

// TestCacheHygieneReachesTheAPI. The number that says a harness is costing
// several times what it should.
func TestCacheHygieneReachesTheAPI(t *testing.T) {
	f := newConsole(t)
	now := timeNow()

	if err := f.db.RecordRequest(t.Context(), store.RequestRecord{
		RequestID: "uncached-1", UserID: f.developer.ID, StartedAt: now,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "m", ResolvedModel: "m", Tool: "some-cli",
		StatusCode: 200, Decision: store.DecisionAllowed, Attempts: 1,
		InputTokens: 50_000, CacheReadTokens: 0,
		Cost: money(5_000_000),
	}); err != nil {
		t.Fatal(err)
	}

	rec, body := f.get(t, "/api/v1/seats", f.adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	rows, _ := body["cache_hygiene"].([]any)
	if len(rows) == 0 {
		t.Fatal("no cache hygiene in the response")
	}
	row, _ := rows[0].(map[string]any)
	if row["uncached"] != true {
		t.Errorf("row = %+v, want the uncached harness flagged", row)
	}
	share, _ := row["cache_read_share"].(float64)
	if share != 0 {
		t.Errorf("cache read share = %v, want 0", row["cache_read_share"])
	}
}
