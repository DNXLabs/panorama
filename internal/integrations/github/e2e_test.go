// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package github_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/integrations/github"
	"github.com/dnxlabs/panorama/internal/jobs"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// TestCostPerMergedPullRequestEndToEnd is §2's whole reason for existing.
//
// It runs the real GitHub source, against a real HTTP server, through the real
// confirmation job, into the real database, and reads the headline number back
// out of the real query. Everything between "a session was attributed to a
// branch" and "this is what a merged pull request cost" is exercised, including
// the one-hop rollup from branch to pull request that the number depends on.
//
// The parts most worth having covered here rather than in a unit test are the
// joins: a branch does not merge, so its spend has to travel exactly one link
// to the pull request that did, and stop there.
func TestCostPerMergedPullRequestEndToEnd(t *testing.T) {
	ctx := t.Context()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		merged := map[string]any{
			"number": 412, "title": "Stream the tokenizer",
			"html_url": "https://github.com/acme/app/pull/412",
			"state":    "closed", "merged_at": "2026-08-03T10:00:00Z",
			"closed_at": "2026-08-03T10:00:00Z",
			"user":      map[string]any{"login": "dana"},
			"head":      map[string]any{"ref": "feat/tokenizer"},
			"base":      map[string]any{"repo": map[string]any{"full_name": "acme/app"}},
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/rate_limit":
			_ = json.NewEncoder(w).Encode(map[string]any{"rate": map[string]any{"remaining": 5000}})
		case r.URL.Path == "/repos/acme/app/pulls" && strings.Contains(r.URL.RawQuery, "feat%2Ftokenizer"):
			_ = json.NewEncoder(w).Encode([]any{merged})
		case r.URL.Path == "/repos/acme/app/pulls/412":
			_ = json.NewEncoder(w).Encode(merged)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	db := openDB(t)
	spend := recordAttributedSpend(t, db, "acme/app", "feat/tokenizer")

	src, err := github.Open(github.Config{APIBaseURL: api.URL, Token: "ghp_test"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	confirmer := jobs.NewConfirmer(jobs.ConfirmerOptions{
		DB: db, Resolver: integrations.Static(src),
	})
	if rerr := confirmer.Register(ctx); rerr != nil {
		t.Fatalf("Register: %v", rerr)
	}
	if rerr := confirmer.RunOnce(ctx); rerr != nil {
		t.Fatalf("RunOnce: %v", rerr)
	}

	// The branch was confirmed and linked, and did not take the merge with it.
	branch, err := db.WorkItem(ctx, attribution.KindBranch, "acme/app@feat/tokenizer")
	if err != nil {
		t.Fatalf("reading the branch: %v", err)
	}
	if branch.Outcome != string(integrations.OutcomeOpen) {
		t.Errorf("branch outcome = %q, want open - a branch never merges", branch.Outcome)
	}
	if branch.ConfirmedAt == nil {
		t.Error("the branch was not marked confirmed")
	}

	// The pull request it was opened as carries the merge.
	pr, err := db.WorkItem(ctx, attribution.KindPullRequest, "acme/app#412")
	if err != nil {
		t.Fatalf("the pull request the branch was opened as was never created: %v", err)
	}
	if pr.Outcome != string(integrations.OutcomeLanded) {
		t.Errorf("pull request outcome = %q, want landed", pr.Outcome)
	}
	if pr.Title != "Stream the tokenizer" || pr.Author != "dana" {
		t.Errorf("pull request = %+v, want the confirmed title and author", pr)
	}

	// And the number the product is sold on.
	summary, err := db.Outcomes(ctx, store.RequestFilter{
		Since: time.Now().UTC().Add(-time.Hour), Until: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Outcomes: %v", err)
	}
	if !summary.Confirmed {
		t.Error("Outcomes reports nothing confirmed after a successful run")
	}
	if summary.LandedPullRequests != 1 {
		t.Fatalf("landed pull requests = %d, want 1", summary.LandedPullRequests)
	}
	if summary.LandedPullRequestCost != spend {
		t.Errorf("cost attributed to the merged pull request = %d, want %d - "+
			"the branch's spend has to travel exactly one link to the pull request that merged",
			summary.LandedPullRequestCost, spend)
	}
}

func openDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(t.Context(), config.Database{
		Driver: config.DriverSQLite,
		DSN: "file:" + filepath.Join(t.TempDir(), "panorama.db") +
			"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)",
		MaxOpenConns: 1,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if merr := db.Migrate(t.Context()); merr != nil {
		t.Fatalf("migrate: %v", merr)
	}
	return db
}

// recordAttributedSpend puts one priced, attributed request into the database
// and returns what it cost - which is what makes its branch worth asking about.
func recordAttributedSpend(t *testing.T, db *store.DB, repo, branch string) pricing.Money {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()

	user, err := db.EnsureUser(ctx, "dev@example.com", "Dev", store.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}

	session, err := db.AttributeSession(ctx, store.Observation{
		Fingerprint: "fp-1", Method: "conversation-prefix",
		UserID: user.ID, Tool: "claude-code", At: now,
		Signals: attribution.Signals{
			Repo:   attribution.Observation{Value: repo, Rung: attribution.RungStructural, Evidence: "git remote"},
			Branch: attribution.Observation{Value: branch, Rung: attribution.RungStructural, Evidence: "environment block"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cost := &pricing.Cost{Input: 3_000_000, Output: 1_000_000, Total: 4_000_000}
	if rerr := db.RecordRequest(ctx, store.RequestRecord{
		RequestID: "req-1", UserID: user.ID, StartedAt: now,
		Surface: "anthropic", Provider: "bedrock",
		RequestedModel: "claude-sonnet-5", ResolvedModel: "us.anthropic.claude-sonnet-5",
		Tool: "claude-code", StatusCode: 200, Decision: store.DecisionAllowed,
		InputTokens: 100, OutputTokens: 20, Attempts: 1, SessionID: session.ID,
		Cost: cost, CostSource: pricing.SourceSeed,
	}); rerr != nil {
		t.Fatal(rerr)
	}
	return cost.Total
}
