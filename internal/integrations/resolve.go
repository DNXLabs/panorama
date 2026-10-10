// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package integrations

import (
	"context"
	"fmt"
)

// Builder turns one stored connection into a live source.
//
// It exists so this package does not have to import the concrete sources, which
// would make every deployment carry every integration's dependencies and would
// invert the direction the interface is meant to point in.
type Builder func(Connection) (Source, error)

// Connection is a connection as it was stored, with its credential already
// decrypted by the caller.
//
// The plaintext lives in this struct for exactly as long as it takes to build a
// source. It is never logged, never serialised, and never returned by an API -
// the store deliberately reads it through its own function so that the ordinary
// path of listing integrations cannot pick one up by accident.
type Connection struct {
	Kind     Kind
	Provider string

	AppID          string
	InstallationID string
	APIBaseURL     string

	// Secret is the decrypted credential, and SecretKind says what it is.
	Secret     string
	SecretKind string

	// Fixtures is the file a mock reads.
	Fixtures string
}

// Resolver builds the current registry on demand.
//
// Not assembled once at startup: an administrator connects GitHub from a
// screen, so a registry fixed at boot would mean the confirmation job kept
// using whatever was configured when the process started - and a connection
// made at ten past the hour would do nothing until somebody restarted the
// deployment.
//
// So it is resolved rather than held: once per confirmation pass, and once per
// console request that needs to test a connection. That is a handful of queries
// an hour, against a table with at most two rows.
type Resolver struct {
	load  func(context.Context) ([]Connection, error)
	build Builder
	// fixed short-circuits resolution for a Static resolver.
	fixed *Registry
}

// Static returns a resolver over a fixed set of sources.
//
// For callers that already hold their sources and are not reading them from
// anywhere - tests about confirmation rather than about resolution, and any
// future path where the set genuinely cannot change at runtime.
func Static(sources ...Source) *Resolver {
	registry := NewRegistry(sources...)
	return &Resolver{fixed: registry}
}

// NewResolver builds a resolver from a loader and a builder.
func NewResolver(load func(context.Context) ([]Connection, error), build Builder) *Resolver {
	return &Resolver{load: load, build: build}
}

// Registry assembles the sources that are currently connected.
//
// A connection that cannot be built - a credential that will not decrypt, a
// provider that is no longer known - is reported rather than skipped. Silently
// dropping it would leave the console showing an integration that the
// confirmation job had quietly stopped using.
func (r *Resolver) Registry(ctx context.Context) (*Registry, error) {
	if r == nil {
		return NewRegistry(), nil
	}
	if r.fixed != nil {
		return r.fixed, nil
	}

	connections, err := r.load(ctx)
	if err != nil {
		return nil, err
	}

	var (
		sources []Source
		errs    []error
	)
	for _, c := range connections {
		source, berr := r.build(c)
		if berr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Kind, berr))
			continue
		}
		sources = append(sources, source)
	}

	registry := NewRegistry(sources...)
	if len(errs) > 0 {
		return registry, fmt.Errorf("integrations: %d connection(s) could not be built: %v", len(errs), errs)
	}
	return registry, nil
}
