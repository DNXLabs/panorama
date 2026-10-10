// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/pricing"
)

func structural(value, evidence string) attribution.Observation {
	return attribution.Observation{Value: value, Rung: attribution.RungStructural, Evidence: evidence}
}

func observe(fingerprint, userID string, signals attribution.Signals, at time.Time) Observation {
	return Observation{
		Fingerprint: fingerprint,
		Method:      "conversation-prefix",
		UserID:      userID,
		Tool:        "claude-code",
		At:          at,
		Signals:     signals,
	}
}

func TestAttributeSessionCreatesTheWorkItem(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		now := time.Now().UTC()
		session, err := db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feature/PAY-1183-costs", "environment block"),
			Ticket: structural("PAY-1183", "branch name"),
		}, now))
		if err != nil {
			t.Fatalf("attribute: %v", err)
		}

		if !session.Attributed() {
			t.Fatal("a session with a ticket in its branch was not attributed")
		}
		if session.WorkItemKey != "PAY-1183" || session.WorkItemKind != "ticket" {
			t.Errorf("work item = %s %q, want ticket PAY-1183", session.WorkItemKind, session.WorkItemKey)
		}
		if got := session.Describe(); got != "PAY-1183 - branch name (structural, high)" {
			t.Errorf("describe = %q", got)
		}
		if session.Repo != "acme/panorama" || session.Branch != "feature/PAY-1183-costs" {
			t.Errorf("session context = %q / %q", session.Repo, session.Branch)
		}
		if session.Requests != 1 {
			t.Errorf("requests = %d, want 1", session.Requests)
		}
	})
}

// TestSessionInheritsAcrossTurns is the session rule made checkable:
// attribute once, inherit, and do not re-guess per request.
func TestSessionInheritsAcrossTurns(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()

		full := attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feature/PAY-1183-costs", "environment block"),
			Ticket: structural("PAY-1183", "branch name"),
		}
		if _, aerr := db.AttributeSession(t.Context(), observe("fp-1", user.ID, full, now)); aerr != nil {
			t.Fatal(aerr)
		}

		// The next twenty turns reveal nothing - the cache skipped the scan -
		// and the session must keep what it already knows.
		var session *Session
		for i := range 20 {
			session, err = db.AttributeSession(t.Context(),
				observe("fp-1", user.ID, attribution.Signals{}, now.Add(time.Duration(i)*time.Second)))
			if err != nil {
				t.Fatal(err)
			}
		}

		if session.WorkItemKey != "PAY-1183" {
			t.Errorf("work item = %q after quiet turns, want it inherited", session.WorkItemKey)
		}
		if session.Repo != "acme/panorama" {
			t.Errorf("repo = %q; a turn with no environment block erased what an earlier one established", session.Repo)
		}
		if session.Requests != 21 {
			t.Errorf("requests = %d, want 21", session.Requests)
		}
	})
}

// TestAttributionUpgradesButDoesNotChurn: only strictly better evidence moves a
// session, because a work item that flickers is worse than either value.
func TestAttributionUpgradesButDoesNotChurn(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()

		// A branch first.
		session, err := db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("spike", "environment block"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}
		if session.WorkItemKind != "branch" {
			t.Fatalf("kind = %q, want branch", session.WorkItemKind)
		}

		// A stamp arrives: strictly better, so it wins.
		session, err = db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Ticket: attribution.Observation{Value: "OPS-9", Rung: attribution.RungStamp, Evidence: "client header"},
		}, now))
		if err != nil {
			t.Fatal(err)
		}
		if session.WorkItemKey != "OPS-9" || session.Rung != "stamp" {
			t.Errorf("work item = %q (%s), want the stamped ticket", session.WorkItemKey, session.Rung)
		}

		// A structural branch afterwards must not pull it back down.
		session, err = db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("spike", "environment block"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}
		if session.WorkItemKey != "OPS-9" {
			t.Errorf("work item = %q; weaker evidence overwrote a stamp", session.WorkItemKey)
		}
	})
}

