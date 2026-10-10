// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"context"
	"errors"
	"testing"
	"time"
)

// memRepo is an in-memory price store implementing the same precedence rule the
// SQL upsert encodes.
type memRepo struct {
	entries map[Key]Entry
	fail    error
	writes  int
}

func newMemRepo() *memRepo { return &memRepo{entries: map[Key]Entry{}} }

func rank(source string) int {
	switch source {
	case SourceConfig:
		return 4
	case SourceAWS:
		return 3
	case SourceSeed:
		return 2
	default:
		return 1
	}
}

func (m *memRepo) UpsertPrices(_ context.Context, entries []Entry) (int, error) {
	if m.fail != nil {
		return 0, m.fail
	}
	m.writes++

	var changed int
	for _, e := range entries {
		if existing, ok := m.entries[e.Key]; ok && rank(e.Source) < rank(existing.Source) {
			continue
		}
		m.entries[e.Key] = e
		changed++
	}
	return changed, nil
}

func (m *memRepo) LoadPrices(context.Context) ([]Entry, error) {
	if m.fail != nil {
		return nil, m.fail
	}
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	return out, nil
}

// stubSource is a price source under test control.
type stubSource struct {
	name    string
	entries []Entry
	err     error
	calls   int
}

func (s *stubSource) Name() string {
	if s.name != "" {
		return s.name
	}
	return "bedrock"
}
func (s *stubSource) Region() string { return "us-east-1" }

func (s *stubSource) Fetch(context.Context) ([]Entry, error) {
	s.calls++
	return s.entries, s.err
}

func sources(list ...Source) func() []Source { return func() []Source { return list } }

func entry(model, source string, input Rate) Entry {
	return Entry{
		Key:       Key{Provider: "bedrock", Model: model, Scope: ScopeRegional},
		Rates:     Rates{Input: input, Output: input * 5},
		Currency:  CurrencyUSD,
		Source:    source,
		UpdatedAt: time.Unix(0, 0).UTC(),
	}
}

func TestLoadSeedsAndAppliesOverrides(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(Options{
		Repository: repo,
		Overrides: map[string]RateCard{
			// Keyed by the reference a route names, provider and profile
			// prefix included.
			"bedrock/us.anthropic.claude-sonnet-5": {Rates: Rates{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4, CacheWrite1h: 5}},
		},
	})

	if err := svc.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The seed is in the table.
	if svc.Table().Len() == 0 {
		t.Fatal("the table is empty after loading")
	}

	// The override won for the model it names, and the prefix decided which of
	// the two scopes it applied to.
	got, ok := svc.Table().Lookup("bedrock", "us.anthropic.claude-sonnet-5")
	if !ok {
		t.Fatal("the overridden model is missing")
	}
	if got.Source != SourceConfig || got.Rates.Input != 1 {
		t.Errorf("override did not win: %+v", got)
	}

	global, ok := svc.Table().Lookup("bedrock", "global.anthropic.claude-sonnet-5")
	if !ok {
		t.Fatal("the global profile lost its seeded rate")
	}
	if global.Source != SourceSeed {
		t.Errorf("the override leaked across scopes: global source = %q", global.Source)
	}
}

// TestSeedNeverOverwritesFetchedRates covers the restart case: a deployment
// that has been running for a month and restarts must not fall back to the
// prices its binary was built with.
func TestSeedNeverOverwritesFetchedRates(t *testing.T) {
	repo := newMemRepo()
	svc := NewService(Options{
		Repository: repo,
		Sources:    sources(&stubSource{entries: []Entry{entry("anthropic.claude-sonnet-5", SourceAWS, 9_000_000_000)}}),
	})

	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Restart: Load runs again against the same store.
	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	got, ok := svc.Table().Lookup("bedrock", "anthropic.claude-sonnet-5")
	if !ok {
		t.Fatal("model missing after restart")
	}
	if got.Source != SourceAWS || got.Rates.Input != 9_000_000_000 {
		t.Errorf("the seed overwrote a fetched rate on restart: %+v", got)
	}
}

func TestRefreshReplacesTheTable(t *testing.T) {
	repo := newMemRepo()
	source := &stubSource{entries: []Entry{entry("anthropic.new-model", SourceAWS, 1_000_000_000)}}
	svc := NewService(Options{Repository: repo, Sources: sources(source)})

	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Table().Lookup("bedrock", "anthropic.new-model"); ok {
		t.Fatal("the model is priced before the fetch")
	}

	if err := svc.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := svc.Table().Lookup("bedrock", "anthropic.new-model"); !ok {
		t.Error("the fetched model is not in the table")
	}
}

