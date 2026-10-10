// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetupSession is a redeemed setup code, live.
type SetupSession struct {
	ID        string
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ErrSetupRejected is returned when a setup cookie does not identify a
// live session. Singular, like ErrTokenRejected: unknown, expired, and revoked
// are the same answer, because the difference is useful only to someone
// guessing.
var ErrSetupRejected = errors.New("store: setup session rejected")

// CreateSetupSession opens a session for a redeemed code.
func (db *DB) CreateSetupSession(ctx context.Context, tokenHash, csrf, sourceIP string, expiresAt time.Time) (*SetupSession, error) {
	s := &SetupSession{
		ID:        NewID("stp"),
		CSRFToken: csrf,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt.UTC(),
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO setup_sessions (id, token_hash, csrf_token, created_at, expires_at, source_ip)
		VALUES (?, ?, ?, ?, ?, ?)`),
		s.ID, tokenHash, csrf, s.CreatedAt, s.ExpiresAt, sourceIP)
	if err != nil {
		return nil, fmt.Errorf("store: create setup session: %w", err)
	}
	return s, nil
}

// AuthenticateSetupSession resolves a setup cookie.
//
// The principal it returns carries no user id and no email, because there is no
// user and no email. What it carries is the kind, which is what the
// authorization layer keys on.
func (db *DB) AuthenticateSetupSession(ctx context.Context, tokenHash string) (*Principal, *SetupSession, error) {
	var (
		s         SetupSession
		expiresAt time.Time
		revoked   sql.NullTime
	)

	err := db.QueryRowContext(ctx, db.rebind(`
		SELECT id, csrf_token, created_at, expires_at, revoked_at
		FROM setup_sessions WHERE token_hash = ?`), tokenHash).
		Scan(&s.ID, &s.CSRFToken, &s.CreatedAt, &expiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrSetupRejected
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: read setup session: %w", err)
	}

	if revoked.Valid || !expiresAt.After(time.Now().UTC()) {
		return nil, nil, ErrSetupRejected
	}
	s.ExpiresAt = expiresAt

	return &Principal{
		Kind: PrincipalKindSetup,
		Role: RoleSetup,
	}, &s, nil
}

// EndSetupSessions revokes every live setup session.
//
// Called when one is redeemed, so a code minted and forgotten cannot be sitting
// alongside the one in use, and called on sign-out. A setup session is the
// most privileged thing that can happen to this deployment's identity
// configuration; exactly one should ever be live.
func (db *DB) EndSetupSessions(ctx context.Context) (int64, error) {
	res, err := db.ExecContext(ctx, db.rebind(
		`UPDATE setup_sessions SET revoked_at = ? WHERE revoked_at IS NULL`), time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("store: end setup sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SweepSetupSessions deletes expired rows, so the table stays approximately
// empty between the rare occasions anything is in it.
func (db *DB) SweepSetupSessions(ctx context.Context) (int64, error) {
	res, err := db.ExecContext(ctx, db.rebind(
		`DELETE FROM setup_sessions WHERE expires_at < ?`), time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("store: sweep setup sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
