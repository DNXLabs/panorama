// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TeamRef names a team a user belongs to.
type TeamRef struct {
	ID   string
	Slug string
	Name string
	// Role is "lead" or "member".
	Role string
}

// UserSummary is a person as the Users screen shows them.
//
// There is no spend on this type, and there is no query in this package that
// would fill one in.
//
// That is the second trust stance made structural rather than remembered. A
// list of named people with a money column beside each one is a leaderboard
// however it is captioned, and it becomes a sortable one the moment a console
// renders it. What an administrator actually needs in order to do their job -
// who is blocked, who has not onboarded, who holds which role - is here in
// full; what they would need in order to compare two engineers is not, because
// the query does not exist.
//
// A person's own spend is theirs to see, and is served by the usage endpoints
// scoped to the caller.
type UserSummary struct {
	User
	Teams []TeamRef

	// Onboarding, which is a funnel rather than a flag: an account exists, then
	// a token is issued, then a first request arrives. Admins chase the gaps.
	Tokens          int
	LastTokenUsedAt *time.Time
	FirstRequestAt  *time.Time
	LastRequestAt   *time.Time

	// Tools seen, so an admin can tell a developer set up on Claude Code from
	// one who has configured nothing.
	Tools []string
}

// Onboarded reports whether this person has ever actually used the gateway.
func (u UserSummary) Onboarded() bool { return u.FirstRequestAt != nil }

// ListUsers reads everyone, with the facts an administrator needs and none they
// do not.
func (db *DB) ListUsers(ctx context.Context) ([]UserSummary, error) {
	// The timestamps come out of ORDER BY ... LIMIT 1 rather than min() and
	// max(). An aggregate loses the column's declared type, and the SQLite
	// driver decides to hand back a time.Time from the declaration - so max()
	// over a TIMESTAMP column returns a string that will not scan. Reading the
	// column itself keeps the type on both dialects.
	rows, err := db.QueryContext(ctx, `
		SELECT u.id, coalesce(u.external_id, ''), u.email, u.display_name, u.role, u.role_source, u.status, u.created_at,
		       (SELECT count(*) FROM gateway_tokens t WHERE t.user_id = u.id AND t.revoked_at IS NULL),
		       (SELECT t.last_used_at FROM gateway_tokens t
		         WHERE t.user_id = u.id AND t.last_used_at IS NOT NULL
		         ORDER BY t.last_used_at DESC LIMIT 1),
		       (SELECT r.started_at FROM requests r WHERE r.user_id = u.id
		         ORDER BY r.started_at ASC LIMIT 1),
		       (SELECT r.started_at FROM requests r WHERE r.user_id = u.id
		         ORDER BY r.started_at DESC LIMIT 1)
		FROM users u
		ORDER BY u.display_name, u.email`)
	if err != nil {
		return nil, fmt.Errorf("store: list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out   []UserSummary
		byID  = map[string]int{}
		order int
	)
	for rows.Next() {
		var u UserSummary
		if serr := rows.Scan(&u.ID, &u.ExternalID, &u.Email, &u.DisplayName, &u.Role, &u.RoleSource, &u.Status,
			&u.CreatedAt, &u.Tokens, &u.LastTokenUsedAt, &u.FirstRequestAt, &u.LastRequestAt); serr != nil {
			return nil, fmt.Errorf("store: scan user: %w", serr)
		}
		byID[u.ID] = order
		order++
		out = append(out, u)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read users: %w", rerr)
	}

	if err := db.attachTeams(ctx, out, byID); err != nil {
		return nil, err
	}
	return out, db.attachTools(ctx, out, byID)
}

func (db *DB) attachTeams(ctx context.Context, users []UserSummary, byID map[string]int) error {
	if len(users) == 0 {
		return nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT m.user_id, t.id, t.slug, t.name, m.role
		FROM team_members m JOIN teams t ON t.id = m.team_id
		ORDER BY t.name`)
	if err != nil {
		return fmt.Errorf("store: read team membership: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			userID string
			ref    TeamRef
		)
		if serr := rows.Scan(&userID, &ref.ID, &ref.Slug, &ref.Name, &ref.Role); serr != nil {
			return fmt.Errorf("store: scan team membership: %w", serr)
		}
		if i, ok := byID[userID]; ok {
			users[i].Teams = append(users[i].Teams, ref)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("store: read team membership: %w", rerr)
	}
	return nil
}

// attachTools records which clients each person has been seen using.
//
// A count of requests per tool would be a per-user usage number, so this is
// deliberately the set of tools and not how much each was used.
func (db *DB) attachTools(ctx context.Context, users []UserSummary, byID map[string]int) error {
	if len(users) == 0 {
		return nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT user_id, tool FROM requests
		WHERE user_id IS NOT NULL AND tool <> ''
		ORDER BY tool`)
	if err != nil {
		return fmt.Errorf("store: read tools seen: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var userID, tool string
		if serr := rows.Scan(&userID, &tool); serr != nil {
			return fmt.Errorf("store: scan tool seen: %w", serr)
		}
		if i, ok := byID[userID]; ok {
			users[i].Tools = append(users[i].Tools, tool)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("store: read tools seen: %w", rerr)
	}
	return nil
}

// UserByID reads one person, or ErrNotFound.
func (db *DB) UserByID(ctx context.Context, id string) (*User, error) {
	var u User
	err := db.QueryRowContext(ctx, db.rebind(`
		SELECT id, coalesce(external_id, ''), email, display_name, role, role_source, status, created_at
		FROM users WHERE id = ?`), id).
		Scan(&u.ID, &u.ExternalID, &u.Email, &u.DisplayName, &u.Role, &u.RoleSource, &u.Status, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: user by id: %w", err)
	}
	return &u, nil
}

// CountUsers reports how many accounts exist. Zero is the signal that a
// deployment has never been set up.
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

// SetUserRole changes what a person may do.
//
// Their live console sessions are revoked in the same transaction, because a
// role is copied into a session when it is established: without this, a
// demoted admin keeps administering until their browser session expires.
func (db *DB) SetUserRole(ctx context.Context, id, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("store: %w: %q is not a role", ErrInvalidInput, role)
	}
	// Recorded as a person's decision, which is what stops the next sign-in
	// from recomputing it back to whatever the group mapping says. Without it
	// this function writes a role that lasts until its owner reloads a browser
	// tab, and says nothing about having done so.
	return db.updateUser(ctx, id, `role = ?, role_source = ?`, role, RoleSourceManual)
}

// FollowGroupMapping hands an account back to the group mapping.
//
// The role does not change here, because nothing outside a sign-in can
// recompute it: group membership is the provider's record and Panorama keeps no
// copy. What changes is that the next sign-in is allowed to decide again.
func (db *DB) FollowGroupMapping(ctx context.Context, id string) error {
	return db.updateUser(ctx, id, `role_source = ?`, RoleSourceSSO)
}

// SetUserStatus enables or disables an account, revoking its sessions.
//
// Disabling does not delete anything. The audit trail has to outlive the
// account, and a person who left the company is still who those requests
// belonged to.
func (db *DB) SetUserStatus(ctx context.Context, id, status string) error {
	if status != "active" && status != "disabled" {
		return fmt.Errorf("store: %w: %q is not a status", ErrInvalidInput, status)
	}
	return db.updateUser(ctx, id, `status = ?`, status)
}

func (db *DB) updateUser(ctx context.Context, id, assignment string, values ...any) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: update user: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, db.rebind(
		`UPDATE users SET `+assignment+`, updated_at = ? WHERE id = ?`), append(values, now, id)...)
	if err != nil {
		return fmt.Errorf("store: update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, db.rebind(
		`UPDATE console_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`),
		now, id); err != nil {
		return fmt.Errorf("store: revoke sessions after user change: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit user change: %w", err)
	}
	return nil
}

