// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// AuthenticateGateway resolves a presented gateway credential to its principal.
//
// The dispatch lives here, once, because both surfaces need it: the data plane
// authenticates model requests, and the control plane accepts a gateway token
// for scripts and for a deployment that has no single sign-on yet. Two copies
// of this switch would eventually disagree about which credentials are
// spendable, and the permissive copy would win silently.
//
// Which table to consult is decided from the token's prefix, before any query
// is built, so authentication stays exactly one indexed lookup. auth.Parse has
// already refused anything that is not a gateway credential - a CLI grant
// included - so the default arm here is unreachable in practice and exists
// because a switch on a credential kind should never fall through to "allow".
func AuthenticateGateway(ctx context.Context, db *store.DB, token string) (*store.Principal, error) {
	hash := auth.Hash(token)

	switch auth.KindOf(token) {
	case auth.KindService:
		return db.AuthenticateServiceToken(ctx, hash)
	case auth.KindUser:
		return db.AuthenticateToken(ctx, hash)
	default:
		return nil, store.ErrTokenRejected
	}
}
