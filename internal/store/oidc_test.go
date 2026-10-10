// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"testing"
	"time"
)

// TestTheStrongestMatchingGroupWins. Anything else would make a person's role
// depend on the order their provider happened to list their groups, and would
// mean adding someone to a second group could silently demote them.
func TestTheStrongestMatchingGroupWins(t *testing.T) {
	mapping := []GroupRole{
		{Group: "eng", Role: RoleDeveloper},
		{Group: "platform", Role: RoleAdmin},
		{Group: "leads", Role: RoleManager},
	}

	for name, tc := range map[string]struct {
		groups      []string
		defaultRole string
		want        string
		permitted   bool
	}{
		"one match": {
			groups: []string{"eng"}, defaultRole: RoleDeveloper,
			want: RoleDeveloper, permitted: true,
		},
		"the strongest of several, whatever the order": {
			groups: []string{"eng", "platform", "leads"}, defaultRole: RoleDeveloper,
			want: RoleAdmin, permitted: true,
		},
		"the same set, reversed": {
			groups: []string{"leads", "platform", "eng"}, defaultRole: RoleDeveloper,
			want: RoleAdmin, permitted: true,
		},
		"case does not decide who is an admin": {
			groups: []string{"PLATFORM"}, defaultRole: RoleDeveloper,
			want: RoleAdmin, permitted: true,
		},
		"no match falls to the default": {
			groups: []string{"marketing"}, defaultRole: RoleDeveloper,
			want: RoleDeveloper, permitted: true,
		},
		"no match and no default refuses": {
			groups: []string{"marketing"}, defaultRole: "none",
			want: "", permitted: false,
		},
		"no groups at all and no default refuses": {
			groups: nil, defaultRole: "none",
			want: "", permitted: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, permitted := ResolveRole(Identity{Groups: tc.groups}, mapping, tc.defaultRole)
			if permitted != tc.permitted {
				t.Fatalf("permitted = %v, want %v", permitted, tc.permitted)
			}
			if got != tc.want {
				t.Errorf("role = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTwoCasingsOfOneGroupResolveToTheStronger. The matcher lowercases both
// sides, so "Platform" and "platform" are one group - but they are two rows, and giving
// the slot to whichever was read last would make the role an account holds
// depend on row order, with somebody quietly not being an administrator any
// more as the symptom.
//
// The write path refuses to create the pair. This is what makes a pair that
// already exists resolve the same way every time, and it resolves the way every
// other ambiguity here does: the strongest wins.
func TestTwoCasingsOfOneGroupResolveToTheStronger(t *testing.T) {
	for name, mapping := range map[string][]GroupRole{
		"weaker first": {
			{Group: "Platform", Role: RoleDeveloper},
			{Group: "platform", Role: RoleAdmin},
		},
		"stronger first": {
			{Group: "platform", Role: RoleAdmin},
			{Group: "Platform", Role: RoleDeveloper},
		},
		"and untrimmed, which a hand-edited row can be": {
			{Group: " platform ", Role: RoleAdmin},
			{Group: "Platform", Role: RoleDeveloper},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, permitted := ResolveRole(
				Identity{Groups: []string{"Platform"}}, mapping, RoleDeveloper)
			if !permitted {
				t.Fatal("refused a mapped group")
			}
			if got != RoleAdmin {
				t.Errorf("role = %q, want %q - row order decided it", got, RoleAdmin)
			}
		})
	}
}

// TestSignInAdoptsAnAccountCreatedBeforeSSO. `panorama token create` makes a
// user by email before there is any identity provider. The owner's first
// sign-in has to land on that account rather than creating a second one beside
// it, or their existing traffic detaches from them.
func TestSignInAdoptsAnAccountCreatedBeforeSSO(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		existing, err := db.CreateUser(ctx, "dev@example.com", "dev@example.com", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		user, roleChanged, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "dev@example.com", DisplayName: "Dev Eloper",
		}, RoleManager, RoleSourceSSO)
		if err != nil {
			t.Fatalf("UpsertIdentity: %v", err)
		}
		if user.ID != existing.ID {
			t.Fatalf("sign-in created a second account (%s) beside the existing one (%s)", user.ID, existing.ID)
		}
		if !roleChanged {
			t.Error("the role moved from developer to manager but was not reported as changed")
		}
		if user.DisplayName != "Dev Eloper" {
			t.Errorf("display name = %q, want it refreshed from the token", user.DisplayName)
		}

		count, err := db.CountUsers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%d users, want 1", count)
		}
	})
}

