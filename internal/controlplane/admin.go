// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
)

// Settings an administrator edits from the console.
//
// Kept here as an allowlist rather than accepting whatever key a client sends.
// The settings table is read by the gateway as well as by the console, and a
// free-form write endpoint onto it is a way to reconfigure the deployment
// through a screen that was only meant to rename the organization.
const (
	settingOrgName       = "org.name"
	settingRetentionDays = "retention.request_days"
	settingCurrency      = "display.currency"
)

// editableSettings is the allowlist, with what each value may be.
var editableSettings = map[string]func(string) error{
	settingOrgName: func(v string) error {
		if len(v) > 120 {
			return errors.New("an organization name of more than 120 characters is not a name")
		}
		return nil
	},
	settingRetentionDays: func(v string) error {
		n, err := parsePositiveInt(v)
		if err != nil || n < 1 || n > 3650 {
			return errors.New("must be a whole number of days between 1 and 3650")
		}
		return nil
	},
	// The command-line sign-in switch. Off is always accepted; on is refused
	// until the build offers the flow, because a setting that reads "on" over
	// a feature the server refuses is two answers to one question and the
	// wrong one is on the screen. See cli_auth.go for the pair of switches.
	settingCLISignIn: func(v string) error {
		switch v {
		case "false":
			return nil
		case "true":
			if !cliSignInReleased {
				return fmt.Errorf("command-line sign-in is not available in this release: "+
					"engineers authenticate with a gateway token minted on My setup, which lives %d days",
					int(auth.GatewayTokenTTL.Hours()/24))
			}
			return nil
		default:
			return errors.New(`must be "true" or "false"`)
		}
	},
	settingCurrency: func(v string) error {
		if v != "USD" {
			// Stated rather than silently accepted. Panorama prices in dollars
			// because that is what the provider bills in; displaying another
			// currency would mean holding an exchange rate, and a cost report
			// that moves with the foreign exchange market is not a cost report.
			return errors.New("only USD is supported: rates are published in dollars and Panorama does not convert")
		}
		return nil
	},
}

