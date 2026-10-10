// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"errors"
	"strings"
	"testing"
)

// sonnet5Regional is what AWS publishes for Claude Sonnet 5 through a `us.`
// inference profile, in dollars per million tokens.
var sonnet5Regional = Rates{
	Input:        2_200_000_000,
	Output:       11_000_000_000,
	CacheRead:    220_000_000,
	CacheWrite:   2_750_000_000,
	CacheWrite1h: 4_400_000_000,
}

// sonnet5Global is the same model through the `global.` profile, 10% cheaper.
var sonnet5Global = Rates{
	Input:        2_000_000_000,
	Output:       10_000_000_000,
	CacheRead:    200_000_000,
	CacheWrite:   2_500_000_000,
	CacheWrite1h: 4_000_000_000,
}

func TestParseModel(t *testing.T) {
	cases := []struct {
		in    string
		model string
		scope Scope
	}{
		{"us.anthropic.claude-sonnet-5", "anthropic.claude-sonnet-5", ScopeRegional},
		{"global.anthropic.claude-sonnet-5", "anthropic.claude-sonnet-5", ScopeGlobal},
		{"eu.anthropic.claude-sonnet-5", "anthropic.claude-sonnet-5", ScopeRegional},
		{"apac.anthropic.claude-sonnet-5", "anthropic.claude-sonnet-5", ScopeRegional},
		{"us-gov.anthropic.claude-sonnet-5", "anthropic.claude-sonnet-5", ScopeRegional},
		// A bare foundation-model id bills in its own region.
		{"anthropic.claude-3-haiku-20240307-v1:0", "anthropic.claude-3-haiku-20240307-v1:0", ScopeRegional},
		// An ARN is reduced to the id, because a client may be configured with
		// either form.
		{
			"arn:aws:bedrock:us-east-1:123456789012:inference-profile/global.anthropic.claude-opus-5",
			"anthropic.claude-opus-5", ScopeGlobal,
		},
		// A prefix this build does not know stays put, so no price matches and
		// the request is recorded unpriced rather than costed as another model.
		{"zz.anthropic.claude-sonnet-5", "zz.anthropic.claude-sonnet-5", ScopeRegional},
		{"", "", ScopeRegional},
	}

	for _, c := range cases {
		model, scope := ParseModel(c.in)
		if model != c.model || scope != c.scope {
			t.Errorf("ParseModel(%q) = (%q, %s), want (%q, %s)", c.in, model, scope, c.model, c.scope)
		}
	}
}

// TestScopeChangesTheBill is the whole reason the profile prefix survives into
// the price-table key. Stripping it as noise would misprice every request by
// the difference between these two numbers.
func TestScopeChangesTheBill(t *testing.T) {
	usage := Usage{InputTokens: 1_000_000, OutputTokens: 100_000}

	regional, err := Compute(usage, sonnet5Regional)
	if err != nil {
		t.Fatal(err)
	}
	global, err := Compute(usage, sonnet5Global)
	if err != nil {
		t.Fatal(err)
	}

	if regional.Total.USD() != "3.300000000" {
		t.Errorf("regional total = %s, want 3.300000000", regional.Total.USD())
	}
	if global.Total.USD() != "3.000000000" {
		t.Errorf("global total = %s, want 3.000000000", global.Total.USD())
	}
	if regional.Total <= global.Total {
		t.Error("the regional profile must not be cheaper than the global one for this model")
	}
}

func TestComputeSplitsCacheWritesByTTL(t *testing.T) {
	// 100k cache writes, 40k of them with the one-hour TTL.
	usage := Usage{CacheWriteTokens: 100_000, CacheWrite1hTokens: 40_000}

	cost, err := Compute(usage, sonnet5Regional)
	if err != nil {
		t.Fatal(err)
	}

	// 60k at $2.75/M = $0.165, 40k at $4.40/M = $0.176.
	if cost.CacheWrite.USD() != "0.341000000" {
		t.Errorf("cache write cost = %s, want 0.341000000", cost.CacheWrite.USD())
	}

	// Priced entirely at the five-minute rate it would be $0.275 - a 24%
	// understatement - which is what a single cache-write column would produce.
	flat, err := sonnet5Regional.CacheWrite.Cost(100_000)
	if err != nil {
		t.Fatal(err)
	}
	if flat >= cost.CacheWrite {
		t.Errorf("the TTL split should cost more than a flat five-minute rate: %s vs %s", cost.CacheWrite, flat)
	}
}

func TestComputeRejectsImpossibleUsage(t *testing.T) {
	usage := Usage{CacheWriteTokens: 10, CacheWrite1hTokens: 40}

	if _, err := Compute(usage, sonnet5Regional); !errors.Is(err, ErrInconsistentUsage) {
		t.Errorf("error = %v, want ErrInconsistentUsage", err)
	}
}

// TestComputeRefusesToPriceAnUnknownClassAtZero is the honesty rule in code: a
// token class with no published rate makes the whole request unpriced, rather
// than contributing nothing to the total and looking like a discount.
func TestComputeRefusesToPriceAnUnknownClassAtZero(t *testing.T) {
	// A model with no cache pricing, as several retired ones genuinely have.
	rates := Rates{Input: 250_000_000, Output: 1_250_000_000}

	// Plain input and output still price.
	if _, err := Compute(Usage{InputTokens: 1000, OutputTokens: 100}, rates); err != nil {
		t.Fatalf("input and output should price: %v", err)
	}

	// Cache reads against a model with no cache-read rate must not silently
	// cost nothing.
	_, err := Compute(Usage{InputTokens: 1000, CacheReadTokens: 5000}, rates)
	if !errors.Is(err, ErrRateMissing) {
		t.Fatalf("error = %v, want ErrRateMissing", err)
	}
	if !strings.Contains(err.Error(), "cache_read") {
		t.Errorf("the error must name the class that has no rate, got %q", err)
	}
}

