// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/integrations"
)

// attributeBranch records a session working on a branch, which is what the
// gateway infers with no client configuration at all.
func attributeBranch(t *testing.T, db *DB, fingerprint, userID, repo, branch string, at time.Time) *Session {
	t.Helper()

	s, err := db.AttributeSession(t.Context(), observe(fingerprint, userID, attribution.Signals{
		Repo:   structural(repo, "git remote"),
		Branch: structural(branch, "environment block"),
	}, at))
	if err != nil {
		t.Fatalf("attribute %s: %v", fingerprint, err)
	}
	return s
}

func mergedAt(t *testing.T, text string) *time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	at = at.UTC()
	return &at
}

// TestOnlyWorkSomeoneSpentOnIsWorthAsking. Confirmation is bounded by traffic
// rather than by the size of the organisation, so a repository with ten
// thousand branches costs one lookup for the one branch someone used.
func TestOnlyWorkSomeoneSpentOnIsWorthAsking(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		attributeBranch(t, db, "fp-1", user.ID, "acme/panorama", "feat/tokenizer", now)

		// A work item nothing was ever attributed to. It exists because some
		// other pass created it; nobody spent money on it.
		if _, ierr := db.ExecContext(ctx, db.rebind(`
			INSERT INTO work_items (id, kind, key, source, first_seen_at, last_seen_at)
			VALUES (?, 'ticket', 'OLD-1', 'inferred', ?, ?)`), NewID("wrk"), now, now); ierr != nil {
			t.Fatal(ierr)
		}

		refs, err := db.WorkItemsToConfirm(ctx, now, 100)
		if err != nil {
			t.Fatalf("WorkItemsToConfirm: %v", err)
		}
		if len(refs) != 1 {
			t.Fatalf("%d references to confirm, want 1 - only the work with spend behind it", len(refs))
		}
		if refs[0].Key != "acme/panorama@feat/tokenizer" {
			t.Errorf("reference = %q, want the branch someone worked on", refs[0].Key)
		}
	})
}

// TestConfirmationFillsInWhatOnlyGitHubKnows, and creates the ticket the pull
// request named even though no session was ever attributed to it.
func TestConfirmationFillsInWhatOnlyGitHubKnows(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		attributeBranch(t, db, "fp-1", user.ID, "acme/panorama", "feat/tokenizer", now)

		refs, err := db.WorkItemsToConfirm(ctx, now, 100)
		if err != nil {
			t.Fatal(err)
		}

		result, err := db.ConfirmWorkItems(ctx, "github", refs, []integrations.WorkItem{{
			Kind: "branch", Key: "acme/panorama@feat/tokenizer",
			Repo: "acme/panorama", Branch: "feat/tokenizer",
			Title: "feat: streaming tokenizer", URL: "https://github.com/acme/panorama/pull/412",
			Author: "a.chen", Project: "acme/panorama",
			Status: "Merged", Outcome: integrations.OutcomeLanded,
			MergedAt: mergedAt(t, "2026-08-03T14:12:00Z"),
			Links:    []integrations.Link{{Kind: "ticket", Key: "PAY-1183"}},
		}}, now)
		if err != nil {
			t.Fatalf("ConfirmWorkItems: %v", err)
		}
		if result.Checked != 1 || result.Confirmed != 1 || result.Linked != 1 {
			t.Errorf("result = %+v, want one checked, confirmed, and linked", result)
		}

		branch, err := db.WorkItem(ctx, "branch", "acme/panorama@feat/tokenizer")
		if err != nil {
			t.Fatalf("WorkItem: %v", err)
		}
		if !branch.Confirmed() {
			t.Error("the branch was confirmed but does not say so")
		}
		if branch.Title != "feat: streaming tokenizer" || branch.Author != "a.chen" {
			t.Errorf("work item = %+v, want the title and author only GitHub knows", branch)
		}
		if branch.Source != "github" {
			t.Errorf("source = %q, want github now that GitHub has spoken", branch.Source)
		}

		// The ticket exists because the pull request named it, and it is
		// pointedly not confirmed: GitHub naming PAY-1183 is evidence that
		// PAY-1183 exists, not evidence of what Jira says about it.
		ticket, err := db.WorkItem(ctx, "ticket", "PAY-1183")
		if err != nil {
			t.Fatalf("the linked ticket was not created: %v", err)
		}
		if ticket.Confirmed() {
			t.Error("a ticket GitHub merely mentioned was recorded as confirmed by Jira")
		}

		links, err := db.WorkItemLinks(ctx, branch.ID)
		if err != nil {
			t.Fatalf("WorkItemLinks: %v", err)
		}
		if len(links) != 1 || links[0].Key != "PAY-1183" || links[0].Direction != "to" {
			t.Errorf("links = %+v, want the ticket this branch names", links)
		}

		// And the link reads from the other end too, which is how the ticket's
		// drill-in finds the work that implemented it.
		back, err := db.WorkItemLinks(ctx, ticket.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(back) != 1 || back[0].Direction != "from" {
			t.Errorf("reverse links = %+v, want the branch that names this ticket", back)
		}
	})
}

