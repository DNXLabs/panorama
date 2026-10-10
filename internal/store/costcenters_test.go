// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/costcenters"
)

func newCostCenter(t *testing.T, db *DB, code, name string) string {
	t.Helper()
	id, err := db.SaveCostCenter(t.Context(), CostCenter{
		Code: code, Name: name, WarnAtPercent: 80, AcceptsStamp: true,
	})
	if err != nil {
		t.Fatalf("save cost center %s: %v", code, err)
	}
	return id
}

func newRule(t *testing.T, db *DB, center string, match costcenters.Match, pattern string, pos int) string {
	t.Helper()
	id, err := db.SaveCostCenterRule(t.Context(), costcenters.Rule{
		CostCenterID: center, Match: match, Pattern: pattern, Position: pos,
	})
	if err != nil {
		t.Fatalf("save rule %s: %v", pattern, err)
	}
	return id
}

func allocationOf(t *testing.T, db *DB, kind, key string) (center, rule string) {
	t.Helper()
	var c, r *string
	err := db.QueryRowContext(t.Context(), db.rebind(
		`SELECT cost_center_id, cost_center_rule_id FROM work_items WHERE kind = ? AND key = ?`),
		kind, key).Scan(&c, &r)
	if err != nil {
		t.Fatalf("read allocation for %s %s: %v", kind, key, err)
	}
	if c != nil {
		center = *c
	}
	if r != nil {
		rule = *r
	}
	return center, rule
}

// seedWork puts one attributed session into the database and returns nothing:
// every assertion below is about where its work item landed.
func seedWork(t *testing.T, db *DB, fingerprint, repo, branch, ticket string) {
	t.Helper()
	user, err := db.CreateUser(t.Context(), fingerprint+"@example.com", "Dev", RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	signals := attribution.Signals{
		Repo:   structural(repo, "git remote"),
		Branch: structural(branch, "environment block"),
	}
	if ticket != "" {
		signals.Ticket = structural(ticket, "branch name")
	}
	if _, err := db.AttributeSession(t.Context(),
		observe(fingerprint, user.ID, signals, time.Now().UTC())); err != nil {
		t.Fatalf("attribute: %v", err)
	}
}

func TestANewWorkItemIsAllocatedWhenItIsCreated(t *testing.T) {
	// Waiting for the next rule write would leave a repository seen for the
	// first time indistinguishable, on the worklist, from one no rule covers.
	eachDialect(t, func(t *testing.T, db *DB) {
		payments := newCostCenter(t, db, "CC-100", "Payments")
		rule := newRule(t, db, payments, costcenters.MatchRepo, "acme/payments-*", 0)

		seedWork(t, db, "fp-1", "acme/payments-api", "feat/x", "")

		center, got := allocationOf(t, db, "branch", "acme/payments-api@feat/x")
		if center != payments || got != rule {
			t.Errorf("allocation = %q by %q, want %q by %q", center, got, payments, rule)
		}
	})
}

func TestARuleWrittenLaterRestatesWorkAlreadyRecorded(t *testing.T) {
	// The requirement this whole design hangs on: rules change mid-month, and
	// when they do every period is restated, closed ones included.
	eachDialect(t, func(t *testing.T, db *DB) {
		seedWork(t, db, "fp-1", "acme/payments-api", "feat/x", "")

		if center, _ := allocationOf(t, db, "branch", "acme/payments-api@feat/x"); center != "" {
			t.Fatalf("allocated to %q before any rule existed", center)
		}

		payments := newCostCenter(t, db, "CC-100", "Payments")
		newRule(t, db, payments, costcenters.MatchRepo, "acme/payments-*", 0)

		if center, _ := allocationOf(t, db, "branch", "acme/payments-api@feat/x"); center != payments {
			t.Errorf("allocation = %q, want %q - the rule did not reach recorded work", center, payments)
		}
	})
}

func TestDeletingARuleReturnsItsWorkToUnallocated(t *testing.T) {
	// Not to something plausible. The work reappears in the worklist, which is
	// the visible outcome rather than the quiet one.
	eachDialect(t, func(t *testing.T, db *DB) {
		payments := newCostCenter(t, db, "CC-100", "Payments")
		rule := newRule(t, db, payments, costcenters.MatchRepo, "acme/payments-*", 0)
		seedWork(t, db, "fp-1", "acme/payments-api", "feat/x", "")

		ok, err := db.DeleteCostCenterRule(t.Context(), rule)
		if err != nil || !ok {
			t.Fatalf("delete rule: ok=%v err=%v", ok, err)
		}

		center, got := allocationOf(t, db, "branch", "acme/payments-api@feat/x")
		if center != "" || got != "" {
			t.Errorf("allocation = %q by %q, want unallocated", center, got)
		}
	})
}

func TestReorderingRulesChangesWhichOneClaimsTheWork(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		payments := newCostCenter(t, db, "CC-100", "Payments")
		platform := newCostCenter(t, db, "CC-200", "Platform")

		narrow := newRule(t, db, payments, costcenters.MatchRepo, "acme/payments-*", 0)
		broad := newRule(t, db, platform, costcenters.MatchRepo, "acme/*", 1)

		seedWork(t, db, "fp-1", "acme/payments-api", "feat/x", "")
		if center, _ := allocationOf(t, db, "branch", "acme/payments-api@feat/x"); center != payments {
			t.Fatalf("allocation = %q, want the narrow rule to win", center)
		}

		if err := db.ReorderCostCenterRules(t.Context(), []string{broad, narrow}); err != nil {
			t.Fatalf("reorder: %v", err)
		}
		if center, _ := allocationOf(t, db, "branch", "acme/payments-api@feat/x"); center != platform {
			t.Errorf("allocation = %q, want %q after reordering", center, platform)
		}
	})
}

