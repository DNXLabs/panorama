// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

func priceEntry(model string, scope pricing.Scope, source string, input pricing.Rate) pricing.Entry {
	return pricing.Entry{
		Key:       pricing.Key{Provider: "bedrock", Model: model, Scope: scope},
		Rates:     pricing.Rates{Input: input, Output: input * 5, CacheRead: input / 10, CacheWrite: input * 5 / 4},
		Currency:  pricing.CurrencyUSD,
		Source:    source,
		Region:    "us-east-1",
		AsOf:      time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
	}
}

func TestUpsertAndLoadPrices(t *testing.T) {
	eachDialect(t, testUpsertAndLoadPrices)
}

func testUpsertAndLoadPrices(t *testing.T, db *DB) {
	ctx := t.Context()

	want := priceEntry("anthropic.claude-sonnet-5", pricing.ScopeRegional, pricing.SourceAWS, 2_200_000_000)
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{want}); err != nil {
		t.Fatalf("UpsertPrices: %v", err)
	}

	entries, err := db.LoadPrices(ctx)
	if err != nil {
		t.Fatalf("LoadPrices: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("loaded %d prices, want 1", len(entries))
	}

	got := entries[0]
	if got.Key != want.Key {
		t.Errorf("key = %v, want %v", got.Key, want.Key)
	}
	if got.Rates != want.Rates {
		t.Errorf("rates = %+v, want %+v", got.Rates, want.Rates)
	}
	if got.Source != want.Source || got.Region != want.Region || got.Currency != want.Currency {
		t.Errorf("provenance = %s/%s/%s, want %s/%s/%s",
			got.Source, got.Region, got.Currency, want.Source, want.Region, want.Currency)
	}
	if !got.AsOf.Equal(want.AsOf) {
		t.Errorf("AsOf = %v, want %v", got.AsOf, want.AsOf)
	}
}

// TestPriceScopesAreSeparateRows is the schema half of the finding that a
// model's `us.` and `global.` rates differ: they must not collide on one key.
func TestPriceScopesAreSeparateRows(t *testing.T) {
	db := migratedSQLite(t)
	ctx := t.Context()

	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry("anthropic.claude-sonnet-5", pricing.ScopeRegional, pricing.SourceAWS, 2_200_000_000),
		priceEntry("anthropic.claude-sonnet-5", pricing.ScopeGlobal, pricing.SourceAWS, 2_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := db.LoadPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("stored %d rows, want 2 - one per scope", len(entries))
	}

	table := pricing.NewTable(entries)
	regional, _ := table.Lookup("bedrock", "us.anthropic.claude-sonnet-5")
	global, _ := table.Lookup("bedrock", "global.anthropic.claude-sonnet-5")
	if regional.Rates.Input == global.Rates.Input {
		t.Error("the two scopes collapsed onto one rate")
	}
}

// TestPriceSourcePrecedence is what keeps a restart from undoing a month of
// refreshes and an operator's negotiated rate from being overwritten daily.
func TestPriceSourcePrecedence(t *testing.T) {
	eachDialect(t, testPriceSourcePrecedence)
}

func testPriceSourcePrecedence(t *testing.T, db *DB) {
	ctx := t.Context()

	const model = "anthropic.claude-sonnet-5"
	rateOf := func(t *testing.T) (pricing.Rate, string) {
		t.Helper()
		entries, err := db.LoadPrices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected exactly one row, got %d", len(entries))
		}
		return entries[0].Rates.Input, entries[0].Source
	}

	// Seed first, as a fresh deployment does.
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceSeed, 1_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}

	// A fetch outranks the seed.
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceAWS, 2_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	if rate, source := rateOf(t); rate != 2_000_000_000 || source != pricing.SourceAWS {
		t.Fatalf("after a fetch: rate=%d source=%s, want the fetched rate", rate, source)
	}

	// The seed reapplied at the next restart must not win it back.
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceSeed, 1_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	if rate, source := rateOf(t); rate != 2_000_000_000 || source != pricing.SourceAWS {
		t.Errorf("the seed overwrote a fetched rate: rate=%d source=%s", rate, source)
	}

	// A configured rate outranks everything.
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceConfig, 3_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	if rate, source := rateOf(t); rate != 3_000_000_000 || source != pricing.SourceConfig {
		t.Fatalf("a configured rate did not win: rate=%d source=%s", rate, source)
	}

	// And the next daily fetch must not undo it.
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceAWS, 2_500_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	if rate, source := rateOf(t); rate != 3_000_000_000 || source != pricing.SourceConfig {
		t.Errorf("a fetch overwrote an operator's configured rate: rate=%d source=%s", rate, source)
	}
}

