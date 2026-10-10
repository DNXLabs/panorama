// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// setupCodeKey is the settings key holding the hash of the outstanding setup
// code.
const setupCodeKey = "setup.code_hash"

// setupCodeIssuedKey records when the outstanding code was minted, RFC 3339.
//
// Only the hash of a code is stored, so an outstanding code cannot be shown to
// anybody - not even to the operator who is entitled to it. That makes "a code
// already exists" a dead end rather than an answer: the one on the sign-in gate
// may have scrolled out of a log, been minted into an exec session that never
// reached the log group, or belonged to a task that no longer exists. This is
// what lets the gate tell a code somebody is mid-way through using from one
// nobody will ever read.
const setupCodeIssuedKey = "setup.code_issued_at"

// setupCodeGrace is how long an outstanding code is left alone.
//
// Long enough to cover the only loop that matters - load the gate, find the
// code in the logs, come back and paste it - with room for CloudWatch's own
// delivery lag. Past it the code is assumed lost and the next page load mints
// another, which is the whole point: a code nobody can read should not be able
// to lock the deployment out of issuing one that somebody can.
//
// It is a window rather than a switch because both extremes are wrong. Never
// replacing strands whoever lost the code. Replacing on every request hands
// anyone who can reach this unauthenticated endpoint - and it is internet
// facing on a normal deployment - a button that invalidates the code an
// operator is in the middle of typing, forever.
const setupCodeGrace = 15 * time.Minute

// setupIdle bounds a setup session.
//
// Long enough to configure a deployment and short enough that one left open in
// a closed tab stops mattering. Not slid on activity: this is the one principal
// Panorama manages itself, and a window that renewed itself would turn it into
// a standing login, which is what it was designed not to be.
const setupIdle = 60 * time.Minute

// PrepareSetup makes a deployment nobody can sign in to configurable.
//
// The problem is circular twice over. Before an identity provider is
// configured, the console is authenticated and there is nobody to authenticate
// as; afterwards, a group mapping that admits nobody leaves nobody who can edit
// the mapping. The only thing that breaks either circle is proof of access to
// the deployment itself - which is what reading the server's own log is.
//
// So: on a deployment nobody has signed in to, a one-time code is minted and
// logged. It is stored hashed, exactly as every other credential here, and
// redeeming it opens a session that can configure the deployment and create no
// account at all. Once anybody has signed in the code is destroyed, because
// from then on there is a person who can do this from the console - and
// `panorama admin setup` issues a fresh one if that stops being true.
//
// Only issued when none is outstanding. Regenerating on every start would let a
// restart silently invalidate a code an operator is halfway through using, and
// would mean two replicas of a fresh deployment each logged a different code
// and only one of them worked.
func PrepareSetup(ctx context.Context, db *store.DB, log *slog.Logger) error {
	users, err := db.CountUsers(ctx)
	if err != nil {
		return err
	}

	if users > 0 {
		removed, derr := db.DeleteSetting(ctx, setupCodeKey)
		if derr != nil {
			return derr
		}
		if removed {
			log.InfoContext(ctx, "setup code destroyed; somebody can sign in to this deployment now")
		}
		return nil
	}

	switch _, serr := db.Setting(ctx, setupCodeKey); {
	case serr == nil:
		log.WarnContext(ctx, "this deployment is not set up and a setup code is already outstanding",
			"hint", "run `panorama admin setup` to issue a fresh one; the old one stops working")
		return nil
	case !errors.Is(serr, store.ErrNotFound):
		return serr
	}

	code, err := IssueSetupCode(ctx, db)
	if err != nil {
		return err
	}
	logSetupCode(ctx, log, code)
	return nil
}

// outstandingCodeIsFresh reports whether the outstanding code was minted
// recently enough that somebody is plausibly still using it.
//
// An unreadable timestamp is treated as stale rather than fresh. The failure it
// guards against is a deployment that cannot issue a code anybody can read, and
// refusing to mint on the strength of a value this cannot parse would be
// choosing that failure over an extra code in a log.
func outstandingCodeIsFresh(ctx context.Context, db *store.DB) bool {
	at, err := db.Setting(ctx, setupCodeIssuedKey)
	if err != nil {
		return false
	}
	issued, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return false
	}
	return time.Since(issued) < setupCodeGrace
}

// IssueSetupCode mints a setup code, replacing any outstanding one.
//
// Not refused on a deployment that already has users. That is the point: the situation it answers is a deployment full
// of people none of whom can sign in. What keeps it acceptable is what it
// opens - no account, no password, an hour, and nothing reachable but this
// deployment's own configuration.
func IssueSetupCode(ctx context.Context, db *store.DB) (string, error) {
	secret, err := auth.NewSecret(auth.SetupCodePrefix)
	if err != nil {
		return "", err
	}
	// The hash is stored, not the code. A database disclosure on a deployment
	// mid-setup should not hand over the ability to reconfigure it.
	if err := db.SetSetting(ctx, setupCodeKey, secret.Hash); err != nil {
		return "", err
	}
	if err := db.SetSetting(ctx, setupCodeIssuedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", err
	}
	return secret.Plaintext, nil
}

