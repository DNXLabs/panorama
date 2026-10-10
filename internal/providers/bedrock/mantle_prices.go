// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// Where bedrock-mantle rates are published, checked against the public offer
// files.
//
// Open-weight and xAI models are under AmazonBedrock, in a usage type that
// names the model and says "mantle": USE1-openai.gpt-oss-120b-mantle-input-
// tokens-standard. Claude Haiku 4.5's Mantle rows are under
// AmazonBedrockService in the same shape. No other Claude model, and no GPT-5.x,
// has a Mantle row at all - and AWS states that per-token prices are the same on
// both endpoints for the same model, at the in-region rate. So a Claude model
// with no Mantle row is priced at bedrock-runtime's regional rate for the same
// model family, and says so in the log; anything else with no row stays
// unpriced, which is visible, rather than borrowing a rate it was never given.
var mantlePriceServices = []string{"AmazonBedrock", "AmazonBedrockService"}

// mantleClasses maps the class half of a Mantle usage type to the rate it sets.
var mantleClasses = map[string]tokenClass{
	"input-tokens":          classInput,
	"output-tokens":         classOutput,
	"cache-read-tokens":     classCacheRead,
	"cache-write-tokens":    classCacheWrite5m,
	"cache-write-tokens-1h": classCacheWrite1h,
}

// mantleTier is the only service tier read. Priority is 1.75x it, flex and batch
// half, and the gateway refuses a request asking for any of them - so a rate
// from another tier would price a request that never ran at it.
const mantleTier = "standard"

// mantlePriceAliases are usage-type model names that are not the model's id.
// A literal list, because the alternative - folding punctuation until two
// strings match - is how one model's price gets attached to another's traffic.
var mantlePriceAliases = map[string]string{
	"zai.glm5": "zai.glm-5",
}

// MantlePriceSource reads bedrock-mantle rates for one region.
type MantlePriceSource struct {
	prices    *awspricing.Client
	catalogue *MantleCatalogue
	runtime   *PriceSource
	region    string
	log       *slog.Logger
	now       func() time.Time
}

// NewMantlePriceSource builds a price source for a Mantle catalogue.
func NewMantlePriceSource(ctx context.Context, conf Config, catalogue *MantleCatalogue) (*MantlePriceSource, error) {
	if err := validRegion(conf.Region); err != nil {
		return nil, err
	}
	if conf.Logger == nil {
		conf.Logger = slog.New(slog.DiscardHandler)
	}
	pricingCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(pricingAPIRegion))
	if err != nil {
		return nil, fmt.Errorf("bedrock-mantle: load aws config for pricing: %w", err)
	}
	runtime, err := NewPriceSource(ctx, conf)
	if err != nil {
		return nil, err
	}
	return &MantlePriceSource{
		prices:    awspricing.NewFromConfig(pricingCfg),
		catalogue: catalogue,
		runtime:   runtime,
		region:    conf.Region,
		log:       conf.Logger,
		now:       time.Now,
	}, nil
}

// Name identifies the provider these prices are for.
func (s *MantlePriceSource) Name() string { return MantleName }

// Region reports which region's prices are fetched.
func (s *MantlePriceSource) Region() string { return s.region }

// Fetch returns the standard-tier rates for every model this region lists.
func (s *MantlePriceSource) Fetch(ctx context.Context) ([]pricing.Entry, error) {
	listed, err := s.catalogue.Catalogue(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(listed))
	for _, e := range listed {
		known[e.ID] = true
	}

	rates := map[string]*pricing.Rates{}
	asOf := map[string]time.Time{}
	var skipped int
	unmatched := map[string]struct{}{}

	for _, service := range mantlePriceServices {
		products, perr := s.products(ctx, service)
		if perr != nil {
			return nil, perr
		}
		for _, p := range products {
			model, class, ok := parseMantleUsageType(p.Product.Attributes.UsageType)
			if !ok {
				skipped++
				continue
			}
			if !known[model] {
				unmatched[model] = struct{}{}
				continue
			}
			for _, dim := range p.dimensions() {
				rate, rerr := rateFrom(dim.PricePerUnit.USD, dim.Unit)
				if rerr != nil || rate == 0 {
					continue
				}
				if rates[model] == nil {
					rates[model] = &pricing.Rates{}
				}
				setRate(rates[model], class, rate)
				if dim.effectiveDate.After(asOf[model]) {
					asOf[model] = dim.effectiveDate
				}
			}
		}
	}

	// Claude models with no Mantle row, at the runtime regional rate for the
	// same family. The runtime fetch is the one the bedrock provider makes,
	// asked about this region.
	borrowed := s.runtimeRates(ctx, listed, rates, asOf)

	now := s.now().UTC()
	entries := make([]pricing.Entry, 0, len(rates))
	for model, r := range rates {
		entry := pricing.Entry{
			Key:       pricing.Key{Provider: MantleName, Model: model, Scope: pricing.ScopeRegional},
			Rates:     *r,
			Currency:  pricing.CurrencyUSD,
			Source:    pricing.SourceAWS,
			Region:    s.region,
			AsOf:      asOf[model],
			UpdatedAt: now,
		}
		if verr := entry.Validate(); verr != nil {
			s.log.WarnContext(ctx, "discarding an invalid price", "error", verr, "model", model)
			continue
		}
		entries = append(entries, entry)
	}

	s.log.InfoContext(ctx, "fetched bedrock-mantle prices",
		"provider", MantleName, "region", s.region,
		"models_listed", len(listed), "models_priced", len(entries),
		"priced_at_the_runtime_rate", strings.Join(borrowed, ", "),
		"products_skipped", skipped, "rows_for_unlisted_models", len(unmatched))
	return entries, nil
}

