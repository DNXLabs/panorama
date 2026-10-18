// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/dnxlabs/panorama/internal/providers"
)

// ProviderFailure is the latest request a provider refused to serve, as
// Settings, Model providers shows it to an administrator.
type ProviderFailure struct {
	Kind      providers.Kind
	Region    string
	Model     string
	ErrorKind string
	// Message is the provider's own words (providers.Detail), which is the
	// whole point of keeping the row: the engineer who met the refusal was
	// told only that it happened.
	Message   string
	RequestID string
	At        time.Time
}

// maxFailureMessage bounds what is kept of the provider's text. AWS's refusals
// are a sentence or two; anything longer is an SDK wrapping that adds nothing.
const maxFailureMessage = 1000

// RecordProviderFailure replaces a provider's latest refusal.
//
// Called off the request path, by the data plane's recorder, for refusals
// only (providers.Refusal): their text is the provider's, and none of it is
// derived from a request body.
func (db *DB) RecordProviderFailure(ctx context.Context, f ProviderFailure) error {
	message := f.Message
	if len(message) > maxFailureMessage {
		message = message[:maxFailureMessage]
		// Cut on a rune boundary, so a Postgres TEXT column is never handed a
		// broken UTF-8 sequence.
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	if _, err := db.ExecContext(ctx, db.rebind(`
INSERT INTO provider_failures (kind, region, model, error_kind, message, request_id, failed_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (kind) DO UPDATE SET
	region = excluded.region, model = excluded.model, error_kind = excluded.error_kind,
	message = excluded.message, request_id = excluded.request_id, failed_at = excluded.failed_at`),
		string(f.Kind), f.Region, f.Model, f.ErrorKind, message, f.RequestID, f.At.UTC()); err != nil {
		return fmt.Errorf("store: record a refusal by %s: %w", f.Kind, err)
	}
	return nil
}

// ProviderFailures returns each provider's latest refusal, keyed by kind.
func (db *DB) ProviderFailures(ctx context.Context) (map[providers.Kind]ProviderFailure, error) {
	rows, err := db.QueryContext(ctx, `
SELECT kind, region, model, error_kind, message, request_id, failed_at FROM provider_failures`)
	if err != nil {
		return nil, fmt.Errorf("store: read provider refusals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[providers.Kind]ProviderFailure{}
	for rows.Next() {
		var (
			f    ProviderFailure
			kind string
		)
		if err := rows.Scan(&kind, &f.Region, &f.Model, &f.ErrorKind, &f.Message, &f.RequestID, &f.At); err != nil {
			return nil, fmt.Errorf("store: scan provider refusal: %w", err)
		}
		f.Kind = providers.Kind(kind)
		out[f.Kind] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read provider refusals: %w", err)
	}
	return out, nil
}

// ClearProviderFailure forgets a provider's latest refusal, when a live check
// has just invoked the model it was about in the region it happened in: the
// row answered "why is this failing", and that is no longer the answer. A
// check that passed on another model clears nothing, since the refused one
// may still be refused.
func (db *DB) ClearProviderFailure(ctx context.Context, kind providers.Kind, region, model string) error {
	if _, err := db.ExecContext(ctx, db.rebind(`
DELETE FROM provider_failures WHERE kind = ? AND region = ? AND model = ?`),
		string(kind), region, model); err != nil {
		return fmt.Errorf("store: clear the refusal recorded for %s: %w", kind, err)
	}
	return nil
}