// TokenSummary is a gateway token as its owner sees it listed.
type TokenSummary struct {
	GatewayToken
	// LastUsedAt is nil for a token no tool has presented yet, which is
	// precisely the state the onboarding screen is watching for.
	LastUsedAt *time.Time
	// Revoked tokens are listed too, briefly, so a user who has just revoked
	// one sees that it happened rather than a row silently vanishing.
	Revoked bool
}

// ListGatewayTokens reads a person's tokens, newest first.
func (db *DB) ListGatewayTokens(ctx context.Context, userID string) ([]TokenSummary, error) {
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT id, user_id, name, prefix, created_at, expires_at, revoked_at, last_used_at
		FROM gateway_tokens WHERE user_id = ?
		ORDER BY created_at DESC`), userID)
	if err != nil {
		return nil, fmt.Errorf("store: list gateway tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TokenSummary
	for rows.Next() {
		var (
			t          TokenSummary
			lastUsedAt sql.NullTime
		)
		if serr := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedAt,
			&t.ExpiresAt, &t.RevokedAt, &lastUsedAt); serr != nil {
			return nil, fmt.Errorf("store: scan gateway token: %w", serr)
		}
		t.Revoked = t.RevokedAt != nil
		if lastUsedAt.Valid {
			at := lastUsedAt.Time.UTC()
			t.LastUsedAt = &at
		}
		out = append(out, t)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read gateway tokens: %w", rerr)
	}
	return out, nil
}

// GatewayTokenOwner reports who a token belongs to, so a revoke request can be
// authorised before it is carried out.
func (db *DB) GatewayTokenOwner(ctx context.Context, tokenID string) (string, error) {
	var userID string
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT user_id FROM gateway_tokens WHERE id = ?`), tokenID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: gateway token owner: %w", err)
	}
	return userID, nil
}

// TeamDetail is a team with its membership.
type TeamDetail struct {
	Team
	Members []TeamMember
}

// TeamMember is one person's membership of a team.
type TeamMember struct {
	UserID      string
	Email       string
	DisplayName string
	Role        string
	Status      string
}

// TeamWithMembers reads one team and who is in it.
func (db *DB) TeamWithMembers(ctx context.Context, slug string) (*TeamDetail, error) {
	team, err := db.TeamBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT u.id, u.email, u.display_name, m.role, u.status
		FROM team_members m JOIN users u ON u.id = m.user_id
		WHERE m.team_id = ?
		ORDER BY u.display_name, u.email`), team.ID)
	if err != nil {
		return nil, fmt.Errorf("store: read team members: %w", err)
	}
	defer func() { _ = rows.Close() }()

	detail := &TeamDetail{Team: *team}
	for rows.Next() {
		var m TeamMember
		if serr := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Role, &m.Status); serr != nil {
			return nil, fmt.Errorf("store: scan team member: %w", serr)
		}
		detail.Members = append(detail.Members, m)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read team members: %w", rerr)
	}
	return detail, nil
}

// Slugify exposes the team-slug rule so the console can show the slug a name
// will produce before the team is created.
func Slugify(name string) string { return slugify(name) }

// NormalizeEmail is the one place an address is folded to its stored form.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
