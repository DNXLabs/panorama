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

// ServiceToken is a credential that belongs to a team rather than to a person.
//
// The case it exists for is continuous integration: a pipeline spends money,
// that money belongs to a team's budget, and it has to keep working after the
// engineer who set it up leaves. A personal token satisfies none of those.
type ServiceToken struct {
	ID         string
	TeamID     string
	TeamName   string
	TeamSlug   string
	CreatedBy  string
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// Revoked reports whether the token has been withdrawn.
func (t *ServiceToken) Revoked() bool { return t != nil && t.RevokedAt != nil }

// CreateServiceToken records a newly minted token for a team.
//
// createdBy is provenance rather than ownership: it answers "who set this up"
// and is allowed to become null when that person's account goes away, because
// the pipeline outlives them.
func (db *DB) CreateServiceToken(ctx context.Context, teamID, createdBy, name, hash, prefix string, expiresAt *time.Time) (*ServiceToken, error) {
	t := &ServiceToken{
		ID:        NewID("svt"),
		TeamID:    teamID,
		CreatedBy: createdBy,
		Name:      name,
		Prefix:    prefix,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}

	var creator any
	if createdBy != "" {
		creator = createdBy
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO service_tokens (id, team_id, created_by, name, prefix, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		t.ID, t.TeamID, creator, t.Name, t.Prefix, hash, t.CreatedAt, t.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("store: create service token: %w", err)
	}
	return t, nil
}

// AuthenticateServiceToken resolves a service token hash to its principal.
//
// One indexed lookup in one table, chosen by the token's prefix before this was
// called. Every rejection returns ErrTokenRejected, the same singular error a
// personal token gets, so nothing about which table was consulted or why it
// refused reaches the caller.
//
// There is no user to check the status of, which is the point. What is checked
// instead is that the team still exists - and that is enforced by the foreign
// key rather than here, because a cascade has already removed the row.
func (db *DB) AuthenticateServiceToken(ctx context.Context, hash string) (*Principal, error) {
	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT s.id, s.team_id, s.name, s.expires_at, s.revoked_at, t.name
		FROM service_tokens s
		JOIN teams t ON t.id = s.team_id
		WHERE s.token_hash = ?`), hash)

	var (
		p         Principal
		name      string
		teamName  string
		expiresAt sql.NullTime
		revokedAt sql.NullTime
	)

	err := row.Scan(&p.TokenID, &p.TeamID, &name, &expiresAt, &revokedAt, &teamName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenRejected
	}
	if err != nil {
		return nil, fmt.Errorf("store: authenticate service token: %w", err)
	}

	if revokedAt.Valid {
		return nil, ErrTokenRejected
	}
	if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		return nil, ErrTokenRejected
	}

	p.Kind = PrincipalKindService
	// RoleService is not on the admin/manager/developer ladder and cannot be
	// written to a person. It is what refuses this principal at every console
	// endpoint: the control plane admits it nowhere, so a handler added later
	// is closed rather than quietly reachable.
	p.Role = RoleService
	// Shown wherever a person's name would be, so an audit row for CI traffic
	// reads as the thing it is rather than as a blank.
	p.DisplayName = teamName + " · " + name

	return &p, nil
}

// TouchServiceToken records that a service token was used. Off the request
// path, exactly like TouchToken.
func (db *DB) TouchServiceToken(ctx context.Context, tokenID string) error {
	_, err := db.ExecContext(ctx, db.rebind(
		`UPDATE service_tokens SET last_used_at = ? WHERE id = ?`),
		time.Now().UTC(), tokenID)
	if err != nil {
		return fmt.Errorf("store: touch service token: %w", err)
	}
	return nil
}

// RevokeServiceToken marks a service token unusable. Permanent, like every
// other revocation here.
func (db *DB) RevokeServiceToken(ctx context.Context, tokenID string) error {
	_, err := db.ExecContext(ctx, db.rebind(
		`UPDATE service_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`),
		time.Now().UTC(), tokenID)
	if err != nil {
		return fmt.Errorf("store: revoke service token: %w", err)
	}
	return nil
}

// ListServiceTokens returns a team's tokens, or every team's when teamID is
// empty. Metadata only: the credential is not recoverable from this.
func (db *DB) ListServiceTokens(ctx context.Context, teamID string) ([]ServiceToken, error) {
	query := `
		SELECT s.id, s.team_id, t.name, t.slug, s.name, s.prefix,
		       s.created_at, s.last_used_at, s.expires_at, s.revoked_at
		FROM service_tokens s
		JOIN teams t ON t.id = s.team_id`
	args := []any{}
	if teamID != "" {
		query += ` WHERE s.team_id = ?`
		args = append(args, teamID)
	}
	query += ` ORDER BY s.created_at DESC`

	rows, err := db.QueryContext(ctx, db.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("store: list service tokens: %w", err)
	}
	defer rows.Close()

	var out []ServiceToken
	for rows.Next() {
		var (
			t          ServiceToken
			lastUsedAt sql.NullTime
			expiresAt  sql.NullTime
			revokedAt  sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.TeamID, &t.TeamName, &t.TeamSlug, &t.Name, &t.Prefix,
			&t.CreatedAt, &lastUsedAt, &expiresAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("store: list service tokens: %w", err)
		}
		if lastUsedAt.Valid {
			t.LastUsedAt = &lastUsedAt.Time
		}
		if expiresAt.Valid {
			t.ExpiresAt = &expiresAt.Time
		}
		if revokedAt.Valid {
			t.RevokedAt = &revokedAt.Time
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list service tokens: %w", err)
	}
	return out, nil
}

// ServiceTokenTeam returns which team a service token bills, for an
// authorization check before revocation.
func (db *DB) ServiceTokenTeam(ctx context.Context, tokenID string) (string, error) {
	var teamID string
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT team_id FROM service_tokens WHERE id = ?`), tokenID).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: service token team: %w", err)
	}
	return teamID, nil
}
