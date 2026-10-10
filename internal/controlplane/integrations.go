// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/secrets"
	"github.com/dnxlabs/panorama/internal/store"
)

// connectRequest is what the console's connect form sends.
type connectRequest struct {
	Provider string `json:"provider"`

	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	APIBaseURL     string `json:"api_base_url"`

	// PrivateKey and Token are the two credential shapes. Exactly one, and only
	// on the way in - no response ever carries either back.
	PrivateKey string `json:"private_key"`
	Token      string `json:"token"`

	// Fixtures is the file a mock reads.
	Fixtures string `json:"fixtures"`
}

// handleConnectIntegration stores a connection an administrator set up.
//
// Administrators only. Connecting an integration decides what this deployment
// reaches out to and with whose access, which is the same class of decision as
// editing the model allowlist rather than the same class as reading a screen.
//
// The credential is encrypted before it is written and is never returned by any
// endpoint - including this one, which answers with what was stored rather than
// echoing what it was given.
func (a *API) handleConnectIntegration(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	kind := r.PathValue("kind")
	if !integrations.Kind(kind).Valid() {
		writeAPIError(w, r, http.StatusNotFound, "Panorama has no integration by that name.")
		return
	}

	var req connectRequest
	if !decodeBody(w, r, &req) {
		return
	}

	cfg := store.IntegrationConfig{
		Kind:           kind,
		Provider:       strings.TrimSpace(req.Provider),
		Enabled:        true,
		AppID:          strings.TrimSpace(req.AppID),
		InstallationID: strings.TrimSpace(req.InstallationID),
		APIBaseURL:     strings.TrimRight(strings.TrimSpace(req.APIBaseURL), "/"),
	}

	secret, problem := credentialFor(kind, cfg.Provider, &req, &cfg)
	if problem != "" {
		writeAPIError(w, r, http.StatusBadRequest, problem)
		return
	}

	ciphertext := ""
	if secret != "" {
		sealed, err := a.secrets.Seal(secret)
		if err != nil {
			if errors.Is(err, secrets.ErrNoKey) {
				// The one failure an administrator can act on immediately, so
				// it is said rather than logged as a server error.
				writeAPIError(w, r, http.StatusPreconditionFailed,
					"This deployment has no encryption key, so it cannot store a credential. "+
						"Set PANORAMA_ENCRYPTION_KEY to any passphrase of 16 characters or more, or to what `panorama admin generate-key` mints, and restart.")
				return
			}
			a.fail(w, r, "sealing an integration credential", err)
			return
		}
		ciphertext = sealed
	}

	// Saved first, then verified, because verifying means building a source and
	// the source is built from what is stored. A connection that saves and then
	// fails its check is visible on the screen as exactly that - connected,
	// erroring, with the reason - which is more useful than a rejected form
	// that leaves nothing behind to fix.
	if err := a.db.SaveIntegrationConfig(r.Context(), cfg, ciphertext, p.UserID); err != nil {
		a.fail(w, r, "saving an integration", err)
		return
	}

	a.log.InfoContext(r.Context(), "integration connected",
		"integration", kind, "provider", cfg.Provider, "by", p.UserID,
		"request_id", RequestIDFrom(r.Context()))

	// Checked immediately so the answer to "did that work" is on the screen
	// that asked, rather than fifteen minutes later.
	status := map[string]any{"connected": true, "checked": false}
	if registry, rerr := a.integrations.Registry(r.Context()); rerr == nil {
		if source, serr := registry.SourceFor(sourceProbe(kind)); serr == nil {
			// Recorded here as well as at startup, because this connection was
			// made while the process was already running: without it the
			// console would show a blank "Connection" row until a restart.
			if regErr := a.db.RegisterIntegration(r.Context(), kind, source.Provider(), source.Describe()); regErr != nil {
				a.log.ErrorContext(r.Context(), "registering integration", "error", regErr)
			}
			checkErr := source.Check(r.Context())
			if cerr := a.db.RecordIntegrationCheck(r.Context(), kind, checkErr); cerr != nil {
				a.log.ErrorContext(r.Context(), "recording integration check", "error", cerr)
			}
			status["checked"] = true
			status["ok"] = checkErr == nil
			status["detail"] = source.Describe()
			if checkErr != nil {
				status["error"] = checkErr.Error()
			}
		}
	} else {
		status["error"] = rerr.Error()
	}

	writeJSON(w, http.StatusOK, status)
}

// credentialFor validates the shape of one provider's connection and returns
// the secret to store, plus a sentence for the person who got it wrong.
//
// The second return is a message rather than an error because that is what it
// is: every one of these is shown to somebody filling in a form, and none is
// wrapped, matched on, or logged as a failure. Returning them as errors meant
// writing them in the clipped style Go errors take, which is the wrong register
// for a sentence somebody reads next to the field they mistyped.
//
// One credential shape, complete, enforced where the form is. Both at once is refused rather than resolved by
// precedence, because a deployment that believed it was using its App while
// quietly using somebody's token has the wrong access and the wrong expiry.
func credentialFor(kind, provider string, req *connectRequest, cfg *store.IntegrationConfig) (secret, problem string) {
	switch provider {
	case config.ProviderMock:
		if req.Fixtures == "" {
			return "", "A mock needs a fixtures file; one with nothing to read answers nothing and looks broken."
		}
		cfg.SecretKind = store.SecretKindNone
		return "", ""

	case config.ProviderGitHub:
		if kind != string(integrations.KindGitHub) {
			return "", "The GitHub provider speaks for branches and pull requests, not tickets."
		}

		key := strings.TrimSpace(req.PrivateKey)
		token := strings.TrimSpace(req.Token)

		switch {
		case key != "" && token != "":
			return "", "Use a GitHub App or a token, not both."
		case key != "":
			if cfg.AppID == "" {
				return "", "A GitHub App needs its App ID as well as its private key."
			}
			cfg.SecretKind = store.SecretKindAppKey
			return key, ""
		case token != "":
			cfg.SecretKind = store.SecretKindToken
			return token, ""
		default:
			// No credential supplied. Valid when one is already stored - an
			// administrator changing the API base URL should not have to paste
			// their private key again - and the store leaves the existing
			// ciphertext alone when none arrives.
			cfg.SecretKind = ""
			return "", ""
		}

	case "":
		return "", "Choose what to connect this to."
	default:
		return "", "Panorama cannot connect to " + provider + "."
	}
}

// handleDisconnectIntegration removes a connection and its credential.
func (a *API) handleDisconnectIntegration(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	kind := r.PathValue("kind")
	if !integrations.Kind(kind).Valid() {
		writeAPIError(w, r, http.StatusNotFound, "Panorama has no integration by that name.")
		return
	}

	if err := a.db.DisconnectIntegration(r.Context(), kind); err != nil {
		a.fail(w, r, "disconnecting an integration", err)
		return
	}

	a.log.InfoContext(r.Context(), "integration disconnected",
		"integration", kind, "by", p.UserID, "request_id", RequestIDFrom(r.Context()))

	writeJSON(w, http.StatusOK, map[string]any{"disconnected": true})
}

// sourceProbe names a work-item kind the given system speaks for, so a
// connection can be looked up in the registry it was just added to.
func sourceProbe(kind string) string {
	if kind == string(integrations.KindJira) {
		return "ticket"
	}
	return "pull_request"
}
