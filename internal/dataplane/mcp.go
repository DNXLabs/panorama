// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/controlplane"
	"github.com/dnxlabs/panorama/internal/costcenters"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/tools"
	"github.com/dnxlabs/panorama/internal/version"
	"github.com/dnxlabs/panorama/internal/websearch"
)

// SurfaceMCP is recorded on the audit row of a tool call made at /mcp.
const SurfaceMCP = "mcp"

// The model name a search is recorded under, so it reads as what it is in
// every breakdown by model: the audit trail, the spend chart, the export.
const (
	webSearchTool  = "web_search"
	webSearchModel = "web-search"
)

// mcpProtocolVersions are the MCP revisions this endpoint speaks, newest
// first. It uses nothing a later revision changed: one JSON-RPC message per
// POST, a JSON reply, no session and no server-initiated stream.
var mcpProtocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// maxMCPBody bounds a tool call. A search's arguments are a query and two
// domain lists; a megabyte is generous.
const maxMCPBody = 1 << 20

// mcpSurface frames the endpoint's failures before a JSON-RPC exchange has
// started: authentication, chiefly. It shares nothing else with the model
// surfaces.
var mcpSurface = &surface{name: SurfaceMCP, writeError: writeMCPHTTPError}

// JSON-RPC 2.0 error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// notification reports a message with no id, which gets no response.
func (r rpcRequest) notification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleMCP serves the Model Context Protocol's Streamable HTTP transport, as
// far as a stateless tool server needs it.
//
// The endpoint offers tools the gateway runs itself - today, web search - to
// every coding tool, whatever model it is pointed at. It is authenticated like
// the model surfaces, with the same gateway token, so a search is somebody's,
// billed to their budgets and recorded beside their requests.
//
// Only POST is served. GET would open a stream for messages the server sends
// unprompted, and this server sends none; the transport says a server
// answers 405 to that. No session id is issued, because nothing is held
// between calls.
func (d *DataPlane) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Panorama's MCP endpoint answers POST only.", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpProtocolVersions, v) {
		writeMCPHTTPError(w, http.StatusBadRequest, "",
			fmt.Sprintf("MCP protocol version %s is not supported; this server speaks %s.", v, strings.Join(mcpProtocolVersions, ", ")),
			controlplane.RequestIDFrom(r.Context()))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		writeRPC(w, nil, nil, &rpcErrorBody{Code: rpcInvalidRequest, Message: "The request body could not be read."})
		return
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		// Batching was in one revision of the protocol and removed in the
		// next; no client this endpoint is configured for sends one.
		writeRPC(w, nil, nil, &rpcErrorBody{Code: rpcInvalidRequest, Message: "Batched JSON-RPC requests are not supported."})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcErrorBody{Code: rpcParseError, Message: "The request body is not JSON-RPC."})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, nil, &rpcErrorBody{Code: rpcInvalidRequest, Message: "Expected a JSON-RPC 2.0 request with a method."})
		return
	}
	if req.notification() {
		// notifications/initialized, notifications/cancelled: nothing to
		// answer, and nothing held that a cancellation could stop.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		writeRPC(w, req.ID, d.mcpInitialize(req.Params), nil)
	case "ping":
		writeRPC(w, req.ID, struct{}{}, nil)
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": d.mcpTools()}, nil)
	case "tools/call":
		d.mcpCall(w, r, req)
	default:
		writeRPC(w, req.ID, nil, &rpcErrorBody{Code: rpcMethodNotFound, Message: "Method not found: " + req.Method})
	}
}

// mcpInitialize answers the handshake with the client's revision when this
// server speaks it, and its own newest otherwise, which is what the protocol
// asks of a server.
func (d *DataPlane) mcpInitialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	negotiated := mcpProtocolVersions[0]
	if slices.Contains(mcpProtocolVersions, p.ProtocolVersion) {
		negotiated = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": negotiated,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "panorama", "title": "Panorama", "version": version.Version},
		"instructions": "Tools run by this organization's Panorama gateway. web_search searches the web; " +
			"use it for anything that may have changed since your training data, such as library versions, " +
			"API documentation, error messages and release notes.",
	}
}

