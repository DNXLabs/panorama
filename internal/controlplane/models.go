// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/jobs"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/tools"
)

// handleModels serves everything the routing screen renders.
//
// Three facts about the same model, joined here once rather than in whichever
// client happens to need them: what the provider offers, what this deployment
// permits, and what each costs. A screen that fetched them separately would
// render a route pointing at a model the catalogue no longer lists as though
// nothing were wrong.
//
// # What a developer gets
//
// A route's conditions name people. A developer is served the routes they can
// actually use and nothing about who else can use them - not the conditions,
// not the names in them, not the routes that exclude them. That is the same
// filter the gateway applies when it refuses a name: a route somebody may not
// use is not one they can see.
func (a *API) handleModels(w http.ResponseWriter, r *http.Request) {
	catalogue, err := a.db.ListCatalogue(r.Context())
	if err != nil {
		a.fail(w, r, "reading the model catalogue", err)
		return
	}
	routes, err := a.db.ListRoutes(r.Context())
	if err != nil {
		a.fail(w, r, "reading routes", err)
		return
	}
	fetched, err := a.db.CatalogueFetchedAt(r.Context())
	if err != nil {
		a.fail(w, r, "reading the model catalogue", err)
		return
	}

	p := PrincipalFrom(r.Context())
	privileged := seesEveryone(p.Role)

	byRef := make(map[providers.ModelRef]store.CatalogueModel, len(catalogue))
	for _, m := range catalogue {
		byRef[m.Ref()] = m
	}

	models := make([]map[string]any, 0, len(catalogue))
	for _, m := range catalogue {
		models = append(models, a.describeModel(m))
	}

	// The set the caller is measured against when a route names people or
	// teams. Built once here rather than per route.
	set := a.policy.Snapshot()
	in := policy.Input{UserID: p.UserID, TeamID: p.TeamID, Tool: ""}
	teams := set.Teams[p.UserID]
	if p.TeamID != "" {
		teams = []string{p.TeamID}
	}

	out := make([]map[string]any, 0, len(routes))
	for _, rt := range routes {
		// A tool condition is not applied here: the console is not a coding
		// tool, so measuring it against one would hide a route from the person
		// who uses it everywhere except in a browser.
		if !privileged && !availableIgnoringTool(rt.Route, in, teams) {
			continue
		}
		out = append(out, a.describeRoute(rt, byRef, set, privileged))
	}

	discoveredAt := map[string]string{}
	var latest time.Time
	for kind, at := range fetched {
		discoveredAt[string(kind)] = at.UTC().Format(time.RFC3339)
		if at.After(latest) {
			latest = at
		}
	}

	body := map[string]any{
		"routes": out,
		"models": models,
		// Which providers exist and where each is invoked, so the catalogue can
		// be filtered by provider and a row can say where its requests go.
		"providers":     a.providerSummaries(),
		"discovered":    len(fetched) > 0,
		"discovered_by": discoveredAt,
		// Whether this caller may change routes. An administrator, and nobody
		// else: a setup session configures how to reach Bedrock, not which
		// models a deployment permits, and everyone else is reading.
		"can_edit": p.Role == store.RoleAdmin,
		// The tools a condition may name. An allowlist, and the same list the
		// data plane derives from a user agent - a condition naming anything
		// else would match no request ever and look like a route that had
		// quietly stopped working.
		"tools": tools.Known(),
	}
	if len(fetched) > 0 {
		body["discovered_at"] = latest.UTC().Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, body)
}

// availableIgnoringTool answers the identity half of a route's conditions.
//
// The console asks it because a browser is not a coding tool: applying the tool
// condition here would hide a Claude-Code-only route from the very person whose
// Claude Code uses it. The gateway applies all three, which is the only place
// the answer decides anything.
func availableIgnoringTool(route policy.Route, in policy.Input, teams []string) bool {
	route.Tools = nil
	return route.Available(in, teams)
}

