// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package fakebedrock serves Amazon Bedrock's wire protocol, for tests.
//
// It exists because everything in this repository that stands in for a model
// backend does so at the providers.Provider interface, which means the whole of
// internal/providers/bedrock has never executed in a test: not the SigV4
// signing, not the event-stream frame decoding, not classify's mapping of an
// SDK error onto a providers.ErrorKind, and not prepareBody's translation of
// beta flags between the two dialects. A double at the interface re-tests what
// internal/dataplane/anthropic_test.go already covers. This one sits below the
// SDK, so all of it runs.
//
// Nothing in the product had to change to make that possible. The provider
// builds its clients with awsconfig.LoadDefaultConfig, which populates
// cfg.ConfigSources from the environment, and the SDK's own service-specific
// endpoint override reads it from there:
//
//	AWS_ENDPOINT_URL_BEDROCK_RUNTIME   InvokeModel, InvokeModelWithResponseStream
//	AWS_ENDPOINT_URL_BEDROCK           ListFoundationModels, ListInferenceProfiles
//
// So a test points those at this server and the real provider talks to it. That
// is deliberately better than adding an endpoint field to bedrock.Config: a
// configuration key that only tests use is a production surface with no
// production reason to exist, and an operator who found it could point a
// deployment's model traffic at anything.
//
// # What it is not
//
// The signature is not verified. The SDK still computes one - that path is
// exercised - but nothing here checks it, so this proves nothing about
// authentication to AWS and must never be read as though it did. Dummy
// credentials in the environment are enough to drive it.
package fakebedrock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Usage is the accounting every invocation reports.
//
// It is fixed rather than derived from the request, because the point of the
// numbers is that a test can multiply them by a rate from pricing.Seed() and
// assert the cost column exactly. A fake that counted tokens would make the
// expected cost a second implementation of the thing under test.
type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	// CacheWrite5mTokens and CacheWrite1hTokens are reported separately because
	// Bedrock charges them at different rates and reports the split in
	// cache_creation. A single cache-write number cannot be costed correctly.
	CacheWrite5mTokens int64
	CacheWrite1hTokens int64
}

// Total returns the flat cache-write figure Bedrock reports alongside the split.
func (u Usage) Total() int64 { return u.CacheWrite5mTokens + u.CacheWrite1hTokens }

// DefaultUsage covers all four token classes, and both cache-write TTLs.
//
// Every class is non-zero on purpose: a zero is indistinguishable from a class
// the pricing path dropped, so a fake that reported none of them would let the
// cache-aware cost model regress without failing anything.
var DefaultUsage = Usage{
	InputTokens:        1000,
	OutputTokens:       500,
	CacheReadTokens:    800,
	CacheWrite5mTokens: 200,
	CacheWrite1hTokens: 100,
}

// Received is one request as this server was handed it.
//
// Body is what the provider sent, which is not what the client sent: prepareBody
// has already stripped model and stream, added anthropic_version, and filtered
// the beta flags. Recording it here is how a test asserts that translation
// without needing AWS to refuse anything.
type Received struct {
	Model     string          `json:"model"`
	Streaming bool            `json:"streaming"`
	Body      json.RawMessage `json:"body"`
	At        time.Time       `json:"at"`

	// Endpoint is EndpointMantle for a request that arrived on a Mantle path,
	// and empty for bedrock-runtime. Path is the Mantle path it arrived on.
	Endpoint string `json:"endpoint,omitempty"`
	Path     string `json:"path,omitempty"`
	// SigningService and SigningRegion are read from a Mantle request's SigV4
	// credential scope - not verified, but what a test asserts the provider
	// signed for.
	SigningService   string   `json:"signing_service,omitempty"`
	SigningRegion    string   `json:"signing_region,omitempty"`
	AnthropicVersion string   `json:"anthropic_version,omitempty"`
	Betas            []string `json:"betas,omitempty"`
}

// Fault is an error this server can be told to return instead of a response.
type Fault string