// mountAdmin registers the routes that configure the deployment.
//
// Split from mountAPI so that the role requirement is visible in one list
// rather than repeated in each handler. Reads that a manager may make are
// separated from writes only an administrator may make, and neither is
// reachable without the wrapper.
func (s *Server) mountAdmin(api *API) {
	admin := s.requireRole(store.RoleAdmin)
	// An administrator, or a redeemed setup code. Deployment configuration is
	// the only thing a setup session may reach, because establishing or
	// repairing it is the only reason one exists. Naming RoleSetup is what
	// admits it; every other route in this map refuses it by default, including
	// any added later.
	setup := s.requireRole(store.RoleAdmin, store.RoleSetup)
	leaders := s.requireRole(store.RoleAdmin, store.RoleManager)
	anyone := s.authenticated

	for pattern, handler := range map[string]http.Handler{
		// Reading the deployment's own shape: what this account can invoke, and
		// which of it this deployment permits. Not org data, so everyone signed
		// in may read it - an engineer whose request was refused should be able
		// to see what they may ask for instead, and Budgets and Integrations
		// both render route names to anybody who opens them.
		//
		// Not a setup session, though: the provider step reports what discovery
		// found from /api/v1/providers, and there is nothing about the
		// allowlist a one-time code needs to see.
		"GET /api/v1/models": anyone(http.HandlerFunc(api.handleModels)),

		"GET /api/v1/integrations": anyone(http.HandlerFunc(api.handleIntegrations)),
		"GET /api/v1/outcomes":     anyone(http.HandlerFunc(api.handleOutcomes)),
		// Seat economics and cache hygiene name nobody, so anyone signed in may
		// read them. The per-user table in the same response is gated inside
		// the handler by spendVisibility, which is where that decision lives.
		"GET /api/v1/seats": anyone(http.HandlerFunc(api.handleSeats)),

		"GET /api/v1/tokens":         anyone(http.HandlerFunc(api.handleListTokens)),
		"POST /api/v1/tokens":        anyone(http.HandlerFunc(api.handleCreateToken)),
		"DELETE /api/v1/tokens/{id}": anyone(http.HandlerFunc(api.handleRevokeToken)),

		// Approving a waiting `panorama login`. Anyone signed in may approve a
		// sign-in for themselves - it grants their own access and nobody
		// else's - and the write carries CSRF like every other, which is what
		// stops a page somebody was tricked into loading from approving one.
		//
		// Both are also behind the command-line sign-in switch, which this
		// release answers no to: with no flow to open there is nothing waiting
		// to approve. Authenticated first, then the switch, so a caller with no
		// credential is told that rather than which features this deployment
		// runs.
		//
		// Listing and revoking are not behind it. A grant already on a laptop
		// has to stay revocable whatever the switch says, and the list is how
		// an operator finds the one to revoke.
		"GET /api/v1/cli/pending":    anyone(s.requireCLISignIn(http.HandlerFunc(api.handlePendingCLI))),
		"POST /api/v1/cli/decide":    anyone(s.requireCLISignIn(http.HandlerFunc(api.handleDecideCLI))),
		"GET /api/v1/grants":         anyone(http.HandlerFunc(api.handleListGrants)),
		"DELETE /api/v1/grants/{id}": anyone(http.HandlerFunc(api.handleRevokeGrant)),

		// Credentials a pipeline holds. Reading is for leaders, because the
		// question is which pipelines can spend a team's budget; minting is for
		// administrators, because it is the one place somebody creates a
		// credential they will not be the principal of.
		"GET /api/v1/service-tokens":         leaders(http.HandlerFunc(api.handleListServiceTokens)),
		"POST /api/v1/service-tokens":        admin(http.HandlerFunc(api.handleCreateServiceToken)),
		"DELETE /api/v1/service-tokens/{id}": admin(http.HandlerFunc(api.handleRevokeServiceToken)),
		"GET /api/v1/setup":                  setup(http.HandlerFunc(api.handleSetupState)),
		"GET /api/v1/setup/tools":            anyone(http.HandlerFunc(api.handleToolSetup)),
		"GET /api/v1/setup/onboarding":       anyone(http.HandlerFunc(api.handleOnboarding)),
		"GET /api/v1/teams":                  anyone(http.HandlerFunc(api.handleListTeams)),
		"GET /api/v1/teams/{slug}":           leaders(http.HandlerFunc(api.handleTeam)),
		"GET /api/v1/users":                  leaders(http.HandlerFunc(api.handleListUsers)),
		"GET /api/v1/settings":               leaders(http.HandlerFunc(api.handleSettings)),
		"GET /api/v1/identity":               setup(http.HandlerFunc(api.handleIdentity)),
		"PATCH /api/v1/settings":             admin(http.HandlerFunc(api.handleUpdateSettings)),
		"PUT /api/v1/identity/groups":        setup(http.HandlerFunc(api.handleSetGroupRoles)),
		"POST /api/v1/identity/check":        setup(http.HandlerFunc(api.handleCheckIdentity)),
		// Preflight reaches a URL the operator supplied, and doctor reports how
		// the deployment's own SSO is configured. An administrator or a setup
		// session, and nothing weaker.
		"GET /api/v1/identity/presets":    setup(http.HandlerFunc(api.handleSSOPresets)),
		"POST /api/v1/identity/preflight": setup(http.HandlerFunc(api.handleSSOPreflight)),
		"POST /api/v1/identity/doctor":    setup(http.HandlerFunc(api.handleSSODoctor)),
		"PUT /api/v1/budgets":             admin(http.HandlerFunc(api.handleSaveBudget)),
		// Reading the chart of accounts is a leader's; writing it is an
		// administrator's. A developer is served none of it - their own
		// numbers live on the personal screens, and a cost center is somebody
		// else's line by definition.
		"GET /api/v1/cost-centers":               leaders(http.HandlerFunc(api.handleCostCenters)),
		"GET /api/v1/allocation":                 leaders(http.HandlerFunc(api.handleAllocation)),
		"POST /api/v1/cost-centers":              admin(http.HandlerFunc(api.handleSaveCostCenter)),
		"PATCH /api/v1/cost-centers/{id}":        admin(http.HandlerFunc(api.handleSaveCostCenter)),
		"DELETE /api/v1/cost-centers/{id}":       admin(http.HandlerFunc(api.handleDeleteCostCenter)),
		"POST /api/v1/cost-centers/rules":        admin(http.HandlerFunc(api.handleSaveCostCenterRule)),
		"DELETE /api/v1/cost-centers/rules/{id}": admin(http.HandlerFunc(api.handleDeleteCostCenterRule)),
		"POST /api/v1/cost-centers/order":        admin(http.HandlerFunc(api.handleOrderCostCenterRules)),
		"POST /api/v1/cost-centers/reallocate":   admin(http.HandlerFunc(api.handleReallocate)),
		"POST /api/v1/cost-centers/test":         admin(http.HandlerFunc(api.handleTestAllocation)),

		"DELETE /api/v1/budgets/{id}": admin(http.HandlerFunc(api.handleDeleteBudget)),
		"PATCH /api/v1/users/{id}":    admin(http.HandlerFunc(api.handleUpdateUser)),
		"POST /api/v1/teams":          admin(http.HandlerFunc(api.handleCreateTeam)),
		"POST /api/v1/teams/{slug}":   admin(http.HandlerFunc(api.handleTeamMembers)),
		"DELETE /api/v1/teams/{slug}": admin(http.HandlerFunc(api.handleDeleteTeam)),
		"POST /api/v1/setup/step":     setup(http.HandlerFunc(api.handleSetupStep)),

		// How this deployment reaches its model providers. An administrator or a
		// setup session: a region and a switch are deployment configuration, the
		// same class of thing as the identity connection, and establishing them
		// is what a setup session exists for. Which models engineers may use is
		// not here - that is a route, and an administrator's alone.
		//
		// The check spends money - one token - and preflight reaches AWS, so
		// neither is open to anybody weaker. Both writes carry CSRF like every
		// other.
		"GET /api/v1/providers":                   setup(http.HandlerFunc(api.handleProviders)),
		"PUT /api/v1/providers/{kind}":            setup(http.HandlerFunc(api.handleSaveProvider)),
		"POST /api/v1/providers/{kind}/preflight": setup(http.HandlerFunc(api.handlePreflightProvider)),
		"POST /api/v1/providers/{kind}/check":     setup(http.HandlerFunc(api.handleCheckProvider)),
		// Who administers the deployment. A setup session may change this only
		// because "the first sign-in owns it" can seat the wrong person, and
		// because it could already grant admin by mapping a group to it - this
		// is the same power by a more direct route, not a wider one.
		"GET /api/v1/setup/administrators":   setup(http.HandlerFunc(api.handleSetupAdministrators)),
		"PATCH /api/v1/setup/administrators": setup(http.HandlerFunc(api.handleSetSetupAdministrator)),
		"POST /api/v1/integrations/run":      admin(http.HandlerFunc(api.handleCheckIntegrations)),
		// Connecting decides what this deployment reaches out to and with whose
		// access - the same class of decision as editing the model allowlist,
		// not the same class as reading a screen.
		"PUT /api/v1/integrations/{kind}":    admin(http.HandlerFunc(api.handleConnectIntegration)),
		"DELETE /api/v1/integrations/{kind}": admin(http.HandlerFunc(api.handleDisconnectIntegration)),

		// Asking the providers what this account can invoke. A setup session
		// may, because the answer is what its live check picks a model from and
		// because the IAM permissions it needs are exactly what that step exists
		// to prove.
		"POST /api/v1/models/discover": setup(http.HandlerFunc(api.handleDiscoverModels)),

		// Which models this deployment permits, and where their traffic is
		// processed. An administrator, and not a setup session: this is an
		// operational decision made months after a deployment is stood up -
		// whose spend it changes, whose prompts leave which country - and
		// somebody holding a one-time code has no standing to make it. Setup
		// establishes that a provider can be reached; this decides what may be
		// asked of it.
		"PUT /api/v1/routes": admin(http.HandlerFunc(api.handleSaveRoute)),

		// Web search, offered to every coding tool over MCP. An
		// administrator's alone, and not a setup session's, for the reason
		// routes are: a search sends text a model wrote from somebody's
		// conversation to a service, which is a decision about where
		// prompt-derived content goes. The check lists the gateway's tools and
		// runs no search, so it spends nothing.
		"GET /api/v1/web-search":        admin(http.HandlerFunc(api.handleWebSearch)),
		"PUT /api/v1/web-search":        admin(http.HandlerFunc(api.handleSaveWebSearch)),
		"POST /api/v1/web-search/check": admin(http.HandlerFunc(api.handleCheckWebSearch)),
		"DELETE /api/v1/routes/{name}":  admin(http.HandlerFunc(api.handleDeleteRoute)),
	} {
		s.mux.Handle(pattern, handler)
	}
}