func TestATicketRuleReachesWorkInAMonorepo(t *testing.T) {
	// The case repository rules cannot serve, and it needs no integration: the
	// ticket work item comes from the branch name structurally.
	eachDialect(t, func(t *testing.T, db *DB) {
		payments := newCostCenter(t, db, "CC-100", "Payments")
		platform := newCostCenter(t, db, "CC-200", "Platform")

		rule := newRule(t, db, payments, costcenters.MatchTicket, "PAY-*", 0)
		newRule(t, db, platform, costcenters.MatchRepo, "*", 1)

		seedWork(t, db, "fp-1", "acme/monorepo", "feat/PAY-1183-costs", "PAY-1183")

		center, got := allocationOf(t, db, "ticket", "PAY-1183")
		if center != payments || got != rule {
			t.Errorf("allocation = %q by %q, want the ticket rule to win in a monorepo", center, got)
		}
	})
}

func TestACostCenterWithRecordedSpendCannotBeDeleted(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		payments := newCostCenter(t, db, "CC-100", "Payments")
		newRule(t, db, payments, costcenters.MatchRepo, "acme/*", 0)
		seedWork(t, db, "fp-1", "acme/panorama", "feat/x", "")

		_, err := db.DeleteCostCenter(t.Context(), payments)
		if !errors.Is(err, ErrCostCenterInUse) {
			t.Errorf("delete error = %v, want ErrCostCenterInUse", err)
		}
	})
}

func TestArchivingKeepsExistingRulesMatching(t *testing.T) {
	// The point of archiving over deleting: a closed account still owns its
	// history. If archiving stopped its rules matching, closing an account
	// would silently move a year of recorded spend into the unallocated bucket.
	eachDialect(t, func(t *testing.T, db *DB) {
		payments := newCostCenter(t, db, "CC-100", "Payments")
		newRule(t, db, payments, costcenters.MatchRepo, "acme/*", 0)
		seedWork(t, db, "fp-1", "acme/panorama", "feat/x", "")

		if _, err := db.SaveCostCenter(t.Context(), CostCenter{
			ID: payments, Code: "CC-100", Name: "Payments",
			WarnAtPercent: 80, AcceptsStamp: true, Archived: true,
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if _, err := db.Reallocate(t.Context()); err != nil {
			t.Fatalf("reallocate: %v", err)
		}

		if center, _ := allocationOf(t, db, "branch", "acme/panorama@feat/x"); center != payments {
			t.Errorf("allocation = %q after archiving, want it to stay on %q", center, payments)
		}

		// It takes no new rules, though.
		_, err := db.SaveCostCenterRule(t.Context(), costcenters.Rule{
			CostCenterID: payments, Match: costcenters.MatchRepo, Pattern: "acme/*",
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("adding a rule to an archived line: err = %v, want ErrInvalidInput", err)
		}
	})
}

func TestOneRuleMayNotBeWrittenTwice(t *testing.T) {
	// Two cost centers claiming one pattern is not a precedence question to be
	// settled by position - it is somebody's mistake, and the ordered list
	// would resolve it silently and permanently.
	eachDialect(t, func(t *testing.T, db *DB) {
		payments := newCostCenter(t, db, "CC-100", "Payments")
		platform := newCostCenter(t, db, "CC-200", "Platform")
		newRule(t, db, payments, costcenters.MatchRepo, "acme/*", 0)

		if _, err := db.SaveCostCenterRule(t.Context(), costcenters.Rule{
			CostCenterID: platform, Match: costcenters.MatchRepo, Pattern: "ACME/*",
		}); err == nil {
			t.Error("a duplicate pattern was accepted")
		}
	})
}

func TestOnlyAStampableLineIsInTheSnapshot(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		open := newCostCenter(t, db, "CC-100", "Payments")

		closed, err := db.SaveCostCenter(t.Context(), CostCenter{
			Code: "CC-200", Name: "Capitalised", WarnAtPercent: 80, AcceptsStamp: false,
		})
		if err != nil {
			t.Fatal(err)
		}
		gone, err := db.SaveCostCenter(t.Context(), CostCenter{
			Code: "CC-300", Name: "Retired", WarnAtPercent: 80, AcceptsStamp: true, Archived: true,
		})
		if err != nil {
			t.Fatal(err)
		}

		stampable, err := db.StampableCostCenters(t.Context())
		if err != nil {
			t.Fatalf("stampable: %v", err)
		}

		if got := stampable.Lookup("cc-100"); got != open {
			t.Errorf("Lookup(cc-100) = %q, want %q", got, open)
		}
		if got := stampable.Lookup("CC-200"); got != "" {
			t.Errorf("a line refusing stamps resolved to %q (%s)", got, closed)
		}
		if got := stampable.Lookup("CC-300"); got != "" {
			t.Errorf("an archived line resolved to %q (%s)", got, gone)
		}
	})
}
