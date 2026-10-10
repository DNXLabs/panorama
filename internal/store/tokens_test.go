// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"testing"
	"time"
)

func TestAuthenticateToken(t *testing.T) {
	db := migratedSQLite(t)
	ctx := t.Context()

	user, err := db.CreateUser(ctx, "Alice@Example.com", "Alice", RoleDeveloper)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.Email != "alice@example.com" {
		t.Errorf("email = %q, want it normalised to lower case", user.Email)
	}

	t.Run("a live token resolves to its owner", func(t *testing.T) {
		tok, err := db.CreateGatewayToken(ctx, user.ID, "laptop", "hash-live", "pnt_live", nil)
		if err != nil {
			t.Fatalf("CreateGatewayToken: %v", err)
		}

		p, err := db.AuthenticateToken(ctx, "hash-live")
		if err != nil {
			t.Fatalf("AuthenticateToken: %v", err)
		}
		if p.UserID != user.ID {
			t.Errorf("UserID = %q, want %q", p.UserID, user.ID)
		}
		if p.TokenID != tok.ID {
			t.Errorf("TokenID = %q, want %q", p.TokenID, tok.ID)
		}
		if p.Role != RoleDeveloper {
			t.Errorf("Role = %q, want %q", p.Role, RoleDeveloper)
		}
	})

	t.Run("an unknown token is rejected", func(t *testing.T) {
		_, err := db.AuthenticateToken(ctx, "hash-that-does-not-exist")
		if !errors.Is(err, ErrTokenRejected) {
			t.Errorf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("a revoked token is rejected", func(t *testing.T) {
		tok, err := db.CreateGatewayToken(ctx, user.ID, "revoked", "hash-revoked", "pnt_revk", nil)
		if err != nil {
			t.Fatal(err)
		}
		if rerr := db.RevokeToken(ctx, tok.ID); rerr != nil {
			t.Fatalf("RevokeToken: %v", rerr)
		}

		_, err = db.AuthenticateToken(ctx, "hash-revoked")
		if !errors.Is(err, ErrTokenRejected) {
			t.Errorf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("an expired token is rejected", func(t *testing.T) {
		past := time.Now().Add(-time.Hour)
		if _, err := db.CreateGatewayToken(ctx, user.ID, "expired", "hash-expired", "pnt_expd", &past); err != nil {
			t.Fatal(err)
		}

		_, err := db.AuthenticateToken(ctx, "hash-expired")
		if !errors.Is(err, ErrTokenRejected) {
			t.Errorf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("a token with a future expiry is accepted", func(t *testing.T) {
		future := time.Now().Add(time.Hour)
		if _, err := db.CreateGatewayToken(ctx, user.ID, "future", "hash-future", "pnt_futr", &future); err != nil {
			t.Fatal(err)
		}

		if _, err := db.AuthenticateToken(ctx, "hash-future"); err != nil {
			t.Errorf("AuthenticateToken: %v", err)
		}
	})

	t.Run("a disabled user's token is rejected", func(t *testing.T) {
		disabled, err := db.CreateUser(ctx, "bob@example.com", "Bob", RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		if _, cerr := db.CreateGatewayToken(ctx, disabled.ID, "bob", "hash-disabled", "pnt_disb", nil); cerr != nil {
			t.Fatal(cerr)
		}
		if _, uerr := db.ExecContext(ctx, db.rebind(`UPDATE users SET status = 'disabled' WHERE id = ?`), disabled.ID); uerr != nil {
			t.Fatal(uerr)
		}

		_, err = db.AuthenticateToken(ctx, "hash-disabled")
		if !errors.Is(err, ErrTokenRejected) {
			t.Errorf("err = %v, want ErrTokenRejected - disabling a user must cut off their tokens", err)
		}
	})
}

func TestEnsureUserIsIdempotent(t *testing.T) {
	db := migratedSQLite(t)
	ctx := t.Context()

	first, err := db.EnsureUser(ctx, "carol@example.com", "Carol", RoleAdmin)
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	// Different case, and a role that must not overwrite the existing one.
	second, err := db.EnsureUser(ctx, "CAROL@example.com", "Carol", RoleDeveloper)
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("EnsureUser created a second account for the same address: %q then %q", first.ID, second.ID)
	}
	if second.Role != RoleAdmin {
		t.Errorf("Role = %q, want the existing %q left alone", second.Role, RoleAdmin)
	}
}

func TestCreateUserRejectsAnUnknownRole(t *testing.T) {
	db := migratedSQLite(t)

	if _, err := db.CreateUser(t.Context(), "dan@example.com", "Dan", "superuser"); err == nil {
		t.Fatal("an unrecognised role must be refused")
	}
}

func TestTouchTokenRecordsUse(t *testing.T) {
	db := migratedSQLite(t)
	ctx := t.Context()

	user, err := db.CreateUser(ctx, "erin@example.com", "Erin", RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateGatewayToken(ctx, user.ID, "laptop", "hash-touch", "pnt_tuch", nil)
	if err != nil {
		t.Fatal(err)
	}

	var before any
	if err := db.QueryRowContext(ctx, db.rebind(`SELECT last_used_at FROM gateway_tokens WHERE id = ?`), tok.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		t.Errorf("last_used_at = %v on a fresh token, want null", before)
	}

	if err := db.TouchToken(ctx, tok.ID); err != nil {
		t.Fatalf("TouchToken: %v", err)
	}

	var after any
	if err := db.QueryRowContext(ctx, db.rebind(`SELECT last_used_at FROM gateway_tokens WHERE id = ?`), tok.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == nil {
		t.Error("last_used_at is still null after TouchToken")
	}
}

func TestRebind(t *testing.T) {
	sqlite := &DB{driver: "sqlite"}
	postgres := &DB{driver: "postgres"}

	const query = `SELECT a FROM t WHERE b = ? AND c = ? AND d = 'literal ? inside'`

	if got := sqlite.rebind(query); got != query {
		t.Errorf("sqlite rebind changed the query:\n%s", got)
	}

	want := `SELECT a FROM t WHERE b = $1 AND c = $2 AND d = 'literal ? inside'`
	if got := postgres.rebind(query); got != want {
		t.Errorf("postgres rebind =\n%s\nwant\n%s", got, want)
	}
}

// migratedSQLite returns a migrated, empty SQLite database.
