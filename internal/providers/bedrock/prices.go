// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsbedrock "github.com/aws/aws-sdk-go-v2/service/bedrock"
	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// Where current Bedrock model prices actually live.
//
// Not under the AmazonBedrock service code, which is the obvious guess and the
// wrong one: that catalogue carries five Anthropic products, all of them
// retired models - Claude 2.0, 2.1, Instant, and the original Claude 3 Haiku
// and Sonnet. Every model anyone runs a coding agent against is published under
// AmazonBedrockFoundationModels instead, in a different shape, with different
// attributes. Checked against the live API.
const foundationModelsService = "AmazonBedrockFoundationModels"

// pricingAPIRegion is where the Price List Query API is called.
//
// The API is offered in only a handful of regions while the catalogue it serves
// covers all of them, so this is fixed rather than following the Bedrock region.
// Which region's prices are wanted is a filter, not an endpoint.
const pricingAPIRegion = "us-east-1"

// editionSuffix decorates every model name in the pricing catalogue:
// "Claude Sonnet 5 (Amazon Bedrock Edition)".
const editionSuffix = " (Amazon Bedrock Edition)"

// tokenClass is which of a model's rates a pricing product carries.
type tokenClass int

const (
	classInput tokenClass = iota
	classOutput
	classCacheRead
	classCacheWrite5m
	classCacheWrite1h
)

// usageKind is what a usage type resolves to.
type usageKind struct {
	class tokenClass
	scope pricing.Scope
}

// usageTypes maps a Bedrock usage type to the rate it sets.
//
// An allowlist, and a deliberately literal one. The catalogue contains two
// naming conventions for the same six things - Claude Haiku 4.5 publishes
// "InputTokenCount" while Claude Sonnet 5 publishes "input_tokens_standard" -
// alongside batch, reserved-throughput, provisioned-throughput,
// latency-optimised, and customisation prices that must never be mistaken for
// an on-demand rate. Pattern-matching over that would eventually attach a batch
// price, which is half the on-demand one, to interactive traffic and understate
// every bill. Anything not listed here is skipped and counted.
var usageTypes = map[string]usageKind{
	// The CamelCase catalogue.
	"InputTokenCount":                    {classInput, pricing.ScopeRegional},
	"InputTokenCount_Global":             {classInput, pricing.ScopeGlobal},
	"OutputTokenCount":                   {classOutput, pricing.ScopeRegional},
	"OutputTokenCount_Global":            {classOutput, pricing.ScopeGlobal},
	"CacheReadInputTokenCount":           {classCacheRead, pricing.ScopeRegional},
	"CacheReadInputTokenCount_Global":    {classCacheRead, pricing.ScopeGlobal},
	"CacheWriteInputTokenCount":          {classCacheWrite5m, pricing.ScopeRegional},
	"CacheWriteInputTokenCount_Global":   {classCacheWrite5m, pricing.ScopeGlobal},
	"CacheWrite1hInputTokenCount":        {classCacheWrite1h, pricing.ScopeRegional},
	"CacheWrite1hInputTokenCount_Global": {classCacheWrite1h, pricing.ScopeGlobal},

	// The snake_case catalogue.
	"input_tokens_standard":                 {classInput, pricing.ScopeRegional},
	"input_tokens_global_standard":          {classInput, pricing.ScopeGlobal},
	"output_tokens_standard":                {classOutput, pricing.ScopeRegional},
	"output_tokens_global_standard":         {classOutput, pricing.ScopeGlobal},
	"cache_read_tokens_standard":            {classCacheRead, pricing.ScopeRegional},
	"cache_read_tokens_global_standard":     {classCacheRead, pricing.ScopeGlobal},
	"cache_write_tokens_standard":           {classCacheWrite5m, pricing.ScopeRegional},
	"cache_write_tokens_global_standard":    {classCacheWrite5m, pricing.ScopeGlobal},
	"cache_write_tokens_1h_standard":        {classCacheWrite1h, pricing.ScopeRegional},
	"cache_write_tokens_1h_global_standard": {classCacheWrite1h, pricing.ScopeGlobal},
}

// PriceSource reads Bedrock rates from the AWS Price List Query API.
//
// It needs two AWS permissions: pricing:GetProducts and
// bedrock:ListFoundationModels. The second is not incidental - the price
// catalogue identifies a model only by a human name ("Claude Sonnet 5"), and
// the gateway invokes model ids ("anthropic.claude-sonnet-5"). Bedrock's own
// model list is what joins the two, and without it prices cannot be attached to
// anything.
type PriceSource struct {
	prices  *awspricing.Client
	models  *awsbedrock.Client
	region  string
	log     *slog.Logger
	nowFunc func() time.Time
}

