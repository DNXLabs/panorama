// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
)

// TestARosterIsReplacedWholesale. The console edits membership as a list, so
// the store takes one: the team ends up with exactly the people it was given.
func TestARosterIsReplacedWholesale(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		team, err := db.CreateTeam(t.Context(), "Platform", "", "")
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		alice := mustUser(t, db, "alice@example.com")
		bob := mustUser(t, db, "bob@example.com")

		if serr := db.SetTeamMembers(t.Context(), team.ID, []TeamMemberInput{
			{UserID: alice.ID, Role: TeamRoleLead},
			{UserID: bob.ID},
		}); serr != nil {
			t.Fatalf("set members: %v", serr)
		}

		detail, err := db.TeamWithMembers(t.Context(), team.Slug)
		if err != nil {
			t.Fatalf("read team: %v", err)
		}
		if len(detail.Members) != 2 {
			t.Fatalf("%d members, want 2", len(detail.Members))
		}
		for _, m := range detail.Members {
			if m.UserID == alice.ID && m.Role != TeamRoleLead {
				t.Errorf("the lead role did not survive the write: %q", m.Role)
			}
			// An empty role is stored as member rather than rejected: the
			// console sends a roster, not a role list.
			if m.UserID == bob.ID && m.Role != TeamRoleMember {
				t.Errorf("an unstated role became %q, want member", m.Role)
			}
		}

		// Absent from the list means off the team.
		if serr := db.SetTeamMembers(t.Context(), team.ID, []TeamMemberInput{{UserID: bob.ID}}); serr != nil {
			t.Fatalf("shrink: %v", serr)
		}
		detail, _ = db.TeamWithMembers(t.Context(), team.Slug)
		if len(detail.Members) != 1 || detail.Members[0].UserID != bob.ID {
			t.Fatalf("the roster is %v, want just bob", detail.Members)
		}

		// An empty roster is a real instruction.
		if serr := db.SetTeamMembers(t.Context(), team.ID, nil); serr != nil {
			t.Fatalf("empty: %v", serr)
		}
		detail, _ = db.TeamWithMembers(t.Context(), team.Slug)
		if len(detail.Members) != 0 {
			t.Errorf("%d members remain", len(detail.Members))
		}
	})
}

// TestDeletingATeamTakesItsMembershipAndNotThePeople. The cascade is on the
// join table; an account is not somebody's to delete by tidying up a team list.
func TestDeletingATeamTakesItsMembershipAndNotThePeople(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		team, err := db.CreateTeam(t.Context(), "Platform", "", "")
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		alice := mustUser(t, db, "alice@example.com")
		if aerr := db.AddTeamMember(t.Context(), team.ID, alice.ID, TeamRoleMember); aerr != nil {
			t.Fatalf("add member: %v", aerr)
		}

		removed, err := db.DeleteTeam(t.Context(), team.ID)
		if err != nil || !removed {
			t.Fatalf("delete: %v removed=%v", err, removed)
		}

		if _, terr := db.TeamBySlug(t.Context(), team.Slug); terr == nil {
			t.Error("the team is still there")
		}
		users, err := db.ListUsers(t.Context())
		if err != nil {
			t.Fatalf("list users: %v", err)
		}
		var found bool
		for _, u := range users {
			if u.ID != alice.ID {
				continue
			}
			found = true
			if len(u.Teams) != 0 {
				t.Errorf("membership survived the team: %v", u.Teams)
			}
		}
		if !found {
			t.Error("the person went with the team")
		}

		// Deleting again reports nothing removed rather than an error, so a
		// double-click is not a failure.
		removed, err = db.DeleteTeam(t.Context(), team.ID)
		if err != nil || removed {
			t.Errorf("second delete: %v removed=%v", err, removed)
		}
	})
}

func mustUser(t *testing.T, db *DB, email string) *User {
	t.Helper()
	user, err := db.CreateUser(t.Context(), email, email, RoleDeveloper)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return user
}