// mcpTools lists what may be called. Web search is listed only while it is
// switched on and working, so a tool that is off is not one a model can try.
func (d *DataPlane) mcpTools() []map[string]any {
	if !d.webSearch.Current().Serving() {
		return []map[string]any{}
	}
	return []map[string]any{webSearchDefinition()}
}

func webSearchDefinition() map[string]any {
	domains := func(description string) map[string]any {
		return map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
			"maxItems": websearch.MaxDomains, "description": description,
		}
	}
	return map[string]any{
		"name":  webSearchTool,
		"title": "Web search",
		"description": "Search the web and return the matching pages' titles, URLs, publication dates and " +
			"extracts. Use it for anything that may have changed since your training data: library and API " +
			"documentation, versions, release notes, error messages, current events. Search for key terms " +
			"rather than a sentence; a query is limited to 200 characters. Cite the URLs you use.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type": "string", "maxLength": websearch.MaxQueryChars,
					"description": "What to search for.",
				},
				"max_results": map[string]any{
					"type": "integer", "minimum": 1, "maximum": websearch.MaxResults,
					"description": fmt.Sprintf("How many results to return. Defaults to %d.", defaultSearchResults),
				},
				"allowed_domains": domains("Only return results from these domains. A domain matches its subdomains."),
				"blocked_domains": domains("Never return results from these domains."),
				"published_after": map[string]any{
					"type": "string", "description": "Only results published on or after this date, YYYY-MM-DD.",
				},
				"published_before": map[string]any{
					"type": "string", "description": "Only results published on or before this date, YYYY-MM-DD.",
				},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		"annotations": map[string]any{
			"title":           "Web search",
			"readOnlyHint":    true,
			"destructiveHint": false,
			"idempotentHint":  true,
			"openWorldHint":   true,
		},
	}
}

// defaultSearchResults is fewer than the backend's default of ten. A result
// is an extract of a page, and five of them answer most questions a coding
// agent asks for about a sixth of what ten cost in context.
const defaultSearchResults = 5

type searchArguments struct {
	Query           string   `json:"query"`
	MaxResults      *int     `json:"max_results"`
	AllowedDomains  []string `json:"allowed_domains"`
	BlockedDomains  []string `json:"blocked_domains"`
	PublishedAfter  string   `json:"published_after"`
	PublishedBefore string   `json:"published_before"`
}

