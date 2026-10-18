// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/jobs"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
)

// Model providers are configured here, in the console, by an administrator or
// a setup session - and by `panorama providers`, which calls the functions in
// this file rather than a second copy of them.
//
// Four words for a provider's state, and they are not interchangeable:
//
//	not configured  there is no row, so nothing can be routed to it
//	disabled        there is a row, switched off
//	failing         it is switched on and this process could not build it
//	never checked   it is built, and no live invocation has succeeded yet
//	connected       a live invocation succeeded against the current region
//
// "Never checked" and "failing" are both "not working", and an operator reaching
// for the logs needs to know which - the same distinction the integrations make.
const (
	providerNotConfigured = "not configured"
	providerDisabled      = "disabled"
	providerFailing       = "failing"
	providerNeverChecked  = "never checked"
	providerConnected     = "connected"
)

// ProviderDeps is what configuring a provider touches. The console and the CLI
// each assemble one, so there is one implementation of what a change means.
type ProviderDeps struct {
	DB       *store.DB
	Registry *providers.Registry
	// Policy is reloaded after a change, which is what rebuilds the provider in
	// the snapshot the gateway serves from. Nil in a one-shot CLI command,
	// where the running gateway picks the change up on its next reload.
	Policy *policy.Engine
	// Prices is refreshed for a provider whose region changed. Nil, or one with
	// no refresh interval, leaves pricing to its own schedule.
	Prices *pricing.Service
	Log    *slog.Logger
}

// ProviderChange is a change an administrator asks for. Every field is
// optional on an existing provider; a new one needs a region.
type ProviderChange struct {
	Enabled *bool              `json:"enabled"`
	Region  *string            `json:"region"`
	Options *providers.Options `json:"options"`
}

// ProviderSaved is what a save did.
type ProviderSaved struct {
	Settings      providers.Settings
	RegionChanged bool
	// Discovered is how many targets the new configuration's catalogue holds,
	// and DiscoveryError why it could not be read. A save is not refused over
	// discovery: the configuration is valid, and a region with nothing
	// discoverable yet is something to be told, not stopped by.
	Discovered     int
	DiscoveryError string
	// Stranded names the routes whose model the new catalogue does not have.
	Stranded []string
}

// ErrProviderRefused wraps a change refused for a reason the person asking can
// act on. Its message is meant for them.
var ErrProviderRefused = errors.New("refused")

func refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProviderRefused, fmt.Sprintf(format, args...))
}