// describeRoute renders one route, with the model it becomes.
func (a *API) describeRoute(
	rt store.Route, byRef map[providers.ModelRef]store.CatalogueModel, set policy.Set, privileged bool,
) map[string]any {
	apis := set.RouteAPIs(rt.Route)
	if apis == nil {
		apis = []providers.API{}
	}
	out := map[string]any{
		"name":        rt.Name,
		"description": rt.Description,
		// The reference, provider first, which is what the route stores and
		// what a policy file writes.
		"target":         rt.Target.String(),
		"provider":       rt.Target.Provider,
		"provider_label": a.providerLabel(rt.Target.Provider),
		// The APIs this route can be served on, from the same derivation the
		// gateway refuses a request on the wrong surface with.
		"apis": apis,
		// Whether this route is still served past a soft limit. A developer is
		// served it as well as an administrator: it is the difference between a
		// route they can rely on for the rest of the month and one they will be
		// asked to switch off, and knowing which is which before it happens is
		// the point of saying it at all.
		"serves_over_soft": rt.ServesOverSoft,
		// A route with no model cannot serve a request it admits. The write
		// path will not create one, so this is a state to surface loudly rather
		// than a shape to render blankly.
		"complete": !rt.Target.IsZero(),
	}

	if m, ok := byRef[rt.Target]; ok {
		out["model"] = a.describeModel(m)
	} else if !rt.Target.IsZero() {
		// The catalogue is a cache and the provider is the authority, so this
		// still works - but an operator should know the deployment is pointing
		// somewhere discovery has not seen.
		out["unrecognised"] = true
	}

	if privileged {
		out["users"] = nonNilList(rt.Users)
		out["teams"] = nonNilList(rt.Teams)
		out["tools"] = nonNilList(rt.Tools)
		out["restricted"] = rt.Restricted()
		out["enabled_by"] = rt.EnabledBy
		out["updated_at"] = rt.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// nonNilList keeps an absent condition rendering as [] rather than null, so the
// console has one shape to read instead of two.
func nonNilList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// describeModel renders one catalogue row with its price.
func (a *API) describeModel(m store.CatalogueModel) map[string]any {
	apis := m.APIs
	if apis == nil {
		apis = []providers.API{}
	}
	row := map[string]any{
		// The reference a route stores, and the key a picker returns.
		"ref":            m.Ref().String(),
		"id":             m.ID,
		"model":          m.FoundationModelID,
		"name":           m.DisplayName,
		"vendor":         m.Vendor,
		"provider":       m.Provider,
		"provider_label": a.providerLabel(m.Provider),
		"region":         m.Region,
		"apis":           apis,
		"geography":      m.Geography,
		"global":         m.Global(),
		"regions":        m.Regions,
		"streaming":      m.Streaming,
		"lifecycle":      m.Lifecycle,
		"legacy":         m.Legacy(),
		"requires_prof":  m.RequiresProfile,
		// Where a request to this target may be processed, in the words a
		// compliance reviewer needs rather than a prefix they would look up.
		"residency": m.Residency(),
		// A listed model the provider says cannot be invoked, and why - kept in
		// the list so "why is it missing" has an answer here.
		"available": m.Available(),
	}
	if m.Status != "" {
		row["status"] = m.Status
		row["status_reason"] = m.StatusReason
	}
	if len(m.Retention) > 0 {
		row["retention"] = m.Retention
	}

	// Omitted rather than sent as zero when this build has no entry for the
	// model. The console renders nothing for an absent key and "1M" for a
	// number, which are the two honest states; a zero would render as a model
	// that takes no context.
	if m.ContextWindow > 0 {
		row["context_window"] = m.ContextWindow
	}
	if m.MaxOutputTokens > 0 {
		row["max_output"] = m.MaxOutputTokens
	}

	if entry, ok := a.prices.Lookup(string(m.Provider), m.ID); ok {
		row["rates"] = map[string]string{
			"input":          entry.Rates.Input.USDPerMillion(),
			"output":         entry.Rates.Output.USDPerMillion(),
			"cache_read":     entry.Rates.CacheRead.USDPerMillion(),
			"cache_write":    entry.Rates.CacheWrite.USDPerMillion(),
			"cache_write_1h": entry.Rates.CacheWrite1h.USDPerMillion(),
		}
		row["rate_source"] = entry.Source
		row["priced"] = true
	} else {
		// Named rather than left as an absent key. A model with no rate records
		// its requests unpriced, and the screen where somebody permits a model
		// is where they should learn that - not the cost report a week later.
		row["priced"] = false
	}
	return row
}

// routeRequest is a route as the console sends it: the whole route, every time.
//
// A whole route rather than a patch, because its parts are one decision. A
// condition saved without the model it was added for would be a route serving
// the wrong model to a narrower audience, briefly, and nobody would know which
// half had landed.
type routeRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	// Conditions. An empty list is unrestricted, which is what a route is
	// created with.
	Users []string `json:"users"`
	Teams []string `json:"teams"`
	Tools []string `json:"tools"`

	// Target is the model this route sends traffic to, as provider/model.
	Target string `json:"target"`

	// ServesOverSoft says whether this route is still served past a soft limit.
	//
	// A pointer so that a client which does not send the field gets the default
	// rather than false. A missing boolean decoding to false would quietly turn
	// every route it touched into one that stops at the soft limit - the exact
	// failure this flag exists to make deliberate.
	ServesOverSoft *bool `json:"serves_over_soft"`
}

// handleSaveRoute creates a route or changes one.
//
// Repointing is how an administrator moves traffic between models and between
// geographies, which is a data-residency decision with a price attached, so it
// is logged with what it was and what it became.
func (a *API) handleSaveRoute(w http.ResponseWriter, r *http.Request) {
	var req routeRequest
	if !decodeBody(w, r, &req) {
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeAPIError(w, r, http.StatusBadRequest, "A route needs a name.")
		return
	}
	// A wildcard would make the allowlist not one, and there is nowhere else
	// left that could create one: routes are the only allowlist. Refused here
	// so that "everything is permitted" is not a state this deployment can
	// reach at all.
	if strings.Contains(req.Name, "*") {
		writeAPIError(w, r, http.StatusBadRequest,
			"A route name cannot contain \"*\". Name each route a client may ask for.")
		return
	}

	var target providers.ModelRef
	if strings.TrimSpace(req.Target) != "" {
		ref, perr := providers.ParseModelRef(req.Target)
		if perr != nil {
			writeAPIError(w, r, http.StatusBadRequest, "The route's model is not valid: "+perr.Error())
			return
		}
		target = ref
	}

	route := store.Route{
		Route: policy.Route{
			Name:           req.Name,
			Description:    strings.TrimSpace(req.Description),
			Users:          cleanList(req.Users),
			Teams:          cleanList(req.Teams),
			Tools:          cleanList(req.Tools),
			Target:         target,
			ServesOverSoft: req.ServesOverSoft == nil || *req.ServesOverSoft,
		},
		EnabledBy: actorFor(PrincipalFrom(r.Context())),
	}

	if err := route.Validate(); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	if msg, err := a.checkRouteReferences(r.Context(), route); err != nil {
		a.fail(w, r, "checking the route", err)
		return
	} else if msg != "" {
		writeAPIError(w, r, http.StatusBadRequest, msg)
		return
	}

	previous := a.previousTarget(r.Context(), req.Name)

	if err := a.db.SaveRoute(r.Context(), route); err != nil {
		a.fail(w, r, "saving the route", err)
		return
	}

	// Reloaded rather than waited for. An administrator who has just changed
	// which model a route runs on should see it in force, not in
	// up-to-thirty-seconds.
	if lerr := a.policy.Load(r.Context()); lerr != nil {
		a.fail(w, r, "reloading policy", lerr)
		return
	}

	a.log.InfoContext(r.Context(), "route saved",
		"route", route.Name,
		"target", req.Target,
		"previous_target", previous,
		"serves_over_soft", route.ServesOverSoft,
		"restricted", route.Restricted(),
		"actor", actorFor(PrincipalFrom(r.Context())))

	body := map[string]any{"name": route.Name, "saved": true}
	// Said after the write, not instead of it. A route that stops at the soft
	// limit with nowhere to send the people it stops is a legitimate thing to
	// want - that is how a deployment says "past the warning line, stop" - and
	// it is also what somebody who forgot the second route has just built.
	// Refusing would take the first away to prevent the second; the warning
	// tells them which one they made.
	if warning := a.stopWithNoAlternative(route.Route); warning != "" {
		body["warning"] = warning
		a.log.WarnContext(r.Context(), "a route stops at the soft limit with nothing to switch to",
			"route", route.Name, "detail", warning)
	}
	writeJSON(w, http.StatusOK, body)
}

// stopWithNoAlternative warns about a route that stops at a soft limit and
// leaves the people it stops with nothing to switch to.
//
// The whole design rests on the refusal naming somewhere else to go. When it
// cannot, the engineer gets a message telling them to switch and a list with
// nothing in it, and the soft limit has silently become a hard one for
// everybody this route admits.
//
// Whether an alternative exists is asked per person rather than globally,
// because conditions make "there is another route" and "there is another route
// for you" different questions: a deployment can have five economy routes and
// still strand the one team whose route names them and nobody else. This checks
// the route's own population against the routes that serve past a soft limit,
// which is the same comparison the refusal will make.
func (a *API) stopWithNoAlternative(route policy.Route) string {
	if route.ServesOverSoft {
		return ""
	}

	for _, other := range a.policy.Snapshot().Routes {
		if other.Name == route.Name || !other.ServesOverSoft || other.Target.IsZero() {
			continue
		}
		// An alternative has to be reachable by everyone this route is. A route
		// with no conditions covers everybody; one with conditions covers this
		// route's population only when its conditions are no narrower.
		if coversAtLeast(other, route) {
			return ""
		}
	}

	return "This route stops at the soft budget limit and no route that serves past one is " +
		"available to the same people, so they will be refused with nothing to switch to. " +
		"Add a route on a cheaper model - or leave this one serving past the soft limit if " +
		"stopping is what you meant."
}

// coversAtLeast reports whether every request `narrow` admits is also admitted
// by `wide`.
//
// A condition is a requirement, so a route with no user condition admits every
// user and one that names people admits only those. `wide` therefore covers
// `narrow` on a condition when it either sets none, or sets a superset of what
// narrow sets. Compared per condition and not across them: the three are ANDed,
// so covering all three is covering the route.
func coversAtLeast(wide, narrow policy.Route) bool {
	for _, c := range []struct{ wide, narrow []string }{
		{wide.Users, narrow.Users},
		{wide.Teams, narrow.Teams},
		{wide.Tools, narrow.Tools},
	} {
		if len(c.wide) == 0 {
			continue
		}
		if len(c.narrow) == 0 {
			// Narrow admits everybody on this condition and wide does not.
			return false
		}
		for _, v := range c.narrow {
			if !slices.ContainsFunc(c.wide, func(w string) bool { return strings.EqualFold(w, v) }) {
				return false
			}
		}
	}
	return true
}

// previousTarget reads the model a route pointed at before this write, for the
// log line. A failure to read it is not a reason to refuse the write: the log
// loses a comparison, the administrator loses nothing.
func (a *API) previousTarget(ctx context.Context, name string) string {
	existing, err := a.db.ListRoutes(ctx)
	if err != nil {
		a.log.WarnContext(ctx, "reading the previous target for the audit log", "error", err)
		return ""
	}
	for _, e := range existing {
		if e.Name == name {
			return e.Target.String()
		}
	}
	return ""
}

// checkRouteReferences refuses a route naming something this deployment does
// not have: a model nobody has discovered, one on a provider that is not
// serving, one that speaks no API this gateway serves, a team or a person who
// does not exist, a tool no request can ever be identified as.
//
// Refused rather than accepted with a warning. A condition naming a user id
// that does not resolve matches nothing, so the route would silently serve
// nobody; a target absent from the catalogue is almost always a typo. Either
// way the alternative is finding out at the desk of whoever's request fails.
func (a *API) checkRouteReferences(ctx context.Context, route store.Route) (string, error) {
	catalogue, err := a.db.ListCatalogue(ctx)
	if err != nil {
		return "", err
	}
	set := a.policy.Snapshot()
	if msg := RouteTargetProblem(route.Route, catalogue, set); msg != "" {
		return msg, nil
	}

	for _, tool := range route.Tools {
		if !tools.Valid(tool) {
			return fmt.Sprintf("%q is not a tool Panorama can identify, so a route restricted to it "+
				"would match no request at all.", tool), nil
		}
	}

	if len(route.Users) > 0 {
		users, uerr := a.db.ListUsers(ctx)
		if uerr != nil {
			return "", uerr
		}
		present := make(map[string]bool, len(users))
		for _, u := range users {
			present[u.ID] = true
		}
		for _, id := range route.Users {
			if !present[id] {
				return "This route names somebody who is not in this deployment. " +
					"Choose people from the list.", nil
			}
		}
	}

	if len(route.Teams) > 0 {
		teams, terr := a.db.ListTeams(ctx)
		if terr != nil {
			return "", terr
		}
		present := make(map[string]bool, len(teams))
		for _, t := range teams {
			present[t.ID] = true
		}
		for _, id := range route.Teams {
			if !present[id] {
				return "This route names a team that does not exist. Choose teams from the list.", nil
			}
		}
	}

	return "", nil
}

// RouteTargetProblem says what is wrong with a route's model, or "" when
// nothing is.
//
// The console and `panorama policy apply` both call it, because a route one of
// them refuses and the other writes is worse than either answer alone. Three
// questions, in the order an administrator can act on them: is the provider
// serving, has discovery seen the model on it, and does the model speak an API
// this gateway serves.
//
// The provider question is asked of the snapshot passed in, and a snapshot
// with no providers in it at all - a policy file applied before anything is
// configured - does not ask it: the order a deployment is set up in should not
// decide whether its policy file applies.
func RouteTargetProblem(route policy.Route, catalogue []store.CatalogueModel, set policy.Set) string {
	if route.Target.IsZero() {
		return ""
	}
	kind := route.Target.Provider

	if len(set.Providers) > 0 {
		state, configured := set.Providers[kind]
		switch {
		case !configured:
			return fmt.Sprintf("The provider %s is not configured on this deployment. "+
				"Configure it in Settings, Model providers, or choose a model on another provider.", kind)
		case !state.Settings.Enabled:
			return fmt.Sprintf("The provider %s is turned off. Turn it on in Settings, Model providers, "+
				"or choose a model on another provider.", kind)
		}
	}

	onProvider := 0
	var row *store.CatalogueModel
	for i := range catalogue {
		if catalogue[i].Provider != kind {
			continue
		}
		onProvider++
		if catalogue[i].ID == route.Target.Model {
			row = &catalogue[i]
		}
	}
	if onProvider == 0 {
		// Nothing to check against. A deployment can write its policy before
		// anybody has run discovery, and refusing here would make the order
		// matter for no reason.
		return ""
	}
	if row == nil {
		return "This deployment has not discovered " + route.Target.String() +
			". Refresh the catalogue, or choose a model from the list."
	}
	if len(row.APIs) == 0 {
		return route.Target.String() + " speaks no API this gateway serves, so a route to it could never " +
			"serve a request. Choose another model, or record the APIs it speaks in Settings, Model providers."
	}
	return ""
}

// cleanList trims a list of identifiers and drops the empties, so a form that
// posts a blank row does not create a condition that matches nothing.
func cleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// handleDeleteRoute removes a name from the allowlist.
func (a *API) handleDeleteRoute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	removed, err := a.db.DeleteRoute(r.Context(), name)
	if err != nil {
		a.fail(w, r, "removing the route", err)
		return
	}
	if !removed {
		writeAPIError(w, r, http.StatusNotFound, "There is no route called "+name+".")
		return
	}

	if lerr := a.policy.Load(r.Context()); lerr != nil {
		a.fail(w, r, "reloading policy", lerr)
		return
	}

	a.log.InfoContext(r.Context(), "route removed",
		"route", name, "actor", actorFor(PrincipalFrom(r.Context())))
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "removed": true})
}

