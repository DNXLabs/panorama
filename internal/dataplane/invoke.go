// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/controlplane"
	"github.com/dnxlabs/panorama/internal/costcenters"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/tools"
)

// handleInvoke serves one model invocation on one surface.
//
// There is one of these, not one per API. The three surfaces differ in the
// shape of a body, an error and a stream, and in nothing that decides anything:
// every request is authenticated the same way, attributed before policy, decided
// by one call to the engine, recorded by one function, and sent to exactly one
// provider. A handler per surface would be three copies of the order those
// happen in, and the order is the part that has to be right.
func (d *DataPlane) handleInvoke(w http.ResponseWriter, r *http.Request, sf *surface) {
	start := time.Now()
	requestID := controlplane.RequestIDFrom(r.Context())
	principal := PrincipalFrom(r.Context())

	rec := store.RequestRecord{
		RequestID: requestID,
		StartedAt: start.UTC(),
		Surface:   sf.name,
		Tool:      tools.FromUserAgent(r.UserAgent()),
		UserAgent: r.UserAgent(),
		ClientIP:  clientIP(r),
		Decision:  store.DecisionAllowed,
	}
	if principal != nil {
		rec.UserID = principal.UserID
		if principal.IsService() {
			// A service token has no user, and its two identifying facts go in
			// their own columns rather than borrowing the personal ones. Team
			// is what its spend bills to; the token id is what a leaked
			// credential is traced by after it has been revoked and deleted.
			rec.TeamID = principal.TeamID
			rec.ServiceTokenID = principal.TokenID
		} else {
			rec.TokenID = principal.TokenID
		}
	}

	// obs is what the body revealed about the work behind this request. It is
	// filled in below, once there is a body to look at, and the failure paths
	// close over it so that a request refused after extraction still lands in
	// its session.
	var obs *store.Observation

	// fail records the outcome and answers the client. Every exit from this
	// handler goes through here or through the streaming path below, so no
	// request escapes the audit trail.
	fail := func(status int, errType, kind, message string) { //nolint:contextcheck // closes over the request context by design
		rec.StatusCode = status
		rec.Decision = store.DecisionError
		rec.ErrorKind = kind
		rec.Duration = time.Since(start)
		d.record(r.Context(), rec, obs)
		sf.writeError(w, status, errType, message, requestID)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, d.maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(http.StatusRequestEntityTooLarge, errTypeRequestTooLarge, "body_too_large",
				fmt.Sprintf("The request body exceeds Panorama's %d MiB limit.", d.maxBodyBytes>>20))
			return
		}
		fail(http.StatusBadRequest, errTypeInvalidRequest, "body_read",
			"Panorama could not read the request body.")
		return
	}

	// A shallow decode: the top level only. The message array - nearly the
	// whole payload - stays as opaque bytes, and the same map is handed to the
	// provider for its own rewrite, so the conversation is scanned once.
	var fields map[string]json.RawMessage
	if perr := json.Unmarshal(body, &fields); perr != nil || fields == nil {
		fail(http.StatusBadRequest, errTypeInvalidRequest, "body_parse",
			"The request body is not a JSON object.")
		return
	}
	envelope, problem := readEnvelope(fields)
	if problem != "" {
		fail(http.StatusBadRequest, errTypeInvalidRequest, "body_parse", problem)
		return
	}
	rec.RequestedModel = envelope.Model
	rec.Streaming = envelope.Stream

	if envelope.Model == "" {
		fail(http.StatusBadRequest, errTypeInvalidRequest, "model_missing",
			"The request did not name a model.")
		return
	}

	// Attribution runs before policy, for two reasons. A request that policy is
	// about to refuse is still a turn of somebody's conversation, and the work
	// it was refused on is exactly what an operator will ask about. And it is
	// the last point at which the body is in hand.
	obs = d.observe(r, rec, body, sf.shape)

	// One decision, and it decides everything: whether this name exists for
	// this caller, whether a budget stops them, which model the name becomes,
	// and which provider serves it. There is no second lookup and no allowlist
	// beside this one - the route *is* the allowlist, so a name that resolves
	// has already been permitted, and there is no order in which the two could
	// disagree.
	decision := d.policy.Evaluate(policy.Input{
		UserID:  rec.UserID,
		TeamID:  rec.TeamID,
		Tool:    rec.Tool,
		Surface: rec.Surface,
		API:     sf.api,
		Route:   envelope.Model,
		// Held to the same bounds as every other client-supplied header. The
		// engine resolves it against the snapshot, so this costs a map read.
		CostCenterStamp: attribution.CleanHeader(r.Header.Get(costcenters.Header)),
		Now:             start,
	})
	rec.BudgetState = string(decision.Budget)
	// Allocation, carried straight through: the engine resolved it and nothing
	// acts on it. A stamp that named nothing is recorded as sent, so a
	// deployment full of misspelt codes is visible in the audit trail instead
	// of quietly allocating by the rules and looking correct.
	rec.CostCenterID, rec.CostCenterStamp = decision.CostCenterID, decision.CostCenterStamp

	if decision.Refused {
		d.refuse(w, r, sf, rec, obs, decision, envelope.Model, start)
		return
	}

	if decision.Blocked {
		// Two ways a budget stops a request - out of money, or out of this
		// particular route - told apart by the reason rather than by a second
		// decision value. `budget_state` already says which line was crossed,
		// so the console reads the pair and nothing has to be kept in step
		// with a third column that says it again.
		rec.Decision = store.DecisionBlocked
		rec.StatusCode = http.StatusForbidden
		rec.ErrorKind = decision.BlockReason
		rec.Duration = time.Since(start)
		d.record(r.Context(), rec, obs)

		d.log.LogAttrs(r.Context(), slog.LevelWarn, "request stopped by a budget",
			append(logAttrs(r.Context()),
				slog.String("request_id", requestID),
				slog.String("route", envelope.Model),
				slog.String("reason", decision.BlockReason),
				slog.String("budget_state", string(decision.Budget)),
				slog.String("budget", decision.BudgetID),
			)...)

		// 403 rather than 429, deliberately. A rate-limit status invites a
		// client to retry with backoff, and a budget that resets next month
		// would have Claude Code retrying into a wall for weeks. A permission
		// error is understood as "not until something changes", which is what
		// this is, and the message says what has to change - including, for a
		// soft limit, the route names that would work right now.
		sf.writeError(w, http.StatusForbidden, errTypePermission, decision.BlockMessage, requestID)
		return
	}

	rec.Provider = string(decision.Target.Provider)
	rec.ResolvedModel = decision.Target.Model

	if refusal := sf.prepare(fields, &envelope); refusal != "" {
		// A request the route could serve, asking for something this gateway
		// does not offer on this surface - a stored response to continue, a
		// service tier billed at a different rate. Refused before a provider
		// is invoked, so it costs nothing, and said in words the engineer can
		// act on.
		rec.ResolvedModel = ""
		fail(http.StatusBadRequest, errTypeInvalidRequest, "unsupported_parameter", refusal)
		return
	}

	if decision.Warning != "" {
		// Crossing a soft limit is information, not a failure: the request is
		// served. The headers are additive and unknown to the clients, which
		// ignore them - the log line is what an operator acts on, and the
		// console reads the budget state off the request row.
		w.Header().Set("x-panorama-budget-state", string(decision.Budget))
		w.Header().Set("x-panorama-budget-warning", decision.Warning)

		d.log.LogAttrs(r.Context(), slog.LevelWarn, "request is over a soft budget limit",
			append(logAttrs(r.Context()),
				slog.String("request_id", requestID),
				slog.String("budget", decision.BudgetID),
				slog.String("detail", decision.Warning),
			)...)
	}

	betas := betasFrom(r.Header)
	if len(betas) > 0 {
		// Beta flags are request metadata, not content, and which ones a client
		// asks for is the first thing to check when a provider rejects an
		// otherwise valid request.
		d.log.LogAttrs(r.Context(), slog.LevelDebug, "client requested beta features",
			append(logAttrs(r.Context()),
				slog.String("request_id", requestID),
				slog.String("betas", strings.Join(betas, ",")),
			)...)
	}

	req := &providers.Request{
		API:    sf.api,
		Model:  decision.Target.Model,
		Body:   body,
		Fields: fields,
		Betas:  betas,
	}

	if envelope.Stream {
		d.stream(w, r, sf, decision, req, envelope, rec, obs, start)
		return
	}

	// One invocation. A route is one model, so a throttle is the client's to
	// retry: it arrives as a 429, which every client this gateway serves
	// already handles.
	rec.Attempts = 1
	resp, err := decision.Provider.Invoke(r.Context(), req)
	if err != nil {
		status, errType, kind, message := providerFailure(err)
		d.logProviderFailure(r, requestID, decision.Target, err)
		fail(status, errType, kind, message)
		return
	}

	setUsage(&rec, resp.Usage)
	rec.StatusCode = http.StatusOK
	rec.Duration = time.Since(start)
	d.record(r.Context(), rec, obs)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(resp.Body); err != nil {
		d.log.WarnContext(r.Context(), "writing response to client", "error", err, "request_id", requestID)
	}
}

