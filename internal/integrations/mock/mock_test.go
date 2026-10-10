// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package mock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dnxlabs/panorama/internal/integrations"
)

// write drops a fixtures file into a temporary directory and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixtures.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixtures: %v", err)
	}
	return path
}

const complete = `
github:
  - key: acme/panorama#412
    repo: acme/panorama
    branch: feat/streaming-tokenizer
    title: "feat: streaming tokenizer"
    url: https://github.com/acme/panorama/pull/412
    author: a.chen
    project: acme/panorama
    status: Merged
    outcome: landed
    created_at: 2026-08-01T09:00:00Z
    merged_at: 2026-08-03T14:12:00Z
    links:
      - kind: ticket
        key: PAY-1183
  - kind: branch
    key: acme/panorama@feat/streaming-tokenizer
    repo: acme/panorama
    branch: feat/streaming-tokenizer
    links:
      - kind: pull_request
        key: acme/panorama#412
jira:
  - key: PAY-1183
    title: Streaming tokenizer for card capture
    url: https://example.atlassian.net/browse/PAY-1183
    project: PAY
    status: Done
    outcome: landed
`

func TestLookupAnswersOnlyWhatItHas(t *testing.T) {
	path := write(t, complete)

	gh, err := Open(integrations.KindGitHub, path)
	if err != nil {
		t.Fatalf("open github fixtures: %v", err)
	}

	got, err := gh.Lookup(t.Context(), []Ref{
		{Kind: "pull_request", Key: "acme/panorama#412"},
		{Kind: "pull_request", Key: "acme/panorama#999"},
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("%d work items, want 1 - an unknown key is not found, not an error", len(got))
	}

	pr := got[0]
	if pr.Title != "feat: streaming tokenizer" || pr.Outcome != integrations.OutcomeLanded {
		t.Errorf("pull request = %+v, want it confirmed and landed", pr)
	}
	if pr.MergedAt == nil {
		t.Error("a merged pull request came back with no merge timestamp")
	}
	if len(pr.Links) != 1 || pr.Links[0].Key != "PAY-1183" {
		t.Errorf("links = %v, want the ticket it implements", pr.Links)
	}
}

// TestASourceAnswersOnlyForItsOwnSystem. The registry routes tickets to Jira
// and pull requests to GitHub; a mock that answered across that line would let
// a test pass against a routing the real deployment does not do.
func TestASourceAnswersOnlyForItsOwnSystem(t *testing.T) {
	path := write(t, complete)

	gh, err := Open(integrations.KindGitHub, path)
	if err != nil {
		t.Fatalf("open github fixtures: %v", err)
	}
	got, err := gh.Lookup(t.Context(), []Ref{{Kind: "ticket", Key: "PAY-1183"}})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("the GitHub source answered for a ticket: %+v", got)
	}

	jira, err := Open(integrations.KindJira, path)
	if err != nil {
		t.Fatalf("open jira fixtures: %v", err)
	}
	got, err = jira.Lookup(t.Context(), []Ref{{Kind: "ticket", Key: "PAY-1183"}})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(got) != 1 || got[0].Project != "PAY" {
		t.Errorf("jira lookup = %+v, want the ticket with its project", got)
	}
}

// TestDescribeSaysItIsAMock is a product requirement rather than a nicety. The
// console prints this string beside numbers that came out of a file, and a
// screenshot where fixtures look like GitHub is a lie about a real deployment.
func TestDescribeSaysItIsAMock(t *testing.T) {
	gh, err := Open(integrations.KindGitHub, write(t, complete))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(gh.Describe(), "mock") {
		t.Errorf("Describe() = %q, and does not say it is a mock", gh.Describe())
	}
}