// handleIntegrations serves the state of the connected systems of record.
func (a *API) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	states, err := a.db.ListIntegrations(r.Context())
	if err != nil {
		a.fail(w, r, "reading integration state", err)
		return
	}

	byKind := make(map[string]store.IntegrationState, len(states))
	for _, s := range states {
		byKind[s.Kind] = s
	}

	configs, err := a.db.IntegrationConfigs(r.Context())
	if err != nil {
		a.fail(w, r, "reading integration configuration", err)
		return
	}
	byConfig := make(map[string]store.IntegrationConfig, len(configs))
	for _, c := range configs {
		byConfig[c.Kind] = c
	}

	// Both are always listed, connected or not, so the screen shows what is
	// available rather than only what is on. What is lost without each is
	// stated here, in one place, rather than written into a screen.
	out := make([]map[string]any, 0, 2)
	for _, kind := range []string{"github", "jira"} {
		row := map[string]any{
			"kind":      kind,
			"connected": false,
			"unlocks":   integrationUnlocks(kind),
			"degraded":  "Attribution keeps working from structural inference. Detections stay unconfirmed and outcome metrics are unavailable.",
		}

		if cfg, ok := byConfig[kind]; ok {
			// The connection as it was set up, so the form re-opens with what
			// is there. Never the credential - only whether one is stored.
			row["provider"] = cfg.Provider
			row["app_id"] = cfg.AppID
			row["installation_id"] = cfg.InstallationID
			row["api_base_url"] = cfg.APIBaseURL
			row["fixtures"] = cfg.Fixtures
			row["has_secret"] = cfg.HasSecret()
			row["secret_kind"] = cfg.SecretKind
			addTime(row, "configured_at", cfg.ConfiguredAt)
		}

		if state, ok := byKind[kind]; ok {
			row["connected"] = true
			row["provider"] = state.Provider
			row["detail"] = state.Detail
			row["status"] = state.Status
			// Surfaced whether or not it has cleared. An integration that fails
			// intermittently is the one an operator has to chase, and a screen
			// showing only the current state hides it.
			row["last_error"] = state.LastError
			row["last_checked_items"] = state.LastCheckedItems
			row["last_confirmed_items"] = state.LastConfirmedItems
			// Said in the payload rather than inferred from the provider string
			// by each client. A number that came out of a fixtures file must
			// never look like one that came out of GitHub.
			row["is_mock"] = state.Provider == "mock"
			addTime(row, "last_checked_at", state.LastCheckedAt)
			addTime(row, "last_synced_at", state.LastSyncedAt)
			addTime(row, "last_error_at", state.LastErrorAt)
		}
		out = append(out, row)
	}

	connected, err := a.db.AnyIntegrationConfigured(r.Context())
	if err != nil {
		a.fail(w, r, "counting integrations", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"integrations":  out,
		"any_connected": connected,
		// Whether a credential can be stored at all. The connect form is
		// useless without one, and an administrator should learn that from the
		// screen rather than from a failed save.
		"can_store_secrets": a.secrets.Configured(),
	})
}

