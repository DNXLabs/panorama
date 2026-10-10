// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package attribution ties spend to the work it paid for.
//
// This is Panorama's headline capability, and the thing that makes it worth
// deploying is that it works with no client configuration. A team points Claude
// Code at the gateway with one environment variable and cost-per-branch
// appears, because coding agents already put the truth in their requests: the
// working directory, the git state, the output of git and gh in tool results.
//
// # The ladder
//
// Every request is attributed through a provenance ladder. Higher rungs always
// win, and the order is fixed rather than weighted:
//
//  1. Stamp - headers a client was configured to send. Precision tier.
//  2. Structural - repo, branch, and pull request extracted in flight from the
//     request body's structural ground truth.
//  3. Content - a ticket key mentioned in conversation text. Low confidence,
//     and used only with corroboration.
//  4. Unattributed - surfaced honestly wherever attributed numbers appear.
//
// Structural always beats content, however confident the content looks. A
// branch named PAY-1183 is a fact about what someone is doing; the string
// PAY-1183 in a sentence is a mention, and sentences discuss other people's
// tickets all day.
//
// # What this package may do
//
// Pattern and structure matching. Nothing else. The privacy invariant is not
// negotiable: bodies are matched in flight and discarded, only extracted
// metadata persists, and the rules live here in the open so the claim is
// auditable. Anything smarter - classifying an ambiguous session with a model -
// runs asynchronously over metadata that was already extracted, and never
// re-reads a body, because no body was kept.
//
// Evidence recorded alongside an attribution names the kind of thing that
// matched - "branch name", "environment block" - and never the text that
// matched it. That distinction is what keeps the evidence trail from becoming a
// body store with extra steps.
//
// # Cost
//
// Extraction runs inline, on the request path, because that is the only moment
// the body exists. It is bounded three ways: a literal anchor search decides
// whether each pattern is worth running at all, patterns then run over a small
// window rather than the whole body, and the whole thing is skipped once a
// session has structural attribution. Steady-state cost for an attributed
// conversation is one hash of a couple of kilobytes.
package attribution

import (
	"sort"
	"strings"
)

// Rung is a level of the provenance ladder.
type Rung string

const (
	// RungStamp is a header the client was configured to send.
	RungStamp Rung = "stamp"
	// RungStructural is extracted from the request body's structural ground
	// truth: the environment block, git and gh output, remotes.
	RungStructural Rung = "structural"
	// RungContent is a key mentioned in conversation text.
	RungContent Rung = "content"
)

// Rank orders the ladder. Higher wins.
func (r Rung) Rank() int {
	switch r {
	case RungStamp:
		return 3
	case RungStructural:
		return 2
	case RungContent:
		return 1
	default:
		return 0
	}
}

// Valid reports whether r is a rung this package knows.
func (r Rung) Valid() bool { return r.Rank() > 0 }

// Confidence is how much to believe an attribution, shown beside it.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Kinds of work item money can be attributed to.
const (
	KindBranch      = "branch"
	KindPullRequest = "pull_request"
	KindTicket      = "ticket"
)

// kindRank breaks a tie between two candidates found on the same rung.
//
// A ticket is what the work is for, a pull request is how it is being done, and
// a branch is where. Given equally good evidence for two of them, the more
// meaningful unit wins - a leader asking what a piece of work cost means the
// ticket.
func kindRank(kind string) int {
	switch kind {
	case KindTicket:
		return 3
	case KindPullRequest:
		return 2
	case KindBranch:
		return 1
	default:
		return 0
	}
}

// Observation is one fact read out of a request, with where it came from.
type Observation struct {
	Value string
	Rung  Rung
	// Evidence names the kind of thing that was matched - "branch name",
	// "environment block". Never the matched text.
	Evidence string
}

// Found reports whether anything was observed.
func (o Observation) Found() bool { return o.Value != "" }

// better reports whether o should replace other.
func (o Observation) better(other Observation) bool {
	if !o.Found() {
		return false
	}
	if !other.Found() {
		return true
	}
	return o.Rung.Rank() > other.Rung.Rank()
}

// Signals is everything one request revealed about the work behind it.
type Signals struct {
	// Fingerprint groups this request with the other turns of its conversation.
	Fingerprint string
	// Method records how the fingerprint was derived, because the two ways have
	// different reliability and the difference should be visible.
	Method string

	Repo   Observation
	Branch Observation
	Ticket Observation
	// PullRequest holds the number, with Repo carrying the repository it
	// belongs to.
	PullRequest Observation

	// Directory is the working directory's last element. It names a repository
	// only when nothing better was found, because a directory name is where the
	// code is checked out rather than what it is.
	Directory Observation
}

// merge folds newer observations into s, keeping whichever came from the higher
// rung. Used to combine a header stamp with what the body revealed.
func (s *Signals) merge(other Signals) {
	for _, pair := range []struct{ dst, src *Observation }{
		{&s.Repo, &other.Repo},
		{&s.Branch, &other.Branch},
		{&s.Ticket, &other.Ticket},
		{&s.PullRequest, &other.PullRequest},
		{&s.Directory, &other.Directory},
	} {
		if pair.src.better(*pair.dst) {
			*pair.dst = *pair.src
		}
	}
	if other.Fingerprint != "" {
		s.Fingerprint, s.Method = other.Fingerprint, other.Method
	}
}

// Empty reports whether anything at all was observed about the work.
func (s Signals) Empty() bool {
	return !s.Repo.Found() && !s.Branch.Found() && !s.Ticket.Found() &&
		!s.PullRequest.Found() && !s.Directory.Found()
}

