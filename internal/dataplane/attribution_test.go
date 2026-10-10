// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/store"
)

// codingAgentBody builds a request shaped like the one a coding agent sends,
// through encoding/json so the escaping is real. See the note in the
// attribution package's tests for why that matters.
func codingAgentBody(t *testing.T, branch string, turns ...string) string {
	t.Helper()

	system := `You are Claude Code, Anthropic's official CLI for Claude.

# Environment
You have been invoked in the following environment:
 - Primary working directory: /Users/dev/src/panorama
 - Is a git repository: true

gitStatus: This is the git status at the start of the conversation.

Current branch: ` + branch + `

Main branch (you will usually use this for PRs): main

Status:
 M internal/pricing/money.go
`

	type block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}

	msgs := make([]message, 0, len(turns))
	for i, text := range turns {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, message{Role: role, Content: []block{{Type: "text", Text: text}}})
	}

	body, err := json.Marshal(map[string]any{
		"model":      testModel,
		"max_tokens": 1024,
		"system":     []block{{Type: "text", Text: system}},
		"messages":   msgs,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

func newAttributingFixture(t *testing.T) *fixture {
	t.Helper()

	extractor, err := attribution.NewExtractor("")
	if err != nil {
		t.Fatalf("extractor: %v", err)
	}
	return newFixtureWith(t, fixtureConfig{
		provider:    &fakeProvider{},
		attribution: extractor,
	})
}

// TestZeroConfigurationAttribution is the M4 gate.
//
// An unmodified coding agent, no client configuration, no headers, no
// integration - and the branch and its ticket come out the other side attached
// to the money.
func TestZeroConfigurationAttribution(t *testing.T) {
	f := newAttributingFixture(t)

	rec := f.do(t, codingAgentBody(t, "feature/PAY-1183-cache-aware-costs", "Add cache-aware pricing"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	f.drain()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}

	s := sessions[0]
	if got := s.Describe(); got != "PAY-1183 - branch name (structural, high)" {
		t.Errorf("describe = %q, want the documented phrasing", got)
	}
	if s.Branch != "feature/PAY-1183-cache-aware-costs" {
		t.Errorf("branch = %q", s.Branch)
	}
	if s.Repo != "panorama" {
		t.Errorf("repo = %q, want the working directory's name when no remote was seen", s.Repo)
	}

	// The request row points at the session, which is how the money follows the
	// attribution when it is later upgraded.
	row := f.latest(t)
	if row.SessionID != s.ID {
		t.Errorf("request session = %q, want %q", row.SessionID, s.ID)
	}
}

// TestCostPerBranchAppears is the same gate, stated as the number a leader
// actually asks for.
func TestCostPerBranchAppears(t *testing.T) {
	f := newAttributingFixture(t)

	// Two conversations on two branches, several turns each.
	for _, turn := range []string{"start the work", "keep going", "nearly there"} {
		f.do(t, codingAgentBody(t, "feature/PAY-1183-costs", "start the work", "on it", turn))
	}
	f.do(t, codingAgentBody(t, "chore/bump-deps", "update the SDKs"))
	f.drain()

	now := time.Now().UTC()
	buckets, err := f.db.SummarizeWork(t.Context(),
		store.RequestFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}

	byKey := map[string]int64{}
	for _, b := range buckets {
		byKey[b.Key] = b.Requests
	}

	if byKey["PAY-1183"] != 3 {
		t.Errorf("PAY-1183 has %d requests, want 3 - the turns of one conversation must roll up together", byKey["PAY-1183"])
	}
	if byKey["panorama@chore/bump-deps"] != 1 {
		t.Errorf("the ticketless branch has %d requests, want 1: %+v", byKey["panorama@chore/bump-deps"], byKey)
	}
}

// TestTurnsOfOneConversationShareASession: the sessions table grows with
// conversations, not with traffic.
func TestTurnsOfOneConversationShareASession(t *testing.T) {
	f := newAttributingFixture(t)

	turns := []string{"Add cache-aware pricing"}
	for range 5 {
		f.do(t, codingAgentBody(t, "feature/PAY-1183-costs", turns...))
		turns = append(turns, "here is what I did", "carry on")
	}
	f.drain()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions for one conversation of five turns, want 1", len(sessions))
	}
	if sessions[0].Requests != 5 {
		t.Errorf("session counted %d turns, want 5", sessions[0].Requests)
	}
}

// TestExtractionIsSkippedOnceSettled is the cost control. Ninety-nine turns of
// a hundred must not re-scan a body to re-derive a fact already known.
func TestExtractionIsSkippedOnceSettled(t *testing.T) {
	f := newAttributingFixture(t)

	body := codingAgentBody(t, "feature/PAY-1183-costs", "start")
	f.do(t, body)
	f.drain()

	fingerprint := f.sessionFingerprint(t)
	if f.dp.sessions.NeedsExtraction(fingerprint) {
		t.Error("a session with structural attribution is still scheduled for a full body scan every turn")
	}

	known, ok := f.dp.sessions.Lookup(fingerprint)
	if !ok {
		t.Fatal("the session was not remembered")
	}
	if known.Key != "PAY-1183" {
		t.Errorf("cached key = %q, want PAY-1183", known.Key)
	}
}

func (f *fixture) sessionFingerprint(t *testing.T) string {
	t.Helper()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil || len(sessions) == 0 {
		t.Fatalf("no session: %v", err)
	}
	return sessions[0].Fingerprint
}

// TestHeaderStampWins is the precision tier of the ladder, reached through the
// proxy rather than in isolation.
func TestHeaderStampWins(t *testing.T) {
	f := newAttributingFixture(t)

	req := f.request(codingAgentBody(t, "feature/PAY-1183-costs", "work"))
	req.Header.Set(attribution.HeaderTicket, "OPS-42")
	req.Header.Set(attribution.HeaderRepo, "acme/panorama")

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	f.drain()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %v %+v", err, sessions)
	}
	s := sessions[0]
	if s.WorkItemKey != "OPS-42" || s.Rung != "stamp" {
		t.Errorf("attributed to %q (%s), want the stamped ticket", s.WorkItemKey, s.Rung)
	}
	// The stamp said nothing about the branch, so the body still supplies it.
	if s.Branch != "feature/PAY-1183-costs" {
		t.Errorf("branch = %q; the stamp erased what the body showed", s.Branch)
	}
}

// TestSessionHeaderGroupsExplicitly: a client that knows its own session
// boundaries beats any inference, including across a rewritten opening.
func TestSessionHeaderGroupsExplicitly(t *testing.T) {
	f := newAttributingFixture(t)

	for _, opening := range []string{"first opening", "a completely different opening"} {
		req := f.request(codingAgentBody(t, "feature/PAY-1183-costs", opening))
		req.Header.Set(attribution.HeaderSession, "my-session-0001")
		f.mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	f.drain()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1: an explicit session header was ignored", len(sessions))
	}
	if sessions[0].Method != "client-header" {
		t.Errorf("method = %q, want client-header", sessions[0].Method)
	}
}

// TestUnattributableTrafficIsRecordedHonestly. A request with nothing to place
// it against is not forced into somebody else's session.
func TestUnattributableTrafficIsRecordedHonestly(t *testing.T) {
	f := newAttributingFixture(t)

	rec := f.do(t, `{"model":"`+testModel+`","max_tokens":16,"messages":[{"role":"user","content":"what is 2+2?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	f.drain()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Attributed() {
			t.Errorf("a request with no work signals was attributed to %q", s.WorkItemKey)
		}
	}

	now := time.Now().UTC()
	coverage, err := f.db.Coverage(t.Context(),
		store.RequestFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Requests != 1 || coverage.AttributedRequests != 0 {
		t.Errorf("coverage = %d of %d, want 0 of 1", coverage.AttributedRequests, coverage.Requests)
	}
}

// TestNoBodyIsRetained is the non-negotiable, asserted rather than assumed.
//
// A structural test rather than a behavioural one: the value that travels from
// the handler to the recorder is checked for any fragment of the conversation
// it came from. If a body ever leaks into the audit path, it will be because
// someone added a field, and this is what fails when they do.
func TestNoBodyIsRetained(t *testing.T) {
	f := newAttributingFixture(t)

	const secret = "the passphrase is hunter2 and the customer is Acme"
	body := codingAgentBody(t, "feature/PAY-1183-costs", secret)

	f.do(t, body)
	f.drain()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		blob := strings.Join([]string{
			s.Repo, s.Branch, s.WorkItemKey, s.WorkItemKind, s.Rung, s.Confidence, s.Evidence,
			s.Method, s.Tool, s.Fingerprint,
		}, " ")
		if strings.Contains(blob, "hunter2") || strings.Contains(blob, "Acme") {
			t.Fatalf("conversation content reached the session record: %q", blob)
		}
	}

	page, err := f.db.ListRequests(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Requests {
		blob := strings.Join([]string{
			r.RequestedModel, r.ResolvedModel, r.ErrorKind, r.Tool,
			r.UserAgent, r.SessionID, r.BudgetState,
		}, " ")
		if strings.Contains(blob, "hunter2") || strings.Contains(blob, "Acme") {
			t.Fatalf("conversation content reached the request record: %q", blob)
		}
	}
}

// TestAttributionOffIsCoherent: a deployment can turn it off and still work.
func TestAttributionOffIsCoherent(t *testing.T) {
	f := newFixture(t, &fakeProvider{})

	rec := f.do(t, codingAgentBody(t, "feature/PAY-1183-costs", "work"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	f.drain()

	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("%d sessions with no extractor configured, want none", len(sessions))
	}
	if row := f.latest(t); row.SessionID != "" {
		t.Errorf("session = %q, want empty", row.SessionID)
	}
}

// opencodeRequest is a turn as opencode sends it: its own user agent, and its
// own session headers, which nobody configured it to send.
func (f *fixture) opencodeRequest(t *testing.T, opening string, headers map[string]string) *http.Request {
	t.Helper()
	req := f.request(codingAgentBody(t, "feature/PAY-1183-costs", opening))
	req.Header.Set("User-Agent", "opencode/1.18.32")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func (f *fixture) sessions(t *testing.T) []store.Session {
	t.Helper()
	sessions, err := f.db.ListSessions(t.Context(), store.RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return sessions
}

// TestOpenCodeGroupsItsOwnTurns: opencode names its conversation in a header,
// so its turns are grouped by that name and not by a hash of the opening.
func TestOpenCodeGroupsItsOwnTurns(t *testing.T) {
	f := newAttributingFixture(t)

	for _, opening := range []string{"first opening", "a completely different opening"} {
		f.mux.ServeHTTP(httptest.NewRecorder(),
			f.opencodeRequest(t, opening, map[string]string{"X-Session-Id": "ses_0001"}))
	}
	f.drain()

	sessions := f.sessions(t)
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1: opencode's own session header was ignored", len(sessions))
	}
	if sessions[0].Method != "opencode-session" {
		t.Errorf("method = %q, want opencode-session", sessions[0].Method)
	}
}

// TestAnOpenCodeSubagentJoinsItsParent: a subagent is doing its parent's work,
// so its turns are grouped with the conversation that started it.
func TestAnOpenCodeSubagentJoinsItsParent(t *testing.T) {
	f := newAttributingFixture(t)

	f.mux.ServeHTTP(httptest.NewRecorder(),
		f.opencodeRequest(t, "the conversation", map[string]string{"X-Session-Id": "ses_parent"}))
	f.mux.ServeHTTP(httptest.NewRecorder(),
		f.opencodeRequest(t, "a subagent's task", map[string]string{
			"X-Session-Id":        "ses_child",
			"X-Parent-Session-Id": "ses_parent",
		}))
	f.drain()

	if n := len(f.sessions(t)); n != 1 {
		t.Fatalf("%d sessions, want 1: the subagent was not grouped with its parent", n)
	}
}

// TestOnlyOpenCodeIsReadForItsSessionHeader: a header means what its sender
// meant by it, so another client's X-Session-Id is not taken as opencode's.
func TestOnlyOpenCodeIsReadForItsSessionHeader(t *testing.T) {
	f := newAttributingFixture(t)

	for _, opening := range []string{"first opening", "a completely different opening"} {
		req := f.request(codingAgentBody(t, "feature/PAY-1183-costs", opening))
		req.Header.Set("User-Agent", "claude-cli/2.1")
		req.Header.Set("X-Session-Id", "ses_0001")
		f.mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	f.drain()

	if n := len(f.sessions(t)); n != 2 {
		t.Errorf("%d sessions, want 2: X-Session-Id was read from a client that is not opencode", n)
	}
}

// TestTheStampedSessionOutranksOpenCodes: the header somebody configured wins
// over the one the tool sends by default.
func TestTheStampedSessionOutranksOpenCodes(t *testing.T) {
	f := newAttributingFixture(t)

	for i, opening := range []string{"first opening", "a completely different opening"} {
		f.mux.ServeHTTP(httptest.NewRecorder(), f.opencodeRequest(t, opening, map[string]string{
			attribution.HeaderSession: "stamped-0001",
			"X-Session-Id":            []string{"ses_a", "ses_b"}[i],
		}))
	}
	f.drain()

	sessions := f.sessions(t)
	if len(sessions) != 1 || sessions[0].Method != "client-header" {
		t.Errorf("sessions = %+v, want one grouped by the stamped header", sessions)
	}
}

// TestClaudeCodeFastModelCallsJoinTheConversation: Claude Code's background
// calls to its fast model open with their own short prompt and no environment
// block, so by inference each would be a session of its own with no
// repository. The conversation id Claude Code sends on every call puts them in
// the conversation that has one.
func TestClaudeCodeFastModelCallsJoinTheConversation(t *testing.T) {
	f := newAttributingFixture(t)
	const conversation = "eb20acae-c856-45d5-997e-accc328a2283"

	main := f.request(codingAgentBody(t, "feat/PAY-1183-costs", "fix the refund webhook"))
	main.Header.Set("X-Claude-Code-Session-Id", conversation)
	f.mux.ServeHTTP(httptest.NewRecorder(), main)

	fast := f.request(`{"model":"` + testModel + `","max_tokens":64,"messages":[{"role":"user","content":"Write a five word title for this conversation."}]}`)
	fast.Header.Set("X-Claude-Code-Session-Id", conversation)
	f.mux.ServeHTTP(httptest.NewRecorder(), fast)
	f.drain()

	sessions := f.sessions(t)
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1: the fast model's call was not put in its conversation", len(sessions))
	}
	if s := sessions[0]; s.Method != "claude-code-session" || s.Branch != "feat/PAY-1183-costs" || s.Requests != 2 {
		t.Errorf("session = method %q, branch %q, %d requests; want claude-code-session, the branch, 2", s.Method, s.Branch, s.Requests)
	}
}
