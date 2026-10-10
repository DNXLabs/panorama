// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package pricefinder

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/dnxlabs/panorama/internal/pricing"
)

// Options a card prices separately, as the reader reports them.
const (
	OptionInRegion = "in_region"
	OptionGeo      = "geo"
	OptionGlobal   = "global"
)

// Extraction is what a reader reports about one card's prices.
type Extraction struct {
	// Rows are the price rows read, one per table row.
	Rows []PriceRow `json:"rows"`
	// NoPrice says why the card states no price, when it states none.
	NoPrice string `json:"no_price,omitempty"`
}

// PriceRow is one table row of a card's Pricing section, as the reader read it.
type PriceRow struct {
	// Section is the heading the row sits under, exactly as the page has it.
	Section string `json:"section"`
	// Row is the row exactly as the Pricing text shows it, pipes included.
	Row string `json:"row"`
	// Option is which inference option the row prices.
	Option string `json:"option"`
	// AboveInputTokens is the prompt size the row applies above: zero for the
	// price at every size, the threshold for a long-context price.
	AboveInputTokens int64 `json:"above_input_tokens"`
	// The rates, in US dollars per million tokens, without the dollar sign,
	// exactly as the row states them. Empty for a class the row gives no
	// price for.
	Input        string `json:"input"`
	Output       string `json:"output"`
	CacheRead    string `json:"cache_read"`
	CacheWrite   string `json:"cache_write"`
	CacheWrite1h string `json:"cache_write_1h"`
}

// Priced is one inference option's price as a card states it, checked.
type Priced struct {
	Option string
	Card   pricing.RateCard
	// Evidence is the rows the price was read from, as the page has them.
	Evidence []string
}

// Verify checks everything a reader reported against the card it read, and
// returns each option's price. A row that does not check out fails the whole
// card: a reader that misread one row is not trusted with the others.
//
// What is checked is what a language model could get wrong while sounding
// right:
//
//   - the section heading exists, and is not another set of regions' prices
//     (GovCloud and China are priced apart, and the table has no region);
//   - the row is in that section, verbatim;
//   - every figure reported is a price cell of that row, and every price cell
//     of the row is reported once - nothing invented, nothing dropped, nothing
//     counted twice;
//   - each figure is in the column whose header names its class, so a cache
//     read reported as a cache write is caught even though both are in the
//     row;
//   - the section states its prices per million tokens, which is the unit
//     every rate in the table is in;
//   - the row's first cell names the option reported;
//   - a long-context row's threshold is the one its heading states;
//   - each option has one base price, and a long-context price above it that
//     is not cheaper.
func Verify(card Card, ex Extraction) ([]Priced, error) {
	if card.Pricing == "" {
		return nil, errors.New("the page has no Pricing section")
	}
	if len(ex.Rows) == 0 {
		if ex.NoPrice != "" {
			return nil, fmt.Errorf("the card states no price: %s", ex.NoPrice)
		}
		return nil, errors.New("the reader reported no prices and no reason")
	}
	if !perMillion(card.Pricing) {
		return nil, errors.New("the Pricing section does not say its prices are per million tokens")
	}

	sections := splitSections(card.Pricing)
	byOption := map[string][]PriceRow{}
	for i, r := range ex.Rows {
		if err := verifyRow(sections, r); err != nil {
			return nil, fmt.Errorf("row %d (%q): %w", i+1, r.Row, err)
		}
		byOption[r.Option] = append(byOption[r.Option], r)
	}

	var out []Priced
	for option, rows := range byOption {
		priced, err := assemble(option, rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", option, err)
		}
		out = append(out, priced)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Option < out[j].Option })
	return out, nil
}

// section is one heading of the Pricing text and the lines under it.
type section struct {
	heading string
	// within is the heading a subsection sits under: GovCloud's short-context
	// table is headed "Short context (272K input tokens or fewer)", and only
	// its parent says whose regions those are.
	within string
	lines  []string
}

func splitSections(pricingText string) []section {
	var (
		out     []section
		current section
		parent  string
	)
	for _, line := range strings.Split(pricingText, "\n") {
		if h, ok := strings.CutPrefix(line, "#### "); ok {
			out = append(out, current)
			current = section{heading: h, within: parent}
			continue
		}
		if h, ok := strings.CutPrefix(line, "### "); ok {
			out = append(out, current)
			parent = h
			current = section{heading: h}
			continue
		}
		current.lines = append(current.lines, line)
	}
	return append(out, current)
}