func TestFixturesAreRefusedRatherThanPatched(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"a key that is not a work item kind": {
			body: "github:\n  - kind: epic\n    key: PAY\n",
			want: "not a work item kind",
		},
		"no key": {
			body: "github:\n  - title: nameless\n",
			want: "key is required",
		},
		"a misspelled field": {
			body: "jira:\n  - key: PAY-1\n    titel: typo\n",
			want: "field titel not found",
		},
		"an outcome nobody defined": {
			body: "jira:\n  - key: PAY-1\n    outcome: shipped\n",
			want: "is not an outcome",
		},
		"a timestamp that is not one": {
			body: "jira:\n  - key: PAY-1\n    created_at: last tuesday\n",
			want: "RFC 3339",
		},
		"merged without landing": {
			body: "github:\n  - key: r#1\n    outcome: open\n    merged_at: 2026-08-01T00:00:00Z\n",
			want: "merged_at is set but the outcome",
		},
		"the same item twice": {
			body: "jira:\n  - key: PAY-1\n  - key: PAY-1\n",
			want: "appears twice",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Open(integrations.KindJira, write(t, tc.body))
			if err == nil {
				t.Fatal("Open accepted fixtures it should have refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestEveryProblemIsReportedAtOnce. A fixtures file is usually wrong in several
// places the first time it is written, and fixing them one error per run is the
// slowest possible way to find that out.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	_, err := Open(integrations.KindJira, write(t,
		"jira:\n  - key: PAY-1\n    outcome: shipped\n  - title: nameless\n"))
	if err == nil {
		t.Fatal("Open accepted two broken fixtures")
	}
	if !strings.Contains(err.Error(), "is not an outcome") || !strings.Contains(err.Error(), "key is required") {
		t.Errorf("error = %v, want both problems named", err)
	}
}

// TestAMockWithNoFixturesIsRefused. Answering nothing at all is
// indistinguishable from a broken integration, so the configuration is rejected
// at startup instead.
func TestAMockWithNoFixturesIsRefused(t *testing.T) {
	if _, err := Open(integrations.KindGitHub, ""); err == nil {
		t.Error("Open accepted a mock source with no fixtures file")
	}
	if _, err := Open(integrations.Kind("clickup"), write(t, complete)); err == nil {
		t.Error("Open accepted a system this package cannot stand in for")
	}
}

// TestTheExampleFixturesAreValid. The file shipped in the repository is the one
// anybody starting from a mock source copies, and a documented example that
// does not parse is worse than none - it makes the reader doubt the format
// rather than their typing.
func TestTheExampleFixturesAreValid(t *testing.T) {
	const path = "../../../integrations-fixtures.example.yaml"

	gh, err := Open(integrations.KindGitHub, path)
	if err != nil {
		t.Fatalf("the example fixtures do not parse: %v", err)
	}
	jira, err := Open(integrations.KindJira, path)
	if err != nil {
		t.Fatalf("the example fixtures do not parse: %v", err)
	}

	// The example has to cover the case the outcome number is actually computed
	// from: a branch with no ticket key in its name, whose spend reaches a
	// merged pull request only through the link. A file where every branch
	// names a ticket never exercises that path, and somebody copying it would
	// conclude cost per merged pull request does not work.
	branches, err := gh.Lookup(t.Context(), []Ref{
		{Kind: "branch", Key: "acme/panorama@chore/bump-aws-sdk"},
	})
	if err != nil || len(branches) != 1 {
		t.Fatalf("the example has no ticket-free branch: %v %v", branches, err)
	}
	if len(branches[0].Links) != 1 {
		t.Fatal("the ticket-free branch links to no pull request, so nothing carries its spend to an outcome")
	}

	merged, err := gh.Lookup(t.Context(), []Ref{{Kind: "pull_request", Key: branches[0].Links[0].Key}})
	if err != nil || len(merged) != 1 {
		t.Fatalf("the branch links to a pull request that is not in the file: %v", err)
	}
	if merged[0].Outcome != integrations.OutcomeLanded {
		t.Errorf("the linked pull request is %q, want one that landed", merged[0].Outcome)
	}

	tickets, err := jira.Lookup(t.Context(), []Ref{{Kind: "ticket", Key: "PAY-1183"}})
	if err != nil || len(tickets) != 1 {
		t.Fatalf("the example has no confirmable ticket: %v", err)
	}
}

func TestCheckFailsWhenTheFixturesGoAway(t *testing.T) {
	path := write(t, complete)
	gh, err := Open(integrations.KindGitHub, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := gh.Check(t.Context()); err != nil {
		t.Fatalf("Check on a readable file = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := gh.Check(t.Context()); err == nil {
		t.Error("Check passed after the fixtures file was removed")
	}
}
