// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/controlplane"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/tools"
)

// PrincipalFrom returns the authenticated principal for a request, if any.
//
// The context key belongs to the control plane, which both surfaces already
// depend on, so there is exactly one notion of who is calling.
func PrincipalFrom(ctx context.Context) *store.Principal {
	return controlplane.PrincipalFrom(ctx)
}

// authenticated wraps a handler so it only runs for a valid gateway token.
//
// Every failure returns the same message. Distinguishing "no such token" from
// "revoked" or "expired" would tell someone probing the endpoint which of
// their guesses was closest, and tells a legitimate user nothing they can act
// on that "check your token" does not.
//
// The failure is written in the surface's own error shape and names the
// variable that surface's tools read, because a message pointing a Codex user
// at ANTHROPIC_AUTH_TOKEN is a message that sends them to the wrong setting.
func (d *DataPlane) authenticated(sf *surface, next http.Handler) http.Handler {
	return d.authenticatedBy(func(*http.Request) *surface { return sf }, next)
}

// authenticatedBy is authenticated for a route shared by more than one surface,
// where the request itself says which shape its client reads.
func (d *DataPlane) authenticatedBy(pick func(*http.Request) *surface, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := controlplane.RequestIDFrom(r.Context())
		sf := pick(r)

		token, err := auth.Parse(r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
		if err != nil {
			d.log.WarnContext(r.Context(), "gateway token rejected",
				"reason", "malformed",
				"request_id", requestID,
				"tool", tools.FromUserAgent(r.UserAgent()),
			)
			sf.writeError(w, http.StatusUnauthorized, errTypeAuthentication, sf.unauthenticatedMessage(), requestID)
			return
		}

		principal, err := controlplane.AuthenticateGateway(r.Context(), d.store, token)
		if err != nil {
			// A store failure and a rejected token are different problems and
			// are logged differently, but they look identical to the client.
			if errors.Is(err, store.ErrTokenRejected) {
				d.log.WarnContext(r.Context(), "gateway token rejected",
					"reason", "unknown, revoked, expired, or user disabled",
					"request_id", requestID,
					"tool", tools.FromUserAgent(r.UserAgent()),
				)
				sf.writeError(w, http.StatusUnauthorized, errTypeAuthentication,
					"This Panorama gateway token is not valid. It may have been revoked or "+
						"expired - generate a new one from your Panorama dashboard.", requestID)
				return
			}

			d.log.ErrorContext(r.Context(), "authenticating gateway token",
				"error", err, "request_id", requestID)
			sf.writeError(w, http.StatusServiceUnavailable, errTypeAPI,
				"Panorama could not verify your token right now. Try again shortly.", requestID)
			return
		}

		ctx := controlplane.WithPrincipal(r.Context(), principal)
		// From here on every log line in this request carries the identity
		// fields the conventions require.
		ctx = withLogContext(ctx, principal, r)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type logAttrsKey struct{}

// withLogContext attaches the structured fields every metric and log line is
// supposed to carry. sessionId joins them in M4, once session grouping exists.
func withLogContext(ctx context.Context, p *store.Principal, r *http.Request) context.Context {
	attrs := []slog.Attr{
		slog.String("userId", p.UserID),
		slog.String("tool", tools.FromUserAgent(r.UserAgent())),
	}
	// A service principal has no user, so a log line about CI traffic would
	// otherwise carry an empty identity. The conventions ask for teamId
	// wherever it is known, and here it is the only identity there is.
	if p.TeamID != "" {
		attrs = append(attrs, slog.String("teamId", p.TeamID))
	}
	return context.WithValue(ctx, logAttrsKey{}, attrs)
}

// logAttrs returns the request-scoped structured fields.
func logAttrs(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	return attrs
}