// otherRegions reports a heading that prices a separate set of regions, which
// the price table cannot represent: it has no region, and its rates are the
// commercial ones.
func otherRegions(heading string) bool {
	h := strings.ToLower(heading)
	return strings.Contains(h, "govcloud") || strings.Contains(h, "china")
}

func verifyRow(sections []section, r PriceRow) error {
	switch r.Option {
	case OptionInRegion, OptionGeo, OptionGlobal:
	default:
		return fmt.Errorf("unknown option %q", r.Option)
	}
	// Every section with this heading, since GovCloud's subsections reuse
	// the commercial ones' words: the row has to be in one of them, and that
	// one must not be another set of regions'.
	var (
		inSection, elsewhere bool
		header               string
	)
	for _, sec := range sections {
		if sec.heading == "" || sec.heading != r.Section || !containsLine(sec.lines, r.Row) {
			continue
		}
		if otherRegions(sec.heading) || otherRegions(sec.within) {
			elsewhere = true
			continue
		}
		inSection = true
		header = headerOf(sec.lines, r.Row)
	}
	if !inSection {
		if elsewhere {
			return fmt.Errorf("section %q prices another set of regions", r.Section)
		}
		return fmt.Errorf("the page has no row %q under a heading %q", r.Row, r.Section)
	}

	cells := splitRow(r.Row)
	if len(cells) < 2 {
		return errors.New("the row has no price cells")
	}
	if got := optionOf(cells[0]); got != r.Option {
		return fmt.Errorf("the row is for %q, reported as %s", cells[0], r.Option)
	}

	var prices []string
	for _, c := range cells[1:] {
		if strings.HasPrefix(c, "$") {
			prices = append(prices, strings.TrimPrefix(c, "$"))
		}
	}
	reported := []string{}
	for _, v := range []string{r.Input, r.Output, r.CacheRead, r.CacheWrite, r.CacheWrite1h} {
		if v != "" {
			reported = append(reported, v)
		}
	}
	if len(reported) != len(prices) {
		return fmt.Errorf("the row has %d prices and %d were reported", len(prices), len(reported))
	}
	remaining := append([]string(nil), prices...)
	for _, v := range reported {
		i := indexOf(remaining, v)
		if i < 0 {
			return fmt.Errorf("$%s is not a price in the row", v)
		}
		remaining = append(remaining[:i], remaining[i+1:]...)
	}
	if r.Input == "" || r.Output == "" {
		return errors.New("an input and an output price are both required")
	}
	if err := byColumn(header, cells, r); err != nil {
		return err
	}

	if r.AboveInputTokens < 0 {
		return errors.New("a negative threshold")
	}
	if r.AboveInputTokens > 0 && !statesThreshold(r.Section, r.AboveInputTokens) {
		return fmt.Errorf("the heading %q does not state a threshold of %d tokens", r.Section, r.AboveInputTokens)
	}
	return nil
}

// assemble turns one option's rows into its base price and tiers.
func assemble(option string, rows []PriceRow) (Priced, error) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].AboveInputTokens < rows[j].AboveInputTokens })
	if rows[0].AboveInputTokens != 0 {
		return Priced{}, errors.New("a long-context price with no base price")
	}
	priced := Priced{Option: option}
	for i, r := range rows {
		if i > 0 && r.AboveInputTokens == rows[i-1].AboveInputTokens {
			return Priced{}, fmt.Errorf("two prices for prompts above %d tokens", r.AboveInputTokens)
		}
		rates, err := pricing.DecimalRates{
			Input: r.Input, Output: r.Output, CacheRead: r.CacheRead,
			CacheWrite: r.CacheWrite, CacheWrite1h: r.CacheWrite1h,
		}.Parse()
		if err != nil {
			return Priced{}, err
		}
		if i == 0 {
			priced.Card.Rates = rates
		} else {
			if rates.Input < priced.Card.Rates.Input || rates.Output < priced.Card.Rates.Output {
				return Priced{}, fmt.Errorf("the price above %d tokens is cheaper than the base price; "+
					"not believed without a person reading it", r.AboveInputTokens)
			}
			priced.Card.Tiers = append(priced.Card.Tiers, pricing.Tier{AboveInputTokens: r.AboveInputTokens, Rates: rates})
		}
		priced.Evidence = append(priced.Evidence, r.Section+": "+r.Row)
	}
	if err := pricing.ValidateCard(priced.Card); err != nil {
		return Priced{}, err
	}
	return priced, nil
}

