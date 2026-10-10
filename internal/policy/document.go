// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
)

// The file form of a policy.
//
// The database is authoritative at runtime - the console edits it directly, and
// the engine reads nothing else. This document exists because the
// people who deploy a gateway keep their configuration in version control, and
// a policy that can only be built by clicking is a policy nobody can review in
// a pull request.
//
// Applying is idempotent and matches on stable identity rather than replacing
// wholesale, so a document can be re-applied on every deploy. See
// store.ApplyPolicy.
//
// References here are the ones a person writes: a user is an email address, a
// team is its slug. They are resolved to ids at apply time, so a document does
// not contain generated identifiers and stays readable. A model, by contrast,
// is written as the provider names it, behind the provider's own name -
// bedrock/us.anthropic.claude-sonnet-5 - because that string is the one the
// provider is given, and inventing a friendly alias for it here would be a
// second naming scheme to keep in step with the catalogue.

// Document is a policy as written in a file.
type Document struct {
	Budgets []BudgetDoc `yaml:"budgets" json:"budgets"`
	Routes  []RouteDoc  `yaml:"routes" json:"routes"`
}

// BudgetDoc is one budget, as written.
type BudgetDoc struct {
	Scope string `yaml:"scope" json:"scope"`
	// ID is the subject: an email address for a user budget, a team slug for a
	// team budget, and unset for the organization.
	ID     string `yaml:"id" json:"id"`
	Period string `yaml:"period" json:"period"`
	Name   string `yaml:"name" json:"name"`
	// Enabled defaults to true when omitted, so the common case is silent and
	// disabling is explicit.
	Enabled *bool `yaml:"enabled" json:"enabled"`

	SoftLimit Amount `yaml:"soft_limit" json:"soft_limit"`
	HardLimit Amount `yaml:"hard_limit" json:"hard_limit"`
}

