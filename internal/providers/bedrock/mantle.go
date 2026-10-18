// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/dnxlabs/panorama/internal/providers"
)

// MantleName is the bedrock-mantle provider's identifier, as recorded.
const MantleName = string(providers.KindBedrockMantle)

// mantleService is the SigV4 signing name. It is also the IAM action prefix,
// which is why bedrock:InvokeModel authorises nothing here.
const mantleService = "bedrock-mantle"

// mantleEndpointEnv redirects the provider to another host.
//
// It exists for the end-to-end suite, which points it at a fake speaking the
// same paths, and for nothing else. There is no SDK client for this endpoint to
// honour an AWS_ENDPOINT_URL_* variable on the provider's behalf, so the
// provider reads the conventional name itself - and because a variable that can
// send a deployment's model traffic to any host is worth noticing, it says so
// at WARN every time a provider is built with it set. It is not a configuration
// key, and there is no console field for it.
const mantleEndpointEnv = "AWS_ENDPOINT_URL_BEDROCK_MANTLE"

// mantleAnthropicVersion is the Messages API version the Anthropic path is
// spoken at. The first-party header, not Bedrock's in-body version string:
// bedrock-mantle serves the Anthropic API as Anthropic defines it.
const mantleAnthropicVersion = "2023-06-01"

// maxResponseBytes bounds a non-streaming response read into memory.
const maxResponseBytes = 64 << 20

// Mantle invokes models on Amazon Bedrock's bedrock-mantle endpoint.
//
// There is no AWS SDK client for this endpoint, so it is plain HTTP signed with
// SigV4 - which is all an SDK client would be. Credentials come from the same
// default chain the runtime provider uses: a task role, an instance role, the
// environment. Nothing here takes a key, and CallWithBearerToken is not in the
// role's policy.
//
// Three request shapes, chosen by the API the request arrived in: Claude over
// the Anthropic Messages API at /anthropic/v1/messages, and every other model
// over OpenAI's Chat Completions or Responses API at the base path its family
// is served on. A model is invoked in the region the call is made to; there
// are no inference profiles, so the region is the residency answer.
//
// There is no retry loop. The runtime provider inherits three attempts from
// its SDK before a throttle reaches it; this one hands a 429 straight to the
// client, which retries it. The asymmetry is documented rather than evened out,
// because a retry loop here would be a second place deciding how long an
// engineer waits.
type Mantle struct {
	region      string
	base        *url.URL
	creds       aws.CredentialsProvider
	signer      *v4.Signer
	client      *http.Client
	unsupported map[string]struct{}
	apis        func(string) []providers.API
	log         *slog.Logger
	now         func() time.Time
}

// NewMantle builds a bedrock-mantle provider for a region.
func NewMantle(ctx context.Context, conf Config) (*Mantle, error) {
	if err := validRegion(conf.Region); err != nil {
		return nil, err
	}
	if conf.Logger == nil {
		conf.Logger = slog.New(slog.DiscardHandler)
	}

	base, err := mantleBase(conf.Region, conf.Logger)
	if err != nil {
		return nil, err
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(conf.Region))
	if err != nil {
		return nil, fmt.Errorf("bedrock-mantle: load aws config: %w", err)
	}

	unsupported := conf.UnsupportedBetas
	if unsupported == nil {
		unsupported = DefaultUnsupportedBetas
	}
	set := make(map[string]struct{}, len(unsupported))
	for _, b := range unsupported {
		set[b] = struct{}{}
	}

	return &Mantle{
		region: conf.Region,
		base:   base,
		creds:  cfg.Credentials,
		signer: v4.NewSigner(),
		// No overall timeout: a stream lasts as long as a model takes, and the
		// request's context is what ends one early.
		client:      &http.Client{Transport: newTransport()},
		unsupported: set,
		apis:        MantleAPIs(conf.APIs),
		log:         conf.Logger,
		now:         time.Now,
	}, nil
}

