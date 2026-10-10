// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Repository is the persistence a price table needs.
//
// An interface rather than the store type because the store already imports
// this package to describe what it is storing. Keeping the dependency one-way
// is what lets rates be defined next to the arithmetic that uses them.
type Repository interface {
	// UpsertPrices writes rates, resolving conflicts by source precedence, and
	// reports how many rows changed.
	UpsertPrices(ctx context.Context, entries []Entry) (int, error)
	// LoadPrices reads every stored rate.
	LoadPrices(ctx context.Context) ([]Entry, error)
}

// Source fetches published rates from a provider.
type Source interface {
	// Name identifies the provider these rates are for.
	Name() string
	// Region reports which region's catalogue is read.
	Region() string
	// Fetch returns current rates.
	Fetch(ctx context.Context) ([]Entry, error)
}

// Options configures a pricing service.
type Options struct {
	Repository Repository
	// Sources lists the price sources to consult, one per configured provider,
	// and is asked afresh at every refresh - a provider enabled in the console
	// at ten past the hour is priced by the next refresh, not the next restart.
	//
	// Optional. Without any the table is whatever the seed and the
	// configuration provide, which is a supportable deployment: an air-gapped
	// one, or an account without pricing:GetProducts.
	Sources func() []Source
	// Overrides are operator-configured rates, keyed by the model reference as
	// a route names it - provider/model, inference-profile prefix included,
	// because the prefix selects which rate set is being overridden.
	Overrides map[string]RateCard
	// RefreshInterval is how often the sources are consulted. Zero disables it.
	RefreshInterval time.Duration
	Logger          *slog.Logger
	// Now is injected by tests.
	Now func() time.Time
}

// Service owns the price table: it seeds it, refreshes it, and hands the data
// plane the in-memory copy that requests are costed against.
type Service struct {
	repo      Repository
	sources   func() []Source
	overrides map[string]RateCard
	interval  time.Duration
	log       *slog.Logger
	now       func() time.Time

	// refreshing serialises refreshes, so the periodic one and one an
	// administrator triggered by saving a region do not interleave their
	// writes and reloads.
	refreshing sync.Mutex

	table *Table
}

// NewService builds a pricing service.
func NewService(opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sources == nil {
		opts.Sources = func() []Source { return nil }
	}
	return &Service{
		repo:      opts.Repository,
		sources:   opts.Sources,
		overrides: opts.Overrides,
		interval:  opts.RefreshInterval,
		log:       opts.Logger,
		now:       opts.Now,
		table:     NewTable(nil),
	}
}

// Table returns the live price table. The pointer is stable for the lifetime of
// the service; a refresh replaces its contents, not the pointer, so callers can
// hold it indefinitely.
func (s *Service) Table() *Table { return s.table }

// Load brings the price table up: the compiled-in seed, then any configured
// override, then whatever is already stored, into memory.
//
// It does not call the pricing API. Startup must not depend on an external
// service being reachable, and the seed exists precisely so that a deployment
// prices its first request correctly without one.
func (s *Service) Load(ctx context.Context) error {
	seed, err := Seed()
	if err != nil {
		return err
	}
	if _, err := s.repo.UpsertPrices(ctx, seed); err != nil {
		return fmt.Errorf("pricing: store seed rates: %w", err)
	}

	overrides := s.configuredEntries()
	if len(overrides) > 0 {
		if _, err := s.repo.UpsertPrices(ctx, overrides); err != nil {
			return fmt.Errorf("pricing: store configured rates: %w", err)
		}
	}

	if err := s.reload(ctx); err != nil {
		return err
	}

	var fromCards int
	for _, e := range seed {
		if e.Source == SourceModelCard {
			fromCards++
		}
	}
	s.log.InfoContext(ctx, "price table loaded",
		"models_priced", s.table.Len(),
		"seed_rates", len(seed),
		"model_card_rates", fromCards,
		// Which found table this binary carries: the release records the same
		// digest beside the S3 object it embedded.
		"found_table", FoundTableDigest(),
		"configured_rates", len(overrides),
	)
	return nil
}

// configuredEntries turns the operator's overrides into price-table rows.
func (s *Service) configuredEntries() []Entry {
	if len(s.overrides) == 0 {
		return nil
	}

	now := s.now().UTC()
	out := make([]Entry, 0, len(s.overrides))
	for ref, card := range s.overrides {
		// Split on the first slash, as a model reference is everywhere else.
		// The configuration normalises every key into this form before it
		// reaches here, so a key without one is a programming error rather
		// than something to guess a provider for.
		provider, modelID, found := strings.Cut(ref, "/")
		if !found || provider == "" || modelID == "" {
			s.log.Error("a configured rate names no provider; it is not applied", "key", ref)
			continue
		}
		model, scope := ParseModel(modelID)
		out = append(out, Entry{
			Key:       Key{Provider: provider, Model: model, Scope: scope},
			Rates:     card.Rates,
			Tiers:     card.Tiers,
			Currency:  CurrencyUSD,
			Source:    SourceConfig,
			AsOf:      now,
			UpdatedAt: now,
		})
	}
	return out
}

// reload replaces the in-memory table from storage.
func (s *Service) reload(ctx context.Context) error {
	entries, err := s.repo.LoadPrices(ctx)
	if err != nil {
		return fmt.Errorf("pricing: load rates: %w", err)
	}
	s.table.Replace(entries)
	return nil
}

