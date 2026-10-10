// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package ssosetup

import (
	"sort"
	"strings"
)

// ClaimFact is one claim name an ID token has been seen to carry.
//
// A name and two counts, never a value. What it answers is "what vocabulary is
// this provider using", which is a question about the token's shape.
type ClaimFact struct {
	Name  string
	Count int64
	// Empty counts the sign-ins where the claim was present and carried
	// nothing. Present-and-empty and absent are different diagnoses with
	// opposite fixes, and the count is the only thing that separates them.
	Empty int64
}

// Claim names that carry group membership somewhere, and are worth suggesting
// by name rather than by pattern.
//
// Not a complete list and cannot be: the point of the pattern below is the
// providers nobody has enumerated, including the namespaced claims an Auth0 or
// Okta rule emits under a URL.
var knownGroupClaims = []string{
	"groups",
	"roles",
	"memberOf",
	"member_of",
	"cognito:groups",
	"group_membership",
	"realm_access",
	"resource_access",
}

// GroupClaimCandidates picks, out of the claim names a provider actually sent,
// the ones that could plausibly be the groups claim.
//
// This is the answer to "what does my provider call its groups" in the only
// form that is not a guess: the provider's own vocabulary, filtered. The
// configured name is excluded - the caller already knows that one did not work,
// and offering it back as a suggestion would read as a bug.
func GroupClaimCandidates(configured string, names []string) []string {
	known := map[string]bool{}
	for _, k := range knownGroupClaims {
		known[strings.ToLower(k)] = true
	}

	var out []string
	for _, name := range names {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "" || lower == strings.ToLower(strings.TrimSpace(configured)) {
			continue
		}
		// The pattern rather than the list is what catches a namespaced claim
		// like https://your-tenant.example.com/claims/groups, which is how
		// Auth0 and Okta rules emit one and which no list will ever contain.
		if known[lower] || strings.Contains(lower, "group") || strings.Contains(lower, "memberof") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Microsoft Entra ID replaces the groups claim with a pointer to Microsoft
// Graph when a user is in more than the number of groups it will put in a
// token. The claim does not shrink and it does not error: it is simply gone,
// replaced by these two, and the sign-in succeeds with no group membership at
// all. It is worth naming because the deployment it breaks is the large one,
// and it breaks for one person rather than for everybody.
const (
	claimNamesOverage   = "_claim_names"
	claimSourcesOverage = "_claim_sources"
)

// GroupsOverage reports whether a provider signalled that it withheld group
// membership because there was too much of it.
func GroupsOverage(names []string) bool {
	for _, name := range names {
		if name == claimNamesOverage || name == claimSourcesOverage {
			return true
		}
	}
	return false
}
