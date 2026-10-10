// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package dataplane is the proxy: the client-facing API surfaces and the
// streaming path between a coding agent and a model provider.
//
// Three API surfaces, each served natively and none translated into another:
// the Anthropic Messages API at /v1/messages, so Claude Code connects with one
// environment change; OpenAI Chat Completions at /v1/chat/completions, for
// opencode's OpenAI-compatible provider, Cline and Continue; and OpenAI
// Responses at /v1/responses, which is the only API Codex speaks. A route is
// served on the surfaces its model speaks, and nowhere else. Each surface has
// its own attribution extractor, because a surface whose traffic lands in the
// unattributed bucket is the one failure this product cannot ship.
//
// Streaming is sacred here. Nothing on this path may buffer a response, block,
// or add meaningful latency; the target is sub-10ms added to time-to-first
// token. Server-sent events are relayed as they arrive, one flush per event.
//
// Request and response bodies are matched in flight and discarded. Nothing in
// this package may persist one.
package dataplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/tools"
	"github.com/dnxlabs/panorama/internal/websearch"
)

// defaultMaxBodyBytes bounds a request body. Coding-agent requests carry whole
// conversations, so the ceiling is high - but unbounded is not a ceiling.
const defaultMaxBodyBytes int64 = 32 << 20 // 32 MiB

// Options configures a data plane.
type Options struct {
	Store *store.DB
	// Prices is the in-memory rate table. A nil table records every request
	// unpriced, which is a legitimate state for a deployment that has not been
	// given prices yet.
	Prices *pricing.Table
	// Policy decides everything about where a request goes: whether the route
	// it named exists for the caller, which model that route becomes, which
	// provider serves it, and whether a budget stops it. A nil engine refuses
	// everything, which is what a deployment with no routes is - there is no
	// second allowlist that could let a request through while this one is
	// empty, and no provider held here that a request could reach around it.
	Policy *policy.Engine
	// Attribution extracts work signals from request bodies in flight. A nil
	// extractor attributes nothing, and every request is recorded honestly as
	// unattributed.
	Attribution *attribution.Extractor
	// Sessions is the live-conversation cache that keeps attribution to once
	// per session rather than once per turn.
	Sessions *attribution.Sessions
	// WebSearch holds the backend /mcp searches with. Nil serves no search,
	// and the endpoint lists no tools.
	WebSearch    *websearch.Service
	Logger       *slog.Logger
	MaxBodyBytes int64
}

// DataPlane serves the client-facing API surfaces.
type DataPlane struct {
	store        *store.DB
	prices       *pricing.Table
	policy       *policy.Engine
	extractor    *attribution.Extractor
	sessions     *attribution.Sessions
	webSearch    *websearch.Service
	log          *slog.Logger
	maxBodyBytes int64

	// recording tracks the off-path metadata writes so a graceful shutdown -
	// and a test - can wait for them instead of racing them.
	recording sync.WaitGroup

	// unpricedWarned remembers which models have already been reported as
	// unpriced, so the log names each one once rather than once per request.
	unpricedWarned sync.Map
}

// New builds a data plane.
func New(opts Options) *DataPlane {
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	if opts.Sessions == nil {
		opts.Sessions = attribution.NewSessions(0)
	}

	return &DataPlane{
		store:        opts.Store,
		prices:       opts.Prices,
		policy:       opts.Policy,
		extractor:    opts.Attribution,
		sessions:     opts.Sessions,
		webSearch:    opts.WebSearch,
		log:          opts.Logger,
		maxBodyBytes: maxBody,
	}
}

// Drain waits for the off-path metadata writes to finish. Shutdown calls it so
// a clean stop does not discard the audit trail for requests it just served.
func (d *DataPlane) Drain() { d.recording.Wait() }

// Routes returns the handlers to mount, keyed by pattern.
//
// Every route here is authenticated. There is no unauthenticated path into the
// data plane, and adding one needs an explicit reason in review.
func (d *DataPlane) Routes() map[string]http.Handler {
	serve := func(sf *surface) http.Handler {
		return d.authenticated(sf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d.handleInvoke(w, r, sf)
		}))
	}
	mcp := d.authenticated(mcpSurface, http.HandlerFunc(d.handleMCP))
	return map[string]http.Handler{
		"POST /v1/messages":         serve(anthropicSurface),
		"POST /v1/chat/completions": serve(chatSurface),
		"POST /v1/responses":        serve(responsesSurface),
		// The routes a caller may use, in whichever of the two shapes the
		// caller's client reads. Authenticated like the rest: a route list is
		// the allowlist as it applies to one person.
		"GET /v1/models": d.authenticatedBy(modelsSurface, http.HandlerFunc(d.handleModels)),
		// Tools the gateway runs itself, over MCP, for every coding tool and
		// every model. Each method is named rather than one pattern for all,
		// because a method-less /mcp and the console's GET / would each be
		// more specific than the other and the mux refuses the pair. GET and
		// DELETE are answered 405, after authentication like everything else.
		"POST /mcp":   mcp,
		"GET /mcp":    mcp,
		"DELETE /mcp": mcp,
	}
}

