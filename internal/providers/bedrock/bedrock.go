// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package bedrock implements the Provider interface against Amazon Bedrock.
//
// This package is the only place in Panorama that imports an AWS SDK. The
// AWS packaging is a CloudFormation template and imports nothing.
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/dnxlabs/panorama/internal/providers"
)

// Name is the runtime provider's identifier in namespaced model IDs and in
// metrics. It is providers.KindBedrock, spelled as a string for the places that
// record it.
const Name = string(providers.KindBedrock)

// anthropicVersion is the value Bedrock requires in the request body for
// Anthropic models. It identifies the wire format, not the model.
const anthropicVersion = "bedrock-2023-05-31"

// DefaultUnsupportedBetas are beta feature flags the first-party Anthropic API
// accepts but Bedrock rejects outright with "invalid beta flag".
//
// The two surfaces are dialects of the same API, not the same API, and a
// gateway that forwards the header unfiltered cannot serve Claude Code at all -
// Bedrock fails the whole request on one unrecognised flag.
//
// No count is stated here on purpose: both the number of flags a client sends
// and the number Bedrock refuses move whenever either side ships. What
// matters is the shape: this list is determined by probing Bedrock directly
// rather than read off a document, it goes stale, and it is configurable per
// provider - the unsupported beta flags in Settings, Model providers - so an
// operator can add one without waiting for a release.
//
// To re-probe: send each flag alone in anthropic_beta with max_tokens 1 and see
// which come back "invalid beta flag".
var DefaultUnsupportedBetas = []string{
	"prompt-caching-scope-2026-01-05",
	"advisor-tool-2026-03-01",
	// A Claude Code that sends it meets a Bedrock that refuses it, failing
	// every request. Verified by invoking global.anthropic.claude-sonnet-5
	// with the flags Panorama forwards - rejected - and with the same flags
	// less this one - accepted.
	"redact-thinking-2026-02-12",
	// Prompt caching is generally available on Bedrock and needs no flag, but
	// the first-party API's original opt-in is still sent by older SDK versions
	// and by anything written against the caching launch documentation. Bedrock
	// rejects it, which fails the request that was asking for caching - the
	// feature this gateway is built to measure. Confirmed against Bedrock:
	// without the flag the same request caches normally.
	"prompt-caching-2024-07-31",
}

// Config configures the Bedrock provider.
type Config struct {
	// Region is the Bedrock region to invoke.
	Region string
	// UnsupportedBetas are dropped from requests before they are sent. Nil
	// means DefaultUnsupportedBetas; an explicitly empty slice forwards
	// everything, which is the right setting for finding out what Bedrock has
	// started supporting.
	UnsupportedBetas []string
	// ContextWindows overrides the built-in context-window table, keyed by
	// model family or by an exact model id. Nil means the built-in table
	// alone.
	//
	// It exists because Bedrock does not publish context windows and this
	// build's table is therefore a snapshot that goes stale. See windows.go.
	ContextWindows map[string]int
	// APIs overrides which client APIs a model family speaks. Read by the
	// Mantle provider only; see apis.go.
	APIs map[string][]providers.API
	// Logger records dropped beta flags. Dropping one changes behaviour, so it
	// is reported rather than done quietly.
	Logger *slog.Logger
}

// Provider invokes Anthropic models on Amazon Bedrock.
type Provider struct {
	client      *bedrockruntime.Client
	region      string
	unsupported map[string]struct{}
	log         *slog.Logger
}

// ConfigFrom reads a provider's stored settings into the configuration this
// package builds from.
func ConfigFrom(s providers.Settings, log *slog.Logger) Config {
	c := Config{
		Region:         s.Region,
		ContextWindows: s.Options.ContextWindows,
		APIs:           s.Options.APIs,
		Logger:         log,
	}
	if s.Options.UnsupportedBetas != nil {
		// An explicitly empty list survives as empty rather than nil: it is how
		// an operator says "forward everything".
		c.UnsupportedBetas = append([]string{}, (*s.Options.UnsupportedBetas)...)
	}
	return c
}