// TestAnAssignedRoleSurvivesTheNextSignIn.
//
// The bug this pins down was reported from a live deployment: an account made
// an administrator was a developer again one sign-in later, because every
// sign-in recomputed the role from a group mapping that matched nothing. The
// promotion was real, was recorded, and lasted until the browser reloaded.
func TestAnAssignedRoleSurvivesTheNextSignIn(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		user, _, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "owner@example.com", DisplayName: "Owner",
		}, RoleDeveloper, RoleSourceSSO)
		if err != nil {
			t.Fatal(err)
		}

		if serr := db.SetUserRole(ctx, user.ID, RoleAdmin); serr != nil {
			t.Fatal(serr)
		}

		// A sign-in carrying no mapped group, which resolves to the default
		// role. Before role_source this silently won.
		after, roleChanged, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "owner@example.com", DisplayName: "Owner",
		}, RoleDeveloper, RoleSourceSSO)
		if err != nil {
			t.Fatal(err)
		}
		if after.Role != RoleAdmin {
			t.Errorf("role = %q after signing in, want %q - an assigned role must outlive a sign-in", after.Role, RoleAdmin)
		}
		if after.RoleSource != RoleSourceManual {
			t.Errorf("role source = %q, want %q", after.RoleSource, RoleSourceManual)
		}
		if roleChanged {
			t.Error("no role changed, but the change was reported - that signs the user out of every browser for nothing")
		}
	})
}

// TestRemovingSomebodyFromAGroupStillDemotesThem.
//
// The other half of the same rule, and the more important one: a role the
// mapping granted is a role the mapping can take away. If pinning applied to
// every account, removing an admin from the admin group at the provider would
// do nothing at all.
func TestRemovingSomebodyFromAGroupStillDemotesThem(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		if _, _, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "lead@example.com", DisplayName: "Lead",
		}, RoleAdmin, RoleSourceSSO); err != nil {
			t.Fatal(err)
		}

		after, roleChanged, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "lead@example.com", DisplayName: "Lead",
		}, RoleDeveloper, RoleSourceSSO)
		if err != nil {
			t.Fatal(err)
		}
		if after.Role != RoleDeveloper {
			t.Errorf("role = %q, want %q - the mapping's grant is the mapping's to revoke", after.Role, RoleDeveloper)
		}
		if !roleChanged {
			t.Error("a demotion was not reported as a change, so the demoted admin keeps their live sessions")
		}
	})
}

// TestFollowGroupMappingHandsTheAccountBack. Unpinning cannot recompute the
// role, because group membership is the provider's record and nothing here
// keeps a copy. What it can do is let the next sign-in decide.
func TestFollowGroupMappingHandsTheAccountBack(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		user, _, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "person@example.com", DisplayName: "Person",
		}, RoleDeveloper, RoleSourceSSO)
		if err != nil {
			t.Fatal(err)
		}
		if serr := db.SetUserRole(ctx, user.ID, RoleAdmin); serr != nil {
			t.Fatal(serr)
		}
		if serr := db.FollowGroupMapping(ctx, user.ID); serr != nil {
			t.Fatal(serr)
		}

		unpinned, err := db.UserByID(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unpinned.Role != RoleAdmin {
			t.Errorf("role = %q, want it unchanged until a sign-in recomputes it", unpinned.Role)
		}

		after, _, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "person@example.com", DisplayName: "Person",
		}, RoleDeveloper, RoleSourceSSO)
		if err != nil {
			t.Fatal(err)
		}
		if after.Role != RoleDeveloper {
			t.Errorf("role = %q, want the mapping to decide again once it was handed back", after.Role)
		}
	})
}