// newTransport is a transport of this provider's own, so its idle connections
// are not shared with anything else in the process.
func newTransport() *http.Transport {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

// MantleHost is the endpoint a region's bedrock-mantle requests go to.
func MantleHost(region string) string { return "bedrock-mantle." + region + ".api.aws" }

// mantleBase resolves where requests go: the regional host, or the test
// override with a warning.
func mantleBase(region string, log *slog.Logger) (*url.URL, error) {
	if override := strings.TrimSpace(os.Getenv(mantleEndpointEnv)); override != "" {
		u, err := url.Parse(override)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("bedrock-mantle: %s=%q is not an http(s) URL", mantleEndpointEnv, override)
		}
		log.Warn("bedrock-mantle requests are redirected by an environment variable",
			"variable", mantleEndpointEnv, "endpoint", u.String(),
			"note", "this exists for the end-to-end suite; a deployment should never set it")
		return u, nil
	}
	return &url.URL{Scheme: "https", Host: MantleHost(region)}, nil
}

// Name identifies this provider.
func (m *Mantle) Name() string { return MantleName }

// Region reports which region this provider invokes.
func (m *Mantle) Region() string { return m.region }

// Invoke performs a non-streaming invocation.
func (m *Mantle) Invoke(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	api, httpReq, err := m.prepare(ctx, req, false)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return nil, transportError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, transportError(err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, m.classifyResponse(ctx, resp.StatusCode, body, req.Betas)
	}

	return &providers.Response{Body: body, Usage: usageOf(api, body)}, nil
}

// InvokeStream begins a streaming invocation.
func (m *Mantle) InvokeStream(ctx context.Context, req *providers.Request) (providers.EventStream, error) {
	ctx, trace := withConnTrace(ctx, m.log)

	api, httpReq, err := m.prepare(ctx, req, true)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return nil, transportError(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, m.classifyResponse(ctx, resp.StatusCode, body, req.Betas)
	}

	return &mantleStream{
		api:   api,
		body:  resp.Body,
		sse:   bufio.NewReaderSize(resp.Body, 64<<10),
		trace: trace,
	}, nil
}

// prepare builds and signs the HTTP request for one invocation.
//
// The body rewrite is shallow, like the runtime provider's: the model is set to
// the provider-native id, and `stream` says which of the two calls this is.
// Everything else is the client's, including every field this build does not
// model.
func (m *Mantle) prepare(ctx context.Context, req *providers.Request, stream bool) (providers.API, *http.Request, error) {
	api := req.API
	if api == "" {
		api = providers.APIAnthropicMessages
	}
	if !slices.Contains(m.apis(req.Model), api) {
		return api, nil, &providers.Error{
			Kind:       providers.ErrKindInvalidRequest,
			StatusCode: http.StatusBadRequest,
			Message:    fmt.Sprintf("%s is not served over the %s API on bedrock-mantle", req.Model, api.Label()),
			Err:        fmt.Errorf("bedrock-mantle: %s does not speak %s", req.Model, api),
		}
	}

	fields, err := req.TopLevel()
	if err != nil {
		return api, nil, err
	}
	model, err := json.Marshal(req.Model)
	if err != nil {
		return api, nil, invalid("could not encode the model id", err)
	}
	fields["model"] = model
	fields["stream"] = json.RawMessage(fmt.Sprintf("%t", stream))

	var (
		path   string
		header = http.Header{}
	)
	switch api {
	case providers.APIAnthropicMessages:
		path = "/anthropic/v1/messages"
		header.Set("anthropic-version", mantleAnthropicVersion)
		// Bedrock's in-body version string belongs to the runtime dialect, and
		// a client that set one meant the other endpoint.
		delete(fields, "anthropic_version")
		if betas := m.filterBetas(ctx, req.Betas); len(betas) > 0 {
			header.Set("anthropic-beta", strings.Join(betas, ","))
		}
	case providers.APIOpenAIChat:
		path = mantleOpenAIBase(req.Model) + "/chat/completions"
		if mantleCompletionTokens(req.Model) {
			m.spellCompletionCap(ctx, req.Model, fields)
		}
	case providers.APIOpenAIResponses:
		path = mantleOpenAIBase(req.Model) + "/responses"
	default:
		return api, nil, invalid("unknown API "+string(api), nil)
	}

	body, err := json.Marshal(fields)
	if err != nil {
		return api, nil, invalid("could not encode request body", err)
	}

	httpReq, err := m.newSignedRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return api, nil, err
	}
	for k, v := range header {
		httpReq.Header[k] = v
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	// Signed after every header is set, so the signature covers them.
	if err := m.sign(ctx, httpReq, body); err != nil {
		return api, nil, err
	}
	return api, httpReq, nil
}