func integrationUnlocks(kind string) string {
	switch kind {
	case "github":
		return "Pull request titles, authors, and merge status - and with them, cost per merged pull request and spend on merged versus abandoned work."
	default:
		return "Ticket titles, projects, and resolution status, and spend rolled up by project."
	}
}

// handleCheckIntegrations runs a health check against everything connected, so
// an administrator does not have to wait for the next sync to learn a
// credential has expired.
func (a *API) handleCheckIntegrations(w http.ResponseWriter, r *http.Request) {
	registry, err := a.integrations.Registry(r.Context())
	if err != nil {
		// Not swallowed into "nothing connected". A credential that will not
		// decrypt is the answer an administrator is here for.
		a.fail(w, r, "resolving integrations", err)
		return
	}
	if !registry.Configured() {
		writeAPIError(w, r, http.StatusBadRequest, "No integrations are connected on this deployment.")
		return
	}

	results := make([]map[string]any, 0, 2)
	for _, source := range registry.Sources() {
		err := source.Check(r.Context())
		if rerr := a.db.RecordIntegrationCheck(r.Context(), string(source.Kind()), err); rerr != nil {
			a.log.ErrorContext(r.Context(), "recording integration check", "error", rerr)
		}

		row := map[string]any{
			"kind":     string(source.Kind()),
			"provider": source.Provider(),
			"detail":   source.Describe(),
			"ok":       err == nil,
		}
		if err != nil {
			row["error"] = err.Error()
		}
		results = append(results, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleOutcomes serves what the money bought.
//
// Every response says whether anything has actually been confirmed, because the
// difference between "nothing merged" and "nothing was asked" is the whole
// difference between a finding and a missing integration.
func (a *API) handleOutcomes(w http.ResponseWriter, r *http.Request) {
	filter, err := a.filterFrom(r)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	summary, err := a.db.Outcomes(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "rolling up outcomes", err)
		return
	}
	projects, err := a.db.SummarizeProjects(r.Context(), filter)
	if err != nil {
		a.fail(w, r, "rolling up projects", err)
		return
	}

	buckets := make([]map[string]any, 0, len(summary.ByOutcome))
	for _, b := range summary.ByOutcome {
		row := renderTotals(b.Totals)
		row["outcome"] = b.Outcome
		row["work_items"] = b.WorkItems
		row["label"] = outcomeLabel(b.Outcome)
		buckets = append(buckets, row)
	}

	connected, cerr := a.db.AnyIntegrationConfigured(r.Context())
	if cerr != nil {
		a.fail(w, r, "counting integrations", cerr)
		return
	}

	body := map[string]any{
		"since":       filter.Since.Format(time.RFC3339),
		"until":       filter.Until.Format(time.RFC3339),
		"scope":       scopeLabel(filter.UserID),
		"by_outcome":  buckets,
		"by_project":  renderWorkBuckets(projects),
		"confirmed":   summary.Confirmed,
		"integration": connected,
	}

	if summary.LandedPullRequests > 0 {
		perPR := int64(summary.LandedPullRequestCost) / summary.LandedPullRequests
		body["cost_per_merged_pr"] = map[string]any{
			"merged_pull_requests": summary.LandedPullRequests,
			"total_nanos":          int64(summary.LandedPullRequestCost),
			"total_usd":            summary.LandedPullRequestCost.USD(),
			"per_pr_nanos":         perPR,
			"per_pr_usd":           pricing.Money(perPR).USD(),
			// The definition travels with the number, because more than one is
			// defensible and a headline stat nobody can reproduce is worse than
			// no headline stat.
			"definition": "Spend recorded in this window against pull requests that have merged, divided by the number of such pull requests. A pull request merged inside the window carries only the spend the window holds.",
		}
	}

	writeJSON(w, http.StatusOK, body)
}

func outcomeLabel(outcome string) string {
	switch outcome {
	case "landed":
		return "Merged or resolved"
	case "abandoned":
		return "Closed without landing"
	case "open":
		return "Still in flight"
	default:
		// Its own row, always. Work nothing has confirmed is not work that is
		// still open, and folding the two together would overstate the second.
		return "Not confirmed"
	}
}

// handleSettings serves the deployment's editable settings, with what each one
// currently is.
func (a *API) handleSettings(w http.ResponseWriter, r *http.Request) {
	stored, err := a.db.Settings(r.Context())
	if err != nil {
		a.fail(w, r, "reading settings", err)
		return
	}

	// Only the allowlist crosses the wire. The settings table also holds the
	// setup code's hash, and a screen that dumped every row would put it on
	// one.
	values := map[string]string{}
	for key := range editableSettings {
		values[key] = stored[key]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"settings": values,
		"defaults": map[string]string{
			settingOrgName:       "",
			settingRetentionDays: "400",
			settingCurrency:      "USD",
			settingCLISignIn:     "false",
		},
		// Configuration the console shows but cannot change, because it comes
		// from the file and the environment where secrets live.
		"deployment": map[string]any{
			"base_url":       a.server.cfg.Server.BaseURL,
			"providers":      a.providerSummaries(),
			"driver":         a.server.cfg.Database.Driver,
			"sso_configured": a.server.cfg.OIDC.Configured(),
			// Whether this build offers command-line sign-in at all, which is
			// what decides whether the switch beside it can be moved. Served
			// rather than compiled into the console, so there is one answer
			// and the screen is not the second place it is decided.
			"cli_sign_in_released": cliSignInReleased,
		},
	})
}

// handleUpdateSettings writes settings from the allowlist above.
func (a *API) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if !decodeBody(w, r, &req) {
		return
	}

	var problems []string
	for key, value := range req {
		validate, ok := editableSettings[key]
		if !ok {
			problems = append(problems, key+" is not a setting this console edits")
			continue
		}
		if err := validate(value); err != nil {
			problems = append(problems, key+": "+err.Error())
		}
	}
	// Every problem at once, and nothing written until they are all gone: a
	// half-applied settings save is a deployment in a state nobody asked for.
	if len(problems) > 0 {
		writeAPIError(w, r, http.StatusBadRequest, strings.Join(problems, "; "))
		return
	}

	for key, value := range req {
		if err := a.db.SetSetting(r.Context(), key, value); err != nil {
			a.fail(w, r, "writing settings", err)
			return
		}
	}

	a.log.InfoContext(r.Context(), "settings changed",
		"keys", len(req), "actor", PrincipalFrom(r.Context()).UserID)
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(req)})
}