// TestAnEmailChangeFollowsTheSubject. The provider owns identities; the subject
// claim is the stable one.
func TestAnEmailChangeFollowsTheSubject(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		first, _, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "old@example.com", DisplayName: "Person",
		}, RoleDeveloper, RoleSourceSSO)
		if err != nil {
			t.Fatal(err)
		}

		second, roleChanged, err := db.UpsertIdentity(ctx, Identity{
			Subject: "sub-1", Email: "new@example.com", DisplayName: "Person",
		}, RoleDeveloper, RoleSourceSSO)
		if err != nil {
			t.Fatal(err)
		}
		if second.ID != first.ID {
			t.Errorf("an email change created a second account")
		}
		if second.Email != "new@example.com" {
			t.Errorf("email = %q, want it to follow the provider", second.Email)
		}
		if roleChanged {
			t.Error("an unchanged role was reported as changed, which would sign the user out for nothing")
		}
	})
}

// TestSignInNeedsTheClaimsItSaysItNeeds. A provider configured without the
// email scope produces a token Panorama cannot turn into an account, and the
// honest answer is to say so rather than to invent one.
func TestSignInNeedsTheClaimsItSaysItNeeds(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		for name, identity := range map[string]Identity{
			"no email":   {Subject: "sub-1"},
			"no subject": {Email: "dev@example.com"},
		} {
			t.Run(name, func(t *testing.T) {
				_, _, err := db.UpsertIdentity(t.Context(), identity, RoleDeveloper, RoleSourceSSO)
				if !errors.Is(err, ErrInvalidInput) {
					t.Errorf("UpsertIdentity = %v, want ErrInvalidInput", err)
				}
			})
		}
	})
}

// TestASignInFlowIsUsableOnce. The row is deleted when it is read, which makes
// an authorization-code replay useless without any reasoning about windows.
func TestASignInFlowIsUsableOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		flow := OIDCFlow{
			State: "state-1", Nonce: "nonce-1", Verifier: "verifier-1",
			RedirectTo: "/work", ExpiresAt: time.Now().UTC().Add(time.Minute),
		}
		if err := db.StartOIDCFlow(ctx, flow); err != nil {
			t.Fatalf("StartOIDCFlow: %v", err)
		}

		got, err := db.TakeOIDCFlow(ctx, "state-1")
		if err != nil {
			t.Fatalf("TakeOIDCFlow: %v", err)
		}
		if got.Verifier != "verifier-1" || got.RedirectTo != "/work" {
			t.Errorf("flow = %+v, want what was stored", got)
		}

		if _, err := db.TakeOIDCFlow(ctx, "state-1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a second take = %v, want ErrNotFound", err)
		}
	})
}

