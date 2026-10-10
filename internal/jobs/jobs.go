// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package jobs holds the work that must not happen on the request path.
//
// Nothing here may be reachable from a proxied request. The rule is the same
// one the whole data plane is built around: a database round trip between a
// keystroke and the first token is exactly what this gateway promises not to
// do, so anything that needs one runs here on an interval instead.
//
// # Why an interval and not River
//
// The architecture names River - an in-process, Postgres-backed queue - as the
// job runner, and that is still where retries, scheduling, and fan-out belong
// once there is any of that to do. The confirmer below is a single idempotent
// pass over a bounded list, and a queue would add a Postgres requirement to a
// deployment that is otherwise happy on SQLite for no behaviour it needs. When
// the first job arrives that genuinely needs durable retry semantics, it takes
// the River dependency with it, and this one can move at the same time.
//
// # What these workers may read
//
// Metadata that in-flight extraction already produced. Never a request body,
// because none was kept - which is what makes "no body storage" a property of
// the system rather than of one code path.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/store"
)

// Defaults for the confirmer, used when the configuration leaves them zero.
const (
	defaultConfirmInterval = 15 * time.Minute
	// defaultStaleAfter is how long a confirmed answer is trusted before it is
	// asked again. A pull request's status changes at most a few times in its
	// life, and asking every quarter of an hour would spend an API budget to
	// re-learn "still merged".
	defaultStaleAfter = 6 * time.Hour
	// defaultBatch bounds one pass. A deployment that has been running for
	// months has more work items than one pass should ask about, and the queue
	// is ordered so that the useful half comes first.
	defaultBatch = 200
)

// Confirmer asks the connected systems of record about the work Panorama
// inferred, and records what they say.
type Confirmer struct {
	db *store.DB
	// resolver rebuilds the registry from what is currently connected, rather
	// than holding one from startup. An administrator connects GitHub from a
	// screen, so a registry fixed at boot would ignore them until a restart.
	resolver *integrations.Resolver

	interval   time.Duration
	staleAfter time.Duration
	batch      int

	log *slog.Logger
	now func() time.Time
}

// ConfirmerOptions configures a Confirmer.
type ConfirmerOptions struct {
	DB       *store.DB
	Resolver *integrations.Resolver

	Interval   time.Duration
	StaleAfter time.Duration
	Batch      int

	Logger *slog.Logger
	// Now is injected by tests.
	Now func() time.Time
}

// NewConfirmer builds a confirmer.
func NewConfirmer(opts ConfirmerOptions) *Confirmer {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultConfirmInterval
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = defaultStaleAfter
	}
	if opts.Batch <= 0 {
		opts.Batch = defaultBatch
	}

	return &Confirmer{
		db:         opts.DB,
		resolver:   opts.Resolver,
		interval:   opts.Interval,
		staleAfter: opts.StaleAfter,
		batch:      opts.Batch,
		log:        opts.Logger,
		now:        opts.Now,
	}
}

