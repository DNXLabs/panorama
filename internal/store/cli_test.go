// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"testing"
	"time"
)

func TestDeviceAuthorizationFlow(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		user, err := db.CreateUser(ctx, "dev@example.com", "Dev", RoleDeveloper)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		t.Run("approved once, claimable once", func(t *testing.T) {
			a, err := db.CreateDeviceAuthorization(ctx,
				"hash-approve", "AAAA-BBBB", "laptop", "10.0.0.1",
				time.Now().UTC().Add(10*time.Minute), 0)
			if err != nil {
				t.Fatalf("CreateDeviceAuthorization: %v", err)
			}

			if _, cerr := db.ClaimDeviceAuthorization(ctx, "hash-approve"); !errors.Is(cerr, ErrAuthorizationPending) {
				t.Fatalf("claim before decision = %v, want ErrAuthorizationPending", cerr)
			}

			found, err := db.DeviceAuthorizationByUserCode(ctx, "AAAA-BBBB")
			if err != nil {
				t.Fatalf("DeviceAuthorizationByUserCode: %v", err)
			}
			if found.ID != a.ID || found.ClientName != "laptop" {
				t.Errorf("looked up %+v, want the flow just created", found)
			}

			if derr := db.DecideDeviceAuthorization(ctx, "AAAA-BBBB", user.ID, true); derr != nil {
				t.Fatalf("DecideDeviceAuthorization: %v", derr)
			}

			claim, err := db.ClaimDeviceAuthorization(ctx, "hash-approve")
			if err != nil {
				t.Fatalf("ClaimDeviceAuthorization: %v", err)
			}
			if claim.UserID != user.ID {
				t.Errorf("claim returned %q, want %q", claim.UserID, user.ID)
			}
			if claim.ClientName != "laptop" {
				t.Errorf("claim client name = %q, want %q - the grant is labelled from it",
					claim.ClientName, "laptop")
			}

			// The whole reason claiming is a conditional update: a replayed
			// device code must not produce a second grant.
			if _, err := db.ClaimDeviceAuthorization(ctx, "hash-approve"); !errors.Is(err, ErrTokenRejected) {
				t.Errorf("second claim = %v, want ErrTokenRejected - a device code is single-use", err)
			}

			// And an approved flow must no longer be reachable from the
			// approval screen, or a second tab could approve it again.
			if _, err := db.DeviceAuthorizationByUserCode(ctx, "AAAA-BBBB"); !errors.Is(err, ErrNotFound) {
				t.Errorf("lookup after approval = %v, want ErrNotFound", err)
			}
		})

		t.Run("denied", func(t *testing.T) {
			if _, err := db.CreateDeviceAuthorization(ctx,
				"hash-deny", "CCCC-DDDD", "laptop", "", time.Now().UTC().Add(10*time.Minute), 0); err != nil {
				t.Fatal(err)
			}
			if err := db.DecideDeviceAuthorization(ctx, "CCCC-DDDD", "", false); err != nil {
				t.Fatalf("DecideDeviceAuthorization: %v", err)
			}
			if _, err := db.ClaimDeviceAuthorization(ctx, "hash-deny"); !errors.Is(err, ErrAuthorizationDenied) {
				t.Errorf("claim after denial = %v, want ErrAuthorizationDenied", err)
			}
		})

		t.Run("expired", func(t *testing.T) {
			if _, err := db.CreateDeviceAuthorization(ctx,
				"hash-expired", "EEEE-FFFF", "laptop", "", time.Now().UTC().Add(-time.Minute), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ClaimDeviceAuthorization(ctx, "hash-expired"); !errors.Is(err, ErrAuthorizationExpired) {
				t.Errorf("claim after expiry = %v, want ErrAuthorizationExpired", err)
			}
			if err := db.DecideDeviceAuthorization(ctx, "EEEE-FFFF", user.ID, true); !errors.Is(err, ErrNotFound) {
				t.Errorf("approving an expired flow = %v, want ErrNotFound", err)
			}
		})

		t.Run("polling faster than the interval is told to slow down", func(t *testing.T) {
			if _, err := db.CreateDeviceAuthorization(ctx,
				"hash-fast", "GGGG-HHHH", "laptop", "", time.Now().UTC().Add(10*time.Minute), 60); err != nil {
				t.Fatal(err)
			}
			// The first poll has nothing to compare against and reports the
			// real state.
			if _, err := db.ClaimDeviceAuthorization(ctx, "hash-fast"); !errors.Is(err, ErrAuthorizationPending) {
				t.Fatalf("first poll = %v, want ErrAuthorizationPending", err)
			}
			if _, err := db.ClaimDeviceAuthorization(ctx, "hash-fast"); !errors.Is(err, ErrSlowDown) {
				t.Errorf("immediate second poll = %v, want ErrSlowDown", err)
			}
		})

		t.Run("an unknown device code is rejected", func(t *testing.T) {
			if _, err := db.ClaimDeviceAuthorization(ctx, "hash-nobody-issued"); !errors.Is(err, ErrTokenRejected) {
				t.Errorf("err = %v, want ErrTokenRejected", err)
			}
		})
	})
}