// The faults, named for the Bedrock exception each produces. classify in
// internal/providers/bedrock maps these onto providers.ErrorKind. On a Mantle
// path each is answered with the HTTP status and error body Mantle uses for
// the same failure.
const (
	FaultThrottling      Fault = "throttling"
	FaultUnavailable     Fault = "unavailable"
	FaultAccessDenied    Fault = "access-denied"
	FaultValidation      Fault = "validation"
	FaultModelNotReady   Fault = "model-not-ready"
	FaultResourceMissing Fault = "resource-missing"
	// FaultMidStream sends a well-formed opening event and then an exception
	// frame. The response has already begun, which is the case the data plane
	// reports in band rather than by status code.
	FaultMidStream Fault = "mid-stream"
)

// Always arms a fault for every matching request until Reset.
//
// It is the setting most tests want, and the reason is not obvious: the AWS SDK
// retries a throttle or a 5xx by itself, three attempts by default, before the
// provider ever sees an error. A fault armed once is therefore consumed by the
// first attempt and the second attempt succeeds, so the call returns a response
// and the test reads as though classify were broken. Count is attempts at the
// wire, not calls to Invoke.
const Always = -1

// scenario is one armed fault, consumed by the requests it matches.
type scenario struct {
	Model     string `json:"model"`
	Fault     Fault  `json:"fault"`
	Remaining int    `json:"count"`
}

// Server serves the Bedrock endpoints Panorama calls.
//
// The zero value is not usable; call New.
type Server struct {
	mu        sync.Mutex
	usage     Usage
	text      string
	received  []Received
	scenarios []scenario
	models    []CatalogueModel
	profiles  []CatalogueProfile
	// mantleModels is what Mantle's /v1/models reports.
	mantleModels []MantleModel
	// lastSigning is the service and region the last Mantle catalogue call
	// was signed for.
	lastSigning [2]string
	// frameDelay spaces the events of a streamed response. Zero sends them as
	// fast as they encode, which is right for most tests and useless for the
	// one that has to observe that the proxy flushed rather than buffered.
	frameDelay time.Duration

	// streamsCompleted and streamFailures record how streams ended.
	//
	// A truncated response has two possible owners - this server stopped
	// writing, or the bytes were lost between here and the provider - and the
	// gateway's log cannot tell them apart. Counting completions here does:
	// if every stream finished and the client saw a short one, the loss is in
	// transit.
	streamsCompleted int
	streamFailures   []string

	// streamsPerConn counts streamed invocations by the client connection they
	// arrived on, which is what makes "the failing connection was reused"
	// mean anything.
	//
	// Under this load nearly every connection after the first few is a reused
	// one, so a handful of failures all reporting reuse may be the base rate
	// rather than a finding. The rate is not visible from the gateway's side,
	// because the trace there is only emitted for requests that failed - a
	// sample of failures cannot say what the population looks like.
	//
	// Keyed on the remote address, which is the client's ephemeral port and so
	// identifies the connection for as long as it is open. A port freed and
	// re-allocated inside one run would be counted as reuse; the connection
	// count is reported alongside so that stays checkable rather than
	// invisible.
	streamsPerConn map[string]int
	streamRequests int
	streamsReused  int
}

// Option configures a Server.
type Option func(*Server)

// WithUsage sets the accounting every invocation reports.
func WithUsage(u Usage) Option { return func(s *Server) { s.usage = u } }

// WithText sets the assistant text a completion returns.
func WithText(text string) Option { return func(s *Server) { s.text = text } }

// WithFrameDelay spaces the events of a streamed response.
func WithFrameDelay(d time.Duration) Option { return func(s *Server) { s.frameDelay = d } }

// WithCatalogue replaces the models and profiles discovery reports.
func WithCatalogue(models []CatalogueModel, profiles []CatalogueProfile) Option {
	return func(s *Server) {
		s.models = models
		s.profiles = profiles
	}
}

// New builds a server with the default catalogue and accounting.
func New(opts ...Option) *Server {
	s := &Server{
		usage:        DefaultUsage,
		text:         "OK",
		models:       DefaultModels(),
		profiles:     DefaultProfiles(),
		mantleModels: DefaultMantleModels(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Received returns every request this server has been handed, oldest first.
func (s *Server) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Received, len(s.received))
	copy(out, s.received)
	return out
}

// Last returns the most recent request, and whether there was one.
func (s *Server) Last() (Received, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.received) == 0 {
		return Received{}, false
	}
	return s.received[len(s.received)-1], true
}