// SaveProvider validates and writes a provider change, rebuilds the provider,
// and discovers what it can now invoke.
func SaveProvider(ctx context.Context, deps ProviderDeps, kind providers.Kind, change ProviderChange, actor string) (ProviderSaved, error) {
	descriptor, ok := deps.Registry.Descriptor(kind)
	if !ok {
		return ProviderSaved{}, refusedf("%q is not a model provider this deployment can serve", kind)
	}

	existing, found, err := deps.DB.Provider(ctx, kind)
	if err != nil {
		return ProviderSaved{}, err
	}

	next := existing
	if !found {
		next = providers.Settings{Kind: kind, Enabled: true}
	}
	if change.Enabled != nil {
		next.Enabled = *change.Enabled
	}
	if change.Region != nil {
		next.Region = strings.TrimSpace(*change.Region)
	}
	if change.Options != nil {
		next.Options = *change.Options
	}
	next.ConfiguredBy = actor

	if next.Region == "" {
		return ProviderSaved{}, refusedf("%s needs a region", descriptor.Label)
	}
	if verr := next.Options.Validate(); verr != nil {
		return ProviderSaved{}, refusedf("%s", verr.Error())
	}
	if descriptor.ValidateOptions != nil {
		if verr := descriptor.ValidateOptions(next.Options); verr != nil {
			return ProviderSaved{}, refusedf("%s", verr.Error())
		}
	}

	// Switching off a provider a route uses is refused, naming the routes. A
	// route whose provider is off admits a request and has nowhere to send it,
	// and the engineer on the other end is told it is the deployment's fault -
	// which it would be, and which is avoidable here.
	if found && existing.Enabled && !next.Enabled {
		routes, rerr := deps.DB.RoutesOnProvider(ctx, kind)
		if rerr != nil {
			return ProviderSaved{}, rerr
		}
		if len(routes) > 0 {
			return ProviderSaved{}, refusedf("%s cannot be turned off while routes use it: %s. "+
				"Point those routes at another model first.", descriptor.Label, strings.Join(routes, ", "))
		}
	}

	// A trial build is the validation of everything the descriptor owns - the
	// region's shape, and whatever else this kind needs - done before anything
	// is written, so a configuration that cannot build is refused at the desk
	// rather than stored and reported as failing.
	var instance *providers.Instance
	if next.Enabled {
		instance, err = descriptor.Build(ctx, next, deps.logger())
		if err != nil {
			return ProviderSaved{}, refusedf("%s", err.Error())
		}
		instance.Settings = next
	}

	regionChanged, err := deps.DB.SaveProvider(ctx, next)
	if err != nil {
		return ProviderSaved{}, err
	}
	saved, _, err := deps.DB.Provider(ctx, kind)
	if err != nil {
		return ProviderSaved{}, err
	}
	out := ProviderSaved{Settings: saved, RegionChanged: regionChanged}

	if deps.Policy != nil {
		if lerr := deps.Policy.Load(ctx); lerr != nil {
			return out, lerr
		}
	}

	deps.logger().InfoContext(ctx, "model provider configured",
		"provider", kind, "region", saved.Region, "enabled", saved.Enabled,
		"region_changed", regionChanged, "actor", actor)

	if instance == nil {
		return out, nil
	}

	// Discovered now rather than at the next pass, for the same reason the
	// console's refresh exists: the moment somebody changed a region is the
	// moment they want to see what it offers. The catalogue replace is scoped to
	// this provider, so the old region's rows go in the same transaction the
	// new region's arrive in.
	dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	n, derr := jobs.Discover(dctx, deps.DB, instance)
	if derr != nil {
		out.DiscoveryError = derr.Error()
		deps.logger().WarnContext(ctx, "discovering a newly configured provider", "provider", kind, "error", derr)
	} else {
		out.Discovered = n
		out.Stranded, err = strandedRoutes(ctx, deps.DB, kind)
		if err != nil {
			return out, err
		}
	}

	if regionChanged && deps.Prices != nil && deps.Prices.Enabled() {
		// In the background, and logged: the price list is large, slow, and in
		// another region, and a save that waited for it would be a save that
		// times out while the configuration it wrote is already in force.
		go func() {
			pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
			defer pcancel()
			if perr := deps.Prices.RefreshProvider(pctx, string(kind)); perr != nil {
				deps.logger().ErrorContext(pctx, "refreshing prices for a provider whose region changed",
					"provider", kind, "error", perr)
			}
		}()
	}
	return out, nil
}

func (d ProviderDeps) logger() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}

// strandedRoutes names the routes on a provider whose model its catalogue does
// not list.
func strandedRoutes(ctx context.Context, db *store.DB, kind providers.Kind) ([]string, error) {
	catalogue, err := db.ListCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	routes, err := db.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}
	return stranded(kind, catalogue, routes), nil
}

func stranded(kind providers.Kind, catalogue []store.CatalogueModel, routes []store.Route) []string {
	listed := map[string]bool{}
	for _, m := range catalogue {
		if m.Provider == kind {
			listed[m.ID] = true
		}
	}
	var out []string
	for _, r := range routes {
		if r.Target.Provider == kind && !listed[r.Target.Model] {
			out = append(out, r.Name)
		}
	}
	return out
}

