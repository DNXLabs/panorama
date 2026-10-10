// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricefinder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

const docs = "https://docs.aws.amazon.com/bedrock/latest/userguide/"

// The fixtures are the pages as AWS served them.
func page(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func lunaCard(t *testing.T) Card {
	t.Helper()
	card, err := ReadCard(page(t, "model-card-openai-gpt-56-luna.html"), docs+"model-card-openai-gpt-56-luna.html")
	if err != nil {
		t.Fatal(err)
	}
	return card
}

const (
	short = "Commercial Regions \u2014 short context (272K input tokens or fewer)"
	long  = "Commercial Regions \u2014 long context (more than 272K input tokens)"
)

// lunaReading is what a correct reading of the GPT-5.6 Luna card reports.
func lunaReading() Extraction {
	row := func(section, option string, above int64, cells ...string) PriceRow {
		return PriceRow{
			Section: section, Option: option, AboveInputTokens: above,
			Row:   Row(append([]string{map[string]string{OptionInRegion: "In-Region", OptionGeo: "Geo CRIS", OptionGlobal: "Global CRIS"}[option]}, prefixed(cells)...)),
			Input: cells[0], CacheWrite: cells[1], CacheRead: cells[2], Output: cells[3],
		}
	}
	return Extraction{Rows: []PriceRow{
		row(short, OptionInRegion, 0, "0.22", "0.275", "0.022", "1.32"),
		row(short, OptionGeo, 0, "0.22", "0.275", "0.022", "1.32"),
		row(short, OptionGlobal, 0, "0.20", "0.25", "0.02", "1.20"),
		row(long, OptionInRegion, 272_000, "0.44", "0.55", "0.044", "1.98"),
		row(long, OptionGeo, 272_000, "0.44", "0.55", "0.044", "1.98"),
		row(long, OptionGlobal, 272_000, "0.40", "0.50", "0.04", "1.80"),
	}}
}

func prefixed(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = "$" + c
	}
	return out
}

func TestTheIndexListsEveryCard(t *testing.T) {
	links, err := CardLinks(page(t, "model-cards.html"), docs+"model-cards.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) < 100 {
		t.Fatalf("found %d cards, want the index's 127", len(links))
	}
	found := false
	for _, l := range links {
		if l.URL == docs+"model-card-openai-gpt-56-luna.html" {
			found = true
		}
		if !strings.HasPrefix(l.URL, "https://") {
			t.Errorf("a link was not made absolute: %s", l.URL)
		}
	}
	if !found {
		t.Error("the GPT-5.6 Luna card is not among the links")
	}
}

// The Pricing section is rendered as headings and one line per table row,
// which is the text the reader reads and every check matches against.
func TestACardsPricingSectionIsRenderedAsText(t *testing.T) {
	card := lunaCard(t)
	for _, want := range []string{
		"### " + short,
		"| In-Region | $0.22 | $0.275 | $0.022 | $1.32 |",
		"### " + long,
		"| Global CRIS | $0.40 | $0.50 | $0.04 | $1.80 |",
		"### AWS GovCloud (US-East and US-West)",
		"#### Short context (272K input tokens or fewer)",
	} {
		if !strings.Contains(card.Pricing, want) {
			t.Errorf("the Pricing text lacks %q:\n%s", want, card.Pricing)
		}
	}
	if strings.Contains(card.Pricing, "Regional Availability") {
		t.Error("the Pricing text ran past the section's end")
	}
	if !standsAlone(card.Text, "openai.gpt-5.6-luna") {
		t.Error("the page text does not name the model id")
	}

	// A Claude card points at the pricing page and states no price.
	sonnet, err := ReadCard(page(t, "model-card-anthropic-claude-sonnet-5.html"), docs+"model-card-anthropic-claude-sonnet-5.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sonnet.Pricing, "$") {
		t.Errorf("the Sonnet 5 card is expected to state no price:\n%s", sonnet.Pricing)
	}
}

func TestAFaithfulReadingIsKept(t *testing.T) {
	priced, err := Verify(lunaCard(t), lunaReading())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(priced) != 3 {
		t.Fatalf("priced %d options, want 3", len(priced))
	}
	inRegion, _ := pick(priced, pricing.Key{Provider: "bedrock-mantle", Model: "openai.gpt-5.6-luna", Scope: pricing.ScopeRegional})
	if got := inRegion.Card.Rates.Decimal(); got.Input != "0.22" || got.CacheWrite != "0.275" || got.CacheRead != "0.022" || got.Output != "1.32" {
		t.Errorf("In-Region = %+v", got)
	}
	if tiers := inRegion.Card.Tiers; len(tiers) != 1 || tiers[0].AboveInputTokens != 272_000 ||
		tiers[0].Rates.Decimal().Output != "1.98" {
		t.Errorf("In-Region tiers = %+v", tiers)
	}
	global, _ := pick(priced, pricing.Key{Provider: "bedrock", Model: "openai.gpt-5.6-luna", Scope: pricing.ScopeGlobal})
	if global.Card.Rates.Decimal().Input != "0.20" {
		t.Errorf("Global = %+v", global.Card.Rates.Decimal())
	}
}