// New builds a Bedrock provider for the given region.
//
// Credentials come from the AWS SDK's default chain - an instance role, a task
// role, IRSA, or the ambient environment. Panorama never asks an operator to
// paste an access key, and there is no configuration field for one.
func New(ctx context.Context, conf Config) (*Provider, error) {
	if err := validRegion(conf.Region); err != nil {
		return nil, err
	}
	if conf.Logger == nil {
		conf.Logger = slog.New(slog.DiscardHandler)
	}

	unsupported := conf.UnsupportedBetas
	if unsupported == nil {
		unsupported = DefaultUnsupportedBetas
	}
	set := make(map[string]struct{}, len(unsupported))
	for _, b := range unsupported {
		set[b] = struct{}{}
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(conf.Region))
	if err != nil {
		return nil, fmt.Errorf("bedrock: load aws config: %w", err)
	}

	return &Provider{
		client:      bedrockruntime.NewFromConfig(cfg),
		region:      conf.Region,
		unsupported: set,
		log:         conf.Logger,
	}, nil
}

// filterBetas removes flags Bedrock would reject, and says so.
//
// This is a translation between two dialects of one API, not a fallback that
// hides a failure: the alternative is a request Bedrock refuses outright. Every
// drop is logged so the behaviour change is visible rather than mysterious.
func (p *Provider) filterBetas(ctx context.Context, betas []string) []string {
	if len(betas) == 0 || len(p.unsupported) == 0 {
		return betas
	}

	kept := make([]string, 0, len(betas))
	var dropped []string
	for _, b := range betas {
		if _, bad := p.unsupported[b]; bad {
			dropped = append(dropped, b)
			continue
		}
		kept = append(kept, b)
	}

	if len(dropped) > 0 {
		p.log.LogAttrs(ctx, slog.LevelWarn, "dropped beta flags Bedrock does not accept",
			slog.String("provider", Name),
			slog.String("dropped", strings.Join(dropped, ",")),
		)
	}
	return kept
}

// Name identifies this provider.
func (p *Provider) Name() string { return Name }

// Region reports which Bedrock region this provider invokes.
func (p *Provider) Region() string { return p.region }

// Invoke performs a non-streaming invocation.
func (p *Provider) Invoke(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	if err := servesAnthropic(req); err != nil {
		return nil, err
	}
	betas := p.filterBetas(ctx, req.Betas)
	body, err := prepareBody(req, betas)
	if err != nil {
		return nil, err
	}

	out, err := p.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     &req.Model,
		Body:        body,
		ContentType: strPtr("application/json"),
		Accept:      strPtr("application/json"),
	})
	if err != nil {
		return nil, p.classifyInvocation(ctx, err, betas)
	}

	return &providers.Response{
		Body:  out.Body,
		Usage: usageFromResponse(out.Body),
	}, nil
}

// InvokeStream begins a streaming invocation.
func (p *Provider) InvokeStream(ctx context.Context, req *providers.Request) (providers.EventStream, error) {
	if err := servesAnthropic(req); err != nil {
		return nil, err
	}
	betas := p.filterBetas(ctx, req.Betas)
	body, err := prepareBody(req, betas)
	if err != nil {
		return nil, err
	}

	// The connection is watched from here on. A streamed response is read long
	// after this call returns, so the transport can finish with the connection
	// while the reader has not - and that is invisible in the error it
	// produces. See connTrace.
	ctx, trace := withConnTrace(ctx, p.log)

	out, err := p.client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     &req.Model,
		Body:        body,
		ContentType: strPtr("application/json"),
		Accept:      strPtr("application/json"),
	})
	if err != nil {
		return nil, p.classifyInvocation(ctx, err, betas)
	}

	return &eventStream{stream: out.GetStream(), trace: trace}, nil
}

