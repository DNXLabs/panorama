// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// upsertPrice writes one rate, resolving conflicts by source precedence.
//
// The WHERE clause on the update is what makes the sources compose. An
// operator's configured rate outranks a fetched one, a fetched one outranks the
// seed compiled into the binary, the seed outranks a rate read from a model
// card, and a rate never moves down that order. Without
// it, the seed reapplied at every startup would overwrite this morning's fetched
// prices with whatever was true when the binary was built, and an operator's
// negotiated rate would survive exactly until the next refresh.
const upsertPrice = `
INSERT INTO price_table (
    provider, model, scope,
    input_rate, output_rate, cache_read_rate, cache_write_rate, cache_write_1h_rate, tiers,
    currency, source, region, as_of, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (provider, model, scope) DO UPDATE SET
    input_rate          = excluded.input_rate,
    output_rate         = excluded.output_rate,
    cache_read_rate     = excluded.cache_read_rate,
    cache_write_rate    = excluded.cache_write_rate,
    cache_write_1h_rate = excluded.cache_write_1h_rate,
    tiers               = excluded.tiers,
    currency            = excluded.currency,
    source              = excluded.source,
    region              = excluded.region,
    as_of               = excluded.as_of,
    updated_at          = excluded.updated_at
WHERE (CASE excluded.source
           WHEN 'config' THEN 4 WHEN 'aws-pricing-api' THEN 3 WHEN 'seed' THEN 2 ELSE 1 END)
   >= (CASE price_table.source
           WHEN 'config' THEN 4 WHEN 'aws-pricing-api' THEN 3 WHEN 'seed' THEN 2 ELSE 1 END)`

// UpsertPrices writes rates into the price table and reports how many rows
// changed.
//
// The whole set is written in one transaction: a refresh that fails halfway
// must not leave a model with this week's input rate and last month's cache
// rate, which would misprice every request against it without looking wrong.
func (db *DB) UpsertPrices(ctx context.Context, entries []pricing.Entry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin price upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, db.rebind(upsertPrice))
	if err != nil {
		return 0, fmt.Errorf("store: prepare price upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var changed int
	for _, e := range entries {
		if verr := e.Validate(); verr != nil {
			return 0, fmt.Errorf("store: refusing to store a price: %w", verr)
		}

		var asOf any
		if !e.AsOf.IsZero() {
			asOf = e.AsOf.UTC()
		}
		updatedAt := e.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = time.Now()
		}
		tiers, terr := encodeTiers(e.Tiers)
		if terr != nil {
			return 0, fmt.Errorf("store: encode tiers for %s: %w", e.Key, terr)
		}

		res, xerr := stmt.ExecContext(ctx,
			e.Provider, e.Model, string(e.Scope),
			int64(e.Rates.Input), int64(e.Rates.Output), int64(e.Rates.CacheRead),
			int64(e.Rates.CacheWrite), int64(e.Rates.CacheWrite1h), tiers,
			e.Currency, e.Source, e.Region, asOf, updatedAt.UTC())
		if xerr != nil {
			return 0, fmt.Errorf("store: upsert price %s: %w", e.Key, xerr)
		}
		if n, aerr := res.RowsAffected(); aerr == nil {
			changed += int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit price upsert: %w", err)
	}
	return changed, nil
}

// LoadPrices reads the whole price table. It is called at startup and after a
// refresh to rebuild the in-memory table the request path consults; nothing
// reads a price from the database per request.
func (db *DB) LoadPrices(ctx context.Context) ([]pricing.Entry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT provider, model, scope,
		       input_rate, output_rate, cache_read_rate, cache_write_rate, cache_write_1h_rate, tiers,
		       currency, source, region, as_of, updated_at
		FROM price_table
		ORDER BY provider, model, scope`)
	if err != nil {
		return nil, fmt.Errorf("store: load prices: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []pricing.Entry
	for rows.Next() {
		var (
			e     pricing.Entry
			scope string
			tiers string
			asOf  sql.NullTime
		)
		if serr := rows.Scan(&e.Provider, &e.Model, &scope,
			&e.Rates.Input, &e.Rates.Output, &e.Rates.CacheRead,
			&e.Rates.CacheWrite, &e.Rates.CacheWrite1h, &tiers,
			&e.Currency, &e.Source, &e.Region, &asOf, &e.UpdatedAt); serr != nil {
			return nil, fmt.Errorf("store: scan price: %w", serr)
		}
		e.Scope = pricing.Scope(scope)
		decoded, derr := decodeTiers(tiers)
		if derr != nil {
			return nil, fmt.Errorf("store: tiers for %s/%s: %w", e.Provider, e.Model, derr)
		}
		e.Tiers = decoded
		if asOf.Valid {
			e.AsOf = asOf.Time
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read prices: %w", err)
	}
	return out, nil
}

// storedTier is a tier as the tiers column holds it: rates in nanodollars per
// million tokens, like every other rate column in the table.
type storedTier struct {
	AboveInputTokens int64 `json:"above_input_tokens"`
	Input            int64 `json:"input,omitempty"`
	Output           int64 `json:"output,omitempty"`
	CacheRead        int64 `json:"cache_read,omitempty"`
	CacheWrite       int64 `json:"cache_write,omitempty"`
	CacheWrite1h     int64 `json:"cache_write_1h,omitempty"`
}

// encodeTiers writes tiers for the tiers column, and an empty string for a
// model with one price at every size.
func encodeTiers(tiers []pricing.Tier) (string, error) {
	if len(tiers) == 0 {
		return "", nil
	}
	out := make([]storedTier, len(tiers))
	for i, t := range tiers {
		out[i] = storedTier{
			AboveInputTokens: t.AboveInputTokens,
			Input:            int64(t.Rates.Input), Output: int64(t.Rates.Output),
			CacheRead: int64(t.Rates.CacheRead), CacheWrite: int64(t.Rates.CacheWrite),
			CacheWrite1h: int64(t.Rates.CacheWrite1h),
		}
	}
	raw, err := json.Marshal(out)
	return string(raw), err
}

// decodeTiers reads the tiers column.
func decodeTiers(raw string) ([]pricing.Tier, error) {
	if raw == "" {
		return nil, nil
	}
	var stored []storedTier
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, err
	}
	out := make([]pricing.Tier, len(stored))
	for i, t := range stored {
		out[i] = pricing.Tier{AboveInputTokens: t.AboveInputTokens, Rates: pricing.Rates{
			Input: pricing.Rate(t.Input), Output: pricing.Rate(t.Output),
			CacheRead: pricing.Rate(t.CacheRead), CacheWrite: pricing.Rate(t.CacheWrite),
			CacheWrite1h: pricing.Rate(t.CacheWrite1h),
		}}
	}
	return out, nil
}
