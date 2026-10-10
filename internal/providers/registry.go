// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// Settings is how one provider is configured: the row an administrator edits in
// Settings, as the rest of the process reads it.
//
// There is no credential here, and for the two Bedrock kinds there never is:
// they sign with whatever the AWS SDK's default chain finds, which is the task
// role. A provider that needs a key - OpenRouter, a first-party API - will add a
// sealed column following the integrations pattern, written through a separate
// path and never returned by one.
type Settings struct {
	Kind    Kind
	Enabled bool
	// Region is where this provider is invoked. One region per provider: the
	// same provider in two regions is not something this product expresses.
	Region  string
	Options Options

	// VerifiedAt is when a live invocation last succeeded against these
	// settings, and nil when none has. Changing the region clears it, because a
	// verification is a statement about one region.
	VerifiedAt    *time.Time
	VerifiedModel string
	ConfiguredAt  time.Time
	ConfiguredBy  string
}

// Options are the per-provider settings that correct this build's snapshot of
// what a provider accepts. Every one of them exists because the provider does
// not publish the fact, so the build carries a table and the table goes stale.
type Options struct {
	// UnsupportedBetas are beta flags dropped before a request is sent. Nil
	// means the provider's built-in list; an empty list forwards everything,
	// which is how you find out what a provider has started accepting.
	UnsupportedBetas *[]string `json:"unsupported_betas,omitempty"`
	// ContextWindows overrides how much context a model is recorded as taking,
	// keyed by model family or exact id. Zero makes a model unrecorded.
	ContextWindows map[string]int `json:"context_windows,omitempty"`
	// APIs overrides which client APIs a model family speaks, keyed by family
	// or exact id. An empty list makes a model browse-only.
	APIs map[string][]API `json:"apis,omitempty"`
}

// ParseOptions reads the options column.
func ParseOptions(raw string) (Options, error) {
	var o Options
	if raw == "" {
		return o, nil
	}
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return Options{}, fmt.Errorf("providers: options are not valid JSON: %w", err)
	}
	return o, o.Validate()
}

// Encode writes the options column.
func (o Options) Encode() (string, error) {
	out, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("providers: encode options: %w", err)
	}
	return string(out), nil
}

// Validate refuses options that could only ever be a mistake.
func (o Options) Validate() error {
	var errs []error
	for key, n := range o.ContextWindows {
		if key == "" {
			errs = append(errs, errors.New("a context window override names no model"))
		}
		if n < 0 {
			errs = append(errs, fmt.Errorf("the context window for %s is negative", key))
		}
	}
	for key, list := range o.APIs {
		if key == "" {
			errs = append(errs, errors.New("an API override names no model"))
		}
		for _, a := range list {
			if !a.Valid() {
				errs = append(errs, fmt.Errorf("%q is not an API this build serves (want one of %s, %s, %s)",
					a, APIAnthropicMessages, APIOpenAIChat, APIOpenAIResponses))
			}
		}
	}
	if o.UnsupportedBetas != nil {
		for _, b := range *o.UnsupportedBetas {
			if b == "" {
				errs = append(errs, errors.New("an unsupported beta flag is empty"))
			}
		}
	}
	return errors.Join(errs...)
}

// fingerprint is what an instance was built from. Two settings with the same
// fingerprint build the same instance, so a reload that changed nothing does
// not rebuild the AWS clients underneath a request in flight.
func (s Settings) fingerprint() string {
	options, _ := s.Options.Encode()
	return fmt.Sprintf("%s|%t|%s|%s", s.Kind, s.Enabled, s.Region, options)
}

// Descriptor is what this build knows about a kind before anybody has
// configured it: what to call it, where it can run, and how to build it.
//
// Descriptors are registered in cmd/panorama and nowhere else. That is what
// keeps the AWS SDK out of every package but the one implementing a Bedrock
// kind: the console, the policy engine and the data plane are handed a
// Registry and never name an implementation.
type Descriptor struct {
	Kind Kind
	// Label is the name a screen shows.
	Label string
	// Summary says in a sentence what this provider is for, beside the other.
	Summary string
	// Regions are the regions this kind is known to be offered in, for a
	// picker. Whether a region outside the list is refused is Build's decision:
	// a list compiled into a release goes stale the day AWS adds a region.
	Regions []string
	// ValidateOptions refuses options this kind cannot honour, at the moment an
	// administrator saves them rather than the moment a build fails. Nil
	// accepts anything Options.Validate accepts.
	ValidateOptions func(Options) error
	// Build turns settings into a live instance. It is called only for enabled
	// settings, and a failure is reported on the provider rather than stopping
	// the process.
	Build func(ctx context.Context, s Settings, log *slog.Logger) (*Instance, error)
	// Hint turns the text of a failed live check into the fix, in the words of
	// the console page somebody has to open.
	Hint func(message string) string
	// Policy is the IAM policy the deployment's role needs for this kind, as
	// JSON to paste. Both Bedrock kinds share one.
	Policy string
	// Endpoint is the host this kind reaches in a region, for the egress
	// question somebody asks when a check times out.
	Endpoint func(region string) string
}

// Instance is one configured provider, built and ready to use.
type Instance struct {
	// Settings are what this instance was built from. Verification and
	// provenance can move on without a rebuild, so anything reporting those
	// reads the State a snapshot carries rather than this.
	Settings Settings
	Provider Provider
	// Catalogue discovers what this provider can invoke. Nil for a provider
	// with nothing to discover.
	Catalogue Cataloguer
	// Prices fetches published rates. Nil for a provider whose rates come only
	// from the seed and the configuration.
	Prices pricing.Source
	// APIs reports the client APIs a model on this provider speaks. It is the
	// one derivation: discovery records its answer on the catalogue row, the
	// console reads that row, and the policy engine asks this function - so a
	// route and the row beside it cannot disagree.
	APIs func(model string) []API
}

