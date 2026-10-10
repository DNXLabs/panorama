// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricefinder

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// DefaultIndexURL is the Bedrock user guide's list of model cards.
const DefaultIndexURL = "https://docs.aws.amazon.com/bedrock/latest/userguide/model-cards.html"

// Catalogue is what one provider lists in one region, or why it could not be
// read.
type Catalogue struct {
	Provider string
	Region   string
	Entries  []providers.CatalogueEntry
	Err      error
}

// Published is what the Price List API gives one provider in one region, or
// why it could not be read.
type Published struct {
	Provider string
	Region   string
	Entries  []pricing.Entry
	Err      error
}

// Inputs are what one run works from.
type Inputs struct {
	Catalogues []Catalogue
	Published  []Published
	// Seed is what the binary carries before the found table: seed.json.
	Seed []pricing.Entry
	// Previous is the table this run adds to.
	Previous pricing.FoundTable
}

// Finder finds prices.
type Finder struct {
	Reader Reader
	// Fetch reads a web page.
	Fetch    func(ctx context.Context, url string) ([]byte, error)
	IndexURL string
	Now      func() time.Time
	Log      *slog.Logger
}

// Missing is a model nothing prices after a run, and why.
type Missing struct {
	Key pricing.Key
	// Model is the id a route would name, as a catalogue lists it.
	Model   string
	Regions []string
	Reason  string
}

// Result is one run's outcome.
type Result struct {
	// Table is the previous table with this run's rows added.
	Table pricing.FoundTable
	Added []pricing.FoundEntry
	// Missing is every servable model still without a price.
	Missing []Missing
	// Problems are failures that made the run incomplete: a catalogue or a
	// price list that could not be read, a reader that failed. A run with
	// problems still adds what it found, and says what it could not look at.
	Problems []string
}

// wanted is one servable model with no price yet.
type wanted struct {
	key     pricing.Key
	model   string
	regions []string
}

// Run finds a price for every servable model nothing prices, and adds what it
// finds to the previous table.
func (f *Finder) Run(ctx context.Context, in Inputs) (Result, error) {
	if f.Now == nil {
		f.Now = time.Now
	}
	if f.Log == nil {
		f.Log = slog.New(slog.DiscardHandler)
	}
	if f.IndexURL == "" {
		f.IndexURL = DefaultIndexURL
	}
	now := f.Now().UTC().Format(time.RFC3339)

	var res Result
	priced := map[pricing.Key]bool{}
	for _, e := range in.Seed {
		priced[e.Key] = true
	}
	for _, e := range in.Previous.Entries {
		priced[e.Key()] = true
	}

	// What can be served, which is what can be routed to and so what has to
	// be priced. A browse-only model is in the catalogue and speaks nothing
	// this gateway serves, so nothing will ever be billed for it here.
	todo := map[pricing.Key]*wanted{}
	var order []pricing.Key
	for _, c := range in.Catalogues {
		if c.Err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("could not read the %s catalogue in %s: %v", c.Provider, c.Region, c.Err))
			continue
		}
		for _, e := range c.Entries {
			if len(e.APIs) == 0 {
				continue
			}
			k := pricing.KeyFor(c.Provider, e.ID)
			if priced[k] {
				continue
			}
			w, ok := todo[k]
			if !ok {
				w = &wanted{key: k, model: e.ID}
				todo[k] = w
				order = append(order, k)
			}
			if !contains(w.regions, c.Region) {
				w.regions = append(w.regions, c.Region)
			}
		}
	}

	// Rung one: the Price List API, which the refresh would fetch anyway.
	published := map[pricing.Key]pricing.Entry{}
	for _, p := range in.Published {
		if p.Err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("could not read %s prices in %s: %v", p.Provider, p.Region, p.Err))
			continue
		}
		for _, e := range p.Entries {
			if _, seen := published[e.Key]; !seen {
				published[e.Key] = e
			}
		}
	}
	var unread []*wanted
	for _, k := range order {
		w := todo[k]
		e, ok := published[k]
		if !ok {
			unread = append(unread, w)
			continue
		}
		res.Added = append(res.Added, pricing.FoundEntry{
			Provider: k.Provider, Model: k.Model, Scope: string(k.Scope),
			DecimalRates: e.Rates.Decimal(), Tiers: pricing.DecimalTiers(e.Tiers),
			Source: pricing.SourceAWS, Region: e.Region,
			Evidence: []string{fmt.Sprintf("AWS Price List API, %s, effective %s", e.Region, e.AsOf.UTC().Format("2006-01-02"))},
			FoundAt:  now,
		})
	}

	// Rung two: the model cards.
	if len(unread) > 0 {
		added, missing, problems := f.readCards(ctx, unread, now)
		res.Added = append(res.Added, added...)
		res.Missing = append(res.Missing, missing...)
		res.Problems = append(res.Problems, problems...)
	}

	res.Table = in.Previous
	res.Table.Entries = append(append([]pricing.FoundEntry(nil), in.Previous.Entries...), res.Added...)
	sort.Slice(res.Table.Entries, func(i, j int) bool { return less(res.Table.Entries[i].Key(), res.Table.Entries[j].Key()) })
	if len(res.Added) > 0 {
		res.Table.GeneratedAt = now
	}
	sort.Slice(res.Missing, func(i, j int) bool { return less(res.Missing[i].Key, res.Missing[j].Key) })

	// The table this run writes is the table a release embeds, so it has to
	// be one the binary will load.
	if _, err := res.Table.PriceEntries(); err != nil {
		return Result{}, fmt.Errorf("pricefinder: the table this run built would not load: %w", err)
	}
	return res, nil
}