// PreflightProvider asks a candidate configuration what it can invoke, and which
// routes it would strand, without writing anything.
//
// It builds its own instance rather than touching the registry, so a preflight
// for a region somebody is only considering never becomes the provider the
// gateway serves from.
func PreflightProvider(ctx context.Context, deps ProviderDeps, kind providers.Kind, change ProviderChange) (map[string]any, error) {
	descriptor, ok := deps.Registry.Descriptor(kind)
	if !ok {
		return nil, refusedf("%q is not a model provider this deployment can serve", kind)
	}
	existing, _, err := deps.DB.Provider(ctx, kind)
	if err != nil {
		return nil, err
	}
	candidate := existing
	candidate.Kind, candidate.Enabled = kind, true
	if change.Region != nil {
		candidate.Region = strings.TrimSpace(*change.Region)
	}
	if change.Options != nil {
		candidate.Options = *change.Options
	}

	out := map[string]any{"kind": kind, "region": candidate.Region, "stored": false}

	instance, err := descriptor.Build(ctx, candidate, deps.logger())
	if err != nil {
		out["ok"], out["error"] = false, err.Error()
		out["hint"] = descriptor.Hint(err.Error())
		return out, nil
	}

	pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	found, err := instance.Catalogue.Catalogue(pctx)
	if err != nil {
		out["ok"], out["error"] = false, err.Error()
		out["hint"] = descriptor.Hint(err.Error())
		return out, nil
	}

	models := make([]store.CatalogueModel, 0, len(found))
	serveable := 0
	for _, e := range found {
		row := jobs.CatalogueRow(kind, e)
		models = append(models, row)
		if len(row.APIs) > 0 && row.Available() {
			serveable++
		}
	}
	routes, err := deps.DB.ListRoutes(ctx)
	if err != nil {
		return nil, err
	}

	out["ok"] = true
	out["models"] = len(models)
	out["serveable"] = serveable
	out["stranded"] = nonNilList(stranded(kind, models, routes))
	return out, nil
}

// CheckResult is what a live check found.
type CheckResult struct {
	OK       bool
	Target   string
	Region   string
	Duration time.Duration
	// Route names the route Target was chosen from, when it was a route's
	// model rather than one named or picked from the catalogue.
	Route string
	// Error is the provider's own account of a failure (providers.Detail),
	// and Hint the fix for it.
	Error string
	Hint  string
	// Access is what the provider says about Target's availability, read
	// after it refused Target, as evidence beside the refusal.
	Access *providers.ModelAccess
	// Refused are the models tried before Target and refused for a reason
	// particular to each - one AWS does not offer this account, one that has
	// been retired - in the order they were tried.
	Refused []CheckAttempt
}

// CheckAttempt is one model a check tried and the provider refused.
type CheckAttempt struct {
	Target string
	Route  string
	Error  string
	Hint   string
	Access *providers.ModelAccess
}

// maxCheckAttempts bounds how many models one check invokes. Each is a real
// request on somebody's account, and three models refused one after another
// for reasons particular to each is already the finding.
const maxCheckAttempts = 3

// checkCandidate is a model a check may invoke, and the API it is invoked in.
type checkCandidate struct {
	target string
	api    providers.API
	// route is the route this model is the target of, or empty.
	route string
}