// record writes request metadata without delaying the client.
//
// The response has already been delivered by the time this runs. A failure to
// record is logged rather than surfaced: losing an audit row is bad, but
// failing a request that already succeeded is worse and helps nobody.
//
// obs carries what the request body revealed about the work behind it, and is
// nil when nothing did. Note what it is not: it holds a fingerprint and a
// handful of extracted values, never the body. The body is matched in the
// handler and left there to be collected, so nothing downstream of this point
// could store one even if it wanted to.
func (d *DataPlane) record(ctx context.Context, rec store.RequestRecord, obs *store.Observation) {
	if d.store == nil {
		return
	}

	// WithoutCancel rather than Background: the response has been delivered, so
	// the request's cancellation must not abort the write - but its values, and
	// the log correlation they carry, are still the right ones.
	writeCtx := context.WithoutCancel(ctx)

	d.recording.Add(1)
	go func() {
		defer d.recording.Done()

		ctx, cancel := context.WithTimeout(writeCtx, 5*time.Second)
		defer cancel()

		// Attribution is resolved before the request row is written, because the
		// row carries the session it belongs to.
		d.attribute(ctx, &rec, obs)

		// Costing happens here rather than in the handler so that not one
		// microsecond of it lands between the model's last token and the
		// client's receipt of it.
		d.price(ctx, &rec)

		// The budget ledger is updated from the priced record, which is the
		// only moment the cost is known. Only requests that reached a provider
		// count: attempts is the same test the reconcile query applies, so the
		// in-memory tally and the database agree about what a budget counts.
		if rec.Attempts > 0 {
			d.policy.Record(policy.Completed{
				UserID: rec.UserID,
				// Carried through so the ledger credits the budgets that
				// enforcement checked. A service token reaches its team here
				// the same way it did there - directly, because there is no
				// membership to look it up through.
				TeamID:     rec.TeamID,
				At:         rec.StartedAt,
				Cost:       rec.Cost,
				UsedTokens: !rec.Usage().Empty(),
			})
		}

		if err := d.store.RecordRequest(ctx, rec); err != nil {
			d.log.ErrorContext(ctx, "recording request metadata",
				"error", err,
				"request_id", rec.RequestID,
				"userId", rec.UserID,
				"model", rec.ResolvedModel,
				"provider", rec.Provider,
				"tool", rec.Tool,
			)
		}
		switch {
		case rec.ServiceTokenID != "":
			if err := d.store.TouchServiceToken(ctx, rec.ServiceTokenID); err != nil {
				d.log.WarnContext(ctx, "recording token use", "error", err)
			}
		case rec.TokenID != "":
			if err := d.store.TouchToken(ctx, rec.TokenID); err != nil {
				d.log.WarnContext(ctx, "recording token use", "error", err)
			}
		}
	}()
}

