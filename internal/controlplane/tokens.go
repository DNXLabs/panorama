// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/store"
)

// maxTokensPerUser bounds how many live tokens one person may hold.
//
// Not a security boundary - a person who can mint one can mint another - but a
// list of two hundred tokens is a list nobody audits, and the point of showing
// last-used beside each one is that somebody looks.
const maxTokensPerUser = 20

// maxTokenDays is the far ceiling on any token's expiry, personal or service.
// auth.max_token_ttl is the deployment's own, usually much lower; this one just
// stops a number that is not really an expiry.
const maxTokenDays = 3650

// handleListTokens serves the caller's own gateway tokens.
//
// Metadata only: the label, the visible prefix, when it was made, and when it
// was last used. The credential itself was shown once at creation and only its
// hash was ever stored, so there is nothing else here to serve.
func (a *API) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	userID := p.UserID
	if requested := r.URL.Query().Get("user"); requested != "" && requested != p.UserID {
		// An administrator may look at whose tokens exist, in order to revoke
		// one - that is the whole of the account-recovery story. They still see
		// only metadata, because the credential does not exist to be seen.
		if p.Role != store.RoleAdmin {
			writeAPIError(w, r, http.StatusForbidden, "Only an administrator can look at another person's tokens.")
			return
		}
		userID = requested
	}

	tokens, err := a.db.ListGatewayTokens(r.Context(), userID)
	if err != nil {
		a.fail(w, r, "listing tokens", err)
		return
	}

	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		row := map[string]any{
			"id":         t.ID,
			"name":       t.Name,
			"prefix":     t.Prefix,
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
			"revoked":    t.Revoked,
			// Explicitly false rather than absent, so the onboarding screen can
			// say "no tool has used this yet" instead of rendering a gap.
			"used": t.LastUsedAt != nil,
		}
		addTime(row, "last_used_at", t.LastUsedAt)
		addTime(row, "expires_at", t.ExpiresAt)
		addTime(row, "revoked_at", t.RevokedAt)
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tokens": out,
		// How long a token minted now would last, in days. Served rather than
		// written into the console, because auth.max_token_ttl can make it
		// shorter and a screen that said "30 days" on a deployment that grants
		// seven would be the console contradicting the credential in the
		// reader's hand.
		"lifetime_days": int(auth.GatewayTokenLifetime(a.server.cfg.Auth.MaxTokenTTL).Hours() / 24),
	})
}

// createTokenRequest is a label and nothing else.
//
// There is no expiry field, and its absence is the feature: every gateway
// token lives auth.GatewayTokenTTL and no request can ask for more. A field
// the server ignored would be worse than none - it would read as a choice.
// Service tokens keep theirs, because a pipeline credential's lifetime is a
// real operational decision made by somebody who knows the pipeline.
type createTokenRequest struct {
	Name string `json:"name"`
}

// handleCreateToken mints a gateway token for the caller.
//
// For the caller, always. There is no parameter that mints one for somebody
// else, and that is deliberate: a token acts as its owner, so an administrator
// who could mint one for an engineer would be able to make requests that the
// audit trail attributes to that engineer. Recovering a colleague's access is
// revoke-and-let-them-mint, not mint-on-their-behalf.
func (a *API) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	var req createTokenRequest
	if !decodeBody(w, r, &req) {
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "console"
	}
	if len(name) > 64 {
		writeAPIError(w, r, http.StatusBadRequest, "That label is too long; 64 characters is plenty.")
		return
	}

	existing, err := a.db.ListGatewayTokens(r.Context(), p.UserID)
	if err != nil {
		a.fail(w, r, "listing tokens", err)
		return
	}
	live := 0
	for _, t := range existing {
		if !t.Revoked {
			live++
		}
	}
	if live >= maxTokensPerUser {
		writeAPIError(w, r, http.StatusConflict,
			"You already hold "+strconv.Itoa(live)+" tokens. Revoke one you no longer use before minting another.")
		return
	}

	// Fixed, and clamped down by auth.max_token_ttl where a deployment set a
	// shorter one. The same call `panorama token create` makes, so the two
	// mint paths cannot end up disagreeing about when a credential dies.
	expiresAt := auth.GatewayTokenExpiry(time.Now(), a.server.cfg.Auth.MaxTokenTTL)

	minted, err := auth.Generate()
	if err != nil {
		a.fail(w, r, "minting a token", err)
		return
	}

	record, err := a.db.CreateGatewayToken(r.Context(), p.UserID, name, minted.Hash, minted.DisplayPrefix, &expiresAt)
	if err != nil {
		a.fail(w, r, "recording a token", err)
		return
	}

	a.log.InfoContext(r.Context(), "gateway token minted",
		"user_id", p.UserID, "token_id", record.ID, "name", name,
		"expires_at", expiresAt.Format(time.RFC3339))

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         record.ID,
		"name":       record.Name,
		"prefix":     record.Prefix,
		"created_at": record.CreatedAt.UTC().Format(time.RFC3339),
		"expires_at": expiresAt.Format(time.RFC3339),
		// The one and only time this value exists outside the client that will
		// hold it. Panorama stored its hash and nothing else, so there is no
		// second chance and the console says so.
		"token": minted.Plaintext,
		"note":  "Copy this now. Panorama stores only its hash and cannot show it again.",
	})
}

// handleRevokeToken revokes a token.
//
// Its owner may revoke it, and so may an administrator: a laptop that walks out
// of the building is exactly the case where waiting for its owner to do it is
// the wrong answer.
func (a *API) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	tokenID := r.PathValue("id")

	owner, err := a.db.GatewayTokenOwner(r.Context(), tokenID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Answered the same way as a token belonging to someone else, so
			// this endpoint cannot be used to discover which ids exist.
			writeAPIError(w, r, http.StatusNotFound, "No such token.")
			return
		}
		a.fail(w, r, "reading a token", err)
		return
	}

	if owner != p.UserID && p.Role != store.RoleAdmin {
		writeAPIError(w, r, http.StatusNotFound, "No such token.")
		return
	}

	if err := a.db.RevokeToken(r.Context(), tokenID); err != nil {
		a.fail(w, r, "revoking a token", err)
		return
	}

	a.log.InfoContext(r.Context(), "gateway token revoked",
		"token_id", tokenID, "owner", owner, "actor", p.UserID)

	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func parsePositiveInt(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("must not be negative")
	}
	return n, nil
}