// splitRow reads a rendered row back into its cells.
func splitRow(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, " | ")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// optionOf names the inference option a row's first cell describes.
func optionOf(cell string) string {
	c := strings.ToLower(cell)
	switch {
	case strings.Contains(c, "in-region") || strings.Contains(c, "in region"):
		return OptionInRegion
	case strings.Contains(c, "global"):
		return OptionGlobal
	case strings.Contains(c, "geo"):
		return OptionGeo
	}
	return ""
}

// statesThreshold reports whether a heading states a token count, in any of
// the spellings the cards use: 272K, 272k, 272,000, 272000, 1M.
func statesThreshold(heading string, tokens int64) bool {
	forms := []string{strconv.FormatInt(tokens, 10), withCommas(tokens)}
	if tokens%1000 == 0 {
		forms = append(forms, strconv.FormatInt(tokens/1000, 10)+"K", strconv.FormatInt(tokens/1000, 10)+"k")
	}
	if tokens%1_000_000 == 0 {
		forms = append(forms, strconv.FormatInt(tokens/1_000_000, 10)+"M")
	}
	for _, f := range forms {
		if containsNumber(heading, f) {
			return true
		}
	}
	return false
}

// containsNumber finds a number as a whole, so 272K does not match 1272K.
func containsNumber(s, number string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], number)
		if j < 0 {
			return false
		}
		at := i + j
		before := at == 0 || !isDigit(s[at-1])
		end := at + len(number)
		after := end == len(s) || !isDigit(s[end])
		if before && after {
			return true
		}
		i = at + 1
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func withCommas(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// headerOf finds the header row of the table a row belongs to: the nearest
// table line above it that states no price.
func headerOf(lines []string, row string) string {
	header := ""
	for _, l := range lines {
		if l == row {
			return header
		}
		switch {
		case !strings.HasPrefix(l, "|"):
			header = ""
		case !strings.Contains(l, "$") && header == "":
			header = l
		}
	}
	return ""
}

// byColumn checks each reported figure is in the column its class heads.
func byColumn(header string, cells []string, r PriceRow) error {
	if header == "" {
		return errors.New("the row's table has no header to say which price is which")
	}
	names := splitRow(header)
	if len(names) != len(cells) {
		return fmt.Errorf("the row has %d cells and its header %d", len(cells), len(names))
	}
	want := map[string]string{
		"input": r.Input, "output": r.Output, "cache_read": r.CacheRead,
		"cache_write": r.CacheWrite, "cache_write_1h": r.CacheWrite1h,
	}
	seen := map[string]bool{}
	for i := 1; i < len(cells); i++ {
		class := classOf(names[i])
		if class == "" {
			return fmt.Errorf("the column %q is not a token class this table prices", names[i])
		}
		if seen[class] {
			return fmt.Errorf("two columns are %s", class)
		}
		seen[class] = true
		cell := strings.TrimPrefix(cells[i], "$")
		if !strings.HasPrefix(cells[i], "$") {
			cell = ""
		}
		if want[class] != cell {
			return fmt.Errorf("%s is %q under %q, reported as %q", class, cells[i], names[i], want[class])
		}
	}
	for class, v := range want {
		if v != "" && !seen[class] {
			return fmt.Errorf("%s was reported and the table has no column for it", class)
		}
	}
	return nil
}

// classOf names the token class a column header prices, by the words in it:
// a cache read, a cache write (the thirty-minute one on a GPT card), a
// one-hour write where a card lists one apart, output, and input.
func classOf(header string) string {
	h := strings.ToLower(header)
	switch {
	case strings.Contains(h, "cache read") || strings.Contains(h, "cached input") || strings.Contains(h, "cache hit"):
		return "cache_read"
	case strings.Contains(h, "cache write") &&
		(strings.Contains(h, "1h") || strings.Contains(h, "1-hour") || strings.Contains(h, "1 hour") || strings.Contains(h, "60m")):
		return "cache_write_1h"
	case strings.Contains(h, "cache write"):
		return "cache_write"
	case strings.Contains(h, "output"):
		return "output"
	case strings.Contains(h, "input"):
		return "input"
	}
	return ""
}

// perMillion reports whether a Pricing section states its unit as per million
// tokens.
func perMillion(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "per 1 million tokens") || strings.Contains(t, "per million tokens") ||
		strings.Contains(t, "per 1m tokens")
}

func containsLine(lines []string, line string) bool {
	for _, l := range lines {
		if l == line {
			return true
		}
	}
	return false
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}
