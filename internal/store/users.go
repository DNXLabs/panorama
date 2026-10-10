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

// Roles a user can hold. Group-to-role mapping happens at sign-in; the set is
// an allowlist enforced by a CHECK constraint in the schema as well as here.
const (
	RoleAdmin     = "admin"
	RoleManager   = "manager"
	RoleDeveloper = "developer"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// ErrInvalidInput is returned for a caller-supplied value the store will not
// act on, such as a malformed pagination cursor. It is distinct from a database
// failure so an API can answer 400 rather than 500.
var ErrInvalidInput = errors.New("store: invalid input")

// Where a role came from, which decides whether a sign-in may recompute it.
//
// RoleSourceSSO means the group mapping decided, by matching a group or by
// falling to the default role, and every sign-in decides again - that is how a
// change at the identity provider takes effect. RoleSourceManual means a person
// decided, and the mapping leaves it alone: a promotion undone by the next
// sign-in is not a promotion, and the account that owns a fresh deployment holds
// its role by exactly this route.
const (
	RoleSourceSSO    = "sso"
	RoleSourceManual = "manual"
)

// User is a person who can use the gateway.
type User struct {
	ID          string
	ExternalID  string
	Email       string
	DisplayName string
	Role        string
	RoleSource  string
	Status      string
	CreatedAt   time.Time
}

// RoleSetup is what a redeemed setup code acts as.
//
// Deliberately not in ValidRole, and deliberately absent from the CHECK
// constraints on users.role and oidc_group_roles.role. It is an authorization
// label that exists only on a live Principal: it cannot be written to a user,
// cannot be mapped to a group, and cannot be chosen as the default role,
// because there is no code path that would accept it and the database would
// refuse it if there were.
//
// It is also not on the admin/manager/developer ladder. roleRank returns zero
// for it, so it can never win a group match - which matters less than it
// sounds, since it can never be in a mapping in the first place, but a role
// that ranks above developer by accident is the kind of thing worth closing
// twice.
const RoleSetup = "setup"

// RoleService is what a team's service token acts as.
//
// Built the same way as RoleSetup and for the same reason: absent from
// ValidRole and from the CHECK constraints on users.role and
// oidc_group_roles.role, so it cannot be written to a person, mapped to a
// group, or chosen as the default role.
//
// The difference is which door it opens. RoleSetup is admitted by the handful
// of routes that configure a deployment; RoleService is admitted by no console
// route at all. It authenticates at the gateway and nowhere else, so a control
// plane endpoint added later refuses a pipeline by default rather than by
// somebody remembering to exclude it.
const RoleService = "service"

// ValidRole reports whether role is one Panorama recognises for a user.
//
// RoleSetup is not among them, on purpose. Everything that assigns a role to a
// person goes through here, so the setup principal cannot become an account by
// any route.
func ValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleManager, RoleDeveloper:
		return true
	default:
		return false
	}
}

// CreateUser inserts a user. Email is normalised to lower case so the
// case-insensitive uniqueness the schema enforces is also what callers see.
func (db *DB) CreateUser(ctx context.Context, email, displayName, role string) (*User, error) {
	if !ValidRole(role) {
		return nil, fmt.Errorf("store: %q is not a valid role", role)
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, errors.New("store: email is required")
	}
	if displayName == "" {
		displayName = email
	}

	u := &User{
		ID:          NewID("usr"),
		Email:       email,
		DisplayName: displayName,
		Role:        role,
		Status:      "active",
		CreatedAt:   time.Now().UTC(),
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO users (id, email, display_name, role, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`),
		u.ID, u.Email, u.DisplayName, u.Role, u.Status, u.CreatedAt, u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: create user: %w", err)
	}
	return u, nil
}

// UserByEmail looks a user up by email address, case-insensitively.
func (db *DB) UserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT id, email, display_name, role, status, created_at
		FROM users WHERE lower(email) = ?`), email)

	var u User
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Status, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: user by email: %w", err)
	}
	return &u, nil
}

// EnsureUser returns the user with this email, creating them if absent.
func (db *DB) EnsureUser(ctx context.Context, email, displayName, role string) (*User, error) {
	u, err := db.UserByEmail(ctx, email)
	switch {
	case err == nil:
		return u, nil
	case errors.Is(err, ErrNotFound):
		return db.CreateUser(ctx, email, displayName, role)
	default:
		return nil, err
	}
}
