// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"net/http"
	"strings"

	"github.com/dnxlabs/panorama/internal/ssosetup"
	"github.com/dnxlabs/panorama/internal/store"
)

// handleSSOPresets serves the provider presets.
//
// Static data, but served rather than compiled into the console so the two
// cannot disagree about what a provider needs - and so a preset gained by
// upgrading the binary reaches a console served by that same binary.
//
// The redirect URI is filled into the notes here rather than left as a
// placeholder for the console to substitute. It is derived from
// server.base_url, so the server is the only thing that knows it, and a note
// telling somebody to register "Panorama's redirect URI" sends them looking for
// a value that was available all along.
func (a *API) handleSSOPresets(w http.ResponseWriter, r *http.Request) {
	redirect := a.server.identity.redirectURL

	out := make([]map[string]any, 0, len(ssosetup.Presets()))
	for _, p := range ssosetup.Presets() {
		out = append(out, map[string]any{
			"id":           p.ID,
			"label":        p.Label,
			"issuer_hint":  p.IssuerHint,
			"scopes":       p.Scopes,
			"groups_claim": p.GroupsClaim,
			"notes":        p.NotesWith(redirect),
			"docs_url":     p.DocsURL,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"presets":      out,
		"formats":      ssosetup.Formats(),
		"redirect_uri": redirect,
	})
}

// preflightRequest asks whether an issuer would work.
type preflightRequest struct {
	Issuer      string   `json:"issuer"`
	ClientID    string   `json:"client_id"`
	Preset      string   `json:"preset"`
	Scopes      []string `json:"scopes"`
	GroupsClaim string   `json:"groups_claim"`
}

// handleSSOPreflight validates an issuer that is not configured yet and returns
// the configuration that would use it.
//
// This is the check that has to happen before a restart rather than after one.
// Everything about the connection lives in the environment, so getting it wrong
// costs a redeploy to find out - and the error surfaces at the identity
// provider, in the provider's words, a long way from the setting that caused
// it.
//
// It stores nothing. The response is the same text `panorama sso probe` prints,
// rendered by the same functions, because two generators of the same artifact
// drift.
//
// The outbound request goes to an administrator-supplied URL. That is what the
// endpoint is for, and Panorama runs inside the network whose identity provider
// it is being pointed at, so a private address is the ordinary case rather than
// the suspicious one. What bounds it: admin role, https only, and a timeout in
// the probe itself.
func (a *API) handleSSOPreflight(w http.ResponseWriter, r *http.Request) {
	var req preflightRequest
	if !decodeBody(w, r, &req) {
		return
	}

	preset, ok := ssosetup.PresetByID(req.Preset)
	if !ok {
		if strings.TrimSpace(req.Preset) != "" {
			writeAPIError(w, r, http.StatusBadRequest,
				"Unknown provider preset. Use one of: "+strings.Join(ssosetup.PresetIDs(), ", ")+".")
			return
		}
		preset, _ = ssosetup.PresetByID(ssosetup.PresetGeneric)
	}

	result := ssosetup.Run(r.Context(), ssosetup.Probe{
		Issuer:      req.Issuer,
		ClientID:    req.ClientID,
		Preset:      preset,
		Scopes:      req.Scopes,
		GroupsClaim: req.GroupsClaim,
		RedirectURI: a.server.identity.redirectURL,
	})

	conn := ssosetup.Connection{
		Issuer:      result.Issuer,
		ClientID:    req.ClientID,
		Scopes:      result.Scopes,
		GroupsClaim: result.GroupsClaim,
		RedirectURI: a.server.identity.redirectURL,
	}

	artifacts := make(map[string]string, len(ssosetup.Formats()))
	for _, format := range ssosetup.Formats() {
		body, rerr := ssosetup.Render(format, conn)
		if rerr != nil {
			a.fail(w, r, "rendering the connection", rerr)
			return
		}
		artifacts[format] = body
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result":       result,
		"redirect_uri": conn.RedirectURI,
		"artifacts":    artifacts,
		"formats":      ssosetup.Formats(),
		// Restated on the response rather than assumed known by the caller: the
		// whole point of the screen is that somebody went looking for a form to
		// save this in.
		"stored": false,
	})
}

