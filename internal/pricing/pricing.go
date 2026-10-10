// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package pricing turns token counts into money.
//
// Coding-agent economics live in prompt caching, so cost is computed over every
// token class separately, with its own per-model rate. A cost engine that
// collapses cache reads into plain input overstates coding-agent spend several
// times over, which is the fastest way to discredit the attribution numbers
// sitting next to it. Observed on real traffic during M1: one Claude Code turn
// read back 75,069 cached tokens, which cost $0.0225 at the cache-read rate and
// would have been reported as $0.225 priced as input - a tenfold error on a
// single request.
//
// Two things that look like details and are not:
//
//   - The `us.` / `global.` inference-profile prefix is a price input. AWS
//     publishes two rate sets per model and the global one is ~10% cheaper. See
//     Scope.
//   - Prompt cache writes have two TTLs at different prices - five minutes and
//     one hour - and Bedrock reports the split. Sonnet 5 charges $2.75 and
//     $4.40 per million for them. See Rates.
//
// Money never passes through a float here. See Money.
//
// Rates live in a Table held in memory and refreshed from the AWS Price List
// API by a background job; nothing on the request path queries a price.
package pricing

import (
	"errors"
	"fmt"
	"time"
)

// Sources a rate can come from, most authoritative last. Recorded on every row
// and surfaced in the API so nobody mistakes a shipped estimate for a fetched
// price.
const (
	// SourceSeed is the table compiled into the binary. It exists so a fresh
	// deployment prices requests before it has ever reached the pricing API,
	// and so an air-gapped one prices them at all.
	SourceSeed = "seed"
	// SourceConfig is an operator-supplied rate from the configuration file.
	// It wins over both of the others: an operator with an enterprise agreement
	// knows their price better than a public catalogue does.
	SourceConfig = "config"
	// SourceAWS is the AWS Price List Query API.
	SourceAWS = "aws-pricing-api"
	// SourceModelCard is a rate read from the model's card in the Amazon Bedrock
	// user guide, for a model the Price List API does not publish at all -
	// OpenAI's GPT-5.x is the case that made it necessary. It is compiled into
	// the binary like the seed and ranks below it: a card is a web page read
	// by the price finder, and a snapshot of the pricing API is a stronger
	// claim about the same model when both exist.
	SourceModelCard = "aws-model-card"
)

// CurrencyUSD is the only currency v1 handles. It is recorded rather than
// assumed so that a future non-USD rate cannot be silently added into a USD
// total.
const CurrencyUSD = "USD"

// ErrRateMissing reports that a request used a token class the model has no
// published rate for.
//
// The request is then recorded unpriced. That is deliberate: pricing an unknown
// class at zero would quietly under-report spend and look like a discount,
// while an unpriced request is visible in every total and names the model an
// operator needs to add.
var ErrRateMissing = errors.New("pricing: no rate for token class")

// ErrNoPrice reports that the table holds no rate at all for a model.
//
// It is ErrRateMissing's sibling rather than the same error, because the two
// send an operator to different places: a missing class is a gap in a price
// that exists, and no price is a model nobody has priced. It wraps
// ErrRateMissing, so anything asking whether a request went unpriced still
// gets the one answer.
var ErrNoPrice = fmt.Errorf("%w: no price for this model", ErrRateMissing)

// ErrInconsistentUsage reports usage that cannot be true, such as more
// one-hour cache writes than cache writes in total.
var ErrInconsistentUsage = errors.New("pricing: inconsistent usage")

// Rates is the per-model price of each token class, in nanodollars per million
// tokens.
type Rates struct {
	Input     Rate
	Output    Rate
	CacheRead Rate
	// CacheWrite is the five-minute TTL rate, which is the default and the one
	// nearly all traffic uses.
	CacheWrite Rate
	// CacheWrite1h is the one-hour TTL rate. It is a separate published price,
	// around 1.6x the five-minute rate, and coding agents do use it - so the
	// two cannot share a column without over- or under-charging whichever
	// traffic is not the assumed one.
	CacheWrite1h Rate
}

// Zero reports whether no rate at all has been set.
func (r Rates) Zero() bool { return r == Rates{} }

// Usage is one request's token accounting.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CacheWrite1hTokens is the portion of CacheWriteTokens written with a
	// one-hour TTL, so it is always less than or equal to it.
	CacheWrite1hTokens int64
}

// Empty reports whether any tokens were used at all. A request that failed
// before reaching the model has no cost rather than a zero one.
func (u Usage) Empty() bool { return u == Usage{} }

// Cost is what a request cost, broken down by token class.
//
// The breakdown is stored, not just the total, because it is the evidence for
// the product's central claim. "Caching saved you 90% on this request" has to
// be checkable against the row rather than recomputed later from rates that
// may since have changed.
type Cost struct {
	Input      Money
	Output     Money
	CacheRead  Money
	CacheWrite Money
	// WebSearch is what searches cost, which is billed per search rather than
	// per token and so is its own class.
	WebSearch Money
	Total     Money
}