// readCards looks up the models no price list carries on their model cards.
func (f *Finder) readCards(ctx context.Context, unread []*wanted, now string) (added []pricing.FoundEntry, missing []Missing, problems []string) {
	miss := func(w *wanted, reason string) {
		missing = append(missing, Missing{Key: w.key, Model: w.model, Regions: w.regions, Reason: reason})
	}

	index, err := f.Fetch(ctx, f.IndexURL)
	var links []Link
	if err == nil {
		links, err = CardLinks(index, f.IndexURL)
	}
	if err != nil {
		problems = append(problems, fmt.Sprintf("could not read the model-card index %s: %v", f.IndexURL, err))
		for _, w := range unread {
			miss(w, "the model-card index could not be read")
		}
		return added, missing, problems
	}

	// One question per model id: bedrock and bedrock-mantle list the same
	// model under the same id, and its card is one page.
	byModel := map[string][]*wanted{}
	var models []string
	for _, w := range unread {
		if _, ok := byModel[w.key.Model]; !ok {
			models = append(models, w.key.Model)
		}
		byModel[w.key.Model] = append(byModel[w.key.Model], w)
	}
	sort.Strings(models)

	matches, err := f.Reader.Match(ctx, models, links)
	if err != nil {
		problems = append(problems, fmt.Sprintf("could not match models to their cards: %v", err))
		for _, w := range unread {
			miss(w, "matching models to cards failed")
		}
		return added, missing, problems
	}

	known := map[string]bool{}
	for _, l := range links {
		known[l.URL] = true
	}
	cards := map[string]Card{}
	for _, model := range models {
		ws := byModel[model]
		url, ok := matches[model]
		if !ok {
			for _, w := range ws {
				miss(w, "no model card documents it")
			}
			continue
		}
		if !known[url] {
			for _, w := range ws {
				miss(w, "the card matched to it is not one the index lists: "+url)
			}
			continue
		}

		card, cached := cards[url]
		if !cached {
			page, ferr := f.Fetch(ctx, url)
			if ferr == nil {
				card, ferr = ReadCard(page, url)
			}
			if ferr != nil {
				problems = append(problems, fmt.Sprintf("could not read %s: %v", url, ferr))
				for _, w := range ws {
					miss(w, "its model card could not be read")
				}
				continue
			}
			cards[url] = card
		}

		named, how := names(card.Text, model)
		if !named {
			for _, w := range ws {
				miss(w, "the card matched to it ("+url+") does not name it")
			}
			continue
		}

		ex, err := f.Reader.Extract(ctx, model, card)
		if err != nil {
			problems = append(problems, fmt.Sprintf("could not read the prices on %s: %v", url, err))
			for _, w := range ws {
				miss(w, "reading its card failed")
			}
			continue
		}
		priced, err := Verify(card, ex)
		if err != nil {
			f.Log.WarnContext(ctx, "a model card's prices did not check out", "model", model, "url", url, "error", err)
			for _, w := range ws {
				miss(w, "the prices read from "+url+" did not check out: "+err.Error())
			}
			continue
		}

		for _, w := range ws {
			option, ok := pick(priced, w.key)
			if !ok {
				miss(w, fmt.Sprintf("its card (%s) states no %s price", url, want(w.key)))
				continue
			}
			evidence := option.Evidence
			if how != "" {
				evidence = append([]string{how}, evidence...)
			}
			added = append(added, pricing.FoundEntry{
				Provider: w.key.Provider, Model: w.key.Model, Scope: string(w.key.Scope),
				DecimalRates: option.Card.Rates.Decimal(), Tiers: pricing.DecimalTiers(option.Card.Tiers),
				Source: pricing.SourceModelCard, SourceURL: url,
				Evidence: evidence, FoundAt: now, FoundBy: f.Reader.Name(),
			})
		}
	}
	return added, missing, problems
}

