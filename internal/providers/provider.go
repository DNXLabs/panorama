// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package providers defines the Provider interface that the data plane calls,
// and holds each backend implementation.
//
// Amazon Bedrock is the only vendor in v1, reached through two providers:
// `bedrock`, which is bedrock-runtime, and `bedrock-mantle`. They are two kinds
// rather than one provider with a switch, because they differ in everything a
// route depends on - identifier spaces, the APIs a model speaks, IAM actions,
// quotas and regional availability - and each is configured once, in one
// region. The same provider in two regions is not something this product
// expresses.
//
// Model identifiers are namespaced by provider - bedrock/us.anthropic.claude-...,
// bedrock-mantle/openai.gpt-oss-120b - so a model name is unambiguous in a route,
// the price table and analytics. See ModelRef.
//
// Nothing outside this package's bedrock subpackage imports an AWS SDK - the
// AWS packaging is a CloudFormation template, which imports nothing at all.
// That boundary is what keeps Panorama AWS-first rather than AWS-only.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// Kind names a provider. It is also the provider's identity: there is one
// configuration per kind, so nothing needs an id beside it.
type Kind string

// The kinds this build can serve.
const (
	// KindBedrock is Amazon Bedrock's bedrock-runtime endpoint: InvokeModel,
	// inference profiles, and the Anthropic Messages body.
	KindBedrock Kind = "bedrock"
	// KindBedrockMantle is Amazon Bedrock's bedrock-mantle endpoint: the
	// Anthropic Messages API for Claude, and the OpenAI Chat Completions and
	// Responses APIs for everything else, each model invoked in the region the
	// call is made to.
	KindBedrockMantle Kind = "bedrock-mantle"
)

// kinds is the allowlist, in the order a screen offers them.
//
// A kind absent from here is refused everywhere a kind is read - a policy file,
// the configuration API, and the database, whose CHECK constraint names the
// same list. Adding one is a change to this list, a descriptor registered in
// cmd/panorama, and a migration widening that constraint.
var kinds = []Kind{KindBedrock, KindBedrockMantle}

// Kinds lists every kind this build can serve.
func Kinds() []Kind { return slices.Clone(kinds) }

// Valid reports whether k is a kind this build can serve.
func (k Kind) Valid() bool { return slices.Contains(kinds, k) }

// ModelRef names a model the way everything outside a provider refers to one:
// which provider, and the identifier that provider is given.
//
// Written `provider/model` - bedrock/us.anthropic.claude-sonnet-5 - and split on
// the first slash only, because a model identifier may carry slashes of its own
// (an inference-profile ARN does) and a provider kind never does.
//
// There is deliberately no form without the provider. A bare identifier would
// be a guess: `anthropic.claude-haiku-4-5` is a valid identifier on both
// endpoints, and they bill, route and authorise differently.
type ModelRef struct {
	Provider Kind
	Model    string
}

// ParseModelRef reads a reference, refusing one that names no provider or a
// provider this build does not know.
func ParseModelRef(s string) (ModelRef, error) {
	s = strings.TrimSpace(s)
	kind, model, found := strings.Cut(s, "/")
	if !found {
		return ModelRef{}, fmt.Errorf("%q names no provider; write it as provider/model, e.g. %s/%s",
			s, KindBedrock, s)
	}
	ref := ModelRef{Provider: Kind(strings.TrimSpace(kind)), Model: strings.TrimSpace(model)}
	if !ref.Provider.Valid() {
		return ModelRef{}, fmt.Errorf("%q names the provider %q, which is not one of %s",
			s, kind, joinKinds())
	}
	if ref.Model == "" {
		return ModelRef{}, fmt.Errorf("%q names a provider and no model", s)
	}
	return ref, nil
}

// MustModelRef parses a reference that is a literal in code, and panics on one
// that is not valid. For tests and fixtures only.
func MustModelRef(s string) ModelRef {
	ref, err := ParseModelRef(s)
	if err != nil {
		panic(err)
	}
	return ref
}