// NewPriceSource builds a price source for the given Bedrock region.
func NewPriceSource(ctx context.Context, conf Config) (*PriceSource, error) {
	if err := validRegion(conf.Region); err != nil {
		return nil, err
	}
	if conf.Logger == nil {
		conf.Logger = slog.New(slog.DiscardHandler)
	}

	pricingCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(pricingAPIRegion))
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config for pricing: %w", err)
	}
	bedrockCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(conf.Region))
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config for %s: %w", conf.Region, err)
	}

	return &PriceSource{
		prices:  awspricing.NewFromConfig(pricingCfg),
		models:  awsbedrock.NewFromConfig(bedrockCfg),
		region:  conf.Region,
		log:     conf.Logger,
		nowFunc: time.Now,
	}, nil
}

// Name identifies the provider these prices are for.
func (s *PriceSource) Name() string { return Name }

// Region reports which region's prices are fetched.
func (s *PriceSource) Region() string { return s.region }

// Fetch returns the current on-demand rates for every model whose pricing can
// be attached to a model id.
//
// Products that cannot be resolved are skipped and counted, never guessed at. A
// price attached to the wrong model is worse than no price: an unpriced request
// is visible in every total, while a mis-priced one is invisible and wrong.
func (s *PriceSource) Fetch(ctx context.Context) ([]pricing.Entry, error) {
	ids, err := s.modelIDs(ctx)
	if err != nil {
		return nil, err
	}

	products, err := s.products(ctx)
	if err != nil {
		return nil, err
	}

	now := s.nowFunc().UTC()
	var (
		rates       = map[pricing.Key]*pricing.Rates{}
		asOf        = map[pricing.Key]time.Time{}
		skipped     int
		unmatched   = map[string]struct{}{}
		unknownUnit = map[string]struct{}{}
	)

	for _, p := range products {
		kind, ok := usageTypes[usageSuffix(p.Product.Attributes.UsageType)]
		if !ok {
			skipped++
			continue
		}

		name := strings.TrimSuffix(p.Product.Attributes.ServiceName, editionSuffix)
		modelIDs := ids[normalizeName(name)]
		if len(modelIDs) == 0 {
			unmatched[name] = struct{}{}
			continue
		}

		for _, dim := range p.dimensions() {
			rate, err := rateFrom(dim.PricePerUnit.USD, dim.Unit)
			if err != nil {
				unknownUnit[dim.Unit] = struct{}{}
				continue
			}
			if rate == 0 {
				// A published zero is a promotion or a placeholder, not a fact
				// to price traffic against.
				continue
			}

			for _, id := range modelIDs {
				key := pricing.Key{Provider: Name, Model: id, Scope: kind.scope}
				if rates[key] == nil {
					rates[key] = &pricing.Rates{}
				}
				setRate(rates[key], kind.class, rate)
				if dim.effectiveDate.After(asOf[key]) {
					asOf[key] = dim.effectiveDate
				}
			}
		}
	}

	entries := make([]pricing.Entry, 0, len(rates))
	for key, r := range rates {
		entry := pricing.Entry{
			Key:       key,
			Rates:     *r,
			Currency:  pricing.CurrencyUSD,
			Source:    pricing.SourceAWS,
			Region:    s.region,
			AsOf:      asOf[key],
			UpdatedAt: now,
		}
		if err := entry.Validate(); err != nil {
			s.log.WarnContext(ctx, "discarding an invalid price", "error", err, "model", key.Model)
			continue
		}
		entries = append(entries, entry)
	}

	s.log.InfoContext(ctx, "fetched Bedrock prices",
		"provider", Name,
		"region", s.region,
		"models_priced", len(entries),
		"products_read", len(products),
		"products_skipped", skipped,
		"models_without_an_id", len(unmatched),
		"unknown_units", len(unknownUnit),
	)
	if len(unmatched) > 0 {
		// Expected and harmless - the catalogue prices models this account
		// cannot invoke - but worth naming, because a model an operator does
		// have access to appearing here is the reason its requests are unpriced.
		s.log.DebugContext(ctx, "priced models with no matching model id",
			"models", strings.Join(sortedKeys(unmatched), ", "))
	}
	if len(unknownUnit) > 0 {
		s.log.WarnContext(ctx, "price dimensions in units this build cannot convert",
			"units", strings.Join(sortedKeys(unknownUnit), ", "))
	}

	return entries, nil
}