// handleDiscoverModels re-asks the providers what this account can invoke.
//
// Offered as a button because the interval is hours: the moment somebody wants
// this is right after they changed an IAM policy or enabled a model, and
// waiting six hours to see whether it worked is not a diagnosis loop anybody
// completes. Every serving provider, or one named with ?provider=.
func (a *API) handleDiscoverModels(w http.ResponseWriter, r *http.Request) {
	only := providers.Kind(r.URL.Query().Get("provider"))

	var instances []*providers.Instance
	for _, inst := range a.providers.Instances() {
		if only == "" || inst.Settings.Kind == only {
			instances = append(instances, inst)
		}
	}
	if len(instances) == 0 {
		writeAPIError(w, r, http.StatusBadRequest,
			"No model provider is serving. Configure one in Settings, Model providers.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	results := make([]map[string]any, 0, len(instances))
	allOK := true
	total := 0
	for _, inst := range instances {
		kind := inst.Settings.Kind
		n, err := jobs.Discover(ctx, a.db, inst)
		row := map[string]any{"provider": kind, "region": inst.Settings.Region}
		if err != nil {
			// Reported rather than failed with a 500: this reaches AWS, and what
			// an operator needs is the provider's own words plus the permission
			// that is usually missing.
			allOK = false
			row["ok"], row["error"] = false, err.Error()
			if d, ok := a.providers.Descriptor(kind); ok {
				row["hint"] = d.Hint(err.Error())
			}
		} else {
			row["ok"], row["targets"] = true, n
			total += n
		}
		results = append(results, row)
	}

	a.log.InfoContext(r.Context(), "model catalogue refreshed from the console",
		"targets", total, "providers", len(instances), "actor", actorFor(PrincipalFrom(r.Context())))

	writeJSON(w, http.StatusOK, map[string]any{"ok": allOK, "targets": total, "providers": results})
}