// CheckProvider proves a provider can actually invoke a model.
//
// This performs a real invocation, and says so. It is one token of output on
// one model, and it is the only thing that distinguishes a region and a set of
// credentials that work from ones that look right - which is where nearly all
// first-run friction lives. On bedrock-runtime an SCP denying InvokeModel, a
// missing aws-marketplace:Subscribe and a wrong identifier all arrive as the
// same AccessDenied; on bedrock-mantle a role granted everything but the other
// service's actions looks, from every list AWS publishes, exactly like one that
// works.
//
// It streams, and reads the stream to its end, because that is what an
// engineer's coding tool does: a check that proved InvokeModel would pass on
// a role or an account that refuses InvokeModelWithResponseStream, and the
// first real request would be the one to find out.
//
// With no model named, it tries the models the routes on this provider point
// at first - those are what engineers will actually be served - and then the
// catalogue's own choice. It moves on to the next only when the provider
// refused one model for a reason particular to that model, and says which and
// why: an account AWS has not offered the newest model is an account that
// works, and reporting it as broken credentials is how somebody ends up
// granting IAM actions to fix a sales conversation.
func CheckProvider(ctx context.Context, deps ProviderDeps, kind providers.Kind, model string, prices *pricing.Table) (CheckResult, error) {
	descriptor, ok := deps.Registry.Descriptor(kind)
	if !ok {
		return CheckResult{}, refusedf("%q is not a model provider this deployment can serve", kind)
	}
	settings, found, err := deps.DB.Provider(ctx, kind)
	if err != nil {
		return CheckResult{}, err
	}
	if !found {
		return CheckResult{}, refusedf("%s is not configured. Choose a region and save it first.", descriptor.Label)
	}
	if !settings.Enabled {
		return CheckResult{}, refusedf("%s is turned off.", descriptor.Label)
	}

	instance, err := deps.Registry.Build(ctx, settings)
	if err != nil {
		return CheckResult{OK: false, Region: settings.Region, Error: err.Error(), Hint: descriptor.Hint(err.Error())}, nil
	}

	catalogue, err := deps.DB.ListCatalogue(ctx)
	if err != nil {
		return CheckResult{}, err
	}

	var candidates []checkCandidate
	named := strings.TrimSpace(model)
	if named != "" {
		list := instance.APIs(named)
		if len(list) == 0 {
			return CheckResult{}, refusedf("%s speaks no API this gateway serves on %s, so it cannot be checked.",
				named, descriptor.Label)
		}
		candidates = []checkCandidate{{target: named, api: list[0]}}
	} else {
		routes, rerr := deps.DB.ListRoutes(ctx)
		if rerr != nil {
			return CheckResult{}, rerr
		}
		candidates = checkCandidates(kind, instance, routes, catalogue, prices)
		if len(candidates) == 0 {
			// Nothing to try, which on a new deployment nearly always means the
			// startup discovery failed - and credentials that cannot list models
			// are the fault this check exists to find. So the provider is asked
			// now, and its refusal is the check's answer. Refusing with
			// "refresh the catalogue first" would name a button on another
			// screen and say nothing about the credentials, to somebody pressing
			// the one control that says it proves them.
			catalogue, err = discoverCatalogue(ctx, deps, instance)
			var failed discoveryFailed
			if errors.As(err, &failed) {
				deps.logger().WarnContext(ctx, "provider check could not read the catalogue", "provider", kind,
					"region", settings.Region, "error", failed.cause)
				return CheckResult{Region: settings.Region, Error: failed.cause.Error(),
					Hint: descriptor.Hint(failed.cause.Error())}, nil
			}
			if err != nil {
				return CheckResult{}, err
			}
			candidates = checkCandidates(kind, instance, routes, catalogue, prices)
		}
		if len(candidates) == 0 {
			// Not a failure of the provider, and said as its own sentence so it
			// is not mistaken for one: it answered, and nothing it lists is
			// something this gateway can serve.
			return CheckResult{}, refusedf("%s lists no model in %s that this gateway can serve, so there is "+
				"nothing to check with. Choose another region, or name a model identifier to try.",
				descriptor.Label, settings.Region)
		}
	}
	if len(candidates) > maxCheckAttempts {
		candidates = candidates[:maxCheckAttempts]
	}

	result := CheckResult{Region: settings.Region}
	for i, c := range candidates {
		took, ierr := invokeForCheck(ctx, instance, c)
		if ierr == nil {
			result.OK, result.Target, result.Route, result.Duration = true, c.target, c.route, took
			if serr := deps.DB.SetProviderVerified(ctx, kind, settings.Region, c.target, time.Now()); serr != nil {
				return result, serr
			}
			// The refusal this model last met is no longer the answer to "why
			// is this failing". One on another model still might be.
			if cerr := deps.DB.ClearProviderFailure(ctx, kind, settings.Region, c.target); cerr != nil {
				return result, cerr
			}
			deps.logger().InfoContext(ctx, "provider verified", "provider", kind, "model", c.target,
				"region", settings.Region, "duration", took, "refused", len(result.Refused))
			return result, nil
		}

		detail := providers.Detail(ierr)
		attempt := CheckAttempt{Target: c.target, Route: c.route, Error: detail, Hint: descriptor.Hint(detail)}
		if providers.Refusal(ierr) {
			attempt.Access = modelAccess(ctx, deps, kind, instance, catalogue, c.target)
		}
		deps.logger().WarnContext(ctx, "provider check failed", "provider", kind, "model", c.target,
			"region", settings.Region, "error", ierr, "hint", attempt.Hint)

		modelSpecific := descriptor.ModelSpecific != nil && descriptor.ModelSpecific(detail)
		if named != "" || i == len(candidates)-1 || !modelSpecific {
			result.Target, result.Route, result.Duration = c.target, c.route, took
			result.Error, result.Hint, result.Access = attempt.Error, attempt.Hint, attempt.Access
			return result, nil
		}
		result.Refused = append(result.Refused, attempt)
	}
	// Unreachable: the last candidate always returns above.
	return result, nil
}