// modelIDs maps a model's display name to every model id that carries it.
//
// One name can map to several ids - Claude 3 Haiku is published with 48k and
// 200k context variants that share a price - so all of them are priced.
func (s *PriceSource) modelIDs(ctx context.Context) (map[string][]string, error) {
	out, err := s.models.ListFoundationModels(ctx, &awsbedrock.ListFoundationModelsInput{})
	if err != nil {
		return nil, fmt.Errorf("bedrock: list foundation models: %w", err)
	}

	ids := make(map[string][]string, len(out.ModelSummaries))
	for _, m := range out.ModelSummaries {
		if m.ModelName == nil || m.ModelId == nil {
			continue
		}
		name := normalizeName(*m.ModelName)
		if !slices.Contains(ids[name], *m.ModelId) {
			ids[name] = append(ids[name], *m.ModelId)
		}
	}
	return ids, nil
}

// products reads every price in this deployment's region.
func (s *PriceSource) products(ctx context.Context) ([]priceProduct, error) {
	paginator := awspricing.NewGetProductsPaginator(s.prices, &awspricing.GetProductsInput{
		ServiceCode: strPtr(foundationModelsService),
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
			return nil, fmt.Errorf("bedrock: get products: %w", err)
		}
		for _, raw := range page.PriceList {
			var p priceProduct
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				// One malformed document does not invalidate the rest, and the
				// count of what was read against what was priced is logged.
				s.log.DebugContext(ctx, "unparsable price document", "error", err)
				continue
			}
			products = append(products, p)
		}
	}
	return products, nil
}

// priceProduct is the shape of one document in a Price List response.
type priceProduct struct {
	Product struct {
		Attributes struct {
			UsageType   string `json:"usagetype"`
			ServiceName string `json:"servicename"`
			RegionCode  string `json:"regionCode"`
		} `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]struct {
			EffectiveDate   time.Time `json:"effectiveDate"`
			PriceDimensions map[string]struct {
				Unit         string `json:"unit"`
				PricePerUnit struct {
					USD string `json:"USD"`
				} `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// priceDimension is one price within a product.
type priceDimension struct {
	Unit         string
	PricePerUnit struct{ USD string }

	effectiveDate time.Time
}

// dimensions flattens the on-demand terms. Only on-demand is read: reserved and
// provisioned throughput are a different product that Panorama does not meter.
func (p priceProduct) dimensions() []priceDimension {
	var out []priceDimension
	for _, term := range p.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			d := priceDimension{Unit: dim.Unit, effectiveDate: term.EffectiveDate}
			d.PricePerUnit.USD = dim.PricePerUnit.USD
			out = append(out, d)
		}
	}
	return out
}

// usageSuffix strips the region decoration off a usage type.
//
// "USE1-MP:USE1_InputTokenCount_Global-Units" describes the same thing as
// "APS2-MP:APS2_InputTokenCount_Global-Units"; only the part between the region
// prefix and the -Units suffix identifies what is being priced.
func usageSuffix(usageType string) string {
	_, after, found := strings.Cut(usageType, "-MP:")
	if !found {
		return ""
	}
	after = strings.TrimSuffix(after, "-Units")

	// Drop the repeated region prefix: "USE1_InputTokenCount" → "InputTokenCount".
	_, suffix, found := strings.Cut(after, "_")
	if !found {
		return ""
	}
	return suffix
}

// rateFrom converts a published price into a rate per million tokens.
//
// The unit is read rather than assumed: the current catalogue quotes "1M
// tokens" and the retired one quotes "1K tokens", a thousandfold difference
// that would otherwise pass silently into a bill.
func rateFrom(usd, unit string) (pricing.Rate, error) {
	switch unit {
	case "1M tokens":
		return pricing.ParseUSDPerMillion(usd)
	case "1K tokens":
		return pricing.ParseUSDPerThousand(usd)
	case "tokens":
		return pricing.ParseUSDPerToken(usd)
	default:
		return 0, fmt.Errorf("bedrock: unsupported price unit %q", unit)
	}
}

// setRate assigns a rate to the class it belongs to.
func setRate(r *pricing.Rates, class tokenClass, rate pricing.Rate) {
	switch class {
	case classInput:
		r.Input = rate
	case classOutput:
		r.Output = rate
	case classCacheRead:
		r.CacheRead = rate
	case classCacheWrite5m:
		r.CacheWrite = rate
	case classCacheWrite1h:
		r.CacheWrite1h = rate
	}
}

// normalizeName makes model names comparable across the two AWS catalogues,
// which differ in case and spacing more often than they agree.
func normalizeName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