// Refresh fetches current rates from every source and stores them.
//
// Sources are fetched independently: one provider's pricing being unreachable
// does not stop the other's rates from landing, and every failure is returned.
func (s *Service) Refresh(ctx context.Context) error {
	sources := s.sources()
	if len(sources) == 0 {
		return errors.New("pricing: no price source is configured")
	}
	var errs []error
	for _, src := range sources {
		if err := s.refreshSource(ctx, src); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RefreshProvider fetches one provider's rates, which is what saving that
// provider's region asks for: the rates of the region it now invokes, now,
// rather than at the next daily refresh.
func (s *Service) RefreshProvider(ctx context.Context, provider string) error {
	for _, src := range s.sources() {
		if src.Name() == provider {
			return s.refreshSource(ctx, src)
		}
	}
	return fmt.Errorf("pricing: no price source for %s", provider)
}

func (s *Service) refreshSource(ctx context.Context, src Source) error {
	s.refreshing.Lock()
	defer s.refreshing.Unlock()

	started := s.now()
	entries, err := src.Fetch(ctx)
	if err != nil {
		return fmt.Errorf("pricing: fetch %s rates: %w", src.Name(), err)
	}
	if len(entries) == 0 {
		// Never let an empty answer empty the table. A catalogue that returns
		// nothing is a catalogue that has moved, and pricing every subsequent
		// request as unknown would be a far louder failure than keeping
		// yesterday's rates for another day.
		return fmt.Errorf("pricing: the %s price source returned no rates; keeping the existing table", src.Name())
	}

	entries = s.keepTiers(ctx, entries)
	changed, err := s.repo.UpsertPrices(ctx, entries)
	if err != nil {
		return fmt.Errorf("pricing: store fetched %s rates: %w", src.Name(), err)
	}
	if err := s.reload(ctx); err != nil {
		return err
	}

	s.log.InfoContext(ctx, "price table refreshed",
		"provider", src.Name(),
		"region", src.Region(),
		"rates_fetched", len(entries),
		"rows_changed", changed,
		"models_priced", s.table.Len(),
		"duration", s.now().Sub(started),
	)
	return nil
}

// Run refreshes rates until the context is cancelled.
//
// It returns immediately when no interval is configured, so a deployment that
// prices from the seed alone does not run an idle goroutine. With an interval
// and no provider yet, it keeps running: a provider configured in the console
// later is priced by the next tick.
func (s *Service) Run(ctx context.Context) {
	if s.interval <= 0 {
		s.log.InfoContext(ctx, "price refresh is off; rates come from the seed and configuration",
			"models_priced", s.table.Len())
		return
	}

	// Once at startup, so a long-running deployment does not serve a whole
	// interval on stale rates after a price change.
	s.refreshOrLog(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshOrLog(ctx)
		}
	}
}

// Enabled reports whether a refresh interval is configured. A caller that
// would trigger a refresh on its own - saving a provider's region - asks this
// first, because an operator who set the interval to zero chose the seed and
// their own rates over the network.
func (s *Service) Enabled() bool { return s.interval > 0 }

// refreshOrLog runs a refresh, treating failure as an interruption to survive
// rather than a reason to stop pricing.
//
// This is the sanctioned kind of fallback: the pricing API being unreachable is
// a genuine service interruption, and the honest response is to keep costing
// requests at the last known good rates and say loudly that they are stale.
// Nothing here hides an error - the failure is logged every time it happens.
func (s *Service) refreshOrLog(ctx context.Context) {
	if len(s.sources()) == 0 {
		return
	}
	if err := s.Refresh(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.ErrorContext(ctx, "refreshing prices failed; continuing on the rates already loaded",
			"error", err,
			"models_priced", s.table.Len(),
			"hint", "the fetch needs pricing:GetProducts, bedrock:ListFoundationModels and bedrock-mantle:ListModels",
		)
	}
}

// keepTiers stops a fetched rate from quietly dropping a model's long-context
// price.
//
// The Price List API publishes one rate per token class and nothing about a
// price that changes above a prompt size, so a fetched rate never carries
// tiers - and a fetched rate outranks the compiled one it replaces. Stored as
// fetched, every request over the threshold would be priced at the short-context
// rate: half of what it cost, for GPT-5.x input.
//
// When the fetched base rates are the compiled ones, the page the tiers were
// read from and the API agree, and the tiers are kept. When they differ, the
// compiled tiers describe a price that is no longer the price, so their rates
// are not kept either - but the threshold is, with no rate above it, so a
// request that large is recorded unpriced rather than under-priced. Unpriced is
// visible in every total; a long session billed at half its cost is not.
func (s *Service) keepTiers(ctx context.Context, fetched []Entry) []Entry {
	for i, e := range fetched {
		if len(e.Tiers) > 0 {
			continue
		}
		current, ok := s.table.entry(e.Key)
		if !ok || len(current.Tiers) == 0 {
			continue
		}
		if current.Rates == e.Rates {
			fetched[i].Tiers = current.Tiers
			continue
		}
		unknown := make([]Tier, len(current.Tiers))
		for j, t := range current.Tiers {
			unknown[j] = Tier{AboveInputTokens: t.AboveInputTokens}
		}
		fetched[i].Tiers = unknown
		s.log.WarnContext(ctx, "a fetched rate differs from the compiled one that has long-context tiers; "+
			"requests above the threshold will be recorded unpriced until a release carries the new tiers",
			"provider", e.Provider, "model", e.Model, "scope", string(e.Scope),
			"above_input_tokens", current.Tiers[0].AboveInputTokens,
			"compiled_source", current.Source)
	}
	return fetched
}
