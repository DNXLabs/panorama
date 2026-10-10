// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"errors"
	"strings"
	"testing"
)

// GPT-5.6 Luna on bedrock-mantle, as its model card states it:
// In-Region, standard tier, with every class dearer above 272K input tokens.
func luna() Entry {
	return Entry{
		Key: Key{Provider: "bedrock-mantle", Model: "openai.gpt-5.6-luna", Scope: ScopeRegional},
		Rates: Rates{
			Input: mustRate("0.22"), Output: mustRate("1.32"),
			CacheRead: mustRate("0.022"), CacheWrite: mustRate("0.275"),
		},
		Tiers: []Tier{{AboveInputTokens: 272_000, Rates: Rates{
			Input: mustRate("0.44"), Output: mustRate("1.98"),
			CacheRead: mustRate("0.044"), CacheWrite: mustRate("0.55"),
		}}},
		Currency: CurrencyUSD,
		Source:   SourceModelCard,
	}
}

func mustRate(usd string) Rate {
	r, err := ParseUSDPerMillion(usd)
	if err != nil {
		panic(err)
	}
	return r
}

// Above the threshold the long-context price applies to the whole request,
// not to the tokens past it - "long-context rates apply to all input and
// output tokens, not just tokens above 272K".
func TestATierPricesTheWholeRequest(t *testing.T) {
	table := NewTable([]Entry{luna()})

	short := Usage{InputTokens: 100_000, OutputTokens: 1_000_000}
	cost, _, err := table.Price("bedrock-mantle", "openai.gpt-5.6-luna", short)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cost.Total, mustMoney("0.022")+mustMoney("1.32"); got != want {
		t.Errorf("short context = %s, want %s", got, want)
	}

	long := Usage{InputTokens: 300_000, OutputTokens: 1_000_000}
	cost, _, err = table.Price("bedrock-mantle", "openai.gpt-5.6-luna", long)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cost.Total, mustMoney("0.132")+mustMoney("1.98"); got != want {
		t.Errorf("long context = %s, want %s: every token at the long rate", got, want)
	}
}

// The threshold is the prompt's size, cache hits included. A long session
// that mostly reads the cache is still a long prompt, and counting only the
// uncached part would keep it on the short-context price.
func TestTheThresholdCountsTheWholePrompt(t *testing.T) {
	e := luna()
	for _, tc := range []struct {
		name  string
		usage Usage
		long  bool
	}{
		{"exactly at the threshold", Usage{InputTokens: 272_000}, false},
		{"one over", Usage{InputTokens: 272_001}, true},
		{"mostly cache reads", Usage{InputTokens: 2_000, CacheReadTokens: 290_000}, true},
		{"cache writes count", Usage{InputTokens: 2_000, CacheWriteTokens: 280_000}, true},
		{"output does not count", Usage{InputTokens: 1_000, OutputTokens: 400_000}, false},
	} {
		got := e.RatesFor(tc.usage)
		if long := got == e.Tiers[0].Rates; long != tc.long {
			t.Errorf("%s: long-context price = %v, want %v", tc.name, long, tc.long)
		}
	}
}

// With several tiers the highest one the prompt exceeds applies.
func TestTheHighestTierExceededApplies(t *testing.T) {
	e := entry("m", SourceSeed, 1)
	e.Tiers = []Tier{
		{AboveInputTokens: 200_000, Rates: Rates{Input: 2, Output: 10}},
		{AboveInputTokens: 500_000, Rates: Rates{Input: 3, Output: 15}},
	}
	for prompt, want := range map[int64]Rate{100_000: 1, 300_000: 2, 600_000: 3} {
		if got := e.RatesFor(Usage{InputTokens: prompt}).Input; got != want {
			t.Errorf("a %d-token prompt is priced at %d, want %d", prompt, got, want)
		}
	}
}

func TestTiersMustAscend(t *testing.T) {
	for name, tiers := range map[string][]Tier{
		"zero threshold": {{AboveInputTokens: 0, Rates: Rates{Input: 1}}},
		"repeated":       {{AboveInputTokens: 10, Rates: Rates{Input: 1}}, {AboveInputTokens: 10, Rates: Rates{Input: 2}}},
		"descending":     {{AboveInputTokens: 20, Rates: Rates{Input: 1}}, {AboveInputTokens: 10, Rates: Rates{Input: 2}}},
		"negative rate":  {{AboveInputTokens: 10, Rates: Rates{Input: -1}}},
	} {
		e := entry("m", SourceSeed, 1)
		e.Tiers = tiers
		if err := e.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A tier with no rates is how the table says "the price above here is not
// known", and only the table may make one. Written by a person it is a
// mistake, and is refused.
func TestAStatedTierNeedsRates(t *testing.T) {
	unknown := []Tier{{AboveInputTokens: 272_000}}

	e := entry("m", SourceAWS, 1)
	e.Tiers = unknown
	if err := e.Validate(); err != nil {
		t.Errorf("the table refused its own unknown-price tier: %v", err)
	}
	if err := ValidateCard(RateCard{Rates: Rates{Input: 1, Output: 1}, Tiers: unknown}); err == nil {
		t.Error("a configured tier with no rates was accepted")
	}
	if _, err := parseSeed([]byte(`{"captured_at":"2026-09-01T00:00:00Z","entries":[{"provider":"bedrock",
		"model":"m","scope":"regional","input":"1","output":"2","tiers":[{"above_input_tokens":10}]}]}`)); err == nil {
		t.Error("a seed tier with no rates was accepted")
	}
}

// A model with no row at all is reported as having no price, not as "no rate
// for token class", which is the other failure.
func TestAnUnknownModelIsNoPrice(t *testing.T) {
	_, _, err := NewTable(nil).Price("bedrock-mantle", "openai.gpt-5.6-luna", Usage{InputTokens: 1})
	if !errors.Is(err, ErrNoPrice) || !errors.Is(err, ErrRateMissing) {
		t.Fatalf("error = %v, want ErrNoPrice, which is still ErrRateMissing", err)
	}
	if !strings.Contains(err.Error(), "no price for this model") {
		t.Errorf("the error reads %q", err)
	}

	e := luna()
	e.Rates.CacheWrite = 0
	_, _, err = NewTable([]Entry{e}).Price("bedrock-mantle", "openai.gpt-5.6-luna", Usage{CacheWriteTokens: 5})
	if errors.Is(err, ErrNoPrice) || !errors.Is(err, ErrRateMissing) {
		t.Errorf("a missing class = %v, want ErrRateMissing and not ErrNoPrice", err)
	}
}

func TestSeedReadsTiers(t *testing.T) {
	entries, err := parseSeed([]byte(`{"captured_at":"2026-09-01T00:00:00Z","entries":[{"provider":"bedrock-mantle",
		"model":"openai.gpt-5.6-luna","scope":"regional","input":"0.22","output":"1.32",
		"tiers":[{"above_input_tokens":272000,"input":"0.44","output":"1.98"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Tiers; len(got) != 1 || got[0].AboveInputTokens != 272_000 || got[0].Rates.Input != mustRate("0.44") {
		t.Errorf("tiers = %+v", got)
	}
}

func mustMoney(usd string) Money {
	m, err := ParseUSD(usd)
	if err != nil {
		panic(err)
	}
	return m
}
