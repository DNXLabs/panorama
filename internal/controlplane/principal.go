// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"

	"github.com/dnxlabs/panorama/internal/store"
)

const principalKey contextKey = iota + 1

// withPrincipal attaches the authenticated identity to a request context.
//
// The key lives here, in the package both surfaces already depend on, so the
// gateway and the console cannot end up with two notions of who is calling -
// which is the kind of divergence that turns into an authorization bug rather
// than a compile error.
func withPrincipal(ctx context.Context, p *store.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// WithPrincipal attaches the authenticated identity to a request context. It is
// exported for the data plane, which authenticates the same tokens against a
// different error format.
func WithPrincipal(ctx context.Context, p *store.Principal) context.Context {
	return withPrincipal(ctx, p)
}

// PrincipalFrom returns the authenticated identity, or nil outside an
// authenticated request.
func PrincipalFrom(ctx context.Context) *store.Principal {
	p, _ := ctx.Value(principalKey).(*store.Principal)
	return p
}
