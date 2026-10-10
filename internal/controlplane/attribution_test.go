// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// attribute records a session against a work item, the way the data plane does.
func (f *consoleFixture) attribute(t *testing.T, fingerprint, userID, branch, ticket string, at time.Time) *store.Session {
	t.Helper()

	signals := attribution.Signals{
		Repo:   attribution.Observation{Value: "acme/panorama", Rung: attribution.RungStructural, Evidence: "git remote"},
		Branch: attribution.Observation{Value: branch, Rung: attribution.RungStructural, Evidence: "environment block"},
	}
	if ticket != "" {
		signals.Ticket = attribution.Observation{Value: ticket, Rung: attribution.RungStructural, Evidence: "branch name"}
	}

	session, err := f.db.AttributeSession(t.Context(), store.Observation{
		Fingerprint: fingerprint, Method: "conversation-prefix",
		UserID: userID, Tool: "claude-code", At: at, Signals: signals,
	})
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	return session
}

// recordIn records one request against a session.
func (f *consoleFixture) recordIn(t *testing.T, userID, requestID, sessionID string, cost int64, at time.Time) {
	t.Helper()

	rec := store.RequestRecord{
		RequestID: requestID, UserID: userID, StartedAt: at,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "claude-sonnet-5", ResolvedModel: "us.anthropic.claude-sonnet-5",
		Tool: "claude-code", StatusCode: 200, Decision: store.DecisionAllowed,
		InputTokens: 100, OutputTokens: 50,
		Cost:       &pricing.Cost{Input: pricing.Money(cost), Total: pricing.Money(cost)},
		CostSource: pricing.SourceSeed,
		Attempts:   1,
		SessionID:  sessionID,
	}
	if err := f.db.RecordRequest(t.Context(), rec); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// TestWorkShowsUnattributedSpendAsARow is the third trust commitment at the API
// boundary. A rollup that omits what it could not place looks authoritative and
// is wrong.
func TestWorkShowsUnattributedSpendAsARow(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	session := f.attribute(t, "fp-1", f.developer.ID, "feature/PAY-1183", "PAY-1183", now)
	f.recordIn(t, f.developer.ID, "attributed", session.ID, 3_000_000, now)
	f.recordIn(t, f.developer.ID, "orphan", "", 7_000_000, now)

	rec, body := f.get(t, "/api/v1/work", f.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	rows, _ := body["by_work_item"].([]any)
	if len(rows) != 2 {
		t.Fatalf("%d work-item rows, want 2 (the ticket and the remainder)", len(rows))
	}

	var sawUnattributed bool
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["attributed"] == false {
			sawUnattributed = true
			if row["label"] != "Unattributed" {
				t.Errorf("the remainder row is labelled %v; each client would otherwise invent its own name for it", row["label"])
			}
			cost, _ := row["cost"].(map[string]any)
			if cost["total_usd"] != pricing.Money(7_000_000).USD() {
				t.Errorf("unattributed cost = %v, want it intact", cost["total_usd"])
			}
		}
	}
	if !sawUnattributed {
		t.Error("unattributed spend was dropped from the response")
	}

	coverage, _ := body["coverage"].(map[string]any)
	if coverage["attributed_requests"] != float64(1) || coverage["requests"] != float64(2) {
		t.Errorf("coverage = %v of %v, want 1 of 2", coverage["attributed_requests"], coverage["requests"])
	}
	if coverage["attributed_cost_percent"] != float64(30) {
		t.Errorf("attributed cost percent = %v, want 30", coverage["attributed_cost_percent"])
	}
	// The other gap travels with it, so nobody reads 100% coverage and assumes
	// the money behind it was all priced.
	if _, ok := coverage["unpriced_requests"]; !ok {
		t.Error("coverage does not carry the unpriced count")
	}
}

func TestSessionsShowTheirProvenance(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	f.attribute(t, "fp-1", f.developer.ID, "feature/PAY-1183", "PAY-1183", now)

	rec, body := f.get(t, "/api/v1/sessions", f.devToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}

	s, _ := sessions[0].(map[string]any)
	if s["describe"] != "PAY-1183 - branch name (structural, high)" {
		t.Errorf("describe = %v, want the documented phrasing", s["describe"])
	}

	work, _ := s["work_item"].(map[string]any)
	for key, want := range map[string]string{
		"key": "PAY-1183", "kind": "ticket", "rung": "structural",
		"confidence": "high", "evidence": "branch name",
	} {
		if work[key] != want {
			t.Errorf("work_item.%s = %v, want %q", key, work[key], want)
		}
	}
}

// TestDeveloperSeesOnlyTheirOwnWork. The same rule as the request log, applied
// to the endpoint that would be most tempting to widen.
func TestDeveloperSeesOnlyTheirOwnWork(t *testing.T) {
	f := newConsole(t)
	now := time.Now().UTC()

	mine := f.attribute(t, "fp-dev", f.developer.ID, "feature/PAY-1", "PAY-1", now)
	theirs := f.attribute(t, "fp-admin", f.admin.ID, "feature/OPS-2", "OPS-2", now)
	f.recordIn(t, f.developer.ID, "r1", mine.ID, 1_000_000, now)
	f.recordIn(t, f.admin.ID, "r2", theirs.ID, 9_000_000, now)

	_, body := f.get(t, "/api/v1/work", f.devToken)
	rows, _ := body["by_work_item"].([]any)

	for _, r := range rows {
		row, _ := r.(map[string]any)
		if row["key"] == "OPS-2" {
			t.Error("a developer saw a work item that is not theirs")
		}
	}

	_, body = f.get(t, "/api/v1/sessions", f.devToken)
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 1 {
		t.Errorf("a developer saw %d sessions, want only their own", len(sessions))
	}

	// An admin sees the organization, which is an aggregate view rather than a
	// comparison between two named people.
	_, body = f.get(t, "/api/v1/work", f.adminToken)
	rows, _ = body["by_work_item"].([]any)
	if len(rows) != 2 {
		t.Errorf("an admin saw %d work items, want both", len(rows))
	}
}

// TestPolicyNeverRevealsAnotherPersonsSpend is the second trust commitment
// written as a test. Budgets are the one place a number attaches to a named
// individual, so this is where the line has to hold.
func TestPolicyNeverRevealsAnotherPersonsSpend(t *testing.T) {
	f := newConsole(t)

	limit := pricing.Money(20_000_000_000)
	set := policy.Set{Budgets: []policy.Budget{
		{Scope: policy.ScopeOrg, Period: policy.PeriodMonth, Name: "deployment", Enabled: true, HardLimit: &limit},
		{Scope: policy.ScopeUser, ScopeID: f.developer.ID, Period: policy.PeriodDay, Name: "dev daily", Enabled: true, HardLimit: &limit},
		{Scope: policy.ScopeUser, ScopeID: f.admin.ID, Period: policy.PeriodDay, Name: "admin daily", Enabled: true, HardLimit: &limit},
	}}
	if _, _, _, err := f.db.ApplyPolicy(t.Context(), set); err != nil {
		t.Fatalf("apply policy: %v", err)
	}
	if err := f.policy.Load(t.Context()); err != nil {
		t.Fatalf("reload policy: %v", err)
	}

	t.Run("a developer sees their own and the organization's", func(t *testing.T) {
		_, body := f.get(t, "/api/v1/policy", f.devToken)
		budgets, _ := body["budgets"].([]any)

		names := map[string]bool{}
		for _, b := range budgets {
			row, _ := b.(map[string]any)
			name, _ := row["name"].(string)
			names[name] = row["spend_visible"] == true
		}

		if !names["dev daily"] {
			t.Error("a developer cannot see their own budget's spend")
		}
		if !names["deployment"] {
			t.Error("a developer cannot see the organization's budget")
		}
		if _, present := names["admin daily"]; present {
			t.Error("a developer was shown another named person's budget")
		}
		// And no rules: the routing table names user ids.
		if _, present := body["rules"]; present {
			t.Error("a developer was served the routing rules")
		}
	})

	t.Run("an admin administers without seeing another person's spend", func(t *testing.T) {
		_, body := f.get(t, "/api/v1/policy", f.adminToken)
		budgets, _ := body["budgets"].([]any)

		var found bool
		for _, b := range budgets {
			row, _ := b.(map[string]any)
			if row["name"] != "dev daily" {
				continue
			}
			found = true

			// The limits are visible, because otherwise nobody could administer
			// them.
			limits, _ := row["limits"].(map[string]any)
			if limits["hard_usd"] == nil {
				t.Error("an admin cannot see the limit on a budget they administer")
			}
			// The spend is not, because that is the figure that would rank one
			// named person against another.
			if row["spend_visible"] != false {
				t.Error("an admin was served another named person's spend")
			}
			for _, leaked := range []string{"spent_nanos", "spent_usd", "requests", "state", "percent_of_hard_limit"} {
				if _, present := row[leaked]; present {
					t.Errorf("%q is present on another person's budget", leaked)
				}
			}
		}
		if !found {
			t.Fatal("an admin cannot see that a per-user budget exists at all")
		}
	})
}

func TestPolicyEndpointRequiresAuthentication(t *testing.T) {
	f := newConsole(t)

	for _, path := range []string{"/api/v1/policy", "/api/v1/work", "/api/v1/sessions"} {
		rec, _ := f.get(t, path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token = %d, want 401", path, rec.Code)
		}
	}
}