// handleSSODoctor checks the live connection against what it has actually seen.
func (a *API) handleSSODoctor(w http.ResponseWriter, r *http.Request) {
	facts, err := a.ssoFacts(r)
	if err != nil {
		a.fail(w, r, "reading what sign-ins have carried", err)
		return
	}

	cfg := a.server.cfg.OIDC
	findings := ssosetup.Diagnose(r.Context(), ssosetup.Deployment{
		Issuer:      cfg.Issuer,
		ClientID:    cfg.ClientID,
		HasSecret:   cfg.ClientSecret != "",
		Scopes:      cfg.Scopes,
		GroupsClaim: cfg.GroupsClaim,
		BaseURL:     a.server.cfg.Server.BaseURL,
		RedirectURI: a.server.identity.redirectURL,
	}, facts)

	writeJSON(w, http.StatusOK, map[string]any{
		"level":    ssosetup.Worst(findings),
		"findings": findings,
	})
}

// ssoFacts gathers what the database knows about sign-ins that have happened.
func (a *API) ssoFacts(r *http.Request) (ssosetup.Facts, error) {
	observed, err := a.db.ObservedGroups(r.Context())
	if err != nil {
		return ssosetup.Facts{}, err
	}
	counters, err := a.db.SignInCounters(r.Context())
	if err != nil {
		return ssosetup.Facts{}, err
	}
	mapping, err := a.db.GroupRoles(r.Context())
	if err != nil {
		return ssosetup.Facts{}, err
	}

	defaultRole := store.RoleDeveloper
	if v, serr := a.db.Setting(r.Context(), settingDefaultRole); serr == nil {
		defaultRole = v
	}

	facts := ssosetup.Facts{
		SSOSignIns:           int(counters[store.CounterSignIns]),
		SignInsWithoutGroups: int(counters[store.CounterSignInsWithoutGroups]),
		DefaultRole:          defaultRole,
	}
	for _, g := range observed {
		facts.ObservedGroups = append(facts.ObservedGroups, g.Group)
	}
	for _, m := range mapping {
		facts.MappedGroups = append(facts.MappedGroups, m.Group)
	}
	return facts, nil
}

// wouldLockEveryoneOut reports whether a strict mapping would admit nobody.
//
// Two different questions, depending on what the deployment knows about itself:
//
//   - Nobody has signed in through SSO yet. Nothing here knows what the
//     provider calls its groups, so the only answerable question is whether any
//     mapping exists at all. An empty one plainly admits nobody.
//   - Sign-ins have happened. Then the groups they carried are recorded, and
//     the real question is answerable: does the proposed mapping name any group
//     this deployment has ever seen? A mapping full of names nobody holds
//     grants nothing, which is the mistake the row count missed.
//
// Observed groups are a record of what arrived, not of who is in what, so this
// cannot say "you specifically will still get in" - only that somebody will.
// That is the difference between a lockout and a demotion, and a lockout is the
// one that has no way back through the console.
func (a *API) wouldLockEveryoneOut(r *http.Request, req groupRolesRequest) (bool, string, error) {
	if len(req.Groups) == 0 {
		return true, `An empty mapping with "cannot sign in" as the default would lock everyone out of this deployment, including you.`, nil
	}

	observed, err := a.db.ObservedGroups(r.Context())
	if err != nil {
		return false, "", err
	}
	if len(observed) == 0 {
		// Never seen a sign-in, and the mapping is not empty. Nothing here can
		// second-guess names it has never been shown, so this is allowed.
		return false, "", nil
	}

	seen := make(map[string]bool, len(observed))
	for _, g := range observed {
		seen[strings.ToLower(strings.TrimSpace(g.Group))] = true
	}
	for _, g := range req.Groups {
		if seen[strings.ToLower(strings.TrimSpace(g.Group))] {
			return false, "", nil
		}
	}

	return true, "None of those groups has ever appeared in a sign-in, so with " +
		`"cannot sign in" as the default nobody could get in - including you.`, nil
}
