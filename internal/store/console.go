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

// ConsoleSession is a signed-in browser.
type ConsoleSession struct {
	ID     string
	UserID string
	// CSRFToken is the value the page must echo back in a header on every
	// state-changing request. It is readable by the page and useless to anyone
	// who cannot read it.
	CSRFToken string

	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time

	UserAgent string
	SourceIP  string
}

// Active reports whether this session may still be used.
func (s ConsoleSession) Active(now time.Time) bool {
	return s.RevokedAt == nil && s.ExpiresAt.After(now)
}

// CreateConsoleSession records a signed-in browser. Only the hash of the cookie
// value is stored, exactly as with a gateway token.
func (db *DB) CreateConsoleSession(ctx context.Context, userID, tokenHash, csrf, userAgent, sourceIP string, expiresAt time.Time) (*ConsoleSession, error) {
	now := time.Now().UTC()

	s := &ConsoleSession{
		ID:         NewID("cse"),
		UserID:     userID,
		CSRFToken:  csrf,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  expiresAt.UTC(),
		UserAgent:  truncate(userAgent, 256),
		SourceIP:   truncate(sourceIP, 64),
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO console_sessions (
			id, token_hash, user_id, csrf_token,
			created_at, last_seen_at, expires_at, user_agent, source_ip
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		s.ID, tokenHash, s.UserID, s.CSRFToken,
		s.CreatedAt, s.LastSeenAt, s.ExpiresAt, s.UserAgent, s.SourceIP)
	if err != nil {
		return nil, fmt.Errorf("store: create console session: %w", err)
	}
	return s, nil
}

// ErrSessionRejected is returned when a session cookie does not identify an
// active user.
//
// Singular, for the same reason ErrTokenRejected is: the difference between an
// unknown session, an expired one, a revoked one, and one belonging to a
// disabled account is useful only to someone guessing.
var ErrSessionRejected = errors.New("store: console session rejected")

// AuthenticateConsoleSession resolves a session cookie's hash to the principal
// it acts as, and to the session itself so CSRF can be checked against it.
func (db *DB) AuthenticateConsoleSession(ctx context.Context, tokenHash string) (*Principal, *ConsoleSession, error) {
	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT c.id, c.user_id, c.csrf_token, c.created_at, c.last_seen_at,
		       c.expires_at, c.revoked_at, c.user_agent, c.source_ip,
		       u.email, u.display_name, u.role, u.status
		FROM console_sessions c
		JOIN users u ON u.id = c.user_id
		WHERE c.token_hash = ?`), tokenHash)

	var (
		s          ConsoleSession
		p          Principal
		userStatus string
	)
	err := row.Scan(&s.ID, &s.UserID, &s.CSRFToken, &s.CreatedAt, &s.LastSeenAt,
		&s.ExpiresAt, &s.RevokedAt, &s.UserAgent, &s.SourceIP,
		&p.Email, &p.DisplayName, &p.Role, &userStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrSessionRejected
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: authenticate console session: %w", err)
	}

	if !s.Active(time.Now().UTC()) || userStatus != "active" {
		return nil, nil, ErrSessionRejected
	}

	p.UserID = s.UserID
	return &p, &s, nil
}

// TouchConsoleSession records that a session was used, and extends it.
//
// Sliding expiry rather than a fixed one: a console left open through a working
// day should not sign itself out mid-task, and a browser nobody has touched for
// a fortnight should not still be able to act.
func (db *DB) TouchConsoleSession(ctx context.Context, id string, expiresAt time.Time) error {
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx, db.rebind(
		`UPDATE console_sessions SET last_seen_at = ?, expires_at = ? WHERE id = ? AND revoked_at IS NULL`),
		now, expiresAt.UTC(), id)
	if err != nil {
		return fmt.Errorf("store: touch console session: %w", err)
	}
	return nil
}

// RevokeConsoleSession signs one browser out.
func (db *DB) RevokeConsoleSession(ctx context.Context, id string) error {
	_, err := db.ExecContext(ctx, db.rebind(
		`UPDATE console_sessions SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`),
		time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("store: revoke console session: %w", err)
	}
	return nil
}

// RevokeUserConsoleSessions signs a user out everywhere.
//
// Called when an account is disabled or its role changes. A role carried in a
// live session would otherwise outlive the change that revoked it, which is the
// difference between an admin who has been demoted and an admin who has not.
func (db *DB) RevokeUserConsoleSessions(ctx context.Context, userID string) error {
	_, err := db.ExecContext(ctx, db.rebind(
		`UPDATE console_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`),
		time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("store: revoke console sessions: %w", err)
	}
	return nil
}

// ListConsoleSessions reads a user's live sessions, newest first.
func (db *DB) ListConsoleSessions(ctx context.Context, userID string) ([]ConsoleSession, error) {
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT id, user_id, csrf_token, created_at, last_seen_at, expires_at,
		       revoked_at, user_agent, source_ip
		FROM console_sessions
		WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ?
		ORDER BY last_seen_at DESC`), userID, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("store: list console sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ConsoleSession
	for rows.Next() {
		var s ConsoleSession
		if serr := rows.Scan(&s.ID, &s.UserID, &s.CSRFToken, &s.CreatedAt, &s.LastSeenAt,
			&s.ExpiresAt, &s.RevokedAt, &s.UserAgent, &s.SourceIP); serr != nil {
			return nil, fmt.Errorf("store: scan console session: %w", serr)
		}
		out = append(out, s)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read console sessions: %w", rerr)
	}
	return out, nil
}

// PurgeExpiredSessions removes sessions and in-flight sign-ins that are past
// their expiry.
//
// Neither table is authoritative for anything once a row is stale, so this is
// housekeeping rather than security: expiry is enforced on read, and a row left
// behind by a crash cannot be used.
func (db *DB) PurgeExpiredSessions(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for _, table := range []string{"console_sessions", "oidc_flows"} {
		res, err := db.ExecContext(ctx, db.rebind(
			`DELETE FROM `+table+` WHERE expires_at < ?`), before.UTC())
		if err != nil {
			return total, fmt.Errorf("store: purge %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Setting reads one deployment setting, or ErrNotFound.
func (db *DB) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT value FROM settings WHERE key = ?`), key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: setting %s: %w", key, err)
	}
	return value, nil
}

// SetSetting writes one deployment setting.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`),
		key, value, now)
	if err != nil {
		return fmt.Errorf("store: set setting %s: %w", key, err)
	}
	return nil
}

// DeleteSetting removes a setting, and reports whether there was one.
func (db *DB) DeleteSetting(ctx context.Context, key string) (bool, error) {
	res, err := db.ExecContext(ctx, db.rebind(`DELETE FROM settings WHERE key = ?`), key)
	if err != nil {
		return false, fmt.Errorf("store: delete setting %s: %w", key, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Settings reads every deployment setting.
func (db *DB) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("store: settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if serr := rows.Scan(&k, &v); serr != nil {
			return nil, fmt.Errorf("store: scan setting: %w", serr)
		}
		out[k] = v
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read settings: %w", rerr)
	}
	return out, nil
}

// ClaimSetting atomically consumes a setting whose value matches, and reports
// whether this caller was the one that consumed it.
//
// It exists for the one-time setup code, which must be usable exactly
// once across however many replicas are running. Doing the comparison in the
// DELETE is what makes that true without a lock: two simultaneous claims both
// run the statement, and exactly one of them affects a row.
//
// The caller is expected to have already compared the value in constant time.
// This is the atomicity, not the comparison.
func (db *DB) ClaimSetting(ctx context.Context, key, value string) (bool, error) {
	res, err := db.ExecContext(ctx, db.rebind(
		`DELETE FROM settings WHERE key = ? AND value = ?`), key, value)
	if err != nil {
		return false, fmt.Errorf("store: claim setting %s: %w", key, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