// invokeForCheck makes the check's one streamed request and reads it to the
// end, so a stream that is refused part way fails the check as it would fail
// an engineer.
func invokeForCheck(ctx context.Context, instance *providers.Instance, c checkCandidate) (time.Duration, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	started := time.Now()
	stream, err := instance.Provider.InvokeStream(cctx, &providers.Request{API: c.api, Model: c.target, Body: checkBody(c.api)})
	if err != nil {
		return time.Since(started), err
	}
	defer func() { _ = stream.Close() }()
	for {
		if _, nerr := stream.Next(); nerr != nil {
			if errors.Is(nerr, io.EOF) {
				return time.Since(started), nil
			}
			return time.Since(started), nerr
		}
	}
}

// modelAccess asks the provider what it says about a refused model's
// availability, where it can say anything. Evidence only, so a failure to read
// it is logged and the check reports the refusal without it.
func modelAccess(
	ctx context.Context, deps ProviderDeps, kind providers.Kind, instance *providers.Instance,
	catalogue []store.CatalogueModel, target string,
) *providers.ModelAccess {
	reporter, ok := instance.Catalogue.(providers.AccessReporter)
	if !ok {
		return nil
	}
	// The foundation model behind a profile, where discovery recorded one:
	// availability is a property of the model, not of the route to it.
	model := target
	for _, m := range catalogue {
		if m.Provider == kind && m.ID == target && m.FoundationModelID != "" {
			model = m.FoundationModelID
			break
		}
	}

	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	access, err := reporter.ModelAccess(actx, model)
	if err != nil {
		deps.logger().WarnContext(ctx, "provider check could not read the model's availability",
			"provider", kind, "model", model, "error", err)
		return nil
	}
	return &access
}

// discoveryFailed is a catalogue that could not be read, kept apart from the
// errors around it: it is the check's finding and is reported as one, the way
// a save reports it, where a catalogue that cannot be listed back is a 500.
type discoveryFailed struct{ cause error }

func (e discoveryFailed) Error() string { return e.cause.Error() }

// discoverCatalogue reads the provider's catalogue on the spot, for a check
// that found nothing stored to pick from. Through jobs.Discover like every
// other refresh, so the rows it leaves behind are the rows the console's own
// refresh would have written.
func discoverCatalogue(ctx context.Context, deps ProviderDeps, instance *providers.Instance) ([]store.CatalogueModel, error) {
	dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if _, err := jobs.Discover(dctx, deps.DB, instance); err != nil {
		return nil, discoveryFailed{cause: err}
	}
	return deps.DB.ListCatalogue(ctx)
}