// newSignedRequest builds a request to a path on this provider's host. It is
// signed by sign, which runs last.
func (m *Mantle) newSignedRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	target := *m.base
	target.Path = strings.TrimSuffix(target.Path, "/") + path

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("bedrock-mantle: build request: %w", err)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return httpReq, nil
}

// sign adds a SigV4 signature for the bedrock-mantle service.
func (m *Mantle) sign(ctx context.Context, req *http.Request, body []byte) error {
	creds, err := m.creds.Retrieve(ctx)
	if err != nil {
		return &providers.Error{
			Kind:       providers.ErrKindForbidden,
			StatusCode: http.StatusBadGateway,
			Message:    "the gateway has no AWS credentials to sign the request with",
			Err:        err,
		}
	}
	sum := sha256.Sum256(body)
	if err := m.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), mantleService, m.region, m.now()); err != nil {
		return fmt.Errorf("bedrock-mantle: sign request: %w", err)
	}
	return nil
}

// filterBetas removes flags this provider is configured not to forward, and
// says so - the same translation the runtime provider makes, for the same
// reason.
func (m *Mantle) filterBetas(ctx context.Context, betas []string) []string {
	if len(betas) == 0 || len(m.unsupported) == 0 {
		return betas
	}
	kept := make([]string, 0, len(betas))
	var dropped []string
	for _, b := range betas {
		if _, bad := m.unsupported[b]; bad {
			dropped = append(dropped, b)
			continue
		}
		kept = append(kept, b)
	}
	if len(dropped) > 0 {
		m.log.LogAttrs(ctx, slog.LevelWarn, "dropped beta flags bedrock-mantle is configured not to accept",
			slog.String("provider", MantleName),
			slog.String("dropped", strings.Join(dropped, ",")),
		)
	}
	return kept
}

// spellCompletionCap moves a Chat Completions output cap from max_tokens to
// max_completion_tokens, for the families that read only the second.
//
// OpenAI replaced max_tokens with max_completion_tokens, and its reasoning
// models refuse the old name: Mantle answers "Unsupported parameter:
// 'max_tokens' is not supported with this model." A generic Chat Completions
// client sends max_tokens to every model - @ai-sdk/openai-compatible, which
// opencode's OpenAI-compatible provider is, has no other spelling - and the
// model it is talking to is a route name it has never heard of, so it has no
// way to know better. Without this, opencode on a GPT-5.6 route has every
// request refused, and the engineer is told only that the provider rejected it.
//
// The two fields carry one number with one meaning, so this is the dialect
// translation the beta flags get, not a decision made on the engineer's
// behalf: the model is capped where the client asked. A client that sent both
// was explicit about the new one, and the old one is dropped - the one case in
// which a number the client sent does not arrive, so that one is said at WARN.
func (m *Mantle) spellCompletionCap(ctx context.Context, model string, fields map[string]json.RawMessage) {
	legacy, ok := fields["max_tokens"]
	if !ok {
		return
	}
	delete(fields, "max_tokens")
	if string(legacy) == "null" {
		return
	}
	if current, both := fields["max_completion_tokens"]; both && string(current) != "null" {
		if !bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(legacy)) {
			m.log.LogAttrs(ctx, slog.LevelWarn, "dropped max_tokens from a request that also set max_completion_tokens",
				slog.String("provider", MantleName),
				slog.String("model", model),
				slog.String("max_tokens", string(legacy)),
				slog.String("max_completion_tokens", string(current)),
			)
		}
		return
	}
	fields["max_completion_tokens"] = legacy
}

