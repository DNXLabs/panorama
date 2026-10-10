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

// The states a device-authorization poll can be in, as RFC 8628 names them.
//
// They are errors rather than a status field because that is what the CLI does
// with them: every one of these except a successful claim means "do not proceed
// yet", and an enumeration returned alongside a nil error is an enumeration
// somebody eventually forgets to check.
var (
	// ErrAuthorizationPending means nobody has approved or denied it yet.
	ErrAuthorizationPending = errors.New("store: authorization pending")
	// ErrSlowDown means the client polled faster than the interval it was
	// given. It is not a refusal; the next poll at the right cadence works.
	ErrSlowDown = errors.New("store: polling too fast")
	// ErrAuthorizationExpired means the window closed before a human decided.
	ErrAuthorizationExpired = errors.New("store: authorization expired")
	// ErrAuthorizationDenied means a human said no.
	ErrAuthorizationDenied = errors.New("store: authorization denied")
)

// pollGrace is how much early a poll may be before it counts as too fast.
//
// A client told to wait five seconds and arriving at 4.99 is keeping the
// cadence, not ignoring it.
const pollGrace = time.Second

// DeviceAuthorization is a login in progress: a CLI has asked, and a human has
// not yet decided.
type DeviceAuthorization struct {
	ID         string
	UserCode   string
	ClientName string
	SourceIP   string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Interval   int
	UserID     string
	ApprovedAt *time.Time
	DeniedAt   *time.Time
	ClaimedAt  *time.Time
}

// Grant is what `panorama login` leaves on a laptop.
type Grant struct {
	ID         string
	UserID     string
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
}

// Revoked reports whether the grant has been withdrawn.
func (g *Grant) Revoked() bool { return g != nil && g.RevokedAt != nil }