// pick chooses the inference option that prices a key.
//
// bedrock-mantle invokes a model in the region the call is made to, which is
// In-Region. bedrock-runtime's regional scope is a geographic profile - us.,
// eu., apac. - so it is Geo CRIS, and In-Region only where a card lists no
// Geo price, which is a model invoked by its bare id; its global scope is
// Global CRIS.
func pick(priced []Priced, k pricing.Key) (Priced, bool) {
	find := func(option string) (Priced, bool) {
		for _, p := range priced {
			if p.Option == option {
				return p, true
			}
		}
		return Priced{}, false
	}
	switch {
	case k.Provider == string(providers.KindBedrockMantle):
		return find(OptionInRegion)
	case k.Scope == pricing.ScopeGlobal:
		return find(OptionGlobal)
	default:
		if p, ok := find(OptionGeo); ok {
			return p, true
		}
		return find(OptionInRegion)
	}
}

func want(k pricing.Key) string {
	switch {
	case k.Provider == string(providers.KindBedrockMantle):
		return "In-Region"
	case k.Scope == pricing.ScopeGlobal:
		return "Global CRIS"
	default:
		return "Geo CRIS or In-Region"
	}
}

// dated is a snapshot id: a model id with a release date on the end.
var dated = regexp.MustCompile(`^(.+)-\d{4}-\d{2}-\d{2}$`)

// names reports whether a card documents a model id: the id itself, standing
// alone in the page's text, or - for a dated snapshot - the id it is a
// snapshot of, in which case how says so for the evidence.
func names(text, model string) (ok bool, how string) {
	if standsAlone(text, model) {
		return true, ""
	}
	if m := dated.FindStringSubmatch(model); m != nil && standsAlone(text, m[1]) {
		return true, fmt.Sprintf("priced as %s, which the card documents and %s is a dated snapshot of", m[1], model)
	}
	return false, ""
}

// standsAlone finds an id that is not part of a longer one: openai.gpt-5.5
// is not found in openai.gpt-5.5-2026-04-23 or us.openai.gpt-5.5.
func standsAlone(text, id string) bool {
	edge := func(b byte) bool {
		idChar := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
			b == '.' || b == '-' || b == ':' || b == '_'
		return !idChar
	}
	for i := 0; ; {
		j := strings.Index(text[i:], id)
		if j < 0 {
			return false
		}
		at := i + j
		end := at + len(id)
		if (at == 0 || edge(text[at-1])) && (end == len(text) || edge(text[end])) {
			return true
		}
		i = at + 1
	}
}

func less(a, b pricing.Key) bool {
	if a.Provider != b.Provider {
		return a.Provider < b.Provider
	}
	if a.Model != b.Model {
		return a.Model < b.Model
	}
	return a.Scope < b.Scope
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
