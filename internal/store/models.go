// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
)

// CatalogueModel is one invocable target as the provider described it.
//
// A row per geography, not per foundation model: `au.anthropic.claude-sonnet-5`
// and `global.anthropic.claude-sonnet-5` are two entries because they are two
// decisions - different regions, and rates about 10% apart.
type CatalogueModel struct {
	// Provider is which provider the id is valid on. The same id on two
	// providers is two rows.
	Provider providers.Kind
	// ID is the string handed to the provider, profile prefix included.
	ID string
	// FoundationModelID is the bare id underneath, which carries the price.
	FoundationModelID string

	// Vendor is who made the model, as the provider names them.
	Vendor      string
	DisplayName string
	// Region is where the provider was asked, which is where a bare id is
	// invoked.
	Region string
	// APIs are the client APIs this target can be served on. Empty is
	// browse-only.
	APIs []providers.API
	// Status is the provider's word for a listed model that cannot be invoked
	// right now, empty otherwise.
	Status       string
	StatusReason string
	// Retention lists the data-retention modes the provider allows.
	Retention []string

	// Geography is the profile prefix as itself: "us", "au", "global", or ""
	// for a bare foundation-model id.
	Geography string
	// Regions are the AWS regions this profile may route to, as the provider
	// enumerated them. Empty for global, whose answer is "anywhere".
	Regions []string

	Streaming       bool
	Lifecycle       string
	RequiresProfile bool

	// ContextWindow is how many tokens this target accepts, or zero when this
	// build has no entry for the model. Zero is an absence, not a limit: see
	// the migration that added the column.
	ContextWindow int

	// MaxOutputTokens is how many tokens it will produce by default, or zero
	// when unrecorded. Same provenance and same handling as ContextWindow.
	MaxOutputTokens int

	DiscoveredAt time.Time
	UpdatedAt    time.Time
}

// Ref is the reference a route uses to name this target.
func (m CatalogueModel) Ref() providers.ModelRef {
	return providers.ModelRef{Provider: m.Provider, Model: m.ID}
}

// Legacy reports whether the provider has marked this model for retirement.
func (m CatalogueModel) Legacy() bool { return strings.EqualFold(m.Lifecycle, "LEGACY") }

// Available reports whether the provider says this model can be invoked now.
func (m CatalogueModel) Available() bool { return m.Status == "" }

// Speaks reports whether this target can be served on an API.
func (m CatalogueModel) Speaks(api providers.API) bool { return slices.Contains(m.APIs, api) }

// Global reports whether this target routes without a geographic restriction.
func (m CatalogueModel) Global() bool { return m.Geography == "global" }

// Residency describes where a request to this target may be processed, in the
// words a compliance reviewer needs rather than a prefix they would have to
// look up.
func (m CatalogueModel) Residency() string {
	switch {
	case m.Global():
		return "any supported AWS region"
	case len(m.Regions) > 0:
		return strings.Join(m.Regions, ", ")
	default:
		// A bare id is invoked where the provider was asked.
		return m.Region
	}
}