// CreateDeviceAuthorization opens a device-authorization flow.
func (db *DB) CreateDeviceAuthorization(ctx context.Context, deviceHash, userCode, clientName, sourceIP string, expiresAt time.Time, interval int) (*DeviceAuthorization, error) {
	a := &DeviceAuthorization{
		ID:         NewID("dva"),
		UserCode:   userCode,
		ClientName: clientName,
		SourceIP:   sourceIP,
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  expiresAt,
		Interval:   interval,
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO cli_authorizations
			(id, device_code_hash, user_code, client_name, source_ip, created_at, expires_at, interval_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		a.ID, deviceHash, a.UserCode, a.ClientName, a.SourceIP, a.CreatedAt, a.ExpiresAt, a.Interval)
	if err != nil {
		return nil, fmt.Errorf("store: create device authorization: %w", err)
	}
	return a, nil
}

// DeviceAuthorizationByUserCode finds a live flow by the code a human typed.
//
// Only a flow still awaiting a decision is returned. One already approved,
// denied, or expired is ErrNotFound, so the approval screen cannot be used to
// approve something twice.
func (db *DB) DeviceAuthorizationByUserCode(ctx context.Context, userCode string) (*DeviceAuthorization, error) {
	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT id, user_code, client_name, source_ip, created_at, expires_at, interval_seconds
		FROM cli_authorizations
		WHERE user_code = ?
		  AND approved_at IS NULL AND denied_at IS NULL AND expires_at > ?`),
		userCode, time.Now().UTC())

	var a DeviceAuthorization
	err := row.Scan(&a.ID, &a.UserCode, &a.ClientName, &a.SourceIP, &a.CreatedAt, &a.ExpiresAt, &a.Interval)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: device authorization by user code: %w", err)
	}
	return &a, nil
}

// DecideDeviceAuthorization records a human's answer.
//
// The update is conditional on the flow still being undecided and unexpired, so
// two browser tabs racing produce one decision rather than the last one to
// arrive.
//
// userID is who decided, and it is recorded for a denial as well as for an
// approval - the two are read differently (an approval says whose grant to
// mint, a denial says who refused) but both are worth knowing. It is written as
// NULL rather than as an empty string when unknown, because the column is a
// foreign key and "" is not a user.
func (db *DB) DecideDeviceAuthorization(ctx context.Context, userCode, userID string, approve bool) error {
	now := time.Now().UTC()

	column := "denied_at"
	if approve {
		column = "approved_at"
	}

	var decider any
	if userID != "" {
		decider = userID
	}

	res, err := db.ExecContext(ctx, db.rebind(`
		UPDATE cli_authorizations
		SET `+column+` = ?, user_id = ?
		WHERE user_code = ?
		  AND approved_at IS NULL AND denied_at IS NULL AND expires_at > ?`),
		now, decider, userCode, now)
	if err != nil {
		return fmt.Errorf("store: decide device authorization: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: decide device authorization: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Claim is what a completed device-authorization poll yields.
//
// The client name comes back with the user id because both are on the row the
// claim already read, and the caller needs both: the grant is minted for that
// person and labelled with that machine. Looking the name up again afterwards
// would be a second query for something already in hand - and a second query
// that can race the row's own deletion.
type Claim struct {
	UserID     string
	ClientName string
}

// ClaimDeviceAuthorization is the CLI's poll.
//
// It returns the approving user exactly once. The claim is a conditional
// update rather than a read followed by a write, so two polls arriving together
// cannot both collect a grant - the second sees claimed_at already set and is
// answered as an unknown code, which is what RFC 8628 asks for.
func (db *DB) ClaimDeviceAuthorization(ctx context.Context, deviceHash string) (Claim, error) {
	now := time.Now().UTC()

	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT id, user_id, client_name, expires_at, approved_at, denied_at, claimed_at, interval_seconds, last_polled_at
		FROM cli_authorizations
		WHERE device_code_hash = ?`), deviceHash)

	var (
		id           string
		userID       sql.NullString
		clientName   string
		expiresAt    time.Time
		approvedAt   sql.NullTime
		deniedAt     sql.NullTime
		claimedAt    sql.NullTime
		interval     int
		lastPolledAt sql.NullTime
	)
	err := row.Scan(&id, &userID, &clientName, &expiresAt, &approvedAt, &deniedAt, &claimedAt, &interval, &lastPolledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrTokenRejected
	}
	if err != nil {
		return Claim{}, fmt.Errorf("store: claim device authorization: %w", err)
	}

	// A code already spent is indistinguishable from one that never existed.
	if claimedAt.Valid {
		return Claim{}, ErrTokenRejected
	}

	// Polling cadence is judged before the decision is reported, so a client
	// hammering the endpoint is told to slow down whatever the answer would
	// have been - otherwise the reprimand only ever arrives once.
	//
	// The grace is what stops a well-behaved client being punished for network
	// jitter: one polling at exactly its interval will sometimes arrive a few
	// milliseconds early, and answering that with slow_down would have it back
	// off from a cadence it was already keeping correctly.
	floor := time.Duration(interval)*time.Second - pollGrace
	tooFast := floor > 0 && lastPolledAt.Valid && now.Sub(lastPolledAt.Time) < floor
	if _, uerr := db.ExecContext(ctx, db.rebind(
		`UPDATE cli_authorizations SET last_polled_at = ? WHERE id = ?`), now, id); uerr != nil {
		return Claim{}, fmt.Errorf("store: claim device authorization: %w", uerr)
	}
	if tooFast {
		return Claim{}, ErrSlowDown
	}

	switch {
	case deniedAt.Valid:
		return Claim{}, ErrAuthorizationDenied
	case !expiresAt.After(now):
		return Claim{}, ErrAuthorizationExpired
	case !approvedAt.Valid:
		return Claim{}, ErrAuthorizationPending
	}

	res, err := db.ExecContext(ctx, db.rebind(
		`UPDATE cli_authorizations SET claimed_at = ? WHERE id = ? AND claimed_at IS NULL`), now, id)
	if err != nil {
		return Claim{}, fmt.Errorf("store: claim device authorization: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Claim{}, fmt.Errorf("store: claim device authorization: %w", err)
	}
	if n == 0 {
		// Another poll got there between the read and the write.
		return Claim{}, ErrTokenRejected
	}
	if !userID.Valid || userID.String == "" {
		return Claim{}, fmt.Errorf("store: device authorization %s was approved with no user", id)
	}
	return Claim{UserID: userID.String, ClientName: clientName}, nil
}

// PurgeExpiredDeviceAuthorizations removes flows nobody completed.
//
// Housekeeping, not security: expiry is enforced in the queries above, so a row
// this has not reached yet is already unusable.
func (db *DB) PurgeExpiredDeviceAuthorizations(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, db.rebind(
		`DELETE FROM cli_authorizations WHERE expires_at < ?`), before)
	if err != nil {
		return 0, fmt.Errorf("store: purge device authorizations: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge device authorizations: %w", err)
	}
	return n, nil
}