// TestRefreshKeepsTheTableWhenTheSourceReturnsNothing guards the failure mode
// that would be worst in production: a catalogue that moves, an empty answer,
// and every subsequent request silently unpriced.
func TestRefreshKeepsTheTableWhenTheSourceReturnsNothing(t *testing.T) {
	repo := newMemRepo()
	source := &stubSource{entries: nil}
	svc := NewService(Options{Repository: repo, Sources: sources(source)})

	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := svc.Table().Len()

	if err := svc.Refresh(t.Context()); err == nil {
		t.Fatal("an empty fetch must be reported as an error")
	}
	if after := svc.Table().Len(); after != before {
		t.Errorf("the table shrank from %d to %d on an empty fetch", before, after)
	}
}

func TestRefreshKeepsTheTableWhenTheSourceFails(t *testing.T) {
	repo := newMemRepo()
	source := &stubSource{err: errors.New("AccessDeniedException: pricing:GetProducts")}
	svc := NewService(Options{Repository: repo, Sources: sources(source)})

	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := svc.Table().Len()

	err := svc.Refresh(t.Context())
	if err == nil {
		t.Fatal("a failed fetch must be reported")
	}
	if svc.Table().Len() != before {
		t.Error("a failed fetch emptied the table; it must keep the last known good rates")
	}
}

func TestRefreshWithoutASourceIsAnError(t *testing.T) {
	svc := NewService(Options{Repository: newMemRepo()})
	if err := svc.Refresh(t.Context()); err == nil {
		t.Error("Refresh with no source must fail rather than quietly do nothing")
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	repo := newMemRepo()
	source := &stubSource{entries: []Entry{entry("anthropic.m", SourceAWS, 1)}}
	svc := NewService(Options{
		Repository:      repo,
		Sources:         sources(source),
		RefreshInterval: time.Hour,
	})
	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}

	// It refreshed once at startup rather than waiting a whole interval.
	if source.calls == 0 {
		t.Error("Run did not refresh at startup")
	}
}

func TestRunDoesNothingWithoutAnInterval(t *testing.T) {
	source := &stubSource{entries: []Entry{entry("anthropic.m", SourceAWS, 1)}}
	svc := NewService(Options{Repository: newMemRepo(), Sources: sources(source)})

	done := make(chan struct{})
	go func() { svc.Run(t.Context()); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run should return immediately when no refresh interval is configured")
	}
	if source.calls != 0 {
		t.Error("Run fetched rates with the refresh switched off")
	}
}

// A deployment with an interval and no provider yet keeps running: a provider
// configured in the console later is priced by the next tick, not the next
// restart.
func TestRunWaitsForASourceToAppear(t *testing.T) {
	var list []Source
	svc := NewService(Options{
		Repository:      newMemRepo(),
		Sources:         func() []Source { return list },
		RefreshInterval: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()

	select {
	case <-done:
		t.Fatal("Run stopped because no provider was configured yet")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	<-done
}

// One provider's pricing failing does not stop another's from landing.
func TestRefreshFetchesEachSourceIndependently(t *testing.T) {
	failing := &stubSource{err: errors.New("AccessDeniedException")}
	working := &stubSource{name: "bedrock-mantle", entries: []Entry{{
		Key:      Key{Provider: "bedrock-mantle", Model: "openai.gpt-oss-120b", Scope: ScopeRegional},
		Rates:    Rates{Input: 150_000_000, Output: 600_000_000},
		Currency: CurrencyUSD, Source: SourceAWS,
	}}}
	svc := NewService(Options{Repository: newMemRepo(), Sources: sources(failing, working)})
	if err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := svc.Refresh(t.Context()); err == nil {
		t.Error("the failing source was not reported")
	}
	if _, ok := svc.Table().Lookup("bedrock-mantle", "openai.gpt-oss-120b"); !ok {
		t.Error("a failing source stopped another provider's rates from landing")
	}

	if err := svc.RefreshProvider(t.Context(), "bedrock-mantle"); err != nil {
		t.Errorf("RefreshProvider: %v", err)
	}
	if err := svc.RefreshProvider(t.Context(), "nobody"); err == nil {
		t.Error("refreshing a provider with no source was not reported")
	}
}

func TestLoadFailsLoudlyWhenStorageFails(t *testing.T) {
	repo := newMemRepo()
	repo.fail = errors.New("database is gone")

	svc := NewService(Options{Repository: repo})
	if err := svc.Load(t.Context()); err == nil {
		t.Error("Load must fail when it cannot store rates, not start with an empty table")
	}
}