func TestWorkItemsAreSharedAcrossSessions(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		alice, err := db.CreateUser(t.Context(), "alice@example.com", "Alice", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		bob, err := db.CreateUser(t.Context(), "bob@example.com", "Bob", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()

		signals := attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feature/PAY-1183", "environment block"),
			Ticket: structural("PAY-1183", "branch name"),
		}

		if _, err := db.AttributeSession(t.Context(), observe("fp-a", alice.ID, signals, now)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AttributeSession(t.Context(), observe("fp-b", bob.ID, signals, now)); err != nil {
			t.Fatal(err)
		}

		var n int
		if err := db.QueryRowContext(t.Context(),
			`SELECT count(*) FROM work_items WHERE key = 'PAY-1183'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			// Two people on one ticket is the normal case, and it has to roll up
			// to one line of spend rather than two.
			t.Errorf("%d work items for one ticket, want 1", n)
		}
	})
}

// TestUnattributedSpendIsARow, not an omission. This is the third trust
// commitment expressed as a query result.
func TestUnattributedSpendIsARow(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()

		session, err := db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feature/PAY-1183", "environment block"),
			Ticket: structural("PAY-1183", "branch name"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}

		// Attributed work.
		writeAttributedRequest(t, db, user.ID, "r1", session.ID, 3_000, now)
		// And a request nothing could be placed against.
		writeAttributedRequest(t, db, user.ID, "r2", "", 7_000, now)

		filter := RequestFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)}

		buckets, err := db.SummarizeWork(t.Context(), filter)
		if err != nil {
			t.Fatalf("summarize: %v", err)
		}

		var attributed, unattributed *WorkBucket
		for i := range buckets {
			if buckets[i].Attributed() {
				attributed = &buckets[i]
			} else {
				unattributed = &buckets[i]
			}
		}

		if attributed == nil || attributed.Key != "PAY-1183" {
			t.Fatalf("no attributed bucket: %+v", buckets)
		}
		if attributed.Cost.Total != 3_000 {
			t.Errorf("attributed cost = %d, want 3000", attributed.Cost.Total)
		}
		if attributed.Rung != "structural" {
			t.Errorf("bucket rung = %q, want structural", attributed.Rung)
		}
		if unattributed == nil {
			t.Fatal("unattributed spend was dropped from the rollup rather than shown as its own row")
		}
		if unattributed.Cost.Total != 7_000 {
			t.Errorf("unattributed cost = %d, want 7000", unattributed.Cost.Total)
		}

		coverage, err := db.Coverage(t.Context(), filter)
		if err != nil {
			t.Fatalf("coverage: %v", err)
		}
		if coverage.Requests != 2 || coverage.AttributedRequests != 1 {
			t.Errorf("coverage = %d of %d requests, want 1 of 2", coverage.AttributedRequests, coverage.Requests)
		}
		if coverage.Cost != 10_000 || coverage.AttributedCost != 3_000 {
			t.Errorf("coverage cost = %d of %d, want 3000 of 10000", coverage.AttributedCost, coverage.Cost)
		}
	})
}

// TestUpgradingASessionMovesEveryRequestWithIt is why the request row carries a
// session rather than a work item.
func TestUpgradingASessionMovesEveryRequestWithIt(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()

		session, err := db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("spike", "environment block"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}

		// Five turns land against the branch.
		for i := range 5 {
			writeAttributedRequest(t, db, user.ID, "early-"+itoa(i), session.ID, 1_000, now)
		}

		// Then the client starts stamping, and the whole session moves.
		if _, aerr := db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Ticket: attribution.Observation{Value: "PAY-1183", Rung: attribution.RungStamp, Evidence: "client header"},
		}, now)); aerr != nil {
			t.Fatal(aerr)
		}

		buckets, err := db.SummarizeWork(t.Context(),
			RequestFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}

		for _, b := range buckets {
			if b.Key == "acme/panorama@spike" {
				t.Errorf("%d requests are still billed to the branch after the session was upgraded", b.Requests)
			}
			if b.Key == "PAY-1183" && b.Cost.Total != 5_000 {
				t.Errorf("the ticket has %d, want all 5000 of the session's spend", b.Cost.Total)
			}
		}
	})
}

func TestSummarizeReposGroupsBySession(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()

		// A session that knows its repository but has no work item: it must
		// still land under the repository rather than in the unattributed row.
		session, err := db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Repo: structural("acme/panorama", "git remote"),
		}, now))
		if err != nil {
			t.Fatal(err)
		}
		writeAttributedRequest(t, db, user.ID, "r1", session.ID, 2_500, now)

		repos, err := db.SummarizeRepos(t.Context(),
			RequestFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}

		var found bool
		for _, r := range repos {
			if r.Key == "acme/panorama" && r.Cost.Total == 2_500 {
				found = true
			}
		}
		if !found {
			t.Errorf("the repository rollup is missing the session's spend: %+v", repos)
		}
	})
}

func TestListSessionsShowsItsWorking(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		user, err := db.CreateUser(t.Context(), "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()

		if _, aerr := db.AttributeSession(t.Context(), observe("fp-1", user.ID, attribution.Signals{
			Repo:   structural("acme/panorama", "git remote"),
			Branch: structural("feature/PAY-1183", "environment block"),
			Ticket: structural("PAY-1183", "branch name"),
		}, now)); aerr != nil {
			t.Fatal(aerr)
		}

		sessions, err := db.ListSessions(t.Context(), RequestFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("%d sessions, want 1", len(sessions))
		}

		s := sessions[0]
		if s.UserEmail != "dev@example.com" {
			t.Errorf("email = %q", s.UserEmail)
		}
		if s.Evidence != "branch name" || s.Rung != "structural" || s.Confidence != "high" {
			t.Errorf("provenance = %q/%q/%q, want the full trail", s.Evidence, s.Rung, s.Confidence)
		}
	})
}

func TestAttributeSessionNeedsAFingerprint(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		_, err := db.AttributeSession(t.Context(), Observation{At: time.Now()})
		if err == nil {
			t.Error("a session with no fingerprint was accepted; it would collect unrelated requests")
		}
	})
}

// writeAttributedRequest records one request against a session.
func writeAttributedRequest(t *testing.T, db *DB, userID, requestID, sessionID string, cost int64, at time.Time) {
	t.Helper()

	rec := RequestRecord{
		RequestID: requestID, UserID: userID, StartedAt: at,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "claude-sonnet-5", ResolvedModel: "us.anthropic.claude-sonnet-5",
		Tool: "claude-code", StatusCode: 200, Decision: DecisionAllowed,
		InputTokens: 100, OutputTokens: 50,
		Cost:       &pricing.Cost{Input: pricing.Money(cost), Total: pricing.Money(cost)},
		CostSource: pricing.SourceSeed,
		Attempts:   1,
		SessionID:  sessionID,
	}
	if err := db.RecordRequest(t.Context(), rec); err != nil {
		t.Fatalf("record %s: %v", requestID, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
