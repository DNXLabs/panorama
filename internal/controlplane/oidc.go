// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/ssosetup"
	"github.com/dnxlabs/panorama/internal/store"
)

// Settings keys the identity configuration uses.
//
// The connection - issuer, client id, client secret - is not among them. It
// lives in the configuration file and the environment, where every other secret
// does, and the console verifies it rather than storing it. What is here is
// organizational data an administrator edits: which of their groups maps to
// which role, and what an unmapped person gets.
const (
	// settingDefaultRole is the role a user in no mapped group receives.
	// "developer" - self-only visibility - unless an administrator sets it to
	// "none", which means an explicit mapping is required to sign in at all.
	settingDefaultRole = "oidc.default_role"
)

// roleNone is the value that turns the group mapping into a strict allowlist.
const roleNone = "none"

// oidcFlowTTL bounds how long a sign-in may take between the redirect out and
// the callback back.
const oidcFlowTTL = 10 * time.Minute

// identity holds the OIDC client, built on first use.
//
// Not at startup, on purpose. Discovery is a network call to someone else's
// service, and a gateway that refused to boot because an identity provider was
// briefly unreachable would take the proxy - which needs no identity provider
// at all - down with it. The cost is that the first sign-in after a restart
// pays for discovery, which nobody notices.
type identity struct {
	cfg config.OIDC
	// redirectURL is derived from the deployment's base URL, once, so a
	// mismatch with what is registered at the provider is a single fact to
	// check rather than one per code path.
	redirectURL string

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

func newIdentity(cfg config.Config) *identity {
	return &identity{
		cfg:         cfg.OIDC,
		redirectURL: ssosetup.RedirectURI(cfg.Server.BaseURL),
	}
}

// configured reports whether SSO is available.
func (i *identity) configured() bool { return i != nil && i.cfg.Configured() }

// connect performs discovery once and caches the result.
func (i *identity) connect(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.oauth != nil {
		return i.oauth, i.verifier, nil
	}

	provider, err := oidc.NewProvider(ctx, i.cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc: discovery against %s failed: %w", i.cfg.Issuer, err)
	}

	i.provider = provider
	i.verifier = provider.Verifier(&oidc.Config{ClientID: i.cfg.ClientID})
	i.oauth = &oauth2.Config{
		ClientID:     i.cfg.ClientID,
		ClientSecret: i.cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  i.redirectURL,
		Scopes:       i.cfg.Scopes,
	}
	return i.oauth, i.verifier, nil
}

// check verifies the connection, for the setup wizard's verified state.
//
// Discovery only. Actually exchanging a code needs a person at a browser, so
// what this can prove is that the issuer is reachable, is an OIDC provider, and
// publishes the endpoints Panorama will use. The rest is proved by the first
// sign-in, which is why the wizard's identity step is not marked done until one
// happens.
func (i *identity) check(ctx context.Context) error {
	if !i.configured() {
		return errors.New("oidc: no issuer is configured")
	}
	_, _, err := i.connect(ctx)
	return err
}

// handleOIDCStart redirects a visitor to their identity provider.
//
// Unauthenticated by nature - it is the thing that produces authentication -
// and it exposes nothing: the response is a redirect to a URL built entirely
// from this deployment's own configuration.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if !s.identity.configured() {
		writeAPIError(w, r, http.StatusNotFound, "Single sign-on is not configured on this deployment.")
		return
	}

	oauthCfg, _, err := s.identity.connect(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "oidc discovery", "error", err, "issuer", s.cfg.OIDC.Issuer)
		writeAPIError(w, r, http.StatusBadGateway,
			"Panorama could not reach the identity provider. An administrator can check the issuer URL.")
		return
	}

	state, err := randomToken()
	if err != nil {
		s.fail(w, r, "starting sign-in", err)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		s.fail(w, r, "starting sign-in", err)
		return
	}
	verifier := oauth2.GenerateVerifier()

	flow := store.OIDCFlow{
		State: state, Nonce: nonce, Verifier: verifier,
		RedirectTo: safeRedirect(r.URL.Query().Get("redirect_to")),
		ExpiresAt:  time.Now().UTC().Add(oidcFlowTTL),
	}
	if err := s.db.StartOIDCFlow(r.Context(), flow); err != nil {
		s.fail(w, r, "starting sign-in", err)
		return
	}

	// The state also travels as a cookie, and both must agree at the callback.
	// The database row proves the flow is one Panorama started and has not been
	// used; the cookie proves it is *this browser's* flow, which is what stops
	// an attacker from completing a sign-in into someone else's session.
	s.setCookie(w, oidcCookie, state, oidcFlowTTL, true)

	http.Redirect(w, r, oauthCfg.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), http.StatusFound)
}

