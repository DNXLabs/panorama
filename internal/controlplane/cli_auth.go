// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// Command-line sign-in is a switch, and in this release it is off.
//
// `panorama login`, the device authorization it opens, the grant it leaves on
// a laptop and the gateway token that grant buys are built and tested. What is
// not settled is when engineers are handed them, so until it is, the credential
// a coding tool holds is a gateway token minted in the console: it lives
// auth.GatewayTokenTTL and is re-minted by hand when it runs out.
//
// THE SETTING IS THE SWITCH, and it is the only thing read at request time.
// cliSignInReleased is a second, narrower thing: it is what an administrator
// meets, refusing the write that would turn the setting on while this build has
// not handed the flow over. Two questions, and they are different - "does this
// deployment serve it" is answered by the row, "may an administrator say yes
// yet" by the constant - so folding them into one value would make the release
// that hands the CLI over a change to the request path rather than to one
// declaration.
//
// The seam is deliberate and worth stating: a deployment that writes the row
// itself gets the flow, and the end-to-end suite is what does. That is correct.
// The flow works; the decision is about when to offer it, not whether it runs,
// and a switch nothing can ever turn on is a switch nothing tests.
//
// Off is enforced here and not by a hidden button. The console's mentions of
// the CLI go with the flow, because a screen offering a credential path this
// deployment refuses is worse than one that never mentioned it - but the
// refusal is on the server, where every refusal in this product is.
const settingCLISignIn = "cli.sign_in"

// cliSignInReleased says whether an administrator may turn the setting on yet.
const cliSignInReleased = false

// cliSignInOn reports whether this deployment serves command-line sign-in.
//
// It fails closed and logs the cause rather than guessing. A switch that reads
// "on" because the database was briefly unreachable is a credential path nobody
// chose to open. The read is off the request path - these are flows that run
// once per machine and one console response - so it costs nothing worth trading
// for that.
func (s *Server) cliSignInOn(ctx context.Context) bool {
	stored, err := s.db.Setting(ctx, settingCLISignIn)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Never set is off, which is what this ships with.
		return false
	case err != nil:
		s.log.ErrorContext(ctx, "reading the command-line sign-in setting",
			"error", err, "note", "treating it as off")
		return false
	}
	return stored == "true"
}

// requireCLISignIn refuses the routes that exist only while the flow is served.
//
// The message names the path that does work, because the person meeting this
// has a tool that will not start and nothing else to tell a switched-off
// feature apart from a broken deployment.
//
// Listing and revoking grants are deliberately not behind it: a credential
// already on somebody's laptop has to stay revocable whatever the switch says,
// and the list is how an operator finds the one to revoke.
func (s *Server) requireCLISignIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cliSignInOn(r.Context()) {
			writeAPIError(w, r, http.StatusForbidden,
				"Command-line sign-in is switched off on this deployment. "+
					"Mint a gateway token on My setup in the console and give that to your tool.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// maxGrantsPerUser bounds how many machines one account can be signed in on.
//
// A ceiling rather than a policy: nobody has fifty laptops, and the number
// exists so that a loop minting grants is a bounded problem rather than an
// unbounded table.
const maxGrantsPerUser = 20

// deviceStartRequest is what `panorama login` sends to open a flow.
type deviceStartRequest struct {
	// ClientName is what the machine calls itself, shown on the approval
	// screen. It is display text from an unauthenticated caller, so it is
	// length-bounded and never interpreted.
	ClientName string `json:"client_name"`
}

// handleDeviceStart opens a device-authorization flow.
//
// Unauthenticated, and it is worth being explicit about why, because the rule
// is that everything except /healthz and /readyz is authenticated.
//
// The premise of a device flow is that the thing asking has no credential yet;
// that is the problem it solves. What this endpoint hands back grants nothing -
// a device code that cannot be exchanged until a signed-in human approves it,
// and a user code that is useless without a console session. No account is
// named, none is looked up, and nothing about the deployment is disclosed. The
// authorization happens on the approval endpoint, which is authenticated and
// carries CSRF like every other write.
//
// It is rate limited because it is an unauthenticated endpoint that writes a
// row: the limiter is there to stop it being a free way to make the deployment
// do database work.
func (s *Server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	if !s.setupLimiter.allow(clientIP(r)) {
		writeAPIError(w, r, http.StatusTooManyRequests,
			"Too many sign-in attempts from this address. Wait a minute and try again.")
		return
	}

	var req deviceStartRequest
	if !decodeBody(w, r, &req) {
		return
	}

	name := strings.TrimSpace(req.ClientName)
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		name = "a command line"
	}

	device, err := auth.NewSecret(auth.GrantPrefix)
	if err != nil {
		s.fail(w, r, "minting a device code", err)
		return
	}
	userCode, err := auth.NewUserCode()
	if err != nil {
		s.fail(w, r, "minting a user code", err)
		return
	}

	ttl := s.cfg.Auth.DeviceCodeTTL
	interval := s.cfg.Auth.DevicePollInterval
	verification := strings.TrimRight(s.cfg.Server.BaseURL, "/") + "/cli"

	if _, err := s.db.CreateDeviceAuthorization(r.Context(),
		auth.Hash(device.Plaintext), userCode, name, clientIP(r),
		time.Now().UTC().Add(ttl), int(interval.Seconds())); err != nil {
		s.fail(w, r, "opening a device authorization", err)
		return
	}

	s.log.InfoContext(r.Context(), "device authorization opened",
		"client", name, "request_id", RequestIDFrom(r.Context()))

	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": device.Plaintext,
		"user_code":   userCode,
		// Where to send the person. Built from base_url rather than from the
		// request's Host, so a deployment behind a proxy sends people to the
		// address they actually reach it on.
		"verification_uri": verification,
		// The same address with the code already in it, which is what RFC 8628
		// calls verification_uri_complete. Both are returned because they are
		// for different situations: this one saves a person transcribing eight
		// characters when the browser is on the machine that ran the command,
		// and the bare one is what gets read aloud or typed on a phone when it
		// is not.
		"verification_uri_complete": verification + "?code=" + url.QueryEscape(userCode),
		"expires_in":                int(ttl.Seconds()),
		"interval":                  int(interval.Seconds()),
	})
}