// Attribution is the conclusion: what this session is working on, and why we
// think so.
type Attribution struct {
	Kind string
	// Key is the work item's natural identifier: "PAY-1183",
	// "acme/panorama#412", "acme/panorama@feat/x".
	Key string

	Repo   string
	Branch string

	Rung       Rung
	Confidence Confidence
	// Evidence names what was matched, for the display the product promises:
	// "PAY-1183 - branch name (structural, high)".
	Evidence string
}

// Describe renders the attribution the way the console shows it.
func (a Attribution) Describe() string {
	if a.Key == "" {
		return "unattributed"
	}
	if a.Evidence == "" {
		return a.Key
	}
	return a.Key + " - " + a.Evidence + " (" + string(a.Rung) + ", " + string(a.Confidence) + ")"
}

// candidate is one possible conclusion, before the best is chosen.
type candidate struct {
	Attribution
}

// Attribute turns observations into a conclusion, or reports that there is
// none.
//
// Returning false is a real answer and the honest one. Unattributed spend is
// shown next to attributed spend everywhere it appears, so there is never a
// reason to reach for a guess to fill the gap.
func Attribute(s Signals) (Attribution, bool) {
	repo := s.RepoName()

	var candidates []candidate

	if s.Ticket.Found() {
		// A content-rung ticket needs corroboration: a key mentioned in a
		// sentence is only evidence of the work at hand if the request also
		// shows structural signs of being that work. Without it, a conversation
		// that happens to discuss someone else's ticket would bill to it.
		corroborated := s.Ticket.Rung != RungContent ||
			(s.Branch.Found() && s.Branch.Rung == RungStructural) ||
			(s.Repo.Found() && s.Repo.Rung == RungStructural)

		if corroborated {
			candidates = append(candidates, candidate{Attribution{
				Kind: KindTicket, Key: s.Ticket.Value,
				Repo: repo, Branch: s.Branch.Value,
				Rung:       s.Ticket.Rung,
				Confidence: confidenceFor(s.Ticket.Rung),
				Evidence:   s.Ticket.Evidence,
			}})
		}
	}

	if s.PullRequest.Found() && repo != "" {
		candidates = append(candidates, candidate{Attribution{
			Kind: KindPullRequest, Key: repo + "#" + s.PullRequest.Value,
			Repo: repo, Branch: s.Branch.Value,
			Rung:       s.PullRequest.Rung,
			Confidence: confidenceFor(s.PullRequest.Rung),
			Evidence:   s.PullRequest.Evidence,
		}})
	}

	if s.Branch.Found() {
		key := s.Branch.Value
		if repo != "" {
			key = repo + "@" + s.Branch.Value
		}
		candidates = append(candidates, candidate{Attribution{
			Kind: KindBranch, Key: key,
			Repo: repo, Branch: s.Branch.Value,
			Rung:       s.Branch.Rung,
			Confidence: confidenceFor(s.Branch.Rung),
			Evidence:   s.Branch.Evidence,
		}})
	}

	if len(candidates) == 0 {
		return Attribution{}, false
	}

	// Rung dominates; kind breaks the tie. Sorted rather than compared pairwise
	// so that adding a fourth kind later cannot quietly reorder the first three.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Rung.Rank() != b.Rung.Rank() {
			return a.Rung.Rank() > b.Rung.Rank()
		}
		return kindRank(a.Kind) > kindRank(b.Kind)
	})

	return candidates[0].Attribution, true
}

// RepoName picks the best available name for the repository.
//
// A real owner/name from a remote is preferred. A working directory's last
// element is the fallback, because "panorama" is where the code is checked out
// and "acme/panorama" is what it is - and two teams with a directory of the
// same name would otherwise share a work item. It is still much better than
// nothing: most conversations never run a command that prints a remote, and
// cost-per-repository has to work for them too.
func (s Signals) RepoName() string {
	if s.Repo.Found() {
		return s.Repo.Value
	}
	if s.Directory.Found() {
		return s.Directory.Value
	}
	return ""
}

// confidenceFor maps a rung onto how much to believe it.
//
// Structure is trusted as much as a stamp: a branch name read out of a git
// status is not a guess. Content is not, and never becomes so.
func confidenceFor(r Rung) Confidence {
	switch r {
	case RungStamp:
		return ConfidenceHigh
	case RungStructural:
		return ConfidenceHigh
	case RungContent:
		return ConfidenceLow
	default:
		return ConfidenceLow
	}
}

// Supersedes reports whether a should replace an existing attribution.
//
// A session is attributed once and inherits across turns rather than being
// re-guessed per request, so this is the only way an answer changes: strictly
// better evidence, or the same evidence pointing somewhere new. Equal-rung
// churn is refused, because a work item that flickers between two values across
// a conversation is worse than either.
func (a Attribution) Supersedes(current Attribution) bool {
	if current.Key == "" {
		return a.Key != ""
	}
	if a.Key == "" {
		return false
	}
	if a.Rung.Rank() != current.Rung.Rank() {
		return a.Rung.Rank() > current.Rung.Rank()
	}
	// Same rung: a more meaningful unit of work wins, so a session attributed
	// to a branch upgrades to the ticket that branch is for.
	return kindRank(a.Kind) > kindRank(current.Kind)
}

// clean normalises a captured value and rejects anything that should not reach
// the database or the console.
//
// Everything this package extracts is attacker-influenced: a request body is
// whatever a client sent, and a header stamp is whatever it chose to put there.
// Values are bounded and stripped of control characters here, once, rather than
// at each of the places they are later displayed.
func clean(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}

	const maxValue = 200
	if len(v) > maxValue {
		return ""
	}

	for _, r := range v {
		// Printable ASCII only. Branch names, repository names and ticket keys
		// are all drawn from it, and a value outside it is either not one of
		// those or is trying to be something else.
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return v
}