// logSetupCode writes the code where the operator will find it.
//
// At warn level and with the whole instruction in one record, because this is
// the single line that decides whether a fresh deployment is usable, and it
// will be read in a scrollback of JSON.
func logSetupCode(ctx context.Context, log *slog.Logger, code string) {
	log.WarnContext(ctx, "PANORAMA IS NOT SET UP - open the console and enter this one-time code",
		"code", code,
		"note", "it works once, opens a session that configures this deployment, and creates no account",
	)
}

// setupRequest is what redeeming asks for: the code, and nothing else.
//
// No email, no name, no password. There is no account to describe - the first
// person to hold one arrives from the identity provider this session exists to
// configure.
type setupRequest struct {
	Code string `json:"code"`
}

// ensureSetupCode mints a code when the deployment needs one and none is
// outstanding, and logs it where the operator will find it.
//
// Minting only at startup would leave the obvious case with no answer: a code
// that was consumed - by finishing a setup session, or by ending one - could
// not be replaced without running a one-off task against the cluster, and the
// image has no shell to get one from. The page an operator is already staring
// at is the cheapest possible place to ask for another.
//
// It does NOT replace an outstanding code, and that restraint is the whole
// reason this is safe to do from an unauthenticated handler. Minting on every
// load would mean a second browser tab, a link unfurler or a crawler silently
// invalidated the code somebody was halfway through typing - which is the
// failure PrepareSetup already refuses for the same reason - and would hand
// anyone who can reach the console an endless credential-churn button. Minting
// only into the gap makes a repeated request a read, so hammering it changes
// nothing after the first.
func ensureSetupCode(ctx context.Context, db *store.DB, log *slog.Logger) error {
	switch _, err := db.Setting(ctx, setupCodeKey); {
	case err == nil && outstandingCodeIsFresh(ctx, db):
		// Said rather than passed over in silence, because from outside the two
		// outcomes are indistinguishable: a gate that declines to mint and a
		// gate that is broken both produce a log with no code in it. The only
		// way to tell them apart was to try redeeming a code and see whether it
		// worked, which is not a diagnostic anybody should have to invent.
		//
		// At info, and the volume is bounded twice over - only on a deployment
		// nobody has signed in to, and only for as long as that is true.
		log.InfoContext(ctx, "a setup code was issued moments ago; the sign-in gate issued no new one",
			"hint", "it is the code in the line above this one. Reload after "+setupCodeGrace.String()+" and a fresh one is issued",
		)
		return nil
	// err != nil is load-bearing and was missing. A stale code leaves err nil,
	// and `!errors.Is(nil, ErrNotFound)` is true - so this arm caught the stale
	// path and returned before minting, making the whole grace window a no-op.
	// Its own test is what found that.
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}

	code, err := IssueSetupCode(ctx, db)
	if err != nil {
		return err
	}
	logSetupCode(ctx, log, code)
	return nil
}

// handleAuthState reports what a visitor needs to know before signing in.
//
// One of the two unauthenticated console endpoints, and deliberately thin: it
// answers whether the deployment has been set up and whether SSO is available,
// and nothing else. Both facts are visible to anyone who can reach the console
// anyway - a sign-in page that offers an SSO button has already told you SSO is
// configured - and neither is organizational data.
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	state := map[string]any{
		"sso_configured": s.cfg.OIDC.Configured(),
		"base_url":       s.cfg.Server.BaseURL,
		// Reported so the gate can offer the door that exists. Saying so
		// discloses nothing: the route is only registered on a deployment
		// nothing outside this machine can reach, and on every other one this
		// is false because the handler is not there to be found.
		"dev_sign_in": s.cfg.Dev.Enabled(),
	}

	users, err := s.db.CountUsers(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "reading deployment state", "error", err)
		writeAPIError(w, r, http.StatusServiceUnavailable, "Panorama could not read its own state.")
		return
	}
	state["setup_required"] = users == 0

	// A deployment nobody has signed in to needs a code, and whoever is loading
	// this is looking at the box it goes in. Failing to mint one is logged and
	// does not fail the response: the gate still has to render, and `panorama
	// admin setup` is still there. Swallowing it silently is what would be
	// wrong.
	if users == 0 {
		if err := ensureSetupCode(r.Context(), s.db, s.log); err != nil {
			s.log.ErrorContext(r.Context(), "issuing a setup code for the sign-in gate", "error", err)
		}
	}

	writeJSON(w, http.StatusOK, state)
}

