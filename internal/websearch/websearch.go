// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package websearch is web search offered to every coding tool as a tool of
// its own, served from this deployment rather than from a model provider.
//
// A coding tool's built-in search is usually a server-side tool on its
// vendor's API: Claude Code asks Anthropic's API for `web_search_20250305`,
// Codex asks OpenAI's Responses API for `web_search`. Bedrock refuses the
// first outright and offers the second only for some models on one endpoint,
// so behind this gateway most routes have no search at all. Emulating either
// server-side tool inside the proxy would put a tool loop on the streaming
// path and translate one vendor's dialect into another's, which are the two
// things the data plane is built not to do.
//
// So search is a tool the harness calls, over MCP, at the gateway's /mcp
// endpoint. Every model on every route can use it, because to the model it is
// an ordinary client tool; every supported coding tool speaks MCP; and the
// gateway authenticates the call, applies the caller's budget, and records it
// beside the requests it served. The search itself is done by a Backend.
//
// Nothing here keeps a query or a result. A query is written by a model from
// the conversation, so it is prompt content, and the audit row for a search
// holds a count, a duration, an outcome and a cost.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// Kind names a backend implementation, and is the identity of its row in the
// web search configuration.
type Kind string

// KindAgentCore is the Web Search Tool on Amazon Bedrock AgentCore Gateway:
// Amazon's own web index, reached with the deployment's AWS role, so a query
// never leaves AWS and there is no key to hold.
const KindAgentCore Kind = "bedrock-agentcore"

// Kinds are the backends this build can construct, in the order the console
// lists them. The database's CHECK constraint holds the same list.
var Kinds = []Kind{KindAgentCore}

// Valid reports whether k is a backend this build knows.
func (k Kind) Valid() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// PublishedPrice is what AWS charges per search on the AgentCore Web Search
// Tool: $7 per 1,000 queries, pay as you go, as published when the tool
// launched. It is the default an administrator can override in Settings,
// because the Price List API does not carry it and a price that changes is
// corrected on a screen rather than by a release.
const PublishedPrice pricing.Money = 7_000_000

// Limits the tool enforces before a backend is asked, so a query the backend
// would refuse is refused here with a message the model can act on.
const (
	// MaxQueryChars is AgentCore's limit, and a sensible one for any backend:
	// a query longer than this is a paragraph of the conversation, not a
	// search.
	MaxQueryChars = 200
	// DefaultResults and MaxResults bound how many results one call returns.
	DefaultResults = 10
	MaxResults     = 25
	// MaxDomains is how many domains either filter list may hold.
	MaxDomains = 100
)

// Query is one search, as the model asked for it.
type Query struct {
	Text       string
	MaxResults int
	// Include restricts results to these domains, and Exclude drops results
	// from them. A root domain matches its subdomains.
	Include []string
	Exclude []string
	// PublishedAfter and PublishedBefore bound the publication date, and are
	// zero when the model set no bound.
	PublishedAfter  time.Time
	PublishedBefore time.Time
}

// Validate reports what is wrong with a query, in words the model reads.
func (q Query) Validate() error {
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return errors.New("query is required")
	}
	if n := utf8.RuneCountInString(text); n > MaxQueryChars {
		return fmt.Errorf("query is %d characters; the limit is %d. Search for the key terms rather than a sentence", n, MaxQueryChars)
	}
	if q.MaxResults < 0 || q.MaxResults > MaxResults {
		return fmt.Errorf("max_results must be between 1 and %d", MaxResults)
	}
	if len(q.Include) > MaxDomains || len(q.Exclude) > MaxDomains {
		return fmt.Errorf("a domain list may hold at most %d domains", MaxDomains)
	}
	if !q.PublishedAfter.IsZero() && !q.PublishedBefore.IsZero() && q.PublishedAfter.After(q.PublishedBefore) {
		return errors.New("published_after is later than published_before")
	}
	return nil
}

// Result is one search result. Every field but URL may be empty.
type Result struct {
	Title     string `json:"title,omitempty"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet,omitempty"`
	Published string `json:"published,omitempty"`
}

// Backend performs searches.
type Backend interface {
	// Kind names the implementation.
	Kind() Kind
	// Search runs one query. A query that matches nothing is an empty result
	// and not an error.
	Search(ctx context.Context, q Query) ([]Result, error)
	// Check reports whether the backend can be reached with this deployment's
	// credentials, without running a search anybody pays for.
	Check(ctx context.Context) error
}

// ErrorKind classifies a backend failure for the audit trail and the log. It
// is never the backend's message verbatim.
type ErrorKind string

const (
	// ErrKindForbidden is a credential or permission failure: the gateway's
	// role cannot invoke the backend. The deployment's to fix.
	ErrKindForbidden ErrorKind = "forbidden"
	// ErrKindThrottled is the backend's rate limit. The model may retry.
	ErrKindThrottled ErrorKind = "throttled"
	// ErrKindUnavailable is the backend failing or unreachable.
	ErrKindUnavailable ErrorKind = "unavailable"
	// ErrKindMisconfigured is a backend that answers but is not set up to
	// search: no web search target on the gateway, for instance.
	ErrKindMisconfigured ErrorKind = "misconfigured"
	// ErrKindRejected is the backend refusing this query.
	ErrKindRejected ErrorKind = "rejected"
)

// Error is a classified backend failure.
type Error struct {
	Kind ErrorKind
	// Message is written for the operator's log. The model is told something
	// shorter, chosen by Kind, because a message naming an IAM action helps
	// nobody inside a coding session.
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("web search: %s: %s: %v", e.Kind, e.Message, e.Err)
	}
	return fmt.Sprintf("web search: %s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the classification of err, and ErrKindUnavailable for an
// error no backend classified.
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ErrKindUnavailable
}

// Descriptor is what the console knows about a backend kind without
// importing its implementation, the way a model provider's descriptor works.
type Descriptor struct {
	Kind  Kind
	Label string
	// Endpoint checks what an administrator typed and returns it normalised,
	// with the region it is in and a short name for it.
	Endpoint func(raw string) (Endpoint, error)
	// RolePolicy is what the deployment's role needs to reach the backend,
	// rendered on the settings screen beside the field.
	RolePolicy any
}

// Endpoint is a backend's address, as its descriptor understood it.
type Endpoint struct {
	URL    string
	Region string
	Name   string
}

// Deployment is a gateway the deployment created for itself and named in its
// configuration. The zero value is a deployment that named none, which leaves
// web search wholly to the console.
type Deployment struct {
	Kind       Kind
	GatewayURL string
}

// Set reports whether the deployment named a gateway.
func (d Deployment) Set() bool { return d.GatewayURL != "" }
