// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"strings"
	"testing"
	"time"
)

// TestSeedIsUsable checks the compiled-in rates, which are the prices a
// deployment charges against until its first refresh. A bad row here is a
// wrong bill on day one.
func TestSeedIsUsable(t *testing.T) {
	entries, err := Seed()
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the seed is empty; a fresh deployment would price nothing")
	}

	seen := map[Key]bool{}
	for _, e := range entries {
		if verr := e.Validate(); verr != nil {
			t.Errorf("invalid seed entry: %v", verr)
		}
		if seen[e.Key] {
			t.Errorf("%s appears twice; one of the two silently wins", e.Key)
		}
		seen[e.Key] = true

		if e.Source != SourceSeed {
			t.Errorf("%s is marked %q; every seed row must be outranked by a fetched rate", e.Key, e.Source)
		}
		if e.Rates.Input <= 0 || e.Rates.Output <= 0 {
			t.Errorf("%s has no input or output rate", e.Key)
		}
		if e.Rates.Output <= e.Rates.Input {
			t.Errorf("%s prices output at or below input, which no model does: %+v", e.Key, e.Rates)
		}
		// A cache read costing as much as fresh input would mean caching saves
		// nothing, which would make the headline feature meaningless. It is
		// always a fraction.
		if e.Rates.CacheRead > 0 && e.Rates.CacheRead >= e.Rates.Input {
			t.Errorf("%s prices cache reads at or above input: %+v", e.Key, e.Rates)
		}
		if e.Rates.CacheWrite > 0 && e.Rates.CacheWrite <= e.Rates.Input {
			t.Errorf("%s prices cache writes at or below input, but a write costs a premium: %+v", e.Key, e.Rates)
		}
		if e.Rates.CacheWrite1h > 0 && e.Rates.CacheWrite1h <= e.Rates.CacheWrite {
			t.Errorf("%s prices the one-hour cache at or below the five-minute one: %+v", e.Key, e.Rates)
		}
	}
}

// TestSeedPricesTheModelsClaudeCodeUses is the practical check: a deployment
// pointing Claude Code at Bedrock should get costed requests without waiting
// for a refresh or being granted a pricing permission.
func TestSeedPricesTheModelsClaudeCodeUses(t *testing.T) {
	entries, err := Seed()
	if err != nil {
		t.Fatal(err)
	}
	table := NewTable(entries)

	// The inference-profile ids an administrator actually routes to.
	for _, modelID := range []string{
		"us.anthropic.claude-sonnet-5",
		"global.anthropic.claude-sonnet-5",
		"us.anthropic.claude-haiku-4-5-20251001-v1:0",
		"global.anthropic.claude-haiku-4-5-20251001-v1:0",
		"us.anthropic.claude-opus-5",
		"us.anthropic.claude-sonnet-4-6",
	} {
		entry, ok := table.Lookup("bedrock", modelID)
		if !ok {
			t.Errorf("%s is not in the seed; its requests would be unpriced on a fresh deployment", modelID)
			continue
		}

		// A full coding-agent turn touches every class, so all five rates have
		// to be present or the request records unpriced.
		usage := Usage{
			InputTokens: 100, OutputTokens: 50,
			CacheReadTokens: 10_000, CacheWriteTokens: 1_000, CacheWrite1hTokens: 100,
		}
		if _, cerr := Compute(usage, entry.Rates); cerr != nil {
			t.Errorf("%s cannot price a normal coding-agent turn: %v", modelID, cerr)
		}
	}
}

func TestSeedRecordsWhenItWasCaptured(t *testing.T) {
	entries, err := Seed()
	if err != nil {
		t.Fatal(err)
	}

	first := entries[0]
	if first.UpdatedAt.IsZero() {
		t.Errorf("%s has no capture date; staleness would be invisible", first.Key)
	}
	if first.Region == "" {
		t.Errorf("%s does not say which catalogue it came from", first.Key)
	}
	if first.UpdatedAt.After(time.Now().Add(24 * time.Hour)) {
		t.Errorf("%s was captured in the future: %s", first.Key, first.UpdatedAt)
	}
}

func TestParseSeedRejectsBadData(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":     `{`,
		"bad date":     `{"captured_at":"yesterday","region":"us-east-1","entries":[]}`,
		"bad rate":     `{"captured_at":"2026-08-14T00:00:00Z","region":"r","entries":[{"provider":"bedrock","model":"m","scope":"regional","input":"free","output":"1"}]}`,
		"bad scope":    `{"captured_at":"2026-08-14T00:00:00Z","region":"r","entries":[{"provider":"bedrock","model":"m","scope":"orbital","input":"1","output":"2"}]}`,
		"no rates":     `{"captured_at":"2026-08-14T00:00:00Z","region":"r","entries":[{"provider":"bedrock","model":"m","scope":"regional"}]}`,
		"no provider":  `{"captured_at":"2026-08-14T00:00:00Z","region":"r","entries":[{"model":"m","scope":"regional","input":"1"}]}`,
		"empty string": ``,
	} {
		if _, err := parseSeed([]byte(raw)); err == nil {
			t.Errorf("%s: parseSeed succeeded, want an error", name)
		}
	}
}

// TestSeedFileIsReviewable checks the property that makes the checked-in rates
// auditable: they are written as decimals a person can compare against the
// published price list, not as pre-scaled integers nobody can check.
func TestSeedFileIsReviewable(t *testing.T) {
	if !strings.Contains(string(seedJSON), `"input": "`) {
		t.Error("seed rates are not decimal strings; the file would not be reviewable")
	}
	if strings.Contains(string(seedJSON), `"input": 2`) {
		t.Error("seed rates appear to be JSON numbers, which lose exactness when parsed")
	}
}

// TestSeedCarriesTheScopeSplit holds the finding the price-table key is built
// on: a model is published at two prices, and which applies is decided by the
// inference-profile prefix.
//
// It reads the shipped table rather than the Price List API, because a live
// check of this needs a region whose catalogue publishes both
// scopes for one model and the deployment's own region does not. So this is
// what would notice a seed regenerated flat, or a parser that dropped the scope
// on the way in - after which `us.` and `global.` would silently cost the same
// and the dimension in Key would be carrying nothing.
//
// What it deliberately cannot notice is AWS changing its mind. That needs a
// live fetch in a region that pairs, and is recorded as unverified in
// TestFetchAgainstLiveAWS rather than pretended at here.
func TestSeedCarriesTheScopeSplit(t *testing.T) {
	entries, err := Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	byKey := make(map[Key]Entry, len(entries))
	for _, e := range entries {
		byKey[e.Key] = e
	}

	var paired, differing int
	for key, regional := range byKey {
		if key.Scope != ScopeRegional {
			continue
		}
		global, ok := byKey[Key{Provider: key.Provider, Model: key.Model, Scope: ScopeGlobal}]
		if !ok {
			continue
		}
		paired++

		// Not every model differs - Sonnet 4 charges the same either way - so
		// the invariant is that global never costs more, not that it always
		// costs less.
		if global.Rates.Input > regional.Rates.Input {
			t.Errorf("%s: global input %s exceeds regional %s; the cheaper scope is not the cheaper one",
				key.Model, global.Rates.Input, regional.Rates.Input)
		}
		if global.Rates.Input != regional.Rates.Input {
			differing++
		}
	}

	if paired == 0 {
		t.Fatal("no model in the seed carries both scopes; the prefix could not be a price input")
	}
	if differing == 0 {
		t.Errorf("all %d scope pairs are identically priced; the scope in Key would be dead weight", paired)
	}
	t.Logf("%d scope pairs in the seed, %d of them priced differently", paired, differing)
}
