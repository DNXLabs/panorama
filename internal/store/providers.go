// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dnxlabs/panorama/internal/providers"
)

// ListProviders returns every configured provider, in the order a screen shows
// them.
//
// A row naming a kind this build does not serve is an error rather than a row
// to skip. The CHECK constraint makes one unreachable by any write this build
// makes, so finding one means the database was edited by hand or by a newer
// build, and serving around it would quietly ignore a provider somebody
// configured.
func (db *DB) ListProviders(ctx context.Context) ([]providers.Settings, error) {
	rows, err := db.QueryContext(ctx, `
SELECT kind, enabled, region, options, verified_at, verified_model, configured_at, configured_by
  FROM providers`)
	if err != nil {
		return nil, fmt.Errorf("store: list providers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []providers.Settings
	for rows.Next() {
		s, serr := scanProvider(rows.Scan)
		if serr != nil {
			return nil, serr
		}
		out = append(out, s)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read providers: %w", rerr)
	}
	providers.SortSettings(out)
	return out, nil
}

// Provider returns one provider's settings, and whether it is configured.
func (db *DB) Provider(ctx context.Context, kind providers.Kind) (providers.Settings, bool, error) {
	list, err := db.ListProviders(ctx)
	if err != nil {
		return providers.Settings{}, false, err
	}
	for _, s := range list {
		if s.Kind == kind {
			return s, true, nil
		}
	}
	return providers.Settings{}, false, nil
}

func scanProvider(scan func(...any) error) (providers.Settings, error) {
	var (
		s        providers.Settings
		kind     string
		options  string
		verified sql.NullTime
	)
	if err := scan(&kind, &s.Enabled, &s.Region, &options, &verified, &s.VerifiedModel,
		&s.ConfiguredAt, &s.ConfiguredBy); err != nil {
		return providers.Settings{}, fmt.Errorf("store: scan provider: %w", err)
	}
	s.Kind = providers.Kind(kind)
	if !s.Kind.Valid() {
		return providers.Settings{}, fmt.Errorf("store: the providers table names %q, which this build does not serve", kind)
	}
	opts, err := providers.ParseOptions(options)
	if err != nil {
		return providers.Settings{}, fmt.Errorf("store: provider %s: %w", kind, err)
	}
	s.Options = opts
	if verified.Valid {
		at := verified.Time.UTC()
		s.VerifiedAt = &at
	}
	return s, nil
}

// SaveProvider writes a provider's configuration, and reports whether its region
// changed.
//
// A changed region clears the verification in the same statement, because a
// check that passed in one region says nothing about another - and a console
// that kept saying "connected" across the change would be the first screen
// nobody believed.
func (db *DB) SaveProvider(ctx context.Context, s providers.Settings) (regionChanged bool, err error) {
	if !s.Kind.Valid() {
		return false, fmt.Errorf("store: %w: %q is not a provider this build serves", ErrInvalidInput, s.Kind)
	}
	if s.Region == "" {
		return false, fmt.Errorf("store: %w: a provider needs a region", ErrInvalidInput)
	}
	options, err := s.Options.Encode()
	if err != nil {
		return false, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: save provider: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var previous string
	switch qerr := tx.QueryRowContext(ctx, db.rebind(`SELECT region FROM providers WHERE kind = ?`),
		string(s.Kind)).Scan(&previous); {
	case errors.Is(qerr, sql.ErrNoRows):
		regionChanged = true
	case qerr != nil:
		return false, fmt.Errorf("store: read provider %s: %w", s.Kind, qerr)
	default:
		regionChanged = previous != s.Region
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, db.rebind(`
INSERT INTO providers (kind, enabled, region, options, verified_at, verified_model, configured_at, configured_by)
VALUES (?, ?, ?, ?, NULL, '', ?, ?)
ON CONFLICT (kind) DO UPDATE SET
    enabled        = excluded.enabled,
    region         = excluded.region,
    options        = excluded.options,
    verified_at    = CASE WHEN providers.region = excluded.region THEN providers.verified_at ELSE NULL END,
    verified_model = CASE WHEN providers.region = excluded.region THEN providers.verified_model ELSE '' END,
    configured_at  = excluded.configured_at,
    configured_by  = excluded.configured_by`),
		string(s.Kind), s.Enabled, s.Region, options, now, s.ConfiguredBy); err != nil {
		return false, fmt.Errorf("store: save provider %s: %w", s.Kind, err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit provider %s: %w", s.Kind, err)
	}
	return regionChanged, nil
}

// SetProviderVerified records that a live invocation succeeded, against which
// model, and when.
//
// Conditional on the region the check ran against still being the configured
// one, so a check that raced a region change cannot mark the new region as
// verified by a call made to the old one.
func (db *DB) SetProviderVerified(ctx context.Context, kind providers.Kind, region, model string, at time.Time) error {
	if _, err := db.ExecContext(ctx, db.rebind(`
UPDATE providers SET verified_at = ?, verified_model = ?
 WHERE kind = ? AND region = ?`), at.UTC(), model, string(kind), region); err != nil {
		return fmt.Errorf("store: record verification for %s: %w", kind, err)
	}
	return nil
}

// SeedProvider creates a provider's row from the configuration file the first
// time a deployment starts without one, and reports what is stored either way.
//
// First boot only. A row that exists is the administrator's, and the file does
// not revise it: two owners for one setting would give every screen and every
// startup path a branch for which one is speaking. The caller warns when
// the two disagree.
func (db *DB) SeedProvider(ctx context.Context, s providers.Settings) (created bool, stored providers.Settings, err error) {
	existing, found, err := db.Provider(ctx, s.Kind)
	if err != nil {
		return false, providers.Settings{}, err
	}
	if found {
		return false, existing, nil
	}

	if _, serr := db.SaveProvider(ctx, s); serr != nil {
		return false, providers.Settings{}, serr
	}

	stored, _, err = db.Provider(ctx, s.Kind)
	return true, stored, err
}

// RoutesOnProvider lists the routes whose model is on a provider, for the
// refusal that stops a provider being disabled underneath them.
func (db *DB) RoutesOnProvider(ctx context.Context, kind providers.Kind) ([]string, error) {
	routes, err := db.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range routes {
		if r.Target.Provider == kind {
			out = append(out, r.Name)
		}
	}
	return out, nil
}
