// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// OIDCFlow is one sign-in redirect in flight.
type OIDCFlow struct {
	State      string
	Nonce      string
	Verifier   string
	RedirectTo string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// StartOIDCFlow records a redirect to the identity provider.
func (db *DB) StartOIDCFlow(ctx context.Context, f OIDCFlow) error {
	now := time.Now().UTC()
	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO oidc_flows (state, nonce, verifier, redirect_to, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`),
		f.State, f.Nonce, f.Verifier, f.RedirectTo, now, f.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("store: start oidc flow: %w", err)
	}
	return nil
}

// TakeOIDCFlow consumes a flow, returning it exactly once.
//
// Read and delete in one transaction, so a replayed callback finds nothing.
// Single-use is what makes an authorization code replay useless even inside the
// flow's short lifetime, and doing it here means no caller has to remember to.
func (db *DB) TakeOIDCFlow(ctx context.Context, state string) (*OIDCFlow, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: take oidc flow: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var f OIDCFlow
	err = tx.QueryRowContext(ctx, db.rebind(`
		SELECT state, nonce, verifier, redirect_to, created_at, expires_at
		FROM oidc_flows WHERE state = ?`), state).
		Scan(&f.State, &f.Nonce, &f.Verifier, &f.RedirectTo, &f.CreatedAt, &f.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read oidc flow: %w", err)
	}

	if _, derr := tx.ExecContext(ctx, db.rebind(`DELETE FROM oidc_flows WHERE state = ?`), state); derr != nil {
		return nil, fmt.Errorf("store: consume oidc flow: %w", derr)
	}
	if cerr := tx.Commit(); cerr != nil {
		return nil, fmt.Errorf("store: commit oidc flow: %w", cerr)
	}

	if !f.ExpiresAt.After(time.Now().UTC()) {
		// Consumed and refused. An expired flow is spent either way, and
		// leaving it in place would let it be retried until the sweeper ran.
		return nil, ErrNotFound
	}
	return &f, nil
}

// GroupRole maps one identity-provider group onto a Panorama role.
type GroupRole struct {
	Group     string
	Role      string
	UpdatedAt time.Time
}

// SetGroupRole records that members of a group hold a role.
func (db *DB) SetGroupRole(ctx context.Context, group, role string) error {
	group = strings.TrimSpace(group)
	if group == "" {
		return fmt.Errorf("store: %w: a group name is required", ErrInvalidInput)
	}
	if !ValidRole(role) {
		return fmt.Errorf("store: %w: %q is not a role", ErrInvalidInput, role)
	}

	now := time.Now().UTC()
	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO oidc_group_roles (group_name, role, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (group_name) DO UPDATE SET role = excluded.role, updated_at = excluded.updated_at`),
		group, role, now, now)
	if err != nil {
		return fmt.Errorf("store: set group role: %w", err)
	}
	return nil
}