// handleOIDCCallback completes a sign-in.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !s.identity.configured() {
		writeAPIError(w, r, http.StatusNotFound, "Single sign-on is not configured on this deployment.")
		return
	}
	s.clearCookie(w, oidcCookie)

	query := r.URL.Query()
	if providerErr := query.Get("error"); providerErr != "" {
		// The provider refused. Its own description is the useful part and it
		// is not ours to paraphrase, so it is logged in full and the visitor
		// is told plainly that the provider said no.
		s.log.WarnContext(r.Context(), "identity provider refused a sign-in",
			"error", providerErr, "description", query.Get("error_description"))
		s.signInFailed(w, r, "Your identity provider refused the sign-in.")
		return
	}

	state := query.Get("state")
	cookie := s.readCookie(r, oidcCookie)
	if state == "" || cookie == "" || !secretsEqual(state, cookie) {
		s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "state does not match this browser")
		s.signInFailed(w, r, "That sign-in did not start here. Try again from the Panorama console.")
		return
	}

	flow, err := s.db.TakeOIDCFlow(r.Context(), state)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.log.WarnContext(r.Context(), "oidc callback rejected", "reason", "unknown, used, or expired flow")
			s.signInFailed(w, r, "That sign-in has expired or was already used. Try again.")
			return
		}
		s.fail(w, r, "completing sign-in", err)
		return
	}

	claims, err := s.exchange(r, query.Get("code"), flow)
	if err != nil {
		s.log.WarnContext(r.Context(), "oidc token exchange failed", "error", err)
		s.signInFailed(w, r, "Panorama could not verify the response from your identity provider.")
		return
	}

	// Before anything about this token is recorded or acted on, and above all
	// before provision: the first account to arrive owns the deployment, and at
	// an issuer that signs in the whole world that is whoever found the address
	// first. A stranger's token also has no business in the record of what this
	// organization's provider sends.
	if reason := domainRefusal(s.cfg.OIDC, claims); reason != "" {
		s.log.WarnContext(r.Context(), "sign-in refused", "reason", reason, "subject", claims.Subject)
		s.signInFailed(w, r, "Your account is not in a domain that may use this Panorama deployment. Sign in with your work account.")
		return
	}

	// Recorded before provisioning, so the refusal below still leaves a record
	// of what the provider called this person's groups. That refusal is exactly
	// when an administrator needs to know, and it is the one path where no
	// account is created to infer it from afterwards.
	//
	// A failure here does not fail the sign-in: this is a diagnostic, and
	// refusing to authenticate somebody because a counter would not increment
	// would be worse than the missing counter. It is logged rather than
	// swallowed.
	if rerr := s.db.RecordSignIn(r.Context(), store.SignInObservation{
		Groups: claims.Groups,
		Claims: claims.Names,
		Empty:  claims.Empty,
	}); rerr != nil {
		s.log.ErrorContext(r.Context(), "recording what a sign-in carried",
			"error", rerr, "groups_claim", s.cfg.OIDC.GroupsClaim)
	}

	user, first, err := s.provision(r.Context(), claims)
	if err != nil {
		if errors.Is(err, errNotPermitted) {
			s.log.WarnContext(r.Context(), "sign-in refused", "reason", "no mapped group", "subject", claims.Subject)
			s.signInFailed(w, r, "Your account is not in a group that may use Panorama. Ask an administrator.")
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			s.log.WarnContext(r.Context(), "sign-in refused", "reason", err.Error(), "subject", claims.Subject)
			s.signInFailed(w, r, "Your identity provider did not send the claims Panorama needs. An administrator can check the requested scopes.")
			return
		}
		s.fail(w, r, "provisioning the signed-in user", err)
		return
	}

	if err := s.signIn(w, r, user.ID); err != nil {
		s.fail(w, r, "establishing a session", err)
		return
	}

	s.log.InfoContext(r.Context(), "signed in", "user_id", user.ID, "role", user.Role)

	// Re-checked here even though it was checked before it was stored. The
	// value made a round trip through the database between those two moments,
	// and an open redirect on a sign-in endpoint is how a phishing page borrows
	// a domain the victim already trusts.
	destination := safeRedirect(flow.RedirectTo)
	if destination == "" {
		// The account that owns a fresh deployment arrives with work to do and
		// no way to know it: the overview it would otherwise land on is empty by
		// definition, since nothing has been proxied yet. Only when the visitor
		// asked for nowhere in particular - a deep link that bounced through
		// sign-in is still honoured.
		destination = "/"
		if first {
			destination = "/welcome"
		}
	}
	// #nosec G710 -- safeRedirect above is the sanitiser: it admits only a
	// same-origin absolute path, rejecting anything with a scheme, a host, a
	// leading "//", a backslash, or a newline. The taint analysis cannot see
	// through it. See TestSafeRedirectRefusesEverythingButAPath.
	http.Redirect(w, r, destination, http.StatusFound)
}