// String writes the reference back out.
func (r ModelRef) String() string {
	if r.IsZero() {
		return ""
	}
	return string(r.Provider) + "/" + r.Model
}

// IsZero reports whether no model is named.
func (r ModelRef) IsZero() bool { return r.Provider == "" && r.Model == "" }

func joinKinds() string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

// API is a client-facing API shape: what a request body and a stream look like.
//
// A model speaks some of these and not others, and a route is served only on
// the APIs its model speaks. Nothing is translated between them - Claude Code
// against a model that only speaks Chat Completions is refused with the
// endpoint that would work, rather than served through a translation layer
// whose gaps would surface as a session failing in the middle of somebody's
// work.
type API string

// The APIs this build serves.
const (
	APIAnthropicMessages API = "anthropic-messages"
	APIOpenAIChat        API = "openai-chat"
	APIOpenAIResponses   API = "openai-responses"
)

var apis = []API{APIAnthropicMessages, APIOpenAIChat, APIOpenAIResponses}

// APIs lists every API this build serves.
func APIs() []API { return slices.Clone(apis) }

// Valid reports whether a is an API this build serves.
func (a API) Valid() bool { return slices.Contains(apis, a) }

// Endpoint is the gateway path a client sends this API to, for a message that
// has to tell somebody where to point their tool instead.
func (a API) Endpoint() string {
	switch a {
	case APIAnthropicMessages:
		return "/v1/messages"
	case APIOpenAIChat:
		return "/v1/chat/completions"
	case APIOpenAIResponses:
		return "/v1/responses"
	default:
		return ""
	}
}

// Label is how a sentence names the API.
func (a API) Label() string {
	switch a {
	case APIAnthropicMessages:
		return "Anthropic Messages"
	case APIOpenAIChat:
		return "OpenAI Chat Completions"
	case APIOpenAIResponses:
		return "OpenAI Responses"
	default:
		return string(a)
	}
}

// ParseAPIs reads a comma-separated list, dropping anything that is not an API
// this build serves. The column it reads is written only by this package's
// callers, so a value it does not know is one a newer build wrote, and treating
// it as absent is the reading that serves nothing it cannot.
func ParseAPIs(s string) []API {
	var out []API
	for _, part := range strings.Split(s, ",") {
		a := API(strings.TrimSpace(part))
		if a.Valid() && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// JoinAPIs writes a list back out for storage.
func JoinAPIs(list []API) string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, string(a))
	}
	return strings.Join(out, ",")
}

// Request is one invocation, carried as the client sent it.
//
// Body is the raw request body. The data plane does not deserialise and
// re-serialise it: a round trip would cost latency proportional to the
// conversation size, and any field the round trip did not model would be
// silently dropped. Providers rewrite only the specific keys their wire format
// requires.
type Request struct {
	// API is the shape Body is in, and the shape a response has to come back
	// in. A provider handed an API it cannot serve for this model refuses the
	// request as invalid rather than guessing at a translation.
	API API
	// Model is the provider-native model identifier to invoke, already
	// resolved from what the client asked for.
	Model string
	// Body is the client's request body, verbatim.
	Body []byte
	// Fields is Body decoded one level deep, when the caller already has it.
	//
	// The data plane decodes the top level once to read the model and to apply
	// the few rewrites a surface makes; handing that map on means a provider
	// rewriting its own keys does not scan a multi-megabyte conversation a
	// second time. Nil means decode Body. A provider may modify the map, and
	// the caller does not read it afterwards.
	Fields map[string]json.RawMessage
	// Betas are the beta feature flags the client asked for.
	//
	// They are carried separately because clients send them as an HTTP header
	// while some providers expect them inside the request body. Translating
	// between the two is the provider's job, not the client's.
	Betas []string
}