// Each of these is a way a language model could report a plausible price
// that the page does not state, and each is caught.
func TestAnUnfaithfulReadingIsRefused(t *testing.T) {
	card := lunaCard(t)
	for name, mutate := range map[string]func(*Extraction){
		"a figure not in the row": func(e *Extraction) { e.Rows[0].Input = "0.23" },
		"a row not on the page": func(e *Extraction) {
			e.Rows[0].Row = "| In-Region | $0.23 | $0.275 | $0.022 | $1.32 |"
		},
		"a price dropped": func(e *Extraction) { e.Rows[0].CacheRead = "" },
		"a price counted twice": func(e *Extraction) {
			e.Rows[0].CacheRead = "0.22"
		},
		"the wrong section": func(e *Extraction) { e.Rows[0].Section = long },
		"GovCloud prices": func(e *Extraction) {
			e.Rows[0].Section = "Short context (272K input tokens or fewer)"
			e.Rows[0].Row = "| In-Region | $0.264 | $0.33 | $0.0264 | $1.584 |"
			e.Rows[0].Input, e.Rows[0].CacheWrite, e.Rows[0].CacheRead, e.Rows[0].Output = "0.264", "0.33", "0.0264", "1.584"
		},
		"the wrong option": func(e *Extraction) { e.Rows[2].Option = OptionGeo },
		"a threshold the heading does not state": func(e *Extraction) {
			for i := range e.Rows {
				if e.Rows[i].AboveInputTokens > 0 {
					e.Rows[i].AboveInputTokens = 200_000
				}
			}
		},
		"a long-context price with no base": func(e *Extraction) { e.Rows = e.Rows[3:] },
		"a long-context price cheaper than the base": func(e *Extraction) {
			// The short and long rows swapped: each is verbatim and in its
			// own section, but the thresholds are the wrong way round.
			e.Rows[0].AboveInputTokens, e.Rows[3].AboveInputTokens = 272_000, 0
		},
		"nothing at all": func(e *Extraction) { e.Rows = nil },
		"two classes swapped": func(e *Extraction) {
			// Every figure is in the row and counted once, in the wrong
			// columns: the cache read and the cache write traded places.
			e.Rows[0].CacheRead, e.Rows[0].CacheWrite = e.Rows[0].CacheWrite, e.Rows[0].CacheRead
		},
	} {
		ex := lunaReading()
		mutate(&ex)
		if _, err := Verify(card, ex); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeReader stands in for the language model.
type fakeReader struct {
	matches  map[string]string
	readings map[string]Extraction
	extracts []string
}

func (f *fakeReader) Name() string { return "fake" }

func (f *fakeReader) Match(_ context.Context, models []string, _ []Link) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range models {
		if u, ok := f.matches[m]; ok {
			out[m] = u
		}
	}
	return out, nil
}

func (f *fakeReader) Extract(_ context.Context, model string, _ Card) (Extraction, error) {
	f.extracts = append(f.extracts, model)
	ex, ok := f.readings[model]
	if !ok {
		return Extraction{}, errors.New("no reading for " + model)
	}
	return ex, nil
}

func fetcher(t *testing.T) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, url string) ([]byte, error) {
		name := strings.TrimPrefix(url, docs)
		if name == url {
			return nil, fmt.Errorf("unexpected fetch of %s", url)
		}
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			return nil, fmt.Errorf("%s: 404", url)
		}
		return raw, nil
	}
}

func servable(ids ...string) []providers.CatalogueEntry {
	out := make([]providers.CatalogueEntry, len(ids))
	for i, id := range ids {
		out[i] = providers.CatalogueEntry{ID: id, APIs: []providers.API{providers.APIOpenAIResponses}}
	}
	return out
}

