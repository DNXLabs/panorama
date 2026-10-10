// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/websearch"
)

// WebSearchSettings is how web search is configured: which backend, whether it
// is on, and where it is.
type WebSearchSettings struct {
	Kind       websearch.Kind
	Enabled    bool
	GatewayURL string
	// Price is what one search costs, or nil for the rate this build carries
	// (websearch.PublishedPrice).
	Price *pricing.Money

	VerifiedAt   *time.Time
	ConfiguredAt time.Time
	ConfiguredBy string

	// Deployed is true when the gateway is the deployment's own, named in its
	// configuration. The gateway and the price are then not the console's to
	// change, and Enabled is the one thing the row decides.
	Deployed bool
}

// PricePerSearch is the rate a search is charged at, and where it came from.
func (s WebSearchSettings) PricePerSearch() (pricing.Money, string) {
	if s.Price != nil {
		return *s.Price, "config"
	}
	return websearch.PublishedPrice, "aws-published"
}

// Config is the settings as the web search service reads them.
func (s WebSearchSettings) Config() websearch.Config {
	price, source := s.PricePerSearch()
	return websearch.Config{
		Kind: s.Kind, Enabled: s.Enabled, GatewayURL: s.GatewayURL,
		Price: price, PriceSource: source,
	}
}

// WebSearch returns the configured backend, and whether there is one.
//
// A row naming a kind this build does not know is an error rather than no
// configuration, for the reason ListProviders gives: the CHECK constraint
// makes one unreachable by this build, so finding one means somebody else
// wrote it, and serving around it would ignore what they configured.
func (db *DB) WebSearch(ctx context.Context) (WebSearchSettings, bool, error) {
	rows, err := db.QueryContext(ctx, `
SELECT kind, enabled, gateway_url, price_nanos, verified_at, configured_at, configured_by
  FROM web_search`)
	if err != nil {
		return WebSearchSettings{}, false, fmt.Errorf("store: read web search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var found []WebSearchSettings
	for rows.Next() {
		var (
			s        WebSearchSettings
			kind     string
			price    sql.NullInt64
			verified sql.NullTime
		)
		if err := rows.Scan(&kind, &s.Enabled, &s.GatewayURL, &price, &verified, &s.ConfiguredAt, &s.ConfiguredBy); err != nil {
			return WebSearchSettings{}, false, fmt.Errorf("store: scan web search: %w", err)
		}
		s.Kind = websearch.Kind(kind)
		if !s.Kind.Valid() {
			return WebSearchSettings{}, false, fmt.Errorf("store: the web_search table names %q, which this build does not know", kind)
		}
		if price.Valid {
			m := pricing.Money(price.Int64)
			s.Price = &m
		}
		if verified.Valid {
			at := verified.Time.UTC()
			s.VerifiedAt = &at
		}
		s.ConfiguredAt = s.ConfiguredAt.UTC()
		found = append(found, s)
	}
	if err := rows.Err(); err != nil {
		return WebSearchSettings{}, false, fmt.Errorf("store: read web search: %w", err)
	}
	switch len(found) {
	case 0:
		return WebSearchSettings{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		// One kind exists today, so two rows cannot. When a second kind is
		// added, which one serves becomes a column rather than a row count.
		return WebSearchSettings{}, false, fmt.Errorf("store: %d web search backends are configured; this build serves one", len(found))
	}
}

// EffectiveWebSearch is the configuration web search actually runs with: the
// stored row, with a gateway the deployment named laid over it.
//
// One function, read by the service that builds the backend and by every
// console handler, because the screen and the endpoint deciding separately
// what is configured would be two answers to one question.
//
// A deployment's gateway wins over a stored one and is priced at the
// published rate, so a URL or price saved before the stack created its own
// gateway is ignored rather than served. Search is on until an administrator
// switches it off, which is the row's enabled flag; a verification counts only
// if it was of this gateway.
func (db *DB) EffectiveWebSearch(ctx context.Context, d websearch.Deployment) (WebSearchSettings, bool, error) {
	s, found, err := db.WebSearch(ctx)
	if err != nil || !d.Set() {
		return s, found, err
	}
	out := WebSearchSettings{Kind: d.Kind, Enabled: true, GatewayURL: d.GatewayURL, Deployed: true}
	if found {
		out.Enabled = s.Enabled
		out.ConfiguredAt, out.ConfiguredBy = s.ConfiguredAt, s.ConfiguredBy
		if s.Kind == d.Kind && s.GatewayURL == d.GatewayURL {
			out.VerifiedAt = s.VerifiedAt
		}
	}
	return out, true, nil
}

// SaveWebSearch writes the web search configuration.
//
// A changed gateway clears the verification in the same statement, because a
// check that reached one gateway says nothing about another.
func (db *DB) SaveWebSearch(ctx context.Context, s WebSearchSettings) error {
	if !s.Kind.Valid() {
		return fmt.Errorf("store: %w: %q is not a web search backend this build knows", ErrInvalidInput, s.Kind)
	}
	if s.GatewayURL == "" {
		return fmt.Errorf("store: %w: web search needs a gateway URL", ErrInvalidInput)
	}
	var price any
	if s.Price != nil {
		if *s.Price < 0 {
			return fmt.Errorf("store: %w: a price cannot be negative", ErrInvalidInput)
		}
		price = int64(*s.Price)
	}

	if _, err := db.ExecContext(ctx, db.rebind(`
INSERT INTO web_search (kind, enabled, gateway_url, price_nanos, verified_at, configured_at, configured_by)
VALUES (?, ?, ?, ?, NULL, ?, ?)
ON CONFLICT (kind) DO UPDATE SET
    enabled       = excluded.enabled,
    gateway_url   = excluded.gateway_url,
    price_nanos   = excluded.price_nanos,
    verified_at   = CASE WHEN web_search.gateway_url = excluded.gateway_url THEN web_search.verified_at ELSE NULL END,
    configured_at = excluded.configured_at,
    configured_by = excluded.configured_by`),
		string(s.Kind), s.Enabled, s.GatewayURL, price, time.Now().UTC(), s.ConfiguredBy); err != nil {
		return fmt.Errorf("store: save web search: %w", err)
	}
	return nil
}

// SetWebSearchVerified records that a check reached the gateway and found its
// tool. Conditional on the gateway still being the configured one, so a check
// that raced a change cannot mark the new gateway as verified by a call made
// to the old one.
func (db *DB) SetWebSearchVerified(ctx context.Context, kind websearch.Kind, gatewayURL string, at time.Time) error {
	if _, err := db.ExecContext(ctx, db.rebind(`
UPDATE web_search SET verified_at = ? WHERE kind = ? AND gateway_url = ?`),
		at.UTC(), string(kind), gatewayURL); err != nil {
		return fmt.Errorf("store: record web search verification: %w", err)
	}
	return nil
}

// ClearWebSearchVerified forgets a verification after a check failed.
func (db *DB) ClearWebSearchVerified(ctx context.Context, kind websearch.Kind) error {
	if _, err := db.ExecContext(ctx, db.rebind(`UPDATE web_search SET verified_at = NULL WHERE kind = ?`), string(kind)); err != nil {
		return fmt.Errorf("store: clear web search verification: %w", err)
	}
	return nil
}