// Usage is the token accounting for one invocation, across all four classes.
//
// Cache reads bill at a fraction of the input rate and dominate coding-agent
// traffic, so a provider that reports them must surface them here. Collapsing
// them into InputTokens would overstate cost several-fold.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CacheWrite1hTokens is the portion of CacheWriteTokens written with a
	// one-hour time to live, so it never exceeds it.
	//
	// The split is here because the two TTLs are separately priced - AWS charges
	// roughly 1.6x more for the one-hour cache - and a single cache-write number
	// cannot be costed correctly without knowing which kind it was.
	CacheWrite1hTokens int64
}

// Response is a complete, non-streamed invocation result.
type Response struct {
	// Body is the provider's response body, to be relayed to the client.
	Body []byte
	// Usage is what the provider reported, when it reported anything.
	Usage Usage
}

// TopLevel returns a request body decoded one level deep, reusing what the
// caller already decoded when it did.
//
// Decoding into map[string]json.RawMessage leaves every nested value - the
// message array, which is nearly the whole payload - as opaque bytes, so a
// rewrite costs in proportion to the number of top-level keys rather than to
// the size of the conversation.
func (r *Request) TopLevel() (map[string]json.RawMessage, error) {
	if r.Fields != nil {
		return r.Fields, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &fields); err != nil || fields == nil {
		if err == nil {
			err = errors.New("the body is null")
		}
		return nil, &Error{
			Kind:       ErrKindInvalidRequest,
			StatusCode: 400,
			Message:    "request body is not a JSON object",
			Err:        err,
		}
	}
	r.Fields = fields
	return fields, nil
}

// EventStream is a streamed invocation in progress.
//
// Next returns event payloads one at a time as they arrive, in the request's
// own API: an Anthropic event for a Messages request, a Chat Completions chunk
// for a chat request, a Responses event for a Responses request. Nothing
// in an implementation may accumulate events or wait for the stream to
// complete: the whole point of this interface is that a token reaches the
// client as soon as the provider emits it.
type EventStream interface {
	// Next returns the next event payload. It returns io.EOF when the stream
	// has ended cleanly.
	Next() ([]byte, error)
	// Usage returns the accounting observed so far. It is meaningful once
	// Next has returned io.EOF.
	Usage() Usage
	// Close releases the stream. It is safe to call more than once.
	Close() error
}

// Diagnostic is implemented by an EventStream that can say something about the
// transport underneath it.
//
// Optional, and deliberately so: it exists for one failure whose error text
// names nothing. A streamed response that stops part way through reports "read
// tcp ...: use of closed network connection", which is net.ErrClosed - a socket
// reporting about itself, so the close came from inside this process, and the
// error says no more than that. What closed it is knowledge only the provider
// holds, because only the provider owns the connection; the data plane folds
// these into the failure it already logs rather than deriving anything from
// them.
//
// Nothing may depend on a stream implementing this, and nothing may read these
// as data. They are for a person reading a log.
type Diagnostic interface {
	Diagnostics() []slog.Attr
}

// Provider is a model backend.
type Provider interface {
	// Name identifies the provider in model namespacing, metrics, and logs. It
	// is the provider's Kind, as a string.
	Name() string
	// Invoke performs a non-streaming invocation.
	Invoke(ctx context.Context, req *Request) (*Response, error)
	// InvokeStream begins a streaming invocation.
	InvokeStream(ctx context.Context, req *Request) (EventStream, error)
}