// handleDeviceGrant is the CLI's poll.
//
// Unauthenticated for the same reason as the start: the caller's credential is
// what this produces. It presents the device code it was given, which is a
// 256-bit secret it has held since the flow opened.
//
// The RFC 8628 error vocabulary is used verbatim - authorization_pending,
// slow_down, expired_token, access_denied - because a CLI written against the
// spec should work against this without a translation table.
func (s *Server) handleDeviceGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	code := strings.TrimSpace(req.DeviceCode)
	if !auth.WellFormed(auth.GrantPrefix, code) {
		writeDeviceError(w, "invalid_request",
			"That is not a Panorama device code.")
		return
	}

	claim, err := s.db.ClaimDeviceAuthorization(r.Context(), auth.Hash(code))
	switch {
	case errors.Is(err, store.ErrAuthorizationPending):
		writeDeviceError(w, "authorization_pending",
			"Waiting for someone to approve this sign-in.")
		return
	case errors.Is(err, store.ErrSlowDown):
		writeDeviceError(w, "slow_down",
			"Polling too fast. Wait for the interval you were given.")
		return
	case errors.Is(err, store.ErrAuthorizationDenied):
		writeDeviceError(w, "access_denied",
			"That sign-in was refused.")
		return
	case errors.Is(err, store.ErrAuthorizationExpired):
		writeDeviceError(w, "expired_token",
			"That sign-in request expired. Run panorama login again.")
		return
	case errors.Is(err, store.ErrTokenRejected):
		// Unknown and already-claimed are the same answer, as everywhere else.
		writeDeviceError(w, "expired_token",
			"That sign-in request is no longer valid. Run panorama login again.")
		return
	case err != nil:
		s.fail(w, r, "claiming a device authorization", err)
		return
	}

	grant, err := auth.GenerateGrant()
	if err != nil {
		s.fail(w, r, "minting a grant", err)
		return
	}

	var expiresAt *time.Time
	if ttl := s.cfg.Auth.GrantTTL; ttl > 0 {
		at := time.Now().UTC().Add(ttl)
		expiresAt = &at
	}

	// The client name was recorded when the flow opened and came back with the
	// claim, so the grant list and the approval screen name the same machine.
	name := claim.ClientName
	if name == "" {
		name = "a command line"
	}

	record, err := s.db.CreateGrant(r.Context(), claim.UserID, name, grant.Hash, grant.DisplayPrefix, expiresAt)
	if err != nil {
		s.fail(w, r, "recording a grant", err)
		return
	}

	s.log.InfoContext(r.Context(), "cli signed in",
		"userId", claim.UserID, "grant", record.ID,
		"request_id", RequestIDFrom(r.Context()))

	out := map[string]any{
		"grant":    grant.Plaintext,
		"grant_id": record.ID,
		"name":     name,
		// So the CLI can say when it will have to sign in again rather than
		// discovering it mid-command.
		"token_ttl_seconds": int(s.cfg.Auth.TokenTTL.Seconds()),
	}
	addTime(out, "expires_at", expiresAt)
	writeJSON(w, http.StatusOK, out)
}