// idClaims is what Panorama reads out of an ID token.
//
// Names and Empty describe the token's shape rather than its contents: the
// names of the claims it carried, and which of those carried nothing. They
// exist because "the groups claim is empty" has two opposite causes - the claim
// is called something else, or the provider is not emitting it - and an
// administrator cannot tell them apart without seeing what did arrive. No claim
// value is copied into either.
type idClaims struct {
	Subject     string
	Email       string
	DisplayName string
	Groups      []string
	Names       []string
	Empty       []string

	// HostedDomain is Google's `hd` claim: the Workspace organization an
	// account is managed by, and absent on a personal account.
	HostedDomain string
	// EmailUnverified is true only when the token says so outright. Most
	// providers send no email_verified at all, and silence is not a denial.
	EmailUnverified bool
}

// domainRefusal says why a verified identity is outside oidc.allowed_domains,
// or nothing when it is inside or there is no list.
//
// Which claim carries the domain depends on who issued the token, and getting
// it wrong at Google is the hole this exists to close. Anybody may create a
// personal Google account on a work address, and it outlives the mailbox: the
// account of somebody who left still says email_verified, on the company's
// domain, years later. `hd` is set only on an account the Workspace
// organization manages, which is why Google's own guidance is to check it and
// not the address. So at a shared issuer `hd` is the only thing read, and a
// token without one is a personal account.
//
// Everywhere else the tenant already decided who may sign in, and the list
// narrows that by the address's domain. An address the provider marks
// unverified is one its owner typed, and proves nothing.
func domainRefusal(cfg config.OIDC, claims *idClaims) string {
	if len(cfg.AllowedDomains) == 0 {
		return ""
	}
	if config.SharedIssuer(cfg.Issuer) {
		if claims.HostedDomain == "" {
			return "a personal account, with no hosted domain, and oidc.allowed_domains is set"
		}
		if !cfg.AllowsDomain(claims.HostedDomain) {
			return "hosted domain " + claims.HostedDomain + " is not in oidc.allowed_domains"
		}
		return ""
	}

	_, domain, found := strings.Cut(claims.Email, "@")
	if !found || domain == "" {
		return "no email address to read a domain from, and oidc.allowed_domains is set"
	}
	if claims.EmailUnverified {
		return "the provider marks this email address unverified, and oidc.allowed_domains is set"
	}
	if !cfg.AllowsDomain(domain) {
		return "email domain " + domain + " is not in oidc.allowed_domains"
	}
	return ""
}

