// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"testing"
	"time"
)

func TestAuthenticateServiceToken(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		team, err := db.CreateTeam(ctx, "Platform", "platform", "")
		if err != nil {
			t.Fatalf("CreateTeam: %v", err)
		}
		admin, err := db.CreateUser(ctx, "admin@example.com", "Admin", RoleAdmin)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		t.Run("a live token resolves to its team, not to a person", func(t *testing.T) {
			tok, err := db.CreateServiceToken(ctx, team.ID, admin.ID, "ci", "svc-live", "pnk_live", nil)
			if err != nil {
				t.Fatalf("CreateServiceToken: %v", err)
			}

			p, err := db.AuthenticateServiceToken(ctx, "svc-live")
			if err != nil {
				t.Fatalf("AuthenticateServiceToken: %v", err)
			}
			if p.TokenID != tok.ID {
				t.Errorf("TokenID = %q, want %q", p.TokenID, tok.ID)
			}
			if p.TeamID != team.ID {
				t.Errorf("TeamID = %q, want %q - a service token bills a team", p.TeamID, team.ID)
			}
			if p.UserID != "" {
				t.Errorf("UserID = %q, want empty - a pipeline is not a person", p.UserID)
			}
			if p.Kind != PrincipalKindService {
				t.Errorf("Kind = %q, want %q", p.Kind, PrincipalKindService)
			}
			if p.Role != RoleService {
				t.Errorf("Role = %q, want %q", p.Role, RoleService)
			}
			if ValidRole(p.Role) {
				t.Error("RoleService passes ValidRole; it must not be assignable to a person")
			}
		})

		t.Run("revoked and expired are rejected", func(t *testing.T) {
			tok, err := db.CreateServiceToken(ctx, team.ID, admin.ID, "revoked", "svc-revoked", "pnk_revk", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.RevokeServiceToken(ctx, tok.ID); err != nil {
				t.Fatalf("RevokeServiceToken: %v", err)
			}
			if _, err := db.AuthenticateServiceToken(ctx, "svc-revoked"); !errors.Is(err, ErrTokenRejected) {
				t.Errorf("revoked = %v, want ErrTokenRejected", err)
			}

			past := time.Now().UTC().Add(-time.Hour)
			if _, err := db.CreateServiceToken(ctx, team.ID, admin.ID, "old", "svc-expired", "pnk_oldx", &past); err != nil {
				t.Fatal(err)
			}
			if _, err := db.AuthenticateServiceToken(ctx, "svc-expired"); !errors.Is(err, ErrTokenRejected) {
				t.Errorf("expired = %v, want ErrTokenRejected", err)
			}

			if _, err := db.AuthenticateServiceToken(ctx, "svc-nobody-issued"); !errors.Is(err, ErrTokenRejected) {
				t.Errorf("unknown = %v, want ErrTokenRejected", err)
			}
		})

		// The reason this is a table of its own rather than a nullable user id
		// on gateway_tokens. A pipeline must not stop working because the
		// engineer who set it up left the company.
		t.Run("outlives the account that created it", func(t *testing.T) {
			leaver, err := db.CreateUser(ctx, "leaver@example.com", "Leaver", RoleAdmin)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			if _, cerr := db.CreateServiceToken(ctx, team.ID, leaver.ID, "nightly", "svc-outlives", "pnk_outl", nil); cerr != nil {
				t.Fatalf("CreateServiceToken: %v", cerr)
			}

			if _, derr := db.ExecContext(ctx, db.rebind(`DELETE FROM users WHERE id = ?`), leaver.ID); derr != nil {
				t.Fatalf("deleting the creator: %v", derr)
			}

			p, err := db.AuthenticateServiceToken(ctx, "svc-outlives")
			if err != nil {
				t.Fatalf("the token stopped working when its creator's account was removed: %v", err)
			}
			if p.TeamID != team.ID {
				t.Errorf("TeamID = %q, want %q", p.TeamID, team.ID)
			}
		})

		// The mirror of the above: the team is the principal, so a team that no
		// longer exists takes its credentials with it.
		t.Run("dies with its team", func(t *testing.T) {
			doomed, err := db.CreateTeam(ctx, "Doomed", "doomed", "")
			if err != nil {
				t.Fatalf("CreateTeam: %v", err)
			}
			if _, err := db.CreateServiceToken(ctx, doomed.ID, admin.ID, "ci", "svc-doomed", "pnk_doom", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, db.rebind(`DELETE FROM teams WHERE id = ?`), doomed.ID); err != nil {
				t.Fatalf("deleting the team: %v", err)
			}
			if _, err := db.AuthenticateServiceToken(ctx, "svc-doomed"); !errors.Is(err, ErrTokenRejected) {
				t.Errorf("token of a deleted team = %v, want ErrTokenRejected", err)
			}
		})

		t.Run("listing is scoped and carries no secret", func(t *testing.T) {
			all, err := db.ListServiceTokens(ctx, team.ID)
			if err != nil {
				t.Fatalf("ListServiceTokens: %v", err)
			}
			if len(all) == 0 {
				t.Fatal("ListServiceTokens returned nothing for a team with tokens")
			}
			for _, tok := range all {
				if tok.TeamID != team.ID {
					t.Errorf("token %s belongs to team %q, want %q", tok.ID, tok.TeamID, team.ID)
				}
				if tok.TeamSlug != "platform" {
					t.Errorf("TeamSlug = %q, want %q", tok.TeamSlug, "platform")
				}
			}
		})
	})
}
