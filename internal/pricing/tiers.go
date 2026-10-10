// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import "fmt"

// Tier is a second price for the same model, which applies once a request's
// prompt is larger than a threshold.
//
// OpenAI's GPT-5.x on Bedrock is the case that made it necessary: at more than
// 272K input tokens every class costs more - input doubles, output rises by
// half - and the model card says the higher rate "applies to all input and
// output tokens, not just tokens above 272K". So a tier is not a marginal
// band like an income tax bracket. It replaces the whole price for that
// request, and a table holding only the first price would report every long
// session at a fraction of what it cost, which is the one direction this
// product promises never to be wrong in.
type Tier struct {
	// AboveInputTokens is the threshold: the tier applies to a request whose
	// prompt is larger than this many tokens. See Usage.PromptTokens.
	AboveInputTokens int64
	// Rates replace the entry's base rates outright. A class left at zero
	// here is a class with no rate at this size, exactly as it would be in the
	// base rates - a request using it is recorded unpriced.
	Rates Rates
}

// RateCard is a model's price as an operator or a file states it: the rates at
// every size, and the tiers above them.
type RateCard struct {
	Rates Rates
	Tiers []Tier
}

// PromptTokens is the size a long-context tier is decided on: the whole prompt,
// cached or not.
//
// Uncached input, cache reads and cache writes are one prompt split three ways
// by what the cache did with it, and a provider deciding whether a request is
// "more than 272K input tokens" is counting the prompt, not the part of it that
// missed the cache. Counting only uncached input would keep a 900K-token
// session that mostly hit the cache on the short-context price.
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// RatesFor returns the rates that price this usage: the highest tier whose
// threshold the prompt exceeds, or the base rates when it exceeds none.
func (e Entry) RatesFor(u Usage) Rates {
	rates := e.Rates
	prompt := u.PromptTokens()
	for _, t := range e.Tiers {
		if prompt > t.AboveInputTokens {
			rates = t.Rates
		}
	}
	return rates
}

// validTiers rejects tiers that would price a request ambiguously: thresholds
// out of order, repeated, or not positive.
//
// A tier with no rates at all is allowed here, and means that the price above
// its threshold is unknown: a request that large is recorded unpriced. The
// table makes one when a fetched rate replaces a tiered one it disagrees with
// (see Service.keepTiers). Nobody may write one - a file or an operator
// stating a tier with no rates has made a mistake - and requireTierRates is
// what says so on those paths.
func validTiers(tiers []Tier) error {
	var previous int64
	for i, t := range tiers {
		switch {
		case t.AboveInputTokens <= 0:
			return fmt.Errorf("tier %d has a threshold of %d tokens; it must be positive", i+1, t.AboveInputTokens)
		case i > 0 && t.AboveInputTokens <= previous:
			return fmt.Errorf("tier %d's threshold of %d tokens is not above the previous one's %d; "+
				"list tiers in ascending order", i+1, t.AboveInputTokens, previous)
		}
		if err := t.Rates.nonNegative(); err != nil {
			return fmt.Errorf("tier %d has %w", i+1, err)
		}
		previous = t.AboveInputTokens
	}
	return nil
}

// requireTierRates refuses a stated tier with no rates.
func requireTierRates(tiers []Tier) error {
	for i, t := range tiers {
		if t.Rates.Zero() {
			return fmt.Errorf("tier %d, above %d tokens, has no rates at all", i+1, t.AboveInputTokens)
		}
	}
	return nil
}

// ValidateCard checks a rate card somebody wrote: the rules the table applies
// to a stored entry, and a rate in every tier.
func ValidateCard(card RateCard) error {
	if err := card.Rates.nonNegative(); err != nil {
		return err
	}
	if err := validTiers(card.Tiers); err != nil {
		return err
	}
	return requireTierRates(card.Tiers)
}
