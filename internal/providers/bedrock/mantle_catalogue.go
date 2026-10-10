// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/dnxlabs/panorama/internal/providers"
)

// MantleCatalogue reports what bedrock-mantle can invoke in one region.
//
// One call, GET /v1/models, and it says less than the runtime catalogue does:
// an id, an owner, a status with a reason, and the data-retention modes a model
// allows. No display name, no context window, no streaming flag, and nothing
// about which API a model speaks - so the rest of each row comes from this
// package's tables (apis.go, windows.go), and a model those tables have not
// heard of is listed with what the provider said and nothing invented beside it.
//
// There are no inference profiles here and so no geography: a model is invoked
// in the region the call is made to, which is recorded on every row as the
// residency answer.
type MantleCatalogue struct {
	provider *Mantle
	windows  map[string]int
}

// NewMantleCatalogue builds a catalogue source over a provider.
func NewMantleCatalogue(provider *Mantle, windows map[string]int) *MantleCatalogue {
	return &MantleCatalogue{provider: provider, windows: windows}
}

// Region reports which region is discovered.
func (c *MantleCatalogue) Region() string { return c.provider.region }

// mantleModel is one row of /v1/models as bedrock-mantle returns it.
type mantleModel struct {
	ID            string `json:"id"`
	OwnedBy       string `json:"owned_by"`
	Status        string `json:"status"`
	StatusReason  string `json:"status_reason"`
	DataRetention *struct {
		AllowedModes []string `json:"allowed_modes"`
	} `json:"data_retention"`
}

// Catalogue lists every model this region serves.
func (c *MantleCatalogue) Catalogue(ctx context.Context) ([]providers.CatalogueEntry, error) {
	models, err := c.list(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]providers.CatalogueEntry, 0, len(models))
	var windowless, speechless []string
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		limits, _ := limitsFor(c.windows, m.ID)
		entry := providers.CatalogueEntry{
			ID:                m.ID,
			FoundationModelID: m.ID,
			Provider:          providers.KindBedrockMantle,
			Region:            c.provider.region,
			Vendor:            vendorLabel(m.OwnedBy, m.ID),
			DisplayName:       displayName(m.ID),
			APIs:              c.provider.apis(m.ID),
			// Every request shape this endpoint serves streams, and the
			// endpoint publishes no per-model flag that could say otherwise.
			Streaming:       true,
			Regions:         []string{c.provider.region},
			Status:          availability(m.Status),
			StatusReason:    m.StatusReason,
			ContextWindow:   limits.Context,
			MaxOutputTokens: limits.Output,
		}
		if m.DataRetention != nil {
			entry.Retention = slices.Clone(m.DataRetention.AllowedModes)
		}
		if entry.ContextWindow == 0 {
			windowless = append(windowless, m.ID)
		}
		if len(entry.APIs) == 0 {
			speechless = append(speechless, m.ID)
		}
		out = append(out, entry)
	}

	// Said once per pass, at info, for the same reason the runtime catalogue
	// says it: a new model this build has no row for is the expected state, and
	// the line is what makes the day a table goes stale visible in the log.
	log := c.provider.log
	if len(speechless) > 0 {
		slices.Sort(speechless)
		log.InfoContext(ctx, "bedrock-mantle lists models this build does not know an API for; they are browse-only",
			"region", c.provider.region, "count", len(speechless), "models", strings.Join(speechless, ", "),
			"remedy", "set the model's APIs in Settings, Model providers, once you have checked what it speaks")
	}
	if len(windowless) > 0 {
		slices.Sort(windowless)
		log.InfoContext(ctx, "no context window recorded for these bedrock-mantle models",
			"region", c.provider.region, "count", len(windowless), "models", strings.Join(windowless, ", "))
	}

	slices.SortFunc(out, func(a, b providers.CatalogueEntry) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (c *MantleCatalogue) list(ctx context.Context) ([]mantleModel, error) {
	m := c.provider
	req, err := m.newSignedRequest(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if serr := m.sign(ctx, req, nil); serr != nil {
		return nil, serr
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bedrock-mantle: list models in %s: %w", m.region, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("bedrock-mantle: read the model list in %s: %w", m.region, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bedrock-mantle: list models in %s: %w", m.region, classifyMantle(resp.StatusCode, body))
	}

	var page struct {
		Data []mantleModel `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("bedrock-mantle: the model list in %s is not the expected shape: %w", m.region, err)
	}
	return page.Data, nil
}

// availability reads a model's status into what a row records: empty for a
// model that can be invoked, and the provider's own word otherwise.
//
// Read as an allowlist of the words that mean available. A status this build
// has not seen is kept and shown, because "listed but not invocable" found in
// the console beats the same thing found by a route failing.
func availability(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "available", "active", "enabled", "ready":
		return ""
	default:
		return status
	}
}

// vendorLabel names who made a model, from the owner the endpoint reports or
// the id's own vendor segment.
func vendorLabel(ownedBy, id string) string {
	vendor := strings.ToLower(strings.TrimSpace(ownedBy))
	if vendor == "" || vendor == "system" || vendor == "aws" || vendor == "amazon-bedrock" {
		vendor = vendorOf(id)
	}
	if label, ok := vendorLabels[vendor]; ok {
		return label
	}
	return vendor
}

// vendorLabels spells a vendor the way the runtime catalogue does, so one
// vendor filter covers both providers.
var vendorLabels = map[string]string{
	"anthropic":  "Anthropic",
	"openai":     "OpenAI",
	"qwen":       "Qwen",
	"moonshotai": "Moonshot AI",
	"deepseek":   "DeepSeek",
	"zai":        "Z.AI",
	"mistral":    "Mistral AI",
	"minimax":    "MiniMax",
	"nvidia":     "NVIDIA",
	"google":     "Google",
	"xai":        "xAI",
	"writer":     "Writer",
}

// displayName is the id without its vendor segment. The endpoint publishes no
// name, and a name invented from an id would be a spelling nobody at the vendor
// uses; the id is at least the string an operator can search for.
func displayName(id string) string {
	if _, rest, ok := strings.Cut(id, "."); ok && rest != "" {
		return rest
	}
	return id
}
