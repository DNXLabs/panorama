// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricefinder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dnxlabs/panorama/internal/providers"
)

// Reader is the part of the finder that reads prose: which card documents a
// model, and what a card's Pricing section says. Everything a Reader returns
// is checked before it is used - Match against the card's own text, Extract by
// Verify - so a Reader is trusted to find things, never to be right about them.
type Reader interface {
	// Name identifies what reads, for the table's provenance.
	Name() string
	// Match picks, for each model id, the link to the card that documents it,
	// or leaves the id out when no card does.
	Match(ctx context.Context, models []string, links []Link) (map[string]string, error)
	// Extract reports the prices one card's Pricing section states.
	Extract(ctx context.Context, model string, card Card) (Extraction, error)
}

// Invoker is what the Claude reader needs from a provider: a non-streaming
// call to the Anthropic Messages API. The Bedrock provider is one, so the
// finder signs, sends and classifies its requests with the same code the
// gateway serves traffic with, and imports no AWS client of its own.
type Invoker interface {
	Invoke(ctx context.Context, req *providers.Request) (*providers.Response, error)
}

// Claude reads with a Claude model over the Messages API.
type Claude struct {
	Invoker Invoker
	// Model is the provider-native id to invoke, inference-profile prefix
	// included.
	Model string
}

// Name reports the model that reads.
func (c Claude) Name() string { return c.Model }

const matchPrompt = `You are matching Amazon Bedrock model identifiers to the pages that document them.

Below is a list of model identifiers, then a list of model card pages (URL and link text) from the Amazon Bedrock user guide.

For each identifier, choose the one model card that documents that exact model. A dated identifier such as "openai.gpt-5.5-2026-04-23" is documented by the card for the model it is a snapshot of ("GPT-5.5"). If no card documents the model, leave the identifier out. Do not guess between two plausible cards; leave it out instead. Every answer is checked against the card's text afterwards.

Record your answer with the record_matches tool.`

const extractPrompt = `You are reading the Pricing section of an Amazon Bedrock model card, to record the standard on-demand price of model %q.

The section is below, as text: "### " starts a heading and "#### " a subheading under it, and a table row is one line with cells separated by " | ".

Record every price row that applies to commercial AWS Regions, using the record_prices tool:
- One entry per table row. Copy "row" exactly as the line appears, character for character, and "section" exactly as the nearest heading above it appears, without the "### " or "#### ".
- "option" is in_region, geo (Geo CRIS, a geographic cross-Region profile) or global (Global CRIS).
- "above_input_tokens" is 0 for the price that applies at every prompt size, or the threshold in tokens for a long-context price (for example 272000 for "more than 272K input tokens").
- Rates are US dollars per million tokens, written as they appear in the row without the dollar sign ("0.275", not "$0.275" or ".275"). "cache_write" is the prompt-cache write price; when the card lists a separate one-hour cache write, that one goes in "cache_write_1h". Leave a class empty when the row shows no price for it, which the page marks with a dash.
- Skip rows for GovCloud, China, batch, priority, flex, reserved or provisioned throughput.

If the section states no price at all (for example it only points to another page), record no rows and say why in "no_price".`

var matchTool = map[string]any{
	"name":        "record_matches",
	"description": "Record which model card documents each model identifier.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"matches": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"model": map[string]any{"type": "string"},
						"url":   map[string]any{"type": "string"},
					},
					"required": []string{"model", "url"},
				},
			},
		},
		"required": []string{"matches"},
	},
}

var extractTool = map[string]any{
	"name":        "record_prices",
	"description": "Record the price rows a model card's Pricing section states.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rows": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"section":            map[string]any{"type": "string"},
						"row":                map[string]any{"type": "string"},
						"option":             map[string]any{"type": "string", "enum": []string{OptionInRegion, OptionGeo, OptionGlobal}},
						"above_input_tokens": map[string]any{"type": "integer"},
						"input":              map[string]any{"type": "string"},
						"output":             map[string]any{"type": "string"},
						"cache_read":         map[string]any{"type": "string"},
						"cache_write":        map[string]any{"type": "string"},
						"cache_write_1h":     map[string]any{"type": "string"},
					},
					"required": []string{"section", "row", "option", "above_input_tokens", "input", "output"},
				},
			},
			"no_price": map[string]any{"type": "string"},
		},
		"required": []string{"rows"},
	},
}

// Match asks which card documents each model.
func (c Claude) Match(ctx context.Context, models []string, links []Link) (map[string]string, error) {
	var b strings.Builder
	b.WriteString(matchPrompt)
	b.WriteString("\n\nModel identifiers:\n")
	for _, m := range models {
		b.WriteString("- " + m + "\n")
	}
	b.WriteString("\nModel cards:\n")
	for _, l := range links {
		b.WriteString("- " + l.URL + " (" + l.Text + ")\n")
	}

	var out struct {
		Matches []struct {
			Model string `json:"model"`
			URL   string `json:"url"`
		} `json:"matches"`
	}
	if err := c.call(ctx, b.String(), matchTool, &out); err != nil {
		return nil, err
	}
	matches := make(map[string]string, len(out.Matches))
	for _, m := range out.Matches {
		if m.Model != "" && m.URL != "" {
			matches[m.Model] = m.URL
		}
	}
	return matches, nil
}

// Extract asks what one card's Pricing section says.
func (c Claude) Extract(ctx context.Context, model string, card Card) (Extraction, error) {
	prompt := fmt.Sprintf(extractPrompt, model) + "\n\n" + card.Pricing
	var out Extraction
	if err := c.call(ctx, prompt, extractTool, &out); err != nil {
		return Extraction{}, err
	}
	return out, nil
}

// call sends one prompt with one tool the model must use, and decodes what it
// put in the tool call.
func (c Claude) call(ctx context.Context, prompt string, tool map[string]any, into any) error {
	name, _ := tool["name"].(string)
	body, err := json.Marshal(map[string]any{
		"max_tokens":  8192,
		"messages":    []map[string]any{{"role": "user", "content": prompt}},
		"tools":       []map[string]any{tool},
		"tool_choice": map[string]any{"type": "tool", "name": name},
	})
	if err != nil {
		return err
	}

	resp, err := c.Invoker.Invoke(ctx, &providers.Request{
		API: providers.APIAnthropicMessages, Model: c.Model, Body: body,
	})
	if err != nil {
		return fmt.Errorf("pricefinder: invoke %s: %w", c.Model, err)
	}

	var msg struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Body, &msg); err != nil {
		return fmt.Errorf("pricefinder: decode the reply: %w", err)
	}
	for _, block := range msg.Content {
		if block.Type == "tool_use" && block.Name == name {
			if err := json.Unmarshal(block.Input, into); err != nil {
				return fmt.Errorf("pricefinder: decode %s: %w", name, err)
			}
			return nil
		}
	}
	if msg.StopReason == "max_tokens" {
		return errors.New("pricefinder: the reply was cut off before the tool call finished")
	}
	return fmt.Errorf("pricefinder: the reply did not use %s", name)
}