// Route is a name a client may ask for, as it is stored.
//
// The routing half of it - conditions, model, and whether it survives a soft
// limit - is policy.Route, because the engine and the console are looking at the
// same thing and a second copy of the shape would drift the first time somebody
// added a condition to one of them. What is added here is provenance: who
// allowed it, and when.
type Route struct {
	policy.Route

	// EnabledBy is who allowed it, kept for the audit trail. A setup session
	// has no user row, so this is a label rather than a reference.
	EnabledBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReplaceCatalogue swaps in a freshly discovered catalogue for one provider.
//
// A replace rather than an upsert, and scoped to a single provider, because the
// catalogue is a cache whose correctness includes what is *absent* from it: a
// model AWS withdrew has to disappear, and an upsert would leave it behind
// forever looking exactly like one that is still there. Scoping to the provider
// means discovering bedrock-runtime never disturbs bedrock-mantle's rows, and a
// region change replaces the old region's rows with the new one's in one step.
//
// It is one transaction. A half-written catalogue would show an administrator a
// list missing arbitrary models with nothing indicating it was incomplete.
func (db *DB) ReplaceCatalogue(ctx context.Context, provider providers.Kind, models []CatalogueModel) error {
	if !provider.Valid() {
		return fmt.Errorf("store: cannot replace a catalogue for %q", provider)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin catalogue replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, derr := tx.ExecContext(ctx,
		db.rebind(`DELETE FROM provider_models WHERE provider = ?`), string(provider)); derr != nil {
		return fmt.Errorf("store: clear catalogue for %s: %w", provider, derr)
	}

	stmt, err := tx.PrepareContext(ctx, db.rebind(`
INSERT INTO provider_models (
    provider, id, foundation_model_id, vendor, display_name, geography, region,
    regions, streaming, lifecycle, requires_profile, apis, status, status_reason,
    retention, context_window, max_output_tokens, discovered_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`))
	if err != nil {
		return fmt.Errorf("store: prepare catalogue insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now().UTC()
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if seen[m.ID] {
			// A provider listing one id twice would fail the primary key and
			// with it the whole refresh; the first occurrence is kept.
			continue
		}
		seen[m.ID] = true
		if _, ierr := stmt.ExecContext(ctx,
			string(provider), m.ID, m.FoundationModelID, m.Vendor, m.DisplayName, m.Geography, m.Region,
			strings.Join(m.Regions, ","), m.Streaming, m.Lifecycle, m.RequiresProfile,
			providers.JoinAPIs(m.APIs), m.Status, m.StatusReason, strings.Join(m.Retention, ","),
			m.ContextWindow, m.MaxOutputTokens, now, now,
		); ierr != nil {
			return fmt.Errorf("store: insert catalogue row %s/%s: %w", provider, m.ID, ierr)
		}
	}

	if cerr := tx.Commit(); cerr != nil {
		return fmt.Errorf("store: commit catalogue replace: %w", cerr)
	}
	return nil
}

const selectCatalogue = `
SELECT provider, id, foundation_model_id, vendor, display_name, geography, region,
       regions, streaming, lifecycle, requires_profile, apis, status, status_reason,
       retention, context_window, max_output_tokens, discovered_at, updated_at
  FROM provider_models`

// ListCatalogue returns every discovered target, most recognisable first.
//
// Ordered by provider, then vendor and display name so the geographies of one
// model sit together - which is how somebody choosing between them reads the
// list - and with the inference profiles ahead of the bare foundation-model id.
//
// That last clause is not cosmetic. Sorting on geography alone puts the empty
// string first, which leads every model with the one target almost nobody
// should pick: of the fifteen Anthropic models in us-east-1, fourteen are
// invocable only through a profile, and the one that still accepts its bare id
// is LEGACY. The normal case belongs at the top.
func (db *DB) ListCatalogue(ctx context.Context) ([]CatalogueModel, error) {
	rows, err := db.QueryContext(ctx, selectCatalogue+`
 ORDER BY provider, vendor, display_name,
          CASE WHEN geography = '' THEN 1 ELSE 0 END, geography, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list catalogue: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CatalogueModel
	for rows.Next() {
		var (
			m                 CatalogueModel
			provider, regions string
			apis, retention   string
		)
		if serr := rows.Scan(
			&provider, &m.ID, &m.FoundationModelID, &m.Vendor, &m.DisplayName, &m.Geography, &m.Region,
			&regions, &m.Streaming, &m.Lifecycle, &m.RequiresProfile, &apis, &m.Status, &m.StatusReason,
			&retention, &m.ContextWindow, &m.MaxOutputTokens, &m.DiscoveredAt, &m.UpdatedAt,
		); serr != nil {
			return nil, fmt.Errorf("store: scan catalogue row: %w", serr)
		}
		m.Provider = providers.Kind(provider)
		if regions != "" {
			m.Regions = strings.Split(regions, ",")
		}
		if retention != "" {
			m.Retention = strings.Split(retention, ",")
		}
		m.APIs = providers.ParseAPIs(apis)
		out = append(out, m)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read catalogue: %w", rerr)
	}
	return out, nil
}

// CatalogueFetchedAt reports when discovery last wrote for each provider, and
// omits a provider it never has.
//
// Served beside the catalogue rather than inferred from it, because "no models"
// and "never discovered" look identical in a list and mean opposite things.
func (db *DB) CatalogueFetchedAt(ctx context.Context) (map[providers.Kind]time.Time, error) {
	// max() over a TIMESTAMP loses the declared type on SQLite and comes back as
	// a string that will not scan into time.Time, so this reads the rows in
	// order and keeps the first per provider instead.
	rows, err := db.QueryContext(ctx,
		`SELECT provider, discovered_at FROM provider_models ORDER BY discovered_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: catalogue fetch time: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[providers.Kind]time.Time{}
	for rows.Next() {
		var (
			provider string
			at       time.Time
		)
		if serr := rows.Scan(&provider, &at); serr != nil {
			return nil, fmt.Errorf("store: scan catalogue fetch time: %w", serr)
		}
		if _, seen := out[providers.Kind(provider)]; !seen {
			out[providers.Kind(provider)] = at
		}
	}
	return out, rows.Err()
}

// ListRoutes returns every name a client may ask for, with its conditions, its
// model, and whether it survives a soft limit.
//
// This is the allowlist. A name absent from it is refused at the gateway.
func (db *DB) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := db.QueryContext(ctx, `
SELECT name, description, match_users, match_teams, match_tools,
       target_model, serves_over_soft,
       enabled_by, created_at, updated_at
  FROM model_routes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list routes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Route
	for rows.Next() {
		var (
			r                           Route
			users, teams, tools, target string
		)
		if serr := rows.Scan(&r.Name, &r.Description, &users, &teams, &tools,
			&target, &r.ServesOverSoft,
			&r.EnabledBy, &r.CreatedAt, &r.UpdatedAt); serr != nil {
			return nil, fmt.Errorf("store: scan route: %w", serr)
		}
		if target != "" {
			// Not flattened into "no model" either. A target that does not parse
			// is one the write path would have refused, so something wrote around
			// it; serving the route as though it had no model would hide that.
			ref, perr := providers.ParseModelRef(target)
			if perr != nil {
				return nil, fmt.Errorf("store: route %q has an unreadable model: %w", r.Name, perr)
			}
			r.Target = ref
		}
		for _, field := range []struct {
			name string
			text string
			dst  *[]string
		}{
			{"users", users, &r.Users},
			{"teams", teams, &r.Teams},
			{"tools", tools, &r.Tools},
		} {
			list, derr := decodeList(field.text)
			if derr != nil {
				// Not flattened into "no condition": an unreadable condition
				// that defaulted to empty would widen the route to everybody,
				// which is the one direction a failure here must never go.
				return nil, fmt.Errorf("store: route %q has an unreadable %s condition: %w",
					r.Name, field.name, derr)
			}
			*field.dst = list
		}

		out = append(out, r)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("store: read routes: %w", rerr)
	}
	return out, nil
}

// SaveRoute creates a route or changes one.
//
// One row and one statement: the model is on the route, so there is no second
// half a reload could land between, and the write is atomic because it is a
// single statement rather than because it is wrapped in anything.
//
// The name is the identity across an edit, which is what keeps every request
// already recorded against this route explicable after somebody repoints it.
func (db *DB) SaveRoute(ctx context.Context, r Route) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("store: %w: %w", ErrInvalidInput, err)
	}

	users, err := encodeList(r.Users)
	if err != nil {
		return fmt.Errorf("store: encode the user condition for %s: %w", r.Name, err)
	}
	teams, err := encodeList(r.Teams)
	if err != nil {
		return fmt.Errorf("store: encode the team condition for %s: %w", r.Name, err)
	}
	tools, err := encodeList(r.Tools)
	if err != nil {
		return fmt.Errorf("store: encode the tool condition for %s: %w", r.Name, err)
	}
	now := time.Now().UTC()
	if _, serr := db.ExecContext(ctx, db.rebind(`
INSERT INTO model_routes (name, description, match_users, match_teams, match_tools,
                          target_model, serves_over_soft,
                          enabled_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    description      = excluded.description,
    match_users      = excluded.match_users,
    match_teams      = excluded.match_teams,
    match_tools      = excluded.match_tools,
    target_model     = excluded.target_model,
    serves_over_soft = excluded.serves_over_soft,
    enabled_by       = excluded.enabled_by,
    updated_at       = excluded.updated_at`),
		r.Name, r.Description, users, teams, tools,
		r.Target.String(), r.ServesOverSoft, r.EnabledBy, now, now); serr != nil {
		return fmt.Errorf("store: save route %s: %w", r.Name, serr)
	}
	return nil
}

// DeleteRoute removes a name and reports whether it was there. The gateway
// refuses it from the next snapshot reload.
func (db *DB) DeleteRoute(ctx context.Context, name string) (bool, error) {
	res, err := db.ExecContext(ctx,
		db.rebind(`DELETE FROM model_routes WHERE name = ?`), name)
	if err != nil {
		return false, fmt.Errorf("store: delete route %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete route %s: %w", name, err)
	}
	return n > 0, nil
}

// encodeList writes a condition as JSON text.
//
// An empty list encodes as [] rather than null, so what this writes and what
// the column defaults to are the same shape.
func encodeList(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	out, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// decodeList reads one back.
func decodeList(text string) ([]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, err
	}
	return out, nil
}