// refuse answers a request the engine refused, and records why.
func (d *DataPlane) refuse(
	w http.ResponseWriter, r *http.Request, sf *surface,
	rec store.RequestRecord, obs *store.Observation, decision policy.Decision,
	route string, start time.Time,
) {
	requestID := controlplane.RequestIDFrom(r.Context())
	rec.ResolvedModel = ""
	rec.Decision = store.DecisionRefused
	rec.ErrorKind = decision.RefusedReason
	rec.StatusCode = http.StatusForbidden

	switch decision.RefusedReason {
	case policy.RefusedNoTarget, policy.RefusedProviderUnavailable:
		// A route that admitted the request and had nowhere to send it - no
		// model, or a model on a provider that is not serving. The engineer can
		// do nothing about either, so the fault is reported as this side's: a
		// 502, and an error in the log for the operator.
		rec.StatusCode = http.StatusBadGateway
		rec.Duration = time.Since(start)
		d.record(r.Context(), rec, obs)

		d.log.LogAttrs(r.Context(), slog.LevelError, "a route has nowhere to send a request",
			append(logAttrs(r.Context()),
				slog.String("request_id", requestID),
				slog.String("route", route),
				slog.String("reason", decision.RefusedReason),
			)...)

		sf.writeError(w, http.StatusBadGateway, errTypeAPI, decision.RefusedMessage, requestID)
		return

	case policy.RefusedWrongAPI:
		// The route exists for this caller, and their tool is pointed at an
		// endpoint its model does not speak. Their configuration, and theirs to
		// fix, so a 400 naming the endpoint that would work.
		rec.StatusCode = http.StatusBadRequest
		rec.Duration = time.Since(start)
		d.record(r.Context(), rec, obs)

		d.log.LogAttrs(r.Context(), slog.LevelWarn, "route requested on an API its model does not speak",
			append(logAttrs(r.Context()),
				slog.String("request_id", requestID),
				slog.String("route", route),
				slog.String("surface", sf.name),
			)...)

		sf.writeError(w, http.StatusBadRequest, errTypeInvalidRequest, decision.RefusedMessage, requestID)
		return
	}

	rec.Duration = time.Since(start)
	d.record(r.Context(), rec, obs)

	// The log distinguishes the two refusals the client is told nothing
	// about: a name nobody created, and a name whose conditions exclude this
	// caller. An operator needs to tell those apart - the first is usually a
	// misconfigured tool, the second a condition doing its job - and the
	// engineer is told the same thing either way, because a route somebody may
	// not use is not one they should be able to detect.
	d.log.LogAttrs(r.Context(), slog.LevelWarn, "route refused at the gateway",
		append(logAttrs(r.Context()),
			slog.String("request_id", requestID),
			slog.String("route", route),
			slog.String("reason", decision.RefusedReason),
			slog.String("surface", sf.name),
		)...)

	sf.writeError(w, http.StatusForbidden, errTypePermission, decision.RefusedMessage, requestID)
}

