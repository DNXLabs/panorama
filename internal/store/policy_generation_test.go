// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
)

// TestEveryPolicyWriteMovesTheGeneration holds the triggers to the tables
// LoadPolicy reads. A write that does not move the counter is a change a
// running gateway serves only at its next full reload, which is the twenty
// seconds of "no route you can use" the counter exists to remove.
func TestEveryPolicyWriteMovesTheGeneration(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		last, err := db.PolicyGeneration(ctx)
		if err != nil {
			t.Fatalf("generation: %v", err)
		}
		moved := func(what string) {
			t.Helper()
			now, gerr := db.PolicyGeneration(ctx)
			if gerr != nil {
				t.Fatalf("generation after %s: %v", what, gerr)
			}
			if now <= last {
				t.Errorf("%s did not move the policy generation (still %d)", what, now)
			}
			last = now
		}

		_, err = db.SaveProvider(ctx, providers.Settings{Kind: providers.KindBedrock, Enabled: true, Region: "us-east-1"})
		if err != nil {
			t.Fatalf("save provider: %v", err)
		}
		moved("saving a provider")

		route := docRoute("fast", "us.anthropic.claude-haiku-4-5")
		apply := func(what string, set policy.Set) {
			t.Helper()
			if _, aerr := db.ApplyPolicy(ctx, set); aerr != nil {
				t.Fatalf("%s: %v", what, aerr)
			}
			moved(what)
		}
		apply("adding a route", policy.Set{Routes: []policy.Route{route}})
		route.Description = "for subagents"
		apply("editing a route", policy.Set{Routes: []policy.Route{route}})

		budget := policy.Budget{Scope: policy.ScopeOrg, Period: policy.PeriodMonth, Enabled: true, HardLimit: nanos(1)}
		apply("adding a budget", policy.Set{Routes: []policy.Route{route}, Budgets: []policy.Budget{budget}})
		budget.HardLimit = nanos(2)
		apply("editing a budget", policy.Set{Routes: []policy.Route{route}, Budgets: []policy.Budget{budget}})
		apply("removing a route and a budget", policy.Set{})

		team, err := db.CreateTeam(ctx, "Payments", "", "")
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		if aerr := db.AddTeamMember(ctx, team.ID, user.ID, TeamRoleMember); aerr != nil {
			t.Fatalf("add member: %v", aerr)
		}
		moved("adding a team member")
		if rerr := db.RemoveTeamMember(ctx, team.ID, user.ID); rerr != nil {
			t.Fatalf("remove member: %v", rerr)
		}
		moved("removing a team member")

		id, err := db.SaveCostCenter(ctx, CostCenter{Code: "PAY", Name: "Payments", WarnAtPercent: 80, AcceptsStamp: true})
		if err != nil {
			t.Fatalf("save cost center: %v", err)
		}
		moved("adding a cost center")
		if _, derr := db.DeleteCostCenter(ctx, id); derr != nil {
			t.Fatalf("delete cost center: %v", derr)
		}
		moved("deleting a cost center")

		// A write anywhere else leaves it alone, or every sign-in and every
		// recorded request would reload every replica's policy.
		if _, rerr := db.ExecContext(ctx, db.rebind(`UPDATE users SET display_name = ? WHERE id = ?`),
			"Someone else", user.ID); rerr != nil {
			t.Fatalf("update user: %v", rerr)
		}
		if now, gerr := db.PolicyGeneration(ctx); gerr != nil || now != last {
			t.Errorf("a write outside the policy moved the generation to %d (%v), want %d", now, gerr, last)
		}
	})
}
