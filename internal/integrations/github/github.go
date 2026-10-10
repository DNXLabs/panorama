// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package github confirms branches and pull requests against the GitHub API.
//
// It answers the two questions the gateway cannot: does this branch exist and
// what is it called, and did the pull request it became actually merge. The
// first is corroboration; the second is the whole of cost per merged pull
// request.
//
// # It asks only about what it was given
//
// Nothing here enumerates a repository. The confirmation job hands over the
// work items Panorama already saw in traffic, and this asks about those. A
// deployment therefore needs read access to the repositories its engineers
// touched and nothing more, and the call count is bounded by conversations
// rather than by the size of the organisation.
//
// # Not found is an answer, not a failure
//
// A branch nobody pushed, a pull request number that was never opened, a
// repository this installation cannot see: all of them come back absent. That
// is the honest report - the gateway saw the branch in somebody's working
// directory, and GitHub having no record of it does not make the observation
// wrong. Treating it as an error would make every deployment's integration look
// broken because one engineer works locally.
//
// # Running out of API budget is a failure
//
// The one thing that must not be reported as "not found" is not having asked.
// Rate limit exhaustion, a revoked installation, a network partition: each
// returns an error, which makes the source unhealthy, which stops the
// confirmation run before it writes anything. Carrying on would stamp "GitHub
// has never heard of this" across every work item in the deployment.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/integrations"
)

// defaultAPI is github.com. Enterprise Server has its own base, which is why
// this is configurable rather than a constant in the request path.
const defaultAPI = "https://api.github.com"

// requestTimeout bounds one call. The confirmation job runs on an interval and
// off the request path, so this is generous - but not unbounded, because a job
// that never finishes never runs again either.
const requestTimeout = 30 * time.Second

// Config is what an operator supplies.
type Config struct {
	// APIBaseURL overrides api.github.com, for GitHub Enterprise Server.
	APIBaseURL string

	// AppID and PrivateKey are the GitHub App path - the documented one.
	AppID      string
	PrivateKey []byte
	// InstallationID is optional: it is discovered when the app is installed in
	// exactly one place, and required when it is installed in several.
	InstallationID string

	// Token is a personal access token, for evaluation.
	Token string
}

// Source is the GitHub implementation of integrations.Source.
type Source struct {
	api    string
	client *http.Client
	cred   credential
}

// Open builds a source from configuration.
//
// Exactly one credential shape must be supplied. Accepting both and preferring
// one would mean a deployment that thought it was using its App was quietly
// using somebody's personal token, with that person's access and that person's
// departure date.
func Open(cfg Config) (*Source, error) {
	api := strings.TrimRight(cfg.APIBaseURL, "/")
	if api == "" {
		api = defaultAPI
	}

	hasApp := cfg.AppID != "" || len(cfg.PrivateKey) > 0
	hasPAT := cfg.Token != ""

	switch {
	case hasApp && hasPAT:
		return nil, errors.New("github: configure either a GitHub App or a token, not both")
	case !hasApp && !hasPAT:
		return nil, errors.New("github: needs either a GitHub App (app_id and private_key_file) or a token")
	case hasPAT:
		return &Source{api: api, client: newClient(), cred: &patCredential{token: cfg.Token}}, nil
	}

	if cfg.AppID == "" || len(cfg.PrivateKey) == 0 {
		return nil, errors.New("github: a GitHub App needs both app_id and private_key_file")
	}
	key, err := parsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}

	return &Source{
		api:    api,
		client: newClient(),
		cred: &appCredential{
			appID:          cfg.AppID,
			installationID: cfg.InstallationID,
			key:            key,
		},
	}, nil
}

func newClient() *http.Client { return &http.Client{Timeout: requestTimeout} }

// Kind names the system this source speaks for.
func (s *Source) Kind() integrations.Kind { return integrations.KindGitHub }

// Provider names the implementation. Distinct from Kind so the console can tell
// a real answer from a fixture.
func (s *Source) Provider() string { return "github" }

// Describe is the sentence the integrations screen shows.
func (s *Source) Describe() string {
	where := s.api
	if where == defaultAPI {
		where = "github.com"
	}
	return where + ", via " + s.cred.describe()
}

