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

// Which kind of credential an integration's ciphertext holds.
const (
	SecretKindNone   = ""
	SecretKindAppKey = "app_key"
	SecretKindToken  = "token"
)

// IntegrationConfig is a connection an administrator set up in the console.
//
// The secret is not in this struct on the way out, and that is deliberate: a
// credential the console can read back is a credential the console can leak.
// What comes out is enough to show what is connected and to let somebody change
// it, never enough to use it somewhere else.
type IntegrationConfig struct {
	Kind     string
	Provider string
	Enabled  bool

	AppID          string
	InstallationID string
	APIBaseURL     string

	// SecretKind says what kind of credential is stored, without disclosing it.
	// Empty means none is.
	SecretKind string

	// Fixtures is the file a mock source reads. Not a secret, and only ever set
	// on a mock.
	Fixtures string

	ConfiguredAt *time.Time
	ConfiguredBy string
}

// HasSecret reports whether a credential is stored for this connection.
func (c IntegrationConfig) HasSecret() bool { return c.SecretKind != SecretKindNone }

// SaveIntegrationConfig writes a connection.
//
// The ciphertext is written only when a new one is supplied. An administrator
// editing the API base URL of a working connection should not have to paste
// their private key again to do it, and passing an empty secret through to the
// column would silently disconnect them.
func (db *DB) SaveIntegrationConfig(ctx context.Context, cfg IntegrationConfig, ciphertext, configuredBy string) error {
	now := time.Now().UTC()

	var by any
	if configuredBy != "" {
		by = configuredBy
	}

	// Insert and update are spelled separately rather than through one upsert
	// because they disagree about the secret: an insert always writes what it
	// was given, an update leaves the stored one alone when none was supplied.
	_, err := db.ExecContext(ctx, db.rebind(`
		INSERT INTO integrations (
			kind, provider, detail, status, created_at, updated_at,
			enabled, app_id, installation_id, api_base_url,
			secret_ciphertext, secret_kind, configured_at, configured_by
		) VALUES (?, ?, '', 'unknown', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind) DO UPDATE SET
			provider        = excluded.provider,
			enabled         = excluded.enabled,
			app_id          = excluded.app_id,
			installation_id = excluded.installation_id,
			api_base_url    = excluded.api_base_url,
			secret_ciphertext = CASE WHEN excluded.secret_ciphertext = ''
			                         THEN integrations.secret_ciphertext
			                         ELSE excluded.secret_ciphertext END,
			secret_kind       = CASE WHEN excluded.secret_ciphertext = ''
			                         THEN integrations.secret_kind
			                         ELSE excluded.secret_kind END,
			configured_at   = excluded.configured_at,
			configured_by   = excluded.configured_by,
			updated_at      = excluded.updated_at`),
		cfg.Kind, cfg.Provider, now, now,
		cfg.Enabled, cfg.AppID, cfg.InstallationID, cfg.APIBaseURL,
		ciphertext, cfg.SecretKind, now, by)
	if err != nil {
		return fmt.Errorf("store: save integration %s: %w", cfg.Kind, err)
	}
	return nil
}

// IntegrationConfigs returns every connection an administrator has set up.
func (db *DB) IntegrationConfigs(ctx context.Context) ([]IntegrationConfig, error) {
	rows, err := db.QueryContext(ctx, db.rebind(`
		SELECT kind, provider, enabled, app_id, installation_id, api_base_url,
		       secret_kind, detail, configured_at, coalesce(configured_by, '')
		FROM integrations
		WHERE provider <> ''
		ORDER BY kind`))
	if err != nil {
		return nil, fmt.Errorf("store: read integration configs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []IntegrationConfig
	for rows.Next() {
		var (
			c            IntegrationConfig
			detail       string
			configuredAt sql.NullTime
		)
		if err := rows.Scan(&c.Kind, &c.Provider, &c.Enabled, &c.AppID, &c.InstallationID,
			&c.APIBaseURL, &c.SecretKind, &detail, &configuredAt, &c.ConfiguredBy); err != nil {
			return nil, fmt.Errorf("store: scan integration config: %w", err)
		}
		// A mock keeps its fixtures path in detail, which is what it reads and
		// what the console shows. It is a path, not a credential.
		if c.Provider == "mock" {
			c.Fixtures = detail
		}
		if configuredAt.Valid {
			c.ConfiguredAt = &configuredAt.Time
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read integration configs: %w", err)
	}
	return out, nil
}

// IntegrationSecret returns the stored ciphertext for one connection.
//
// Separate from IntegrationConfigs so that the common path - rendering the
// console, listing what is connected - cannot accidentally carry a credential
// into a response. Only the code that builds a source calls this.
func (db *DB) IntegrationSecret(ctx context.Context, kind string) (string, error) {
	var ciphertext string
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT secret_ciphertext FROM integrations WHERE kind = ?`), kind).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read integration secret: %w", err)
	}
	return ciphertext, nil
}

// DisconnectIntegration removes a connection and the credential with it.
//
// The row is deleted rather than disabled. A disabled row would keep an
// encrypted credential in the database for a connection nobody intends to use,
// which is the thing this design exists to avoid - and the health history it
// preserved is history about a connection that no longer exists.
func (db *DB) DisconnectIntegration(ctx context.Context, kind string) error {
	_, err := db.ExecContext(ctx, db.rebind(`DELETE FROM integrations WHERE kind = ?`), kind)
	if err != nil {
		return fmt.Errorf("store: disconnect integration %s: %w", kind, err)
	}
	return nil
}

// AnyIntegrationConfigured reports whether an administrator has connected
// anything.
//
// Deliberately a count rather than a resolved registry. "Is an integration
// connected" is a question about rows, and answering it by building sources
// would decrypt a credential to compute a boolean - on the outcomes path, which
// serves a screen every few seconds.
func (db *DB) AnyIntegrationConfigured(ctx context.Context) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, db.rebind(
		`SELECT count(*) FROM integrations WHERE provider <> '' AND enabled = ?`), true).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: count integrations: %w", err)
	}
	return n > 0, nil
}