// TestFetchedRatesUpdateEachOther confirms precedence does not freeze the table:
// a later fetch must still move a fetched price.
func TestFetchedRatesUpdateEachOther(t *testing.T) {
	db := migratedSQLite(t)
	ctx := t.Context()

	const model = "anthropic.claude-sonnet-5"
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceAWS, 2_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceAWS, 2_200_000_000),
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := db.LoadPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Rates.Input != 2_200_000_000 {
		t.Errorf("input rate = %d, want the newer fetched value", entries[0].Rates.Input)
	}
}

func TestUpsertPricesRefusesInvalidRates(t *testing.T) {
	db := migratedSQLite(t)

	bad := priceEntry("anthropic.x", pricing.ScopeRegional, pricing.SourceSeed, 1)
	bad.Currency = "AUD"

	if _, err := db.UpsertPrices(t.Context(), []pricing.Entry{bad}); err == nil {
		t.Fatal("a non-USD rate must be refused rather than summed into a dollar total")
	}

	entries, err := db.LoadPrices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the rejected batch left %d rows behind; the write must be all or nothing", len(entries))
	}
}

// A long-context price survives the round trip through the tiers column, on
// both databases, with every class of every tier intact.
func TestPriceTiersRoundTrip(t *testing.T) {
	eachDialect(t, testPriceTiersRoundTrip)
}

func testPriceTiersRoundTrip(t *testing.T, db *DB) {
	ctx := t.Context()

	want := priceEntry("openai.gpt-5.6-luna", pricing.ScopeRegional, pricing.SourceModelCard, 220_000_000)
	want.Provider = "bedrock-mantle"
	want.Tiers = []pricing.Tier{
		{AboveInputTokens: 272_000, Rates: pricing.Rates{Input: 440_000_000, Output: 1_980_000_000, CacheRead: 44_000_000, CacheWrite: 550_000_000}},
		{AboveInputTokens: 800_000, Rates: pricing.Rates{Input: 880_000_000, Output: 3_960_000_000, CacheWrite1h: 1}},
	}
	plain := priceEntry("anthropic.claude-sonnet-5", pricing.ScopeRegional, pricing.SourceAWS, 2_200_000_000)
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{want, plain}); err != nil {
		t.Fatalf("UpsertPrices: %v", err)
	}

	entries, err := db.LoadPrices(ctx)
	if err != nil {
		t.Fatalf("LoadPrices: %v", err)
	}
	byModel := map[string]pricing.Entry{}
	for _, e := range entries {
		byModel[e.Model] = e
	}
	got := byModel["openai.gpt-5.6-luna"]
	if got.Source != pricing.SourceModelCard {
		t.Errorf("source = %q", got.Source)
	}
	if len(got.Tiers) != len(want.Tiers) {
		t.Fatalf("tiers = %+v, want %+v", got.Tiers, want.Tiers)
	}
	for i := range want.Tiers {
		if got.Tiers[i] != want.Tiers[i] {
			t.Errorf("tier %d = %+v, want %+v", i, got.Tiers[i], want.Tiers[i])
		}
	}
	if tiers := byModel["anthropic.claude-sonnet-5"].Tiers; tiers != nil {
		t.Errorf("a model with one price loaded tiers %+v", tiers)
	}
}

// A rate read from a model card ranks below the seed: the seed is a snapshot of
// the pricing API, which is the stronger claim about the same model.
func TestAModelCardRateRanksBelowTheSeed(t *testing.T) {
	eachDialect(t, testAModelCardRateRanksBelowTheSeed)
}

func testAModelCardRateRanksBelowTheSeed(t *testing.T, db *DB) {
	ctx := t.Context()
	const model = "openai.gpt-5.6-luna"

	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceSeed, 1_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry(model, pricing.ScopeRegional, pricing.SourceModelCard, 9_000_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := db.LoadPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0]; got.Source != pricing.SourceSeed || got.Rates.Input != 1_000_000_000 {
		t.Errorf("a model-card rate replaced the seed: %s at %d", got.Source, got.Rates.Input)
	}

	// And a fetched rate replaces a model-card one.
	if _, err = db.UpsertPrices(ctx, []pricing.Entry{
		priceEntry("openai.gpt-5.6-terra", pricing.ScopeRegional, pricing.SourceModelCard, 2_000_000_000),
		priceEntry("openai.gpt-5.6-terra", pricing.ScopeRegional, pricing.SourceAWS, 2_100_000_000),
	}); err != nil {
		t.Fatal(err)
	}
	entries, err = db.LoadPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Model == "openai.gpt-5.6-terra" && e.Source != pricing.SourceAWS {
			t.Errorf("a fetched rate did not replace a model-card one: %s", e.Source)
		}
	}
}