// mcpCall runs a tools/call, and records it.
//
// Every outcome that is about the search - a query the backend refuses, a
// budget that stops it, a backend that fails - is a tool result with isError
// set, not a protocol error, because that is what reaches the model: it can
// rephrase, or carry on without searching. Only a name that is not a tool is
// a protocol error.
func (d *DataPlane) mcpCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	start := time.Now()
	ctx := r.Context()
	requestID := controlplane.RequestIDFrom(ctx)

	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPC(w, req.ID, nil, &rpcErrorBody{Code: rpcInvalidParams, Message: "tools/call needs a name and arguments."})
		return
	}
	if params.Name != webSearchTool {
		writeRPC(w, req.ID, nil, &rpcErrorBody{Code: rpcInvalidParams, Message: "Unknown tool: " + params.Name})
		return
	}

	state := d.webSearch.Current()
	rec := d.toolRecord(r, start, state)

	finish := func(decision, errorKind string, message string) {
		rec.Decision = decision
		rec.ErrorKind = errorKind
		rec.Duration = time.Since(start)
		d.record(ctx, rec, nil)
		writeRPC(w, req.ID, toolResult(message, true), nil)
	}

	if !state.Serving() {
		// Listed nowhere while off, so this is a client holding a tool list
		// from before an administrator switched search off.
		rec.StatusCode = http.StatusOK
		finish(store.DecisionRefused, "web_search_off",
			"Web search is switched off on this Panorama deployment. Continue without it.")
		return
	}

	query, problem := parseSearchArguments(params.Arguments)
	if problem != "" {
		rec.StatusCode = http.StatusOK
		finish(store.DecisionRefused, "invalid_arguments", problem)
		return
	}

	decision := d.policy.EvaluateTool(policy.Input{
		UserID:          rec.UserID,
		TeamID:          rec.TeamID,
		Tool:            rec.Tool,
		Surface:         SurfaceMCP,
		CostCenterStamp: attribution.CleanHeader(r.Header.Get(costcenters.Header)),
		Now:             start,
	})
	rec.BudgetState = string(decision.Budget)
	rec.CostCenterID = decision.CostCenterID
	rec.CostCenterStamp = decision.CostCenterStamp
	if decision.Blocked {
		rec.StatusCode = http.StatusOK
		d.logTool(ctx, slog.LevelInfo, "web search blocked by a budget", requestID,
			"reason", decision.BlockReason, "budget", decision.BudgetID)
		finish(store.DecisionBlocked, decision.BlockReason, decision.BlockMessage)
		return
	}

	rec.Attempts = 1
	results, err := state.Backend.Search(ctx, query)
	if err != nil {
		kind := websearch.KindOf(err)
		level := slog.LevelWarn
		if kind == websearch.ErrKindForbidden || kind == websearch.ErrKindMisconfigured {
			// The deployment's fault, and every search will meet it until
			// somebody fixes the role or the gateway.
			level = slog.LevelError
		}
		if errors.Is(err, context.Canceled) {
			level = slog.LevelInfo
		}
		d.logTool(ctx, level, "web search failed", requestID, "error_kind", string(kind), "error", err)
		rec.StatusCode = http.StatusOK
		finish(store.DecisionError, "web_search_"+string(kind), searchFailureMessage(err, requestID))
		return
	}

	price, source := state.Config.Price, state.Config.PriceSource
	rec.StatusCode = http.StatusOK
	rec.WebSearches = 1
	rec.Cost = &pricing.Cost{WebSearch: price, Total: price}
	rec.CostSource = source
	rec.Duration = time.Since(start)
	d.record(ctx, rec, nil)

	text := formatResults(query.Text, results)
	if decision.Warning != "" {
		text = decision.Warning + "\n\n" + text
	}
	writeRPC(w, req.ID, toolResult(text, false), nil)
}

// toolRecord starts the audit row for a tool call, with who made it.
func (d *DataPlane) toolRecord(r *http.Request, start time.Time, state *websearch.State) store.RequestRecord {
	rec := store.RequestRecord{
		RequestID:      controlplane.RequestIDFrom(r.Context()),
		StartedAt:      start.UTC(),
		Surface:        SurfaceMCP,
		Provider:       string(state.Config.Kind),
		RequestedModel: webSearchTool,
		ResolvedModel:  webSearchModel,
		Tool:           tools.FromUserAgent(r.UserAgent()),
		UserAgent:      r.UserAgent(),
		ClientIP:       clientIP(r),
		Decision:       store.DecisionAllowed,
		// No client sends its conversation on an MCP call, so the search
		// joins the one this person's tool was just in.
		SessionFromSitting: true,
	}
	if rec.Provider == "" {
		rec.Provider = string(websearch.KindAgentCore)
	}
	if p := PrincipalFrom(r.Context()); p != nil {
		rec.UserID = p.UserID
		if p.IsService() {
			rec.TeamID = p.TeamID
			rec.ServiceTokenID = p.TokenID
		} else {
			rec.TokenID = p.TokenID
		}
	}
	return rec
}

func (d *DataPlane) logTool(ctx context.Context, level slog.Level, msg, requestID string, args ...any) {
	attrs := append([]slog.Attr{
		slog.String("request_id", requestID),
		slog.String("tool_call", webSearchTool),
	}, logAttrs(ctx)...)
	for i := 0; i+1 < len(args); i += 2 {
		if key, ok := args[i].(string); ok {
			attrs = append(attrs, slog.Any(key, args[i+1]))
		}
	}
	d.log.LogAttrs(ctx, level, msg, attrs...)
}