// checkBody is the smallest valid request in each API: one token of output, no
// tools, no system prompt. Enough to prove credentials, region, and the model,
// and not enough to cost anything worth reporting. Streaming is the call, not
// the body: each provider sets or strips the flag its wire format wants.
func checkBody(api providers.API) []byte {
	switch api {
	case providers.APIOpenAIChat:
		return []byte(`{"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	case providers.APIOpenAIResponses:
		return []byte(`{"max_output_tokens":16,"store":false,"input":"hi"}`)
	default:
		return []byte(`{"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	}
}

// checkCandidates is what a check with no model named may invoke, in order:
// the targets of this provider's routes, in route order, and then
// verificationTargets. A model appears once, under the first route naming it.
func checkCandidates(
	kind providers.Kind, instance *providers.Instance, routes []store.Route,
	catalogue []store.CatalogueModel, prices *pricing.Table,
) []checkCandidate {
	var out []checkCandidate
	seen := map[string]bool{}
	for _, rt := range routes {
		if rt.Target.Provider != kind || seen[rt.Target.Model] {
			continue
		}
		// A route on a model that speaks nothing is refused at the gateway
		// whatever the provider says, so invoking it proves nothing.
		apis := instance.APIs(rt.Target.Model)
		if len(apis) == 0 {
			continue
		}
		seen[rt.Target.Model] = true
		out = append(out, checkCandidate{target: rt.Target.Model, api: apis[0], route: rt.Name})
	}
	for _, m := range verificationTargets(kind, catalogue, prices) {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, checkCandidate{target: m.ID, api: m.APIs[0]})
	}
	return out
}

// verificationTargets orders what the live check may invoke when nobody named
// a model and no route chose one.
//
// A model this gateway can serve at all - one with an API - and streaming,
// because a model that cannot stream cannot serve a coding agent and proving it
// is reachable proves nothing this gateway needs. Available and not legacy,
// because verifying against a model the provider is withdrawing is a check that
// stops working on a date nobody has in their calendar. Anthropic first where
// there is one, because that is the traffic this product was built on.
//
// Cheapest first among what is left, then by identifier so that clicking verify
// twice checks the same model twice. Unpriced targets sort last rather than
// being excluded: "we do not know what this costs" is a weaker reason to skip a
// model than "it will not work".
func verificationTargets(kind providers.Kind, catalogue []store.CatalogueModel, prices *pricing.Table) []store.CatalogueModel {
	type candidate struct {
		row       store.CatalogueModel
		anthropic bool
		rate      pricing.Rate
		priced    bool
	}

	var eligible []candidate
	for _, m := range catalogue {
		if m.Provider != kind || len(m.APIs) == 0 || !m.Streaming || m.Legacy() || !m.Available() {
			continue
		}
		c := candidate{row: m, anthropic: strings.EqualFold(m.Vendor, "Anthropic")}
		if entry, ok := prices.Lookup(string(kind), m.ID); ok && entry.Rates.Input > 0 {
			c.rate, c.priced = entry.Rates.Input, true
		}
		eligible = append(eligible, c)
	}

	sort.Slice(eligible, func(i, j int) bool {
		x, y := eligible[i], eligible[j]
		if x.anthropic != y.anthropic {
			return x.anthropic
		}
		if x.priced != y.priced {
			return x.priced
		}
		if x.priced && x.rate != y.rate {
			return x.rate < y.rate
		}
		return x.row.ID < y.row.ID
	})
	out := make([]store.CatalogueModel, 0, len(eligible))
	for _, c := range eligible {
		out = append(out, c.row)
	}
	return out
}

// providerDeps is the console's view of what configuring a provider touches.
func (a *API) providerDeps() ProviderDeps {
	return ProviderDeps{DB: a.db, Registry: a.providers, Policy: a.policy, Prices: a.priceService, Log: a.log}
}

// handleProviders serves Settings, Model providers.
func (a *API) handleProviders(w http.ResponseWriter, r *http.Request) {
	stored, err := a.db.ListProviders(r.Context())
	if err != nil {
		a.fail(w, r, "reading model providers", err)
		return
	}
	byKind := map[providers.Kind]providers.Settings{}
	for _, s := range stored {
		byKind[s.Kind] = s
	}

	catalogue, err := a.db.ListCatalogue(r.Context())
	if err != nil {
		a.fail(w, r, "reading the model catalogue", err)
		return
	}
	fetched, err := a.db.CatalogueFetchedAt(r.Context())
	if err != nil {
		a.fail(w, r, "reading the model catalogue", err)
		return
	}
	routes, err := a.db.ListRoutes(r.Context())
	if err != nil {
		a.fail(w, r, "reading routes", err)
		return
	}
	refusals, err := a.db.ProviderFailures(r.Context())
	if err != nil {
		a.fail(w, r, "reading provider refusals", err)
		return
	}
	snapshot := a.policy.Snapshot()

	out := make([]map[string]any, 0, len(a.providers.Descriptors()))
	var policyJSON string
	for _, d := range a.providers.Descriptors() {
		policyJSON = d.Policy
		row := map[string]any{
			"kind":    d.Kind,
			"label":   d.Label,
			"summary": d.Summary,
			"regions": d.Regions,
		}

		settings, configured := byKind[d.Kind]
		row["configured"] = configured
		row["state"] = providerNotConfigured

		models, serveable := 0, 0
		for _, m := range catalogue {
			if m.Provider == d.Kind {
				models++
				if len(m.APIs) > 0 && m.Available() {
					serveable++
				}
			}
		}
		var using []string
		for _, rt := range routes {
			if rt.Target.Provider == d.Kind {
				using = append(using, rt.Name)
			}
		}
		catalogueRow := map[string]any{"models": models, "serveable": serveable}
		if at, ok := fetched[d.Kind]; ok {
			catalogueRow["discovered_at"] = at.UTC().Format(time.RFC3339)
		}
		row["catalogue"] = catalogueRow
		row["routes"] = nonNilList(using)

		if configured {
			row["enabled"] = settings.Enabled
			row["region"] = settings.Region
			row["options"] = settings.Options
			row["configured_at"] = settings.ConfiguredAt.UTC().Format(time.RFC3339)
			row["configured_by"] = settings.ConfiguredBy
			if d.Endpoint != nil {
				row["endpoint"] = d.Endpoint(settings.Region)
			}
			if settings.VerifiedAt != nil {
				row["verified_at"] = settings.VerifiedAt.UTC().Format(time.RFC3339)
				row["verified_model"] = settings.VerifiedModel
			}

			state := snapshot.Providers[d.Kind]
			switch {
			case !settings.Enabled:
				row["state"] = providerDisabled
			case state.Err != nil:
				row["state"] = providerFailing
				// The build error names a region or a variable, never a
				// credential: nothing this kind builds from holds one.
				row["error"] = state.Err.Error()
				row["hint"] = d.Hint(state.Err.Error())
			case settings.VerifiedAt == nil:
				row["state"] = providerNeverChecked
			default:
				row["state"] = providerConnected
			}

			// The latest request the provider refused, in its own words. The
			// engineer who met it was told only that it happened and to look
			// here; this screen is administrators' and setup sessions', and
			// the words name nothing an administrator of this account may not
			// read. The hint is derived now rather than stored, so it is
			// today's advice about the words AWS sent then.
			if f, ok := refusals[d.Kind]; ok {
				row["last_refusal"] = map[string]any{
					"at":         f.At.UTC().Format(time.RFC3339),
					"region":     f.Region,
					"model":      f.Model,
					"kind":       f.ErrorKind,
					"error":      f.Message,
					"hint":       d.Hint(f.Message),
					"request_id": f.RequestID,
				}
			}
		}
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"providers": out,
		// One policy for every kind today, served beside the list rather than
		// repeated in each row. Rendered from the same value the CloudFormation
		// template is tested against.
		"iam_policy":    policyJSON,
		"can_configure": canConfigure(PrincipalFrom(r.Context())),
	})
}

// handleSaveProvider writes one provider's configuration.
func (a *API) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	kind := providers.Kind(r.PathValue("kind"))
	var change ProviderChange
	if !decodeBody(w, r, &change) {
		return
	}

	saved, err := SaveProvider(r.Context(), a.providerDeps(), kind, change, actorFor(PrincipalFrom(r.Context())))
	if errors.Is(err, ErrProviderRefused) {
		writeAPIError(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrProviderRefused.Error()+": "))
		return
	}
	if err != nil {
		a.fail(w, r, "saving the model provider", err)
		return
	}

	body := map[string]any{
		"kind":           saved.Settings.Kind,
		"enabled":        saved.Settings.Enabled,
		"region":         saved.Settings.Region,
		"region_changed": saved.RegionChanged,
		"discovered":     saved.Discovered,
		"stranded":       nonNilList(saved.Stranded),
		"saved":          true,
	}
	if saved.DiscoveryError != "" {
		body["discovery_error"] = saved.DiscoveryError
		if d, ok := a.providers.Descriptor(kind); ok {
			body["hint"] = d.Hint(saved.DiscoveryError)
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// handlePreflightProvider answers what a candidate configuration offers, and
// writes nothing.
func (a *API) handlePreflightProvider(w http.ResponseWriter, r *http.Request) {
	kind := providers.Kind(r.PathValue("kind"))
	var change ProviderChange
	if !decodeBody(w, r, &change) {
		return
	}
	out, err := PreflightProvider(r.Context(), a.providerDeps(), kind, change)
	if errors.Is(err, ErrProviderRefused) {
		writeAPIError(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrProviderRefused.Error()+": "))
		return
	}
	if err != nil {
		a.fail(w, r, "checking the candidate provider configuration", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type providerCheckRequest struct {
	// Model is the identifier to invoke. Empty picks a route's model, or the
	// cheapest thing discovery found that this gateway can serve, which is
	// what somebody clicking "check" means.
	Model string `json:"model"`
}

// handleCheckProvider performs the live check.
func (a *API) handleCheckProvider(w http.ResponseWriter, r *http.Request) {
	kind := providers.Kind(r.PathValue("kind"))
	var req providerCheckRequest
	if !decodeBody(w, r, &req) {
		return
	}

	result, err := CheckProvider(r.Context(), a.providerDeps(), kind, req.Model, a.prices)
	if errors.Is(err, ErrProviderRefused) {
		writeAPIError(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), ErrProviderRefused.Error()+": "))
		return
	}
	if err != nil {
		a.fail(w, r, "checking the model provider", err)
		return
	}

	body := map[string]any{
		"ok":     result.OK,
		"kind":   kind,
		"target": result.Target,
		"region": result.Region,
	}
	if result.Route != "" {
		body["route"] = result.Route
	}
	refused := make([]map[string]any, 0, len(result.Refused))
	for _, at := range result.Refused {
		row := map[string]any{"target": at.Target, "error": at.Error, "hint": at.Hint}
		if at.Route != "" {
			row["route"] = at.Route
		}
		if at.Access != nil {
			row["access"] = renderAccess(*at.Access)
		}
		refused = append(refused, row)
	}
	body["refused"] = refused
	if result.OK {
		body["duration_ms"] = result.Duration.Milliseconds()
		a.log.InfoContext(r.Context(), "provider checked from the console",
			"provider", kind, "actor", actorFor(PrincipalFrom(r.Context())))
	} else {
		body["error"], body["hint"] = result.Error, result.Hint
		if result.Access != nil {
			body["access"] = renderAccess(*result.Access)
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// renderAccess is the one shape a provider's availability report crosses the
// wire in, for the check and for nothing else.
func renderAccess(m providers.ModelAccess) map[string]any {
	out := map[string]any{
		"model":         m.Model,
		"agreement":     m.Agreement,
		"authorization": m.Authorization,
		"entitlement":   m.Entitlement,
		"region":        m.Region,
	}
	if m.AgreementError != "" {
		out["agreement_error"] = m.AgreementError
	}
	return out
}

// providerSummaries is the short form of every provider, for screens that name
// providers without configuring them: routing, the organization document.
func (a *API) providerSummaries() []map[string]any {
	snapshot := a.policy.Snapshot()
	out := []map[string]any{}
	for _, d := range a.providers.Descriptors() {
		state, configured := snapshot.Providers[d.Kind]
		row := map[string]any{"kind": d.Kind, "label": d.Label, "configured": configured}
		if configured {
			row["region"] = state.Settings.Region
			row["enabled"] = state.Settings.Enabled
			row["serving"] = state.Serving()
		}
		out = append(out, row)
	}
	return out
}

// providerLabel names a kind the way a screen does.
func (a *API) providerLabel(kind providers.Kind) string {
	if d, ok := a.providers.Descriptor(kind); ok {
		return d.Label
	}
	return string(kind)
}