func TestGrantExchangeRetiresThePreviousToken(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		user, err := db.CreateUser(ctx, "rotate@example.com", "Rotate", RoleDeveloper)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		grant, err := db.CreateGrant(ctx, user.ID, "laptop", "grant-hash", "png_aaa", nil)
		if err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}

		first, err := db.ExchangeGrant(ctx, grant.ID, user.ID, "laptop", "tok-1", "pnt_one", nil)
		if err != nil {
			t.Fatalf("ExchangeGrant: %v", err)
		}
		if _, aerr := db.AuthenticateToken(ctx, "tok-1"); aerr != nil {
			t.Fatalf("the freshly minted token must work: %v", aerr)
		}

		second, err := db.ExchangeGrant(ctx, grant.ID, user.ID, "laptop", "tok-2", "pnt_two", nil)
		if err != nil {
			t.Fatalf("ExchangeGrant: %v", err)
		}
		if second.ID == first.ID {
			t.Fatal("exchange returned the same token row twice")
		}

		if _, err := db.AuthenticateToken(ctx, "tok-1"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("previous token = %v, want ErrTokenRejected - a refresh retires the one it replaces", err)
		}
		if _, err := db.AuthenticateToken(ctx, "tok-2"); err != nil {
			t.Errorf("current token must work: %v", err)
		}
	})
}

// TestRevokeGrantKillsItsTokens is the assertion the whole two-credential shape
// exists for. Revoking the grant alone would leave the laptop's current token
// buying model calls until it expired on its own.
func TestRevokeGrantKillsItsTokens(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		user, err := db.CreateUser(ctx, "lost@example.com", "Lost Laptop", RoleDeveloper)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		grant, err := db.CreateGrant(ctx, user.ID, "laptop", "grant-live", "png_bbb", nil)
		if err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}
		if _, err := db.ExchangeGrant(ctx, grant.ID, user.ID, "laptop", "tok-live", "pnt_live", nil); err != nil {
			t.Fatalf("ExchangeGrant: %v", err)
		}

		if _, _, err := db.AuthenticateGrant(ctx, "grant-live"); err != nil {
			t.Fatalf("AuthenticateGrant: %v", err)
		}

		if err := db.RevokeGrant(ctx, grant.ID); err != nil {
			t.Fatalf("RevokeGrant: %v", err)
		}

		if _, _, err := db.AuthenticateGrant(ctx, "grant-live"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("grant after revocation = %v, want ErrTokenRejected", err)
		}
		if _, err := db.AuthenticateToken(ctx, "tok-live"); !errors.Is(err, ErrTokenRejected) {
			t.Error("the token the revoked grant minted still works; signing a lost laptop " +
				"out has to stop it now, not when the token happens to expire")
		}
	})
}

func TestAuthenticateGrantRejectsWhatItShould(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *DB) {
		ctx := t.Context()

		user, err := db.CreateUser(ctx, "grants@example.com", "Grants", RoleDeveloper)
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		past := time.Now().UTC().Add(-time.Hour)
		if _, err := db.CreateGrant(ctx, user.ID, "old", "grant-expired", "png_ccc", &past); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.AuthenticateGrant(ctx, "grant-expired"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("expired grant = %v, want ErrTokenRejected", err)
		}

		if _, _, err := db.AuthenticateGrant(ctx, "grant-nobody-issued"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("unknown grant = %v, want ErrTokenRejected", err)
		}

		if _, err := db.CreateGrant(ctx, user.ID, "current", "grant-ok", "png_ddd", nil); err != nil {
			t.Fatal(err)
		}
		if err := db.SetUserStatus(ctx, user.ID, "disabled"); err != nil {
			t.Fatalf("SetUserStatus: %v", err)
		}
		if _, _, err := db.AuthenticateGrant(ctx, "grant-ok"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("grant of a disabled account = %v, want ErrTokenRejected", err)
		}
	})
}
