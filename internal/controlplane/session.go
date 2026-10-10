// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// Cookies the console uses.
//
// The names are prefixed with __Host- when the deployment is served over
// HTTPS. That prefix is enforced by the browser rather than by us: it refuses
// the cookie unless it is Secure, path-scoped to /, and carries no Domain
// attribute, which is what stops a subdomain the organisation does not control
// from setting a session cookie the console would then trust.
const (
	sessionCookie = "panorama_session"
	// setupCookie holds a redeemed setup code's session. A separate cookie from
	// the console session so the two can never be mistaken for one another, and
	// so ending one does not disturb the other.
	setupCookie = "panorama_setup"
	oidcCookie  = "panorama_oidc"
)

// csrfHeader is the header the console echoes its CSRF token back in.
const csrfHeader = "X-Panorama-CSRF"

// Session lifetimes.
const (
	// sessionIdle is how long a console session survives without being used.
	// It slides on every request, so a console left open through a working day
	// does not sign itself out mid-task.
	sessionIdle = 12 * time.Hour
	// sessionAbsolute caps the sliding window. A browser that has been kept
	// alive for a month has outlived any reasonable claim that the person who
	// signed in is still the one at the keyboard.
	sessionAbsolute = 30 * 24 * time.Hour
	// touchInterval keeps the sliding expiry from writing on every request.
	// Losing a few minutes of slide costs nothing; a write per request on the
	// console's polling loop costs a row lock.
	touchInterval = 5 * time.Minute
)

// secureCookies reports whether this deployment is served over HTTPS, which
// decides whether cookies may carry the Secure attribute and the __Host-
// prefix.
//
// A deployment on plain HTTP - an evaluation on a laptop - cannot use either,
// and pretending otherwise would produce cookies the browser silently drops.
func (s *Server) secureCookies() bool {
	return strings.HasPrefix(s.cfg.Server.BaseURL, "https://")
}

// cookieName returns the name a cookie is actually set under.
func (s *Server) cookieName(base string) string {
	if s.secureCookies() {
		return "__Host-" + base
	}
	return base
}

// setCookie writes one of the console's cookies.
func (s *Server) setCookie(w http.ResponseWriter, base, value string, ttl time.Duration, httpOnly bool) {
	// #nosec G124 -- Secure and HttpOnly are set from secureCookies() and the
	// caller rather than as literals, which the check cannot see. Secure is
	// conditional because a browser silently drops a Secure cookie on a plain
	// HTTP origin, and an evaluation deployment on a laptop is one; HttpOnly is
	// a parameter because the OIDC state cookie and the session cookie both
	// want it and nothing here sets it false.
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(base),
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: httpOnly,
		Secure:   s.secureCookies(),
		// Lax rather than Strict because the OIDC callback is a top-level
		// navigation from the identity provider, and Strict would strip the
		// cookie from exactly that request. Lax still withholds it from
		// cross-site POSTs, and the CSRF header covers what is left.
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCookie removes one of the console's cookies.
func (s *Server) clearCookie(w http.ResponseWriter, base string) {
	// #nosec G124 -- same as setCookie; this one carries no value at all.
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(base),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) readCookie(r *http.Request, base string) string {
	c, err := r.Cookie(s.cookieName(base))
	if err != nil {
		return ""
	}
	return c.Value
}

// signIn establishes a console session and sets its cookies.
//
// It returns only an error. The session it creates is reachable through the
// cookie it just set, and both callers discarded the value - a signature that
// hands back a record nobody reads invites the next caller to read it from here
// rather than from the one place sessions are authenticated.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, userID string) error {
	secret, err := auth.NewSecret(auth.SessionPrefix)
	if err != nil {
		return err
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		return err
	}

	_, err = s.db.CreateConsoleSession(r.Context(), userID, secret.Hash, csrf,
		r.UserAgent(), clientIP(r), time.Now().UTC().Add(sessionIdle))
	if err != nil {
		return err
	}

	s.setCookie(w, sessionCookie, secret.Plaintext, sessionIdle, true)

	// The CSRF token is not set as a cookie. The console reads it from
	// GET /api/v1/auth/session, which no cross-origin page can read because
	// Panorama sends no CORS headers at all - so the value stays a thing only
	// a same-origin page can obtain, which is the whole requirement.
	return nil
}

// signOut revokes the current session and clears its cookies.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request) error {
	value := s.readCookie(r, sessionCookie)
	s.clearCookie(w, sessionCookie)

	if value == "" || !auth.WellFormed(auth.SessionPrefix, value) {
		return nil
	}
	_, session, err := s.db.AuthenticateConsoleSession(r.Context(), auth.Hash(value))
	if err != nil {
		// Already gone. Clearing the cookies is the whole job.
		return nil
	}
	return s.db.RevokeConsoleSession(r.Context(), session.ID)
}

