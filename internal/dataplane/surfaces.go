// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package dataplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/providers"
)

// Surfaces the audit trail records. `anthropic` is the value every row written
// before the OpenAI surfaces existed carries, so it keeps its name rather than
// becoming the API's.
const (
	SurfaceAnthropic       = "anthropic"
	SurfaceOpenAIChat      = "openai-chat"
	SurfaceOpenAIResponses = "openai-responses"
)

// surface is one client-facing API: how a request of that shape is prepared,
// and how a response, a stream and an error are framed for the client.
//
// It decides nothing. Who may ask for what, which model it becomes and which
// provider serves it are the engine's, once, for every surface alike.
type surface struct {
	// name is recorded on the request row.
	name string
	// api is the shape, which the engine checks a route's model speaks.
	api providers.API
	// shape tells the extractor where the conversation's opening is.
	shape attribution.Shape

	// prepare applies this surface's own top-level rules to the body, and
	// returns a refusal for a request it will not serve. It runs after the
	// engine admits the request, so a refusal here names a parameter rather
	// than a route.
	prepare func(fields map[string]json.RawMessage, e *envelope) string
	// withhold reports that an event was asked for by the gateway rather than
	// the client, and is not relayed. Nil relays everything.
	withhold func(payload []byte, e envelope) bool
	// eventName names an event on the wire, or returns empty for a stream
	// whose events are unnamed.
	eventName func(payload []byte) string
	// end writes whatever marks the end of a clean stream. Nil writes nothing.
	end func(w http.ResponseWriter) error

	writeError       func(w http.ResponseWriter, status int, errType, message, requestID string)
	writeStreamError func(w http.ResponseWriter, errType, message, requestID string) error
}

// anthropicSurface is the Anthropic Messages API, at /v1/messages: Claude Code,
// and opencode's Anthropic provider. It is relayed as it arrives, with nothing
// rewritten at this layer - the provider translates the dialect it speaks.
var anthropicSurface = &surface{
	name:             SurfaceAnthropic,
	api:              providers.APIAnthropicMessages,
	shape:            attribution.ShapeMessages,
	prepare:          func(map[string]json.RawMessage, *envelope) string { return "" },
	eventName:        eventName,
	writeError:       writeError,
	writeStreamError: writeStreamError,
}

// chatSurface is OpenAI Chat Completions, at /v1/chat/completions: opencode's
// OpenAI-compatible provider, Cline, Continue.
var chatSurface = &surface{
	name:             SurfaceOpenAIChat,
	api:              providers.APIOpenAIChat,
	shape:            attribution.ShapeChat,
	prepare:          prepareChat,
	withhold:         withholdUsageChunk,
	eventName:        func([]byte) string { return "" },
	end:              writeDone,
	writeError:       writeOpenAIError,
	writeStreamError: writeChatStreamError,
}

// responsesSurface is OpenAI Responses, at /v1/responses: Codex CLI, which
// speaks nothing else.
var responsesSurface = &surface{
	name:             SurfaceOpenAIResponses,
	api:              providers.APIOpenAIResponses,
	shape:            attribution.ShapeResponses,
	prepare:          prepareResponses,
	eventName:        eventName,
	writeError:       writeOpenAIError,
	writeStreamError: writeResponsesStreamError,
}

// prepareChat asks for usage on every stream, and refuses a service tier.
//
// Chat Completions reports a stream's usage only when asked, in one chunk near
// the end. A stream without it is a request the gateway cannot meter, and an
// unmetered request is spend nobody can see - so the gateway asks on the
// client's behalf, and withholds the chunk from a client that did not.
func prepareChat(fields map[string]json.RawMessage, e *envelope) string {
	if msg := refuseServiceTier(fields); msg != "" {
		return msg
	}
	if !e.Stream {
		return ""
	}

	options := map[string]json.RawMessage{}
	if raw, ok := fields["stream_options"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &options); err != nil {
			return "stream_options must be an object."
		}
	}
	if raw, ok := options["include_usage"]; ok {
		var asked bool
		if err := json.Unmarshal(raw, &asked); err == nil && asked {
			e.IncludeUsage = true
		}
	}
	options["include_usage"] = json.RawMessage("true")
	encoded, err := json.Marshal(options)
	if err != nil {
		return "stream_options could not be encoded."
	}
	fields["stream_options"] = encoded
	return ""
}

// prepareResponses keeps a Responses request stateless, and refuses what would
// make it otherwise.
//
// The Responses API stores every response for thirty days unless told not to,
// server side, where a later request can continue it by id. That is a body -
// the prompt and the answer - kept by the provider on this deployment's account,
// which is the one thing this product promises never happens, and nothing in the
// gateway could see or delete it. So `store` is forced off when a client leaves
// it unset, and a client asking for storage, or to continue a stored response,
// or to run one in the background, is refused with the reason.
func prepareResponses(fields map[string]json.RawMessage, e *envelope) string {
	if msg := refuseServiceTier(fields); msg != "" {
		return msg
	}
	if raw, ok := fields["store"]; ok && string(raw) != "null" {
		var store bool
		if err := json.Unmarshal(raw, &store); err != nil {
			return "store must be true or false."
		}
		if store {
			return "Panorama does not store responses: a stored response is a prompt and its answer kept by " +
				"the provider, and this gateway keeps no request or response body anywhere. Send store: false, " +
				"and resend the conversation with each turn."
		}
	} else {
		fields["store"] = json.RawMessage("false")
	}
	if raw, ok := fields["previous_response_id"]; ok && string(raw) != "null" && string(raw) != `""` {
		return "Panorama does not continue stored responses, so previous_response_id cannot be used. " +
			"Resend the conversation with each turn."
	}
	if raw, ok := fields["background"]; ok && string(raw) == "true" {
		return "Panorama does not run responses in the background. Send the request without background."
	}
	return ""
}

