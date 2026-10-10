// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
)

// TestRequestFiltersNarrowTheTrail exercises the four typed filters against
// both dialects, because the matching is the one part of this that is written
// in SQL rather than in Go: LIKE is case-sensitive on Postgres and not on
// SQLite, and a filter that quietly worked on one of them would be a console
// that answered differently depending on how the deployment was stood up.
func TestRequestFiltersNarrowTheTrail(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()
		window := RequestFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)}

		ana, err := db.CreateUser(ctx, "ana.silva@example.com", "Ana", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		bo, err := db.CreateUser(ctx, "bo@example.com", "Bo", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		gateway, err := db.AttributeSession(ctx, observe("fp-gateway", ana.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feat/PAY-1183-audit", "environment block"),
			Ticket: structural("PAY-1183", "branch name"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}
		console, err := db.AttributeSession(ctx, observe("fp-console", bo.ID, attribution.Signals{
			Repo:   structural("acme/console", "git remote"),
			Branch: structural("main", "environment block"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}

		write := func(id, userID, sessionID, model string) {
			t.Helper()
			rec := RequestRecord{
				RequestID: id, UserID: userID, SessionID: sessionID,
				StartedAt: now, Surface: "anthropic", Provider: "bedrock",
				RequestedModel: "default", ResolvedModel: model,
				Tool: "claude-code", StatusCode: 200, Decision: DecisionAllowed,
				Attempts: 1,
			}
			if rerr := db.RecordRequest(ctx, rec); rerr != nil {
				t.Fatalf("record %s: %v", id, rerr)
			}
		}
		write("a1", ana.ID, gateway.ID, "us.anthropic.claude-sonnet-5")
		write("b1", bo.ID, console.ID, "global.anthropic.claude-haiku-4-5")
		// A request no session claimed: it has no repository, so no repository
		// filter may return it.
		write("loose", bo.ID, "", "us.anthropic.claude-sonnet-5")

		ids := func(t *testing.T, f RequestFilter) []string {
			t.Helper()
			f.Since, f.Until = window.Since, window.Until
			page, perr := db.ListRequests(ctx, f)
			if perr != nil {
				t.Fatalf("list: %v", perr)
			}
			out := make([]string, 0, len(page.Requests))
			for _, r := range page.Requests {
				out = append(out, r.RequestID)
			}
			return out
		}
		only := func(t *testing.T, f RequestFilter, want string) {
			t.Helper()
			got := ids(t, f)
			if len(got) != 1 || got[0] != want {
				t.Errorf("filter %+v matched %v, want only %q", f, got, want)
			}
		}

		// Case and partiality: nobody types a whole email address into a box.
		only(t, RequestFilter{User: "ANA.SIL"}, "a1")
		only(t, RequestFilter{Repo: "panorama"}, "a1")
		only(t, RequestFilter{Branch: "pay-1183"}, "a1")
		only(t, RequestFilter{Model: "haiku"}, "b1")

		// Every filter narrows, and they narrow together.
		if got := ids(t, RequestFilter{Repo: "acme", Branch: "main"}); len(got) != 1 || got[0] != "b1" {
			t.Errorf("repo and branch together matched %v, want only b1", got)
		}
		if got := ids(t, RequestFilter{Repo: "panorama", Model: "haiku"}); len(got) != 0 {
			t.Errorf("two filters that agree on nothing matched %v, want nothing", got)
		}

		// A request with no session is not a request in every repository.
		for _, got := range ids(t, RequestFilter{Repo: "acme"}) {
			if got == "loose" {
				t.Error("a request belonging to no session was returned by a repository filter")
			}
		}

		// The pin wins: a filter cannot reach outside the scope it is ANDed
		// with, which is what keeps `filterFrom` the only place visibility is
		// decided.
		if got := ids(t, RequestFilter{UserID: ana.ID, User: "bo@"}); len(got) != 0 {
			t.Errorf("a user filter widened a pinned scope to %v", got)
		}
	})
}

// TestRequestFilterTreatsTypingAsText guards the escaping. A branch with a
// wildcard in its name is unusual; a filter that read one as a pattern would
// return everything and look like it had worked.
func TestRequestFilterTreatsTypingAsText(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		session, err := db.AttributeSession(ctx, observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feat/50%-done", "environment block"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}
		writeAttributedRequest(t, db, user.ID, "r1", session.ID, 100, now)

		list := func(f RequestFilter) int {
			t.Helper()
			f.Since, f.Until = now.Add(-time.Hour), now.Add(time.Hour)
			page, perr := db.ListRequests(ctx, f)
			if perr != nil {
				t.Fatalf("list: %v", perr)
			}
			return len(page.Requests)
		}

		if n := list(RequestFilter{Branch: "50%-done"}); n != 1 {
			t.Errorf("a branch searched for itself matched %d rows, want 1", n)
		}
		if n := list(RequestFilter{Branch: "feat/%done"}); n != 0 {
			t.Errorf("a wildcard typed into the box matched %d rows; it is text, not a pattern", n)
		}
		if n := list(RequestFilter{Branch: "_eat/50%-done"}); n != 0 {
			t.Errorf("an underscore typed into the box matched %d rows; it is text, not a pattern", n)
		}
	})
}

// TestListRequestsCarriesTheWork checks that the work a request paid for
// arrives on the row itself. Reaching for it through a second, separately
// limited query over sessions would let a row be returned by a repository
// filter and show no repository.
func TestListRequestsCarriesTheWork(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		session, err := db.AttributeSession(ctx, observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feat/PAY-1183", "environment block"),
			Ticket: structural("PAY-1183", "branch name"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}
		writeAttributedRequest(t, db, user.ID, "r1", session.ID, 100, now)

		page, err := db.ListRequests(ctx, RequestFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(page.Requests) != 1 {
			t.Fatalf("%d rows, want 1", len(page.Requests))
		}
		got := page.Requests[0]
		if got.Repo != "acme/panorama" || got.Branch != "feat/PAY-1183" {
			t.Errorf("repo/branch = %q/%q, want the session's", got.Repo, got.Branch)
		}
		if got.WorkItemKey != "PAY-1183" || got.Rung != "structural" {
			t.Errorf("work item = %q (%q), want PAY-1183 (structural)", got.WorkItemKey, got.Rung)
		}
	})
}

// TestOutcomeFilterAndCountsAgree is the guard on the audit trail's segmented
// control.
//
// The counts and the filter are two readings of one table, and the failure this
// invites is silent: a decision the filter admits but the count folds elsewhere
// makes an option say "4" and return three rows, and nothing about that looks
// like a bug from either side of the wire. It also pins the fold itself - five
// decisions into three classes - because getting `allowed` into the served
// class is the whole reason an outcome exists separately from a decision.
func TestOutcomeFilterAndCountsAgree(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()
		window := RequestFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)}

		user, err := db.CreateUser(ctx, "ana@example.com", "Ana", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		write := func(id, decision string) {
			t.Helper()
			rec := RequestRecord{
				RequestID: id, UserID: user.ID,
				StartedAt: now, Surface: "anthropic", Provider: "bedrock",
				RequestedModel: "default", ResolvedModel: "us.anthropic.claude-sonnet-5",
				Tool: "claude-code", StatusCode: 200, Decision: decision,
			}
			if rerr := db.RecordRequest(ctx, rec); rerr != nil {
				t.Fatalf("record %s: %v", id, rerr)
			}
		}
		write("served-1", DecisionAllowed)
		write("served-2", DecisionAllowed)
		write("served-3", DecisionAllowed)
		write("stopped-1", DecisionBlocked)
		write("stopped-2", DecisionRefused)
		write("failed-1", DecisionError)

		counts, err := db.CountOutcomes(ctx, window)
		if err != nil {
			t.Fatalf("count outcomes: %v", err)
		}
		if counts.Routed != 3 || counts.Blocked != 2 || counts.Errored != 1 {
			t.Errorf("counts = routed %d, blocked %d, error %d; want 3, 2, 1",
				counts.Routed, counts.Blocked, counts.Errored)
		}
		if counts.All != counts.Routed+counts.Blocked+counts.Errored {
			t.Errorf("all = %d, want the sum of the three (%d): the console shows them side by side",
				counts.All, counts.Routed+counts.Blocked+counts.Errored)
		}

		// What each option's count promises, asked of the table it labels.
		for _, outcome := range Outcomes {
			f := window
			f.Outcome = outcome
			page, perr := db.ListRequests(ctx, f)
			if perr != nil {
				t.Fatalf("list %s: %v", outcome, perr)
			}
			if int64(len(page.Requests)) != counts.Count(outcome) {
				t.Errorf("outcome %q: filter returned %d rows, count says %d",
					outcome, len(page.Requests), counts.Count(outcome))
			}
			for _, r := range page.Requests {
				if got := OutcomeOf(r.Decision); got != outcome {
					t.Errorf("outcome %q returned a %q request (decision %q)", outcome, got, r.Decision)
				}
			}
		}

		// The counts describe the window before its own outcome is applied, so
		// every option keeps the size it would select rather than collapsing to
		// the one already chosen.
		narrowed := window
		narrowed.Outcome = OutcomeError
		again, err := db.CountOutcomes(ctx, narrowed)
		if err != nil {
			t.Fatalf("count outcomes with an outcome set: %v", err)
		}
		if again != counts {
			t.Errorf("counts changed when an outcome was selected: %+v, want %+v", again, counts)
		}

		// And they still narrow with everything else. A typed filter is ANDed
		// in, so the control reports the window the reader is actually looking
		// at rather than the whole trail.
		f := window
		f.User = "nobody@example.com"
		empty, err := db.CountOutcomes(ctx, f)
		if err != nil {
			t.Fatalf("count outcomes with a user filter: %v", err)
		}
		if empty.All != 0 {
			t.Errorf("a user filter matching nobody counted %d requests", empty.All)
		}
	})
}
