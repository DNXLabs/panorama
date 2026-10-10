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

// Roles inside a team. Distinct from the deployment roles in users.go: a lead
// leads a team, which is not the same as being able to administer Panorama.
const (
	TeamRoleLead   = "lead"
	TeamRoleMember = "member"
)

// Team is a group of users, and the middle scope a budget can govern.
//
// Teams exist because the numbers leaders are allowed to see are team
// aggregates. That is not an incidental grouping - it is the shape of the
// product's second trust commitment, so a team is a first-class thing rather
// than a tag on a user.
type Team struct {
	ID          string
	Name        string
	Slug        string
	Description string
	CreatedAt   time.Time
}

// CreateTeam inserts a team. The slug is the stable, human-writable identifier
// a policy document refers to.
func (db *DB) CreateTeam(ctx context.Context, name, slug, description string) (*Team, error) {
	name = strings.TrimSpace(name)
	slug = strings.ToLower(strings.TrimSpace(slug))

	if name == "" {
		return nil, fmt.Errorf("store: %w: a team needs a name", ErrInvalidInput)
	}
	if slug == "" {
		slug = slugify(name)
	}
	if !validSlug(slug) {
		return nil, fmt.Errorf("store: %w: %q is not a slug (lower-case letters, digits and hyphens)", ErrInvalidInput, slug)
	}

	t := &Team{
		ID:          NewID("tm"),
		Name:        name,
		Slug:        slug,
		Description: description,
		CreatedAt:   time.Now().UTC(),
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO teams (id, name, slug, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`),
		t.ID, t.Name, t.Slug, t.Description, t.CreatedAt, t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: create team: %w", err)
	}
	return t, nil
}

// TeamBySlug looks a team up by its slug.
func (db *DB) TeamBySlug(ctx context.Context, slug string) (*Team, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))

	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT id, name, slug, description, created_at FROM teams WHERE slug = ?`), slug)

	var t Team
	err := row.Scan(&t.ID, &t.Name, &t.Slug, &t.Description, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: team by slug: %w", err)
	}
	return &t, nil
}

// ListTeams returns every team, by name.
func (db *DB) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, slug, description, created_at FROM teams ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.Slug, &t.Description, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan team: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read teams: %w", err)
	}
	return out, nil
}

// AddTeamMember puts a user on a team, or updates their role if they are
// already on it.
func (db *DB) AddTeamMember(ctx context.Context, teamID, userID, role string) error {
	switch role {
	case TeamRoleLead, TeamRoleMember:
	case "":
		role = TeamRoleMember
	default:
		return fmt.Errorf("store: %w: %q is not a team role (lead or member)", ErrInvalidInput, role)
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO team_members (team_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = excluded.role`),
		teamID, userID, role, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("store: add team member: %w", err)
	}
	return nil
}

// RemoveTeamMember takes a user off a team.
func (db *DB) RemoveTeamMember(ctx context.Context, teamID, userID string) error {
	_, err := db.ExecContext(ctx, db.rebind(
		`DELETE FROM team_members WHERE team_id = ? AND user_id = ?`), teamID, userID)
	if err != nil {
		return fmt.Errorf("store: remove team member: %w", err)
	}
	return nil
}

// TeamMemberCount reports how many people are on a team.
//
// A count rather than a list, because a count is what the console needs and a
// list of names beside a team's spend is halfway to the comparison the product
// does not make.
func (db *DB) TeamMemberCount(ctx context.Context, teamID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT count(*) FROM team_members WHERE team_id = ?`), teamID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count team members: %w", err)
	}
	return n, nil
}

// UserIDByEmail resolves an email address to a user id, for policy documents.
func (db *DB) UserIDByEmail(ctx context.Context, email string) (string, error) {
	u, err := db.UserByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// TeamIDBySlug resolves a team slug to a team id, for policy documents.
func (db *DB) TeamIDBySlug(ctx context.Context, slug string) (string, error) {
	t, err := db.TeamBySlug(ctx, slug)
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// slugify derives a slug from a name, for the common case where an operator
// does not want to think of one.
func slugify(name string) string {
	var b strings.Builder
	b.Grow(len(name))

	lastHyphen := true // leading hyphens are dropped
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// validSlug reports whether a slug is one this store will accept. An allowlist
// of characters rather than a list of forbidden ones.
func validSlug(slug string) bool {
	if slug == "" {
		return false
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return !strings.HasPrefix(slug, "-") && !strings.HasSuffix(slug, "-")
}

// TeamMemberInput is one row of a roster as the console sends it.
type TeamMemberInput struct {
	UserID string
	Role   string
}

// SetTeamMembers replaces a team's roster with exactly this list.
//
// One transaction, because a roster is one edit. Applied as a diff - add, then
// remove - a failure halfway leaves people on a team somebody took them off and
// a lead who is not a member, and nothing on the screen would say which half
// landed.
func (db *DB) SetTeamMembers(ctx context.Context, teamID string, members []TeamMemberInput) error {
	keep := make(map[string]string, len(members))
	for _, m := range members {
		role := m.Role
		switch role {
		case TeamRoleLead, TeamRoleMember:
		case "":
			role = TeamRoleMember
		default:
			return fmt.Errorf("store: %w: %q is not a team role (lead or member)", ErrInvalidInput, role)
		}
		keep[m.UserID] = role
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set team members: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx, db.rebind(
		`DELETE FROM team_members WHERE team_id = ?`), teamID); err != nil {
		return fmt.Errorf("store: clear team members: %w", err)
	}

	now := time.Now().UTC()
	for userID, role := range keep {
		if _, err = tx.ExecContext(ctx, db.rebind(`
			INSERT INTO team_members (team_id, user_id, role, created_at)
			VALUES (?, ?, ?, ?)`), teamID, userID, role, now); err != nil {
			return fmt.Errorf("store: add team member: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: set team members: %w", err)
	}
	return nil
}

// TeamServiceTokens counts the service tokens billed to a team.
//
// Asked before a team is deleted. `service_tokens.team_id` cascades, so
// deleting a team revokes every pipeline credential that bills it - silently,
// at whatever hour the pipeline next runs.
func (db *DB) TeamServiceTokens(ctx context.Context, teamID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT count(*) FROM service_tokens WHERE team_id = ? AND revoked_at IS NULL`),
		teamID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count team service tokens: %w", err)
	}
	return n, nil
}

// DeleteTeam removes a team and its membership.
//
// A budget scoped to it is deliberately left behind: `budgets.scope_id` has no
// foreign key, and the schema's reasoning holds here too - an orphan an
// operator can see in `panorama policy show` beats one that vanished with the
// thing it governed. It governs nobody in the meantime, because no membership
// resolves to it.
func (db *DB) DeleteTeam(ctx context.Context, teamID string) (bool, error) {
	result, err := db.ExecContext(ctx, db.rebind(`DELETE FROM teams WHERE id = ?`), teamID)
	if err != nil {
		return false, fmt.Errorf("store: delete team: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete team: %w", err)
	}
	return affected > 0, nil
}
