// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheCLIWritesWhatTheConsoleShows is the claim nothing else checks.
//
// The console renders a configuration for somebody to paste, and `panorama
// configure` writes one. They are two doors onto the same generator, and the
// whole point of the server owning both is that they cannot disagree - which is
// true until somebody changes one of them.
func TestTheCLIWritesWhatTheConsoleShows(t *testing.T) {
	d := newDeployment(t)
	s := newSandbox(t, d)
	s.deviceLogin(t, d)

	setup := d.toolSetup()

	t.Run("claude code", func(t *testing.T) {
		s.configure(t, d, "claude-code", "-write")

		var written struct {
			APIKeyHelper string            `json:"apiKeyHelper"`
			Env          map[string]string `json:"env"`
		}
		readJSON(t, filepath.Join(s.home, ".claude", "settings.json"), &written)

		if written.APIKeyHelper == "" {
			t.Error("no apiKeyHelper was written; the credential would never rotate")
		}
		if got := written.Env["ANTHROPIC_BASE_URL"]; got != d.baseURL {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", got, d.baseURL)
		}

		// The route names are the deployment's answer, and the console shows
		// the same ones. A mismatch here means an engineer following the screen
		// and an engineer running the command end up on different models.
		row := setup.row(t, "claude-code")
		for name, want := range consoleEnv(t, row) {
			if !strings.HasSuffix(name, "_MODEL") {
				continue
			}
			if got := written.Env[name]; got != want {
				t.Errorf("%s: the CLI wrote %q, the console shows %q", name, got, want)
			}
		}

		// The helper's interval must be shorter than the point at which
		// `panorama token` refreshes, or Claude Code would go on using an
		// expired token for the rest of its own interval.
		if written.Env["CLAUDE_CODE_API_KEY_HELPER_TTL_MS"] == "" {
			t.Error("no helper TTL was written; Claude Code would cache the credential on its own schedule")
		}

		// No window and no output cap. A route for Claude Code carries its
		// model's Anthropic ID, and Claude Code knows both numbers for a model
		// it recognises; a figure written here would describe one route and be
		// applied to every other.
		for _, name := range []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_CODE_MAX_OUTPUT_TOKENS"} {
			if got, ok := written.Env[name]; ok {
				t.Errorf("the CLI wrote %s=%s", name, got)
			}
		}
	})

	t.Run("opencode", func(t *testing.T) {
		s.configure(t, d, "opencode", "-write")

		// The documented path, and on Windows this is the assertion that
		// matters: `panorama configure` joins $HOME/.config unconditionally.
		path := filepath.Join(s.home, ".config", "opencode", "opencode.json")

		var written map[string]any
		readJSON(t, path, &written)

		provider, ok := written["provider"].(map[string]any)
		if !ok {
			t.Fatalf("no provider block was written:\n%v", written)
		}
		panorama, ok := provider["panorama"].(map[string]any)
		if !ok {
			t.Fatalf("no panorama provider in the block:\n%v", provider)
		}

		options, _ := panorama["options"].(map[string]any)
		// The AI SDK appends /messages, so this one carries /v1 and Claude
		// Code's does not. Getting it wrong produces a 404 at the first turn.
		if got := options["baseURL"]; got != d.baseURL+"/v1" {
			t.Errorf("baseURL = %v, want %s/v1", got, d.baseURL)
		}

		models, ok := panorama["models"].(map[string]any)
		if !ok || len(models) == 0 {
			t.Fatal("the provider block declares no models; opencode would have nothing to pick")
		}

		// Every model must carry the user agent, because it is the only reason
		// opencode traffic is identifiable at all.
		for name, raw := range models {
			entry, _ := raw.(map[string]any)
			headers, _ := entry["headers"].(map[string]any)
			if got, _ := headers["user-agent"].(string); !strings.Contains(got, "opencode") {
				t.Errorf("model %q carries user-agent %q; its requests would be recorded as unknown", name, got)
			}

			// The window and output cap the browser cannot compute. They are
			// omitted rather than guessed when a model's window is unrecorded,
			// so an absent limit is only correct if the catalogue has no
			// window - and this fixture's models all do.
			limit, ok := entry["limit"].(map[string]any)
			if !ok {
				t.Errorf("model %q carries no limit; opencode would assume its own default window", name)
				continue
			}
			if ctx, _ := limit["context"].(float64); ctx <= 0 {
				t.Errorf("model %q declares a context window of %v", name, limit["context"])
			}
		}

		// Declaring routes and selecting none leaves opencode on whatever its
		// picker had last, which on a first run is nothing at all.
		if written["model"] == nil || written["small_model"] == nil {
			t.Error("the block selects no model; a first run would have a configured provider and no model")
		}
	})
}

// consoleEnv reads the environment rows out of a setup row.
func consoleEnv(t *testing.T, row map[string]any) map[string]string {
	t.Helper()

	raw, ok := row["env"].([]any)
	if !ok {
		t.Fatalf("the row carries no env rows:\n%v", row)
	}
	out := make(map[string]string, len(raw))
	for _, item := range raw {
		pair, _ := item.(map[string]any)
		name, _ := pair["name"].(string)
		value, _ := pair["value"].(string)
		out[name] = value
	}
	return out
}

// TestTheConsoleConfigNamesOnlyRoutes holds the rule a client never sees: what
// goes into ANTHROPIC_MODEL is a name somebody routed, never a model id.
func TestTheConsoleConfigNamesOnlyRoutes(t *testing.T) {
	d := newDeployment(t)
	setup := d.toolSetup()

	env := consoleEnv(t, setup.row(t, "claude-code"))
	routes := map[string]bool{routeStandard: true, routeFast: true, routeThrifty: true, routeMantleSonnet: true}

	for _, name := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		value := env[name]
		if value == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		if !routes[value] {
			t.Errorf("%s = %q, which is not a route on this deployment", name, value)
		}
		if strings.Contains(value, "anthropic.") {
			t.Errorf("%s = %q looks like a model id; a client must never name a model", name, value)
		}
	}

	// The small route is picked by looking at the catalogue's display name, so
	// this is the assertion that catches a haiku route it failed to find.
	if env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != routeFast {
		t.Errorf("the small route is %q, want %q", env["ANTHROPIC_DEFAULT_HAIKU_MODEL"], routeFast)
	}

	// Nothing served here may be a real credential.
	blob, err := json.Marshal(setup)
	if err != nil {
		t.Fatalf("re-encoding the setup response: %v", err)
	}
	if strings.Contains(string(blob), d.token) {
		t.Error("the setup endpoint echoed a live gateway token")
	}
}
