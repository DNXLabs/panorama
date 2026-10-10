// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/dnxlabs/panorama/internal/websearch"
)

// agentCoreService is the SigV4 signing name of an AgentCore gateway.
const agentCoreService = "bedrock-agentcore"

// agentCoreEndpointEnv redirects gateway calls to a test double, for the same
// reason AWS_ENDPOINT_URL_BEDROCK_MANTLE exists: the SDK convention, read by
// hand because this is plain HTTP rather than an SDK client. It is logged at
// WARN whenever it is set, because a variable that sends a deployment's
// signed requests somewhere else is worth noticing, and it is never a console
// field.
const agentCoreEndpointEnv = "AWS_ENDPOINT_URL_BEDROCK_AGENTCORE"

// webSearchToolSuffix is how the gateway names the connector's tool: the
// target's name, three underscores, and the connector's own tool name.
const webSearchToolSuffix = "___WebSearch"

// maxSnippetChars bounds one result's text. The index returns roughly two
// kilobytes of extracted passages per result, which is ten results and twenty
// kilobytes of a model's context per search; a coding agent searching for an
// API needs the first paragraph, and can search again.
const maxSnippetChars = 1200

// maxGatewayResponse bounds a gateway response read into memory.
const maxGatewayResponse = 4 << 20

// gatewayHost is the shape of an AgentCore gateway's host name. Only a host of
// this shape is accepted from the console, because the gateway is called with
// a SigV4 signature made from the deployment's role: a field that accepted any
// URL would be a way to send signed requests to a host of somebody's choosing.
var gatewayHost = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*)\.gateway\.bedrock-agentcore\.([a-z]{2}(?:-[a-z]+)+-\d{1,2})\.amazonaws\.com$`)

// Gateway is an AgentCore gateway, as identified by the URL its console shows.
type Gateway struct {
	ID     string
	Region string
	// URL is the gateway's MCP endpoint, normalised.
	URL string
}

// ParseGatewayURL reads a gateway's MCP URL, in the form the AgentCore console
// and CreateGateway return it:
// https://<gateway-id>.gateway.bedrock-agentcore.<region>.amazonaws.com/mcp
func ParseGatewayURL(raw string) (Gateway, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return Gateway{}, fmt.Errorf("%q is not an https URL. Copy the gateway URL from the AgentCore console; it ends in /mcp", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
		return Gateway{}, errors.New("a gateway URL has no credentials, port, query or fragment")
	}
	m := gatewayHost.FindStringSubmatch(strings.ToLower(u.Hostname()))
	if m == nil {
		return Gateway{}, fmt.Errorf("%s is not an AgentCore gateway host. It looks like <gateway-id>.gateway.bedrock-agentcore.<region>.amazonaws.com", u.Hostname())
	}
	if path := strings.TrimSuffix(u.Path, "/"); path != "/mcp" {
		return Gateway{}, errors.New("a gateway URL ends in /mcp")
	}
	return Gateway{
		ID:     m[1],
		Region: m[2],
		URL:    "https://" + strings.ToLower(u.Hostname()) + "/mcp",
	}, nil
}

// WebSearch searches the web through the Web Search Tool connector on an
// AgentCore gateway.
//
// The gateway is configured with IAM inbound authorization, so a call is a
// JSON-RPC request signed with the deployment's own role, which needs
// bedrock-agentcore:InvokeGateway and nothing else: the gateway's service role
// is what holds InvokeWebSearch. The search runs on Amazon's own index, inside
// AWS, and there is no key anywhere.
//
// Checked against a live gateway rather than read from a document: the gateway
// answers tools/list and tools/call without an initialize handshake, replies
// with plain JSON, and returns results as a JSON document inside the text of
// the call's single content block.
type WebSearch struct {
	gateway  Gateway
	endpoint string
	creds    aws.CredentialsProvider
	signer   *v4.Signer
	client   *http.Client
	log      *slog.Logger
	now      func() time.Time

	ids atomic.Int64

	// tool is the connector's tool name on this gateway, found by tools/list
	// the first time it is needed and forgotten when the gateway says it does
	// not know it, so a renamed target costs one failed search rather than a
	// restart.
	mu   sync.Mutex
	tool string
}

// NewWebSearch builds a web search backend for the gateway at gatewayURL.
func NewWebSearch(ctx context.Context, gatewayURL string, log *slog.Logger) (*WebSearch, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	gw, err := ParseGatewayURL(gatewayURL)
	if err != nil {
		return nil, err
	}

	endpoint := gw.URL
	if override := strings.TrimSpace(os.Getenv(agentCoreEndpointEnv)); override != "" {
		u, perr := url.Parse(override)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("web search: %s=%q is not an http(s) URL", agentCoreEndpointEnv, override)
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + "/mcp"
		endpoint = u.String()
		log.Warn("web search requests are redirected by an environment variable",
			"variable", agentCoreEndpointEnv, "endpoint", endpoint,
			"note", "this exists for the end-to-end suite; a deployment should never set it")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(gw.Region))
	if err != nil {
		return nil, fmt.Errorf("web search: load aws config: %w", err)
	}

	return &WebSearch{
		gateway:  gw,
		endpoint: endpoint,
		creds:    cfg.Credentials,
		signer:   v4.NewSigner(),
		// A search is one round trip that takes a second or two. The bound is
		// here rather than only on the caller's context so that a gateway that
		// stops answering cannot hold a coding tool's MCP call open forever.
		client: &http.Client{Transport: newTransport(), Timeout: 30 * time.Second},
		log:    log,
		now:    time.Now,
	}, nil
}

// Kind names this backend.
func (w *WebSearch) Kind() websearch.Kind { return websearch.KindAgentCore }

// Gateway reports which gateway this backend calls.
func (w *WebSearch) Gateway() Gateway { return w.gateway }

// Check lists the gateway's tools and confirms the web search one is there.
// It runs no search, so it costs nothing.
func (w *WebSearch) Check(ctx context.Context) error {
	w.forget()
	_, err := w.toolName(ctx)
	return err
}

// Search runs one query.
func (w *WebSearch) Search(ctx context.Context, q websearch.Query) ([]websearch.Result, error) {
	tool, err := w.toolName(ctx)
	if err != nil {
		return nil, err
	}

	args := map[string]any{"query": strings.TrimSpace(q.Text)}
	if q.MaxResults > 0 {
		args["maxResults"] = q.MaxResults
	}
	filters := map[string]any{}
	if len(q.Include) > 0 || len(q.Exclude) > 0 {
		domains := map[string]any{}
		if len(q.Include) > 0 {
			domains["include"] = q.Include
		}
		if len(q.Exclude) > 0 {
			domains["exclude"] = q.Exclude
		}
		filters["domainFilter"] = domains
	}
	if !q.PublishedAfter.IsZero() || !q.PublishedBefore.IsZero() {
		dates := map[string]any{}
		if !q.PublishedAfter.IsZero() {
			dates["from"] = q.PublishedAfter.UTC().Format(time.RFC3339)
		}
		if !q.PublishedBefore.IsZero() {
			dates["to"] = q.PublishedBefore.UTC().Format(time.RFC3339)
		}
		filters["publishedDateFilter"] = dates
	}
	if len(filters) > 0 {
		args["filters"] = filters
	}

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := w.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, &result); err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) && rpcErr.Code == rpcInvalidParams && strings.Contains(rpcErr.Message, "Unknown tool") {
			w.forget()
			return nil, &websearch.Error{Kind: websearch.ErrKindMisconfigured,
				Message: fmt.Sprintf("the gateway no longer has the tool %s; it is looked up again on the next search", tool), Err: err}
		}
		return nil, err
	}

	text := ""
	for _, c := range result.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if result.IsError {
		// The connector's own refusal of this query - a validation failure,
		// say - which the model can act on.
		return nil, &websearch.Error{Kind: websearch.ErrKindRejected, Message: text}
	}

	var doc struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Text          string `json:"text"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, &websearch.Error{Kind: websearch.ErrKindUnavailable,
			Message: "the web search tool returned something that is not its result document", Err: err}
	}

	out := make([]websearch.Result, 0, len(doc.Results))
	for _, r := range doc.Results {
		if r.URL == "" {
			continue
		}
		out = append(out, websearch.Result{
			Title:     strings.TrimSpace(r.Title),
			URL:       r.URL,
			Snippet:   trimSnippet(strings.TrimSpace(r.Text), maxSnippetChars),
			Published: publishedDate(r.PublishedDate),
		})
	}
	return out, nil
}

