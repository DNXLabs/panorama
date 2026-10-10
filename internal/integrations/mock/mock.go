// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package mock is a Source backed by a file instead of a network.
//
// It exists for two reasons, and it is honest about being what it is in both.
//
// The first is tests. Confirmation, outcome metrics, and the PR-to-ticket
// relation are most of the value of the integrations, and none of it should be
// reachable only through a GitHub App installation and a Jira tenant. Every
// test in this repository that exercises a confirmed work item runs against
// this package.
//
// The second is a whole product to look at before the real sources are written.
// A deployment can point at a fixtures file and see the Work screen, the
// outcome numbers, and the confirmed badges work end to end.
//
// # It never invents anything
//
// A mock that answered plausibly for any key would put fabricated merge status
// into a console that is otherwise careful to distinguish what it knows from
// what it guessed, and a screenshot of it would be a lie about a real
// deployment. So: a key that is not in the fixtures file is not found, exactly
// as an unknown key would be against the real API, and a mock source with no
// fixtures answers nothing at all rather than generating a demo.
//
// Describe reports the file it read, and the console prints that beside the
// integration, so nobody looks at a fixtures-backed number and thinks it came
// from GitHub.
package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dnxlabs/panorama/internal/integrations"
)

// Source answers lookups from a fixtures file.
type Source struct {
	kind  integrations.Kind
	path  string
	items map[refKey]integrations.WorkItem
}

type refKey struct{ kind, key string }

// Fixtures is the file format.
//
// One file describes both systems, because the relation between them is the
// interesting part and splitting it across two files makes the link the one
// thing a fixture author has to get right twice.
type Fixtures struct {
	GitHub []Item `yaml:"github"`
	Jira   []Item `yaml:"jira"`
}