// refuseServiceTier refuses any tier but the default.
//
// Priority is billed at 1.75 times the standard rate and flex at half, and the
// price table holds the standard rate - so a request at another tier would be
// recorded at a cost it did not have. Refused rather than rewritten to the
// default, because a client that asked for priority and silently got standard
// latency is a client somebody spends a week debugging.
func refuseServiceTier(fields map[string]json.RawMessage) string {
	raw, ok := fields["service_tier"]
	if !ok || string(raw) == "null" {
		return ""
	}
	var tier string
	if err := json.Unmarshal(raw, &tier); err != nil {
		return "service_tier must be a string."
	}
	if tier == "" || tier == "default" {
		return ""
	}
	return "Panorama serves the default service tier only, and prices requests at its rate. " +
		"Remove service_tier (it was " + tier + ")."
}

// withholdUsageChunk reports the usage-only chunk the gateway asked for on a
// client's behalf.
//
// A chunk with no choices and a usage object is what include_usage adds; a
// client that did not ask for it may not expect a chunk with nothing to render.
func withholdUsageChunk(payload []byte, e envelope) bool {
	if e.IncludeUsage || !bytes.Contains(payload, []byte(`"usage"`)) {
		return false
	}
	var chunk struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return false
	}
	return len(chunk.Choices) == 0 && len(chunk.Usage) > 0 && string(chunk.Usage) != "null"
}

// writeDone ends a Chat Completions stream the way OpenAI does.
func writeDone(w http.ResponseWriter) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// openAIError is the OpenAI error envelope.
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
	// RequestID is not part of OpenAI's schema. Additive and ignored by
	// clients, and what turns "it failed" into a row an operator can find.
	RequestID string `json:"request_id,omitempty"`
}

// openAIErrorFor translates this package's Anthropic-named error types into
// the type and code an OpenAI client reads.
func openAIErrorFor(errType, message, requestID string) openAIErrorBody {
	code := func(s string) *string { return &s }
	body := openAIErrorBody{Message: message, RequestID: requestID}
	switch errType {
	case errTypeAuthentication:
		body.Type, body.Code = "invalid_request_error", code("invalid_api_key")
	case errTypePermission:
		body.Type, body.Code = "permission_error", code("forbidden")
	case errTypeNotFound:
		body.Type, body.Code = "invalid_request_error", code("model_not_found")
	case errTypeRateLimit:
		body.Type, body.Code = "rate_limit_error", code("rate_limit_exceeded")
	case errTypeOverloaded:
		body.Type, body.Code = "server_error", code("overloaded")
	case errTypeRequestTooLarge:
		body.Type, body.Code = "invalid_request_error", code("request_too_large")
	case errTypeInvalidRequest:
		body.Type = "invalid_request_error"
	default:
		body.Type = "server_error"
	}
	return body
}

// writeOpenAIError sends an OpenAI-shaped error.
func writeOpenAIError(w http.ResponseWriter, status int, errType, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{Error: openAIErrorFor(errType, message, requestID)})
}

// writeChatStreamError reports a failure inside a started Chat Completions
// stream, as the error object OpenAI streams carry in a data line.
func writeChatStreamError(w http.ResponseWriter, errType, message, requestID string) error {
	payload, err := json.Marshal(openAIError{Error: openAIErrorFor(errType, message, requestID)})
	if err != nil {
		return err
	}
	return writeSSE(w, "", payload)
}

// writeResponsesStreamError reports a failure inside a started Responses
// stream, as the `error` event that API defines.
func writeResponsesStreamError(w http.ResponseWriter, errType, message, requestID string) error {
	body := openAIErrorFor(errType, message, requestID)
	event := map[string]any{
		"type":       "error",
		"message":    body.Message,
		"code":       body.Code,
		"param":      nil,
		"request_id": requestID,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return writeSSE(w, "error", payload)
}

// unauthenticatedMessage is what a surface's client is told when no valid token
// was sent, naming the variable that tool reads.
func (sf *surface) unauthenticatedMessage() string {
	if sf.name == SurfaceMCP {
		return "No valid Panorama gateway token was supplied. The MCP server entry for Panorama sends it " +
			"as an Authorization header; copy the entry again from My setup in the Panorama console."
	}
	switch sf.api {
	case providers.APIOpenAIChat, providers.APIOpenAIResponses:
		return "No valid Panorama gateway token was supplied. Set your tool's API key - " +
			"OPENAI_API_KEY, or the variable its provider configuration reads - to the token from your Panorama dashboard."
	default:
		return "No valid Panorama gateway token was supplied. Set ANTHROPIC_AUTH_TOKEN " +
			"to the token from your Panorama dashboard."
	}
}
