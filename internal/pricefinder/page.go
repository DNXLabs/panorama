// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package pricefinder finds a price for every model a provider can serve that
// nothing compiled into the binary prices, and adds it to the found table a
// release embeds.
//
// Two rungs, in order of how much they can be trusted:
//
//  1. A snapshot of the AWS Price List API. Deterministic: the same rates a
//     connected deployment's refresh would fetch, taken so an air-gapped one
//     has them too.
//  2. The model's card in the Amazon Bedrock user guide, for a model the Price
//     List API does not publish at all - OpenAI's GPT-5.x is the case that
//     made this necessary. A card is a web page, so a language model reads it,
//     and nothing it says is kept until code has found every figure it
//     reported in a row it quoted, under a heading it named, on the page.
//
// The table only grows. A model already priced by the seed or by an earlier
// run is never read again, so a price that shipped in a release is the price
// somebody could have reviewed.
package pricefinder

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Link is one model card the index page links to.
type Link struct {
	URL  string `json:"url"`
	Text string `json:"text"`
}

// cardPath matches a model card's file name.
var cardPath = regexp.MustCompile(`(^|/)model-card-[a-z0-9-]+\.html$`)

// CardLinks reads the model-card links out of an index page, resolved against
// the page's own address and listed once each.
func CardLinks(page []byte, base string) ([]Link, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("pricefinder: parse index: %w", err)
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("pricefinder: index address %q: %w", base, err)
	}

	var (
		out  []Link
		seen = map[string]bool{}
	)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := attr(n, "href")
			if u, perr := url.Parse(href); perr == nil && cardPath.MatchString(u.Path) {
				abs := baseURL.ResolveReference(u)
				abs.Fragment = ""
				if !seen[abs.String()] {
					seen[abs.String()] = true
					out = append(out, Link{URL: abs.String(), Text: collapse(textOf(n))})
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out, nil
}

// Card is one model card, read.
type Card struct {
	URL string
	// Text is the whole page as plain text, which is what a model id is
	// looked for in.
	Text string
	// Pricing is the page's Pricing section as markdown: headings, sentences
	// and tables as pipe-separated rows. It is what the language model reads,
	// and what everything it reports is checked against. Empty when the page
	// has no Pricing section.
	Pricing string
}

// ReadCard parses a model card.
func ReadCard(page []byte, address string) (Card, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return Card{}, fmt.Errorf("pricefinder: parse %s: %w", address, err)
	}
	card := Card{URL: address, Text: collapse(textOf(root))}

	// The Pricing section is the h2 whose id ends in "-pricing", up to the
	// next h2. Found by structure rather than by the word "Pricing", which a
	// card also uses in running text.
	var body *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		if body != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "h2" && strings.HasSuffix(attr(n, "id"), "-pricing") {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(root)
	if body == nil {
		return card, nil
	}

	var b strings.Builder
	b.WriteString("## Pricing\n")
	for n := body.NextSibling; n != nil; n = n.NextSibling {
		if n.Type == html.ElementNode && n.Data == "h2" {
			break
		}
		renderBlock(&b, n)
	}
	card.Pricing = strings.TrimSpace(b.String())
	return card, nil
}

// renderBlock writes one block of the Pricing section as markdown.
func renderBlock(b *strings.Builder, n *html.Node) {
	if n.Type == html.TextNode {
		if t := collapse(n.Data); t != "" {
			b.WriteString(t + "\n")
		}
		return
	}
	if n.Type != html.ElementNode {
		return
	}
	switch n.Data {
	case "h3":
		b.WriteString("\n### " + collapse(textOf(n)) + "\n")
		return
	case "h4", "h5":
		b.WriteString("\n#### " + collapse(textOf(n)) + "\n")
		return
	case "p", "li":
		if t := collapse(textOf(n)); t != "" {
			b.WriteString(t + "\n")
		}
		return
	case "table":
		b.WriteString("\n")
		for _, row := range tableRows(n) {
			b.WriteString(Row(row) + "\n")
		}
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		renderBlock(b, c)
	}
}

// Row renders table cells as the one line the checks match against.
func Row(cells []string) string {
	return "| " + strings.Join(cells, " | ") + " |"
}

// tableRows reads a table's rows as the text of each cell.
func tableRows(table *html.Node) [][]string {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
					cells = append(cells, collapse(textOf(c)))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)
	return rows
}

func textOf(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
		return ""
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(textOf(c))
		if c.Type == html.ElementNode && blockish[c.Data] {
			b.WriteString(" ")
		}
	}
	return b.String()
}

// blockish elements separate the words either side of them in plain text.
var blockish = map[string]bool{
	"p": true, "div": true, "li": true, "td": true, "th": true, "tr": true, "br": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "table": true,
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// collapse folds runs of whitespace, non-breaking spaces included, into one
// space.
func collapse(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, " ", " ")), " ")
}