// classifyResponse turns a non-200 answer into a provider error, and names the
// beta flags when the provider refused one without saying which.
func (m *Mantle) classifyResponse(ctx context.Context, status int, body []byte, betas []string) error {
	out := classifyMantle(status, body)
	if len(betas) > 0 && bytes.Contains(bytes.ToLower(body), []byte("beta")) {
		m.log.LogAttrs(ctx, slog.LevelError, "bedrock-mantle rejected a request that carried beta flags",
			slog.String("provider", MantleName),
			slog.String("forwarded", strings.Join(m.filterBetas(ctx, betas), ",")),
			slog.String("note", "if one of these is not supported, add it to the provider's unsupported "+
				"beta flags in Settings, Model providers"),
		)
	}
	return out
}

// mantleErrorBody reads the three error shapes bedrock-mantle has been seen to
// return: OpenAI's {"error":{...}}, Anthropic's {"type":"error","error":{...}},
// and the AWS service shape {"message": ...}.
type mantleErrorBody struct {
	Error *struct {
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// classifyMantle maps a status and body onto the six kinds.
//
// The provider's text travels in Err for the log and is never relayed: it names
// the account's quotas and the model's internals, and the client cannot act on
// it.
func classifyMantle(status int, body []byte) error {
	var parsed mantleErrorBody
	_ = json.Unmarshal(body, &parsed)

	errType, message := "", parsed.Message
	if parsed.Error != nil {
		errType = parsed.Error.Type
		if parsed.Error.Message != "" {
			message = parsed.Error.Message
		}
	}
	detail := strings.TrimSpace(fmt.Sprintf("%d %s: %s", status, errType, truncate(message, 512)))
	cause := errors.New("bedrock-mantle: " + detail)

	switch {
	case status == http.StatusTooManyRequests || errType == "rate_limit_error":
		return &providers.Error{Kind: providers.ErrKindThrottled, StatusCode: http.StatusTooManyRequests,
			Message: "the model is rate limited right now", Detail: detail, Err: cause}
	case status == http.StatusServiceUnavailable || status == 529 || errType == "overloaded_error":
		return &providers.Error{Kind: providers.ErrKindUnavailable, StatusCode: http.StatusServiceUnavailable,
			Message: "the model is temporarily unavailable", Detail: detail, Err: cause}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &providers.Error{Kind: providers.ErrKindForbidden, StatusCode: http.StatusBadGateway,
			Message: "the gateway is not permitted to invoke this model", Detail: detail, Err: cause}
	case status == http.StatusNotFound:
		return &providers.Error{Kind: providers.ErrKindNotFound, StatusCode: http.StatusBadGateway,
			Message: "the model is not available to this deployment", Detail: detail, Err: cause}
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge ||
		status == http.StatusUnprocessableEntity:
		return &providers.Error{Kind: providers.ErrKindInvalidRequest, StatusCode: http.StatusBadRequest,
			Message: "the provider rejected the request", Detail: detail, Err: cause}
	default:
		return &providers.Error{Kind: providers.ErrKindUpstream, StatusCode: http.StatusBadGateway,
			Message: "the model provider returned an error", Detail: detail, Err: cause}
	}
}

// transportError classifies a failure to reach the endpoint at all.
func transportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return &providers.Error{
		Kind:       providers.ErrKindUpstream,
		StatusCode: http.StatusBadGateway,
		Message:    "the model provider could not be reached",
		Err:        fmt.Errorf("bedrock-mantle: %w", err),
	}
}

func invalid(message string, err error) error {
	return &providers.Error{
		Kind:       providers.ErrKindInvalidRequest,
		StatusCode: http.StatusBadRequest,
		Message:    message,
		Err:        err,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// mantleStream relays a bedrock-mantle server-sent event stream.
//
// Events are handed on one at a time as they arrive; nothing is accumulated but
// a handful of integers.
//
// It stops at the stream's own end marker rather than at the end of the body,
// and that is load-bearing: bedrock-mantle has been observed holding the
// connection open after `data: [DONE]`, so a reader waiting for EOF waits for a
// timeout instead of finishing the request.
type mantleStream struct {
	api   providers.API
	body  io.ReadCloser
	sse   *bufio.Reader
	trace *connTrace

	mu    sync.Mutex
	usage providers.Usage
	// finished is the stream's own end marker having arrived: nothing more is
	// read. complete is weaker - enough of the response has arrived that the
	// body closing now is an ordinary end rather than a truncation.
	finished bool
	complete bool
	closed   bool
}

// Diagnostics reports the connection this stream was served over.
func (s *mantleStream) Diagnostics() []slog.Attr { return s.trace.attrs() }

// Next returns the next event payload, or io.EOF at the end of the stream.
func (s *mantleStream) Next() ([]byte, error) {
	for {
		s.mu.Lock()
		done := s.finished
		s.mu.Unlock()
		if done {
			return nil, io.EOF
		}

		event, data, err := readEvent(s.sse)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, s.endedEarly()
			}
			return nil, transportError(err)
		}
		if len(data) == 0 {
			// A comment, a keep-alive, or an event with no payload.
			continue
		}
		if string(data) == "[DONE]" {
			s.finish()
			return nil, io.EOF
		}

		if perr := s.observe(event, data); perr != nil {
			return nil, perr
		}
		return data, nil
	}
}

// observe reads usage and the end of the stream out of one event, and turns an
// in-band error into a provider error.
func (s *mantleStream) observe(event string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.api {
	case providers.APIAnthropicMessages:
		if event == "error" || bytes.HasPrefix(bytes.TrimSpace(data), []byte(`{"type":"error"`)) {
			return inBandError(data)
		}
		observeAnthropic(&s.usage, data)
		if event == "message_stop" || bytes.Contains(data, []byte(`"type":"message_stop"`)) {
			s.finished, s.complete = true, true
		}
	case providers.APIOpenAIChat:
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte(`{"error"`)) {
			return inBandError(data)
		}
		if observeChatChunk(&s.usage, data) {
			s.complete = true
		}
	case providers.APIOpenAIResponses:
		if event == "error" {
			return inBandError(data)
		}
		if observeResponsesEvent(&s.usage, data) {
			s.finished, s.complete = true, true
		}
	}
	return nil
}