// handleSetupSession redeems a setup code and opens a setup session.
//
// Unauthenticated, because the premise is that nobody can authenticate. What
// stands in for it is a 256-bit code that exists only where somebody with
// access to this deployment put it.
func (s *Server) handleSetupSession(w http.ResponseWriter, r *http.Request) {
	if !s.setupLimiter.allow(clientIP(r)) {
		writeAPIError(w, r, http.StatusTooManyRequests,
			"Too many attempts. Wait a minute and try again.")
		return
	}

	var req setupRequest
	if !decodeBody(w, r, &req) {
		return
	}

	// Shape-checked before anything is looked up, so a value that could not be
	// one of ours is never hashed or used as a query parameter.
	code := strings.TrimSpace(req.Code)
	if !auth.WellFormed(auth.SetupCodePrefix, code) {
		s.refuseSetup(w, r, "malformed")
		return
	}

	stored, err := s.db.Setting(r.Context(), setupCodeKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.refuseSetup(w, r, "no code outstanding")
			return
		}
		s.log.ErrorContext(r.Context(), "reading setup code", "error", err)
		writeAPIError(w, r, http.StatusServiceUnavailable, "Panorama could not read its own state.")
		return
	}

	if !auth.SecretsMatch(auth.Hash(code), stored) {
		s.refuseSetup(w, r, "wrong code")
		return
	}

	// Consumed atomically, so two simultaneous redemptions - or two replicas -
	// produce exactly one session.
	claimed, err := s.db.ClaimSetting(r.Context(), setupCodeKey, stored)
	if err != nil {
		s.log.ErrorContext(r.Context(), "claiming setup code", "error", err)
		writeAPIError(w, r, http.StatusServiceUnavailable, "Panorama could not complete setup.")
		return
	}
	if !claimed {
		s.refuseSetup(w, r, "already claimed")
		return
	}

	session, err := s.startSetupSession(w, r)
	if err != nil {
		s.log.ErrorContext(r.Context(), "opening a setup session", "error", err)
		writeAPIError(w, r, http.StatusInternalServerError, "The code was accepted but the session was not. Issue another.")
		return
	}

	// At warn level. This is the most privileged thing that can happen to this
	// deployment's configuration, and the question afterwards is always when and
	// from where.
	s.log.WarnContext(r.Context(), "SETUP SESSION OPENED - this deployment is configurable without signing in",
		"session_id", session.ID, "source_ip", clientIP(r),
		"expires_at", session.ExpiresAt.Format(time.RFC3339),
		"note", "the code is destroyed; issue another with `panorama admin setup`")

	writeJSON(w, http.StatusOK, map[string]any{
		"user":       renderPrincipal(&store.Principal{Kind: store.PrincipalKindSetup, Role: store.RoleSetup}),
		"csrf_token": session.CSRFToken,
		"expires_at": session.ExpiresAt,
	})
}

// startSetupSession mints the cookie and the row behind it.
func (s *Server) startSetupSession(w http.ResponseWriter, r *http.Request) (*store.SetupSession, error) {
	secret, err := auth.NewSecret(auth.SetupSessionPrefix)
	if err != nil {
		return nil, err
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		return nil, err
	}

	// Any earlier setup session ends here. Exactly one should ever be live, and
	// a code minted, forgotten, and redeemed later should not find a second one
	// still open beside it.
	if _, eerr := s.db.EndSetupSessions(r.Context()); eerr != nil {
		return nil, eerr
	}

	session, err := s.db.CreateSetupSession(r.Context(), secret.Hash, csrf,
		clientIP(r), time.Now().UTC().Add(setupIdle))
	if err != nil {
		return nil, err
	}

	// A setup session replaces whatever else this browser held. Being in setup
	// is a mode, not a second tab, and leaving a console session set beside it
	// would make which one answered a question of cookie ordering.
	s.clearCookie(w, sessionCookie)
	s.setCookie(w, setupCookie, secret.Plaintext, setupIdle, true)
	return session, nil
}

// refuseSetup answers every failed redemption identically.
//
// The reason is logged and never returned. Telling a caller that the code was
// right but already claimed, or that none is outstanding, narrows the search for
// anyone trying them.
func (s *Server) refuseSetup(w http.ResponseWriter, r *http.Request, reason string) {
	s.log.WarnContext(r.Context(), "setup code refused",
		"reason", reason, "source_ip", clientIP(r),
		"request_id", RequestIDFrom(r.Context()))
	writeAPIError(w, r, http.StatusUnauthorized,
		"That code is not valid. Check the server log for the current one, or run `panorama admin setup`.")
}

// endSetupSession closes a setup session.
func (s *Server) endSetupSession(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.EndSetupSessions(r.Context()); err != nil {
		s.fail(w, r, "ending the setup session", err)
		return
	}
	s.clearCookie(w, setupCookie)
	s.log.InfoContext(r.Context(), "setup session ended")
	writeJSON(w, http.StatusOK, map[string]any{"signed_out": true})
}