// Arm queues a fault for the next count requests naming model.
//
// An empty model matches any, and Always matches every request until Reset.
// Faults are consumed in the order they were armed, so a test can arm a throttle
// for one model and leave another healthy.
//
// Count is requests that reach this server, which is not the same as calls to
// Invoke: see Always.
func (s *Server) Arm(model string, f Fault, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scenarios = append(s.scenarios, scenario{Model: model, Fault: f, Remaining: count})
}

// Reset clears the armed faults and the recorded requests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = nil
	s.scenarios = nil
	s.streamsPerConn = nil
	s.streamRequests = 0
	s.streamsReused = 0
}

// take returns the fault armed for model, consuming one use of it.
func (s *Server) take(model string) Fault {
	for i := range s.scenarios {
		sc := &s.scenarios[i]
		if sc.Remaining == 0 {
			continue
		}
		if sc.Model != "" && sc.Model != model {
			continue
		}
		if sc.Remaining > 0 {
			sc.Remaining--
		}
		return sc.Fault
	}
	return ""
}

// record appends a request and returns the fault it should meet, if any. One
// lock covers both so a concurrent request cannot take the fault armed for this
// one and leave it recorded as though it had been served normally.
func (s *Server) record(r Received) Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, r)
	return s.take(r.Model)
}

func (s *Server) currentUsage() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *Server) currentText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text
}

func (s *Server) currentDelay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frameDelay
}

// noteConnection records that a streamed invocation arrived on a connection.
func (s *Server) noteConnection(remoteAddr string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.streamsPerConn == nil {
		s.streamsPerConn = make(map[string]int)
	}
	s.streamsPerConn[remoteAddr]++
	s.streamRequests++
	if s.streamsPerConn[remoteAddr] > 1 {
		s.streamsReused++
	}
}

// Reuse reports how this server's streamed invocations were spread over client
// connections: how many there were, how many arrived on a connection that had
// already served one, and how many distinct connections carried them.
func (s *Server) Reuse() (total, reused, connections int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamRequests, s.streamsReused, len(s.streamsPerConn)
}

// streamEnded records how one streamed response finished.
//
// The frame count travels with the error because "the fake stopped writing" and
// "the fake wrote everything and the bytes went missing afterwards" are the two
// halves of any short stream, and the error text alone says which only by
// accident.
func (s *Server) streamEnded(written, total int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.streamFailures = append(s.streamFailures,
			fmt.Sprintf("after %d of %d frames: %v", written, total, err))
		return
	}
	s.streamsCompleted++
}

// Streams reports how many streamed responses this server wrote in full, and
// the errors that stopped the rest.
func (s *Server) Streams() (completed int, failures []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamsCompleted, append([]string(nil), s.streamFailures...)
}

// WithMantleCatalogue replaces the models Mantle's /v1/models reports.
func WithMantleCatalogue(models []MantleModel) Option {
	return func(s *Server) { s.mantleModels = models }
}

// ServeHTTP routes the Bedrock calls this product makes, and nothing else.
//
// The paths are matched by hand rather than through http.ServeMux because a
// model id is a path segment containing dots and, for some models, a colon -
// escaped by the SDK on the way out. Unescaping it here keeps what a test arms
// and what the provider asked for the same string.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()

	if s.handleMantle(w, r) {
		return
	}

	switch {
	case strings.HasPrefix(path, "/model/"):
		s.handleModel(w, r, strings.TrimPrefix(path, "/model/"))
	case path == "/foundation-models" && r.Method == http.MethodGet:
		s.handleFoundationModels(w, r)
	case path == "/inference-profiles" && r.Method == http.MethodGet:
		s.handleInferenceProfiles(w, r)
	case path == "/_fake/received" && r.Method == http.MethodGet:
		writeJSON(w, s.Received())
	case path == "/_fake/scenario" && r.Method == http.MethodPost:
		s.handleArm(w, r)
	case path == "/_fake/reset" && r.Method == http.MethodPost:
		s.Reset()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		writeAWSError(w, http.StatusNotFound, "ResourceNotFoundException",
			fmt.Sprintf("fakebedrock: no route for %s %s", r.Method, path))
	}
}

