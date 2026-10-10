// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/tools"
)

// modelsSurface says which client is asking for the model list. Anthropic's
// clients send an anthropic-version header on every call and OpenAI's never
// do, which is the one signal the two share a path on.
func modelsSurface(r *http.Request) *surface {
	if r.Header.Get("anthropic-version") != "" {
		return anthropicSurface
	}
	return chatSurface
}

// handleModels lists the routes the caller may use, as models.
//
// A coding tool that lists models before it sends one - opencode's picker, a
// Codex /model menu - is shown route names, because a route name is the only
// thing a client may ask for. Filtered exactly as a refusal is: by the route's
// conditions for this caller and this tool, and by the API the listing client
// speaks, so nothing listed is a name the next request would be refused for.
//
// Budget state is deliberately not applied, for the reason Set.Available gives:
// a list a tool may cache should not change with the hour.
func (d *DataPlane) handleModels(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	sf := modelsSurface(r)

	set := d.policy.Snapshot()
	teams := set.Teams[p.UserID]
	if p.TeamID != "" {
		teams = []string{p.TeamID}
	}
	in := policy.Input{UserID: p.UserID, TeamID: p.TeamID, Tool: tools.FromUserAgent(r.UserAgent())}

	var names []string
	if sf.api == providers.APIAnthropicMessages {
		in.API = providers.APIAnthropicMessages
		names = set.Available(in, teams)
	} else {
		// An OpenAI client may speak either OpenAI API, and says nothing about
		// which on this path, so it is shown the routes reachable by either.
		for _, api := range []providers.API{providers.APIOpenAIChat, providers.APIOpenAIResponses} {
			in.API = api
			for _, name := range set.Available(in, teams) {
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
		slices.Sort(names)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if sf.api == providers.APIAnthropicMessages {
		type model struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		}
		data := make([]model, 0, len(names))
		for _, n := range names {
			data = append(data, model{Type: "model", ID: n, DisplayName: n,
				CreatedAt: time.Unix(0, 0).UTC().Format(time.RFC3339)})
		}
		body := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
		if len(names) > 0 {
			body["first_id"], body["last_id"] = names[0], names[len(names)-1]
		}
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(names))
	for _, n := range names {
		data = append(data, model{ID: n, Object: "model", OwnedBy: "panorama"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