// RouteDoc is one route, as written.
//
// The model is written on the route itself, because that is what it is: where
// this name sends traffic. A route is five lines.
type RouteDoc struct {
	// Name is what a client asks for, and the route's identity across edits.
	// Re-applying a document updates the route with the same name rather than
	// replacing it, which is what keeps requests already recorded against it
	// explicable.
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`

	// Users, Teams and Tools are the conditions. Omitted means unrestricted.
	// Users are email addresses and teams are slugs, resolved to ids at apply
	// time so a document holds no generated identifiers.
	//
	// Every condition given must hold. A route naming a user and a team admits
	// that user when they are in that team, and nobody else.
	Users []string `yaml:"users" json:"users"`
	Teams []string `yaml:"teams" json:"teams"`
	Tools []string `yaml:"tools" json:"tools"`

	// Model is the model this route sends traffic to, as provider/model - the
	// provider's kind, then the exact string that provider is given,
	// inference-profile prefix included. Required: a route with nowhere to send
	// a request would admit one and then refuse it.
	//
	// A bare model id is refused rather than read as Bedrock's: the same string
	// is valid on more than one provider, and the two differ in where a prompt
	// is processed and what it costs.
	Model string `yaml:"model" json:"model"`

	// ServesOverSoft says whether this route is still served once a budget
	// governing the caller has passed its soft limit. Defaults to true when
	// omitted, so the ordinary route is silent and stopping is explicit.
	ServesOverSoft *bool `yaml:"serves_over_soft" json:"serves_over_soft"`
}

// Amount is a money value held as the text it was written as.
//
// Reading the literal rather than a parsed number is the same decision the
// price table makes: YAML would helpfully turn 800.00 into a float64 that is
// not 800.00, and a budget that is a rounding error away from what the operator
// typed is a budget that gets argued with. JSON does exactly the same thing,
// which is why the console posts amounts as strings and this type reads them
// that way from both.
type Amount string

// UnmarshalYAML keeps a scalar's literal text.
func (a *Amount) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: an amount must be a single value like 250.00", node.Line)
	}
	*a = Amount(node.Value)
	return nil
}

// UnmarshalJSON keeps a value's literal text, whether it arrived as a string
// or as a number.
//
// A console that posts 800.00 as a JSON number has already lost nothing - but
// 0.1 and 1e21 have, and the point of this type is that no representation
// stands between what someone typed and what is enforced.
func (a *Amount) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" {
		*a = ""
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("an amount must be a value like \"250.00\": %w", err)
		}
		*a = Amount(s)
		return nil
	}
	// A bare JSON number: keep the digits as written rather than parsing them.
	*a = Amount(text)
	return nil
}

// MarshalJSON writes the literal back out, so a document that is read and
// re-sent is unchanged.
func (a Amount) MarshalJSON() ([]byte, error) { return json.Marshal(string(a)) }

// Set reports whether an amount was given.
func (a Amount) Set() bool { return strings.TrimSpace(string(a)) != "" }

// Money parses the amount into exact nanodollars.
func (a Amount) Money() (pricing.Money, error) {
	return pricing.ParseUSD(strings.TrimPrefix(strings.TrimSpace(string(a)), "$"))
}

// Resolver turns the references a person writes into the identifiers the engine
// matches on.
type Resolver interface {
	// UserIDByEmail returns the id of the user with this email.
	UserIDByEmail(ctx context.Context, email string) (string, error)
	// TeamIDBySlug returns the id of the team with this slug.
	TeamIDBySlug(ctx context.Context, slug string) (string, error)
}

// ParseDocument reads a policy document.
//
// Unknown keys are an error. A misspelled matcher would otherwise be dropped
// silently, and a rule that matches everything because its one condition was
// ignored is the worst possible outcome for a typo.
func ParseDocument(r io.Reader) (Document, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var doc Document
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty document is a legitimate way to say "no policy", and
			// applying it removes everything. That is a real operation.
			return Document{}, nil
		}
		return Document{}, fmt.Errorf("policy: parse document: %w", err)
	}
	return doc, nil
}

// Compile turns a document into the set the engine evaluates, resolving
// references and reporting every problem at once.
//
// Every problem at once, because an operator fixing a policy file should see
// the whole list rather than one error per attempt - the same stance
// configuration validation takes.
func (d Document) Compile(ctx context.Context, res Resolver) (Set, error) {
	var (
		set  Set
		errs []error
	)

	seenBudget := make(map[Key]string, len(d.Budgets))
	for i, bd := range d.Budgets {
		b, err := bd.compile(ctx, res)
		if err != nil {
			errs = append(errs, fmt.Errorf("budgets[%d]: %w", i, err))
			continue
		}
		if previous, dup := seenBudget[b.Key()]; dup {
			errs = append(errs, fmt.Errorf("budgets[%d]: a %s %s budget is already defined (%s)",
				i, b.Period, b.Scope, previous))
			continue
		}
		seenBudget[b.Key()] = bd.describe()
		set.Budgets = append(set.Budgets, b)
	}

	seenRoute := make(map[string]struct{}, len(d.Routes))
	for i, rd := range d.Routes {
		route, err := rd.compile(ctx, res)
		if err != nil {
			errs = append(errs, fmt.Errorf("routes[%d] (%s): %w", i, rd.describe(), err))
			continue
		}
		if _, dup := seenRoute[route.Name]; dup {
			errs = append(errs, fmt.Errorf("routes[%d]: two routes are named %q; a name is a route's identity", i, route.Name))
			continue
		}
		seenRoute[route.Name] = struct{}{}
		set.Routes = append(set.Routes, route)
	}

	if err := errors.Join(errs...); err != nil {
		return Set{}, err
	}

	set.Index()
	return set, nil
}

func (bd BudgetDoc) compile(ctx context.Context, res Resolver) (Budget, error) {
	b := Budget{
		Scope:   strings.TrimSpace(bd.Scope),
		Period:  Period(strings.TrimSpace(bd.Period)),
		Name:    bd.Name,
		Enabled: enabled(bd.Enabled),
	}

	var errs []error

	switch b.Scope {
	case ScopeOrg:
		if bd.ID != "" {
			errs = append(errs, errors.New("an org budget has no id: v1 is one deployment, one organization"))
		}
	case ScopeUser:
		id, err := resolveUser(ctx, res, bd.ID)
		if err != nil {
			errs = append(errs, err)
		}
		b.ScopeID = id
	case ScopeTeam:
		id, err := resolveTeam(ctx, res, bd.ID)
		if err != nil {
			errs = append(errs, err)
		}
		b.ScopeID = id
	}

	for _, field := range []struct {
		name  string
		value Amount
		dst   **pricing.Money
	}{
		{"soft_limit", bd.SoftLimit, &b.SoftLimit},
		{"hard_limit", bd.HardLimit, &b.HardLimit},
	} {
		if !field.value.Set() {
			continue
		}
		m, err := field.value.Money()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not an amount like 250.00", field.name, field.value))
			continue
		}
		*field.dst = &m
	}

	// Validate covers scope, period, and the relationships between limits.
	if err := b.Validate(); err != nil {
		errs = append(errs, err)
	}

	return b, errors.Join(errs...)
}

func (rd RouteDoc) compile(ctx context.Context, res Resolver) (Route, error) {
	route := Route{
		Name:        strings.TrimSpace(rd.Name),
		Description: strings.TrimSpace(rd.Description),
	}

	var errs []error

	for _, email := range rd.Users {
		id, err := resolveUser(ctx, res, email)
		if err != nil {
			errs = append(errs, fmt.Errorf("users: %w", err))
			continue
		}
		route.Users = append(route.Users, id)
	}
	for _, slug := range rd.Teams {
		id, err := resolveTeam(ctx, res, slug)
		if err != nil {
			errs = append(errs, fmt.Errorf("teams: %w", err))
			continue
		}
		route.Teams = append(route.Teams, id)
	}
	for _, tool := range rd.Tools {
		route.Tools = append(route.Tools, strings.TrimSpace(tool))
	}

	if model := strings.TrimSpace(rd.Model); model != "" {
		ref, err := providers.ParseModelRef(model)
		if err != nil {
			errs = append(errs, fmt.Errorf("model: %w", err))
			// Held as written so Validate does not also report the route as
			// having no model: it has one, spelled wrong, and one sentence
			// saying so is the useful error. The route is never returned.
			ref = providers.ModelRef{Provider: providers.KindBedrock, Model: model}
		}
		route.Target = ref
	}
	route.ServesOverSoft = enabled(rd.ServesOverSoft)

	if err := route.Validate(); err != nil {
		errs = append(errs, err)
	}

	return route, errors.Join(errs...)
}

func resolveUser(ctx context.Context, res Resolver, email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", errors.New("no user given (an email address)")
	}
	if res == nil {
		return "", errors.New("no resolver: cannot turn an email address into a user")
	}

	id, err := res.UserIDByEmail(ctx, email)
	if err != nil {
		// Silently creating the account would let a typo become a budget on a
		// user who does not exist, enforcing nothing and looking like it does.
		return "", fmt.Errorf("no user %s: %w", email, err)
	}
	return id, nil
}

func resolveTeam(ctx context.Context, res Resolver, slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", errors.New("no team given (a team slug)")
	}
	if res == nil {
		return "", errors.New("no resolver: cannot turn a slug into a team")
	}

	id, err := res.TeamIDBySlug(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("no team %q: %w", slug, err)
	}
	return id, nil
}

// enabled reads an optional flag that defaults to on.
func enabled(v *bool) bool { return v == nil || *v }

func (bd BudgetDoc) describe() string {
	if bd.Name != "" {
		return bd.Name
	}
	if bd.ID != "" {
		return bd.Scope + " " + bd.ID
	}
	return bd.Scope
}

func (rd RouteDoc) describe() string {
	if rd.Name != "" {
		return rd.Name
	}
	return "unnamed"
}
