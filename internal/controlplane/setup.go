// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/tools"
)

// Setup steps, in the order the wizard runs them.
//
// A provider and models are two steps rather than one, and the split is the
// whole shape of this screen. Reaching a model provider is deployment
// configuration - a region, a set of credentials, an egress path - settled once
// by whoever stands the deployment up, and the only step a setup session can do
// anything about.
// Which models engineers may ask for is an operational decision made afterwards
// by an administrator, revisited whenever AWS ships a model or a team's costs
// change, and stored in the database. Folding them together made a wizard that
// asked somebody holding a one-time code to make a choice they had no standing
// to make.
const (
	stepIdentity     = "identity"
	stepProvider     = "provider"
	stepModels       = "models"
	stepTeam         = "team"
	stepDevelopers   = "developers"
	stepIntegrations = "integrations"
)

// Settings the wizard records.
//
// Only one kind of thing is recorded here: a choice nothing else can reveal
// (was this step skipped on purpose). The other thing setup ever recorded - that
// a live invocation succeeded at a moment - lives on the provider it verified,
// and is cleared when that provider's region changes. Everything else about a
// step's state is derived from the deployment itself.
//
// That is the difference between a wizard that is honest and one that is not. A
// stored "identity: done" flag says done after somebody removed the issuer from
// the configuration; a derived one says what is true right now.
const (
	settingSkippedPrefix  = "setup.skipped."
	settingSetupDismissed = "setup.dismissed"
)

// Step states.
const (
	stateDone    = "done"
	stateTodo    = "todo"
	stateSkipped = "skipped"
)

// handleSetupState reports where the deployment stands.
func (a *API) handleSetupState(w http.ResponseWriter, r *http.Request) {
	settings, err := a.db.Settings(r.Context())
	if err != nil {
		a.fail(w, r, "reading setup state", err)
		return
	}

	steps, err := a.setupSteps(r.Context(), settings)
	if err != nil {
		a.fail(w, r, "reading setup state", err)
		return
	}

	remaining := 0
	for _, s := range steps {
		if s["state"] == stateTodo && s["required"] == true {
			remaining++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"steps":     steps,
		"remaining": remaining,
		"complete":  remaining == 0,
		"dismissed": settings[settingSetupDismissed] == "true",
		"endpoint":  a.server.cfg.Server.BaseURL,
		// A setup session exists to do exactly this, so it configures as
		// much as an administrator does. Anyone else is reading.
		"can_configure": canConfigure(PrincipalFrom(r.Context())),
	})
}

