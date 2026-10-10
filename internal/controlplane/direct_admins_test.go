// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"testing"

	"github.com/dnxlabs/panorama/internal/store"
)

// TestDirectAdministrators covers the two rules that let the group mapping do
// its job without a deployment locking itself out.
//
// Both are about a failure that arrives late. A role pinned by hand is a role
// the provider can no longer revoke, and a hand-back is a demotion that does
// not happen until the person next signs in - so neither mistake is visible at
// the moment it is made, and the guards have to be on the write.
func TestDirectAdministrators(t *testing.T) {
	t.Run("refuses every role but administrator", func(t *testing.T) {
		f := newConsole(t)

		rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+f.developer.ID, f.adminToken,
			map[string]any{"role": store.RoleManager})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}

		user, err := f.db.UserByID(t.Context(), f.developer.ID)
		if err != nil {
			t.Fatalf("reading the account: %v", err)
		}
		if user.Role != store.RoleDeveloper {
			t.Errorf("role = %q, want %q - a refused write must change nothing", user.Role, store.RoleDeveloper)
		}
		if user.RoleSource != store.RoleSourceSSO {
			t.Errorf("role source = %q, want %q", user.RoleSource, store.RoleSourceSSO)
		}
	})

	// A handler that acted only when the role *changed* would make pinning
	// somebody the mapping had already made an administrator do nothing at all -
	// silently, and for the most common entry on the list.
	t.Run("assigning administrator pins a role the mapping already granted", func(t *testing.T) {
		f := newConsole(t)

		before, err := f.db.UserByID(t.Context(), f.admin.ID)
		if err != nil {
			t.Fatalf("reading the account: %v", err)
		}
		if before.Role != store.RoleAdmin || before.RoleSource != store.RoleSourceSSO {
			t.Fatalf("fixture should start as an unpinned admin, got %q/%q", before.Role, before.RoleSource)
		}

		rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
			map[string]any{"role": store.RoleAdmin})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}

		after, err := f.db.UserByID(t.Context(), f.admin.ID)
		if err != nil {
			t.Fatalf("reading the account back: %v", err)
		}
		if after.RoleSource != store.RoleSourceManual {
			t.Errorf("role source = %q, want %q - assigning a role is what pins it",
				after.RoleSource, store.RoleSourceManual)
		}
	})

	t.Run("refuses handing back the last administrator no group can grant", func(t *testing.T) {
		f := newConsole(t)

		pin(t, f)

		rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
			map[string]any{"role_source": store.RoleSourceSSO})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
		}

		user, err := f.db.UserByID(t.Context(), f.admin.ID)
		if err != nil {
			t.Fatalf("reading the account: %v", err)
		}
		if user.RoleSource != store.RoleSourceManual {
			t.Errorf("role source = %q, want %q - the refusal must leave the pin in place",
				user.RoleSource, store.RoleSourceManual)
		}
	})

	t.Run("allows the hand-back once a group is mapped to administrator", func(t *testing.T) {
		f := newConsole(t)

		pin(t, f)
		if err := f.db.SetGroupRole(t.Context(), "platform-leads", store.RoleAdmin); err != nil {
			t.Fatalf("mapping a group: %v", err)
		}

		rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
			map[string]any{"role_source": store.RoleSourceSSO})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}

		user, err := f.db.UserByID(t.Context(), f.admin.ID)
		if err != nil {
			t.Fatalf("reading the account: %v", err)
		}
		if user.RoleSource != store.RoleSourceSSO {
			t.Errorf("role source = %q, want %q", user.RoleSource, store.RoleSourceSSO)
		}
		// The role itself does not move here, and that is the contract: group
		// membership is the provider's record and the next sign-in is what
		// applies it.
		if user.Role != store.RoleAdmin {
			t.Errorf("role = %q, want %q - a hand-back is not a demotion", user.Role, store.RoleAdmin)
		}
	})

	t.Run("allows the hand-back while another pinned administrator remains", func(t *testing.T) {
		f := newConsole(t)

		other, _ := f.newUser(t, "second@example.com", store.RoleAdmin)
		pin(t, f)
		if rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+other.ID, f.adminToken,
			map[string]any{"role": store.RoleAdmin}); rec.Code != http.StatusOK {
			t.Fatalf("pinning the second administrator: %d: %s", rec.Code, rec.Body)
		}

		rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
			map[string]any{"role_source": store.RoleSourceSSO})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	})
}

// pin makes the fixture's administrator a direct one.
func pin(t *testing.T, f *consoleFixture) {
	t.Helper()

	rec, _ := f.send(t, http.MethodPatch, "/api/v1/users/"+f.admin.ID, f.adminToken,
		map[string]any{"role": store.RoleAdmin})
	if rec.Code != http.StatusOK {
		t.Fatalf("pinning the administrator: %d: %s", rec.Code, rec.Body)
	}
}
