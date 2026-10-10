// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package attribution

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The bodies in this file are built with encoding/json rather than written out
// as string literals, and that is the point of them.
//
// Everything this package matches lives inside JSON string values, where a
// newline is the two characters \ and n and a tab is \ and t. A test written as
// a hand-typed literal proves the patterns work against text a human formatted;
// marshalling the same content proves they work against what a client actually
// sends. The two are not the same document, and an extractor written without
// this distinction matches nothing in production.

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// claudeCodeRequest builds a request shaped like the one a coding agent sends.
type claudeCodeRequest struct {
	system   string
	messages []string
	metadata string
}

func (c claudeCodeRequest) body(t *testing.T) []byte {
	t.Helper()

	msgs := make([]message, 0, len(c.messages))
	for i, text := range c.messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, message{Role: role, Content: []block{{Type: "text", Text: text}}})
	}

	req := map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 8192,
		"messages":   msgs,
		"system":     []block{{Type: "text", Text: c.system}},
	}
	if c.metadata != "" {
		req["metadata"] = map[string]string{"user_id": c.metadata}
	}

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return body
}

// currentSystemPrompt is the environment block a current Claude Code writes.
const currentSystemPrompt = `You are Claude Code, Anthropic's official CLI for Claude.

# Environment
You have been invoked in the following environment:
 - Primary working directory: /Users/dev/src/panorama
 - Is a git repository: true
 - Platform: darwin

gitStatus: This is the git status at the start of the conversation. Note that this status is a snapshot in time, and will not update during the conversation.

Current branch: feature/PAY-1183-cache-aware-costs

Main branch (you will usually use this for PRs): main

Status:
 M internal/pricing/money.go
?? internal/pricing/money_test.go

Recent commits:
26428f0 M2: metering and cache-aware pricing
409a346 M1: proxy core
`

// legacySystemPrompt is the <env> block an older Claude Code wrote. Both are
// supported: a client upgrade must not silently stop attribution.
const legacySystemPrompt = `You are Claude Code.

<env>
Working directory: /home/dev/checkouts/billing-service
Is directory a git repo: Yes
Platform: linux
Today's date: 2026-08-15
</env>
`

const gitRemoteOutput = `origin	git@github.com:acme/panorama.git (fetch)
origin	git@github.com:acme/panorama.git (push)
`

const ghPullRequestOutput = `title:	M2: metering and cache-aware pricing
state:	OPEN
url:	https://github.com/acme/panorama/pull/412
`

func newExtractor(t *testing.T, pattern string) *Extractor {
	t.Helper()
	e, err := NewExtractor(pattern)
	if err != nil {
		t.Fatalf("NewExtractor(%q): %v", pattern, err)
	}
	return e
}

// TestStructuralFromACurrentClaudeCodeRequest is the M4 gate in one test: an
// unmodified client, no configuration, and the branch comes out.
func TestStructuralFromACurrentClaudeCodeRequest(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system:   currentSystemPrompt,
		messages: []string{"Add cache-aware pricing", "Looking at the price table now."},
	}.body(t)

	// Sanity: the fixture really is escaped, so this test is testing what it
	// claims to test.
	if !strings.Contains(string(body), `Current branch: feature/PAY-1183-cache-aware-costs\n`) {
		t.Fatal("the fixture is not JSON-escaped; this test would prove nothing")
	}

	s := e.Structural(body)

	if got := s.Branch.Value; got != "feature/PAY-1183-cache-aware-costs" {
		t.Errorf("branch = %q, want the branch from the environment block", got)
	}
	if s.Branch.Rung != RungStructural {
		t.Errorf("branch rung = %q, want structural", s.Branch.Rung)
	}
	if got := s.Directory.Value; got != "panorama" {
		t.Errorf("directory = %q, want panorama", got)
	}
	if got := s.Ticket.Value; got != "PAY-1183" {
		t.Errorf("ticket = %q, want PAY-1183 from the branch name", got)
	}
	if s.Ticket.Rung != RungStructural || s.Ticket.Evidence != "branch name" {
		t.Errorf("ticket provenance = %q/%q, want structural/branch name", s.Ticket.Rung, s.Ticket.Evidence)
	}

	a, ok := Attribute(s)
	if !ok {
		t.Fatal("nothing was attributed from a request that names its branch and ticket")
	}
	if a.Kind != KindTicket || a.Key != "PAY-1183" {
		t.Errorf("attributed to %s %q, want ticket PAY-1183", a.Kind, a.Key)
	}
	if got := a.Describe(); got != "PAY-1183 - branch name (structural, high)" {
		t.Errorf("describe = %q, want the documented phrasing", got)
	}
}