// setupSteps derives each step's state from the deployment.
func (a *API) setupSteps(ctx context.Context, settings map[string]string) ([]map[string]any, error) {
	users, err := a.db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	teams, err := a.db.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	integrationsConnected, err := a.db.AnyIntegrationConfigured(ctx)
	if err != nil {
		return nil, err
	}

	var (
		signedInViaSSO bool
		onboarded      int
		admins         int
	)
	for _, u := range users {
		if u.ExternalID != "" {
			signedInViaSSO = true
		}
		if u.Onboarded() || u.Tokens > 0 {
			onboarded++
		}
		if u.Role == store.RoleAdmin {
			admins++
		}
	}

	budgets := 0
	for _, s := range a.policy.Status() {
		if s.Budget.Enabled {
			budgets++
		}
	}

	// Verified, not merely configured. A live invocation is the only thing that
	// distinguishes a region and a set of credentials that work from ones that
	// look right, and it is the only check that sees an SCP, a missing
	// Marketplace permission, a role without the other endpoint's actions, or a
	// wrong identifier - all of which look like success from every list AWS
	// publishes.
	configured, err := a.db.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	providerVerified := false
	providerRows := make([]map[string]any, 0, len(configured))
	for _, ps := range configured {
		row := map[string]any{
			"kind": ps.Kind, "label": a.providerLabel(ps.Kind),
			"region": ps.Region, "enabled": ps.Enabled,
		}
		if ps.VerifiedAt != nil {
			row["verified_at"] = ps.VerifiedAt.UTC().Format(time.RFC3339)
			if ps.Enabled {
				providerVerified = true
			}
		}
		providerRows = append(providerRows, row)
	}

	// How many names a client may ask for. Routes are the whole of the
	// allowlist, so this is the count: zero means the gateway refuses
	// everything, however well Bedrock is reachable.
	saved, rerr := a.db.ListRoutes(ctx)
	if rerr != nil {
		return nil, rerr
	}
	// Counted as "routes that can serve something", not as rows. A route with
	// no model admits a request and then has nowhere to send it, and a setup
	// step that called that done would be marking the deployment ready while
	// every request it serves is refused.
	routes := 0
	for _, route := range saved {
		if !route.Target.IsZero() {
			routes++
		}
	}

	catalogue, cerr := a.db.ListCatalogue(ctx)
	if cerr != nil {
		return nil, cerr
	}
	catalogueSize := len(catalogue)

	steps := []map[string]any{
		{
			"id":       stepIdentity,
			"title":    "Connect identity",
			"summary":  "Sign in with your organization's identity provider, and map its groups to Panorama roles.",
			"required": false,
			"done":     a.server.cfg.OIDC.Configured() && signedInViaSSO,
			"detail": map[string]any{
				"configured":   a.server.cfg.OIDC.Configured(),
				"issuer":       a.server.cfg.OIDC.Issuer,
				"signed_in":    signedInViaSSO,
				"redirect_url": a.server.identity.redirectURL,
				// The split is stated here as well as on the identity screen,
				// because this is where somebody first goes looking for a form
				// to type an issuer into and does not find one.
				"note": "The connection lives in the configuration file or the environment, beside every other secret. Panorama verifies it here; it does not store your client secret in its database.",
			},
		},
		{
			"id":      stepProvider,
			"title":   "Connect a model provider",
			"summary": "Choose where a model provider is invoked, and prove this deployment can actually invoke a model there.",
			// Verified, not merely configured: see providerVerified above.
			"required": true,
			"done":     providerVerified,
			"detail": map[string]any{
				"providers":  providerRows,
				"discovered": catalogueSize,
				"note": "A model provider is a region and a switch, stored in the database - set here, or later " +
					"in Settings, Model providers. Panorama never asks you to paste an access key - both " +
					"Bedrock providers sign with whatever the AWS SDK's default credential chain finds, and the " +
					"role's policy grants both endpoints in every region, so choosing a region never needs an IAM " +
					"change. Which models your engineers may ask for is not set here: that is chosen afterwards, " +
					"on Routing.",
			},
		},
		{
			"id":      stepModels,
			"title":   "Choose which models engineers may use",
			"summary": "Pick from what this account can actually invoke, and decide where each one's traffic is processed.",
			// Separate from reaching a provider, and required in its own right: a
			// deployment that can invoke its providers perfectly and has no route
			// refuses every request, and nothing about the provider step would
			// say so.
			"required": true,
			"done":     routes > 0,
			"detail": map[string]any{
				"routes":     routes,
				"discovered": catalogueSize,
				"providers":  providerRows,
				"note": "Every name a client may ask for is a route, stored in the database and changeable " +
					"without a redeploy. A route says who it exists for and which model it runs on; an " +
					"engineer points their tool at its name and never names a model. Choosing " +
					"a geography chooses where requests are processed and what they cost - a local profile " +
					"keeps traffic in one country and bills about ten per cent more than global.",
			},
		},
		{
			"id":       stepTeam,
			"title":    "Create a team and set a budget",
			"summary":  "A team to group people by, and a budget so nothing runs away before anyone is watching.",
			"required": true,
			"done":     len(teams) > 0 && budgets > 0,
			"detail":   map[string]any{"teams": len(teams), "budgets": budgets},
		},
		{
			"id":       stepDevelopers,
			"title":    "Onboard developers",
			"summary":  "Send engineers the setup link. One environment variable each, and their spend starts appearing against real work.",
			"required": true,
			"done":     onboarded > 1,
			"detail": map[string]any{
				"users": len(users), "onboarded": onboarded, "admins": admins,
				"onboarding_url": strings.TrimSuffix(a.server.cfg.Server.BaseURL, "/") + "/setup",
			},
		},
		{
			"id":       stepIntegrations,
			"title":    "Connect GitHub",
			"summary":  "Optional. Attribution already works without it; what it adds is titles, merge status, and cost per merged pull request.",
			"required": false,
			"done":     integrationsConnected,
			"detail":   map[string]any{"connected": integrationsConnected},
		},
	}

	for _, step := range steps {
		id, _ := step["id"].(string)
		done, _ := step["done"].(bool)
		switch {
		case done:
			step["state"] = stateDone
		case settings[settingSkippedPrefix+id] == "true":
			step["state"] = stateSkipped
		default:
			step["state"] = stateTodo
		}
		delete(step, "done")
	}
	return steps, nil
}

type setupStepRequest struct {
	Step string `json:"step"`
	// Action is "skip", "unskip", or "dismiss".
	Action string `json:"action"`
}

