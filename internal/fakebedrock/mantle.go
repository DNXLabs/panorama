// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package fakebedrock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// bedrock-mantle's wire protocol, served by the same Server as the runtime
// endpoints.
//
// Mantle has no SDK client, so the provider speaks plain HTTP signed with SigV4
// and reads AWS_ENDPOINT_URL_BEDROCK_MANTLE itself. Pointing that at this server
// runs the whole of the Mantle provider - signing, the three request paths,
// server-sent events in three shapes, the error mapping - against something
// that answers the way Mantle does.
//
//	POST /anthropic/v1/messages          Claude, over the Anthropic Messages API
//	POST /v1/chat/completions            Chat Completions, most families
//	POST /openai/v1/chat/completions     Chat Completions, GPT-5.x and later
//	POST /v1/responses                   Responses, gpt-oss
//	POST /openai/v1/responses            Responses, GPT-5.x and later
//	GET  /v1/models                      the catalogue
//
// A model sent to the base path its family is not served at is refused as not
// found, which is what Mantle does and the one mistake that is easiest to make.

// EndpointMantle labels a request that arrived on a Mantle path.
const EndpointMantle = "bedrock-mantle"

// Mantle model ids the default catalogue lists.
const (
	MantleSonnet5   = "anthropic.claude-sonnet-5"
	MantleHaiku45   = "anthropic.claude-haiku-4-5"
	MantleGPTOSS    = "openai.gpt-oss-120b"
	MantleQwenCoder = "qwen.qwen3-coder-480b-a35b-instruct"
	MantleGPT55     = "openai.gpt-5.5"
	// MantleUnknown is a model no table in this build has heard of, so it is
	// listed and speaks nothing: browse-only.
	MantleUnknown = "acme.mystery-1"
	// MantleUnavailable is listed with a status saying it cannot be invoked.
	MantleUnavailable = "anthropic.claude-mythos-5"
)

// MantleModel is one row of Mantle's /v1/models, in the wire's field names.
type MantleModel struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	Status        string `json:"status,omitempty"`
	StatusReason  string `json:"status_reason,omitempty"`
	DataRetention *struct {
		AllowedModes []string `json:"allowed_modes"`
	} `json:"data_retention,omitempty"`
}

// DefaultMantleModels is the catalogue /v1/models reports.
func DefaultMantleModels() []MantleModel {
	model := func(id, owner string) MantleModel {
		m := MantleModel{ID: id, Object: "model", OwnedBy: owner}
		m.DataRetention = &struct {
			AllowedModes []string `json:"allowed_modes"`
		}{AllowedModes: []string{"none"}}
		return m
	}
	unavailable := model(MantleUnavailable, "anthropic")
	unavailable.Status = "unavailable"
	unavailable.StatusReason = "requires the aws_review data retention mode"
	return []MantleModel{
		model(MantleSonnet5, "anthropic"),
		model(MantleHaiku45, "anthropic"),
		model(MantleGPTOSS, "openai"),
		model(MantleQwenCoder, "qwen"),
		model(MantleGPT55, "openai"),
		model(MantleUnknown, "acme"),
		unavailable,
	}
}

// mantleOpenAIBase is where this fake serves a family's OpenAI-shaped paths,
// matching the provider's own table on purpose: the test is that the provider
// sends a model where Mantle serves it.
func mantleOpenAIBase(model string) string {
	for _, prefix := range []string{"openai.gpt-5", "openai.gpt-6", "google.gemma-4-", "xai.grok-"} {
		if strings.HasPrefix(model, prefix) {
			return "/openai/v1"
		}
	}
	return "/v1"
}

// mantleCompletionTokens reports a family of OpenAI's reasoning models, which
// refuse max_tokens and reasoning with function tools on Chat Completions -
// matching the provider's own table on purpose: the test is that the provider
// spells the cap the way the model reads it, and that a tool is sent the API
// that works.
func mantleCompletionTokens(model string) bool {
	return strings.HasPrefix(model, "openai.gpt-5") || strings.HasPrefix(model, "openai.gpt-6")
}

// hasTools reports a tools array with something in it.
func hasTools(raw json.RawMessage) bool {
	var tools []json.RawMessage
	return json.Unmarshal(raw, &tools) == nil && len(tools) > 0
}