// eventStream adapts Bedrock's event stream to providers.EventStream.
//
// Events are handed on one at a time as they arrive. Nothing here accumulates
// the response.
type eventStream struct {
	stream *bedrockruntime.InvokeModelWithResponseStreamEventStream
	// trace is what the transport did with the connection underneath. It is
	// read only when the stream fails, and only by a person.
	trace *connTrace

	mu     sync.Mutex
	usage  providers.Usage
	closed bool
}

// Diagnostics reports the connection this stream was served over.
//
// It satisfies providers.Diagnostic, which the data plane folds into the
// failure it already logs. Nothing derives behaviour from these.
func (s *eventStream) Diagnostics() []slog.Attr { return s.trace.attrs() }

// Next returns the next event payload, or io.EOF at the end of the stream.
func (s *eventStream) Next() ([]byte, error) {
	for event := range s.stream.Events() {
		chunk, ok := event.(*brtypes.ResponseStreamMemberChunk)
		if !ok {
			// An event type this build does not model. Skipping it is correct:
			// relaying an unknown frame to a client expecting Anthropic SSE
			// would corrupt the stream.
			continue
		}

		payload := chunk.Value.Bytes
		s.observe(payload)
		return payload, nil
	}

	if err := s.stream.Err(); err != nil {
		return nil, classify(err)
	}
	return nil, io.EOF
}

// observe pulls token accounting out of an event without altering it.
//
// This is the only work done per event on the hot path: a shallow decode of a
// small envelope, measured in microseconds. Anything heavier belongs in a job.
func (s *eventStream) observe(payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	observeAnthropic(&s.usage, payload)
}

// observeAnthropic reads the usage an Anthropic Messages stream event carries
// into u. Shared by both Bedrock endpoints, which stream Claude in the same
// event shape.
func observeAnthropic(u *providers.Usage, payload []byte) {
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Usage *wireUsage `json:"usage"`
		} `json:"message"`
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		// A malformed event is the provider's problem, not a reason to fail
		// the stream - the client sees exactly what Bedrock sent either way.
		return
	}

	switch env.Type {
	case "message_start":
		// Input accounting, including both cache classes, arrives once up
		// front.
		if w := env.Message.Usage; w != nil {
			observed := w.usage()
			u.InputTokens = observed.InputTokens
			u.CacheReadTokens = observed.CacheReadTokens
			u.CacheWriteTokens = observed.CacheWriteTokens
			u.CacheWrite1hTokens = observed.CacheWrite1hTokens
			if observed.OutputTokens > 0 {
				u.OutputTokens = observed.OutputTokens
			}
		}
	case "message_delta":
		// Output accounting is cumulative and restated as generation
		// progresses; the last value wins.
		if w := env.Usage; w != nil {
			if w.OutputTokens > 0 {
				u.OutputTokens = w.OutputTokens
			}
			if w.InputTokens > 0 {
				u.InputTokens = w.InputTokens
			}
		}
	}
}

// Usage returns the accounting observed so far.
func (s *eventStream) Usage() providers.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// Close releases the stream.
func (s *eventStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	return s.stream.Close()
}

// wireUsage mirrors the usage object Anthropic models report on Bedrock.
//
// Bedrock splits cache writes by TTL in a nested `cache_creation` object and
// reports their sum in the flat cache_creation_input_tokens. Both are needed:
// AWS publishes a separate, higher rate for the one-hour cache - $4.40 against
// $2.75 per million on Sonnet 5 - so a gateway holding only the sum cannot cost
// a cache write correctly.
type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// usage converts the wire object into the provider-neutral accounting.
func (u *wireUsage) usage() providers.Usage {
	out := providers.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	if u.CacheCreation != nil {
		out.CacheWrite1hTokens = u.CacheCreation.Ephemeral1h
		// Older responses omit the flat total. Reconstructing it from the split
		// keeps the invariant that the total covers both TTLs.
		if out.CacheWriteTokens == 0 {
			out.CacheWriteTokens = u.CacheCreation.Ephemeral5m + u.CacheCreation.Ephemeral1h
		}
	}
	return out
}

