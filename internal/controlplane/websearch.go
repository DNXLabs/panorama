// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/websearch"
)

// mcpServerName is what every client's configuration calls this deployment's
// MCP server, so the tool appears as the same thing in each of them.
const mcpServerName = "panorama"

// webSearchRequest is what Settings, Web search saves.
type webSearchRequest struct {
	Enabled    bool   `json:"enabled"`
	GatewayURL string `json:"gateway_url"`
	// PricePerSearch is dollars per search as exact decimal text, or empty for
	// the rate AWS published.
	PricePerSearch string `json:"price_per_search"`
}

// handleWebSearch serves the web search configuration and its state.
//
// Administrators only, like everything that decides what this deployment sends
// where: a search query is written by a model from somebody's conversation.
func (a *API) handleWebSearch(w http.ResponseWriter, r *http.Request) {
	s, configured, err := a.db.EffectiveWebSearch(r.Context(), a.webSearchDep)
	if err != nil {
		a.fail(w, r, "reading the web search configuration", err)
		return
	}
	writeJSON(w, http.StatusOK, a.webSearchState(s, configured))
}

// handleSaveWebSearch writes the configuration and reloads the backend.
//
// Saved, then built, then reported: a gateway that saves and fails to build is
// on the screen as exactly that, with the reason, rather than a form that
// refused and left nothing behind to fix. The same order connecting an
// integration uses.
func (a *API) handleSaveWebSearch(w http.ResponseWriter, r *http.Request) {
	var req webSearchRequest
	if !decodeBody(w, r, &req) {
		return
	}

	d := a.webSearchKind
	if d.Endpoint == nil {
		writeAPIError(w, r, http.StatusServiceUnavailable, "This build has no web search backend.")
		return
	}
	raw := req.GatewayURL
	if a.webSearchDep.Set() {
		// The deployment's gateway is the only one it may have, and the switch
		// is the only thing an administrator decides. Anything else in the
		// body is refused rather than ignored, so a script that thinks it set
		// a price is told it did not.
		if (raw != "" && raw != a.webSearchDep.GatewayURL) || strings.TrimSpace(req.PricePerSearch) != "" {
			writeAPIError(w, r, http.StatusBadRequest,
				"This deployment's gateway and price are set by its configuration (web_search.gateway_url); only the switch can be changed here.")
			return
		}
		raw = a.webSearchDep.GatewayURL
	}
	gw, err := d.Endpoint(raw)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	settings := store.WebSearchSettings{
		Kind:         d.Kind,
		Enabled:      req.Enabled,
		GatewayURL:   gw.URL,
		ConfiguredBy: actorFor(PrincipalFrom(r.Context())),
	}
	if text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.PricePerSearch), "$")); text != "" {
		price, perr := pricing.ParseUSD(text)
		if perr != nil || price < 0 {
			writeAPIError(w, r, http.StatusBadRequest, "The price per search is a dollar amount, like 0.007.")
			return
		}
		settings.Price = &price
	}

	if serr := a.db.SaveWebSearch(r.Context(), settings); serr != nil {
		a.fail(w, r, "saving the web search configuration", serr)
		return
	}
	a.log.InfoContext(r.Context(), "web search configured",
		"backend", string(settings.Kind), "gateway", gw.Name, "region", gw.Region,
		"enabled", settings.Enabled, "by", settings.ConfiguredBy,
		"request_id", RequestIDFrom(r.Context()))

	// The build error, if any, is held by the service and served below; it
	// is logged there, once.
	_ = a.webSearch.Reload(r.Context())

	saved, configured, err := a.db.EffectiveWebSearch(r.Context(), a.webSearchDep)
	if err != nil {
		a.fail(w, r, "reading the web search configuration", err)
		return
	}
	writeJSON(w, http.StatusOK, a.webSearchState(saved, configured))
}