// TestAskingAboutSomethingThatDoesNotExistIsAnAnswer. A branch that was never
// pushed comes back unrecognised, and that must not leave it permanently at the
// front of the queue - otherwise every pass forever asks the same question.
func TestAskingAboutSomethingThatDoesNotExistIsAnAnswer(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		attributeBranch(t, db, "fp-1", user.ID, "acme/panorama", "scratch/never-pushed", now)

		refs, err := db.WorkItemsToConfirm(ctx, now, 100)
		if err != nil {
			t.Fatal(err)
		}
		if _, cerr := db.ConfirmWorkItems(ctx, "github", refs, nil, now); cerr != nil {
			t.Fatalf("ConfirmWorkItems: %v", cerr)
		}

		item, err := db.WorkItem(ctx, "branch", "acme/panorama@scratch/never-pushed")
		if err != nil {
			t.Fatal(err)
		}
		if !item.Checked() {
			t.Error("an unrecognised work item was not marked as checked, so it would be asked about forever")
		}
		if item.Confirmed() {
			t.Error("an unrecognised work item was recorded as confirmed")
		}

		// And it is not offered again until it goes stale.
		again, err := db.WorkItemsToConfirm(ctx, now.Add(-time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 0 {
			t.Errorf("%d references offered again immediately, want 0", len(again))
		}

		// It is offered again once the answer is old enough to be worth
		// re-asking, because a branch pushed tomorrow should be found.
		stale, err := db.WorkItemsToConfirm(ctx, now.Add(time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(stale) != 1 {
			t.Errorf("%d references offered once stale, want 1", len(stale))
		}
	})
}

// TestOutcomesFollowABranchToItsPullRequest is the whole reason the link table
// exists. A session is attributed to a branch, and a branch never merges - the
// pull request it was opened as does.
func TestOutcomesFollowABranchToItsPullRequest(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		landed := attributeBranch(t, db, "fp-1", user.ID, "acme/panorama", "feat/tokenizer", now)
		dropped := attributeBranch(t, db, "fp-2", user.ID, "acme/panorama", "spike/vector-cache", now)
		writeAttributedRequest(t, db, user.ID, "r1", landed.ID, 6_000_000, now)
		writeAttributedRequest(t, db, user.ID, "r2", dropped.ID, 3_000_000, now)
		writeAttributedRequest(t, db, user.ID, "r3", "", 1_000_000, now)

		refs, err := db.WorkItemsToConfirm(ctx, now, 100)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ConfirmWorkItems(ctx, "github", refs, []integrations.WorkItem{
			{
				Kind: "branch", Key: "acme/panorama@feat/tokenizer",
				Links: []integrations.Link{{Kind: "pull_request", Key: "acme/panorama#412"}},
			},
			{
				Kind: "branch", Key: "acme/panorama@spike/vector-cache",
				Links: []integrations.Link{{Kind: "pull_request", Key: "acme/panorama#418"}},
			},
			{
				Kind: "pull_request", Key: "acme/panorama#412",
				Status: "Merged", Outcome: integrations.OutcomeLanded,
				MergedAt: mergedAt(t, "2026-08-03T14:12:00Z"),
			},
			{
				Kind: "pull_request", Key: "acme/panorama#418",
				Status: "Closed", Outcome: integrations.OutcomeAbandoned,
			},
		}, now)
		if err != nil {
			t.Fatalf("ConfirmWorkItems: %v", err)
		}

		outcomes, err := db.Outcomes(ctx, RequestFilter{})
		if err != nil {
			t.Fatalf("Outcomes: %v", err)
		}
		if !outcomes.Confirmed {
			t.Fatal("Outcomes reports nothing confirmed after two pull requests were")
		}

		got := map[string]int64{}
		for _, b := range outcomes.ByOutcome {
			got[b.Outcome] = int64(b.Cost.Total)
		}
		if got["landed"] != 6_000_000 {
			t.Errorf("landed spend = %d, want the branch's spend to follow its merged pull request", got["landed"])
		}
		if got["abandoned"] != 3_000_000 {
			t.Errorf("abandoned spend = %d, want 3000000", got["abandoned"])
		}
		// The unattributed request has no outcome, and it is a row of its own
		// rather than being folded in with work that is merely still open.
		if got[""] != 1_000_000 {
			t.Errorf("spend with no outcome = %d, want the unattributed request's 1000000", got[""])
		}

		if outcomes.LandedPullRequests != 1 {
			t.Errorf("landed pull requests = %d, want 1", outcomes.LandedPullRequests)
		}
		if int64(outcomes.LandedPullRequestCost) != 6_000_000 {
			t.Errorf("cost against landed pull requests = %d, want 6000000", outcomes.LandedPullRequestCost)
		}
	})
}

