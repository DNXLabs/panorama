// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package costcenters

import "testing"

func repoRule(id, center, pattern string, pos int) Rule {
	return Rule{ID: id, CostCenterID: center, Position: pos, Match: MatchRepo, Pattern: pattern}
}

func ticketRule(id, center, pattern string, pos int) Rule {
	return Rule{ID: id, CostCenterID: center, Position: pos, Match: MatchTicket, Pattern: pattern}
}

func TestAStarCrossesASlash(t *testing.T) {
	// path.Match was the obvious thing to reach for and is wrong here: its star
	// stops at a separator, so this pattern would match nothing and a quarter of
	// spend would land in the unallocated bucket with no explanation.
	r := repoRule("r1", "cc1", "*payments*", 0)
	if !r.Matches(Subject{Kind: "branch", Repo: "acme/payments-api"}) {
		t.Error("*payments* should match acme/payments-api")
	}
}

func TestMatchingFoldsCase(t *testing.T) {
	r := repoRule("r1", "cc1", "ACME/Payments-*", 0)
	if !r.Matches(Subject{Kind: "branch", Repo: "acme/payments-api"}) {
		t.Error("a repository pattern should match without regard to case")
	}
	tr := ticketRule("r2", "cc1", "pay-*", 0)
	if !tr.Matches(Subject{Kind: KindTicket, Key: "PAY-1183"}) {
		t.Error("a ticket pattern should match without regard to case")
	}
}

func TestACatchAllTicketRuleDoesNotClaimPullRequests(t *testing.T) {
	// A pull request's key is "acme/panorama#412" and a branch's is
	// "acme/panorama@feat/x". Without the kind check, "*" on a ticket rule
	// takes both, and every rule below it is dead.
	r := ticketRule("r1", "cc1", "*", 0)

	if r.Matches(Subject{Kind: "pull_request", Key: "acme/panorama#412"}) {
		t.Error("a ticket rule claimed a pull request")
	}
	if r.Matches(Subject{Kind: "branch", Key: "acme/panorama@feat/x"}) {
		t.Error("a ticket rule claimed a branch")
	}
	if !r.Matches(Subject{Kind: KindTicket, Key: "PAY-1183"}) {
		t.Error("a ticket rule should claim a ticket")
	}
}

func TestARepositoryRuleNeedsARepository(t *testing.T) {
	// A ticket reached by a content signal can carry no repository. There is
	// nothing to match, so nothing matched - a catch-all included.
	r := repoRule("r1", "cc1", "*", 0)
	if r.Matches(Subject{Kind: KindTicket, Key: "PAY-1183", Repo: ""}) {
		t.Error("a repository rule claimed a work item with no repository")
	}
}

func TestTheFirstMatchingRuleWins(t *testing.T) {
	// The narrow rule is listed first, which is the whole of the precedence.
	rules := []Rule{
		repoRule("r1", "payments", "acme/payments-*", 0),
		repoRule("r2", "platform", "acme/*", 1),
	}

	got, ok := Resolve(rules, Subject{Kind: "branch", Repo: "acme/payments-api"})
	if !ok || got.CostCenterID != "payments" {
		t.Fatalf("resolved to %q (ok=%v), want payments", got.CostCenterID, ok)
	}

	got, ok = Resolve(rules, Subject{Kind: "branch", Repo: "acme/panorama"})
	if !ok || got.CostCenterID != "platform" {
		t.Fatalf("resolved to %q (ok=%v), want platform", got.CostCenterID, ok)
	}
}

func TestUnallocatedIsAnAnswerRatherThanAFailure(t *testing.T) {
	// The product's position on the gap, expressed as a return value: no rule
	// claimed this, and nothing invents one. A fallback here is what would stop
	// the missing rule from ever being written.
	rules := []Rule{repoRule("r1", "payments", "acme/payments-*", 0)}

	if _, ok := Resolve(rules, Subject{Kind: "branch", Repo: "acme/website"}); ok {
		t.Error("an unmatched repository should resolve to nothing")
	}
	if _, ok := Resolve(nil, Subject{Kind: "branch", Repo: "acme/website"}); ok {
		t.Error("no rules at all should resolve to nothing")
	}
}

func TestATicketPrefixRescuesAMonorepo(t *testing.T) {
	// The case repository rules cannot serve: one repository, several products.
	// The ticket rung needs no integration - a branch named feat/PAY-1183-x is
	// attributed to the ticket structurally.
	rules := []Rule{
		ticketRule("r1", "payments", "PAY-*", 0),
		ticketRule("r2", "infra", "INFRA-*", 1),
		repoRule("r3", "platform", "*", 2),
	}

	mono := "acme/monorepo"
	for _, tc := range []struct{ key, want string }{
		{"PAY-1183", "payments"},
		{"INFRA-77", "infra"},
		{"MISC-4", "platform"}, // falls to the repository catch-all
	} {
		got, ok := Resolve(rules, Subject{Kind: KindTicket, Key: tc.key, Repo: mono})
		if !ok || got.CostCenterID != tc.want {
			t.Errorf("%s resolved to %q (ok=%v), want %q", tc.key, got.CostCenterID, ok, tc.want)
		}
	}
}

func TestUnreachableFindsRulesUnderACatchAll(t *testing.T) {
	rules := []Rule{
		repoRule("r1", "platform", "*", 0),
		repoRule("r2", "payments", "acme/payments-*", 1),
		ticketRule("r3", "payments", "PAY-*", 2), // a different kind, still live
	}

	got := Unreachable(rules)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("unreachable = %v, want [1]", got)
	}
}

func TestUnreachableFindsADuplicatePattern(t *testing.T) {
	rules := []Rule{
		repoRule("r1", "platform", "acme/*", 0),
		repoRule("r2", "payments", "ACME/*", 1),
	}
	if got := Unreachable(rules); len(got) != 1 || got[0] != 1 {
		t.Fatalf("unreachable = %v, want [1]", got)
	}
}

func TestEveryStampFailureLooksTheSame(t *testing.T) {
	// Unknown code, a line that refuses stamps, and no header at all are one
	// answer on the hot path, because the caller does the same thing with all
	// three: allocate by the rules, and keep what was sent.
	s := Stampable{"cc-100": "id-100"}

	if got := s.Lookup("CC-100"); got != "id-100" {
		t.Errorf("Lookup(CC-100) = %q, want id-100", got)
	}
	for _, in := range []string{"", "   ", "CC-999", "cc 100"} {
		if got := s.Lookup(in); got != "" {
			t.Errorf("Lookup(%q) = %q, want empty", in, got)
		}
	}
}

func TestValidatePatternRefusesWhatCannotWork(t *testing.T) {
	if err := ValidatePattern(""); err == nil {
		t.Error("an empty pattern should be refused")
	}
	if err := ValidatePattern(" acme/* "); err == nil {
		t.Error("a padded pattern should be refused rather than silently trimmed")
	}
	// A catch-all is legitimate and deliberate: it is a visible row an
	// administrator wrote, which is what separates it from the implicit
	// fallback this package refuses.
	if err := ValidatePattern("*"); err != nil {
		t.Errorf("a catch-all should be allowed: %v", err)
	}
}
