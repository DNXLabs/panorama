// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/integrations"
)

// fakeGitHub serves the handful of endpoints this source calls.
type fakeGitHub struct {
	*httptest.Server
	calls []string
	// pulls is keyed "owner/repo#number".
	pulls map[string]map[string]any
	// heads is keyed "owner/repo:branch" and holds the number of the pull
	// request opened from it.
	heads map[string]int
	// branches is keyed "owner/repo/branch".
	branches map[string]bool
	// status, when set, is returned for everything, with these headers.
	status  int
	headers map[string]string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	f := &fakeGitHub{
		pulls:    map[string]map[string]any{},
		heads:    map[string]int{},
		branches: map[string]bool{},
		headers:  map[string]string{},
	}

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls = append(f.calls, r.URL.Path+"?"+r.URL.RawQuery)

		if f.status != 0 {
			for k, v := range f.headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

		switch {
		case r.URL.Path == "/rate_limit":
			_ = json.NewEncoder(w).Encode(map[string]any{"rate": map[string]any{"remaining": 4999}})

		// /repos/{owner}/{repo}/pulls/{number}
		case len(parts) == 5 && parts[0] == "repos" && parts[3] == "pulls":
			pr, ok := f.pulls[parts[1]+"/"+parts[2]+"#"+parts[4]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(pr)

		// /repos/{owner}/{repo}/pulls?head=owner:branch
		case len(parts) == 4 && parts[0] == "repos" && parts[3] == "pulls":
			head := r.URL.Query().Get("head")
			owner, branch, _ := strings.Cut(head, ":")
			number, ok := f.heads[owner+"/"+parts[2]+":"+branch]
			if !ok {
				_ = json.NewEncoder(w).Encode([]any{})
				return
			}
			pr := f.pulls[parts[1]+"/"+parts[2]+"#"+strconv.Itoa(number)]
			_ = json.NewEncoder(w).Encode([]any{pr})

		// /repos/{owner}/{repo}/branches/{branch}
		case len(parts) >= 5 && parts[0] == "repos" && parts[3] == "branches":
			name := strings.Join(parts[4:], "/")
			if !f.branches[parts[1]+"/"+parts[2]+"/"+name] {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name})

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) source(t *testing.T) *Source {
	t.Helper()
	src, err := Open(Config{APIBaseURL: f.URL, Token: "ghp_test"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return src
}

func (f *fakeGitHub) addPull(repo string, number int, fields map[string]any) {
	pr := map[string]any{
		"number":     number,
		"title":      "A change",
		"html_url":   "https://github.com/" + repo + "/pull/" + strconv.Itoa(number),
		"state":      "open",
		"user":       map[string]any{"login": "dana"},
		"head":       map[string]any{"ref": "feat/x"},
		"base":       map[string]any{"repo": map[string]any{"full_name": repo}},
		"created_at": "2026-08-01T10:00:00Z",
		"updated_at": "2026-08-02T10:00:00Z",
	}
	for k, v := range fields {
		pr[k] = v
	}
	f.pulls[repo+"#"+strconv.Itoa(number)] = pr
}

// TestOutcomeMapping is the most expensive mistake available in this package.
//
// A merged pull request reports state "closed". Grouping on state rather than
// on merged_at would file every merge as abandoned - and cost per merged pull
// request, the headline number, would read zero while spend on abandoned work
// read one hundred per cent.
func TestOutcomeMapping(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)

	f.addPull("acme/app", 1, map[string]any{
		"state": "closed", "merged_at": "2026-08-03T10:00:00Z", "closed_at": "2026-08-03T10:00:00Z",
	})
	f.addPull("acme/app", 2, map[string]any{
		"state": "closed", "closed_at": "2026-08-03T10:00:00Z",
	})
	f.addPull("acme/app", 3, nil)
	f.addPull("acme/app", 4, map[string]any{"draft": true})

	for _, tc := range []struct {
		number  int
		status  string
		outcome integrations.Outcome
	}{
		{1, "Merged", integrations.OutcomeLanded},
		{2, "Closed", integrations.OutcomeAbandoned},
		{3, "Open", integrations.OutcomeOpen},
		{4, "Draft", integrations.OutcomeOpen},
	} {
		key := "acme/app#" + strconv.Itoa(tc.number)
		items, err := src.Lookup(t.Context(), []integrations.Ref{
			{Kind: attribution.KindPullRequest, Key: key},
		})
		if err != nil {
			t.Fatalf("Lookup(%s): %v", key, err)
		}
		if len(items) != 1 {
			t.Fatalf("Lookup(%s) returned %d items, want 1", key, len(items))
		}
		if items[0].Outcome != tc.outcome {
			t.Errorf("%s: outcome = %q, want %q", key, items[0].Outcome, tc.outcome)
		}
		if items[0].Status != tc.status {
			t.Errorf("%s: status = %q, want %q", key, items[0].Status, tc.status)
		}
		if verr := items[0].Validate(); verr != nil {
			t.Errorf("%s: %v", key, verr)
		}
	}
}

func TestPullRequestCarriesWhatTheConsoleShows(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)
	f.addPull("acme/app", 412, map[string]any{
		"title": "Stream the tokenizer", "state": "closed",
		"merged_at": "2026-08-03T10:00:00Z",
	})

	items, err := src.Lookup(t.Context(), []integrations.Ref{
		{Kind: attribution.KindPullRequest, Key: "acme/app#412"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := items[0]

	if got.Key != "acme/app#412" {
		t.Errorf("Key = %q, want the key that was asked about - the answer upserts onto that row", got.Key)
	}
	if got.Title != "Stream the tokenizer" || got.Author != "dana" {
		t.Errorf("title/author = %q/%q", got.Title, got.Author)
	}
	if got.Repo != "acme/app" || got.Project != "acme/app" {
		t.Errorf("repo/project = %q/%q", got.Repo, got.Project)
	}
	if got.ExternalID != "412" {
		t.Errorf("ExternalID = %q, want the number", got.ExternalID)
	}
	if got.MergedAt == nil {
		t.Error("MergedAt is nil for a merged pull request")
	}
	if got.URL == "" {
		t.Error("URL is empty")
	}
}

// TestBranchResolvesToItsPullRequest. The branch is the thing the gateway sees;
// the pull request is the thing that merges.
func TestBranchResolvesToItsPullRequest(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)

	f.addPull("acme/app", 412, map[string]any{
		"state": "closed", "merged_at": "2026-08-03T10:00:00Z",
		"head": map[string]any{"ref": "feat/streaming"},
	})
	f.heads["acme/app:feat/streaming"] = 412

	items, err := src.Lookup(t.Context(), []integrations.Ref{
		{Kind: attribution.KindBranch, Key: "acme/app@feat/streaming"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want the branch and the pull request", len(items))
	}

	branch, pr := items[0], items[1]
	if branch.Kind != attribution.KindBranch || pr.Kind != attribution.KindPullRequest {
		t.Fatalf("kinds = %q, %q", branch.Kind, pr.Kind)
	}

	// The relation is recorded rather than collapsed: the console shows the
	// chain, and the outcome rollup follows exactly one hop of it.
	if len(branch.Links) != 1 || branch.Links[0].Key != "acme/app#412" {
		t.Errorf("branch links = %+v, want one link to the pull request", branch.Links)
	}

	// And the branch does not inherit the merge. A branch never merges; the
	// pull request it was opened as does, and copying the outcome across would
	// count one landing twice.
	if branch.Outcome != integrations.OutcomeOpen {
		t.Errorf("branch outcome = %q, want open - a branch does not merge", branch.Outcome)
	}
	if pr.Outcome != integrations.OutcomeLanded {
		t.Errorf("pull request outcome = %q, want landed", pr.Outcome)
	}
}

// TestBranchWithNoPullRequestIsStillConfirmed. A pushed branch is real work
// even though it has nothing to say about outcome.
func TestBranchWithNoPullRequestIsStillConfirmed(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)
	f.branches["acme/app/feat/solo"] = true

	items, err := src.Lookup(t.Context(), []integrations.Ref{
		{Kind: attribution.KindBranch, Key: "acme/app@feat/solo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want just the branch", len(items))
	}
	if items[0].Outcome != integrations.OutcomeOpen || items[0].Status != "pushed, no pull request" {
		t.Errorf("branch = %+v", items[0])
	}
}

// TestNotFoundIsAbsentNotAnError is the distinction the whole confirmation
// story rests on. A branch nobody pushed is not a broken integration.
func TestNotFoundIsAbsentNotAnError(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)

	items, err := src.Lookup(t.Context(), []integrations.Ref{
		{Kind: attribution.KindPullRequest, Key: "acme/app#999"},
		{Kind: attribution.KindBranch, Key: "acme/app@never-pushed"},
		// A branch the gateway saw with no remote, so there is no repository
		// to ask about.
		{Kind: attribution.KindBranch, Key: "just-a-branch-name"},
		// A ticket routed here by mistake.
		{Kind: attribution.KindTicket, Key: "PAY-1183"},
	})
	if err != nil {
		t.Fatalf("Lookup returned an error for things GitHub simply does not have: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("got %d items, want none: %+v", len(items), items)
	}
}

// TestRateLimitIsAnError is the other half of the same rule. Not having asked
// must never be reported as "GitHub has never heard of this".
func TestRateLimitIsAnError(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)

	f.status = http.StatusForbidden
	f.headers["X-RateLimit-Remaining"] = "0"
	f.headers["X-RateLimit-Reset"] = strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)

	_, err := src.Lookup(t.Context(), []integrations.Ref{
		{Kind: attribution.KindPullRequest, Key: "acme/app#1"},
	})
	if err == nil {
		t.Fatal("a rate-limited lookup came back as an empty result; " +
			"the confirmation run would write not-found across every work item")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v, want it to name the rate limit rather than read as a permissions problem", err)
	}

	if cerr := src.Check(t.Context()); cerr == nil {
		t.Error("Check passed while rate limited; the source must report unhealthy so the run stops")
	}
}

// TestOneFailureStopsTheRun. Returning the successes and dropping the rest
// would silently mark the rest as unconfirmed.
func TestOneFailureStopsTheRun(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)
	f.addPull("acme/app", 1, nil)

	// The first ref answers, then the server starts failing.
	f.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := src.Lookup(t.Context(), []integrations.Ref{
		{Kind: attribution.KindPullRequest, Key: "acme/app#1"},
		{Kind: attribution.KindPullRequest, Key: "acme/app#2"},
	})
	if err == nil {
		t.Fatal("Lookup swallowed a server error")
	}
}

func TestOpenRequiresExactlyOneCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"neither", Config{}},
		{"both", Config{AppID: "1", PrivateKey: []byte("x"), Token: "ghp_x"}},
		{"app without a key", Config{AppID: "1"}},
		{"key without an app", Config{PrivateKey: []byte("x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(tc.cfg); err == nil {
				t.Error("Open accepted an ambiguous credential")
			}
		})
	}
}