// handleCheckWebSearch asks the gateway for its tools and records the answer.
// It runs no search, so it costs nothing.
func (a *API) handleCheckWebSearch(w http.ResponseWriter, r *http.Request) {
	s, configured, err := a.db.EffectiveWebSearch(r.Context(), a.webSearchDep)
	if err != nil {
		a.fail(w, r, "reading the web search configuration", err)
		return
	}
	if !configured {
		writeAPIError(w, r, http.StatusBadRequest, "Web search has no gateway to check yet. Save one first.")
		return
	}
	if s.Deployed {
		// A verification is recorded on the row, against its gateway. The
		// deployment's gateway may have no row yet, or one naming a gateway
		// pasted before the stack made its own, so the row is brought to it
		// first; the switch keeps whatever it was.
		row, found, rerr := a.db.WebSearch(r.Context())
		if rerr != nil {
			a.fail(w, r, "reading the web search configuration", rerr)
			return
		}
		if !found || row.Kind != s.Kind || row.GatewayURL != s.GatewayURL {
			if serr := a.db.SaveWebSearch(r.Context(), store.WebSearchSettings{
				Kind: s.Kind, Enabled: s.Enabled, GatewayURL: s.GatewayURL,
				ConfiguredBy: actorFor(PrincipalFrom(r.Context())),
			}); serr != nil {
				a.fail(w, r, "recording the web search check", serr)
				return
			}
		}
	}

	// A reload first, so the check is of what is saved and not of what this
	// replica built before somebody changed it elsewhere.
	_ = a.webSearch.Reload(r.Context())
	state := a.webSearch.Current()

	var checkErr error
	switch {
	case state.BuildErr != nil:
		checkErr = state.BuildErr
	case state.Backend == nil:
		checkErr = fmt.Errorf("the web search backend has not been built")
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		checkErr = state.Backend.Check(ctx)
		cancel()
	}

	if checkErr == nil {
		if verr := a.db.SetWebSearchVerified(r.Context(), s.Kind, s.GatewayURL, time.Now()); verr != nil {
			a.fail(w, r, "recording the web search check", verr)
			return
		}
		a.log.InfoContext(r.Context(), "web search checked from the console",
			"backend", string(s.Kind), "actor", actorFor(PrincipalFrom(r.Context())))
	} else {
		if cerr := a.db.ClearWebSearchVerified(r.Context(), s.Kind); cerr != nil {
			a.fail(w, r, "recording the web search check", cerr)
			return
		}
		a.log.WarnContext(r.Context(), "web search check failed",
			"backend", string(s.Kind), "error", checkErr, "error_kind", string(websearch.KindOf(checkErr)))
	}

	checked, _, err := a.db.EffectiveWebSearch(r.Context(), a.webSearchDep)
	if err != nil {
		a.fail(w, r, "reading the web search configuration", err)
		return
	}
	body := a.webSearchState(checked, true)
	body["ok"] = checkErr == nil
	if checkErr != nil {
		body["error"] = checkErr.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// webSearchState is the configuration as the settings screen reads it.
func (a *API) webSearchState(s store.WebSearchSettings, configured bool) map[string]any {
	state := a.webSearch.Current()
	out := map[string]any{
		"configured":      configured,
		"serving":         state.Serving(),
		"published_price": websearch.PublishedPrice.USD(),
		"mcp_url":         strings.TrimSuffix(a.server.cfg.Server.BaseURL, "/") + "/mcp",
		// What the deployment's role needs to call a gateway, as the text an
		// operator pastes. The gateway's own service role, which runs the
		// search, is created with the gateway.
		"role_policy": a.webSearchPolicyText(),
		"label":       a.webSearchKind.Label,
		"state":       webSearchWord(s, configured, state),
	}
	if !configured {
		return out
	}
	price, source := s.PricePerSearch()
	out["kind"] = s.Kind
	out["deployed"] = s.Deployed
	out["enabled"] = s.Enabled
	out["gateway_url"] = s.GatewayURL
	out["price_per_search"] = price.USD()
	out["price_source"] = source
	if a.webSearchKind.Endpoint != nil {
		if gw, err := a.webSearchKind.Endpoint(s.GatewayURL); err == nil {
			out["gateway_id"] = gw.Name
			out["region"] = gw.Region
		}
	}
	if s.VerifiedAt != nil {
		out["verified_at"] = s.VerifiedAt.Format(time.RFC3339)
	}
	if !s.ConfiguredAt.IsZero() {
		out["configured_at"] = s.ConfiguredAt.Format(time.RFC3339)
	}
	if state.BuildErr != nil {
		out["error"] = state.BuildErr.Error()
	}
	return out
}

// webSearchWord is the state in one word, for the pill beside the name. Five
// rather than two, for the reason a model provider has five: "failing" and
// "never checked" are both not working, and an operator reaching for the logs
// needs to know which.
func webSearchWord(s store.WebSearchSettings, configured bool, state *websearch.State) string {
	switch {
	case !configured:
		return "not configured"
	case !s.Enabled:
		return "off"
	case state.BuildErr != nil || state.Backend == nil:
		return "failing"
	case s.VerifiedAt == nil:
		return "never checked"
	default:
		return "connected"
	}
}

func (a *API) webSearchPolicyText() string {
	if a.webSearchKind.RolePolicy == nil {
		return ""
	}
	raw, err := json.MarshalIndent(a.webSearchKind.RolePolicy, "", "  ")
	if err != nil {
		// A literal of strings cannot fail to encode.
		a.log.Error("rendering the web search role policy", "error", err)
		return ""
	}
	return string(raw)
}

// searchServing reports whether My setup should configure the MCP server.
// It is the same test the endpoint applies before listing the tool, so the
// screen never tells somebody to add a server that offers nothing.
func (a *API) searchServing() bool { return a.webSearch.Current().Serving() }

// claudeCodeMCP is how Claude Code is told about the MCP server: one command,
// and a deny rule for its own WebSearch tool.
//
// The rule is not optional. Claude Code's WebSearch is Anthropic's server-side
// tool, which Bedrock refuses, so left available the model would reach for it
// first and get an error; denied, it is not in the model's tool list at all
// and this server's web_search is the only search there is.
func claudeCodeMCP(base string) map[string]any {
	return map[string]any{
		"command": fmt.Sprintf("claude mcp add --transport http --scope user %s %s/mcp --header \"Authorization: Bearer %s\"",
			mcpServerName, base, tokenPlaceholder),
		"settings": map[string]any{
			"permissions": map[string]any{"deny": []string{"WebSearch"}},
		},
	}
}

// openCodeMCP is the mcp block opencode reads, beside its provider blocks in
// the same file.
func openCodeMCP(base, credential string) map[string]any {
	return map[string]any{
		mcpServerName: map[string]any{
			"type":    "remote",
			"url":     base + "/mcp",
			"enabled": true,
			"headers": map[string]string{"Authorization": "Bearer " + credential},
		},
	}
}

// codexMCP is the config.toml table Codex reads. The token comes from the
// same variable the model provider entry names.
func codexMCP(base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[mcp_servers.%s]\n", mcpServerName)
	fmt.Fprintf(&b, "url = %q\n", base+"/mcp")
	b.WriteString("bearer_token_env_var = \"PANORAMA_TOKEN\"\n")
	return b.String()
}