// A whole run: what the seed and the previous table price is left alone, a
// price the Price List API publishes is snapshotted, GPT-5.x is read off its
// card under every key it is served as, and what nothing documents is
// reported rather than guessed.
func TestARunOnlyAdds(t *testing.T) {
	seedRow := pricing.Entry{
		Key:   pricing.Key{Provider: "bedrock-mantle", Model: "openai.gpt-oss-120b", Scope: pricing.ScopeRegional},
		Rates: pricing.Rates{Input: 1, Output: 2}, Currency: pricing.CurrencyUSD, Source: pricing.SourceSeed,
	}
	previousRow := pricing.FoundEntry{
		Provider: "bedrock-mantle", Model: "openai.gpt-5.6-terra", Scope: "regional",
		DecimalRates: pricing.DecimalRates{Input: "2.20", Output: "13.20"},
		Source:       pricing.SourceModelCard, FoundAt: "2026-09-20T00:00:00Z",
	}
	published := pricing.Entry{
		Key:   pricing.Key{Provider: "bedrock-mantle", Model: "qwen.qwen3-coder-next", Scope: pricing.ScopeRegional},
		Rates: pricing.Rates{Input: 500_000_000, Output: 1_200_000_000}, Currency: pricing.CurrencyUSD,
		Source: pricing.SourceAWS, Region: "us-east-1", AsOf: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}

	gpt55 := Extraction{Rows: []PriceRow{
		{Section: "Commercial Regions \u2014 short context (272K input tokens or fewer)", Option: OptionInRegion,
			Row: "| In-Region | $5.50 | \u2014 | $0.55 | $33.00 |", Input: "5.50", CacheRead: "0.55", Output: "33.00"},
		{Section: "Commercial Regions \u2014 long context (more than 272K input tokens)", Option: OptionInRegion,
			AboveInputTokens: 272_000,
			Row:              "| In-Region | $11.00 | \u2014 | $1.10 | $49.50 |", Input: "11.00", CacheRead: "1.10", Output: "49.50"},
	}}
	reader := &fakeReader{
		matches: map[string]string{
			"openai.gpt-5.6-luna":       docs + "model-card-openai-gpt-56-luna.html",
			"openai.gpt-5.5-2026-04-23": docs + "model-card-openai-gpt-55.html",
			"anthropic.claude-sonnet-5": docs + "model-card-anthropic-claude-sonnet-5.html",
		},
		readings: map[string]Extraction{
			"openai.gpt-5.6-luna":       lunaReading(),
			"openai.gpt-5.5-2026-04-23": gpt55,
			"anthropic.claude-sonnet-5": {NoPrice: "the card points to the Amazon Bedrock pricing page"},
		},
	}
	f := &Finder{
		Reader: reader, Fetch: fetcher(t), IndexURL: docs + "model-cards.html",
		Now: func() time.Time { return time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) },
	}
	res, err := f.Run(t.Context(), Inputs{
		Catalogues: []Catalogue{
			{Provider: "bedrock-mantle", Region: "us-east-1", Entries: append(servable(
				"openai.gpt-oss-120b", "openai.gpt-5.6-terra", "qwen.qwen3-coder-next",
				"openai.gpt-5.6-luna", "openai.gpt-5.5-2026-04-23", "acme.nobody-documents-this",
				"anthropic.claude-sonnet-5"),
				providers.CatalogueEntry{ID: "acme.browse-only"})},
			{Provider: "bedrock", Region: "us-east-1", Entries: servable("us.openai.gpt-5.6-luna", "global.openai.gpt-5.6-luna")},
			{Provider: "bedrock-mantle", Region: "eu-west-1", Err: errors.New("dial tcp: no such host")},
		},
		Published: []Published{{Provider: "bedrock-mantle", Region: "us-east-1", Entries: []pricing.Entry{published}}},
		Seed:      []pricing.Entry{seedRow},
		Previous:  pricing.FoundTable{Note: "kept", Entries: []pricing.FoundEntry{previousRow}},
	})
	if err != nil {
		t.Fatal(err)
	}

	added := map[string]pricing.FoundEntry{}
	for _, a := range res.Added {
		added[a.Provider+"/"+a.Model+"/"+a.Scope] = a
	}
	for key, want := range map[string]struct{ source, input string }{
		"bedrock-mantle/qwen.qwen3-coder-next/regional":     {pricing.SourceAWS, "0.50"},
		"bedrock-mantle/openai.gpt-5.6-luna/regional":       {pricing.SourceModelCard, "0.22"},
		"bedrock/openai.gpt-5.6-luna/regional":              {pricing.SourceModelCard, "0.22"},
		"bedrock/openai.gpt-5.6-luna/global":                {pricing.SourceModelCard, "0.20"},
		"bedrock-mantle/openai.gpt-5.5-2026-04-23/regional": {pricing.SourceModelCard, "5.50"},
	} {
		got, ok := added[key]
		if !ok {
			t.Errorf("%s was not added", key)
			continue
		}
		if got.Source != want.source || got.Input != want.input {
			t.Errorf("%s = %s at %s, want %s at %s", key, got.Source, got.Input, want.source, want.input)
		}
	}
	if len(res.Added) != 5 {
		t.Errorf("added %d rows, want 5: %+v", len(res.Added), res.Added)
	}
	if luna := added["bedrock-mantle/openai.gpt-5.6-luna/regional"]; len(luna.Tiers) != 1 || luna.FoundBy != "fake" ||
		luna.SourceURL != docs+"model-card-openai-gpt-56-luna.html" || len(luna.Evidence) != 2 {
		t.Errorf("the Luna row = %+v", luna)
	}
	if dated := added["bedrock-mantle/openai.gpt-5.5-2026-04-23/regional"]; !strings.Contains(dated.Evidence[0], "dated snapshot") {
		t.Errorf("the dated snapshot's evidence does not say it was priced as its family: %v", dated.Evidence)
	}

	// The seed's model and the previous table's model were not read again,
	// and one card was read once for all three keys it prices.
	for _, m := range reader.extracts {
		if m == "openai.gpt-5.6-terra" || m == "openai.gpt-oss-120b" {
			t.Errorf("%s was read although it is already priced", m)
		}
	}
	if len(reader.extracts) != 3 {
		t.Errorf("extracted %v, want each model's card read once", reader.extracts)
	}

	// The previous row is carried unchanged.
	var kept bool
	for _, e := range res.Table.Entries {
		if e.Model == previousRow.Model && e.Input == "2.20" && e.FoundAt == previousRow.FoundAt {
			kept = true
		}
	}
	if !kept || len(res.Table.Entries) != 6 || res.Table.Note != "kept" {
		t.Errorf("the table = %d rows, previous kept %v", len(res.Table.Entries), kept)
	}

	missing := map[string]string{}
	for _, m := range res.Missing {
		missing[m.Model] = m.Reason
	}
	if !strings.Contains(missing["acme.nobody-documents-this"], "no model card") {
		t.Errorf("missing = %v", missing)
	}
	if !strings.Contains(missing["anthropic.claude-sonnet-5"], "states no price") {
		t.Errorf("a card with no price = %q", missing["anthropic.claude-sonnet-5"])
	}
	if _, ok := missing["acme.browse-only"]; ok {
		t.Error("a browse-only model was reported as needing a price")
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "eu-west-1") {
		t.Errorf("problems = %v, want the unreadable eu-west-1 catalogue", res.Problems)
	}

	// And what it wrote loads as the binary will load it.
	entries, err := res.Table.PriceEntries()
	if err != nil {
		t.Fatalf("the table does not load: %v", err)
	}
	for _, e := range entries {
		if e.Model == "openai.gpt-5.6-luna" && e.Provider == "bedrock-mantle" {
			if _, _, perr := pricing.NewTable([]pricing.Entry{e}).Price("bedrock-mantle", "openai.gpt-5.6-luna",
				pricing.Usage{InputTokens: 300_000, OutputTokens: 1}); perr != nil {
				t.Errorf("a long Luna request is unpriced: %v", perr)
			}
		}
	}

	var report bytes.Buffer
	if err := WriteReport(&report, res, "abc123"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"this run added 5", "Still unpriced", "acme.nobody-documents-this", "eu-west-1"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("the report lacks %q:\n%s", want, report.String())
		}
	}
}