// CatalogueEntry is one invocable target as a provider describes it.
//
// One entry per identifier the provider will actually accept, not one per
// model: a model reachable through several inference profiles produces several
// entries, because they route to different places and bill at different rates.
type CatalogueEntry struct {
	// ID is the exact string to invoke, profile prefix included.
	ID string
	// FoundationModelID is the bare model underneath, which carries the price.
	FoundationModelID string

	// Provider is which provider this identifier is valid on. Two providers
	// can report the same string - `anthropic.claude-haiku-4-5` exists on both
	// Bedrock endpoints - and it is two different targets.
	Provider Kind
	// Region is where the provider was asked. A bare identifier is invoked in
	// that region, so for a provider with no inference profiles it is the whole
	// residency answer.
	Region string

	// Vendor is who made the model - "Anthropic", "OpenAI" - as the provider
	// names them. Not the provider: that is Provider above.
	Vendor      string
	DisplayName string

	// APIs are the client-facing APIs this target can be served on. Empty is a
	// model this build can list and cannot serve, which the console shows and
	// a route refuses.
	APIs []API
	// Status is the provider's own word when a model is listed but cannot be
	// invoked right now, and empty otherwise. Reason says why, when the
	// provider does.
	Status       string
	StatusReason string
	// Retention lists the data-retention modes the provider allows for this
	// model, as it names them, and is empty when it does not say.
	Retention []string

	// Geography is the inference-profile prefix as itself - "us", "au",
	// "global" - or empty for a bare foundation-model id.
	Geography string
	// Regions are the regions this target may route a request to. Empty means
	// unrestricted, which is what `global` is.
	Regions []string

	// Streaming reports whether the provider will stream this model. A model
	// that cannot stream cannot serve a coding agent.
	Streaming bool
	// Lifecycle is the provider's own word: "ACTIVE", "LEGACY".
	Lifecycle string
	// RequiresProfile reports that the bare model id is refused and an
	// inference profile must be used instead.
	RequiresProfile bool

	// ContextWindow is how many tokens this target accepts, or zero when the
	// provider could not say.
	//
	// Zero is "not recorded", never "no context" - Bedrock publishes no
	// context length at all, so the provider fills this from a table it
	// maintains and leaves it at zero for anything the table has not heard of.
	// Every consumer distinguishes the two: a number nobody stands behind is
	// worse here than an absence, because a route refusal and a client's token
	// budget are both derived from it.
	ContextWindow int

	// MaxOutputTokens is how many tokens the target will produce by default,
	// or zero when the provider could not say. Zero is "not recorded", for the
	// same reason and with the same handling as ContextWindow.
	MaxOutputTokens int
}

// Cataloguer is the optional capability of reporting what a provider can invoke
// in this account and region.
//
// Optional because it is genuinely not universal - a provider reached through a
// fixed endpoint with a fixed model list has nothing to discover - and a
// Provider that cannot answer should fail to satisfy this interface rather than
// return an empty list, which would be indistinguishable from an account with
// no models.
type Cataloguer interface {
	Catalogue(ctx context.Context) ([]CatalogueEntry, error)
}

// Error is a provider failure classified into terms the data plane can act on
// without knowing which backend produced it.
type Error struct {
	// Kind is the coarse class, recorded in the audit trail and mapped onto the
	// status and error type a client is sent.
	Kind ErrorKind
	// StatusCode is the HTTP status to return to the client.
	StatusCode int
	// Message is safe to show a client. Provider text is not relayed verbatim.
	Message string
	// Err is the underlying error, for logs only.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("provider: %s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("provider: %s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrorKind classifies a provider failure.
type ErrorKind string

const (
	// ErrKindThrottled means the provider is rate limiting. Under real
	// coding-agent load this is routine, not exceptional, and it reaches the
	// client as a 429: every client this gateway serves retries one, and a
	// route is one model, so there is nowhere else to send the request.
	ErrKindThrottled ErrorKind = "throttled"
	// ErrKindUnavailable means the model or region is temporarily unreachable.
	ErrKindUnavailable ErrorKind = "unavailable"
	// ErrKindInvalidRequest means the client sent something the provider
	// rejected. Retrying elsewhere would fail identically.
	ErrKindInvalidRequest ErrorKind = "invalid_request"
	// ErrKindNotFound means the model does not exist or is not enabled for
	// this account.
	ErrKindNotFound ErrorKind = "not_found"
	// ErrKindForbidden means the deployment's credentials lack access.
	ErrKindForbidden ErrorKind = "forbidden"
	// ErrKindUpstream is any other provider-side failure.
	ErrKindUpstream ErrorKind = "upstream"
)

// AsError extracts a *Error from an error chain.
func AsError(err error) (*Error, bool) {
	var pe *Error
	ok := errors.As(err, &pe)
	return pe, ok
}