// errNoCredential means the request carried nothing that could authenticate it.
var errNoCredential = errors.New("controlplane: no credential presented")

// authenticateRequest resolves who is calling, from a session cookie or a
// gateway token.
//
// Two credentials, because they serve two callers. A browser holds a cookie,
// which is ambient and therefore needs CSRF protection; a script or a CLI holds
// a gateway token, which is not sent automatically and therefore does not. The
// cookie is tried first: a browser that has both should act as the person who
// signed in.
func (s *Server) authenticateRequest(w http.ResponseWriter, r *http.Request) (*store.Principal, error) {
	// Tried first. A browser holding both is a browser mid-setup, and the setup
	// session is the one that was deliberately opened most recently - though
	// startSetupSession clears the other, so this is a belt on top of braces
	// rather than the mechanism.
	if value := s.readCookie(r, setupCookie); value != "" {
		if !auth.WellFormed(auth.SetupSessionPrefix, value) {
			s.clearCookie(w, setupCookie)
			return nil, store.ErrSetupRejected
		}

		principal, session, err := s.db.AuthenticateSetupSession(r.Context(), auth.Hash(value))
		if err != nil {
			if errors.Is(err, store.ErrSetupRejected) {
				s.clearCookie(w, setupCookie)
			}
			return nil, err
		}

		// The same double-submit rule as a console session: the cookie is
		// ambient, so a write has to prove the page made it.
		if err := s.checkCSRFToken(r, session.CSRFToken); err != nil {
			return nil, err
		}
		// Deliberately not slid. A setup session expires when it expires.
		return principal, nil
	}

	if value := s.readCookie(r, sessionCookie); value != "" {
		if !auth.WellFormed(auth.SessionPrefix, value) {
			// A malformed cookie is stale or forged; either way it will never
			// authenticate, and leaving it set makes every subsequent request
			// fail the same way.
			s.clearCookie(w, sessionCookie)
			return nil, store.ErrSessionRejected
		}

		principal, session, err := s.db.AuthenticateConsoleSession(r.Context(), auth.Hash(value))
		if err != nil {
			if errors.Is(err, store.ErrSessionRejected) {
				s.clearCookie(w, sessionCookie)
			}
			return nil, err
		}

		if err := s.checkCSRF(r, session); err != nil {
			return nil, err
		}
		s.slide(r, session)
		return principal, nil
	}

	token, err := auth.Parse(r.Header.Get("Authorization"), r.Header.Get("x-api-key"))
	if err != nil {
		return nil, errNoCredential
	}
	// A service token authenticates here and then reaches nothing: RoleService
	// is on no route's requireRole list, so every console endpoint refuses it,
	// including any added later. Resolving it rather than rejecting it outright
	// is what makes that refusal a 403 with a reason instead of a confusing 401.
	return AuthenticateGateway(r.Context(), s.db, token)
}

// errCSRF is returned when a cookie-authenticated write does not carry a
// matching CSRF token.
var errCSRF = errors.New("controlplane: csrf token missing or wrong")

// checkCSRF enforces the double-submit token on state-changing requests.
//
// Only for cookie authentication, and only for unsafe methods. A gateway token
// is never attached by the browser on someone else's behalf, so there is
// nothing to forge; and a GET that changes state would be a bug worth fixing
// rather than a case to defend here.
func (s *Server) checkCSRF(r *http.Request, session *store.ConsoleSession) error {
	return s.checkCSRFToken(r, session.CSRFToken)
}

// checkCSRFToken is the comparison itself, shared by both cookie-borne
// credentials. One derivation, so a setup session cannot end up with a weaker
// rule than a console session by having been written separately.
func (s *Server) checkCSRFToken(r *http.Request, want string) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	presented := r.Header.Get(csrfHeader)
	if presented == "" || !auth.SecretsMatch(presented, want) {
		return errCSRF
	}
	return nil
}

// slide extends a session that is being used.
//
// Best effort and off the response path in spirit: a failed extension costs a
// user an early sign-out at worst, and is not worth failing their request over.
func (s *Server) slide(r *http.Request, session *store.ConsoleSession) {
	now := time.Now().UTC()
	if now.Sub(session.LastSeenAt) < touchInterval {
		return
	}

	expires := now.Add(sessionIdle)
	if ceiling := session.CreatedAt.Add(sessionAbsolute); expires.After(ceiling) {
		expires = ceiling
	}
	if err := s.db.TouchConsoleSession(r.Context(), session.ID, expires); err != nil {
		s.log.WarnContext(r.Context(), "extending console session", "error", err)
	}
}

// clientIP reports the address a request came from, for the session list.
//
// Deliberately not trusting X-Forwarded-For: behind a proxy this records the
// proxy, which is honest, whereas trusting a header any client can set records
// a fiction. Making it configurable is a v1.x question.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
