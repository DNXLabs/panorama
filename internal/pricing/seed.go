// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// seedJSON is the rate table compiled into the binary.
//
//go:embed seed.json
var seedJSON []byte

// seedFile is the on-disk shape of the seed.
//
// Rates are decimal strings rather than integers so the file can be reviewed
// against the published price list by reading it. "2.20" is checkable;
// 2200000000 is a number someone has to trust. They are parsed digit by digit,
// so the exactness costs nothing.
type seedFile struct {
	// Note explains to a reader of the file what they are looking at.
	Note string `json:"note"`
	// CapturedAt is when these rates were read from AWS.
	CapturedAt string `json:"captured_at"`
	// Region is the catalogue they were read from.
	Region  string      `json:"region"`
	Entries []seedEntry `json:"entries"`
}

type seedEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Scope    string `json:"scope"`
	DecimalRates
	Tiers []DecimalTier `json:"tiers,omitempty"`
	// AsOf is the date AWS published the price as effective.
	AsOf string `json:"as_of"`
}

// DecimalRates are one set of rates as a file states them: US dollars per
// million tokens, as decimal text, and an empty string for a class with no
// published rate.
type DecimalRates struct {
	Input        string `json:"input"`
	Output       string `json:"output"`
	CacheRead    string `json:"cache_read,omitempty"`
	CacheWrite   string `json:"cache_write,omitempty"`
	CacheWrite1h string `json:"cache_write_1h,omitempty"`
}

// DecimalTier is a Tier as a file states it.
type DecimalTier struct {
	AboveInputTokens int64 `json:"above_input_tokens"`
	DecimalRates
}

// Decimal renders rates in the form a file states them: the shortest decimal
// that is exact, with at least cents - "0.22", "0.275", "13.20" - so a file
// reads like the price list it was copied from.
func (r Rates) Decimal() DecimalRates {
	text := func(rate Rate) string {
		if rate == 0 {
			return ""
		}
		s := rate.USDPerMillion()
		whole, frac, found := strings.Cut(s, ".")
		if !found {
			return s + ".00"
		}
		frac = strings.TrimRight(frac, "0")
		for len(frac) < 2 {
			frac += "0"
		}
		return whole + "." + frac
	}
	return DecimalRates{
		Input: text(r.Input), Output: text(r.Output), CacheRead: text(r.CacheRead),
		CacheWrite: text(r.CacheWrite), CacheWrite1h: text(r.CacheWrite1h),
	}
}

// DecimalTiers renders tiers in the form a file states them.
func DecimalTiers(tiers []Tier) []DecimalTier {
	if len(tiers) == 0 {
		return nil
	}
	out := make([]DecimalTier, len(tiers))
	for i, t := range tiers {
		out[i] = DecimalTier{AboveInputTokens: t.AboveInputTokens, DecimalRates: t.Rates.Decimal()}
	}
	return out
}

// Parse reads decimal rates.
func (d DecimalRates) Parse() (Rates, error) {
	var out Rates
	for _, field := range []struct {
		name  string
		value string
		dst   *Rate
	}{
		{"input", d.Input, &out.Input},
		{"output", d.Output, &out.Output},
		{"cache_read", d.CacheRead, &out.CacheRead},
		{"cache_write", d.CacheWrite, &out.CacheWrite},
		{"cache_write_1h", d.CacheWrite1h, &out.CacheWrite1h},
	} {
		if field.value == "" {
			continue
		}
		rate, err := ParseUSDPerMillion(field.value)
		if err != nil {
			return Rates{}, fmt.Errorf("%s: %w", field.name, err)
		}
		*field.dst = rate
	}
	return out, nil
}

// ParseDecimalTiers reads tiers as a file states them.
func ParseDecimalTiers(tiers []DecimalTier) ([]Tier, error) {
	if len(tiers) == 0 {
		return nil, nil
	}
	out := make([]Tier, len(tiers))
	for i, t := range tiers {
		rates, err := t.Parse()
		if err != nil {
			return nil, fmt.Errorf("tier above %d tokens: %w", t.AboveInputTokens, err)
		}
		out[i] = Tier{AboveInputTokens: t.AboveInputTokens, Rates: rates}
	}
	return out, nil
}

var (
	seedOnce    sync.Once
	seedEntries []Entry
	seedErr     error
)

// Seed returns the rates compiled into the binary.
//
// They exist so a fresh deployment prices its first request correctly, before
// it has ever reached the AWS pricing API - and so a deployment that can never
// reach it, because it is air-gapped or lacks the IAM permission, still prices
// requests at all. Every row is marked SourceSeed and is outranked by both a
// fetched rate and a configured one, so a live deployment moves off these
// values within a day and never moves back.
//
// They are a snapshot, not a source of truth. Rates published after the
// snapshot are not here, which is why an unpriced model is reported loudly
// rather than guessed at from a similar one.
//
// That is seed.json and the price finder's table together (see found.go),
// with the seed winning any model both price.
func Seed() ([]Entry, error) {
	seedOnce.Do(func() {
		seed, err := parseSeed(seedJSON)
		if err != nil {
			seedErr = err
			return
		}
		table, err := ParseFoundTable(foundJSON)
		if err != nil {
			seedErr = err
			return
		}
		found, err := table.PriceEntries()
		if err != nil {
			seedErr = err
			return
		}
		seedEntries = compiled(seed, found)
	})
	return seedEntries, seedErr
}

func parseSeed(raw []byte) ([]Entry, error) {
	var file seedFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("pricing: parse seed rates: %w", err)
	}

	captured, err := time.Parse(time.RFC3339, file.CapturedAt)
	if err != nil {
		return nil, fmt.Errorf("pricing: seed captured_at: %w", err)
	}

	out := make([]Entry, 0, len(file.Entries))
	for _, s := range file.Entries {
		rates, rerr := s.Parse()
		if rerr != nil {
			return nil, fmt.Errorf("pricing: seed rate for %s: %w", s.Model, rerr)
		}
		tiers, terr := ParseDecimalTiers(s.Tiers)
		if terr == nil {
			terr = requireTierRates(tiers)
		}
		if terr != nil {
			return nil, fmt.Errorf("pricing: seed rate for %s: %w", s.Model, terr)
		}

		asOf := captured
		if s.AsOf != "" {
			if parsed, perr := time.Parse(time.RFC3339, s.AsOf); perr == nil {
				asOf = parsed
			}
		}

		entry := Entry{
			Key:       Key{Provider: s.Provider, Model: s.Model, Scope: Scope(s.Scope)},
			Rates:     rates,
			Tiers:     tiers,
			Currency:  CurrencyUSD,
			Source:    SourceSeed,
			Region:    file.Region,
			AsOf:      asOf,
			UpdatedAt: captured,
		}
		if verr := entry.Validate(); verr != nil {
			return nil, fmt.Errorf("pricing: seed: %w", verr)
		}
		out = append(out, entry)
	}
	return out, nil
}