// State is a kind as the running process sees it: how it is configured, and
// what building it produced.
type State struct {
	Settings Settings
	// Instance is nil when the provider is disabled or failed to build.
	Instance *Instance
	// Err is why it failed to build, for the log and the console. A route to a
	// provider in this state is refused with a 502 naming the deployment as the
	// fault, because it is.
	Err error
}

// Serving reports whether requests can be sent to this provider.
func (s State) Serving() bool { return s.Settings.Enabled && s.Instance != nil && s.Err == nil }

// Registry builds instances from settings and keeps them until the settings
// change.
type Registry struct {
	log         *slog.Logger
	descriptors map[Kind]Descriptor

	mu    sync.Mutex
	built map[Kind]built
}

type built struct {
	fingerprint string
	instance    *Instance
	err         error
}

// NewRegistry builds a registry for the given descriptors. A descriptor for a
// kind this build does not list is a programming error, and panics.
func NewRegistry(log *slog.Logger, descriptors ...Descriptor) *Registry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Registry{
		log:         log,
		descriptors: make(map[Kind]Descriptor, len(descriptors)),
		built:       make(map[Kind]built),
	}
	for _, d := range descriptors {
		if !d.Kind.Valid() {
			panic(fmt.Sprintf("providers: a descriptor for %q, which is not a kind this build lists", d.Kind))
		}
		r.descriptors[d.Kind] = d
	}
	return r
}

// Descriptor returns what this build knows about a kind.
func (r *Registry) Descriptor(k Kind) (Descriptor, bool) {
	if r == nil {
		return Descriptor{}, false
	}
	d, ok := r.descriptors[k]
	return d, ok
}

// Descriptors lists every registered kind, in the order a screen offers them.
func (r *Registry) Descriptors() []Descriptor {
	if r == nil {
		return nil
	}
	out := make([]Descriptor, 0, len(r.descriptors))
	for _, k := range kinds {
		if d, ok := r.descriptors[k]; ok {
			out = append(out, d)
		}
	}
	return out
}

// Build returns the instance for these settings, building it only when the
// settings differ from the last build of this kind.
//
// Disabled settings build nothing. A build that failed is remembered with its
// settings, so a broken configuration is reported once per change rather than
// retried and logged on every reload.
func (r *Registry) Build(ctx context.Context, s Settings) (*Instance, error) {
	if r == nil {
		return nil, errors.New("providers: no registry")
	}
	d, ok := r.descriptors[s.Kind]
	if !ok {
		return nil, fmt.Errorf("providers: %q is not a provider this build can serve", s.Kind)
	}
	if !s.Enabled {
		return nil, fmt.Errorf("providers: %s is disabled", s.Kind)
	}

	fp := s.fingerprint()
	r.mu.Lock()
	defer r.mu.Unlock()

	if b, ok := r.built[s.Kind]; ok && b.fingerprint == fp {
		return b.instance, b.err
	}

	instance, err := d.Build(ctx, s, r.log)
	if err != nil {
		r.log.ErrorContext(ctx, "a model provider could not be built",
			"provider", s.Kind, "region", s.Region, "error", err)
	} else {
		instance.Settings = s
		r.log.InfoContext(ctx, "model provider ready", "provider", s.Kind, "region", s.Region)
	}
	r.built[s.Kind] = built{fingerprint: fp, instance: instance, err: err}
	return instance, err
}

// Sync builds every enabled provider in settings and forgets the rest.
//
// It returns a state for every kind named, enabled or not, so a reader can say
// "disabled" and "failed to build" as two different things.
func (r *Registry) Sync(ctx context.Context, settings []Settings) map[Kind]State {
	out := make(map[Kind]State, len(settings))
	seen := make(map[Kind]bool, len(settings))
	for _, s := range settings {
		seen[s.Kind] = true
		state := State{Settings: s}
		if s.Enabled {
			state.Instance, state.Err = r.Build(ctx, s)
		}
		out[s.Kind] = state
	}

	if r != nil {
		r.mu.Lock()
		for k, b := range r.built {
			enabled := false
			for _, s := range settings {
				if s.Kind == k && s.Enabled {
					enabled = true
				}
			}
			if !seen[k] || !enabled {
				delete(r.built, k)
				if b.instance != nil {
					r.log.InfoContext(ctx, "model provider stopped", "provider", k)
				}
			}
		}
		r.mu.Unlock()
	}
	return out
}

// Instances lists every provider built and serving, in kind order.
func (r *Registry) Instances() []*Instance {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*Instance, 0, len(r.built))
	for _, k := range kinds {
		if b, ok := r.built[k]; ok && b.instance != nil && b.err == nil {
			out = append(out, b.instance)
		}
	}
	return out
}

// Instance returns the built instance for one kind, if it is serving.
func (r *Registry) Instance(k Kind) (*Instance, bool) {
	for _, inst := range r.Instances() {
		if inst.Settings.Kind == k {
			return inst, true
		}
	}
	return nil, false
}

// PriceSources lists the price source of every serving provider that has one.
func (r *Registry) PriceSources() []pricing.Source {
	var out []pricing.Source
	for _, inst := range r.Instances() {
		if inst.Prices != nil {
			out = append(out, inst.Prices)
		}
	}
	return out
}

// SortSettings orders settings by kind, which is the order every list of
// providers is shown in.
func SortSettings(list []Settings) {
	slices.SortFunc(list, func(a, b Settings) int {
		return slices.Index(kinds, a.Kind) - slices.Index(kinds, b.Kind)
	})
}