// DeleteGroupRole removes a mapping, and reports whether there was one.
func (db *DB) DeleteGroupRole(ctx context.Context, group string) (bool, error) {
	res, err := db.ExecContext(ctx, db.rebind(
		`DELETE FROM oidc_group_roles WHERE group_name = ?`), group)
	if err != nil {
		return false, fmt.Errorf("store: delete group role: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GroupRoles reads the whole mapping, ordered so the console does not reshuffle.
func (db *DB) GroupRoles(ctx context.Context) ([]GroupRole, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT group_name, role, updated_at FROM oidc_group_roles ORDER BY group_name`)
	if err != nil {
		return nil, fmt.Errorf("store: group roles: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []GroupRole
	for rows.Next() {
		var g GroupRole
		if serr := rows.Scan(&g.Group, &g.Role, &g.UpdatedAt); serr != nil {
			return nil, fmt.Errorf("store: scan group role: %w", serr)
		}
		out = append(out, g)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read group roles: %w", rerr)
	}
	return out, nil
}

// Identity is what an identity provider asserted about a person at sign-in.
type Identity struct {
	Subject     string
	Email       string
	DisplayName string
	Groups      []string
}

// ResolveRole picks the role an identity holds, given the mapping and the role
// unmapped users fall to.
//
// The strongest matching group wins. Anything else would make role assignment
// depend on the order the provider happened to list a user's groups, which is
// not a property anyone should have to reason about - and would mean adding
// someone to a second group could silently demote them.
func ResolveRole(identity Identity, mapping []GroupRole, defaultRole string) (string, bool) {
	byGroup := make(map[string]string, len(mapping))
	for _, m := range mapping {
		// Two rows differing only in case are one group to this matcher, so the
		// stronger of them wins for the same reason the strongest matching
		// group does: which row happened to be read last is not a property
		// anybody should have to reason about, and it decided who was an
		// administrator. The write path refuses to create such a pair; this is
		// what makes one that already exists - a hand-edited database, or a
		// mapping written before that check - resolve the same way every time.
		key := strings.ToLower(strings.TrimSpace(m.Group))
		if roleRank(m.Role) > roleRank(byGroup[key]) {
			byGroup[key] = m.Role
		}
	}

	best := ""
	for _, g := range identity.Groups {
		role, ok := byGroup[strings.ToLower(strings.TrimSpace(g))]
		if !ok {
			continue
		}
		if best == "" || roleRank(role) > roleRank(best) {
			best = role
		}
	}
	if best != "" {
		return best, true
	}

	if !ValidRole(defaultRole) {
		// The configured default is "none": only an explicitly mapped group may
		// sign in. Refusing is the whole point of that setting, so it is
		// refused rather than quietly downgraded to developer.
		return "", false
	}
	return defaultRole, true
}

func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleManager:
		return 2
	case RoleDeveloper:
		return 1
	default:
		return 0
	}
}

// UpsertIdentity provisions or updates the account behind a sign-in.
//
// Panorama does not own identities: the provider does. So the subject claim is
// the key, the email and display name are refreshed from the token on every
// sign-in, and an account whose email changed at the provider follows it rather
// than becoming a second account.
//
// The role is applied only when it differs, and the returned flag says whether
// it moved, so the caller can sign the user out of their other browsers - a
// role held in a live session would otherwise outlive the change that revoked
// it.
//
// source says who decided the role. A sign-in decides with RoleSourceSSO, and
// an account whose role a person set by hand is left alone: recomputing it would
// undo the promotion at the next sign-in, silently, which is how a deployment
// ends up with nobody who can administer it. RoleSourceManual overrides in the
// other direction, and is how the first sign-in of a deployment keeps the
// administrator it is granted.
func (db *DB) UpsertIdentity(ctx context.Context, identity Identity, role, source string) (*User, bool, error) {
	email := strings.ToLower(strings.TrimSpace(identity.Email))
	if email == "" {
		return nil, false, fmt.Errorf("store: %w: the identity provider returned no email address", ErrInvalidInput)
	}
	if identity.Subject == "" {
		return nil, false, fmt.Errorf("store: %w: the identity provider returned no subject", ErrInvalidInput)
	}
	if !ValidRole(role) {
		return nil, false, fmt.Errorf("store: %w: %q is not a role", ErrInvalidInput, role)
	}
	if source != RoleSourceSSO && source != RoleSourceManual {
		return nil, false, fmt.Errorf("store: %w: %q is not a role source", ErrInvalidInput, source)
	}

	name := identity.DisplayName
	if name == "" {
		name = email
	}
	now := time.Now().UTC()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("store: upsert identity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := db.userForIdentity(ctx, tx, identity.Subject, email)
	if err != nil {
		return nil, false, err
	}

	if existing == nil {
		u := &User{
			ID: NewID("usr"), ExternalID: identity.Subject, Email: email,
			DisplayName: name, Role: role, RoleSource: source, Status: "active", CreatedAt: now,
		}
		_, err = tx.ExecContext(ctx, db.rebind(`
			INSERT INTO users (id, external_id, email, display_name, role, role_source, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			u.ID, u.ExternalID, u.Email, u.DisplayName, u.Role, u.RoleSource, u.Status, now, now)
		if err != nil {
			return nil, false, fmt.Errorf("store: provision user: %w", err)
		}
		if cerr := tx.Commit(); cerr != nil {
			return nil, false, fmt.Errorf("store: commit provisioning: %w", cerr)
		}
		return u, true, nil
	}

	// A role a person assigned is not the mapping's to revise. The sign-in
	// still refreshes everything else the provider is authoritative for -
	// subject, email, display name - because that is the provider's record and
	// this is not a disagreement about it.
	if existing.RoleSource == RoleSourceManual && source == RoleSourceSSO {
		role, source = existing.Role, existing.RoleSource
	}

	roleChanged := existing.Role != role
	_, err = tx.ExecContext(ctx, db.rebind(`
		UPDATE users SET external_id = ?, email = ?, display_name = ?, role = ?, role_source = ?, updated_at = ?
		WHERE id = ?`),
		identity.Subject, email, name, role, source, now, existing.ID)
	if err != nil {
		return nil, false, fmt.Errorf("store: refresh user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("store: commit refresh: %w", err)
	}

	existing.ExternalID = identity.Subject
	existing.Email = email
	existing.DisplayName = name
	existing.Role = role
	existing.RoleSource = source
	return existing, roleChanged, nil
}

// userForIdentity finds the account a sign-in belongs to.
//
// The subject is tried first, because it is the stable identifier and survives
// an email change. Email is the fallback, and only that: it is how an account
// created by `panorama token create` before SSO was configured gets adopted by
// its owner's first sign-in rather than duplicated.
func (db *DB) userForIdentity(ctx context.Context, tx *sql.Tx, subject, email string) (*User, error) {
	for _, lookup := range []struct{ column, value string }{
		{"external_id", subject},
		{"lower(email)", email},
	} {
		var u User
		err := tx.QueryRowContext(ctx, db.rebind(`
			SELECT id, coalesce(external_id, ''), email, display_name, role, role_source, status, created_at
			FROM users WHERE `+lookup.column+` = ?`), lookup.value).
			Scan(&u.ID, &u.ExternalID, &u.Email, &u.DisplayName, &u.Role, &u.RoleSource, &u.Status, &u.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: find user for identity: %w", err)
		}
		return &u, nil
	}
	return nil, nil
}

// Counter names recorded on each completed sign-in.
const (
	// CounterSignIns counts sign-ins rather than accounts. A deployment where
	// one person signed in forty times and nobody else ever did is a different
	// situation from forty people signing in once, and the users table cannot
	// tell them apart.
	CounterSignIns = "sign_ins"
	// CounterSignInsWithoutGroups counts sign-ins whose groups claim was
	// empty. One is somebody in no group; all of them is a claim that is not
	// being emitted at all.
	CounterSignInsWithoutGroups = "sign_ins_without_groups"
)

// ObservedGroup is a group string an identity provider has actually sent.
type ObservedGroup struct {
	Group     string
	FirstSeen time.Time
	LastSeen  time.Time
	Count     int64
}

// ObservedClaim is a claim name an ID token has actually carried.
//
// Empty counts sign-ins where the claim was present and carried nothing, which
// is a different diagnosis from the claim being absent: present-and-empty means
// the provider knows the claim and has nothing to put in it, absent means it is
// not being emitted, and only one of those is fixed by renaming a setting.
type ObservedClaim struct {
	Claim     string
	FirstSeen time.Time
	LastSeen  time.Time
	Count     int64
	Empty     int64
}

// SignInObservation is what one completed sign-in revealed about the token
// shape a provider is issuing.
//
// Groups are the values of the configured groups claim. Claims are the names of
// every top-level claim the ID token carried, with the ones that carried nothing
// listed in Empty. There are no claim *values* here and there is nowhere to put
// one: a value is somebody's email, subject, name or group membership, and this
// records the shape of a token rather than its contents.
type SignInObservation struct {
	Groups []string
	Claims []string
	Empty  []string
}

// RecordSignIn records what a sign-in carried.
//
// Called for every completed token exchange, including one that is then refused
// for holding no mapped group - that refusal is precisely when an administrator
// needs to know what the provider called the groups the person was in.
//
// Nothing here records who was in what. The group names are organizational
// data, the same category as the mapping they feed; the membership behind them
// stays the provider's record.
func (db *DB) RecordSignIn(ctx context.Context, obs SignInObservation) error {
	now := time.Now().UTC()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: record sign-in: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seen := map[string]bool{}
	for _, g := range obs.Groups {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			// A provider repeating a group within one token should not count
			// as two sign-ins carrying it.
			continue
		}
		seen[g] = true

		if _, eerr := tx.ExecContext(ctx, db.rebind(`
			INSERT INTO oidc_observed_groups (group_name, first_seen_at, last_seen_at, seen_count)
			VALUES (?, ?, ?, 1)
			ON CONFLICT (group_name) DO UPDATE SET
				last_seen_at = excluded.last_seen_at,
				seen_count   = oidc_observed_groups.seen_count + 1`),
			g, now, now); eerr != nil {
			return fmt.Errorf("store: record observed group: %w", eerr)
		}
	}

	empty := map[string]bool{}
	for _, c := range obs.Empty {
		empty[strings.TrimSpace(c)] = true
	}
	recorded := map[string]bool{}
	for _, c := range obs.Claims {
		c = strings.TrimSpace(c)
		if c == "" || recorded[c] {
			continue
		}
		recorded[c] = true

		wasEmpty := 0
		if empty[c] {
			wasEmpty = 1
		}
		if _, eerr := tx.ExecContext(ctx, db.rebind(`
			INSERT INTO oidc_observed_claims (claim_name, first_seen_at, last_seen_at, seen_count, empty_count)
			VALUES (?, ?, ?, 1, ?)
			ON CONFLICT (claim_name) DO UPDATE SET
				last_seen_at = excluded.last_seen_at,
				seen_count   = oidc_observed_claims.seen_count + 1,
				empty_count  = oidc_observed_claims.empty_count + excluded.empty_count`),
			c, now, now, wasEmpty); eerr != nil {
			return fmt.Errorf("store: record observed claim: %w", eerr)
		}
	}

	if err := bumpCounter(ctx, tx, db, CounterSignIns); err != nil {
		return err
	}
	if len(seen) == 0 {
		if err := bumpCounter(ctx, tx, db, CounterSignInsWithoutGroups); err != nil {
			return err
		}
	}

	if cerr := tx.Commit(); cerr != nil {
		return fmt.Errorf("store: commit sign-in observation: %w", cerr)
	}
	return nil
}

// bumpCounter increments in one statement, so concurrent sign-ins cannot
// interleave a read and a write and lose one.
func bumpCounter(ctx context.Context, tx *sql.Tx, db *DB, name string) error {
	_, err := tx.ExecContext(ctx, db.rebind(`
		INSERT INTO oidc_signin_counters (name, count) VALUES (?, 1)
		ON CONFLICT (name) DO UPDATE SET count = oidc_signin_counters.count + 1`), name)
	if err != nil {
		return fmt.Errorf("store: bump %s: %w", name, err)
	}
	return nil
}

// ObservedGroups lists what has been seen, most-used first, so the mapping
// screen offers the groups a deployment actually runs on before the ones one
// person was in once.
func (db *DB) ObservedGroups(ctx context.Context) ([]ObservedGroup, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT group_name, first_seen_at, last_seen_at, seen_count
		FROM oidc_observed_groups
		ORDER BY seen_count DESC, group_name`)
	if err != nil {
		return nil, fmt.Errorf("store: observed groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ObservedGroup
	for rows.Next() {
		var g ObservedGroup
		if serr := rows.Scan(&g.Group, &g.FirstSeen, &g.LastSeen, &g.Count); serr != nil {
			return nil, fmt.Errorf("store: scan observed group: %w", serr)
		}
		out = append(out, g)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read observed groups: %w", rerr)
	}
	return out, nil
}

// ObservedClaims lists the claim names sign-ins have carried, alphabetically so
// an operator can scan for the one they expected rather than for the one that
// happened to arrive most often.
func (db *DB) ObservedClaims(ctx context.Context) ([]ObservedClaim, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT claim_name, first_seen_at, last_seen_at, seen_count, empty_count
		FROM oidc_observed_claims
		ORDER BY claim_name`)
	if err != nil {
		return nil, fmt.Errorf("store: observed claims: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ObservedClaim
	for rows.Next() {
		var c ObservedClaim
		if serr := rows.Scan(&c.Claim, &c.FirstSeen, &c.LastSeen, &c.Count, &c.Empty); serr != nil {
			return nil, fmt.Errorf("store: scan observed claim: %w", serr)
		}
		out = append(out, c)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read observed claims: %w", rerr)
	}
	return out, nil
}

// SignInCounters reads every counter. A name that has never been incremented is
// absent rather than zero, so callers use the map's zero value.
func (db *DB) SignInCounters(ctx context.Context) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, count FROM oidc_signin_counters`)
	if err != nil {
		return nil, fmt.Errorf("store: sign-in counters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var (
			name string
			n    int64
		)
		if serr := rows.Scan(&name, &n); serr != nil {
			return nil, fmt.Errorf("store: scan sign-in counter: %w", serr)
		}
		out[name] = n
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read sign-in counters: %w", rerr)
	}
	return out, nil
}

// SortGroupRoles orders a mapping for display: strongest role first, then by
// group name, so the console shows who is privileged at the top.
func SortGroupRoles(roles []GroupRole) {
	sort.SliceStable(roles, func(i, j int) bool {
		if roleRank(roles[i].Role) != roleRank(roles[j].Role) {
			return roleRank(roles[i].Role) > roleRank(roles[j].Role)
		}
		return roles[i].Group < roles[j].Group
	})
}