// handleModel serves both invocation shapes. The trailing path element says
// which; everything before it is the model id.
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodPost {
		writeAWSError(w, http.StatusNotFound, "ResourceNotFoundException", "fakebedrock: invocations are POST")
		return
	}

	slash := strings.LastIndex(rest, "/")
	if slash < 0 {
		writeAWSError(w, http.StatusBadRequest, "ValidationException", "fakebedrock: malformed invocation path")
		return
	}
	escaped, action := rest[:slash], rest[slash+1:]

	model, err := url.PathUnescape(escaped)
	if err != nil {
		writeAWSError(w, http.StatusBadRequest, "ValidationException", "fakebedrock: undecodable model id")
		return
	}

	streaming := action == "invoke-with-response-stream"
	if !streaming && action != "invoke" {
		writeAWSError(w, http.StatusNotFound, "ResourceNotFoundException",
			"fakebedrock: unknown action "+action)
		return
	}

	body, err := readBody(r)
	if err != nil {
		writeAWSError(w, http.StatusBadRequest, "ValidationException", "fakebedrock: "+err.Error())
		return
	}

	fault := s.record(Received{Model: model, Streaming: streaming, Body: body, At: time.Now().UTC()})

	// A fault that is not mid-stream is answered before anything is written, so
	// the SDK sees an ordinary error response and classify does its work.
	if fault != "" && fault != FaultMidStream {
		status, code, message := faultResponse(fault, model)
		writeAWSError(w, status, code, message)
		return
	}

	if streaming {
		s.noteConnection(r.RemoteAddr)
		s.writeStream(w, fault == FaultMidStream)
		return
	}
	s.writeCompletion(w)
}

func (s *Server) handleArm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Fault Fault  `json:"fault"`
		Count int    `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAWSError(w, http.StatusBadRequest, "ValidationException", "fakebedrock: "+err.Error())
		return
	}
	if req.Count == 0 {
		// Nothing useful is ever meant by "arm this zero times", and a caller
		// that omitted the field wants the fault to hold - the SDK's own
		// retries would otherwise consume a single use before the provider saw
		// it. See Always.
		req.Count = Always
	}
	s.Arm(req.Model, req.Fault, req.Count)
	writeJSON(w, map[string]bool{"ok": true})
}

// faultResponse maps a fault onto the wire shape classify reads.
//
// The status codes are the ones Bedrock actually returns; the SDK matches the
// exception on the X-Amzn-Errortype header, and the status is what a reader of
// a failing test will look at first.
func faultResponse(f Fault, model string) (status int, code, message string) {
	switch f {
	case FaultThrottling:
		return http.StatusTooManyRequests, "ThrottlingException",
			"Too many requests, please wait before trying again."
	case FaultUnavailable:
		return http.StatusServiceUnavailable, "ServiceUnavailableException",
			"The service is unavailable. Try again later."
	case FaultAccessDenied:
		return http.StatusForbidden, "AccessDeniedException",
			"You don't have access to the model with the specified model ID."
	case FaultValidation:
		return http.StatusBadRequest, "ValidationException",
			"The provided request is not valid."
	case FaultModelNotReady:
		return http.StatusTooManyRequests, "ModelNotReadyException",
			"Model is not ready for inference."
	case FaultResourceMissing:
		return http.StatusNotFound, "ResourceNotFoundException",
			"Could not resolve the foundation model from the provided model identifier: " + model
	default:
		return http.StatusInternalServerError, "InternalServerException",
			"fakebedrock: unknown fault " + string(f)
	}
}

// writeAWSError writes the restJson1 error shape the SDK deserializes.
//
// Both the header and the body carry the type: the SDK prefers the header, and
// the body is what makes a captured response readable to a person.
func writeAWSError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-Errortype", code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": code, "message": message})
}

// writeJSON writes a successful JSON response. Failures go through
// writeAWSError, which is the only thing here that answers with another status.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