// handleCLIToken exchanges a grant for a short-lived gateway token.
//
// This is the only endpoint that accepts a grant, which is why auth.ParseGrant
// exists separately from auth.Parse: the gateway refuses a grant outright, so a
// grant that leaks cannot buy model calls, and a handler cannot accidentally
// admit one by reaching for the more convenient parser.
//
// Not rate limited by address. A developer's tooling calls this on a schedule
// from wherever they are, and the credential it requires is already a 256-bit
// secret - a limiter here would break a busy office before it stopped anyone.
func (s *Server) handleCLIToken(w http.ResponseWriter, r *http.Request) {
	presented, err := auth.ParseGrant(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, r, http.StatusUnauthorized,
			"No Panorama grant was supplied. Run panorama login.")
		return
	}

	principal, grantID, err := s.db.AuthenticateGrant(r.Context(), auth.Hash(presented))
	if err != nil {
		if errors.Is(err, store.ErrTokenRejected) {
			s.log.WarnContext(r.Context(), "grant rejected",
				"reason", "unknown, revoked, expired, or user disabled",
				"request_id", RequestIDFrom(r.Context()))
			// Said plainly, because this is what a developer sees when their
			// access was withdrawn. A vague message here turns a deliberate
			// revocation into an afternoon of debugging.
			writeAPIError(w, r, http.StatusUnauthorized,
				"This Panorama sign-in is no longer valid. Run panorama login again.")
			return
		}
		s.fail(w, r, "authenticating a grant", err)
		return
	}

	token, err := auth.Generate()
	if err != nil {
		s.fail(w, r, "minting a gateway token", err)
		return
	}

	expiresAt := time.Now().UTC().Add(s.cfg.Auth.TokenTTL)
	record, err := s.db.ExchangeGrant(r.Context(), grantID, principal.UserID,
		"cli", token.Hash, token.DisplayPrefix, &expiresAt)
	if err != nil {
		s.fail(w, r, "exchanging a grant", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token.Plaintext,
		"token_id":   record.ID,
		"expires_at": expiresAt.Format(time.RFC3339),
		"expires_in": int(s.cfg.Auth.TokenTTL.Seconds()),
	})
}