// usageFromResponse reads the usage object from a non-streaming response.
func usageFromResponse(body []byte) providers.Usage {
	var env struct {
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return providers.Usage{}
	}
	return env.Usage.usage()
}

// prepareBody adapts an Anthropic Messages request for Bedrock's InvokeModel.
//
// Three differences have to be reconciled. Bedrock takes the model in the URL
// and rejects it in the body; it requires an anthropic_version identifying the
// wire format; and it reads beta feature flags from an anthropic_beta field in
// the body rather than from the anthropic-beta HTTP header the first-party API
// uses. Without that last translation a client asking for any beta feature has
// the feature's parameters rejected as unrecognised - which is how a working
// Claude Code request fails against Bedrock. Streaming is selected by the
// operation rather than by a body field.
//
// The rewrite is deliberately shallow. Decoding into map[string]json.RawMessage
// leaves every nested value - the message array, which is nearly the whole
// payload - as opaque bytes, so the cost stays proportional to the number of
// top-level keys rather than to the size of the conversation.
func prepareBody(req *providers.Request, betas []string) ([]byte, error) {
	fields, err := req.TopLevel()
	if err != nil {
		return nil, err
	}

	// Bedrock rejects a request whose body carries these; both are expressed
	// through the API call instead.
	delete(fields, "model")
	delete(fields, "stream")

	if _, ok := fields["anthropic_version"]; !ok {
		fields["anthropic_version"] = json.RawMessage(`"` + anthropicVersion + `"`)
	}

	// A body that already names its betas wins: the client was explicit.
	if _, ok := fields["anthropic_beta"]; !ok && len(betas) > 0 {
		encoded, merr := json.Marshal(betas)
		if merr != nil {
			return nil, &providers.Error{
				Kind:       providers.ErrKindInvalidRequest,
				StatusCode: http.StatusBadRequest,
				Message:    "could not encode beta flags",
				Err:        merr,
			}
		}
		fields["anthropic_beta"] = encoded
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &providers.Error{
			Kind:       providers.ErrKindInvalidRequest,
			StatusCode: http.StatusBadRequest,
			Message:    "could not encode request body",
			Err:        err,
		}
	}
	return out, nil
}

// classify turns an AWS error into a provider error the data plane can act on
// without knowing anything about Bedrock.
//
// Provider messages are not relayed to clients: they leak account and model
// details, and the client cannot act on them. The original travels in Err for
// classifyInvocation is classify, plus the one thing the error itself will not
// say.
//
// Bedrock reports an unrecognised beta flag as "invalid beta flag" and does not
// name it, so the flag that failed the request is the one piece of information
// missing from the only record of the failure. Panorama is holding it at that
// moment - it filtered the list a few lines earlier - so it says so, and the
// operator reads the answer out of the error instead of learning that a debug
// line exists, enabling it, and reproducing.
//
// Flag names are not content: they name features, never anything a user typed.
func (p *Provider) classifyInvocation(ctx context.Context, err error, betas []string) error {
	out := classify(err)
	if len(betas) == 0 || !strings.Contains(strings.ToLower(err.Error()), "invalid beta flag") {
		return out
	}

	p.log.LogAttrs(ctx, slog.LevelError, "Bedrock rejected a beta flag it was sent",
		slog.String("provider", Name),
		slog.String("forwarded", strings.Join(betas, ",")),
		slog.String("note", "one of these is not supported on Bedrock; add it to the "+
			"provider's unsupported beta flags in Settings, Model providers, to drop it"),
	)
	return out
}