// toolName finds the web search tool on the gateway.
func (w *WebSearch) toolName(ctx context.Context) (string, error) {
	w.mu.Lock()
	cached := w.tool
	w.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := w.rpc(ctx, "tools/list", map[string]any{}, &listed); err != nil {
		return "", err
	}
	var found []string
	for _, t := range listed.Tools {
		if strings.HasSuffix(t.Name, webSearchToolSuffix) {
			found = append(found, t.Name)
		}
	}
	switch len(found) {
	case 0:
		return "", &websearch.Error{Kind: websearch.ErrKindMisconfigured,
			Message: fmt.Sprintf("gateway %s has no web search target; add one with the connector web-search", w.gateway.ID)}
	case 1:
	default:
		// Two targets would be two sets of domain filters, and choosing one by
		// the order a list came back in would be choosing the filter by chance.
		return "", &websearch.Error{Kind: websearch.ErrKindMisconfigured,
			Message: fmt.Sprintf("gateway %s has %d web search targets (%s); keep one", w.gateway.ID, len(found), strings.Join(found, ", "))}
	}

	w.mu.Lock()
	w.tool = found[0]
	w.mu.Unlock()
	return found[0], nil
}

func (w *WebSearch) forget() {
	w.mu.Lock()
	w.tool = ""
	w.mu.Unlock()
}