// attemptLimiter is a fixed-window counter per source address.
//
// A 256-bit code is not brute-forceable and this does not pretend to be the
// thing protecting it. What it does is keep an unauthenticated endpoint from
// being a free way to make a deployment do database work, and put a bound on
// how much noise one address can put in the log.
type attemptLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	seen    map[string]*attemptWindow
	nowFunc func() time.Time
}

type attemptWindow struct {
	start time.Time
	count int
}

func newAttemptLimiter(limit int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{
		window: window, limit: limit,
		seen:    map[string]*attemptWindow{},
		nowFunc: time.Now,
	}
}

func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFunc()

	// Swept here rather than on a timer: the map only grows while someone is
	// hammering the endpoint, and every call is already holding the lock.
	for k, w := range l.seen {
		if now.Sub(w.start) > l.window {
			delete(l.seen, k)
		}
	}

	w, ok := l.seen[key]
	if !ok || now.Sub(w.start) > l.window {
		l.seen[key] = &attemptWindow{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// administratorsRequest changes who administers this deployment.
type administratorsRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// handleSetupAdministrators lists who holds which role.
//
// Narrow on purpose. The people screen carries teams, token counts, and when
// somebody last made a request; none of that is any of a setup session's
// business, and reusing that endpoint would have handed it over for the sake of
// four fields.
//
// It exists because "the first sign-in owns the deployment" can put the wrong
// person in charge - a colleague who reached the URL first. Proof of access to
// the deployment is the right authority to fix that, and it is the same
// authority that could already grant admin by mapping a group to it.
func (a *API) handleSetupAdministrators(w http.ResponseWriter, r *http.Request) {
	users, err := a.db.ListUsers(r.Context())
	if err != nil {
		a.fail(w, r, "listing users", err)
		return
	}

	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id":           u.ID,
			"email":        u.Email,
			"display_name": u.DisplayName,
			"role":         u.Role,
			"status":       u.Status,
			// Whether the group mapping still decides this person's role. A
			// role set here is recorded as a decision a person made, so it is
			// not recomputed at their next sign-in - this says which accounts
			// are in that state.
			"role_source": u.RoleSource,
			// Whether this account has ever completed an SSO round trip.
			"sso_linked": u.ExternalID != "",
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"users": out,
		// Stated rather than left for the console to infer. A role assigned
		// here outlives a sign-in; a role the group mapping granted does not,
		// and the difference is the whole reason this endpoint is safe to
		// offer from a session that creates no account.
		"assigned_roles_survive_sign_in": true,
	})
}

// handleSetSetupAdministrator changes one person's role.
//
// A role set here is not durable for anybody who signs in through SSO:
// provision recomputes it from group membership at every sign-in, so the group
// mapping is what makes it stick. That is not a defect to route around - it is
// what "Panorama does not own identities" means - so the console says it, and
// this grant is for the minutes between finding the wrong person in charge and
// fixing the mapping properly.
func (a *API) handleSetSetupAdministrator(w http.ResponseWriter, r *http.Request) {
	var req administratorsRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if !store.ValidRole(req.Role) {
		writeAPIError(w, r, http.StatusBadRequest, "Role must be admin, manager, or developer.")
		return
	}

	target, err := a.db.UserByID(r.Context(), req.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "No such user.")
			return
		}
		a.fail(w, r, "reading a user", err)
		return
	}

	if target.Role == req.Role {
		writeJSON(w, http.StatusOK, map[string]any{"id": target.ID, "role": target.Role})
		return
	}

	// The same guard the console applies. A setup session can always promote
	// somebody afterwards, so this is a footgun rather than a dead end - but
	// promoting the replacement first costs nothing and this makes the order
	// obvious.
	if lerr := a.refuseLastAdminChange(r, target, req.Role, target.Status); lerr != nil {
		writeAPIError(w, r, http.StatusBadRequest, lerr.Error())
		return
	}

	if serr := a.db.SetUserRole(r.Context(), target.ID, req.Role); serr != nil {
		a.fail(w, r, "changing a role", serr)
		return
	}

	// A role is copied into a session when it is established, so a change has
	// to end the sessions that predate it.
	if rerr := a.db.RevokeUserConsoleSessions(r.Context(), target.ID); rerr != nil {
		a.log.ErrorContext(r.Context(), "revoking sessions after a role change",
			"error", rerr, "user_id", target.ID)
	}

	a.log.WarnContext(r.Context(), "role changed from a setup session",
		"user_id", target.ID, "email", target.Email,
		"from", target.Role, "to", req.Role, "source_ip", clientIP(r))

	writeJSON(w, http.StatusOK, map[string]any{"id": target.ID, "role": req.Role})
}