// handleSetupStep records the only two things about setup that cannot be
// derived: that a step was skipped on purpose, and that the whole banner was
// dismissed.
func (a *API) handleSetupStep(w http.ResponseWriter, r *http.Request) {
	var req setupStepRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Action == "dismiss" {
		if err := a.db.SetSetting(r.Context(), settingSetupDismissed, "true"); err != nil {
			a.fail(w, r, "recording setup state", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dismissed": true})
		return
	}

	known := map[string]bool{
		stepIdentity: true, stepProvider: true, stepModels: true,
		stepTeam: true, stepDevelopers: true, stepIntegrations: true,
	}

	if !known[req.Step] {
		writeAPIError(w, r, http.StatusBadRequest, "That is not a setup step.")
		return
	}

	switch req.Action {
	case "skip":
		if err := a.db.SetSetting(r.Context(), settingSkippedPrefix+req.Step, "true"); err != nil {
			a.fail(w, r, "recording setup state", err)
			return
		}
	case "unskip":
		if _, err := a.db.DeleteSetting(r.Context(), settingSkippedPrefix+req.Step); err != nil {
			a.fail(w, r, "recording setup state", err)
			return
		}
	default:
		writeAPIError(w, r, http.StatusBadRequest, `Action must be "skip", "unskip", or "dismiss".`)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"step": req.Step, "action": req.Action})
}

// tool describes one coding tool's setup.
type tool struct {
	ID        string
	Name      string
	Supported bool
	// Surface names the API this tool speaks, which is what decides whether
	// this deployment can serve it yet.
	Surface string
	Note    string
}

// toolSetups is the honest coverage list.
//
// Three tools are configured from this screen: Claude Code speaks the Anthropic
// Messages API, Codex speaks only OpenAI Responses, and opencode speaks all
// three APIs this gateway serves - one provider block per API, so a route on a
// Claude model and a route on an OpenAI or open-weight model are all in its
// picker. The rest are listed anyway, marked, because a setup screen that hides
// what it cannot do is a screen an engineer stops trusting.
//
// opencode is first, and the ordering is a recommendation rather than a
// ranking. A route name is a model this deployment invented, and the clients
// differ in what they can be told about one: opencode's configuration declares
// a model outright, context window included, while Claude Code looks the name
// up in a registry it ships and assumes 200k when it misses. The same route
// therefore reaches an engineer intact through one and diminished through the
// other, and the list leads with the one that arrives whole.
var toolSetups = []tool{
	{
		ID: tools.OpenCode, Name: "opencode", Supported: true, Surface: "anthropic, openai-responses, openai-chat",
		Note: "Native. One provider block per API, pointed at the gateway, streaming untouched. Every route you may use is declared as a model with its context window beside it, so anything selectable in the picker is a route already conditioned to you.",
	},
	{
		ID: tools.ClaudeCode, Name: "Claude Code", Supported: true, Surface: "anthropic",
		Note: "Native. Two environment variables, no proxy shim, streaming untouched. A route named after its model, such as claude-sonnet-5, gets requests prepared for that model; any other name is treated as its newest. Only routes whose model speaks the Anthropic Messages API are offered.",
	},
	{
		ID: tools.CodexCLI, Name: "Codex CLI", Supported: true, Surface: "openai-responses",
		Note: "Native, over the Responses API. A model provider entry in config.toml pointed at the gateway. Only routes whose model speaks Responses are offered, and responses are never stored: Codex resends the conversation each turn, which is what it does without server-side storage anyway.",
	},
	{
		ID: tools.Cline, Name: "Cline", Surface: "openai-chat",
		Note: "Speaks Chat Completions, which this gateway serves at /v1/chat/completions for routes whose model speaks it. Not configured from this screen, and its request shape is not yet covered by the end-to-end suite.",
	},
	{
		ID: tools.Continue, Name: "Continue", Surface: "openai-chat",
		Note: "Speaks Chat Completions, which this gateway serves at /v1/chat/completions for routes whose model speaks it. Not configured from this screen, and its request shape is not yet covered by the end-to-end suite.",
	},
	{
		ID: tools.GeminiCLI, Name: "Gemini CLI", Surface: "gemini",
		Note: "Speaks Google's own API, which this gateway does not serve.",
	},
	{
		ID: "copilot", Name: "GitHub Copilot", Surface: "none",
		Note: "Cannot be proxied. Copilot talks to GitHub's own endpoint and offers no way to point it elsewhere. Stated because pretending otherwise would be the first thing an engineer caught us on.",
	},
	{
		ID: "cursor", Name: "Cursor", Surface: "partial",
		Note: "Partial. Cursor can be pointed at a custom OpenAI-compatible endpoint for some features; its agent traffic does not go through it, so attribution sees only part of the spend.",
	},
}

// handleToolSetup serves the copy-paste configuration for this deployment and
// this person.
//
// It does not mint a token, and it does not include one. The console asks for a
// token explicitly, shows it once, and substitutes it into these blocks on the
// page - so a screen that someone leaves open, or screenshots, does not carry a
// working credential.
func (a *API) handleToolSetup(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	base := strings.TrimSuffix(a.server.cfg.Server.BaseURL, "/")

	out := make([]map[string]any, 0, len(toolSetups))
	for _, t := range toolSetups {
		row := map[string]any{
			"id": t.ID, "name": t.Name, "supported": t.Supported,
			"surface": t.Surface, "note": t.Note,
		}
		if t.ID == tools.OpenCode && t.Supported {
			for k, v := range a.openCodeSetup(r.Context(), p, base) {
				row[k] = v
			}
			out = append(out, row)
			continue
		}
		if t.ID == tools.CodexCLI && t.Supported {
			for k, v := range a.codexSetup(r.Context(), p, base) {
				row[k] = v
			}
			out = append(out, row)
			continue
		}
		if t.Supported {
			// A route name, never a model id. This is the whole of what an
			// engineer configures: which model that name runs on is a decision
			// this deployment makes and can change without anybody touching a
			// laptop.
			//
			// ANTHROPIC_MODEL rather than ANTHROPIC_DEFAULT_MODEL, because the
			// latter applies only when no settings file sets `model` - and the
			// file this configuration lands in is the same one /model saves
			// that key into. Anybody who has ever picked a model has one, so
			// the deployment's own setting would be outranked by a stale choice
			// and silently do nothing, which is the failure that is hardest to
			// see from here. ANTHROPIC_MODEL wins over the file, and is also
			// the older variable, so it works on clients that predate the other.
			// /model still switches within a session; it just stops persisting,
			// which is the right trade for a name the deployment decides.
			//
			// Two model variables, and the second is not optional. The first
			// covers what the engineer asks for; the second covers what Claude
			// Code asks for on its own - summarising a conversation, naming a
			// session, and any subagent whose definition names the haiku tier.
			// Those are made without anybody choosing a model, so leaving the
			// variable unset means a name nobody routed arrives unprompted and
			// the tool looks broken rather than unconfigured.
			//
			// The opus, sonnet and fable tiers are deliberately NOT set. They
			// are only consulted when somebody deliberately picks that tier,
			// and pointing them at this route would mean choosing Opus quietly
			// serves whatever the route runs - a substitution nothing reports,
			// on the one screen where the engineer is being explicit about what
			// they want. A deployment that does not offer Opus should say so,
			// and the refusal does: it names the model and lists the routes
			// that exist. An administrator who does want a tier served adds a
			// route and the engineer names it.
			//
			// ANTHROPIC_SMALL_FAST_MODEL is deprecated in favour of
			// ANTHROPIC_DEFAULT_HAIKU_MODEL and is not sent.
			primary, small := a.routesFor(r.Context(), p, t.ID, providers.APIAnthropicMessages)
			env := [][2]string{
				{"ANTHROPIC_BASE_URL", base},
				{"ANTHROPIC_AUTH_TOKEN", tokenPlaceholder},
				{"ANTHROPIC_MODEL", primary},
				{"ANTHROPIC_DEFAULT_HAIKU_MODEL", small},
			}
			// No window and no output cap. A route for Claude Code is named
			// with its model's Anthropic ID (the route editor asks for it), so
			// Claude Code recognises the model and knows both numbers itself -
			// and shapes the request for that model, which is the part the
			// variables never could. They would describe the served primary
			// route only: a reader who picked another route would paste that
			// route's name beside the first one's window.
			rows := make([]map[string]string, 0, len(env))
			exports := make([]string, 0, len(env)+2)
			for _, kv := range env {
				rows = append(rows, map[string]string{"name": kv[0], "value": kv[1]})
				exports = append(exports, "export "+kv[0]+"="+kv[1])
			}
			row["env"] = rows
			row["shell"] = strings.Join(append(exports, "", "claude"), "\n")
			row["routes"] = a.routeNamesFor(p, t.ID, providers.APIAnthropicMessages)
			row["path"] = claudeCodeConfigPath
			if a.searchServing() {
				row["mcp"] = claudeCodeMCP(base)
			}

			// The recommended path, offered first: sign in once and let the
			// credential rotate itself. The copy-paste block above stays,
			// because a machine with no Panorama binary on it still has to
			// work - but a token pasted into a shell profile is a credential
			// that lives until somebody remembers it, and this one does not.
			row["cli"] = map[string]any{
				"recommended": true,
				"why": "Nothing to paste and nothing to rotate. The token is short-lived, " +
					"refreshed in the background, and stops working when the machine is signed out.",
				// Two commands, served as two, because they are two steps a
				// person takes at two different moments - signing a machine in
				// is done once, writing a tool's configuration once per tool.
				// Joined into one string here, they were split again in the
				// browser on a newline, which is a parser standing between the
				// only two things that ever produce and consume them.
				"login":     "panorama login -url " + base,
				"configure": "panorama configure claude-code -write",
				// Shown as well as written, because an engineer wants to see
				// what a command is about to put in their settings file.
				//
				// #nosec G101 -- apiKeyHelper names a command, not a secret.
				// The indirection is the point: nothing here is a credential.
				"settings": map[string]any{
					"apiKeyHelper": "panorama token --quiet",
					"env":          settingsEnv(base, primary, small),
				},
			}
			// The precision tier, offered as an upgrade and never as a
			// requirement. Structural attribution already works; this is for
			// somebody who wants every request stamped rather than most.
			row["stamping"] = map[string]any{
				"optional": true,
				"why":      "Attribution already works without this. Stamping makes it exact rather than inferred, and moves the provenance rung from structural to stamp.",
				"headers": []string{
					attribution.HeaderRepo, attribution.HeaderBranch,
					attribution.HeaderTicket, attribution.HeaderPullRequest,
				},
				"shell": `export ANTHROPIC_CUSTOM_HEADERS="` +
					attribution.HeaderRepo + `: $(git remote get-url origin 2>/dev/null | sed -E 's#.*[:/]([^/]+/[^/]+?)(\.git)?$#\1#'), ` +
					attribution.HeaderBranch + `: $(git rev-parse --abbrev-ref HEAD 2>/dev/null)"`,
			}
		}
		out = append(out, row)
	}

	// The CLI half, removed in one place rather than guarded at each of the
	// three that build it. `cli` present is what says the sign-in path works,
	// so a deployment with the switch off serves the same rows with the
	// credential question already answered: there is one way to hold a token
	// and the screen prints the steps for it.
	if !a.server.cliSignInOn(r.Context()) {
		for _, row := range out {
			delete(row, "cli")
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tools":    out,
		"endpoint": base,
		"user":     map[string]string{"email": p.Email, "display_name": p.DisplayName},
		// The console substitutes the token it just minted for this; nothing
		// here carries a credential.
		"token_placeholder": tokenPlaceholder,
		// Whether the blocks above configure the MCP server. Served so the
		// screen can say what it is for, rather than leave somebody to find a
		// server entry and wonder.
		"web_search": a.searchServing(),
	})
}

// routesFor picks the two route names a tool is configured with: the one it
// uses for work, and the one it uses for the small, fast asides it makes on its
// own. Only routes whose model speaks api are candidates.
//
// The small one is chosen by looking at the model behind each route rather than
// at the route's name, because a name is whatever an administrator called it and
// the model underneath is the fact. A deployment with only one route gets that
// route twice, which is correct and slightly expensive, and the console says so.
//
// A deployment with no routes gets a placeholder rather than an empty string. A
// block that silently omits the variable would have the tool send its own
// built-in model name, which is not a route, and the refusal would look like a
// broken gateway rather than a step nobody has done yet.
func (a *API) routesFor(ctx context.Context, p *store.Principal, tool string, apis ...providers.API) (primary, small string) {
	names := a.routeNamesFor(p, tool, apis...)
	if len(names) == 0 {
		return routePlaceholder, routePlaceholder
	}

	primary, small = names[0], names[0]

	catalogue, err := a.db.ListCatalogue(ctx)
	if err != nil {
		// The names are still right; only the choice between them is worse.
		a.log.WarnContext(ctx, "reading the catalogue to pick a small model", "error", err)
		return primary, small
	}
	byRef := make(map[providers.ModelRef]store.CatalogueModel, len(catalogue))
	for _, m := range catalogue {
		byRef[m.Ref()] = m
	}

	routes, err := a.db.ListRoutes(ctx)
	if err != nil {
		a.log.WarnContext(ctx, "reading routes to pick a small model", "error", err)
		return primary, small
	}
	available := make(map[string]bool, len(names))
	for _, n := range names {
		available[n] = true
	}
	for _, route := range routes {
		if !available[route.Name] {
			continue
		}
		if m, known := byRef[route.Target]; known && strings.Contains(strings.ToLower(m.DisplayName), "haiku") {
			small = route.Name
			break
		}
	}
	return primary, small
}

// routeNamesFor lists the routes this person may ask for with this tool, over
// any of the given APIs.
//
// Filtered by the route's own conditions, by the tool's, and by what the route's
// model speaks, so the block a developer copies names something their request
// will actually be served. It reads the policy snapshot rather than deriving
// availability a second way: two derivations of "may this person use this
// route" would eventually disagree, and the one on the request path is the one
// that counts.
func (a *API) routeNamesFor(p *store.Principal, tool string, apis ...providers.API) []string {
	set := a.policy.Snapshot()
	teams := set.Teams[p.UserID]
	if p.TeamID != "" {
		teams = []string{p.TeamID}
	}
	in := policy.Input{UserID: p.UserID, TeamID: p.TeamID, Tool: tool}
	if len(apis) == 0 {
		return set.Available(in, teams)
	}
	var out []string
	for _, api := range apis {
		in.API = api
		for _, name := range set.Available(in, teams) {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// openCodeProvider is what the generated block calls this deployment inside
// opencode's own configuration, for routes on the Anthropic Messages API. It is
// the first half of every such model id the engineer then sees in the picker -
// "panorama/deep-work" - and of the two top-level keys that say which of them
// opencode starts on.
const openCodeProvider = "panorama"

// openCodeOpenAIProvider is the same, for routes whose model speaks OpenAI
// Responses. A second provider rather than a second model list in the first,
// because opencode binds a provider to one SDK package, and the package is what
// decides the request shape: @ai-sdk/openai speaks Responses.
//
// The name is the one this block had when it held Chat Completions routes, on
// purpose. Every route in it then was a model that also speaks Responses, and
// the file an engineer already has names them "panorama-openai/..."; keeping
// the name is what makes re-running setup replace the block that fails rather
// than leave it beside a new one.
const openCodeOpenAIProvider = "panorama-openai"

// openCodeChatProvider is the same again, for routes whose model speaks Chat
// Completions and not Responses, on the package that speaks that.
const openCodeChatProvider = "panorama-chat"

// openCodeConfigPath is where opencode reads its global configuration.
const openCodeConfigPath = "~/.config/opencode/opencode.json"

// claudeCodeConfigPath is where Claude Code reads its user settings.
//
// Served rather than written into the console, because it is the same string
// `panorama configure claude-code -write` writes to and the console labels a
// block with - and a path spelled in two places is a path that is eventually
// spelled two ways.
const claudeCodeConfigPath = "~/.claude/settings.json"

// codexConfigPath is where Codex reads its configuration.
const codexConfigPath = "~/.codex/config.toml"

// openCodeUserAgent is what the generated opencode configuration identifies
// itself as.
//
// opencode reaches the gateway through the Vercel AI SDK, whose providers send
// no user agent naming the tool that is using them - so without this every
// opencode request would be recorded as `unknown`, and a route condition naming
// opencode would match nothing. The provider block sets it per model, which is
// where opencode's own schema puts headers.
//
// Client-set, like every user agent, and nothing but identification is built
// on it. A route's tool condition has always been exactly this trusting; what
// changes here is only that the truth is available to be told.
const openCodeUserAgent = "opencode (panorama)"

// openCodeSetup renders the provider blocks opencode needs.
//
// It is a different shape from Claude Code's, and the difference is the reason
// opencode is listed first. Claude Code is told a route name and looks it up in
// a model registry it ships; a name that deployment invented is not in there,
// so it falls back to assuming a 200k context window and its own default output
// cap. opencode declares a model outright - name, context window, output cap -
// so a route arrives as what it actually is.
//
// Every route the reader may use is declared, not just one, under the provider
// block for the API its model speaks. opencode's picker offers what the blocks
// contain, so a person who can use three routes sees three; and because the
// providers are ours alone, nothing else is in those lists to be picked by
// accident.
func (a *API) openCodeSetup(ctx context.Context, p *store.Principal, base string) map[string]any {
	// A route on a model that speaks more than one API is declared once, under
	// the first of these it speaks.
	//
	// Responses comes before Chat Completions because a coding agent sends
	// tools on every request, and OpenAI's reasoning models refuse function
	// tools on Chat Completions unless reasoning is switched off: "Function
	// tools with reasoning_effort are not supported for gpt-5.6-terra in
	// /v1/chat/completions" (opencode on a GPT-5.6 route).
	// Responses serves both, and it is what opencode's own Mantle provider
	// uses for every model that has it. Chat Completions is for the models
	// that speak nothing else.
	anthropicRoutes := a.routeNamesFor(p, tools.OpenCode, providers.APIAnthropicMessages)
	declared := slices.Clone(anthropicRoutes)
	unclaimed := func(api providers.API) []string {
		out := []string{}
		for _, name := range a.routeNamesFor(p, tools.OpenCode, api) {
			if !slices.Contains(declared, name) {
				out = append(out, name)
				declared = append(declared, name)
			}
		}
		return out
	}
	responsesRoutes := unclaimed(providers.APIOpenAIResponses)
	chatRoutes := unclaimed(providers.APIOpenAIChat)
	if len(declared) == 0 {
		anthropicRoutes = []string{routePlaceholder}
	}
	limits := a.routeLimits(ctx)
	search := a.searchServing()

	// Which of the declared routes opencode starts on, and which it uses for
	// the small asides it makes on its own - the same pair Claude Code gets as
	// ANTHROPIC_MODEL and ANTHROPIC_DEFAULT_HAIKU_MODEL, picked by the same
	// function so the two clients cannot end up disagreeing about which route
	// is the everyday one.
	//
	// Declaring the providers without choosing a model left opencode on
	// whatever was last picked in its own picker, which for a first run is
	// nothing at all: the models were all there and none of them was selected.
	primary, small := a.routesFor(ctx, p, tools.OpenCode,
		providers.APIAnthropicMessages, providers.APIOpenAIResponses, providers.APIOpenAIChat)
	qualify := func(name string) string {
		switch {
		case slices.Contains(responsesRoutes, name):
			return openCodeOpenAIProvider + "/" + name
		case slices.Contains(chatRoutes, name):
			return openCodeChatProvider + "/" + name
		}
		return openCodeProvider + "/" + name
	}

	declare := func(names []string) map[string]any {
		models := make(map[string]any, len(names))
		for _, name := range names {
			entry := map[string]any{
				"name":    name,
				"headers": map[string]string{"user-agent": openCodeUserAgent},
			}
			// Omitted rather than guessed when this deployment cannot say. A
			// model entry with no limit leaves opencode on its own defaults,
			// which is the state it would have been in anyway; a limit invented
			// here would be a number an engineer's session is managed against.
			if lim, ok := limits[name]; ok {
				entry["limit"] = map[string]int{"context": lim.context, "output": lim.output}
			}
			models[name] = entry
		}
		return models
	}

	// Two renderings of one block, differing only in where the credential comes
	// from - the same split Claude Code's block makes between a pasted token
	// and `panorama token`.
	render := func(apiKey string) string {
		blocks := map[string]any{}
		if len(anthropicRoutes) > 0 {
			blocks[openCodeProvider] = map[string]any{
				"name": "Panorama",
				// The Anthropic provider, for routes on models that speak the
				// Messages API: the gateway serves it natively.
				"npm": "@ai-sdk/anthropic",
				"options": map[string]any{
					// With /v1, because the AI SDK appends "/messages" to
					// what it is given. Claude Code's ANTHROPIC_BASE_URL is
					// the deployment root and it appends the whole path
					// itself, so the blocks on this screen carry different
					// URLs for the same gateway on purpose.
					"baseURL": base + "/v1",
					// Sent as x-api-key, which the gateway accepts
					// alongside the Authorization header Claude Code uses.
					"apiKey": apiKey,
				},
				"models": declare(anthropicRoutes),
			}
		}
		if len(responsesRoutes) > 0 {
			blocks[openCodeOpenAIProvider] = map[string]any{
				"name": "Panorama (OpenAI)",
				// OpenAI's own provider, for routes on models that speak
				// Responses, which is the API it sends. opencode sets store
				// to false for this package and strips the stored-item ids
				// from the history it resends, so each turn is stateless -
				// which is the only kind of Responses request the gateway
				// serves.
				"npm": "@ai-sdk/openai",
				"options": map[string]any{
					// With /v1: the SDK appends "/responses".
					"baseURL": base + "/v1",
					// Sent as a bearer token, which the gateway accepts.
					"apiKey": apiKey,
				},
				"models": declare(responsesRoutes),
			}
		}
		if len(chatRoutes) > 0 {
			blocks[openCodeChatProvider] = map[string]any{
				"name": "Panorama (Chat Completions)",
				// The OpenAI-compatible provider, for routes on models that
				// speak Chat Completions and nothing else: the gateway serves
				// that natively too, and translates nothing between the two.
				"npm": "@ai-sdk/openai-compatible",
				"options": map[string]any{
					// With /v1 for the same reason: the SDK appends
					// "/chat/completions".
					"baseURL": base + "/v1",
					// Sent as a bearer token, which the gateway accepts.
					"apiKey": apiKey,
					// Asks for the usage chunk the gateway would otherwise ask
					// for on opencode's behalf - and then relay, because it was
					// asked for.
					"includeUsage": true,
				},
				"models": declare(chatRoutes),
			}
		}

		config := map[string]any{
			"$schema":  "https://opencode.ai/config.json",
			"provider": blocks,
			// Namespaced by provider, which is opencode's own spelling for a
			// model: "panorama/", "panorama-openai/" or "panorama-chat/" is
			// the provider block, and the name after it is a route.
			"model":       qualify(primary),
			"small_model": qualify(small),
		}
		// The gateway's MCP server, carrying the same credential as the
		// provider blocks, in the same file, so one paste configures both.
		if search {
			config["mcp"] = openCodeMCP(base, apiKey)
		}

		// Indented because a person reads it before they paste it, and because
		// the file it lands in is one they may already have written by hand.
		encoded, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			// Nothing here can fail to encode; if it somehow does, say so
			// rather than shipping a half-rendered block somebody would paste.
			a.log.ErrorContext(ctx, "rendering the opencode configuration", "error", err)
			return "{}"
		}
		return string(encoded)
	}

	return map[string]any{
		"routes": nonNilList(declared),
		"config": render(tokenPlaceholder),
		"path":   openCodeConfigPath,
		// Recommended, and honest about being weaker than Claude Code's. There
		// is no command-based credential helper in opencode: a key is a literal,
		// an environment variable, or the contents of a file. So the best
		// available is a token minted per shell - which rotates whenever a shell
		// is opened, and does not rotate underneath a session that outlives its
		// token's lifetime. Saying so is the point; a block that implied
		// otherwise would be found out by an expiry mid-session.
		"cli": map[string]any{
			"recommended": true,
			"why": "The token is minted per shell and refreshed when it is due, so nothing " +
				"long-lived is written to disk. opencode has no credential-helper command, " +
				"so a session running longer than a token's lifetime needs a new shell.",
			"config": render("{env:PANORAMA_TOKEN}"),
			"login":  "panorama login -url " + base,
			// Three lines in the second step rather than two, and the third is
			// what the provider block's `{env:PANORAMA_TOKEN}` reads. opencode
			// has no credential-helper command, so the export is part of
			// configuring it rather than an aside.
			"configure": strings.Join([]string{
				"panorama configure opencode -write",
				"export PANORAMA_TOKEN=$(panorama token --quiet)",
			}, "\n"),
		},
		// No stamping block. opencode's headers are static strings in a file, so
		// the shell interpolation Claude Code's block uses has nowhere to run: a
		// repo and branch captured when the file was written would be wrong by
		// the next checkout. Structural attribution covers this client without
		// one, and the screen says nothing rather than explaining an absence.
	}
}

// codexProvider is what the generated block calls this deployment in Codex's
// model_providers table.
const codexProvider = "panorama"

// codexSetup renders the config.toml entries Codex needs.
//
// A model provider entry pointed at the gateway, with `wire_api = "responses"`
// because that is the only API Codex speaks, and `env_key` naming the variable
// the token is read from. Codex has no credential-helper command either, so the
// credential arrives the way opencode's does: minted per shell.
//
// The window and output cap are written when this deployment can state them,
// for the reason they are everywhere else: a route name is a model Codex has
// never heard of, and its own assumption is the one a session is managed
// against otherwise.
func (a *API) codexSetup(ctx context.Context, p *store.Principal, base string) map[string]any {
	names := a.routeNamesFor(p, tools.CodexCLI, providers.APIOpenAIResponses)
	primary := routePlaceholder
	if len(names) > 0 {
		primary = names[0]
	}
	limits := a.routeLimits(ctx)
	search := a.searchServing()

	// One rendering per route as well as the default, because the window and
	// the output cap belong to the route chosen. The console swaps whole blocks
	// when a reader picks another route rather than rewriting numbers it has no
	// way to compute.
	configs := make(map[string]string, len(names))
	for _, name := range names {
		configs[name] = codexConfig(name, base, limits, search)
	}

	return map[string]any{
		"routes":  nonNilList(names),
		"config":  codexConfig(primary, base, limits, search),
		"configs": configs,
		"path":    codexConfigPath,
		"shell":   "export PANORAMA_TOKEN=" + tokenPlaceholder + "\ncodex",
		"cli": map[string]any{
			"recommended": true,
			"why": "The token is minted per shell and refreshed when it is due, so nothing " +
				"long-lived is written to disk. Codex has no credential-helper command, " +
				"so a session running longer than a token's lifetime needs a new shell.",
			"login":     "panorama login -url " + base,
			"configure": "export PANORAMA_TOKEN=$(panorama token --quiet)\ncodex",
		},
	}
}

// codexConfig is config.toml for one route.
//
// With search on, the gateway's MCP server is declared as well, reading the
// token from the variable the provider entry already names.
func codexConfig(route, base string, limits map[string]struct{ context, output int }, search bool) string {
	var b strings.Builder
	b.WriteString("# Panorama: the model here is a route name.\n")
	fmt.Fprintf(&b, "model = %q\n", route)
	fmt.Fprintf(&b, "model_provider = %q\n", codexProvider)
	if lim, ok := limits[route]; ok {
		fmt.Fprintf(&b, "model_context_window = %d\n", lim.context)
		fmt.Fprintf(&b, "model_max_output_tokens = %d\n", lim.output)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "[model_providers.%s]\n", codexProvider)
	b.WriteString("name = \"Panorama\"\n")
	fmt.Fprintf(&b, "base_url = %q\n", base+"/v1")
	b.WriteString("env_key = \"PANORAMA_TOKEN\"\n")
	b.WriteString("wire_api = \"responses\"\n")
	if search {
		b.WriteString(codexMCP(base))
	}
	return b.String()
}

// routeLimits reports what each route may tell a client about itself.
//
// Both numbers are for the route's model, and both are absent unless that model
// has both recorded - see policy.RouteMinimum. A figure a client is configured
// with once and managed against for months has to be one somebody stands
// behind.
func (a *API) routeLimits(ctx context.Context) map[string]struct{ context, output int } {
	out := map[string]struct{ context, output int }{}

	catalogue, err := a.db.ListCatalogue(ctx)
	if err != nil {
		// The route names are still right; only the limits beside them are
		// missing, and a missing limit leaves the client on its own defaults.
		a.log.WarnContext(ctx, "reading the catalogue for client limits", "error", err)
		return out
	}
	contexts := make(map[providers.ModelRef]int, len(catalogue))
	outputs := make(map[providers.ModelRef]int, len(catalogue))
	for _, m := range catalogue {
		if m.ContextWindow > 0 {
			contexts[m.Ref()] = m.ContextWindow
		}
		if m.MaxOutputTokens > 0 {
			outputs[m.Ref()] = m.MaxOutputTokens
		}
	}

	for _, route := range a.policy.Snapshot().Routes {
		window := policy.RouteMinimum(contexts, route)
		maxOut := policy.RouteMinimum(outputs, route)
		if window > 0 && maxOut > 0 {
			out[route.Name] = struct{ context, output int }{window, maxOut}
		}
	}
	return out
}

// settingsEnv is the env block of the settings file the CLI writes.
//
// The token is absent on purpose: apiKeyHelper fetches it, and a settings file
// that also carried one would be the long-lived credential this path exists to
// avoid.
func settingsEnv(base, primary, small string) map[string]string {
	return map[string]string{
		"ANTHROPIC_BASE_URL":            base,
		"ANTHROPIC_MODEL":               primary,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL": small,
	}
}

// routePlaceholder is what a configuration block carries when this deployment
// has no route the reader may use. It is deliberately not a model id.
const routePlaceholder = "your-route-name"

// tokenPlaceholder is what the configuration blocks carry where a credential
// would go.
// #nosec G101 -- a literal placeholder the console substitutes a real token
// for on the page. It is deliberately shaped like one so the copy-paste block
// reads correctly, and deliberately is not one.
const tokenPlaceholder = "pnt_YOUR_TOKEN"

// handleOnboarding answers "has it worked yet" for the person asking.
//
// It is the verification step of developer onboarding, and it reports what
// attribution actually detected rather than a tick: the point of the step is
// that somebody sees repo, branch, and ticket appear without having configured
// any of them.
func (a *API) handleOnboarding(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())

	tokens, err := a.db.ListGatewayTokens(r.Context(), p.UserID)
	if err != nil {
		a.fail(w, r, "reading onboarding state", err)
		return
	}
	live := 0
	for _, t := range tokens {
		if !t.Revoked {
			live++
		}
	}

	filter := store.RequestFilter{UserID: p.UserID, Limit: 1}
	page, err := a.db.ListRequests(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "reading onboarding state", err)
		return
	}

	body := map[string]any{
		"has_token":   live > 0,
		"has_request": len(page.Requests) > 0,
	}

	if len(page.Requests) > 0 {
		// One row, newest first: the latest request, which is what the console
		// labels it. It was called the first for a while, and was only ever that
		// for an account's first request.
		latest := page.Requests[0]
		detected := map[string]any{
			"tool": latest.Tool,
			// Both halves of the substitution this product exists to make: the
			// route the client asked for, and the model it became. One without
			// the other is either a name with nothing behind it or a model the
			// engineer never named.
			"route": latest.RequestedModel,
			"model": latest.ResolvedModel,
		}

		if latest.SessionID != "" {
			sessions, serr := a.db.ListSessions(r.Context(), store.RequestFilter{UserID: p.UserID, Limit: 1})
			if serr != nil {
				a.fail(w, r, "reading onboarding state", serr)
				return
			}
			if len(sessions) > 0 {
				s := sessions[0]
				detected["repo"] = s.Repo
				detected["branch"] = s.Branch
				detected["attributed"] = s.Attributed()
				// The sentence the whole product is for, served rather than
				// assembled by a screen: "PAY-1183 - branch name (structural,
				// high)".
				detected["describe"] = s.Describe()
				if s.Attributed() {
					detected["work_item"] = map[string]string{
						"key": s.WorkItemKey, "kind": s.WorkItemKind,
						"rung": s.Rung, "confidence": s.Confidence, "evidence": s.Evidence,
					}
				}
			}
		}
		body["detected"] = detected
		body["at"] = latest.StartedAt.UTC().Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, body)
}

// canConfigure reports whether this principal may change the deployment's own
// configuration.
func canConfigure(p *store.Principal) bool {
	return p != nil && (p.Role == store.RoleAdmin || p.IsSetup())
}

// actorFor names who did something, for a log line.
//
// A setup session has no user id because it is not a person, and an empty
// "actor" field reads as a bug rather than as the fact it is.
func actorFor(p *store.Principal) string {
	switch {
	case p == nil:
		return "unknown"
	case p.IsSetup():
		return "setup-session"
	default:
		return p.UserID
	}
}