// Check reports whether the connection works.
//
// /rate_limit is the probe because it needs no scope, costs nothing against the
// budget it reports, and exercises the whole credential path - for an App that
// means minting a JWT, discovering the installation, and exchanging it for a
// token, which is where a misconfiguration actually shows up.
func (s *Source) Check(ctx context.Context) error {
	var out struct {
		Rate struct {
			Remaining int `json:"remaining"`
		} `json:"rate"`
	}
	if err := s.get(ctx, "/rate_limit", &out); err != nil {
		return err
	}
	return nil
}

// Lookup answers what GitHub knows about the given references.
func (s *Source) Lookup(ctx context.Context, refs []integrations.Ref) ([]integrations.WorkItem, error) {
	var out []integrations.WorkItem

	for _, ref := range refs {
		items, err := s.lookupOne(ctx, ref)
		if err != nil {
			// One failure ends the run. The alternative - carrying on and
			// returning what did answer - would report the rest as not found,
			// and "GitHub has never heard of this branch" is a claim that must
			// only ever be made after actually asking.
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

func (s *Source) lookupOne(ctx context.Context, ref integrations.Ref) ([]integrations.WorkItem, error) {
	switch ref.Kind {
	case attribution.KindPullRequest:
		return s.pullRequest(ctx, ref)
	case attribution.KindBranch:
		return s.branch(ctx, ref)
	default:
		// A ticket reached the GitHub source. The registry routes by kind so
		// this should be unreachable; answering "not found" rather than erroring
		// keeps a routing mistake from taking the whole run down.
		return nil, nil
	}
}

// pullRequest confirms one pull request, from a key shaped owner/repo#412.
func (s *Source) pullRequest(ctx context.Context, ref integrations.Ref) ([]integrations.WorkItem, error) {
	owner, repo, number, ok := splitPullRequestKey(ref.Key)
	if !ok {
		return nil, nil
	}

	var pr pullRequestResponse
	found, err := s.getFound(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%s", owner, repo, number), &pr)
	if err != nil || !found {
		return nil, err
	}
	return []integrations.WorkItem{pr.workItem(ref.Key, owner+"/"+repo)}, nil
}

// branch confirms one branch, from a key shaped owner/repo@feat/x.
//
// Two calls at most, and usually one. A branch's interesting fact is the pull
// request it was opened as - that is what carries a merge - so the pull request
// is asked for first, and the branch endpoint is only reached for a branch that
// has none. A branch nobody opened a pull request for is still real and still
// worth confirming; it simply has nothing to say about outcome.
func (s *Source) branch(ctx context.Context, ref integrations.Ref) ([]integrations.WorkItem, error) {
	owner, repo, name, ok := splitBranchKey(ref.Key)
	if !ok {
		// A branch key with no repository in it. The gateway records one when
		// it read a branch name without a remote, and there is no repository to
		// ask about - so this is absent rather than an error.
		return nil, nil
	}
	full := owner + "/" + repo

	var prs []pullRequestResponse
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&per_page=1&head=%s",
		owner, repo, url.QueryEscape(owner+":"+name))
	found, err := s.getFound(ctx, path, &prs)
	if err != nil {
		return nil, err
	}

	if found && len(prs) > 0 {
		pr := prs[0]
		prItem := pr.workItem(full+"#"+strconv.Itoa(pr.Number), full)

		// The branch is confirmed and linked to its pull request; it is not
		// given the pull request's outcome. A branch does not merge - the pull
		// request it was opened as does - and copying the outcome across would
		// double-count the same landing under two keys.
		branchItem := integrations.WorkItem{
			Kind: attribution.KindBranch, Key: ref.Key,
			Repo: full, Branch: name, Project: full,
			URL:     pr.HTMLURL,
			Status:  "open as " + prItem.Key,
			Outcome: integrations.OutcomeOpen,
			Links:   []integrations.Link{{Kind: attribution.KindPullRequest, Key: prItem.Key}},
		}
		return []integrations.WorkItem{branchItem, prItem}, nil
	}

	var br struct {
		Name string `json:"name"`
	}
	found, err = s.getFound(ctx, fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, url.PathEscape(name)), &br)
	if err != nil || !found {
		return nil, err
	}

	return []integrations.WorkItem{{
		Kind: attribution.KindBranch, Key: ref.Key,
		Repo: full, Branch: name, Project: full,
		URL:     fmt.Sprintf("https://github.com/%s/tree/%s", full, name),
		Status:  "pushed, no pull request",
		Outcome: integrations.OutcomeOpen,
	}}, nil
}

// pullRequestResponse is the half of GitHub's pull request payload this reads.
type pullRequestResponse struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
}

func (p pullRequestResponse) workItem(key, repo string) integrations.WorkItem {
	if p.Base.Repo.FullName != "" {
		repo = p.Base.Repo.FullName
	}

	status, outcome := p.outcome()

	return integrations.WorkItem{
		Kind: attribution.KindPullRequest, Key: key,
		Repo: repo, Branch: p.Head.Ref, Project: repo,
		Title:      p.Title,
		URL:        p.HTMLURL,
		Author:     p.User.Login,
		Status:     status,
		Outcome:    outcome,
		ExternalID: strconv.Itoa(p.Number),
		CreatedAt:  p.CreatedAt,
		UpdatedAt:  p.UpdatedAt,
		MergedAt:   p.MergedAt,
		ClosedAt:   p.ClosedAt,
	}
}

// outcome maps GitHub's vocabulary onto the canonical one.
//
// The status keeps GitHub's word for display; the outcome is what the metrics
// read. `merged_at` decides rather than `state`, because a merged pull request
// reports state "closed" - grouping on state would file every merge as
// abandoned, which is the single most expensive mistake available here.
func (p pullRequestResponse) outcome() (string, integrations.Outcome) {
	switch {
	case p.MergedAt != nil:
		return "Merged", integrations.OutcomeLanded
	case p.State == "closed":
		return "Closed", integrations.OutcomeAbandoned
	case p.Draft:
		return "Draft", integrations.OutcomeOpen
	default:
		return "Open", integrations.OutcomeOpen
	}
}

// splitPullRequestKey reads owner/repo#412.
func splitPullRequestKey(key string) (owner, repo, number string, ok bool) {
	full, num, found := strings.Cut(key, "#")
	if !found || num == "" {
		return "", "", "", false
	}
	if _, err := strconv.Atoi(num); err != nil {
		return "", "", "", false
	}
	owner, repo, found = strings.Cut(full, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", "", false
	}
	return owner, repo, num, true
}

// splitBranchKey reads owner/repo@feat/x.
//
// Cut at the first @, because a branch name may contain one and an owner may
// not. A key with no @ is a branch the gateway saw without a remote; it has no
// repository and cannot be asked about.
func splitBranchKey(key string) (owner, repo, branch string, ok bool) {
	full, name, found := strings.Cut(key, "@")
	if !found || name == "" {
		return "", "", "", false
	}
	owner, repo, found = strings.Cut(full, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", "", false
	}
	return owner, repo, name, true
}

// get performs a call that must succeed.
func (s *Source) get(ctx context.Context, path string, out any) error {
	found, err := s.getFound(ctx, path, out)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("github: %s: not found", path)
	}
	return nil
}