// handleMantle serves the Mantle paths. It reports whether the path was one.
func (s *Server) handleMantle(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.EscapedPath()

	if path == "/v1/models" && r.Method == http.MethodGet {
		s.recordSignature(r)
		writeJSON(w, map[string]any{"object": "list", "data": s.mantleCatalogue()})
		return true
	}

	var shape string
	switch path {
	case "/anthropic/v1/messages":
		shape = "anthropic"
	case "/v1/chat/completions", "/openai/v1/chat/completions":
		shape = "chat"
	case "/v1/responses", "/openai/v1/responses":
		shape = "responses"
	default:
		return false
	}
	if r.Method != http.MethodPost {
		writeMantleError(w, http.StatusMethodNotAllowed, "invalid_request_error", "fakebedrock: invocations are POST")
		return true
	}

	body, err := readBody(r)
	if err != nil {
		writeMantleError(w, http.StatusBadRequest, "invalid_request_error", "fakebedrock: "+err.Error())
		return true
	}
	var envelope struct {
		Model         string          `json:"model"`
		Stream        bool            `json:"stream"`
		MaxTokens     json.RawMessage `json:"max_tokens"`
		Tools         json.RawMessage `json:"tools"`
		Reasoning     string          `json:"reasoning_effort"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeMantleError(w, http.StatusBadRequest, "invalid_request_error", "fakebedrock: "+err.Error())
		return true
	}

	service, region := signatureScope(r.Header.Get("Authorization"))
	received := Received{
		Model: envelope.Model, Streaming: envelope.Stream, Body: body, At: time.Now().UTC(),
		Endpoint: EndpointMantle, Path: path,
		SigningService: service, SigningRegion: region,
		AnthropicVersion: r.Header.Get("anthropic-version"),
	}
	if betas := r.Header.Get("anthropic-beta"); betas != "" {
		received.Betas = strings.Split(betas, ",")
	}
	fault := s.record(received)

	// A model sent to a base path its family is not served at is not found,
	// exactly as Mantle answers it.
	if shape != "anthropic" && !strings.HasPrefix(path, mantleOpenAIBase(envelope.Model)+"/") {
		writeMantleError(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("fakebedrock: %s is not served at %s", envelope.Model, path))
		return true
	}

	// GPT-5.x and later read a Chat Completions cap as max_completion_tokens
	// and refuse the old name, in Mantle's own words.
	if shape == "chat" && mantleCompletionTokens(envelope.Model) &&
		len(envelope.MaxTokens) > 0 && string(envelope.MaxTokens) != "null" {
		writeMantleError(w, http.StatusBadRequest, "invalid_request_error",
			"Unsupported parameter: 'max_tokens' is not supported with this model.")
		return true
	}
	// And they take function tools on Chat Completions only with reasoning
	// switched off, which a coding agent never asks for - so this is what a
	// GPT route sent Chat Completions by a tool meets.
	if shape == "chat" && mantleCompletionTokens(envelope.Model) && hasTools(envelope.Tools) &&
		envelope.Reasoning != "none" {
		writeMantleError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
			"Function tools with reasoning_effort are not supported for %s in /v1/chat/completions. "+
				"To use function tools, use /v1/responses or set reasoning_effort to 'none'.",
			strings.TrimPrefix(envelope.Model, "openai.")))
		return true
	}

	if fault != "" && fault != FaultMidStream {
		status, errType := mantleFault(fault)
		writeMantleError(w, status, errType, "fakebedrock: "+string(fault))
		return true
	}

	usage, text := s.currentUsage(), s.currentText()
	if !envelope.Stream {
		switch shape {
		case "anthropic":
			s.writeCompletion(w)
		case "chat":
			writeJSON(w, chatCompletion(usage, text))
		case "responses":
			writeJSON(w, responseObject(usage, text))
		}
		return true
	}

	includeUsage := envelope.StreamOptions != nil && envelope.StreamOptions.IncludeUsage
	s.noteConnection(r.RemoteAddr)
	s.writeMantleStream(w, r, shape, usage, text, includeUsage, fault == FaultMidStream)
	return true
}

// writeMantleStream answers a streaming invocation with server-sent events.
func (s *Server) writeMantleStream(w http.ResponseWriter, r *http.Request, shape string, u Usage, text string, includeUsage, failMidway bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	delay := s.currentDelay()

	type event struct {
		name string
		data []byte
	}
	var events []event
	switch shape {
	case "anthropic":
		for _, e := range anthropicEvents(u, text) {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(e, &head)
			events = append(events, event{name: head.Type, data: e})
		}
	case "chat":
		for _, c := range chatChunks(u, text, includeUsage) {
			events = append(events, event{data: c})
		}
		events = append(events, event{data: []byte("[DONE]")})
	case "responses":
		for _, e := range responseEvents(u, text) {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(e, &head)
			events = append(events, event{name: head.Type, data: e})
		}
	}
	if failMidway {
		failure := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"fakebedrock: the model stream failed"}}`)
		if shape == "chat" {
			failure = []byte(`{"error":{"type":"server_error","message":"fakebedrock: the model stream failed"}}`)
		}
		events = []event{events[0], {name: "error", data: failure}}
	}

	written := 0
	for _, e := range events {
		if e.name != "" {
			if _, err := io.WriteString(w, "event: "+e.name+"\n"); err != nil {
				s.streamEnded(written, len(events), err)
				return
			}
		}
		if _, err := io.WriteString(w, "data: "+string(e.data)+"\n\n"); err != nil {
			s.streamEnded(written, len(events), err)
			return
		}
		written++
		_ = rc.Flush()
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	s.streamEnded(written, len(events), nil)

	// Mantle has been seen keeping a Chat Completions response open after
	// [DONE]. So does this, for a while, so a provider that waits for the body
	// to end rather than for the marker takes seconds per request here - which
	// a test notices - instead of working against this fake and hanging against
	// the real thing.
	if shape == "chat" && !failMidway {
		select {
		case <-r.Context().Done():
		case <-time.After(mantleHoldOpen):
		}
	}
}

