// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package integrations confirms what the gateway inferred against the systems
// where work is actually tracked, and records what became of it.
//
// GitHub supplies pull requests, branches, and merge status. Jira supplies
// tickets, projects, and hierarchy. Both feed two things:
//
//   - Corroboration. A branch read out of a request body is a guess about a
//     work item until something authoritative agrees that the branch exists,
//     names it, and says what happened to it.
//   - Outcome. Cost per merged pull request, and spend on merged versus
//     abandoned work - the numbers that answer what the money bought.
//
// # Confirmation, not synchronisation
//
// A source is asked about the work items Panorama has already seen, and about
// nothing else. It is not asked to enumerate a repository or a Jira project.
//
// That is a deliberate inversion of the usual integration shape, and it is
// cheaper in every direction: nothing is pulled for work nobody spent money on,
// a deployment needs read scope on only what its engineers touched, and the
// number of API calls is bounded by conversations rather than by the size of
// the organisation. The cost is that Panorama never knows about a pull request
// no session was attributed to, which is exactly the pull request its numbers
// have nothing to say about anyway.
//
// # Confirmation does not move an attribution
//
// A confirmed work item gets a title, a URL, a status, an author, and an
// outcome. It does not get a different rung, and the session attributed to it
// does not move.
//
// The reason is that the ladder answers a different question. "This session was
// working on the branch feat/streaming-tokenizer" is a fact about what the
// gateway observed; GitHub agreeing that the branch exists does not make the
// observation better, and GitHub having never heard of it does not make the
// observation wrong - the branch may simply never have been pushed. What
// confirmation adds is a relation, recorded as such: this branch belongs to
// that pull request, which belongs to that ticket. The console shows the chain
// rather than silently rewriting the attribution to its far end.
//
// # ClickUp and GitLab
//
// After v1, behind this interface. Nothing here names GitHub or Jira outside
// the Kind constants and the two implementations.
package integrations

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kind identifies the system a source speaks for.
type Kind string

const (
	KindGitHub Kind = "github"
	KindJira   Kind = "jira"
)

// Valid reports whether k is a system Panorama knows.
func (k Kind) Valid() bool { return k == KindGitHub || k == KindJira }

// ErrNotConfigured is returned by a registry lookup for a kind of work item no
// configured source speaks for. It is not a failure: a deployment with no Jira
// connected still attributes to tickets, it just cannot confirm them.
var ErrNotConfigured = errors.New("integrations: no source configured")

// Outcome is the canonical lifecycle position of a work item, and the only
// field the outcome metrics read.
//
// Sources map their own vocabulary onto it, and keep their own word in Status
// for display. Both are needed: "Merged", "Closed", "Done", "Won't Do" and
// "Released" are five statuses and three outcomes, and a metric that groups on
// the source's word would report each spelling as its own category.
type Outcome string

const (
	// OutcomeUnknown is a work item nothing has been asked about yet, or one a
	// source reported without a status it could map. It is counted and shown
	// as its own bucket rather than folded into either of the others, for the
	// same reason unattributed spend is.
	OutcomeUnknown Outcome = ""
	// OutcomeOpen is still in flight.
	OutcomeOpen Outcome = "open"
	// OutcomeLanded is a merged pull request or a resolved ticket.
	OutcomeLanded Outcome = "landed"
	// OutcomeAbandoned is a pull request closed without merging, or a ticket
	// cancelled. The spend was real; the work did not ship.
	OutcomeAbandoned Outcome = "abandoned"
)

// Valid reports whether o is an outcome this package defines.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeUnknown, OutcomeOpen, OutcomeLanded, OutcomeAbandoned:
		return true
	default:
		return false
	}
}

// Ref names a work item Panorama inferred and would like confirmed.
//
// Kind and Key match the work_items row exactly, so a lookup's answer upserts
// onto the row that prompted it rather than creating a near-duplicate.
type Ref struct {
	// Kind is one of attribution.KindBranch, KindPullRequest, KindTicket.
	Kind string
	Key  string

	Repo   string
	Branch string
}

// Link is a relation between two work items: a pull request naming the ticket
// it implements, a branch naming the pull request it was opened as.
type Link struct {
	Kind string
	Key  string
}

// WorkItem is what a source knows about a unit of work.
//
// Every field is optional except Kind and Key. A source that knows only that a
// branch exists returns exactly that, and the console shows exactly that: an
// empty title is honest about a branch that was pushed and never opened as a
// pull request.
type WorkItem struct {
	Kind string
	Key  string

	Repo   string
	Branch string

	Title  string
	URL    string
	Author string
	// Project is the Jira project or epic, or the GitHub repository, whichever
	// the source rolls this item up under. It drives the by-project view.
	Project string

	// Status is the source's own word, for display.
	Status string
	// Outcome is the canonical position, for metrics.
	Outcome Outcome

	// ExternalID is the source's own identifier, kept so a later API call can
	// address the item without reconstructing its key.
	ExternalID string

	CreatedAt time.Time
	UpdatedAt time.Time
	MergedAt  *time.Time
	ClosedAt  *time.Time

	Links []Link
}

