// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dnxlabs/panorama/internal/store"
)

// The development sign-in.
//
// This is the one door in Panorama that opens a console session without an
// identity provider, and everything about it is arranged so that it cannot be
// open anywhere it should not be:
//
//   - The route is not registered unless `dev.sign_in_as` names an account, so
//     on every other deployment it is a 404 from the mux rather than a handler
//     that decides to refuse. A door that does not exist cannot be probed for,
//     mis-configured, or reached by a bug in a check.
//   - Config validation refuses to start at all if it is set while
//     `server.base_url` is anything but loopback, so the environment variable
//     escaping from a development compose file into a production one is a
//     deployment that does not boot rather than one that quietly works.
//   - It is announced at startup and logged at every use, at WARN, naming the
//     account. A session that nobody authenticated should be the loudest line
//     in the log.
//
// What it exists for: the console cannot be reviewed without being signed in
// to, and signing in otherwise means real credentials at a real identity
// provider - which a test, CI, or a coding agent asked to check a change to a
// screen does not have.

// handleDevSignIn opens a console session for the configured account.
//
// Unauthenticated, and there is no pretending otherwise - it is an
// administrator session for the asking. What stands in for authentication is
// that the deployment is reachable only from the machine it runs on, which is
// established at startup rather than here: a check in this handler would be one
// bug away from being wrong, and by the time a request arrives it is too late
// to tell an operator they have made a mistake.
func (s *Server) handleDevSignIn(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(s.cfg.Dev.SignInAs))

	// An administrator, pinned as a deliberate assignment. `manual` rather than
	// `sso` because the role did not come from a group mapping and the next
	// sign-in must not recompute it - a development account is in no group, so
	// a recomputed role would fall to the default and the second sign-in would
	// show a reviewer a different console from the first.
	user, _, err := s.db.UpsertIdentity(r.Context(), store.Identity{
		// The subject is derived from the email rather than issued by anyone,
		// which is exactly what makes this a development affordance: there is
		// no authority behind it. Prefixed so it cannot collide with a real
		// provider's subject for the same person, which would otherwise let
		// this quietly take over an SSO account.
		Subject:     "panorama-dev:" + email,
		Email:       email,
		DisplayName: strings.SplitN(email, "@", 2)[0],
	}, store.RoleAdmin, store.RoleSourceManual)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			s.log.ErrorContext(r.Context(), "development sign-in refused", "error", err, "email", email)
			writeAPIError(w, r, http.StatusBadRequest, "dev.sign_in_as is not usable as an account.")
			return
		}
		s.fail(w, r, "provisioning the development account", err)
		return
	}

	if err := s.signIn(w, r, user.ID); err != nil {
		s.fail(w, r, "establishing a session", err)
		return
	}

	s.log.WarnContext(r.Context(), "DEVELOPMENT SIGN-IN - a console session was opened with no identity provider",
		"email", email, "user_id", user.ID, "role", user.Role,
		"note", "dev.sign_in_as is set; this deployment must be a development stack")

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