// Register records every configured source, so the console can distinguish
// "connected, never run" from "nothing configured" before the first pass.
func (c *Confirmer) Register(ctx context.Context) error {
	registry, err := c.resolver.Registry(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for _, source := range registry.Sources() {
		if err := c.db.RegisterIntegration(ctx, string(source.Kind()), source.Provider(), source.Describe()); err != nil {
			errs = append(errs, err)
			continue
		}
		c.log.InfoContext(ctx, "integration configured",
			"integration", string(source.Kind()),
			"provider", source.Provider(),
			"detail", source.Describe(),
		)
	}
	return errors.Join(errs...)
}

// RunOnce performs a single confirmation pass.
func (c *Confirmer) RunOnce(ctx context.Context) error {
	// Resolved every pass, so a connection made in the console since the last
	// one is used by this one.
	registry, err := c.resolver.Registry(ctx)
	if err != nil {
		return err
	}
	if !registry.Configured() {
		return nil
	}

	now := c.now().UTC()
	refs, werr := c.db.WorkItemsToConfirm(ctx, now.Add(-c.staleAfter), c.batch)
	if werr != nil {
		return werr
	}
	if len(refs) == 0 {
		// Nothing to confirm, but the connection is still checked.
		//
		// Otherwise a freshly configured integration reads "unknown" until
		// somebody's traffic happens to produce a work item - which is exactly
		// the wrong moment to discover that the private key is wrong. The
		// operator configuring it is looking at the screen now, and their job
		// is to make it work and see that it worked.
		return c.checkAll(ctx, registry)
	}

	routed, unroutable := registry.Group(refs)
	if len(unroutable) > 0 {
		// Logged rather than dropped silently. "96 tickets not checked - no
		// Jira connected" is the sentence an operator needs; a pass that
		// reported only what it managed would look complete.
		c.log.InfoContext(ctx, "work items nothing can confirm",
			"work_items", len(unroutable),
			"reason", "no source is configured for their kind",
		)
	}

	var errs []error
	for _, source := range registry.Sources() {
		kind := source.Kind()
		batch := routed[kind]
		if len(batch) == 0 {
			continue
		}
		if err := c.confirm(ctx, source, batch, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkAll records each source's health without asking it about anything.
//
// A failure here is logged and recorded rather than returned. There is no work
// to abandon - that is the whole premise of this path - so an unreachable
// source is a red badge on the integrations screen, not a job that reports
// itself as having failed every fifteen minutes on an idle deployment.
func (c *Confirmer) checkAll(ctx context.Context, registry *integrations.Registry) error {
	for _, source := range registry.Sources() {
		kind := string(source.Kind())
		if err := c.db.RecordIntegrationCheck(ctx, kind, source.Check(ctx)); err != nil {
			c.log.ErrorContext(ctx, "recording integration health", "integration", kind, "error", err)
		}
	}
	return nil
}

// confirm asks one source about one batch and writes what it said.
func (c *Confirmer) confirm(ctx context.Context, source integrations.Source, refs []integrations.Ref, now time.Time) error {
	kind := string(source.Kind())

	checkErr := source.Check(ctx)
	if err := c.db.RecordIntegrationCheck(ctx, kind, checkErr); err != nil {
		c.log.ErrorContext(ctx, "recording integration health", "integration", kind, "error", err)
	}
	if checkErr != nil {
		// Not a fallback to some other answer: the pass for this source stops,
		// the failure is recorded and shown, and nothing is marked checked. The
		// alternative - carrying on and writing "not found" for every reference
		// - would turn an unreachable API into a deployment-wide claim that
		// none of the work exists.
		return fmt.Errorf("jobs: %s is not reachable: %w", kind, checkErr)
	}

	found, err := source.Lookup(ctx, refs)
	if err != nil {
		return fmt.Errorf("jobs: %s lookup: %w", kind, err)
	}

	result, err := c.db.ConfirmWorkItems(ctx, kind, refs, found, now)
	if err != nil {
		return err
	}
	if err := c.db.RecordIntegrationSync(ctx, kind, result); err != nil {
		c.log.ErrorContext(ctx, "recording integration sync", "integration", kind, "error", err)
	}

	c.log.InfoContext(ctx, "work items confirmed",
		"integration", kind,
		"asked", len(refs),
		"confirmed", result.Confirmed,
		"unrecognised", len(refs)-len(found),
		"links", result.Linked,
	)
	return nil
}

// Run confirms work items until the context is cancelled.
//
// It returns immediately when nothing is connected, so a deployment with no
// integrations does not run an idle goroutine - and, more usefully, so its logs
// do not fill with a job reporting that it has nothing to do.
func (c *Confirmer) Run(ctx context.Context) {
	// Deliberately not an early return for "nothing connected". Connections are
	// made from the console now, so a deployment that starts with none may have
	// one ten minutes later, and a loop that exited at boot would never notice.
	c.runOrLog(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runOrLog(ctx)
		}
	}
}

func (c *Confirmer) runOrLog(ctx context.Context) {
	if err := c.RunOnce(ctx); err != nil {
		// Logged every time rather than once. A confirmation that has been
		// failing for a week is a week of outcome numbers quietly not moving,
		// and the log is where that becomes visible.
		c.log.ErrorContext(ctx, "confirming work items", "error", err)
	}
}
