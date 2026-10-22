// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// Signing in to the console with a gateway token.
//
// A deployment without single sign-on is reached with a gateway token, which
// `panorama token create` mints where the deployment runs. The token already
// authenticates every /api/v1 route as its owner, so a browser holding it
// could do everything a console session does - it just could not hold it,
// which left an evaluator with a working API and a console they could not
// open. This exchanges the one for the other.
//
// It grants nothing new. The session it opens is the token owner's, with the
// owner's role, and lives exactly as long as any other console session; the
// token is not consumed, extended or recorded anywhere it was not already.
//
// It exists only while single sign-on does not. With an issuer configured the
// route is not registered, so an organization that put its console behind its
// identity provider - and that provider's MFA - does not have a second door
// beside it that a token pasted into a dotfile months ago would open.
//
// Three things keep it from being more than that:
//
//   - Only a personal gateway token (pnt_). A service token belongs to a team
//     and reaches no console route; a CLI grant buys nothing until exchanged;
//     a session or a setup code is not a token at all. Each is refused on
//     shape, before anything is looked up.
//   - A JSON body, and nothing else. An HTML form on another site can post
//     text/plain or a urlencoded body to this origin without a preflight, and
//     would sign a victim's browser in as the attacker - login CSRF, which
//     on this console means the victim's next token lands in somebody else's
//     account. application/json cannot be sent cross-origin without a
//     preflight, and Panorama answers none.
//   - The same per-address limiter as the setup code. A 256-bit token is not
//     guessable and the limiter does not pretend to protect it; it bounds the
//     database work and the log noise one address can cause.

// tokenSignInRequest is the body of POST /api/v1/auth/token.
type tokenSignInRequest struct {
	Token string `json:"token"`
}

// handleTokenSignIn opens a console session for the owner of a gateway token.
func (s *Server) handleTokenSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.setupLimiter.allow(clientIP(r)) {
		writeAPIError(w, r, http.StatusTooManyRequests,
			"Too many attempts. Wait a minute and try again.")
		return
	}

	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		writeAPIError(w, r, http.StatusUnsupportedMediaType, "Send the token as a JSON body.")
		return
	}

	var req tokenSignInRequest
	if !decodeBody(w, r, &req) {
		return
	}

	// A personal gateway token only, decided on shape so that anything else -
	// a stray Anthropic key pasted into the wrong field among them - is never
	// hashed or looked up.
	token := strings.TrimSpace(req.Token)
	if !auth.WellFormed(auth.Prefix, token) {
		s.refuseTokenSignIn(w, r, "not a gateway token")
		return
	}

	principal, err := AuthenticateGateway(r.Context(), s.db, token)
	if err != nil {
		if errors.Is(err, store.ErrTokenRejected) {
			// Unknown, expired, revoked and a disabled owner all read the same
			// from outside, so the response does not say which one it was.
			s.refuseTokenSignIn(w, r, "token rejected")
			return
		}
		s.fail(w, r, "authenticating a gateway token for sign-in", err)
		return
	}
	// The shape check has already excluded a service token. Asked again of the
	// principal itself, because a session for an account with no user would
	// be a session for nobody.
	if principal.Role == store.RoleService || principal.UserID == "" {
		s.refuseTokenSignIn(w, r, "not a personal token")
		return
	}

	if err := s.signIn(w, r, principal.UserID); err != nil {
		s.fail(w, r, "establishing a session", err)
		return
	}

	// At warn level, like the setup code: a console session opened without the
	// identity provider is the line somebody reviewing this deployment looks
	// for first.
	s.log.WarnContext(r.Context(), "console session opened with a gateway token",
		"user_id", principal.UserID, "email", principal.Email, "role", principal.Role,
		"token_id", principal.TokenID, "source_ip", clientIP(r),
		"request_id", RequestIDFrom(r.Context()),
		"note", "single sign-on is not configured; this door closes when it is")

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// refuseTokenSignIn answers every refusal identically, and logs which it was.
func (s *Server) refuseTokenSignIn(w http.ResponseWriter, r *http.Request, reason string) {
	s.log.WarnContext(r.Context(), "gateway token sign-in refused",
		"reason", reason, "source_ip", clientIP(r),
		"request_id", RequestIDFrom(r.Context()))
	writeAPIError(w, r, http.StatusUnauthorized,
		"That is not a gateway token this deployment accepts. Check that it was copied whole and has not expired or been revoked, or mint another with `panorama token create`.")
}