// CreateGrant records what a signed-in CLI will hold.
func (db *DB) CreateGrant(ctx context.Context, userID, name, hash, prefix string, expiresAt *time.Time) (*Grant, error) {
	g := &Grant{
		ID:        NewID("grt"),
		UserID:    userID,
		Name:      name,
		Prefix:    prefix,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}

	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO cli_grants (id, user_id, name, prefix, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`),
		g.ID, g.UserID, g.Name, g.Prefix, hash, g.CreatedAt, g.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("store: create grant: %w", err)
	}
	return g, nil
}

// AuthenticateGrant resolves a grant hash to the person it belongs to.
//
// Not on the request path: this runs once per token refresh, which is once an
// hour per laptop rather than once per model call. It still returns the same
// singular ErrTokenRejected, because the reasons a grant fails - unknown,
// revoked, expired, owner disabled - are exactly as useful to a guesser here as
// they are at the gateway.
func (db *DB) AuthenticateGrant(ctx context.Context, hash string) (*Principal, string, error) {
	row := db.QueryRowContext(ctx, db.rebind(`
		SELECT g.id, g.expires_at, g.revoked_at,
		       u.id, u.email, u.display_name, u.role, u.status
		FROM cli_grants g
		JOIN users u ON u.id = g.user_id
		WHERE g.token_hash = ?`), hash)

	var (
		p          Principal
		grantID    string
		expiresAt  sql.NullTime
		revokedAt  sql.NullTime
		userStatus string
	)
	err := row.Scan(&grantID, &expiresAt, &revokedAt,
		&p.UserID, &p.Email, &p.DisplayName, &p.Role, &userStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrTokenRejected
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: authenticate grant: %w", err)
	}

	if revokedAt.Valid {
		return nil, "", ErrTokenRejected
	}
	if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		return nil, "", ErrTokenRejected
	}
	if userStatus != "active" {
		return nil, "", ErrTokenRejected
	}

	p.Kind = PrincipalKindUser
	return &p, grantID, nil
}

// ExchangeGrant mints a gateway token for a grant and retires the one it
// replaces, in one transaction.
//
// Both halves together, because either alone is wrong: minting without
// revoking would leave a laptop's whole history of tokens live, and revoking
// without minting would sign it out mid-turn. A grant therefore holds at most
// two working tokens - the one in use and the one just issued - which is what
// bounds how long a revoked grant keeps buying model calls.
func (db *DB) ExchangeGrant(ctx context.Context, grantID, userID, name, hash, prefix string, expiresAt *time.Time) (*GatewayToken, error) {
	now := time.Now().UTC()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: exchange grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, db.rebind(
		`UPDATE gateway_tokens SET revoked_at = ? WHERE grant_id = ? AND revoked_at IS NULL`),
		now, grantID); err != nil {
		return nil, fmt.Errorf("store: exchange grant: retire previous: %w", err)
	}

	t := &GatewayToken{
		ID:        NewID("gwt"),
		UserID:    userID,
		Name:      name,
		Prefix:    prefix,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	if _, err := tx.ExecContext(ctx, db.rebind(`
		INSERT INTO gateway_tokens (id, user_id, name, prefix, token_hash, created_at, expires_at, grant_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		t.ID, t.UserID, t.Name, t.Prefix, hash, t.CreatedAt, t.ExpiresAt, grantID); err != nil {
		return nil, fmt.Errorf("store: exchange grant: mint: %w", err)
	}

	if _, err := tx.ExecContext(ctx, db.rebind(
		`UPDATE cli_grants SET last_used_at = ? WHERE id = ?`), now, grantID); err != nil {
		return nil, fmt.Errorf("store: exchange grant: touch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: exchange grant: %w", err)
	}
	return t, nil
}

// RevokeGrant withdraws a grant and every token it minted, together.
//
// Revoking the grant alone would sign the laptop out of refreshing while
// leaving its current token working until it expired on its own - which is the
// difference between "revoked" and "revoked within the hour" at exactly the
// moment somebody is reaching for this because a machine was lost.
func (db *DB) RevokeGrant(ctx context.Context, grantID string) error {
	now := time.Now().UTC()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: revoke grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, db.rebind(
		`UPDATE cli_grants SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`),
		now, grantID); err != nil {
		return fmt.Errorf("store: revoke grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.rebind(
		`UPDATE gateway_tokens SET revoked_at = ? WHERE grant_id = ? AND revoked_at IS NULL`),
		now, grantID); err != nil {
		return fmt.Errorf("store: revoke grant: tokens: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: revoke grant: %w", err)
	}
	return nil
}

// ListGrants returns a person's signed-in machines, newest first.
func (db *DB) ListGrants(ctx context.Context, userID string) ([]Grant, error) {
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT id, user_id, name, prefix, created_at, last_used_at, expires_at, revoked_at
		FROM cli_grants
		WHERE user_id = ?
		ORDER BY created_at DESC`), userID)
	if err != nil {
		return nil, fmt.Errorf("store: list grants: %w", err)
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var (
			g          Grant
			lastUsedAt sql.NullTime
			expiresAt  sql.NullTime
			revokedAt  sql.NullTime
		)
		if err := rows.Scan(&g.ID, &g.UserID, &g.Name, &g.Prefix,
			&g.CreatedAt, &lastUsedAt, &expiresAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("store: list grants: %w", err)
		}
		if lastUsedAt.Valid {
			g.LastUsedAt = &lastUsedAt.Time
		}
		if expiresAt.Valid {
			g.ExpiresAt = &expiresAt.Time
		}
		if revokedAt.Valid {
			g.RevokedAt = &revokedAt.Time
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list grants: %w", err)
	}
	return out, nil
}

// GrantOwner returns which account a grant belongs to, for an authorization
// check before revocation.
func (db *DB) GrantOwner(ctx context.Context, grantID string) (string, error) {
	var userID string
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT user_id FROM cli_grants WHERE id = ?`), grantID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: grant owner: %w", err)
	}
	return userID, nil
}