func TestComputeIgnoresUnusedClasses(t *testing.T) {
	// A model with no one-hour cache rate is fine as long as nothing used it.
	rates := sonnet5Regional
	rates.CacheWrite1h = 0

	if _, err := Compute(Usage{CacheWriteTokens: 1000}, rates); err != nil {
		t.Errorf("a class with no tokens must not require a rate: %v", err)
	}
	if _, err := Compute(Usage{CacheWriteTokens: 1000, CacheWrite1hTokens: 10}, rates); !errors.Is(err, ErrRateMissing) {
		t.Errorf("error = %v, want ErrRateMissing once the class is used", err)
	}
}

func TestComputeTotalsTheClasses(t *testing.T) {
	usage := Usage{
		InputTokens: 13, OutputTokens: 4,
		CacheReadTokens: 75_069, CacheWriteTokens: 174,
	}

	cost, err := Compute(usage, sonnet5Regional)
	if err != nil {
		t.Fatal(err)
	}

	if sum := cost.Input + cost.Output + cost.CacheRead + cost.CacheWrite; sum != cost.Total {
		t.Errorf("total %s is not the sum of the classes %s", cost.Total, sum)
	}
	// Cache reads dominate a coding-agent turn; that is the shape the whole
	// product is built around.
	if cost.CacheRead <= cost.Input {
		t.Errorf("expected cache reads to dominate this turn: read=%s input=%s", cost.CacheRead, cost.Input)
	}
}

func TestTableLookupUsesTheProfilePrefix(t *testing.T) {
	table := NewTable([]Entry{
		{
			Key:      Key{Provider: "bedrock", Model: "anthropic.claude-sonnet-5", Scope: ScopeRegional},
			Rates:    sonnet5Regional,
			Currency: CurrencyUSD, Source: SourceSeed,
		},
		{
			Key:      Key{Provider: "bedrock", Model: "anthropic.claude-sonnet-5", Scope: ScopeGlobal},
			Rates:    sonnet5Global,
			Currency: CurrencyUSD, Source: SourceSeed,
		},
	})

	regional, ok := table.Lookup("bedrock", "us.anthropic.claude-sonnet-5")
	if !ok {
		t.Fatal("us. profile did not resolve")
	}
	if regional.Rates != sonnet5Regional {
		t.Error("us. profile resolved to the wrong rate set")
	}

	global, ok := table.Lookup("bedrock", "global.anthropic.claude-sonnet-5")
	if !ok {
		t.Fatal("global. profile did not resolve")
	}
	if global.Rates != sonnet5Global {
		t.Error("global. profile resolved to the wrong rate set")
	}

	if _, ok := table.Lookup("bedrock", "us.anthropic.claude-nonexistent"); ok {
		t.Error("an unknown model must not resolve to anything")
	}
	if _, ok := table.Lookup("openai", "us.anthropic.claude-sonnet-5"); ok {
		t.Error("a model must not resolve under another provider")
	}
}

func TestNilTableIsUsable(t *testing.T) {
	// A deployment with no prices records unpriced requests rather than
	// crashing on the first one.
	var table *Table

	if _, ok := table.Lookup("bedrock", "us.anthropic.claude-sonnet-5"); ok {
		t.Error("a nil table reported a price")
	}
	if table.Len() != 0 || table.All() != nil {
		t.Error("a nil table should be empty")
	}
	if _, _, err := table.Price("bedrock", "m", Usage{InputTokens: 1}); !errors.Is(err, ErrRateMissing) {
		t.Errorf("error = %v, want ErrRateMissing", err)
	}
}

func TestTableReplaceIsAtomic(t *testing.T) {
	table := NewTable([]Entry{{
		Key:      Key{Provider: "bedrock", Model: "m", Scope: ScopeRegional},
		Rates:    Rates{Input: 1},
		Currency: CurrencyUSD, Source: SourceSeed,
	}})

	table.Replace([]Entry{{
		Key:      Key{Provider: "bedrock", Model: "m", Scope: ScopeRegional},
		Rates:    Rates{Input: 2},
		Currency: CurrencyUSD, Source: SourceAWS,
	}})

	got, ok := table.Lookup("bedrock", "m")
	if !ok {
		t.Fatal("model disappeared after replace")
	}
	if got.Rates.Input != 2 || got.Source != SourceAWS {
		t.Errorf("replace did not take effect: %+v", got)
	}
	if table.Len() != 1 {
		t.Errorf("Len() = %d, want 1", table.Len())
	}
}

func TestEntryValidate(t *testing.T) {
	valid := Entry{
		Key:      Key{Provider: "bedrock", Model: "anthropic.x", Scope: ScopeRegional},
		Rates:    Rates{Input: 1},
		Currency: CurrencyUSD,
		Source:   SourceSeed,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid entry was rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Entry){
		"no provider":     func(e *Entry) { e.Provider = "" },
		"no model":        func(e *Entry) { e.Model = "" },
		"unknown scope":   func(e *Entry) { e.Scope = "planetary" },
		"other currency":  func(e *Entry) { e.Currency = "AUD" },
		"no rates at all": func(e *Entry) { e.Rates = Rates{} },
		"negative rate":   func(e *Entry) { e.Rates.Output = -1 },
	} {
		bad := valid
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
}