// TestConfirmationDoesNotMoveTheAttribution. The session stays on the branch it
// was observed working on. Rewriting it to the pull request on GitHub's say-so
// would make the provenance trail a claim about a system Panorama did not
// observe.
func TestConfirmationDoesNotMoveTheAttribution(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		attributeBranch(t, db, "fp-1", user.ID, "acme/panorama", "feat/tokenizer", now)

		refs, err := db.WorkItemsToConfirm(ctx, now, 100)
		if err != nil {
			t.Fatal(err)
		}
		if _, cerr := db.ConfirmWorkItems(ctx, "github", refs, []integrations.WorkItem{{
			Kind: "branch", Key: "acme/panorama@feat/tokenizer",
			Outcome: integrations.OutcomeLanded,
			Links:   []integrations.Link{{Kind: "ticket", Key: "PAY-1183"}},
		}}, now); cerr != nil {
			t.Fatal(cerr)
		}

		session, err := db.SessionByFingerprint(ctx, "fp-1")
		if err != nil {
			t.Fatal(err)
		}
		if session.WorkItemKey != "acme/panorama@feat/tokenizer" {
			t.Errorf("session moved to %q; confirmation must not re-attribute", session.WorkItemKey)
		}
		if session.Rung != string(attribution.RungStructural) {
			t.Errorf("rung = %q, want it unchanged at structural", session.Rung)
		}
	})
}

// TestASourceReturningRubbishIsRefused. The outcome numbers read exactly one
// column, and a source writing an unknown value into it would be a metric that
// silently gains a category.
func TestASourceReturningRubbishIsRefused(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		_, err := db.ConfirmWorkItems(t.Context(), "github", nil, []integrations.WorkItem{{
			Kind: "pull_request", Key: "r#1", Outcome: "shipped",
		}}, time.Now())
		if err == nil {
			t.Fatal("ConfirmWorkItems accepted an outcome nobody defined")
		}
	})
}

// TestIntegrationHealthIsRecordedAndKept. A failure that has cleared is still
// worth reading: an intermittent integration is the one an operator has to
// chase, and a screen that shows only the current state hides it.
func TestIntegrationHealthIsRecordedAndKept(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		if err := db.RegisterIntegration(ctx, "github", "mock", "mock source - 3 fixtures"); err != nil {
			t.Fatalf("RegisterIntegration: %v", err)
		}

		states, err := db.ListIntegrations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(states) != 1 || states[0].Status != IntegrationUnknown {
			t.Fatalf("states = %+v, want one integration in the unknown state before the first check", states)
		}
		if states[0].Provider != "mock" {
			t.Errorf("provider = %q; a fixtures-backed integration must say so", states[0].Provider)
		}

		if cerr := db.RecordIntegrationCheck(ctx, "github", errUnreachable{}); cerr != nil {
			t.Fatal(cerr)
		}
		if cerr := db.RecordIntegrationCheck(ctx, "github", nil); cerr != nil {
			t.Fatal(cerr)
		}

		states, err = db.ListIntegrations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if states[0].Status != IntegrationOK {
			t.Errorf("status = %q after a successful check, want ok", states[0].Status)
		}
		if states[0].LastError == "" {
			t.Error("the previous failure was erased by a success; an intermittent integration would be invisible")
		}

		if serr := db.RecordIntegrationSync(ctx, "github", ConfirmResult{Checked: 9, Confirmed: 4}); serr != nil {
			t.Fatal(serr)
		}
		states, err = db.ListIntegrations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if states[0].LastCheckedItems != 9 || states[0].LastConfirmedItems != 4 {
			t.Errorf("last run = %d checked, %d confirmed; want 9 and 4", states[0].LastCheckedItems, states[0].LastConfirmedItems)
		}
	})
}

type errUnreachable struct{}

func (errUnreachable) Error() string { return "dial tcp: connection refused" }

// TestRegisteringAgainKeepsTheHealth. A restart is not evidence that the last
// failure is over.
func TestRegisteringAgainKeepsTheHealth(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		if err := db.RegisterIntegration(ctx, "jira", "mock", "first"); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordIntegrationCheck(ctx, "jira", errUnreachable{}); err != nil {
			t.Fatal(err)
		}
		if err := db.RegisterIntegration(ctx, "jira", "mock", "second"); err != nil {
			t.Fatal(err)
		}

		states, err := db.ListIntegrations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if states[0].Status != IntegrationError {
			t.Errorf("status = %q after re-registering, want the failure to survive a restart", states[0].Status)
		}
		if states[0].Detail != "second" {
			t.Errorf("detail = %q, want it refreshed to the current configuration", states[0].Detail)
		}
	})
}