// TestWorkingDirectoryKeepsOnlyItsLastElement: the full path is the one
// extracted value that would say something about a person rather than about
// their work.
func TestWorkingDirectoryKeepsOnlyItsLastElement(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system:   currentSystemPrompt,
		messages: []string{"hello"},
	}.body(t)

	s := e.Structural(body)
	if strings.Contains(s.Directory.Value, "/") || strings.Contains(s.Directory.Value, "dev") {
		t.Errorf("directory = %q, want only the last element - the path names a person", s.Directory.Value)
	}
}

// TestWorkingDirectoryOnWindows covers the one path shape the general text
// class cannot read.
//
// The body is JSON, so a Windows separator is written twice, and the class that
// terminates a capture at a backslash stopped at the drive letter. Every
// session on Windows therefore recorded its repository as "C:" - attribution to
// a work item still worked, because a branch name carries no backslashes, so
// nothing failed and an organization's Windows engineers quietly shared one
// bogus checkout. Found by running the real tool on a Windows runner.
func TestWorkingDirectoryOnWindows(t *testing.T) {
	cases := []struct {
		name string
		// cwd is the path as the agent writes it - single separators. The
		// doubling happens in the JSON encoding below, which is exactly where
		// it happens in production.
		cwd  string
		want string
	}{
		{"a drive-letter path", `C:\Users\dev\src\panorama`, "panorama"},
		{"a UNC share", `\\build-01\checkouts\panorama`, "panorama"},
		{"a trailing separator", `C:\Users\dev\src\panorama\`, "panorama"},
		{"posix, unchanged", "/home/dev/src/panorama", "panorama"},
		// A drive root is not a checkout, and naming it would be the original
		// bug with a better path behind it.
		{"a bare drive names nothing", `C:\`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newExtractor(t, "")
			prompt := "# Environment\n - Primary working directory: " + tc.cwd +
				"\n - Is a git repository: true\n\nCurrent branch: main\n"
			body := claudeCodeRequest{system: prompt, messages: []string{"hello"}}.body(t)

			// The guard that keeps this case honest. If the encoder ever stops
			// doubling a separator, the pattern under test is no longer being
			// exercised and the case would pass while proving nothing.
			if strings.Contains(tc.cwd, `\`) && !bytes.Contains(body, []byte(`\\`)) {
				t.Fatal("the encoded body carries no escaped separator; this case tests nothing")
			}

			if got := e.Structural(body).Directory.Value; got != tc.want {
				t.Errorf("directory = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLegacyEnvironmentBlockStillWorks. The spelling changed between Claude
// Code versions; an extractor pinned to one does not fail loudly, it silently
// stops attributing.
func TestLegacyEnvironmentBlockStillWorks(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system:   legacySystemPrompt,
		messages: []string{"fix the invoice rounding"},
	}.body(t)

	s := e.Structural(body)
	if got := s.Directory.Value; got != "billing-service" {
		t.Errorf("directory = %q, want billing-service from the <env> block", got)
	}
}

func TestRemoteGivesTheRepository(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system:   currentSystemPrompt,
		messages: []string{"what is the remote?", gitRemoteOutput},
	}.body(t)

	s := e.Structural(body)
	if got := s.Repo.Value; got != "acme/panorama" {
		t.Errorf("repo = %q, want acme/panorama - and without the .git suffix", got)
	}

	a, ok := Attribute(s)
	if !ok {
		t.Fatal("nothing attributed")
	}
	if a.Repo != "acme/panorama" {
		t.Errorf("attribution repo = %q, want the remote rather than the directory name", a.Repo)
	}
}

func TestPullRequestFromGhOutput(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system: `You are Claude Code.

# Environment
 - Primary working directory: /home/dev/panorama
`,
		messages: []string{"what is the PR status?", ghPullRequestOutput},
	}.body(t)

	s := e.Structural(body)
	if got := s.PullRequest.Value; got != "412" {
		t.Errorf("pull request = %q, want 412", got)
	}
	if got := s.Repo.Value; got != "acme/panorama" {
		t.Errorf("repo = %q, want it read from the pull request URL", got)
	}

	a, ok := Attribute(s)
	if !ok {
		t.Fatal("nothing attributed")
	}
	if a.Kind != KindPullRequest || a.Key != "acme/panorama#412" {
		t.Errorf("attributed to %s %q, want pull_request acme/panorama#412", a.Kind, a.Key)
	}
}

// TestBranchWithNoTicketIsStillAttributed is the zero-configuration promise:
// most branches have no ticket key in them, and cost-per-branch has to work
// anyway.
func TestBranchWithNoTicketIsStillAttributed(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system: strings.Replace(currentSystemPrompt,
			"feature/PAY-1183-cache-aware-costs", "refactor-the-price-table", 1),
		messages: []string{"tidy up the rate lookup", gitRemoteOutput},
	}.body(t)

	a, ok := Attribute(e.Structural(body))
	if !ok {
		t.Fatal("a branch with no ticket key was not attributed at all")
	}
	if a.Kind != KindBranch {
		t.Errorf("kind = %q, want branch", a.Kind)
	}
	if a.Key != "acme/panorama@refactor-the-price-table" {
		t.Errorf("key = %q, want the repository and branch", a.Key)
	}
	if a.Confidence != ConfidenceHigh {
		t.Errorf("confidence = %q, want high - a branch name is a fact", a.Confidence)
	}
}

// TestContentTicketNeedsCorroboration. A key in a sentence is a mention, and
// engineers discuss each other's tickets all day.
func TestContentTicketNeedsCorroboration(t *testing.T) {
	e := newExtractor(t, "")

	t.Run("uncorroborated is not attributed", func(t *testing.T) {
		body := claudeCodeRequest{
			system:   "You are Claude Code.",
			messages: []string{"Do you know if PAY-1183 was ever finished?"},
		}.body(t)

		s := e.Structural(body)
		if s.Ticket.Rung != RungContent {
			t.Fatalf("ticket rung = %q, want content", s.Ticket.Rung)
		}
		if _, ok := Attribute(s); ok {
			t.Error("a ticket mentioned in passing, with no structural context, was attributed")
		}
	})

	t.Run("corroborated by a branch, at the content rung", func(t *testing.T) {
		system := strings.Replace(currentSystemPrompt,
			"feature/PAY-1183-cache-aware-costs", "spike", 1)
		body := claudeCodeRequest{
			system:   system,
			messages: []string{"This is the work for BILL-77.", gitRemoteOutput},
		}.body(t)

		s := e.Structural(body)
		a, ok := Attribute(s)
		if !ok {
			t.Fatal("nothing attributed")
		}
		// Structure beats content: the branch wins over the mention, even
		// though a ticket is the more meaningful unit.
		if a.Kind != KindBranch {
			t.Errorf("attributed to %s %q; structural evidence must beat a mention", a.Kind, a.Key)
		}
	})
}

// TestWellKnownIdentifiersAreNotTickets. UTF-8 is shaped exactly like a ticket
// key and is not one.
func TestWellKnownIdentifiersAreNotTickets(t *testing.T) {
	e := newExtractor(t, "")

	for _, text := range []string{
		"the encoding must be UTF-8 throughout",
		"we still hash with SHA-1 in that path",
		"see CVE-2024-1234 for the details",
		"per RFC-2119 this is a MUST",
		"switch to AES-256 there",
	} {
		body := claudeCodeRequest{
			system:   currentSystemPrompt,
			messages: []string{text},
		}.body(t)

		// The branch in the fixture supplies PAY-1183 structurally, so a
		// content match would have to beat it - but the point here is that the
		// guard runs at all. Strip the branch to isolate the content path.
		plain := claudeCodeRequest{system: "You are Claude Code.", messages: []string{text}}.body(t)
		_ = body

		if got := e.Structural(plain).Ticket.Value; got != "" {
			t.Errorf("%q produced ticket %q", text, got)
		}
	}
}

// TestConfiguredTicketPatternIsAnAllowlist. Naming your projects turns the
// lowest rung from a heuristic into an allowlist.
func TestConfiguredTicketPatternIsAnAllowlist(t *testing.T) {
	e := newExtractor(t, `\b(PAY|BILL)-\d+\b`)

	body := claudeCodeRequest{
		system:   "You are Claude Code.",
		messages: []string{"related to OPS-42 and to PAY-1183"},
	}.body(t)

	if got := e.Structural(body).Ticket.Value; got != "PAY-1183" {
		t.Errorf("ticket = %q, want PAY-1183 - a configured pattern must exclude OPS-42", got)
	}
}

func TestStampWinsOverStructure(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system:   currentSystemPrompt,
		messages: []string{"work", gitRemoteOutput},
	}.body(t)

	header := http.Header{}
	header.Set(HeaderTicket, "OPS-9000")
	header.Set(HeaderRepo, "acme/other")

	s := Combine(Stamp(header.Get), e.Structural(body))

	a, ok := Attribute(s)
	if !ok {
		t.Fatal("nothing attributed")
	}
	if a.Key != "OPS-9000" {
		t.Errorf("key = %q, want the stamped ticket", a.Key)
	}
	if a.Rung != RungStamp || a.Confidence != ConfidenceHigh {
		t.Errorf("provenance = %q/%q, want stamp/high", a.Rung, a.Confidence)
	}
	// A partial stamp still lets the body fill in what it did not say.
	if s.Branch.Value != "feature/PAY-1183-cache-aware-costs" {
		t.Errorf("branch = %q; a stamp that says nothing about the branch must not erase it", s.Branch.Value)
	}
}

// TestStampValuesAreTreatedAsHostile. A header is whatever the caller put in
// it, and these values reach the database and the console.
func TestStampValuesAreTreatedAsHostile(t *testing.T) {
	for name, value := range map[string]string{
		"a newline":           "PAY-1\nSet-Cookie: x=1",
		"a control character": "PAY-1\x00",
		"non-ASCII":           "PAY-1\u202e",
		"absurdly long":       strings.Repeat("A", 5000),
		"only whitespace":     "   ",
		"an HTML fragment":    "<script>alert(1)</script>",
	} {
		header := http.Header{}
		// Set() would reject some of these outright; the map form is how a
		// crafted request would arrive.
		header[http.CanonicalHeaderKey(HeaderTicket)] = []string{value}

		got := Stamp(header.Get).Ticket.Value
		if name == "an HTML fragment" {
			// Printable ASCII and within bounds: kept, and rendered as text
			// rather than markup by everything downstream.
			if got != value {
				t.Errorf("%s: value = %q, want it kept intact", name, got)
			}
			continue
		}
		if got != "" {
			t.Errorf("%s: value = %q, want it rejected", name, got)
		}
	}
}

func TestNoSignalsMeansUnattributed(t *testing.T) {
	e := newExtractor(t, "")
	body := claudeCodeRequest{
		system:   "You are a helpful assistant.",
		messages: []string{"What is the capital of France?"},
	}.body(t)

	if _, ok := Attribute(e.Structural(body)); ok {
		t.Error("a conversation with nothing to do with any repository was attributed")
	}
}

func TestAttributionSupersedes(t *testing.T) {
	branch := Attribution{Kind: KindBranch, Key: "r@b", Rung: RungStructural}
	ticket := Attribution{Kind: KindTicket, Key: "PAY-1", Rung: RungStructural}
	stamped := Attribution{Kind: KindTicket, Key: "OPS-9", Rung: RungStamp}
	mention := Attribution{Kind: KindTicket, Key: "PAY-2", Rung: RungContent}

	tests := []struct {
		name             string
		candidate, given Attribution
		want             bool
	}{
		{"anything beats nothing", branch, Attribution{}, true},
		{"nothing beats nothing", Attribution{}, Attribution{}, false},
		{"a ticket upgrades a branch at the same rung", ticket, branch, true},
		{"a branch does not downgrade a ticket", branch, ticket, false},
		{"a stamp beats structure", stamped, ticket, true},
		{"structure does not beat a stamp", ticket, stamped, false},
		{"a mention does not beat structure", mention, branch, false},
		{"the same answer does not churn", ticket, ticket, false},
	}

	for _, tc := range tests {
		if got := tc.candidate.Supersedes(tc.given); got != tc.want {
			t.Errorf("%s: Supersedes = %v, want %v", tc.name, got, tc.want)
		}
	}
}
