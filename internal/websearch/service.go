// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package websearch

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// Config is how web search is configured, as the service reads it.
type Config struct {
	Kind       Kind
	Enabled    bool
	GatewayURL string
	// Price is what one search costs, and PriceSource where that figure came
	// from, recorded on the request row the way a token rate's source is.
	Price       pricing.Money
	PriceSource string
}

// Builder constructs a backend from its configuration.
type Builder func(ctx context.Context, c Config) (Backend, error)

// Loader reads the configuration, and reports whether there is any.
type Loader func(ctx context.Context) (Config, bool, error)

// Service holds the backend the /mcp endpoint searches with.
//
// Resolved rather than held, for the reason integrations are: an
// administrator turns search on from a screen, and a backend fixed at boot
// would ignore them until a restart. It is reloaded on an interval and
// straight after a save, and the endpoint reads an atomic pointer - no query
// runs between a tool call arriving and the search starting.
type Service struct {
	load     Loader
	build    Builder
	interval time.Duration
	log      *slog.Logger

	// mu serialises reloads, so two of them cannot build the same backend
	// twice and publish in the wrong order.
	mu  sync.Mutex
	cur atomic.Pointer[State]
}

// State is what the service currently holds.
type State struct {
	Config     Config
	Configured bool
	// Backend is nil when search is unconfigured or the backend failed to
	// build, and BuildErr says which.
	Backend  Backend
	BuildErr error
}

// Serving reports whether a search can run: configured, switched on, and
// built.
func (s *State) Serving() bool {
	return s != nil && s.Configured && s.Config.Enabled && s.Backend != nil
}

// NewService builds a service. It holds nothing until Reload runs.
func NewService(load Loader, build Builder, interval time.Duration, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{load: load, build: build, interval: interval, log: log}
	s.cur.Store(&State{})
	return s
}

// Current returns what the service holds. A nil service holds nothing, which
// is a deployment with no web search.
func (s *Service) Current() *State {
	if s == nil {
		return &State{}
	}
	return s.cur.Load()
}

// Reload reads the configuration and rebuilds the backend when what it is
// built from has changed.
//
// A backend that fails to build is published as a failure rather than kept at
// its previous value: carrying on with the old gateway after an administrator
// changed it would be searching somewhere nobody configured. The error is
// returned as well as held, so the save that caused it can say so.
func (s *Service) Reload(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, configured, err := s.load(ctx)
	if err != nil {
		return fmt.Errorf("web search: load configuration: %w", err)
	}
	prev := s.cur.Load()
	next := &State{Config: cfg, Configured: configured}
	if !configured {
		s.cur.Store(next)
		return nil
	}

	sameTarget := prev.Configured && prev.Config.Kind == cfg.Kind && prev.Config.GatewayURL == cfg.GatewayURL
	if sameTarget && prev.BuildErr == nil && prev.Backend != nil {
		next.Backend = prev.Backend
		s.cur.Store(next)
		return nil
	}

	backend, berr := s.build(ctx, cfg)
	next.Backend, next.BuildErr = backend, berr
	s.cur.Store(next)
	if berr != nil {
		// Said once per failure rather than once per reload: the same broken
		// gateway every thirty seconds would bury the line that matters.
		if !sameTarget || prev.BuildErr == nil || prev.BuildErr.Error() != berr.Error() {
			s.log.ErrorContext(ctx, "web search backend could not be built",
				"backend", string(cfg.Kind), "gateway", cfg.GatewayURL, "error", berr)
		}
		return berr
	}
	s.log.InfoContext(ctx, "web search backend built",
		"backend", string(cfg.Kind), "gateway", cfg.GatewayURL, "enabled", cfg.Enabled)
	return nil
}

// Run reloads on the interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	if s == nil || s.interval <= 0 {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Reload(ctx); err != nil {
				s.log.WarnContext(ctx, "reloading web search", "error", err)
			}
		}
	}
}