// JSON-RPC error codes the gateway was seen to return.
const (
	rpcInvalidParams = -32602
	// rpcAuthorization is the gateway's code for a caller IAM refuses.
	rpcAuthorization = -32002
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("json-rpc %d: %s", e.Code, e.Message) }

// rpc makes one signed JSON-RPC call and decodes its result into out.
func (w *WebSearch) rpc(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      w.ids.Add(1),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("web search: encode %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("web search: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	creds, err := w.creds.Retrieve(ctx)
	if err != nil {
		return &websearch.Error{Kind: websearch.ErrKindForbidden,
			Message: "the gateway has no AWS credentials to sign the request with", Err: err}
	}
	sum := sha256.Sum256(body)
	if serr := w.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), agentCoreService, w.gateway.Region, w.now()); serr != nil {
		return fmt.Errorf("web search: sign request: %w", serr)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &websearch.Error{Kind: websearch.ErrKindUnavailable, Message: "the gateway could not be reached", Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGatewayResponse))
	if err != nil {
		return &websearch.Error{Kind: websearch.ErrKindUnavailable, Message: "reading the gateway's response", Err: err}
	}
	payload := raw
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// The MCP transport lets a server answer a POST with a stream. This
		// gateway answers with JSON; a stream is read for the one response it
		// carries rather than refused, because both are the protocol.
		payload = lastEventData(raw)
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	decodeErr := json.Unmarshal(payload, &envelope)

	if resp.StatusCode != http.StatusOK {
		detail := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if decodeErr == nil && envelope.Error != nil {
			detail += ": " + envelope.Error.Message
		}
		return &websearch.Error{Kind: statusKind(resp.StatusCode, envelope.Error), Message: w.hint(resp.StatusCode, detail)}
	}
	if decodeErr != nil {
		return &websearch.Error{Kind: websearch.ErrKindUnavailable, Message: "the gateway's response is not JSON-RPC", Err: decodeErr}
	}
	if envelope.Error != nil {
		if envelope.Error.Code == rpcAuthorization {
			return &websearch.Error{Kind: websearch.ErrKindForbidden, Message: w.hint(http.StatusForbidden, envelope.Error.Message)}
		}
		return envelope.Error
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return &websearch.Error{Kind: websearch.ErrKindUnavailable, Message: "the gateway's " + method + " result could not be read", Err: err}
	}
	return nil
}

// hint adds what an operator should look at to a failure only they can fix.
func (w *WebSearch) hint(status int, detail string) string {
	switch status {
	case http.StatusForbidden:
		return detail + fmt.Sprintf(". The deployment's role needs bedrock-agentcore:InvokeGateway on gateway %s in %s, and the gateway must use IAM inbound authorization", w.gateway.ID, w.gateway.Region)
	case http.StatusNotFound:
		return detail + fmt.Sprintf(". Gateway %s does not exist in %s", w.gateway.ID, w.gateway.Region)
	default:
		return detail
	}
}

func statusKind(status int, rpc *rpcError) websearch.ErrorKind {
	switch {
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		return websearch.ErrKindForbidden
	case rpc != nil && rpc.Code == rpcAuthorization:
		return websearch.ErrKindForbidden
	case status == http.StatusTooManyRequests:
		return websearch.ErrKindThrottled
	case status == http.StatusNotFound:
		return websearch.ErrKindMisconfigured
	default:
		return websearch.ErrKindUnavailable
	}
}

// lastEventData returns the data of the last event in a server-sent event
// stream, which is where a JSON-RPC response rides.
func lastEventData(raw []byte) []byte {
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			last = bytes.TrimSpace(data)
		}
	}
	return last
}

// publishedDate normalises the index's date to YYYY-MM-DD.
//
// The index writes dates as "05:00PM, Monday, August 31 2026, PDT", or the word
// "unknown". A date that parses is reduced to the day; one that does not is
// passed on as written rather than dropped, because the model can still read
// it; "unknown" is no date at all.
func publishedDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "unknown") {
		return ""
	}
	for _, layout := range []string{"03:04PM, Monday, January 2 2006, MST", time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return s
}

// trimSnippet cuts s to at most n characters, at a word boundary where there is
// one near the end, and marks the cut.
func trimSnippet(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)[:n]
	cut := string(runes)
	if i := strings.LastIndexAny(cut, " \n"); i > len(cut)*3/4 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + " ..."
}

// WebSearchDescriptor describes the AgentCore backend to the console.
func WebSearchDescriptor() websearch.Descriptor {
	return websearch.Descriptor{
		Kind:  websearch.KindAgentCore,
		Label: "Amazon Bedrock AgentCore Web Search",
		Endpoint: func(raw string) (websearch.Endpoint, error) {
			gw, err := ParseGatewayURL(raw)
			if err != nil {
				return websearch.Endpoint{}, err
			}
			return websearch.Endpoint{URL: gw.URL, Region: gw.Region, Name: gw.ID}, nil
		},
		RolePolicy: WebSearchPolicy,
	}
}
