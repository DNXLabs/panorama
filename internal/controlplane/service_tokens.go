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

// handleListServiceTokens serves the deployment's team-owned credentials.
//
// Leaders only, and metadata only. A service token is not anybody's personal
// credential, so unlike gateway tokens there is no "your own" case here: the
// question it answers is which pipelines can spend a team's budget, which is a
// question about the team.
func (a *API) handleListServiceTokens(w http.ResponseWriter, r *http.Request) {
	teamID := ""
	if slug := strings.TrimSpace(r.URL.Query().Get("team")); slug != "" {
		id, err := a.db.TeamIDBySlug(r.Context(), slug)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeAPIError(w, r, http.StatusNotFound, "No such team.")
				return
			}
			a.fail(w, r, "resolving a team", err)
			return
		}
		teamID = id
	}

	tokens, err := a.db.ListServiceTokens(r.Context(), teamID)
	if err != nil {
		a.fail(w, r, "listing service tokens", err)
		return
	}

	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		row := map[string]any{
			"id":         t.ID,
			"name":       t.Name,
			"prefix":     t.Prefix,
			"team":       t.TeamName,
			"team_slug":  t.TeamSlug,
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339),
			"revoked":    t.Revoked(),
			"used":       t.LastUsedAt != nil,
		}
		addTime(row, "last_used_at", t.LastUsedAt)
		addTime(row, "expires_at", t.ExpiresAt)
		addTime(row, "revoked_at", t.RevokedAt)
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

type createServiceTokenRequest struct {
	// Team is the slug whose budget this token spends.
	Team string `json:"team"`
	Name string `json:"name"`
	// ExpiresInDays bounds the credential. Optional, and unlike a personal
	// token it is worth setting: a pipeline's credential lives in a secret
	// store where nobody looks at it again.
	ExpiresInDays int `json:"expires_in_days"`
}

// handleCreateServiceToken mints a credential for a team.
//
// Administrators only. This is the one place in the product where somebody
// creates a credential they will not be the principal of, which is exactly why
// it is not open to managers by default: the token spends a team's budget and
// the audit trail attributes its traffic to the team, so the question "who may
// make one" is the question "who may commit a team's money".
func (a *API) handleCreateServiceToken(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	var req createServiceTokenRequest
	if !decodeBody(w, r, &req) {
		return
	}

	slug := strings.TrimSpace(req.Team)
	if slug == "" {
		writeAPIError(w, r, http.StatusBadRequest,
			"Name the team whose budget this token spends.")
		return
	}
	teamID, err := a.db.TeamIDBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "No such team.")
			return
		}
		a.fail(w, r, "resolving a team", err)
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeAPIError(w, r, http.StatusBadRequest,
			"Give it a name - which pipeline this is, so it can be told from the others later.")
		return
	}
	if len(name) > 64 {
		writeAPIError(w, r, http.StatusBadRequest, "That label is too long; 64 characters is plenty.")
		return
	}

	expiresAt, ok := a.tokenExpiry(w, r, req.ExpiresInDays)
	if !ok {
		return
	}

	token, err := auth.GenerateService()
	if err != nil {
		a.fail(w, r, "minting a service token", err)
		return
	}

	record, err := a.db.CreateServiceToken(r.Context(), teamID, p.UserID, name,
		token.Hash, token.DisplayPrefix, expiresAt)
	if err != nil {
		a.fail(w, r, "recording a service token", err)
		return
	}

	a.log.InfoContext(r.Context(), "service token created",
		"teamId", teamID, "token", record.ID, "by", p.UserID,
		"request_id", RequestIDFrom(r.Context()))

	out := map[string]any{
		// Shown once. Only the hash was stored, so there is no second chance
		// and the console says so.
		"token":      token.Plaintext,
		"id":         record.ID,
		"name":       name,
		"team":       slug,
		"created_at": record.CreatedAt.UTC().Format(time.RFC3339),
	}
	addTime(out, "expires_at", expiresAt)
	writeJSON(w, http.StatusCreated, out)
}

// handleRevokeServiceToken withdraws a team's credential.
func (a *API) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	id := r.PathValue("id")

	if _, err := a.db.ServiceTokenTeam(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, r, http.StatusNotFound, "No such token.")
			return
		}
		a.fail(w, r, "reading a service token", err)
		return
	}

	if err := a.db.RevokeServiceToken(r.Context(), id); err != nil {
		a.fail(w, r, "revoking a service token", err)
		return
	}

	a.log.InfoContext(r.Context(), "service token revoked",
		"token", id, "by", p.UserID, "request_id", RequestIDFrom(r.Context()))

	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// tokenExpiry turns a requested lifetime in days into a deadline, applying the
// deployment's ceiling.
//
// Shared by personal and service tokens so that auth.max_token_ttl means the
// same thing for both. A cap enforced in one of two places is a cap somebody
// routes around without meaning to.
func (a *API) tokenExpiry(w http.ResponseWriter, r *http.Request, days int) (*time.Time, bool) {
	maxTTL := a.server.cfg.Auth.MaxTokenTTL

	if days < 0 {
		writeAPIError(w, r, http.StatusBadRequest, "An expiry cannot be negative.")
		return nil, false
	}
	if days == 0 {
		if maxTTL <= 0 {
			return nil, true
		}
		// A deployment that has set a ceiling has said tokens expire. Silently
		// minting a permanent one because the field was left blank would be a
		// default that contradicts a setting.
		at := time.Now().UTC().Add(maxTTL)
		return &at, true
	}

	ttl := time.Duration(days) * 24 * time.Hour
	if maxTTL > 0 && ttl > maxTTL {
		writeAPIError(w, r, http.StatusBadRequest,
			"That is longer than this deployment allows. The maximum is "+
				formatDays(maxTTL)+".")
		return nil, false
	}
	if days > maxTokenDays {
		writeAPIError(w, r, http.StatusBadRequest, "That expiry is too far away.")
		return nil, false
	}

	at := time.Now().UTC().Add(ttl)
	return &at, true
}

// formatDays renders a ceiling the way an operator set it.
func formatDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days > 1:
		return strconv.Itoa(days) + " days"
	case days == 1:
		return "1 day"
	default:
		return d.String()
	}
}