// TestAnExpiredFlowIsConsumedAndRefused, rather than left behind to be retried
// until a sweeper happens to run.
func TestAnExpiredFlowIsConsumedAndRefused(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		if err := db.StartOIDCFlow(ctx, OIDCFlow{
			State: "old", Nonce: "n", Verifier: "v",
			ExpiresAt: time.Now().UTC().Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := db.TakeOIDCFlow(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Errorf("taking an expired flow = %v, want ErrNotFound", err)
		}

		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM oidc_flows`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d expired flows left behind, want 0", n)
		}
	})
}

// TestGroupMappingRejectsARoleNobodyDefined, because the mapping is what grants
// privilege and a typo in it should not become a role.
func TestGroupMappingRejectsARoleNobodyDefined(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		if err := db.SetGroupRole(ctx, "platform", "superuser"); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("SetGroupRole with an invented role = %v, want ErrInvalidInput", err)
		}
		if err := db.SetGroupRole(ctx, "  ", RoleAdmin); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("SetGroupRole with a blank group = %v, want ErrInvalidInput", err)
		}

		if err := db.SetGroupRole(ctx, "platform", RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if err := db.SetGroupRole(ctx, "platform", RoleManager); err != nil {
			t.Fatal(err)
		}

		roles, err := db.GroupRoles(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(roles) != 1 || roles[0].Role != RoleManager {
			t.Errorf("mapping = %+v, want one entry updated in place", roles)
		}

		removed, err := db.DeleteGroupRole(ctx, "platform")
		if err != nil || !removed {
			t.Errorf("DeleteGroupRole = %v, %v", removed, err)
		}
	})
}

// TestPurgingSessionsIsHousekeepingOnly - expiry is enforced on read, so this
// only has to stop two tables growing forever.
func TestPurgingSessionsIsHousekeepingOnly(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()
		now := time.Now().UTC()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}

		if _, cerr := db.CreateConsoleSession(ctx, user.ID, "hash-old", "csrf", "", "", now.Add(-time.Hour)); cerr != nil {
			t.Fatal(cerr)
		}
		if _, cerr := db.CreateConsoleSession(ctx, user.ID, "hash-live", "csrf", "", "", now.Add(time.Hour)); cerr != nil {
			t.Fatal(cerr)
		}

		// The expired one is already unusable, before any sweeping.
		if _, _, aerr := db.AuthenticateConsoleSession(ctx, "hash-old"); !errors.Is(aerr, ErrSessionRejected) {
			t.Errorf("an expired session authenticated: %v", aerr)
		}

		removed, err := db.PurgeExpiredSessions(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 1 {
			t.Errorf("purged %d rows, want 1", removed)
		}

		if _, _, err := db.AuthenticateConsoleSession(ctx, "hash-live"); err != nil {
			t.Errorf("the live session was swept: %v", err)
		}
	})
}

// TestObservedGroupsRecordWhatTheProviderActuallySent.
//
// The mapping asks an administrator to type group names, and nothing else can
// tell them what theirs are called. A wrong guess grants
// nothing and fails silently, so the recorded names are what turn that from
// guesswork into picking from a list.
func TestObservedGroupsRecordWhatTheProviderActuallySent(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		if err := db.RecordSignIn(ctx, SignInObservation{Groups: []string{"platform", "engineering"}}); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordSignIn(ctx, SignInObservation{Groups: []string{"platform"}}); err != nil {
			t.Fatal(err)
		}

		got, err := db.ObservedGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 groups, got %d: %+v", len(got), got)
		}
		// Most-seen first, so the groups a deployment runs on sort above the one
		// somebody was in once.
		if got[0].Group != "platform" || got[0].Count != 2 {
			t.Fatalf("want platform seen twice at the top, got %+v", got[0])
		}
		if got[1].Group != "engineering" || got[1].Count != 1 {
			t.Fatalf("want engineering seen once, got %+v", got[1])
		}
		if !got[0].LastSeen.After(got[0].FirstSeen) && got[0].LastSeen.Before(got[0].FirstSeen) {
			t.Fatal("last_seen must not precede first_seen")
		}
	})
}

// TestASignInCarryingNoGroupsIsCountedSeparately. One is a person in no group,
// which is ordinary. All of them is a claim that is not being emitted, which is
// a misconfiguration nothing else would surface.
func TestASignInCarryingNoGroupsIsCountedSeparately(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		for _, groups := range [][]string{
			{"platform"},
			nil,
			{},
			{"   "}, // whitespace is not a group name
		} {
			if err := db.RecordSignIn(ctx, SignInObservation{Groups: groups}); err != nil {
				t.Fatal(err)
			}
		}

		counters, err := db.SignInCounters(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if counters[CounterSignIns] != 4 {
			t.Fatalf("want 4 sign-ins, got %d", counters[CounterSignIns])
		}
		if counters[CounterSignInsWithoutGroups] != 3 {
			t.Fatalf("want 3 sign-ins without groups, got %d", counters[CounterSignInsWithoutGroups])
		}

		groups, err := db.ObservedGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Group != "platform" {
			t.Fatalf("blank group names must not be recorded, got %+v", groups)
		}
	})
}

// TestARepeatedGroupInOneTokenCountsOnce. A provider listing a group twice in
// one token is one sign-in that carried it, not two.
func TestARepeatedGroupInOneTokenCountsOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		if err := db.RecordSignIn(ctx, SignInObservation{Groups: []string{"platform", "platform", " platform "}}); err != nil {
			t.Fatal(err)
		}

		got, err := db.ObservedGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Count != 1 {
			t.Fatalf("want one group seen once, got %+v", got)
		}
	})
}

// TestObservedGroupsAreEmptyBeforeAnySignIn, so the console can tell "nothing
// has arrived" from "groups arrived and none were mapped".
func TestObservedGroupsAreEmptyBeforeAnySignIn(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		groups, err := db.ObservedGroups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 0 {
			t.Fatalf("want nothing observed, got %+v", groups)
		}

		counters, err := db.SignInCounters(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(counters) != 0 {
			t.Fatalf("want no counters, got %+v", counters)
		}
	})
}