// A section that does not state its unit is not read: a price per thousand
// tokens taken as per million is a thousandfold error nothing else would catch.
func TestASectionWithoutItsUnitIsRefused(t *testing.T) {
	card := lunaCard(t)
	card.Pricing = strings.Replace(card.Pricing, "per 1 million tokens", "per unit", 1)
	if _, err := Verify(card, lunaReading()); err == nil || !strings.Contains(err.Error(), "per million") {
		t.Errorf("error = %v, want the missing unit named", err)
	}
}

// A card the reader matches must name the model, or the match is a guess.
func TestAMatchTheCardDoesNotConfirmIsRefused(t *testing.T) {
	reader := &fakeReader{
		matches:  map[string]string{"openai.gpt-5.6-terra": docs + "model-card-openai-gpt-56-luna.html"},
		readings: map[string]Extraction{"openai.gpt-5.6-terra": lunaReading()},
	}
	f := &Finder{Reader: reader, Fetch: fetcher(t), IndexURL: docs + "model-cards.html"}
	res, err := f.Run(t.Context(), Inputs{Catalogues: []Catalogue{
		{Provider: "bedrock-mantle", Region: "us-east-1", Entries: servable("openai.gpt-5.6-terra")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 || len(res.Missing) != 1 || !strings.Contains(res.Missing[0].Reason, "does not name it") {
		t.Errorf("added %+v, missing %+v", res.Added, res.Missing)
	}
	if len(reader.extracts) != 0 {
		t.Error("a card that does not name the model was read for its prices")
	}
}
