// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"testing"
)

const foundTable = `{
  "note": "test",
  "entries": [
    {"provider": "bedrock-mantle", "model": "openai.gpt-5.6-luna", "scope": "regional",
     "input": "0.22", "output": "1.32", "cache_read": "0.022", "cache_write": "0.275",
     "tiers": [{"above_input_tokens": 272000, "input": "0.44", "output": "1.98", "cache_read": "0.044", "cache_write": "0.55"}],
     "source": "aws-model-card",
     "source_url": "https://docs.aws.amazon.com/bedrock/latest/userguide/model-card-openai-gpt-56-luna.html",
     "evidence": ["| In-Region | $0.22 | $0.275 | $0.022 | $1.32 |"],
     "found_at": "2026-09-21T00:00:00Z", "found_by": "global.anthropic.claude-haiku-4-5-20251001-v1:0"},
    {"provider": "bedrock-mantle", "model": "qwen.qwen3-coder-next", "scope": "regional",
     "input": "0.50", "output": "1.20", "source": "aws-pricing-api", "region": "us-east-1",
     "found_at": "2026-09-21T00:00:00Z"}
  ]
}`

// A snapshot of the pricing API ranks as the seed does, because it is what the
// seed is; a model card ranks below both.
func TestFoundRowsRankByWhatTheyWereReadFrom(t *testing.T) {
	table, err := ParseFoundTable([]byte(foundTable))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := table.PriceEntries()
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0]; got.Source != SourceModelCard || len(got.Tiers) != 1 {
		t.Errorf("the model-card row is %s with %d tiers", got.Source, len(got.Tiers))
	}
	if got := entries[1]; got.Source != SourceSeed || got.Region != "us-east-1" {
		t.Errorf("the snapshot row is %s from %q", got.Source, got.Region)
	}

	bad := FoundTable{Entries: []FoundEntry{table.Entries[0]}}
	bad.Entries[0].Source = "a-blog-post"
	if _, err := bad.PriceEntries(); err == nil {
		t.Error("a row from an unknown source was accepted")
	}
}

// The found table adds models, and never replaces one the seed prices: the
// seed is a snapshot of the pricing API, which is a stronger claim than a page
// read after the fact.
func TestTheFoundTableOnlyAdds(t *testing.T) {
	seeded := entry("anthropic.claude-sonnet-5", SourceSeed, 3)
	clash := entry("anthropic.claude-sonnet-5", SourceModelCard, 99)
	added := entry("openai.gpt-5.6-luna", SourceModelCard, 1)

	got := compiled([]Entry{seeded}, []Entry{clash, added})
	if len(got) != 2 {
		t.Fatalf("compiled %d rows, want 2", len(got))
	}
	byKey := map[Key]Entry{}
	for _, e := range got {
		byKey[e.Key] = e
	}
	if byKey[seeded.Key].Rates.Input != 3 {
		t.Error("the found table replaced a model the seed prices")
	}
	if _, ok := byKey[added.Key]; !ok {
		t.Error("the found table did not add the model the seed lacks")
	}
}

// The repository's copy is empty, so the only rows a binary carries are the
// ones the release took from the Prices workflow's table. A run of the finder
// on a laptop that got committed would otherwise ship unreviewed.
func TestTheRepositoryFoundTableIsEmpty(t *testing.T) {
	table, err := ParseFoundTable(foundJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Entries) != 0 {
		t.Errorf("internal/pricing/found_prices.json has %d rows; the release fills it from S3, "+
			"and the repository's copy stays empty", len(table.Entries))
	}
}

// A fetched rate never carries tiers, and it outranks the compiled row it
// replaces. When the two agree on the base price, the tiers are kept.
func TestARefreshKeepsTiersItAgreesWith(t *testing.T) {
	compiledRow := luna()
	repo := newMemRepo()
	repo.entries[compiledRow.Key] = compiledRow

	fetched := compiledRow
	fetched.Tiers = nil
	fetched.Source = SourceAWS
	svc := NewService(Options{Repository: repo, Sources: sources(&stubSource{name: "bedrock-mantle", entries: []Entry{fetched}})})
	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	got, ok := svc.Table().Lookup("bedrock-mantle", "openai.gpt-5.6-luna")
	if !ok || got.Source != SourceAWS {
		t.Fatalf("the fetched rate did not replace the compiled one: %+v", got)
	}
	if len(got.Tiers) != 1 || got.Tiers[0].Rates != compiledRow.Tiers[0].Rates {
		t.Errorf("tiers = %+v, want the compiled tiers kept", got.Tiers)
	}
}

// When they disagree the compiled tiers are a price that is no longer the
// price. The threshold is kept with no rate above it, so a long request is
// recorded unpriced rather than billed at the short-context rate.
func TestARefreshThatDisagreesLeavesLongRequestsUnpriced(t *testing.T) {
	compiledRow := luna()
	repo := newMemRepo()
	repo.entries[compiledRow.Key] = compiledRow

	fetched := compiledRow
	fetched.Tiers = nil
	fetched.Source = SourceAWS
	fetched.Rates.Input = mustRate("0.25")
	svc := NewService(Options{Repository: repo, Sources: sources(&stubSource{name: "bedrock-mantle", entries: []Entry{fetched}})})
	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	table := svc.Table()
	if _, _, err := table.Price("bedrock-mantle", "openai.gpt-5.6-luna", Usage{InputTokens: 1_000}); err != nil {
		t.Errorf("a short request is unpriced: %v", err)
	}
	if _, _, err := table.Price("bedrock-mantle", "openai.gpt-5.6-luna", Usage{InputTokens: 300_000}); err == nil {
		t.Error("a long request was priced at a short-context rate nobody published for it")
	}
}
