// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
)

// defaultDiscoveryInterval is how often the provider is asked what it can
// invoke.
//
// Hours rather than minutes, because the answer changes when AWS ships a model
// or an operator changes an IAM policy - neither of which happens on a cadence
// worth polling for. The console offers a refresh for the case that does matter:
// somebody has just changed something and wants to see it.
const defaultDiscoveryInterval = 6 * time.Hour

// Discoverer keeps the model catalogue current, for every configured provider.
//
// The catalogue is a cache of each provider's answer, and everything an
// administrator does with models is chosen from it: which to permit, on which
// provider, in which geography, at what price. A stale one offers models that
// have been withdrawn and hides ones that were added, so it carries its fetch
// time and the console shows it.
type Discoverer struct {
	db        *store.DB
	instances func() []*providers.Instance
	interval  time.Duration
	log       *slog.Logger
}

// DiscovererOptions configures the job.
type DiscovererOptions struct {
	DB *store.DB
	// Instances lists the providers to ask, and is asked afresh on every pass -
	// a provider enabled in the console is discovered by the next pass, and one
	// disabled stops being asked.
	Instances func() []*providers.Instance
	// Interval is how often to re-ask. Zero uses the default; negative disables
	// the background loop, which is what tests want.
	Interval time.Duration
	Logger   *slog.Logger
}

// NewDiscoverer builds the job.
func NewDiscoverer(opts DiscovererOptions) *Discoverer {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Interval == 0 {
		opts.Interval = defaultDiscoveryInterval
	}
	if opts.Instances == nil {
		opts.Instances = func() []*providers.Instance { return nil }
	}
	return &Discoverer{
		db:        opts.DB,
		instances: opts.Instances,
		interval:  opts.Interval,
		log:       opts.Logger,
	}
}

// RunOnce replaces each provider's catalogue with what it currently offers.
//
// Providers are discovered independently, and every failure is returned. A
// failure leaves that provider's previous catalogue in place rather than
// emptying it: an expired credential would otherwise wipe the list an
// administrator configures from, and the console would show a deployment with
// no models available - which looks like a much worse problem than the one that
// actually occurred.
func (d *Discoverer) RunOnce(ctx context.Context) error {
	var errs []error
	for _, inst := range d.instances() {
		if _, err := Discover(ctx, d.db, inst); err != nil {
			errs = append(errs, err)
			continue
		}
	}
	return errors.Join(errs...)
}

// Discover replaces one provider's catalogue and reports how many targets it
// found. The console's refresh, the CLI and the job all call it, so a catalogue
// written from any of them is the same rows.
func Discover(ctx context.Context, db *store.DB, inst *providers.Instance) (int, error) {
	if inst == nil || inst.Catalogue == nil {
		return 0, nil
	}
	kind := inst.Settings.Kind

	found, err := inst.Catalogue.Catalogue(ctx)
	if err != nil {
		return 0, fmt.Errorf("jobs: discovering %s models in %s: %w", kind, inst.Settings.Region, err)
	}

	models := make([]store.CatalogueModel, 0, len(found))
	for _, e := range found {
		models = append(models, CatalogueRow(kind, e))
	}

	if err := db.ReplaceCatalogue(ctx, kind, models); err != nil {
		return 0, err
	}
	return len(models), nil
}

// CatalogueRow converts what a provider reported into the row that stores it.
// One function, because it has three callers, and a copy of the fields in each
// is exactly where a new column gets dropped by one of them.
func CatalogueRow(kind providers.Kind, e providers.CatalogueEntry) store.CatalogueModel {
	region := e.Region
	return store.CatalogueModel{
		Provider:          kind,
		ID:                e.ID,
		FoundationModelID: e.FoundationModelID,
		Vendor:            e.Vendor,
		DisplayName:       e.DisplayName,
		Region:            region,
		APIs:              e.APIs,
		Status:            e.Status,
		StatusReason:      e.StatusReason,
		Retention:         e.Retention,
		Geography:         e.Geography,
		Regions:           e.Regions,
		Streaming:         e.Streaming,
		Lifecycle:         e.Lifecycle,
		RequiresProfile:   e.RequiresProfile,
		ContextWindow:     e.ContextWindow,
		MaxOutputTokens:   e.MaxOutputTokens,
	}
}

// Run keeps the catalogue current until the context is cancelled.
func (d *Discoverer) Run(ctx context.Context) {
	if d.interval < 0 {
		return
	}

	d.runOrLog(ctx)

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runOrLog(ctx)
		}
	}
}

func (d *Discoverer) runOrLog(ctx context.Context) {
	for _, inst := range d.instances() {
		n, err := Discover(ctx, d.db, inst)
		if err != nil {
			// Logged every time. A catalogue that has been failing to refresh
			// is a list of models slowly diverging from what the account really
			// offers, and nothing else would say so.
			d.log.ErrorContext(ctx, "refreshing the model catalogue", "provider", inst.Settings.Kind, "error", err)
			continue
		}
		d.log.InfoContext(ctx, "model catalogue refreshed",
			"provider", inst.Settings.Kind, "region", inst.Settings.Region, "targets", n)
	}
}