// finish marks the stream as having reached its own end.
func (s *mantleStream) finish() {
	s.mu.Lock()
	s.finished, s.complete = true, true
	s.mu.Unlock()
}

// endedEarly decides what a body ending means. After the stream's own end it is
// the ordinary close; before it, the response was cut short, and the client is
// told in band rather than handed half a response presented as whole.
func (s *mantleStream) endedEarly() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.complete {
		s.finished = true
		return io.EOF
	}
	return &providers.Error{
		Kind:       providers.ErrKindUpstream,
		StatusCode: http.StatusBadGateway,
		Message:    "the model provider ended the response early",
		Err:        errors.New("bedrock-mantle: stream ended before its end marker"),
	}
}

// Usage returns the accounting observed so far.
func (s *mantleStream) Usage() providers.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// Close releases the stream.
func (s *mantleStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.body.Close()
}

// readEvent reads one server-sent event: the lines up to a blank one.
//
// Only `event:` and `data:` are read; several data lines are joined with a
// newline, as the specification says. A body that ends mid-event returns what
// it has, and io.EOF on the call after.
func readEvent(r *bufio.Reader) (event string, data []byte, err error) {
	var lines [][]byte
	for {
		line, rerr := r.ReadBytes('\n')
		trimmed := bytes.TrimRight(line, "\r\n")

		if len(trimmed) == 0 {
			if len(lines) > 0 || event != "" {
				return event, bytes.Join(lines, []byte("\n")), nil
			}
			if rerr != nil {
				return "", nil, rerr
			}
			continue
		}

		switch {
		case bytes.HasPrefix(trimmed, []byte("data:")):
			lines = append(lines, bytes.TrimPrefix(bytes.TrimPrefix(trimmed, []byte("data:")), []byte(" ")))
		case bytes.HasPrefix(trimmed, []byte("event:")):
			event = string(bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("event:"))))
		}

		if rerr != nil {
			if len(lines) > 0 {
				return event, bytes.Join(lines, []byte("\n")), nil
			}
			return "", nil, rerr
		}
	}
}