func TestCheckReachesTheAPI(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)

	if err := src.Check(t.Context()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(f.calls) == 0 || !strings.HasPrefix(f.calls[0], "/rate_limit") {
		t.Errorf("Check called %v, want /rate_limit", f.calls)
	}
}

func TestDescribeNamesTheCredentialWithoutDisclosingIt(t *testing.T) {
	f := newFakeGitHub(t)
	src := f.source(t)

	got := src.Describe()
	if !strings.Contains(got, "personal access token") {
		t.Errorf("Describe() = %q, want it to say a token is behind this", got)
	}
	if strings.Contains(got, "ghp_test") {
		t.Error("Describe() leaks the credential")
	}
}

func TestKeyParsing(t *testing.T) {
	for _, tc := range []struct {
		key                 string
		owner, repo, suffix string
		ok                  bool
		pull                bool
	}{
		{key: "acme/app#412", owner: "acme", repo: "app", suffix: "412", ok: true, pull: true},
		{key: "acme/app#abc", pull: true},
		{key: "acme/app", pull: true},
		{key: "acme#412", pull: true},
		{key: "acme/app@feat/x", owner: "acme", repo: "app", suffix: "feat/x", ok: true},
		{key: "feat/x"},
		{key: "acme/app@"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			var owner, repo, suffix string
			var ok bool
			if tc.pull {
				owner, repo, suffix, ok = splitPullRequestKey(tc.key)
			} else {
				owner, repo, suffix, ok = splitBranchKey(tc.key)
			}
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && (owner != tc.owner || repo != tc.repo || suffix != tc.suffix) {
				t.Errorf("got %q/%q %q, want %q/%q %q", owner, repo, suffix, tc.owner, tc.repo, tc.suffix)
			}
		})
	}
}

// The interface this package exists to satisfy, checked at compile time.
var _ integrations.Source = (*Source)(nil)