// Validate reports what is wrong with a work item a source returned.
//
// Sources are code Panorama does not necessarily own - a mock reads a file an
// operator wrote, and the GitHub and Jira sources parse someone else's JSON -
// so what comes back is checked before it reaches the database rather than
// trusted because it came from a Source.
func (w WorkItem) Validate() error {
	if w.Kind == "" || w.Key == "" {
		return fmt.Errorf("integrations: work item needs a kind and a key, got %q/%q", w.Kind, w.Key)
	}
	if !w.Outcome.Valid() {
		return fmt.Errorf("integrations: %s: %q is not an outcome", w.Key, w.Outcome)
	}
	for _, l := range w.Links {
		if l.Kind == "" || l.Key == "" {
			return fmt.Errorf("integrations: %s: a link needs a kind and a key", w.Key)
		}
	}
	return nil
}

// Source is one connected system.
type Source interface {
	// Kind names the system.
	Kind() Kind

	// Provider names the implementation behind it: "github", "jira", or
	// "mock". It is a separate answer from Kind because a mock stands in for a
	// system without being it, and the console has to be able to say which of
	// the two it is looking at.
	Provider() string

	// Describe is the sentence the integrations screen shows: what this source
	// actually is, so a fixture-backed source can say so in words.
	Describe() string

	// Lookup answers what the source knows about the given references.
	//
	// A reference the source does not recognise is absent from the result
	// rather than an error. That is the normal answer for a branch that was
	// never pushed or a ticket key that matched the pattern without being one,
	// and treating it as a failure would make every deployment's integration
	// look broken.
	Lookup(ctx context.Context, refs []Ref) ([]WorkItem, error)

	// Check reports whether the connection works, for the integrations screen
	// and for the setup wizard's verified state.
	Check(ctx context.Context) error
}

// Registry routes a reference to whichever source speaks for that kind of work
// item.
//
// A nil Registry is usable and answers ErrNotConfigured for everything, which
// is what a deployment with no integrations connected is: fully functional,
// with outcome metrics unavailable and saying so.
type Registry struct {
	sources map[Kind]Source
}

// NewRegistry builds a registry from the configured sources. A nil source is
// skipped, so a caller can pass the result of a constructor that returns nil
// when its half of the configuration is empty.
func NewRegistry(sources ...Source) *Registry {
	r := &Registry{sources: make(map[Kind]Source, len(sources))}
	for _, s := range sources {
		if s == nil {
			continue
		}
		r.sources[s.Kind()] = s
	}
	return r
}

// SourceFor returns the source that speaks for this kind of work item.
//
// The mapping is fixed rather than configurable: a pull request is a GitHub
// concept and a ticket is a tracker concept, and letting a deployment point
// "ticket" at GitHub Issues is a v1.x question, not a v1 one.
func (r *Registry) SourceFor(workItemKind string) (Source, error) {
	if r == nil {
		return nil, ErrNotConfigured
	}
	kind, ok := sourceKindFor(workItemKind)
	if !ok {
		return nil, fmt.Errorf("integrations: %w for work items of kind %q", ErrNotConfigured, workItemKind)
	}
	s, ok := r.sources[kind]
	if !ok {
		return nil, fmt.Errorf("integrations: %w: %s", ErrNotConfigured, kind)
	}
	return s, nil
}

// Sources lists what is connected, in a stable order so the integrations screen
// does not reshuffle between refreshes.
func (r *Registry) Sources() []Source {
	if r == nil {
		return nil
	}
	var out []Source
	for _, k := range []Kind{KindGitHub, KindJira} {
		if s, ok := r.sources[k]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Configured reports whether anything is connected at all.
func (r *Registry) Configured() bool { return r != nil && len(r.sources) > 0 }

// Group splits references by the source that will answer them, and reports the
// ones nothing can answer.
//
// The unroutable set is returned rather than dropped because it is the honest
// half of the integrations screen: "412 pull requests confirmed, 96 tickets not
// checked - no Jira connected" is the sentence an operator needs, and it cannot
// be written by a function that silently discards what it could not route.
func (r *Registry) Group(refs []Ref) (routed map[Kind][]Ref, unroutable []Ref) {
	routed = make(map[Kind][]Ref)
	for _, ref := range refs {
		kind, ok := sourceKindFor(ref.Kind)
		if !ok || r == nil {
			unroutable = append(unroutable, ref)
			continue
		}
		if _, ok := r.sources[kind]; !ok {
			unroutable = append(unroutable, ref)
			continue
		}
		routed[kind] = append(routed[kind], ref)
	}
	return routed, unroutable
}

func sourceKindFor(workItemKind string) (Kind, bool) {
	switch workItemKind {
	case "branch", "pull_request":
		return KindGitHub, true
	case "ticket":
		return KindJira, true
	default:
		return "", false
	}
}