// inBandError classifies an error event a stream carried after it began.
func inBandError(data []byte) error {
	var parsed mantleErrorBody
	_ = json.Unmarshal(data, &parsed)
	status := http.StatusBadGateway
	if parsed.Error != nil {
		switch parsed.Error.Type {
		case "rate_limit_error":
			status = http.StatusTooManyRequests
		case "overloaded_error":
			status = http.StatusServiceUnavailable
		case "invalid_request_error":
			status = http.StatusBadRequest
		}
	}
	return classifyMantle(status, data)
}

// openAIUsage is the usage object both OpenAI APIs report, under their two
// spellings: Chat Completions says prompt and completion, Responses says input
// and output.
type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

// usage converts OpenAI accounting into the four classes.
//
// The cached tokens are *inside* the input figure in both APIs, where
// Anthropic's are beside it - so they are subtracted, or every cache read would
// be billed twice: once at the input rate and again at the cache-read rate.
// The same goes for cache writes where the model reports them (the GPT-5.6
// family does). Reasoning tokens are inside output and billed at the output
// rate, once.
func (u openAIUsage) usage() providers.Usage {
	input := u.PromptTokens + u.InputTokens
	output := u.CompletionTokens + u.OutputTokens

	var cached, written int64
	if d := u.PromptTokensDetails; d != nil {
		cached += d.CachedTokens
		written += d.CacheWriteTokens
	}
	if d := u.InputTokensDetails; d != nil {
		cached += d.CachedTokens
		written += d.CacheWriteTokens
	}

	uncached := input - cached - written
	if uncached < 0 {
		// A provider reporting more cached than it received. Not billed as a
		// negative; the cache figures stand as reported.
		uncached = 0
	}
	return providers.Usage{
		InputTokens:      uncached,
		OutputTokens:     output,
		CacheReadTokens:  cached,
		CacheWriteTokens: written,
	}
}

// observeChatChunk reads a Chat Completions stream chunk, and reports whether
// it is one after which the body closing is an ordinary end.
//
// Usage arrives in one chunk near the end - with stream_options.include_usage,
// which the data plane asks for - as a chunk whose choices are empty. The
// stream is not over at that chunk, or at a finish reason: [DONE] ends it.
func observeChatChunk(u *providers.Usage, data []byte) bool {
	if !bytes.Contains(data, []byte(`"usage"`)) && !bytes.Contains(data, []byte(`"finish_reason"`)) {
		return false
	}
	var chunk struct {
		Usage   *openAIUsage `json:"usage"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return false
	}
	complete := false
	if chunk.Usage != nil {
		*u = chunk.Usage.usage()
		complete = true
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			complete = true
		}
	}
	return complete
}

// observeResponsesEvent reads a Responses stream event, and reports whether it
// is the terminal one. Usage arrives once, on that event, inside the response
// object it restates.
func observeResponsesEvent(u *providers.Usage, data []byte) bool {
	if !bytes.Contains(data, []byte(`"response.`)) {
		return false
	}
	var env struct {
		Type     string `json:"type"`
		Response *struct {
			Usage *openAIUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return false
	}
	switch env.Type {
	case "response.completed", "response.incomplete", "response.failed":
		if env.Response != nil && env.Response.Usage != nil {
			*u = env.Response.Usage.usage()
		}
		return true
	}
	return false
}

// usageOf reads the usage from a complete, non-streamed response.
func usageOf(api providers.API, body []byte) providers.Usage {
	switch api {
	case providers.APIOpenAIChat, providers.APIOpenAIResponses:
		var env struct {
			Usage *openAIUsage `json:"usage"`
		}
		if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
			return providers.Usage{}
		}
		return env.Usage.usage()
	default:
		return usageFromResponse(body)
	}
}