// getFound performs a call where 404 is an answer.
//
// The distinction this function exists to hold: a 404 means GitHub does not
// have it, and anything else that went wrong means we did not ask. Only the
// first is allowed to look like an empty result.
func (s *Source) getFound(ctx context.Context, path string, out any) (bool, error) {
	header, err := s.cred.header(ctx, s.client, s.api)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.api+path, nil)
	if err != nil {
		return false, err
	}
	setHeaders(req, header)

	resp, err := s.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("github: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, statusError(resp, path)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, fmt.Errorf("github: reading %s: %w", path, err)
	}
	return true, nil
}

func setHeaders(req *http.Request, authorization string) {
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/vnd.github+json")
	// Pinned, because GitHub changes response shapes behind this and a
	// deployment should not start reading a different one after an upgrade it
	// did not make.
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "panorama")
}

// statusError turns a failed response into an error worth reading.
//
// Rate limiting is named explicitly, with when it clears, because it is the
// failure an operator will actually hit and "403 Forbidden" sends them looking
// at permissions instead of at the clock.
func statusError(resp *http.Response, what string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" {
		when := "shortly"
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if unix, err := strconv.ParseInt(reset, 10, 64); err == nil {
				when = "at " + time.Unix(unix, 0).UTC().Format(time.RFC3339)
			}
		}
		return fmt.Errorf("github: %s: API rate limit exhausted, resets %s", what, when)
	}

	if message := errorMessage(body); message != "" {
		return fmt.Errorf("github: %s: %s (%s)", what, message, resp.Status)
	}
	return fmt.Errorf("github: %s: %s", what, resp.Status)
}

func errorMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Message
}
