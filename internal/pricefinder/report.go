// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricefinder

import (
	"fmt"
	"io"
	"strings"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// WriteReport writes a run's outcome as markdown: what was added and from
// where, what is still unpriced and why, and what the run could not look at.
// It is the Prices workflow's job summary and the body of the issue that
// lists unpriced models.
func WriteReport(w io.Writer, res Result, digest string) error {
	var b strings.Builder

	fmt.Fprintf(&b, "## Prices\n\n")
	fmt.Fprintf(&b, "The found table has %d rows (`%s`); this run added %d.\n\n", len(res.Table.Entries), digest, len(res.Added))

	if len(res.Added) > 0 {
		b.WriteString("### Added\n\n")
		b.WriteString("| Model | Provider | Scope | Input | Output | Cache read | Cache write | Above a prompt size | Read from |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
		for _, a := range res.Added {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s | %s | %s |\n",
				a.Model, a.Provider, a.Scope, money(a.Input), money(a.Output), money(a.CacheRead),
				money(a.CacheWrite), tiers(a.Tiers), source(a))
		}
		b.WriteString("\nRates are US dollars per million tokens. A model card is read by a language model and kept only when every figure was found in a row it quoted, under a heading it named, on the page.\n\n")
	}

	switch {
	case len(res.Missing) > 0:
		fmt.Fprintf(&b, "### Still unpriced (%d)\n\n", len(res.Missing))
		b.WriteString("A request for one of these is recorded unpriced until a rate is set under `pricing.models` or a later run finds one.\n\n")
		b.WriteString("| Model | Provider | Scope | Regions | Why |\n| --- | --- | --- | --- | --- |\n")
		for _, m := range res.Missing {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
				m.Model, m.Key.Provider, m.Key.Scope, strings.Join(m.Regions, ", "), escape(m.Reason))
		}
		b.WriteString("\n")
	case len(res.Problems) == 0:
		b.WriteString("Every model a provider can serve in the regions read has a price.\n\n")
	default:
		b.WriteString("Every model this run could read has a price, but it could not look at everything: see below.\n\n")
	}

	if len(res.Problems) > 0 {
		fmt.Fprintf(&b, "### Could not look (%d)\n\n", len(res.Problems))
		for _, p := range res.Problems {
			b.WriteString("- " + escape(p) + "\n")
		}
		b.WriteString("\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteAnnotations writes GitHub Actions workflow commands, so each unpriced
// model and each problem is a warning on the run rather than a line in a log.
func WriteAnnotations(w io.Writer, res Result) error {
	var b strings.Builder
	for _, m := range res.Missing {
		fmt.Fprintf(&b, "::warning title=Unpriced model::%s (%s, %s): %s\n",
			m.Model, m.Key.Provider, m.Key.Scope, command(m.Reason))
	}
	for _, p := range res.Problems {
		fmt.Fprintf(&b, "::warning title=Price finder could not look::%s\n", command(p))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func money(v string) string {
	if v == "" {
		return "none"
	}
	return "$" + v
}

func tiers(ts []pricing.DecimalTier) string {
	if len(ts) == 0 {
		return ""
	}
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = fmt.Sprintf("%d: %s in, %s out", t.AboveInputTokens, money(t.Input), money(t.Output))
	}
	return strings.Join(parts, "; ")
}

func source(e pricing.FoundEntry) string {
	if e.Source == pricing.SourceModelCard {
		return fmt.Sprintf("[model card](%s)", e.SourceURL)
	}
	return "Price List API, " + e.Region
}

// escape keeps a free-text reason from breaking a markdown table.
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}

// command escapes text for a workflow command, which ends at a newline.
func command(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	return strings.ReplaceAll(s, "\n", "%0A")
}

// Summary is the run in numbers, for the workflow to act on without parsing
// the report.
type Summary struct {
	Rows     int    `json:"rows"`
	Added    int    `json:"added"`
	Unpriced int    `json:"unpriced"`
	Problems int    `json:"problems"`
	Table    string `json:"table"`
}

// Summarise counts a run.
func Summarise(res Result, digest string) Summary {
	return Summary{
		Rows: len(res.Table.Entries), Added: len(res.Added),
		Unpriced: len(res.Missing), Problems: len(res.Problems), Table: digest,
	}
}