// Item is one work item as written in the fixtures file.
type Item struct {
	Kind   string `yaml:"kind"`
	Key    string `yaml:"key"`
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`

	Title   string `yaml:"title"`
	URL     string `yaml:"url"`
	Author  string `yaml:"author"`
	Project string `yaml:"project"`

	Status  string `yaml:"status"`
	Outcome string `yaml:"outcome"`

	ExternalID string `yaml:"external_id"`

	CreatedAt string `yaml:"created_at"`
	UpdatedAt string `yaml:"updated_at"`
	MergedAt  string `yaml:"merged_at"`
	ClosedAt  string `yaml:"closed_at"`

	Links []Link `yaml:"links"`
}

// Link is a relation to another work item.
type Link struct {
	Kind string `yaml:"kind"`
	Key  string `yaml:"key"`
}

// Open reads a fixtures file and returns the source for one system.
//
// The whole file is parsed and validated even though only one system's items
// are kept, so a typo in the Jira half is reported when the GitHub source is
// built rather than surviving until something looks at a ticket.
func Open(kind integrations.Kind, path string) (*Source, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("mock: %q is not a system this package can stand in for", kind)
	}
	if path == "" {
		return nil, errors.New("mock: a fixtures file is required - a mock source with nothing to read would answer nothing and look like a broken integration")
	}

	data, err := os.ReadFile(path) // #nosec G304 -- an operator-configured path, read at startup
	if err != nil {
		return nil, fmt.Errorf("mock: read fixtures %s: %w", path, err)
	}

	var f Fixtures
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	// Unknown keys are an error here for the same reason they are in the main
	// configuration: a misspelled field in a fixture silently does nothing, and
	// the resulting test passes for the wrong reason.
	dec.KnownFields(true)
	if derr := dec.Decode(&f); derr != nil {
		return nil, fmt.Errorf("mock: parse fixtures %s: %w", path, derr)
	}

	all, err := f.compile()
	if err != nil {
		return nil, fmt.Errorf("mock: %s: %w", path, err)
	}

	s := &Source{kind: kind, path: path, items: make(map[refKey]integrations.WorkItem)}
	for k, item := range all {
		if sourceOwns(kind, item.Kind) {
			s.items[k] = item
		}
	}
	return s, nil
}

// sourceOwns mirrors the registry's fixed routing, so a fixture filed under the
// wrong heading is not silently answered by the wrong system.
func sourceOwns(kind integrations.Kind, workItemKind string) bool {
	switch workItemKind {
	case "branch", "pull_request":
		return kind == integrations.KindGitHub
	case "ticket":
		return kind == integrations.KindJira
	default:
		return false
	}
}

// compile turns the file's items into work items, reporting every problem it
// finds rather than the first, because a fixtures file is usually wrong in
// several places at once the first time it is written.
func (f Fixtures) compile() (map[refKey]integrations.WorkItem, error) {
	out := make(map[refKey]integrations.WorkItem)
	var problems []string

	add := func(heading string, defaultKind string, items []Item) {
		for i, it := range items {
			where := fmt.Sprintf("%s[%d]", heading, i)

			kind := it.Kind
			if kind == "" {
				kind = defaultKind
			}
			if !sourceOwns(integrations.KindGitHub, kind) && !sourceOwns(integrations.KindJira, kind) {
				problems = append(problems, fmt.Sprintf("%s: %q is not a work item kind", where, kind))
				continue
			}
			if strings.TrimSpace(it.Key) == "" {
				problems = append(problems, where+": key is required")
				continue
			}

			w, err := it.workItem(kind)
			if err != nil {
				problems = append(problems, where+": "+err.Error())
				continue
			}
			k := refKey{kind: kind, key: it.Key}
			if _, dup := out[k]; dup {
				problems = append(problems, fmt.Sprintf("%s: %s %s appears twice", where, kind, it.Key))
				continue
			}
			out[k] = w
		}
	}

	// A GitHub fixture is a pull request unless it says otherwise, because that
	// is what nearly all of them are; a Jira one can only be a ticket.
	add("github", "pull_request", f.GitHub)
	add("jira", "ticket", f.Jira)

	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return out, nil
}

func (it Item) workItem(kind string) (integrations.WorkItem, error) {
	w := integrations.WorkItem{
		Kind:       kind,
		Key:        it.Key,
		Repo:       it.Repo,
		Branch:     it.Branch,
		Title:      it.Title,
		URL:        it.URL,
		Author:     it.Author,
		Project:    it.Project,
		Status:     it.Status,
		Outcome:    integrations.Outcome(it.Outcome),
		ExternalID: it.ExternalID,
	}

	for _, l := range it.Links {
		w.Links = append(w.Links, integrations.Link{Kind: l.Kind, Key: l.Key})
	}

	for _, ts := range []struct {
		name string
		text string
		dst  *time.Time
		opt  **time.Time
	}{
		{name: "created_at", text: it.CreatedAt, dst: &w.CreatedAt},
		{name: "updated_at", text: it.UpdatedAt, dst: &w.UpdatedAt},
		{name: "merged_at", text: it.MergedAt, opt: &w.MergedAt},
		{name: "closed_at", text: it.ClosedAt, opt: &w.ClosedAt},
	} {
		if ts.text == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, ts.text)
		if err != nil {
			return integrations.WorkItem{}, fmt.Errorf("%s: %q is not an RFC 3339 timestamp", ts.name, ts.text)
		}
		at = at.UTC()
		if ts.dst != nil {
			*ts.dst = at
		} else {
			*ts.opt = &at
		}
	}

	if err := w.Validate(); err != nil {
		return integrations.WorkItem{}, err
	}
	// A merge timestamp and an outcome that is not "landed" is the fixture
	// equivalent of a corrupt API response, and it would quietly skew the one
	// number these fixtures exist to produce.
	if w.MergedAt != nil && w.Outcome != integrations.OutcomeLanded {
		return integrations.WorkItem{}, fmt.Errorf("merged_at is set but the outcome is %q, not %q",
			w.Outcome, integrations.OutcomeLanded)
	}
	return w, nil
}

// Kind names the system this source stands in for.
func (s *Source) Kind() integrations.Kind { return s.kind }

// Provider is "mock", everywhere it is asked, for as long as this package
// exists. It is the single string the console keys its "these numbers came out
// of a file" treatment off, so it is a constant rather than something derived.
func (s *Source) Provider() string { return "mock" }

// Describe says plainly what this is, because the console prints it next to
// numbers that came out of it.
func (s *Source) Describe() string {
	return fmt.Sprintf("mock source - %d fixtures from %s", len(s.items), filepath.Base(s.path))
}

// Lookup answers from the fixtures. An unknown reference is absent from the
// result, which is what the real APIs report for a branch that was never pushed.
func (s *Source) Lookup(_ context.Context, refs []Ref) ([]integrations.WorkItem, error) {
	out := make([]integrations.WorkItem, 0, len(refs))
	for _, ref := range refs {
		if w, ok := s.items[refKey{kind: ref.Kind, key: ref.Key}]; ok {
			out = append(out, w)
		}
	}
	return out, nil
}

// Ref is integrations.Ref, aliased so the method set above reads without the
// package qualifier repeated on every line.
type Ref = integrations.Ref

// Check succeeds if the fixtures file is still readable, which is the closest
// thing this source has to a reachable API.
func (s *Source) Check(_ context.Context) error {
	if _, err := os.Stat(s.path); err != nil {
		return fmt.Errorf("mock: fixtures %s: %w", s.path, err)
	}
	return nil
}
