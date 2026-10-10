// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// foundJSON is the price finder's table, compiled into the binary beside the
// seed.
//
// The repository's copy is empty. A release replaces it with the table the
// Prices workflow keeps in S3 before it builds, which is how a model the Price
// List API does not publish - OpenAI's GPT-5.x is the case that started this -
// is priced by an air-gapped deployment, and by a connected one whose refresh
// can find nothing to fetch.
//
//go:embed found_prices.json
var foundJSON []byte

// FoundTable is the price finder's output and the file a release embeds.
//
// It only ever grows. The finder adds a row for a model nothing else prices and
// never rewrites one it added, so a price that is in a release stays the price
// that was reviewed into it; a row that has gone stale is removed from the
// table by hand, and the next run reads the model again.
type FoundTable struct {
	Note string `json:"note"`
	// GeneratedAt is when the finder last added to the table.
	GeneratedAt string       `json:"generated_at,omitempty"`
	Entries     []FoundEntry `json:"entries"`
}

// FoundEntry is one model's price as the finder found it, with what it was
// read from.
type FoundEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Scope    string `json:"scope"`
	DecimalRates
	Tiers []DecimalTier `json:"tiers,omitempty"`

	// Source is where the rate was read: SourceAWS for a snapshot of the Price
	// List API, taken because the seed did not carry the model, or
	// SourceModelCard for a model card read by a language model and checked
	// against the page.
	Source string `json:"source"`
	// Region is the Price List catalogue a snapshot was read from. Empty for a
	// model card, whose rates are stated for every commercial region at once.
	Region string `json:"region,omitempty"`
	// SourceURL is the page a model-card rate was read from.
	SourceURL string `json:"source_url,omitempty"`
	// Evidence is the text the rates were checked against: the table rows of
	// a model card, quoted exactly, or the usage types of a snapshot.
	Evidence []string `json:"evidence,omitempty"`
	FoundAt  string   `json:"found_at"`
	// FoundBy is the model that read a model card.
	FoundBy string `json:"found_by,omitempty"`
}

// Key is the price-table key this row prices.
func (f FoundEntry) Key() Key {
	return Key{Provider: f.Provider, Model: f.Model, Scope: Scope(f.Scope)}
}

// ParseFoundTable reads a found table.
func ParseFoundTable(raw []byte) (FoundTable, error) {
	var t FoundTable
	if err := json.Unmarshal(raw, &t); err != nil {
		return FoundTable{}, fmt.Errorf("pricing: parse found prices: %w", err)
	}
	return t, nil
}

// PriceEntries converts the table's rows into price-table entries, validating each
// as the table would.
func (t FoundTable) PriceEntries() ([]Entry, error) {
	out := make([]Entry, 0, len(t.Entries))
	for _, f := range t.Entries {
		e, err := f.entry()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (f FoundEntry) entry() (Entry, error) {
	rates, err := f.Parse()
	if err != nil {
		return Entry{}, fmt.Errorf("pricing: found price for %s: %w", f.Model, err)
	}
	tiers, err := ParseDecimalTiers(f.Tiers)
	if err != nil {
		return Entry{}, fmt.Errorf("pricing: found price for %s: %w", f.Model, err)
	}
	if err = requireTierRates(tiers); err != nil {
		return Entry{}, fmt.Errorf("pricing: found price for %s: %w", f.Model, err)
	}

	// A snapshot of the pricing API is what the seed is, so it ranks as the
	// seed does; a model card ranks below both.
	var source string
	switch f.Source {
	case SourceAWS:
		source = SourceSeed
	case SourceModelCard:
		source = SourceModelCard
	default:
		return Entry{}, fmt.Errorf("pricing: found price for %s has source %q; want %s or %s",
			f.Model, f.Source, SourceAWS, SourceModelCard)
	}

	found, err := time.Parse(time.RFC3339, f.FoundAt)
	if err != nil {
		return Entry{}, fmt.Errorf("pricing: found price for %s: found_at: %w", f.Model, err)
	}

	e := Entry{
		Key:       f.Key(),
		Rates:     rates,
		Tiers:     tiers,
		Currency:  CurrencyUSD,
		Source:    source,
		Region:    f.Region,
		AsOf:      found,
		UpdatedAt: found,
	}
	if err = e.Validate(); err != nil {
		return Entry{}, fmt.Errorf("pricing: found: %w", err)
	}
	return e, nil
}

// FoundTableDigest identifies the found table compiled into this binary: the
// first twelve hex digits of its SHA-256, which is what the release records
// beside the S3 object it came from.
func FoundTableDigest() string {
	sum := sha256.Sum256(foundJSON)
	return hex.EncodeToString(sum[:])[:12]
}

// compiled merges the seed and the found table: the found table adds models,
// and never replaces one the seed prices.
func compiled(seed, found []Entry) []Entry {
	have := make(map[Key]bool, len(seed))
	for _, e := range seed {
		have[e.Key] = true
	}
	out := append([]Entry(nil), seed...)
	for _, e := range found {
		if !have[e.Key] {
			out = append(out, e)
			have[e.Key] = true
		}
	}
	return out
}
