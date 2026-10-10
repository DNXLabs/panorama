// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package costcenters allocates spend to the accounting line that paid for it.
//
// A cost center answers a different question from a budget, and they are
// deliberately not the same mechanism.
//
// A budget enforces on a principal, because the principal is the only thing the
// gateway knows at the moment a request needs a decision: the token identifies
// a person or a pipeline before a byte of the body has been read. A cost center
// accounts for work, and the work is not known then. Attribution is per session
// and inherited across turns, so the opening turn of a conversation has no
// repository yet, and a session's work item can still be superseded afterwards
// by better evidence.
//
// That is why nothing in this package enforces anything. A cost center carries
// a plan and raises a warning; hard limits stay on people and teams, where
// there is something to enforce against at request time and somebody who can
// act on being stopped. A cost center limit that blocked would admit every new
// session until it was attributed and then stop somebody mid-conversation over
// a repository they may not own, with no switch available to them.
//
// # Allocation is evidence or nothing
//
// There is no fallback rung. A work item no rule claims is unallocated, and
// unallocated is stated rather than absorbed - the same commitment the product
// already makes to unattributed spend and to unpriced requests.
//
// Falling back to the spender's team was considered and refused. It would
// produce a fully allocated report in which an unknown share of every line was
// an assumption rather than a measurement, and - the deciding argument - it
// would remove the only thing that ever gets a missing rule written. A
// repository with real spend and no rule has to read as a worklist item. Absorb
// it into a team's line and nobody ever writes the rule, because nothing ever
// looks wrong.
//
// # Why an ordered rule list is allowed here
//
// Routing has no ordered rule list, on purpose: one beside the route list would
// decide an authorization question the route list also decides, and two answers
// to "may this request proceed" means the permissive one wins silently the day
// they disagree.
//
// These rules decide a question nothing else decides. They are a total function
// onto at most one cost center, they are read by no other table, and being
// wrong misfiles money rather than opening a door. What an ordered list does
// cost is predictability, which is why Resolve is also what the console's
// tester calls: an administrator types a repository and is shown the rule that
// claims it, rather than finding out a quarter later from a restated report.
package costcenters

import (
	"fmt"
	"strings"
)

// Match is what a rule matches a work item on.
//
// Two, and the pair is not arbitrary. Repository is the obvious one and is not
// sufficient on its own: a company with a monorepo has one repository, so
// repository rules alone would give it one cost center and the whole feature
// would be useless to it. Ticket rules are what rescue that, and they work
// without any integration configured - a branch named `feat/PAY-1183-x` is
// attributed to the ticket `PAY-1183` by the structural rung, so the work item
// exists and carries the key long before anybody connects a tracker.
type Match string

const (
	// MatchTicket matches a work item's own key - "PAY-*", "INFRA-1*".
	MatchTicket Match = "ticket"
	// MatchRepo matches the repository a work item belongs to -
	// "acme/payments-*", "*-infra".
	MatchRepo Match = "repo"
)

// Valid reports whether m is a match kind this package knows.
func (m Match) Valid() bool { return m == MatchTicket || m == MatchRepo }

// Header is the stamp a client may send to name the cost center itself.
//
// It sits with the `x-panorama-` allowlist in internal/attribution rather than
// extending it, because it is not an attribution signal: it says which line
// pays, not what the work was. Nothing forwards it - the Bedrock provider
// builds its own request through the SDK, so no client header reaches a
// provider by construction.
//
// It is the top rung of allocation for the reason the attribution ladder's top
// rung is also a header: somebody who declares which line this belongs to knows
// better than a pattern inferring it, and the cases that need it - a pipeline
// building one product, a contractor billing an engagement - are not
// expressible as a repository pattern.
const Header = "x-panorama-cost-center"

// NormalizeCode folds a cost center code for comparison.
//
// One function, called both when the snapshot is loaded and when a stamp is
// read off a request, because a code folded two ways is a stamp that resolves
// in the console and not on the gateway.
func NormalizeCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

// KindTicket is the work item kind a ticket rule may claim.
//
// Named here rather than imported so the resolver depends on nothing: it is
// pure, which is what lets the console's tester and the store's re-resolve pass
// call exactly the same function.
const KindTicket = "ticket"

// Rule is one line of the ordered allocation list.
type Rule struct {
	ID           string
	CostCenterID string
	// Position orders evaluation, lowest first. Ties break by ID so the order
	// is at least deterministic, but an administrator who cares says so with
	// distinct positions.
	Position int
	Match    Match
	Pattern  string
}

// Subject is what a work item offers to be matched on.
//
// A struct rather than the store's work item type, for the same reason Rule is
// not: this package is imported by the store, so it cannot import it back.
type Subject struct {
	Kind string
	Key  string
	Repo string
}

// Matches reports whether this rule claims this work item.
func (r Rule) Matches(s Subject) bool {
	switch r.Match {
	case MatchTicket:
		// The kind check is what stops a catch-all ticket rule from claiming
		// pull requests and branches, whose keys are a different shape
		// entirely. Without it, "*" on a ticket rule would take everything.
		if s.Kind != KindTicket || s.Key == "" {
			return false
		}
		return glob(r.Pattern, s.Key)

	case MatchRepo:
		// A work item with no repository cannot be claimed by a repository
		// rule, catch-all included. That is the honest reading: there is
		// nothing to match, so nothing matched.
		if s.Repo == "" {
			return false
		}
		return glob(r.Pattern, s.Repo)
	}
	return false
}