// parseSearchArguments reads and checks a search's arguments, and returns a
// message for the model when they are wrong.
func parseSearchArguments(raw json.RawMessage) (websearch.Query, string) {
	var args searchArguments
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(bytes.TrimSpace(raw)) == 0 || dec.Decode(&args) != nil {
		return websearch.Query{}, "The arguments must be an object with a query string, and optionally " +
			"max_results, allowed_domains, blocked_domains, published_after and published_before."
	}
	q := websearch.Query{
		Text:       strings.TrimSpace(args.Query),
		MaxResults: defaultSearchResults,
		Include:    cleanDomains(args.AllowedDomains),
		Exclude:    cleanDomains(args.BlockedDomains),
	}
	if args.MaxResults != nil {
		if *args.MaxResults < 1 {
			return websearch.Query{}, fmt.Sprintf("max_results must be between 1 and %d.", websearch.MaxResults)
		}
		q.MaxResults = *args.MaxResults
	}
	for _, bound := range []struct {
		name string
		text string
		dst  *time.Time
	}{
		{"published_after", args.PublishedAfter, &q.PublishedAfter},
		{"published_before", args.PublishedBefore, &q.PublishedBefore},
	} {
		if bound.text == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", strings.TrimSpace(bound.text))
		if err != nil {
			return websearch.Query{}, bound.name + " must be a date written YYYY-MM-DD."
		}
		*bound.dst = t
	}
	if !q.PublishedBefore.IsZero() {
		// Inclusive of the day named, as the description says.
		q.PublishedBefore = q.PublishedBefore.Add(24*time.Hour - time.Second)
	}
	if err := q.Validate(); err != nil {
		return websearch.Query{}, capitalise(err.Error()) + "."
	}
	return q, ""
}

// cleanDomains trims, lowercases and de-duplicates a domain list, and strips
// a scheme or path a model added to what should be a bare domain.
func cleanDomains(in []string) []string {
	var out []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		if i := strings.IndexByte(d, '/'); i >= 0 {
			d = d[:i]
		}
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// searchFailureMessage is what the model is told when a search fails. The
// backend's own words reach it only for a query the backend refused, which
// is the one failure it can act on; the rest are the deployment's, and name
// the request id an administrator will look for.
func searchFailureMessage(err error, requestID string) string {
	switch websearch.KindOf(err) {
	case websearch.ErrKindRejected:
		var e *websearch.Error
		if errors.As(err, &e) && e.Message != "" {
			return "The search was refused: " + e.Message
		}
		return "The search was refused. Try a shorter or simpler query."
	case websearch.ErrKindThrottled:
		return "Web search is being rate limited. Wait a moment and try again, or continue without it."
	default:
		return fmt.Sprintf("Web search is unavailable on this Panorama deployment right now (request %s). "+
			"Continue without it; the administrator can find the cause in Panorama's log.", requestID)
	}
}

// formatResults renders results as text for the model: numbered, with the
// URL on its own line so it can be cited.
func formatResults(query string, results []websearch.Result) string {
	if len(results) == 0 {
		return fmt.Sprintf("No results for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d results for %q:\n", len(results), query)
	for i, r := range results {
		title := r.Title
		if title == "" {
			title = r.URL
		}
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, title, r.URL)
		if r.Published != "" {
			fmt.Fprintf(&b, "   Published %s\n", r.Published)
		}
		// The index separates the passages it extracted with blank lines;
		// they are kept as lines, indented under their result, and the blank
		// ones dropped so nothing but the extract costs context.
		for _, line := range strings.Split(r.Snippet, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fmt.Fprintf(&b, "   %s\n", line)
			}
		}
	}
	return b.String()
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// writeRPC sends one JSON-RPC response.
func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcErrorBody) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeMCPHTTPError is a failure outside a JSON-RPC exchange - authentication,
// chiefly - which a client reads from the status code. The body is a JSON-RPC
// error so a client that reads it anyway gets the message.
func writeMCPHTTPError(w http.ResponseWriter, status int, _ string, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if requestID != "" {
		message += " (request " + requestID + ")"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": nil,
		"error": rpcErrorBody{Code: rpcInvalidRequest, Message: message},
	})
}