// handleIdentity serves the SSO configuration and the group mapping.
func (a *API) handleIdentity(w http.ResponseWriter, r *http.Request) {
	mapping, err := a.db.GroupRoles(r.Context())
	if err != nil {
		a.fail(w, r, "reading group mapping", err)
		return
	}
	store.SortGroupRoles(mapping)

	defaultRole := store.RoleDeveloper
	switch v, serr := a.db.Setting(r.Context(), settingDefaultRole); {
	case serr == nil:
		defaultRole = v
	case !errors.Is(serr, store.ErrNotFound):
		a.fail(w, r, "reading the default role", serr)
		return
	}

	groups := make([]map[string]string, 0, len(mapping))
	for _, m := range mapping {
		groups = append(groups, map[string]string{"group": m.Group, "role": m.Role})
	}

	// What the provider has actually sent. The mapping above asks for group
	// names and nothing else here could tell an administrator what theirs are
	// called - providers disagree about whether that is a name, a path, or a
	// GUID, and a wrong guess grants nothing rather than failing.
	observed, err := a.db.ObservedGroups(r.Context())
	if err != nil {
		a.fail(w, r, "reading observed groups", err)
		return
	}
	seen := make([]map[string]any, 0, len(observed))
	for _, g := range observed {
		seen = append(seen, map[string]any{
			"group":      g.Group,
			"count":      g.Count,
			"last_seen":  g.LastSeen,
			"first_seen": g.FirstSeen,
		})
	}

	counters, err := a.db.SignInCounters(r.Context())
	if err != nil {
		a.fail(w, r, "reading sign-in counters", err)
		return
	}

	cfg := a.server.cfg.OIDC
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":   cfg.Configured(),
		"issuer":       cfg.Issuer,
		"client_id":    cfg.ClientID,
		"scopes":       cfg.Scopes,
		"groups_claim": cfg.GroupsClaim,
		"redirect_url": a.server.identity.redirectURL,
		// The connection is configuration and the mapping is org data, and the
		// split is stated so nobody goes looking for a field to edit the issuer
		// with. A client secret in a database is a client secret in a backup.
		"connection_is_configuration": true,
		"groups":                      groups,
		// What this mapping looks like right now, sent back on save so a screen
		// that has gone stale is refused rather than allowed to delete rows it
		// never saw.
		"mapping_version":         mappingVersion(mapping, defaultRole),
		"observed_groups":         seen,
		"sign_ins":                counters[store.CounterSignIns],
		"sign_ins_without_groups": counters[store.CounterSignInsWithoutGroups],
		"default_role":            defaultRole,
		"default_role_options":    []string{store.RoleDeveloper, store.RoleManager, store.RoleAdmin, roleNone},
	})
}