// stream relays a streaming invocation.
//
// Every event is written and flushed as it arrives. Nothing is accumulated:
// the only state kept across events is the token accounting the provider
// reports, which is a handful of integers.
func (d *DataPlane) stream(
	w http.ResponseWriter,
	r *http.Request,
	sf *surface,
	decision policy.Decision,
	req *providers.Request,
	envelope envelope,
	rec store.RequestRecord,
	obs *store.Observation,
	start time.Time,
) {
	requestID := controlplane.RequestIDFrom(r.Context())

	rec.Attempts = 1
	stream, err := decision.Provider.InvokeStream(r.Context(), req)
	if err != nil {
		status, errType, kind, message := providerFailure(err)
		d.logProviderFailure(r, requestID, decision.Target, err)

		rec.StatusCode = status
		rec.Decision = store.DecisionError
		rec.ErrorKind = kind
		rec.Duration = time.Since(start)
		d.record(r.Context(), rec, obs)

		sf.writeError(w, status, errType, message, requestID)
		return
	}
	defer func() { //nolint:contextcheck // closes over the request context by design
		if err := stream.Close(); err != nil {
			d.log.WarnContext(r.Context(), "closing provider stream", "error", err, "request_id", requestID)
		}
	}()

	// Headers go out before the first event so the client can start parsing
	// immediately. X-Accel-Buffering tells an intermediary proxy not to
	// undo the whole point of this by buffering.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		// A ResponseWriter that cannot flush cannot stream. Failing loudly
		// here beats silently buffering the whole response.
		d.log.ErrorContext(r.Context(), "response writer cannot flush; streaming is impossible",
			"error", err, "request_id", requestID)
	}

	rec.StatusCode = http.StatusOK

	var firstToken time.Time
	relayed := 0
	clean := false
	for {
		payload, err := stream.Next()
		if errors.Is(err, io.EOF) {
			clean = true
			break
		}
		if err != nil {
			// Mid-stream failure. The status is long since committed, so the
			// only honest report is in-band.
			_, errType, kind, message := providerFailure(err)
			// Two facts the error text cannot carry, and without which the
			// reasons a stream stops short are indistinguishable in a log.
			// How much of the response already went out separates a provider
			// that never really started from one that died near the end. And
			// whether the inbound request is still live says whether the
			// client was there to receive any of it - a caller that hangs up
			// cancels this request's context, which closes the upstream
			// connection under the read, so the provider error is the
			// consequence and the disconnect is the cause. Without this the
			// two read identically.
			//
			// The provider's own account of the connection joins them where it
			// has one, on this line rather than a second: correlating two log
			// lines by timestamp under concurrent traffic is exactly the work
			// this is meant to save.
			failure := []slog.Attr{
				slog.Int("events_relayed", relayed),
				slog.Bool("client_gone", r.Context().Err() != nil),
			}
			if diag, ok := stream.(providers.Diagnostic); ok {
				failure = append(failure, diag.Diagnostics()...)
			}
			d.logProviderFailure(r, requestID, decision.Target, err, failure...)
			rec.Decision = store.DecisionError
			rec.ErrorKind = kind

			if werr := sf.writeStreamError(w, errType, message, requestID); werr != nil {
				d.log.WarnContext(r.Context(), "writing stream error", "error", werr, "request_id", requestID)
			}
			_ = rc.Flush()
			break
		}

		if sf.withhold != nil && sf.withhold(payload, envelope) {
			// Something the gateway asked the provider for on its own account -
			// the usage chunk a Chat Completions client did not request - and
			// that client would not expect to receive.
			continue
		}

		if firstToken.IsZero() {
			firstToken = time.Now()
		}

		if err := writeSSE(w, sf.eventName(payload), payload); err != nil {
			// The client hung up. Nothing to report to it; the record still
			// gets written.
			d.log.DebugContext(r.Context(), "client disconnected mid-stream",
				"error", err, "request_id", requestID)
			rec.ErrorKind = "client_disconnected"
			break
		}
		if err := rc.Flush(); err != nil {
			d.log.DebugContext(r.Context(), "flush failed mid-stream",
				"error", err, "request_id", requestID)
			rec.ErrorKind = "client_disconnected"
			break
		}
		relayed++
	}

	if clean && sf.end != nil {
		if err := sf.end(w); err != nil {
			rec.ErrorKind = "client_disconnected"
		} else {
			_ = rc.Flush()
		}
	}

	setUsage(&rec, stream.Usage())
	rec.Duration = time.Since(start)
	if !firstToken.IsZero() {
		rec.TimeToFirstToken = firstToken.Sub(start)
	}
	d.record(r.Context(), rec, obs)
}