// mantleHoldOpen is how long a finished Chat Completions stream is held open.
const mantleHoldOpen = 3 * time.Second

func (s *Server) mantleCatalogue() []MantleModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mantleModels
}

// recordSignature notes that a signed request arrived, for a catalogue call
// that carries no body to record.
func (s *Server) recordSignature(r *http.Request) {
	service, region := signatureScope(r.Header.Get("Authorization"))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSigning = [2]string{service, region}
}

// LastCatalogueSignature reports the service and region the last Mantle
// catalogue call was signed for.
func (s *Server) LastCatalogueSignature() (service, region string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSigning[0], s.lastSigning[1]
}

// signatureScope reads the service and region out of a SigV4 Authorization
// header: Credential=AKID/20260915/us-east-1/bedrock-mantle/aws4_request.
//
// Read, not verified - see the package comment. What a test can assert is that
// the provider signed for the right service, which is the mistake that would
// otherwise only show up as an AccessDenied from real AWS.
func signatureScope(authorization string) (service, region string) {
	_, after, found := strings.Cut(authorization, "Credential=")
	if !found {
		return "", ""
	}
	credential, _, _ := strings.Cut(after, ",")
	parts := strings.Split(credential, "/")
	if len(parts) < 5 {
		return "", ""
	}
	return parts[3], parts[2]
}

func mantleFault(f Fault) (status int, errType string) {
	switch f {
	case FaultThrottling:
		return http.StatusTooManyRequests, "rate_limit_error"
	case FaultUnavailable:
		return http.StatusServiceUnavailable, "overloaded_error"
	case FaultAccessDenied:
		return http.StatusForbidden, "permission_error"
	case FaultValidation:
		return http.StatusBadRequest, "invalid_request_error"
	case FaultResourceMissing:
		return http.StatusNotFound, "not_found_error"
	case FaultModelNotReady:
		return 529, "overloaded_error"
	default:
		return http.StatusInternalServerError, "api_error"
	}
}

// writeMantleError writes the OpenAI-shaped error body Mantle returns.
func writeMantleError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": errType, "code": nil, "message": message, "param": nil},
	})
}

// openAIUsage renders the accounting the OpenAI way: cached tokens inside the
// prompt figure rather than beside it, which is the difference the provider has
// to subtract.
func openAIUsage(u Usage, chat bool) map[string]any {
	if chat {
		return map[string]any{
			"prompt_tokens":         u.InputTokens + u.CacheReadTokens,
			"completion_tokens":     u.OutputTokens,
			"total_tokens":          u.InputTokens + u.CacheReadTokens + u.OutputTokens,
			"prompt_tokens_details": map[string]any{"cached_tokens": u.CacheReadTokens},
		}
	}
	return map[string]any{
		"input_tokens":         u.InputTokens + u.CacheReadTokens,
		"output_tokens":        u.OutputTokens,
		"total_tokens":         u.InputTokens + u.CacheReadTokens + u.OutputTokens,
		"input_tokens_details": map[string]any{"cached_tokens": u.CacheReadTokens},
	}
}

func chatCompletion(u Usage, text string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion", "created": 0, "model": "fake",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": text},
		}},
		"usage": openAIUsage(u, true),
	}
}

func chatChunks(u Usage, text string, includeUsage bool) [][]byte {
	chunk := func(delta map[string]any, finish any) []byte {
		out, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 0, "model": "fake",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return out
	}
	out := [][]byte{
		chunk(map[string]any{"role": "assistant", "content": ""}, nil),
		chunk(map[string]any{"content": text}, nil),
		chunk(map[string]any{}, "stop"),
	}
	if includeUsage {
		usage, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 0, "model": "fake",
			"choices": []any{}, "usage": openAIUsage(u, true),
		})
		out = append(out, usage)
	}
	return out
}

func responseObject(u Usage, text string) map[string]any {
	return map[string]any{
		"id": "resp_fake", "object": "response", "status": "completed", "model": "fake",
		"output": []any{map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"usage": openAIUsage(u, false),
	}
}

func responseEvents(u Usage, text string) [][]byte {
	must := func(v any) []byte {
		out, _ := json.Marshal(v)
		return out
	}
	created := responseObject(u, "")
	created["status"] = "in_progress"
	delete(created, "usage")
	return [][]byte{
		must(map[string]any{"type": "response.created", "response": created}),
		must(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": text}),
		must(map[string]any{"type": "response.completed", "response": responseObject(u, text)}),
	}
}