// groupRolesRequest replaces the whole mapping.
type groupRolesRequest struct {
	Groups []struct {
		Group string `json:"group"`
		Role  string `json:"role"`
	} `json:"groups"`
	DefaultRole string `json:"default_role"`
	// MappingVersion is what the mapping looked like when the screen that is
	// saving loaded it. See mappingVersion.
	MappingVersion string `json:"mapping_version"`
}

// mappingVersion fingerprints the mapping and the default role together.
//
// Whole-document replacement is what keeps a privilege change visible to the
// person making it - but it also means a save carries an opinion about every
// row, including the ones the screen never saw. A console tab left open while
// somebody else maps a group will delete that group on its next save, silently,
// because as far as it knows the mapping is what it loaded. That is a demotion
// nobody performed and nothing reports.
//
// So a save says which version it is editing, and a save against a version that
// has moved is refused rather than applied. The alternative - merging - would
// mean guessing which of two intentions about the same row is current, on the
// document that decides who administers the deployment.
//
// Not a timestamp: two changes within the same clock tick would collide, and
// the question is only ever "is this the same mapping", not "which is newer".
func mappingVersion(mapping []store.GroupRole, defaultRole string) string {
	lines := make([]string, 0, len(mapping))
	for _, m := range mapping {
		lines = append(lines, m.Group+"="+m.Role)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\ndefault=" + defaultRole))
	return hex.EncodeToString(sum[:8])
}

// handleSetGroupRoles replaces the group mapping and the default role.
//
// Whole-document rather than per-row, because the mapping is what grants
// privilege and an administrator editing it should be looking at all of it. A
// PATCH that removed one row would be a way to demote someone without the
// screen showing the result.
func (a *API) handleSetGroupRoles(w http.ResponseWriter, r *http.Request) {
	var req groupRolesRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if req.DefaultRole != "" && req.DefaultRole != roleNone && !store.ValidRole(req.DefaultRole) {
		writeAPIError(w, r, http.StatusBadRequest,
			`default_role must be a role, or "none" to require an explicit mapping.`)
		return
	}
	// One group, one role.
	//
	// ResolveRole lowercases both sides, so two mappings differing only in case
	// are one group with two answers, and the one that wins depends on the
	// order rows come back in. The console already refuses the pair, which is
	// exactly why the server has to: a check that lives only in the browser is
	// not a check, and this document is written by scripts holding a gateway
	// token as well as by a screen.
	byName := map[string]string{}
	for _, g := range req.Groups {
		name := strings.TrimSpace(g.Group)
		if name == "" || !store.ValidRole(g.Role) {
			writeAPIError(w, r, http.StatusBadRequest,
				"Every mapping needs a group name and one of the roles admin, manager, or developer.")
			return
		}
		key := strings.ToLower(name)
		if first, clash := byName[key]; clash {
			writeAPIError(w, r, http.StatusBadRequest, fmt.Sprintf(
				"%q and %q are one group to the role mapping, which matches without regard to case. Map it once.",
				first, name))
			return
		}
		byName[key] = name
	}

	// A strict allowlist that admits nobody locks everyone out, including the
	// administrator making the change.
	//
	// Counting rows would catch only the empty mapping and let through the
	// commoner mistake: mapping a group nobody is actually in, which grants
	// nothing and looks configured. Sign-ins record the groups they carried, so
	// the question can be asked properly - is there any
	// mapped group this deployment has ever seen? - and a deployment where no
	// SSO sign-in has happened yet is still answered by the row count, since
	// there is nothing better to go on.
	//
	// It refuses rather than warns, and the mistake is not a trap: `panorama admin
	// recover` opens a session that can undo exactly this, so the mistake is
	// survivable and the message says so.
	if req.DefaultRole == roleNone {
		locked, why, lerr := a.wouldLockEveryoneOut(r, req)
		if lerr != nil {
			a.fail(w, r, "checking the mapping against what has signed in", lerr)
			return
		}
		if locked {
			writeAPIError(w, r, http.StatusBadRequest, why+
				" Map a group somebody is in first - the ones your provider has actually sent are listed on this screen - or keep a default role.")
			return
		}
	}

	existing, err := a.db.GroupRoles(r.Context())
	if err != nil {
		a.fail(w, r, "reading group mapping", err)
		return
	}

	// Refused, not merged. A stale save deletes every row added since the
	// screen loaded it, and on this document that is a silent demotion.
	//
	// Enforced only when the caller says which version it edited: a script
	// holding a gateway token and writing the mapping outright is stating that
	// it is the authority on all of it, which the console never is.
	if req.MappingVersion != "" {
		currentDefault := store.RoleDeveloper
		switch v, serr := a.db.Setting(r.Context(), settingDefaultRole); {
		case serr == nil:
			currentDefault = v
		case !errors.Is(serr, store.ErrNotFound):
			a.fail(w, r, "reading the default role", serr)
			return
		}
		if got := mappingVersion(existing, currentDefault); got != req.MappingVersion {
			writeAPIError(w, r, http.StatusConflict,
				"This mapping changed after this screen loaded it, so saving would undo that change. Reload and make your edit again.")
			return
		}
	}

	wanted := map[string]bool{}
	for _, g := range req.Groups {
		wanted[g.Group] = true
		if serr := a.db.SetGroupRole(r.Context(), g.Group, g.Role); serr != nil {
			a.fail(w, r, "writing group mapping", serr)
			return
		}
	}
	for _, g := range existing {
		if !wanted[g.Group] {
			if _, derr := a.db.DeleteGroupRole(r.Context(), g.Group); derr != nil {
				a.fail(w, r, "removing a group mapping", derr)
				return
			}
		}
	}

	if req.DefaultRole != "" {
		if serr := a.db.SetSetting(r.Context(), settingDefaultRole, req.DefaultRole); serr != nil {
			a.fail(w, r, "writing the default role", serr)
			return
		}
	}

	a.log.InfoContext(r.Context(), "group mapping changed",
		"groups", len(req.Groups), "default_role", req.DefaultRole,
		"actor", PrincipalFrom(r.Context()).UserID)

	writeJSON(w, http.StatusOK, map[string]any{"groups": len(req.Groups)})
}

// handleCheckIdentity verifies that the configured issuer is reachable.
func (a *API) handleCheckIdentity(w http.ResponseWriter, r *http.Request) {
	if err := a.server.identity.check(r.Context()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
			// Named because it is the fix nine times in ten, and because the
			// error from a discovery failure rarely says it.
			"hint": "Check oidc.issuer, that this deployment can reach it, and that " + a.server.identity.redirectURL + " is registered as a redirect URI.",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		// Discovery is all this can prove. Exchanging a code needs a person at
		// a browser, so the wizard's identity step is not done until somebody
		// actually signs in.
		"verified": "discovery",
		"note":     "The issuer is reachable and publishes the endpoints Panorama uses. Sign in once to prove the rest.",
	})
}

func addTime(row map[string]any, key string, at *time.Time) {
	if at != nil {
		row[key] = at.UTC().Format(time.RFC3339)
	}
}