// handlePendingCLI describes a waiting sign-in to the person deciding about it.
//
// Authenticated: only somebody signed in to the console can see that a flow
// exists, and only they can act on it.
func (a *API) handlePendingCLI(w http.ResponseWriter, r *http.Request) {
	code := normaliseUserCode(r.URL.Query().Get("user_code"))
	if code == "" {
		writeAPIError(w, r, http.StatusBadRequest, "Enter the code your terminal is showing.")
		return
	}

	pending, err := a.db.DeviceAuthorizationByUserCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound,
				"No sign-in is waiting for that code. It may have expired - run panorama login again.")
			return
		}
		a.fail(w, r, "reading a device authorization", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_code":   pending.UserCode,
		"client_name": pending.ClientName,
		// Shown so the person approving can tell their own laptop from a
		// request they did not make. This is the whole of the security
		// decision being asked of them, so it is not hidden behind a detail
		// toggle.
		"source_ip":  pending.SourceIP,
		"created_at": pending.CreatedAt.UTC().Format(time.RFC3339),
		"expires_at": pending.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleDecideCLI records an approval or a refusal.
//
// A write, cookie-borne, so checkCSRF has already run by the time this is
// reached - approving a sign-in from a page somebody was tricked into loading
// is exactly the attack the double-submit token is for.
func (a *API) handleDecideCLI(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	var req struct {
		UserCode string `json:"user_code"`
		Approve  bool   `json:"approve"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	code := normaliseUserCode(req.UserCode)
	if code == "" {
		writeAPIError(w, r, http.StatusBadRequest, "Enter the code your terminal is showing.")
		return
	}

	if req.Approve {
		grants, err := a.db.ListGrants(r.Context(), p.UserID)
		if err != nil {
			a.fail(w, r, "listing grants", err)
			return
		}
		live := 0
		for _, g := range grants {
			if !g.Revoked() {
				live++
			}
		}
		if live >= maxGrantsPerUser {
			writeAPIError(w, r, http.StatusConflict,
				"You are signed in on too many machines. Sign one out from My setup first.")
			return
		}
	}

	if err := a.db.DecideDeviceAuthorization(r.Context(), code, p.UserID, req.Approve); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound,
				"No sign-in is waiting for that code. It may have expired, or already been decided.")
			return
		}
		a.fail(w, r, "deciding a device authorization", err)
		return
	}

	a.log.InfoContext(r.Context(), "device authorization decided",
		"userId", p.UserID, "approved", req.Approve,
		"request_id", RequestIDFrom(r.Context()))

	writeJSON(w, http.StatusOK, map[string]any{"approved": req.Approve})
}

// handleListGrants serves the caller's signed-in machines.
func (a *API) handleListGrants(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	userID := p.UserID
	if requested := r.URL.Query().Get("user"); requested != "" && requested != p.UserID {
		// Same rule as gateway tokens: an administrator may see that somebody
		// else's machines exist, in order to sign one out. Metadata only.
		if p.Role != store.RoleAdmin {
			writeAPIError(w, r, http.StatusForbidden,
				"Only an administrator can look at another person's sign-ins.")
			return
		}
		userID = requested
	}

	grants, err := a.db.ListGrants(r.Context(), userID)
	if err != nil {
		a.fail(w, r, "listing grants", err)
		return
	}

	out := make([]map[string]any, 0, len(grants))
	for _, g := range grants {
		row := map[string]any{
			"id":         g.ID,
			"name":       g.Name,
			"prefix":     g.Prefix,
			"created_at": g.CreatedAt.UTC().Format(time.RFC3339),
			"revoked":    g.Revoked(),
			"used":       g.LastUsedAt != nil,
		}
		addTime(row, "last_used_at", g.LastUsedAt)
		addTime(row, "expires_at", g.ExpiresAt)
		addTime(row, "revoked_at", g.RevokedAt)
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

// handleRevokeGrant signs a machine out.
//
// Revoking the grant also revokes the gateway tokens it minted - see
// store.RevokeGrant. Without that, signing out a lost laptop would leave its
// current token working until it happened to expire, which is not what anybody
// reaching for this button means.
func (a *API) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	id := r.PathValue("id")

	owner, err := a.db.GrantOwner(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Not found and not yours are the same answer, so this cannot be
			// used to discover which grant ids exist.
			writeAPIError(w, r, http.StatusNotFound, "No such sign-in.")
			return
		}
		a.fail(w, r, "reading a grant", err)
		return
	}
	if owner != p.UserID && p.Role != store.RoleAdmin {
		writeAPIError(w, r, http.StatusNotFound, "No such sign-in.")
		return
	}

	if err := a.db.RevokeGrant(r.Context(), id); err != nil {
		a.fail(w, r, "revoking a grant", err)
		return
	}

	a.log.InfoContext(r.Context(), "cli signed out",
		"userId", owner, "grant", id, "by", p.UserID,
		"request_id", RequestIDFrom(r.Context()))

	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// normaliseUserCode accepts what a person actually types.
//
// The code is displayed as ABCD-EFGH and will arrive lower-cased, without the
// dash, or with a stray space around it. None of those are a different code, so
// none of them are a failed sign-in.
func normaliseUserCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	if len(s) != 8 {
		return ""
	}
	return s[:4] + "-" + s[4:]
}

// writeDeviceError answers a poll in the shape RFC 8628 specifies.
//
// Always 400. The device-flow errors are all "not yet" or "not any more"
// rather than authentication failures, and answering 401 would have a client
// discard the device code it is supposed to keep polling with.
func writeDeviceError(w http.ResponseWriter, code, description string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":             code,
		"error_description": description,
	})
}