// observe extracts what a request reveals about the work behind it.
//
// This is the only part of attribution that touches a body, and it runs inline
// because the body exists only here. It is bounded twice over: the fingerprint
// is a hash of a couple of kilobytes, and the full scan is skipped entirely
// once the conversation has structural attribution - which, for a session of a
// hundred turns, means ninety-nine of them pay the hash alone.
//
// The returned Observation carries extracted values and nothing else. That is
// the privacy invariant expressed as a type: the body cannot travel past this
// function because it is not in the thing that travels.
func (d *DataPlane) observe(r *http.Request, rec store.RequestRecord, body []byte, shape attribution.Shape) *store.Observation {
	if d.extractor == nil {
		return nil
	}

	// Fingerprints mix in the principal so that two conversations opening
	// identically are never merged. Subject is that principal - a user id for a
	// person, the token id for a pipeline - because a service token's empty
	// user id would merge every CI job in the deployment into one session.
	subject := rec.Subject()
	fingerprint, method := d.extractor.FingerprintShape(shape, subject, body)

	// A client that groups its own turns is exact where inference is a very
	// good guess, so an explicit session header wins outright.
	//
	// opencode and Claude Code group their own turns without being asked to,
	// in headers of their own. Each is read only from the tool that sends it,
	// because the meaning of a header is the meaning its sender gave it, and
	// hashed as a stamp is, so the same id lands in one session and never in
	// somebody else's.
	switch stamped := attribution.CleanHeader(r.Header.Get(attribution.HeaderSession)); {
	case stamped != "":
		fingerprint, method = attribution.StampedFingerprint(subject, stamped), "client-header"
	case rec.Tool == tools.OpenCode:
		if own := attribution.OpenCodeSession(r.Header.Get); own != "" {
			fingerprint, method = attribution.StampedFingerprint(subject, own), "opencode-session"
		}
	case rec.Tool == tools.ClaudeCode:
		if own := attribution.ClaudeCodeSession(r.Header.Get); own != "" {
			fingerprint, method = attribution.StampedFingerprint(subject, own), "claude-code-session"
		}
	}
	if fingerprint == "" {
		return nil
	}

	signals := attribution.Stamp(r.Header.Get)
	if d.sessions.NeedsExtraction(fingerprint) {
		signals = attribution.Combine(signals, d.extractor.StructuralShape(shape, body))
	}
	signals.Fingerprint, signals.Method = fingerprint, method

	return &store.Observation{
		Fingerprint:    fingerprint,
		Method:         method,
		UserID:         rec.UserID,
		ServiceTokenID: rec.ServiceTokenID,
		Tool:           rec.Tool,
		At:             rec.StartedAt,
		Signals:        signals,
	}
}

// attribute persists the observation and tells the cache what was decided.
//
// A failure here costs an attribution, not a request: the audit row is still
// written, and it is written honestly, as unattributed.
func (d *DataPlane) attribute(ctx context.Context, rec *store.RequestRecord, obs *store.Observation) {
	if obs == nil || d.store == nil {
		return
	}

	session, err := d.store.AttributeSession(ctx, *obs)
	if err != nil {
		d.log.ErrorContext(ctx, "attributing a session",
			"error", err,
			"request_id", rec.RequestID,
			"userId", rec.UserID,
			"tool", rec.Tool,
		)
		return
	}

	rec.SessionID = session.ID
	d.sessions.Remember(attribution.Known{
		ID:          session.ID,
		Fingerprint: session.Fingerprint,
		Rung:        attribution.Rung(session.Rung),
		Key:         session.WorkItemKey,
		LastSeen:    obs.At,
	})
}

// price computes what a request cost and attaches it to the record.
//
// A request that cannot be priced is left with a nil cost, which the store
// writes as null. That is the honest answer and it is deliberately not zero:
// unpriced spend stays countable and shows up beside every total, in the same
// way unattributed spend does. Pricing at zero would make an incomplete price
// table look like a discount.
func (d *DataPlane) price(ctx context.Context, rec *store.RequestRecord) {
	usage := rec.Usage()
	if usage.Empty() {
		// A request refused at the gateway or failed before the model saw it
		// has no cost, as distinct from an unknown one.
		return
	}
	if rec.ResolvedModel == "" {
		return
	}

	cost, entry, err := d.prices.Price(rec.Provider, rec.ResolvedModel, usage)
	if err != nil {
		d.warnUnpriced(ctx, rec, err)
		return
	}

	rec.Cost = &cost
	rec.CostSource = entry.Source
}

// warnUnpriced reports a model the price table cannot cost, once per model.
//
// Once, because the alternative is a line per request: a misconfigured
// deployment would bury its own logs and the operator would learn nothing they
// did not learn from the first line. The message carries the exact identifier
// to add, since that is the whole action the reader can take.
func (d *DataPlane) warnUnpriced(ctx context.Context, rec *store.RequestRecord, err error) {
	if _, seen := d.unpricedWarned.LoadOrStore(rec.Provider+"/"+rec.ResolvedModel, struct{}{}); seen {
		return
	}

	// Two causes, and they send the reader to different places: a model with
	// no row at all has never been priced, while a missing class is a gap in a
	// price that exists. Reporting the first in the words of the second sends
	// somebody looking for the class a request used rather than for the model
	// nobody has priced.
	hint := "the model has a price, but not for this token class: state the class under pricing.models"
	if errors.Is(err, pricing.ErrNoPrice) {
		hint = "no price for this model: set one under pricing.models, or run a release whose price table carries it"
	}

	model, scope := pricing.ParseModel(rec.ResolvedModel)
	d.log.LogAttrs(ctx, slog.LevelWarn, "requests are being recorded unpriced",
		slog.String("provider", rec.Provider),
		slog.String("model", rec.ResolvedModel),
		slog.String("priced_as", model),
		slog.String("scope", string(scope)),
		slog.String("error", err.Error()),
		slog.String("hint", hint),
	)
}