// exchange trades the authorization code for a verified ID token.
func (s *Server) exchange(r *http.Request, code string, flow *store.OIDCFlow) (*idClaims, error) {
	if code == "" {
		return nil, errors.New("no authorization code")
	}

	oauthCfg, verifier, err := s.identity.connect(r.Context())
	if err != nil {
		return nil, err
	}

	token, err := oauthCfg.Exchange(r.Context(), code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}

	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("the token response carried no id_token")
	}

	idToken, err := verifier.Verify(r.Context(), rawID)
	if err != nil {
		return nil, fmt.Errorf("verifying id_token: %w", err)
	}
	if idToken.Nonce != flow.Nonce {
		// The nonce binds this token to the request that asked for it. Without
		// the check, a token issued for a different sign-in would be accepted.
		return nil, errors.New("id_token nonce does not match this sign-in")
	}

	var raw map[string]any
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("reading claims: %w", err)
	}

	names, empty := claimShape(raw)
	return &idClaims{
		Subject:     idToken.Subject,
		Email:       stringClaim(raw, "email"),
		DisplayName: firstStringClaim(raw, "name", "preferred_username", "given_name"),
		Groups:      stringsClaim(raw[s.cfg.OIDC.GroupsClaim]),
		Names:       names,
		Empty:       empty,

		HostedDomain:    stringClaim(raw, "hd"),
		EmailUnverified: claimIsFalse(raw["email_verified"]),
	}, nil
}

// claimShape reports the names of the claims a token carried, and which of them
// carried nothing.
//
// Names only, and deliberately: every value in this map is somebody's data.
// What an administrator needs in order to fix a groups claim that is not
// arriving is the vocabulary the provider is using, and the vocabulary is the
// keys.
func claimShape(raw map[string]any) (names, empty []string) {
	for name, value := range raw {
		names = append(names, name)
		if claimIsEmpty(value) {
			empty = append(empty, name)
		}
	}
	sort.Strings(names)
	sort.Strings(empty)
	return names, empty
}

// claimIsFalse is true for a claim that is present and says no. Providers
// disagree about the type: most send a boolean, and Cognito sends the string.
func claimIsFalse(value any) bool {
	switch v := value.(type) {
	case bool:
		return !v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "false")
	default:
		return false
	}
}

// claimIsEmpty is true for a claim that is present and says nothing. A provider
// emitting `"groups": []` has been configured to send groups and found none,
// which is a different problem from one that never mentions groups at all.
func claimIsEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

// errNotPermitted is returned when a verified identity is not allowed to use
// this deployment.
var errNotPermitted = errors.New("controlplane: identity is not in a permitted group")