// envelope is what the gateway reads out of a request body before it decides
// anything: the name the client asked for, and whether it wants a stream.
type envelope struct {
	Model  string
	Stream bool
	// IncludeUsage records whether a Chat Completions client asked for the
	// usage chunk itself, so the gateway knows whether the one it asked for on
	// its own account is the client's to receive.
	IncludeUsage bool
}

// readEnvelope reads the two fields every surface routes on, and says what is
// wrong with them in a sentence a client is shown when either is malformed.
func readEnvelope(fields map[string]json.RawMessage) (envelope, string) {
	var e envelope
	if raw, ok := fields["model"]; ok {
		if err := json.Unmarshal(raw, &e.Model); err != nil {
			return envelope{}, "The request's model is not a string."
		}
	}
	if raw, ok := fields["stream"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &e.Stream); err != nil {
			return envelope{}, "The request's stream flag is not true or false."
		}
	}
	return e, ""
}

// setUsage copies a provider's token accounting onto the audit record.
//
// One function rather than field assignments at each call site, so a class
// added to Usage cannot be carried on the streaming path and dropped on the
// other - which would show up only as a quietly cheaper bill.
func setUsage(rec *store.RequestRecord, u providers.Usage) {
	rec.InputTokens = u.InputTokens
	rec.OutputTokens = u.OutputTokens
	rec.CacheReadTokens = u.CacheReadTokens
	rec.CacheWriteTokens = u.CacheWriteTokens
	rec.CacheWrite1hTokens = u.CacheWrite1hTokens
}

