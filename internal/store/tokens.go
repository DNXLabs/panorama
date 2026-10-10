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

// GatewayToken is the stored record of a credential. The credential itself is
// not here - only its hash.
type GatewayToken struct {
	ID        string
	UserID    string
	Name      string
	Prefix    string
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// PrincipalKind distinguishes who a request is acting as.
//
// Most principals are a person the identity provider owns. Two are not, and
// neither has an account: a redeemed setup code, which configures a deployment
// nobody can yet sign in to, and a service token, which is a team's credential
// held by a pipeline.
//
// Callers that care must compare against these rather than testing for the
// presence of a user id. An account pre-created before its owner's first
// sign-in has no external id either, and conflating "no identity yet" with "not
// a person" would be a way in.
const (
	PrincipalKindUser    = "user"
	PrincipalKindSetup   = "setup"
	PrincipalKindService = "service"
)

// Principal is who a request is acting as, resolved from a presented token.
type Principal struct {
	Kind        string
	UserID      string
	Email       string
	DisplayName string
	Role        string
	TokenID     string
	// TeamID is set only for a service principal, and is what its spend bills
	// to. A person's team is not here on purpose: people can be on several,
	// membership is resolved where it is needed, and copying one team onto the
	// principal would quietly pick a winner.
	TeamID string
}

// IsSetup reports whether this principal is a redeemed setup code rather than a
// person.
func (p *Principal) IsSetup() bool {
	return p != nil && p.Kind == PrincipalKindSetup
}

// IsService reports whether this principal is a team's service token rather
// than a person.
func (p *Principal) IsService() bool {
	return p != nil && p.Kind == PrincipalKindService
}

// ErrTokenRejected is returned when a token does not identify an active user.
// It is deliberately singular: the caller must not be able to tell an unknown
// token from a revoked one, an expired one, or one belonging to a disabled
// user, because that distinction is useful only to someone guessing.
var ErrTokenRejected = errors.New("store: token rejected")

// CreateGatewayToken records a newly minted token for a user.
func (db *DB) CreateGatewayToken(ctx context.Context, userID, name, hash, prefix string, expiresAt *time.Time) (*GatewayToken, error) {
	t := &GatewayToken{
		ID:        NewID("gwt"),
		UserID:    userID,
		Name:      name,
		Prefix:    prefix,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO gateway_tokens (id, user_id, name, prefix, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`),
		t.ID, t.UserID, t.Name, t.Prefix, hash, t.CreatedAt, t.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("store: create gateway token: %w", err)
	}
	return t, nil
}

// AuthenticateToken resolves a token hash to the principal it acts as.
//
// Every rejection returns ErrTokenRejected regardless of cause. The reason is
// logged server-side, never returned.
func (db *DB) AuthenticateToken(ctx context.Context, hash string) (*Principal, error) {
	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT t.id, t.expires_at, t.revoked_at,
		       u.id, u.email, u.display_name, u.role, u.status
		FROM gateway_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?`), hash)

	var (
		p          Principal
		expiresAt  sql.NullTime
		revokedAt  sql.NullTime
		userStatus string
	)

	err := row.Scan(&p.TokenID, &expiresAt, &revokedAt,
		&p.UserID, &p.Email, &p.DisplayName, &p.Role, &userStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenRejected
	}
	if err != nil {
		return nil, fmt.Errorf("store: authenticate token: %w", err)
	}

	if revokedAt.Valid {
		return nil, ErrTokenRejected
	}
	if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		return nil, ErrTokenRejected
	}
	if userStatus != "active" {
		return nil, ErrTokenRejected
	}

	return &p, nil
}

// TouchToken records that a token was used. It is called off the request path:
// a write here must never delay a response, and losing one on shutdown costs
// nothing but a slightly stale timestamp.
func (db *DB) TouchToken(ctx context.Context, tokenID string) error {
	_, err := db.ExecContext(ctx, db.rebind(
		`UPDATE gateway_tokens SET last_used_at = ? WHERE id = ?`),
		time.Now().UTC(), tokenID)
	if err != nil {
		return fmt.Errorf("store: touch token: %w", err)
	}
	return nil
}

// RevokeToken marks a token unusable. Revocation is permanent; mint a new
// token rather than un-revoking one that may have leaked.
func (db *DB) RevokeToken(ctx context.Context, tokenID string) error {
	_, err := db.ExecContext(ctx, db.rebind(
		`UPDATE gateway_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`),
		time.Now().UTC(), tokenID)
	if err != nil {
		return fmt.Errorf("store: revoke token: %w", err)
	}
	return nil
}