// provision maps a verified identity onto a Panorama account.
//
// The second return says this sign-in was the one that created the deployment's
// first account - the caller uses it to decide where to land, since that person
// has a deployment to finish setting up and no reason to guess where.
func (s *Server) provision(ctx context.Context, claims *idClaims) (*store.User, bool, error) {
	mapping, err := s.db.GroupRoles(ctx)
	if err != nil {
		return nil, false, err
	}

	defaultRole := store.RoleDeveloper
	switch v, serr := s.db.Setting(ctx, settingDefaultRole); {
	case serr == nil:
		defaultRole = v
	case !errors.Is(serr, store.ErrNotFound):
		return nil, false, serr
	}

	role, permitted := store.ResolveRole(store.Identity{
		Subject: claims.Subject, Email: claims.Email,
		DisplayName: claims.DisplayName, Groups: claims.Groups,
	}, mapping, defaultRole)

	// The first person through owns the deployment.
	//
	// Without this the new setup flow cannot produce an administrator at all. A
	// setup session configures the connection but creates no account, and the
	// group mapping it would need in order to grant admin is written in terms of
	// group names nothing has seen yet - they only become knowable from a
	// sign-in, which is the thing being gated. Resolving normally here would
	// make the first person a developer, or refuse them outright where the
	// default role is "none", and leave nobody who can fix either.
	//
	// Bounded to the moment there are no users at all, which is one window per
	// deployment and closes on the first sign-in. Two simultaneous first
	// sign-ins both get it, and both are legitimately first.
	users, err := s.db.CountUsers(ctx)
	if err != nil {
		return nil, false, err
	}
	first := users == 0
	source := store.RoleSourceSSO
	if first {
		// Recorded as a deliberate assignment rather than a mapping result,
		// because the next sign-in recomputes every mapping result - and this
		// grant exists precisely because the mapping cannot yet produce an
		// administrator. Pinned, it survives to do the job it was granted for.
		role, permitted, source = store.RoleAdmin, true, store.RoleSourceManual
		s.log.WarnContext(ctx, "FIRST SIGN-IN - this account owns the deployment",
			"email", claims.Email, "role", role,
			"note", "granted because no users existed; map a group to admin before anyone else signs in")
	}

	if !permitted {
		return nil, false, errNotPermitted
	}

	user, roleChanged, err := s.db.UpsertIdentity(ctx, store.Identity{
		Subject: claims.Subject, Email: claims.Email,
		DisplayName: claims.DisplayName, Groups: claims.Groups,
	}, role, source)
	if err != nil {
		return nil, false, err
	}

	if roleChanged {
		// A role is copied into a session when it is established, so a change
		// at the provider has to end the sessions that predate it. Otherwise a
		// demotion takes effect whenever the person happens to close their
		// browser.
		if err := s.db.RevokeUserConsoleSessions(ctx, user.ID); err != nil {
			s.log.ErrorContext(ctx, "revoking sessions after a role change", "error", err, "user_id", user.ID)
		}
		s.log.InfoContext(ctx, "role changed at sign-in",
			"user_id", user.ID, "role", user.Role)
	}
	return user, first, nil
}

// signInFailed sends a visitor back to the console with something it can show.
//
// A redirect rather than a JSON body, because this is a top-level navigation
// from the identity provider and the visitor is looking at a browser tab, not
// at a fetch response. The message travels in the fragment so it never reaches
// a server log or a referrer header.
func (s *Server) signInFailed(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/#sign-in-failed="+url.QueryEscape(message), http.StatusFound)
}

// safeRedirect keeps a sign-in from becoming an open redirect.
//
// Only a same-origin absolute path survives. An open redirect on a login
// endpoint is how a phishing page borrows a domain the victim already trusts,
// and "//evil.example" is a path-looking string that browsers read as a host.
func safeRedirect(candidate string) string {
	if candidate == "" || !strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "//") {
		return ""
	}
	if strings.Contains(candidate, "\\") || strings.ContainsAny(candidate, "\r\n") {
		return ""
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host != "" || parsed.Scheme != "" {
		return ""
	}
	return parsed.RequestURI()
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// secretsEqual compares in constant time, hashing first so that two values of
// different lengths still take the same path.
func secretsEqual(a, b string) bool {
	ah, bh := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return ah == bh
}

func stringClaim(claims map[string]any, key string) string {
	v, _ := claims[key].(string)
	return strings.TrimSpace(v)
}

func firstStringClaim(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := stringClaim(claims, k); v != "" {
			return v
		}
	}
	return ""
}

// stringsClaim reads a groups claim, which providers spell three ways.
//
// A list is the common case. Entra sends a single string when there is one
// group. Some providers send a space-separated string. All three are read,
// because a deployment whose admins silently never get their role is a support
// call nobody enjoys.
func stringsClaim(v any) []string {
	switch value := v.(type) {
	case []string:
		return value
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		return strings.Fields(value)
	default:
		return nil
	}
}