// the log.
func classify(err error) error {
	if err == nil {
		return nil
	}

	detail := awsDetail(err)

	var (
		throttling   *brtypes.ThrottlingException
		unavailable  *brtypes.ServiceUnavailableException
		notReady     *brtypes.ModelNotReadyException
		timeout      *brtypes.ModelTimeoutException
		validation   *brtypes.ValidationException
		accessDenied *brtypes.AccessDeniedException
		notFound     *brtypes.ResourceNotFoundException
	)

	switch {
	case errors.As(err, &throttling):
		return &providers.Error{
			Kind:       providers.ErrKindThrottled,
			StatusCode: http.StatusTooManyRequests,
			Message:    "the model is rate limited right now",
			Detail:     detail,
			Err:        err,
		}
	case errors.As(err, &unavailable), errors.As(err, &notReady), errors.As(err, &timeout):
		return &providers.Error{
			Kind:       providers.ErrKindUnavailable,
			StatusCode: http.StatusServiceUnavailable,
			Message:    "the model is temporarily unavailable",
			Detail:     detail,
			Err:        err,
		}
	case errors.As(err, &validation):
		return &providers.Error{
			Kind:       providers.ErrKindInvalidRequest,
			StatusCode: http.StatusBadRequest,
			Message:    "the provider rejected the request",
			Detail:     detail,
			Err:        err,
		}
	case errors.As(err, &accessDenied):
		return &providers.Error{
			Kind:       providers.ErrKindForbidden,
			StatusCode: http.StatusBadGateway,
			Message:    "the gateway is not permitted to invoke this model",
			Detail:     detail,
			Err:        err,
		}
	case errors.As(err, &notFound):
		return &providers.Error{
			Kind:       providers.ErrKindNotFound,
			StatusCode: http.StatusBadGateway,
			Message:    "the model is not available to this deployment",
			Detail:     detail,
			Err:        err,
		}
	case errors.Is(err, context.Canceled):
		// The client hung up. Not a provider failure.
		return err
	default:
		return &providers.Error{
			Kind:       providers.ErrKindUpstream,
			StatusCode: http.StatusBadGateway,
			Message:    "the model provider returned an error",
			Detail:     detail,
			Err:        err,
		}
	}
}

// awsDetail is AWS's error code and message, without the SDK's wrapping of
// operation, status and request id around them. It is what an administrator
// is shown and what the hints are matched against: "Model use case details
// have not been submitted for this account" is the whole of what they need,
// and the rest of the error is noise around it. Empty for a failure that never
// reached AWS, whose error text is then the detail.
func awsDetail(err error) string {
	var api interface {
		ErrorCode() string
		ErrorMessage() string
	}
	if !errors.As(err, &api) {
		return ""
	}
	if msg := api.ErrorMessage(); msg != "" {
		return api.ErrorCode() + ": " + msg
	}
	return api.ErrorCode()
}

// servesAnthropic refuses a request in any shape but the Anthropic Messages
// body, which is the only one this provider sends through InvokeModel.
//
// An empty API is the Messages body, because that is what every caller sent
// before a request carried its shape - the setup check among them.
func servesAnthropic(req *providers.Request) error {
	if req.API == "" || req.API == providers.APIAnthropicMessages {
		return nil
	}
	return &providers.Error{
		Kind:       providers.ErrKindInvalidRequest,
		StatusCode: http.StatusBadRequest,
		Message:    "the " + Name + " provider serves the Anthropic Messages API only",
		Err:        fmt.Errorf("bedrock: asked to serve %s", req.API),
	}
}

// regionShape is what an AWS region name looks like: us-east-1, us-gov-west-1,
// ap-southeast-4. An allowlist of shape rather than of names, because a list of
// names compiled into a release is out of date the day AWS opens a region -
// whether the region is real is what the live check answers. The shape still
// matters: the Mantle provider builds a hostname out of this string.
var regionShape = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d{1,2}$`)

func validRegion(region string) error {
	if region == "" {
		return errors.New("bedrock: region is required")
	}
	if !regionShape.MatchString(region) {
		return fmt.Errorf("bedrock: %q is not an AWS region name, like us-east-1", region)
	}
	return nil
}

func strPtr(s string) *string { return &s }