// writeSSE emits one server-sent event.
//
// data is relayed byte for byte. That is the whole contract of a streaming
// proxy: re-encoding it would cost latency proportional to the payload and
// would silently drop any field this build does not model. The response is
// text/event-stream consumed by an API client, never HTML rendered in a
// browser, and the only two producers are the upstream provider's own payload
// and this package's own error envelope.
func writeSSE(w http.ResponseWriter, event string, data []byte) error {
	if event != "" {
		if _, err := io.WriteString(w, "event: "+event+"\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil { // #nosec G705 -- provider payload relayed verbatim into an event stream, not markup
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}

// eventName reads the SSE event name out of an event payload.
//
// Anthropic's stream and the Responses stream both name each event after its
// `type`, and clients dispatch on the name, so it has to be reproduced rather
// than omitted. This is a scan for one top-level key, not a parse of the
// payload.
func eventName(payload []byte) string {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	return envelope.Type
}

// providerFailure maps a provider error onto the client-facing response.
//
// Named in the Anthropic error vocabulary, which is this package's own; a
// surface speaking another API translates the type when it writes the error.
func providerFailure(err error) (status int, errType, kind, message string) {
	pe, ok := providers.AsError(err)
	if !ok {
		return http.StatusBadGateway, errTypeAPI, "upstream",
			"Panorama could not reach the model provider."
	}

	switch pe.Kind {
	case providers.ErrKindThrottled:
		return http.StatusTooManyRequests, errTypeRateLimit, string(pe.Kind),
			"The model is rate limited right now. Retry shortly."
	case providers.ErrKindUnavailable:
		return http.StatusServiceUnavailable, errTypeOverloaded, string(pe.Kind),
			"The model is temporarily unavailable. Retry shortly."
	case providers.ErrKindInvalidRequest:
		return http.StatusBadRequest, errTypeInvalidRequest, string(pe.Kind),
			"The model provider rejected this request."
	case providers.ErrKindNotFound:
		return http.StatusBadGateway, errTypeAPI, string(pe.Kind),
			"That model is not available to this Panorama deployment. Ask an administrator to enable it."
	case providers.ErrKindForbidden:
		return http.StatusBadGateway, errTypeAPI, string(pe.Kind),
			"Panorama is not permitted to invoke that model. Ask an administrator to check the deployment's model access."
	default:
		return pe.StatusCode, errTypeAPI, string(pe.Kind),
			"The model provider returned an error."
	}
}

// logProviderFailure records the underlying failure server-side, where the
// provider's own message is safe to keep.
func (d *DataPlane) logProviderFailure(
	r *http.Request, requestID string, target providers.ModelRef, err error, extra ...slog.Attr,
) {
	attrs := append(logAttrs(r.Context()),
		slog.String("request_id", requestID),
		slog.String("model", target.Model),
		slog.String("provider", string(target.Provider)),
		slog.String("error", err.Error()),
	)
	if pe, ok := providers.AsError(err); ok {
		attrs = append(attrs, slog.String("kind", string(pe.Kind)))
	}
	attrs = append(attrs, extra...)
	d.log.LogAttrs(r.Context(), slog.LevelError, "provider invocation failed", attrs...)
}

// betasFrom reads the beta feature flags a client asked for.
//
// The first-party Anthropic API takes these as a comma-separated
// anthropic-beta header, and clients send them that way. Providers that expect
// them elsewhere translate; this function just parses the header faithfully so
// nothing is silently dropped.
func betasFrom(h http.Header) []string {
	raw := h.Values("anthropic-beta")
	if len(raw) == 0 {
		return nil
	}

	var out []string
	for _, header := range raw {
		for _, beta := range strings.Split(header, ",") {
			if beta = strings.TrimSpace(beta); beta != "" {
				out = append(out, beta)
			}
		}
	}
	return out
}

// clientIP reports the peer address. It deliberately does not trust
// X-Forwarded-For: that header is client-supplied, and honouring it without
// knowing which proxies are in front of the gateway lets any caller forge the
// address recorded in the audit trail. Trusted-proxy handling arrives with the
// deployment work in M7.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