// runtimeRates fills in Claude models that have no Mantle row, from the runtime
// regional rate for the same family, and returns which it filled.
//
// Only Claude, and only when the family resolves to one rate. AWS's statement
// that the endpoints bill identically is about the same model; a family with
// two runtime rates (a 48k and a 200k variant priced apart, say) is ambiguous,
// and is left unpriced rather than priced at whichever came first.
func (s *MantlePriceSource) runtimeRates(
	ctx context.Context,
	listed []providers.CatalogueEntry,
	rates map[string]*pricing.Rates,
	asOf map[string]time.Time,
) []string {
	var needed []string
	for _, e := range listed {
		if vendorOf(e.ID) == "anthropic" && rates[e.ID] == nil {
			needed = append(needed, e.ID)
		}
	}
	if len(needed) == 0 {
		return nil
	}

	runtime, err := s.runtime.Fetch(ctx)
	if err != nil {
		// Logged and carried on: the open-weight rates above are still right,
		// and the Claude models stay unpriced until the next refresh, which is
		// visible in every total.
		s.log.WarnContext(ctx, "could not read runtime rates to price bedrock-mantle Claude models",
			"region", s.region, "error", err)
		return nil
	}

	byFamily := map[string][]pricing.Entry{}
	for _, e := range runtime {
		if e.Scope != pricing.ScopeRegional {
			continue
		}
		f := modelFamily(e.Model)
		byFamily[f] = append(byFamily[f], e)
	}

	var borrowed []string
	for _, id := range needed {
		candidates := byFamily[modelFamily(id)]
		if len(candidates) == 0 {
			continue
		}
		first := candidates[0]
		ambiguous := false
		for _, c := range candidates[1:] {
			if c.Rates != first.Rates {
				ambiguous = true
			}
		}
		if ambiguous {
			s.log.WarnContext(ctx, "a bedrock-mantle Claude model matches runtime rates that disagree; leaving it unpriced",
				"model", id, "region", s.region)
			continue
		}
		r := first.Rates
		rates[id] = &r
		asOf[id] = first.AsOf
		borrowed = append(borrowed, id)
	}
	return borrowed
}

// parseMantleUsageType reads a Mantle usage type into the model it prices and
// the class of token.
//
//	USE1-openai.gpt-oss-120b-mantle-input-tokens-standard
//	     └ model ─────────┘        └ class ───┘ └ tier ┘
//
// Anything else - another tier, a runtime usage type with no "-mantle-", a
// class this build does not list - is not a rate this source sets.
func parseMantleUsageType(usageType string) (model string, class tokenClass, ok bool) {
	_, rest, found := strings.Cut(usageType, "-")
	if !found {
		return "", 0, false
	}
	model, tail, found := strings.Cut(rest, "-mantle-")
	if !found || model == "" {
		return "", 0, false
	}
	classPart, found := strings.CutSuffix(tail, "-"+mantleTier)
	if !found {
		return "", 0, false
	}
	class, ok = mantleClasses[classPart]
	if !ok {
		return "", 0, false
	}
	if alias, aliased := mantlePriceAliases[model]; aliased {
		model = alias
	}
	return model, class, true
}

// products reads every price in this region under one service code.
func (s *MantlePriceSource) products(ctx context.Context, service string) ([]priceProduct, error) {
	paginator := awspricing.NewGetProductsPaginator(s.prices, &awspricing.GetProductsInput{
		ServiceCode: strPtr(service),
		Filters: []pricingtypes.Filter{{
			Type:  pricingtypes.FilterTypeTermMatch,
			Field: strPtr("regionCode"),
			Value: strPtr(s.region),
		}},
	})

	var products []priceProduct
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("bedrock-mantle: get %s products: %w", service, err)
		}
		for _, raw := range page.PriceList {
			if !strings.Contains(raw, "-mantle-") {
				continue
			}
			var p priceProduct
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				s.log.DebugContext(ctx, "unparsable price document", "error", err)
				continue
			}
			products = append(products, p)
		}
	}
	return products, nil
}