// Compute prices one request's usage.
//
// Every class with tokens must have a rate. A missing rate fails the whole
// computation rather than contributing zero, so an incomplete price table
// produces visibly unpriced requests instead of quietly cheap ones.
func Compute(u Usage, r Rates) (Cost, error) {
	if u.CacheWrite1hTokens > u.CacheWriteTokens {
		return Cost{}, fmt.Errorf("%w: %d one-hour cache writes exceeds %d cache writes in total",
			ErrInconsistentUsage, u.CacheWrite1hTokens, u.CacheWriteTokens)
	}

	// The five-minute portion is whatever was not written for an hour.
	write5m := u.CacheWriteTokens - u.CacheWrite1hTokens

	var (
		cost Cost
		err  error
	)
	if cost.Input, err = priceClass("input", u.InputTokens, r.Input); err != nil {
		return Cost{}, err
	}
	if cost.Output, err = priceClass("output", u.OutputTokens, r.Output); err != nil {
		return Cost{}, err
	}
	if cost.CacheRead, err = priceClass("cache_read", u.CacheReadTokens, r.CacheRead); err != nil {
		return Cost{}, err
	}

	write5mCost, err := priceClass("cache_write", write5m, r.CacheWrite)
	if err != nil {
		return Cost{}, err
	}
	write1hCost, err := priceClass("cache_write_1h", u.CacheWrite1hTokens, r.CacheWrite1h)
	if err != nil {
		return Cost{}, err
	}
	cost.CacheWrite = write5mCost + write1hCost

	cost.Total = cost.Input + cost.Output + cost.CacheRead + cost.CacheWrite
	if cost.Total < 0 {
		return Cost{}, fmt.Errorf("%w: total cost overflowed", ErrCostOverflow)
	}
	return cost, nil
}

// priceClass multiplies one token class by its rate, insisting that a class
// with tokens has a rate.
func priceClass(class string, tokens int64, rate Rate) (Money, error) {
	if tokens == 0 {
		return 0, nil
	}
	if rate == 0 {
		return 0, fmt.Errorf("%w: %s (%d tokens)", ErrRateMissing, class, tokens)
	}
	cost, err := rate.Cost(tokens)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", class, err)
	}
	return cost, nil
}

// Key identifies a priced model.
type Key struct {
	Provider string
	// Model is the foundation-model id, without any inference-profile prefix.
	Model string
	Scope Scope
}

// String renders the key for a log line.
func (k Key) String() string {
	return k.Provider + "/" + k.Model + " (" + string(k.Scope) + ")"
}

// KeyFor builds the price-table key for a provider-native model identifier.
func KeyFor(provider, modelID string) Key {
	model, scope := ParseModel(modelID)
	return Key{Provider: provider, Model: model, Scope: scope}
}

// Entry is one row of the price table.
type Entry struct {
	Key
	Rates Rates
	// Tiers are the rates that replace Rates for a request whose prompt is
	// larger than a threshold, in ascending order of threshold. Empty for a
	// model with one price at every size. See RatesFor.
	Tiers []Tier

	Currency string
	Source   string
	// Region is the AWS region catalogue this rate was read from.
	//
	// It is provenance, not part of the key. Every Anthropic rate checked in
	// August 2026 was identical in us-east-1, us-west-2, eu-central-1 and
	// ap-southeast-2, so pricing per region would multiply the table for no
	// gain. Recording where a rate came from means a future divergence is
	// visible rather than silently averaged, and a deployment always refreshes
	// against its own region.
	Region string

	// AsOf is the publication date AWS stamped on the price.
	AsOf      time.Time
	UpdatedAt time.Time
}

// Validate rejects an entry that would price traffic wrongly if stored.
func (e Entry) Validate() error {
	switch {
	case e.Provider == "":
		return errors.New("pricing: entry has no provider")
	case e.Model == "":
		return errors.New("pricing: entry has no model")
	case e.Scope != ScopeRegional && e.Scope != ScopeGlobal:
		return fmt.Errorf("pricing: entry %s has unknown scope %q", e.Model, e.Scope)
	case e.Currency != CurrencyUSD:
		return fmt.Errorf("pricing: entry %s is priced in %q; only %s is supported",
			e.Model, e.Currency, CurrencyUSD)
	case e.Rates.Zero():
		return fmt.Errorf("pricing: entry %s has no rates at all", e.Model)
	}

	if err := e.Rates.nonNegative(); err != nil {
		return fmt.Errorf("pricing: entry %s has %w", e.Model, err)
	}
	if err := validTiers(e.Tiers); err != nil {
		return fmt.Errorf("pricing: entry %s: %w", e.Model, err)
	}
	return nil
}

// nonNegative rejects a negative rate in any class.
func (r Rates) nonNegative() error {
	for _, c := range []struct {
		name string
		rate Rate
	}{
		{"input", r.Input}, {"output", r.Output}, {"cache_read", r.CacheRead},
		{"cache_write", r.CacheWrite}, {"cache_write_1h", r.CacheWrite1h},
	} {
		if c.rate < 0 {
			return fmt.Errorf("a negative %s rate", c.name)
		}
	}
	return nil
}
