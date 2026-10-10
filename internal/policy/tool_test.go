// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"strings"
	"testing"
)

// A search names no route, so only money decides it: a hard limit stops it
// as it stops a model request, and a soft limit warns, even for somebody whose
// only route is one that stops at the soft limit.
func TestASearchAnswersToBudgetsAndNotToRoutes(t *testing.T) {
	repo := &fakeRepo{
		set: Set{
			Routes: []Route{stopsAtSoft("premium", "us.anthropic.opus")},
			Budgets: []Budget{
				{ID: "bdg_soft", Scope: ScopeOrg, Period: PeriodMonth, Name: "deployment",
					Enabled: true, SoftLimit: usd(t, "800.00")},
				{ID: "bdg_hard", Scope: ScopeUser, ScopeID: "usr_hard", Period: PeriodDay, Name: "daily",
					Enabled: true, HardLimit: usd(t, "10.00")},
			},
		},
		spend: map[string]map[string]Spend{
			ScopeOrg:  {"": {Cost: *usd(t, "812.00"), Requests: 9_000}},
			ScopeUser: {"usr_hard": {Cost: *usd(t, "12.50"), Requests: 40}},
		},
	}
	e := newEngine(t, repo, "2026-08-14T12:00:00Z")

	soft := e.EvaluateTool(Input{UserID: "usr_1", Tool: "claude-code"})
	if soft.Blocked || soft.Refused {
		t.Fatalf("a soft limit stopped a search: %+v", soft)
	}
	if soft.Budget != StateOverSoft || soft.Warning == "" || soft.BudgetID != "bdg_soft" {
		t.Errorf("soft: %+v", soft)
	}

	hard := e.EvaluateTool(Input{UserID: "usr_hard"})
	if !hard.Blocked || hard.BlockReason != BlockedBudgetExhausted || !strings.Contains(hard.BlockMessage, "$10.00") {
		t.Errorf("hard: %+v", hard)
	}

	if d := (*Engine)(nil).EvaluateTool(Input{UserID: "usr_1"}); d.Blocked || d.Warning != "" {
		t.Errorf("a nil engine should decide nothing: %+v", d)
	}
}