// Resolve returns the first rule that claims this work item.
//
// Rules must already be ordered - Position then ID - because ordering them here
// would be a second place that decided precedence, and the console's tester and
// the re-resolve pass would eventually disagree about which rule wins.
//
// The false return is the whole of the "unallocated" answer, and it is a state
// this package expects rather than a failure to be papered over.
func Resolve(rules []Rule, s Subject) (Rule, bool) {
	for _, r := range rules {
		if r.Matches(s) {
			return r, true
		}
	}
	return Rule{}, false
}

// ValidatePattern refuses a pattern that could not do what its author meant.
//
// A bare "*" is permitted, and is the one place a catch-all is legitimate: an
// administrator who writes one has seen the unallocated worklist and answered
// it deliberately, in a row anybody reading the table can see. That is the
// difference between it and the fallback this package refuses, which would have
// been implicit and invisible.
func ValidatePattern(p string) error {
	if strings.TrimSpace(p) != p {
		return fmt.Errorf("costcenters: pattern %q has leading or trailing spaces", p)
	}
	if p == "" {
		return fmt.Errorf("costcenters: a rule needs a pattern")
	}
	return nil
}

// Stampable is the lookup a stamped request needs: folded code to cost center
// id, holding only the lines a client may actually name.
//
// Archived lines and lines with accepts_stamp off are absent rather than
// present-and-rejected, so the hot path is one map lookup with no second
// condition to get wrong. That collapses "no such code" and "that line refuses
// stamps" into one answer, which is correct here because the outcome is
// identical - fall through to the rules, and keep the raw string. The two are
// still told apart where it matters: the console resolves a recorded stamp
// against the full list when it draws the worklist, and a query there is free.
type Stampable map[string]string

// Lookup resolves a raw header value to a cost center id.
//
// The empty return is every failure at once - no header, an unknown code, a
// line that refuses stamps - and every one of them means the same thing to the
// caller: allocate by the rules instead, and record what was sent.
func (s Stampable) Lookup(header string) string {
	code := NormalizeCode(header)
	if code == "" {
		return ""
	}
	return s[code]
}

// Unreachable reports the indexes of rules no work item can ever reach.
//
// Deliberately conservative: it reports a rule shadowed by an earlier rule of
// the same kind that is either a catch-all or an identical pattern, and does
// not attempt general pattern subsumption. Those two are the cases that
// actually occur - somebody adds a catch-all to close the worklist and then
// keeps adding rules under it - and a checker that guessed at subsumption would
// warn about rules that work, which is how a warning gets ignored.
func Unreachable(rules []Rule) []int {
	var out []int
	seen := make(map[string]bool, len(rules))
	catchAll := make(map[Match]bool, 2)

	for i, r := range rules {
		key := string(r.Match) + "\x00" + strings.ToLower(r.Pattern)
		if catchAll[r.Match] || seen[key] {
			out = append(out, i)
			continue
		}
		seen[key] = true
		if r.Pattern == "*" {
			catchAll[r.Match] = true
		}
	}
	return out
}

// Shadowed reports rules a repository catch-all makes unreachable in practice.
//
// Separate from Unreachable, and worded differently, because the claim is
// weaker and has to stay weaker. A `repo: *` rule claims any work item carrying
// a repository, which is every branch, every pull request, and every ticket
// inferred from a branch - so a ticket rule below one effectively never fires.
// "Effectively" is the whole difference: a ticket reached by a content signal
// can carry no repository, and would still reach the rule below. Reporting that
// as "never matches" would be a claim this package cannot support, and a
// warning that is wrong in a case somebody hits is a warning they learn to
// ignore.
//
// It exists because the arrangement is easy to build by accident and impossible
// to see: an administrator adds a catch-all to close the unallocated worklist,
// then adds a ticket rule under it, and the ticket rule saves cleanly and does
// nothing.
func Shadowed(rules []Rule) []int {
	var out []int
	catchAll := false
	for i, r := range rules {
		if catchAll {
			out = append(out, i)
			continue
		}
		if r.Match == MatchRepo && r.Pattern == "*" {
			catchAll = true
		}
	}
	return out
}

// glob matches a pattern against a value, without regard to case.
//
// The whole syntax is "*", standing for any run of characters including none.
// No "?", no character classes, and - unlike path.Match, which was the obvious
// thing to reach for - "*" crosses "/". A pattern of "*payments*" meant to
// catch "acme/payments-api" silently matching nothing is exactly the kind of
// surprise that misfiles a quarter of spend.
//
// Case is folded on both sides, because GitHub treats a repository name that
// way and a ticket prefix is written both ways by the same person in a week.
func glob(pattern, value string) bool {
	return match(strings.ToLower(pattern), strings.ToLower(value))
}

// match is the standard greedy two-pointer wildcard match: linear, no
// recursion, and no backtracking blow-up on a pattern full of stars.
func match(pattern, value string) bool {
	var p, v, mark int
	star := -1

	for v < len(value) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, v
			p++
		case p < len(pattern) && pattern[p] == value[v]:
			p++
			v++
		case star >= 0:
			// Backtrack: let the last star swallow one more character.
			p = star + 1
			mark++
			v = mark
		default:
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
