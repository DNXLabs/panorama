// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package integrations

import (
	"context"
	"errors"
	"testing"
)

type stub struct {
	kind Kind
	err  error
}

func (s stub) Kind() Kind       { return s.kind }
func (s stub) Provider() string { return string(s.kind) }
func (s stub) Describe() string { return string(s.kind) + " stub" }
func (s stub) Lookup(context.Context, []Ref) ([]WorkItem, error) {
	return nil, nil
}
func (s stub) Check(context.Context) error { return s.err }

// TestNilRegistryIsUsable. A deployment with nothing connected is the common
// case, not a broken one: attribution still works, and only the outcome metrics
// are unavailable. Every call has to answer rather than panic.
func TestNilRegistryIsUsable(t *testing.T) {
	var r *Registry

	if r.Configured() {
		t.Error("a nil registry reports itself as configured")
	}
	if _, err := r.SourceFor("ticket"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("SourceFor on a nil registry = %v, want ErrNotConfigured", err)
	}
	if got := r.Sources(); len(got) != 0 {
		t.Errorf("a nil registry listed %d sources", len(got))
	}

	routed, unroutable := r.Group([]Ref{{Kind: "ticket", Key: "PAY-1"}})
	if len(routed) != 0 || len(unroutable) != 1 {
		t.Errorf("Group on a nil registry routed %d and could not route %d, want 0 and 1",
			len(routed), len(unroutable))
	}
}

// TestGroupReportsWhatNothingCanAnswer is the honest half of the integrations
// screen. A registry that quietly dropped the references it could not route
// would make a deployment with no Jira look like one whose tickets were all
// confirmed.
func TestGroupReportsWhatNothingCanAnswer(t *testing.T) {
	r := NewRegistry(stub{kind: KindGitHub})

	routed, unroutable := r.Group([]Ref{
		{Kind: "pull_request", Key: "acme/panorama#412"},
		{Kind: "branch", Key: "acme/panorama@feat/x"},
		{Kind: "ticket", Key: "PAY-1183"},
		{Kind: "epic", Key: "PAY"},
	})

	if len(routed[KindGitHub]) != 2 {
		t.Errorf("%d references routed to GitHub, want 2", len(routed[KindGitHub]))
	}
	if len(unroutable) != 2 {
		t.Fatalf("%d references unroutable, want 2 - the ticket and the unknown kind", len(unroutable))
	}
}

// TestNilSourcesAreSkipped lets a caller pass a constructor's nil result
// straight through, which is what the wiring does for a half-configured
// deployment.
func TestNilSourcesAreSkipped(t *testing.T) {
	r := NewRegistry(nil, stub{kind: KindJira}, nil)

	if got := len(r.Sources()); got != 1 {
		t.Fatalf("%d sources, want 1", got)
	}
	if _, err := r.SourceFor("ticket"); err != nil {
		t.Errorf("SourceFor(ticket) = %v, want the Jira source", err)
	}
	if _, err := r.SourceFor("pull_request"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("SourceFor(pull_request) = %v, want ErrNotConfigured", err)
	}
}

// TestSourcesAreListedInAStableOrder keeps the integrations screen from
// reshuffling between refreshes.
func TestSourcesAreListedInAStableOrder(t *testing.T) {
	r := NewRegistry(stub{kind: KindJira}, stub{kind: KindGitHub})

	got := r.Sources()
	if len(got) != 2 || got[0].Kind() != KindGitHub || got[1].Kind() != KindJira {
		t.Errorf("source order = %v, want GitHub then Jira regardless of construction order", got)
	}
}

func TestWorkItemValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		item WorkItem
		ok   bool
	}{
		"complete": {
			item: WorkItem{Kind: "ticket", Key: "PAY-1183", Outcome: OutcomeLanded},
			ok:   true,
		},
		"unknown outcome is honest and allowed": {
			item: WorkItem{Kind: "branch", Key: "a@b"},
			ok:   true,
		},
		"no key": {
			item: WorkItem{Kind: "ticket", Outcome: OutcomeOpen},
		},
		"invented outcome": {
			item: WorkItem{Kind: "ticket", Key: "PAY-1", Outcome: "shipped"},
		},
		"half a link": {
			item: WorkItem{Kind: "ticket", Key: "PAY-1", Links: []Link{{Kind: "pull_request"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.item.Validate()
			if tc.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Error("Validate() accepted an item it should have refused")
			}
		})
	}
}
