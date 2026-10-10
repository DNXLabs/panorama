// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Command pricefinder adds a price for every model a provider can serve that
// the binary does not already price, to the found table a release embeds.
//
// It is run by the Prices workflow (.github/workflows/prices.yml) after every
// merge, against the table that workflow keeps in S3, and it is not part of
// the product: nothing in the image runs it. See internal/pricefinder for what
// it trusts and what it checks.
//
//	pricefinder -regions us-east-1,us-west-2 -previous old.json -out new.json -report report.md
//
// Exit status: 0 when the run looked at everything, 3 when it wrote its
// output but could not look at something - a catalogue, a price list, a page -
// and 1 when it wrote nothing.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dnxlabs/panorama/internal/pricefinder"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/providers/bedrock"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		regions      = flag.String("regions", "us-east-1", "comma-separated regions whose catalogues and price lists are read")
		readerRegion = flag.String("reader-region", "us-east-1", "region the reading model is invoked in")
		readerModel  = flag.String("reader-model", "global.anthropic.claude-haiku-4-5-20251001-v1:0", "the Claude model that reads model cards, as a Bedrock model or profile id")
		previous     = flag.String("previous", "", "the table to add to; empty starts a new one")
		out          = flag.String("out", "found_prices.json", "where to write the table")
		report       = flag.String("report", "", "where to write the markdown report; empty writes none")
		summary      = flag.String("summary", "", "where to write the run's counts as JSON; empty writes none")
		indexURL     = flag.String("index", pricefinder.DefaultIndexURL, "the page that links every model card")
		check        = flag.String("check", "", "only check that this file is a table the binary can load, and exit")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *check != "" {
		return checkTable(log, *check)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	prev, err := readPrevious(*previous)
	if err != nil {
		log.Error("reading the previous table", "path", *previous, "error", err)
		return 1
	}
	seed, err := pricing.Seed()
	if err != nil {
		log.Error("reading the seed", "error", err)
		return 1
	}

	in := pricefinder.Inputs{Seed: seed, Previous: prev}
	for _, region := range split(*regions) {
		in.Catalogues, in.Published = read(ctx, log, region, in.Catalogues, in.Published)
	}

	provider, err := bedrock.New(ctx, bedrock.Config{Region: *readerRegion, Logger: log})
	if err != nil {
		log.Error("building the reader", "region", *readerRegion, "error", err)
		return 1
	}
	finder := &pricefinder.Finder{
		Reader:   pricefinder.Claude{Invoker: provider, Model: *readerModel},
		Fetch:    fetch,
		IndexURL: *indexURL,
		Log:      log,
	}
	res, err := finder.Run(ctx, in)
	if err != nil {
		log.Error("finding prices", "error", err)
		return 1
	}

	encoded, err := json.MarshalIndent(res.Table, "", "  ")
	if err != nil {
		log.Error("encoding the table", "error", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(*out, encoded, 0o644); err != nil { //nolint:gosec // a public price table
		log.Error("writing the table", "path", *out, "error", err)
		return 1
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])[:12]

	if *report != "" {
		f, err := os.Create(*report)
		if err != nil {
			log.Error("writing the report", "path", *report, "error", err)
			return 1
		}
		werr := pricefinder.WriteReport(f, res, digest)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			log.Error("writing the report", "path", *report, "error", werr)
			return 1
		}
	}
	if *summary != "" {
		counts, err := json.Marshal(pricefinder.Summarise(res, digest))
		if err == nil {
			err = os.WriteFile(*summary, append(counts, '\n'), 0o644) //nolint:gosec // counts
		}
		if err != nil {
			log.Error("writing the summary", "path", *summary, "error", err)
			return 1
		}
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		if err := pricefinder.WriteAnnotations(os.Stdout, res); err != nil {
			log.Error("writing annotations", "error", err)
		}
	}

	log.Info("price finder done",
		"rows", len(res.Table.Entries), "added", len(res.Added),
		"unpriced", len(res.Missing), "problems", len(res.Problems), "table", digest)
	if len(res.Problems) > 0 {
		return 3
	}
	return 0
}

// read collects one region's catalogues and price lists, both providers.
func read(ctx context.Context, log *slog.Logger, region string, cats []pricefinder.Catalogue, pubs []pricefinder.Published) ([]pricefinder.Catalogue, []pricefinder.Published) {
	conf := bedrock.Config{Region: region, Logger: log}
	runtimeName := string(providers.KindBedrock)
	mantleName := string(providers.KindBedrockMantle)

	catalogue, err := bedrock.NewCatalogue(ctx, conf)
	if err == nil {
		var entries []providers.CatalogueEntry
		entries, err = catalogue.Catalogue(ctx)
		cats = append(cats, pricefinder.Catalogue{Provider: runtimeName, Region: region, Entries: entries, Err: err})
	} else {
		cats = append(cats, pricefinder.Catalogue{Provider: runtimeName, Region: region, Err: err})
	}
	prices, err := bedrock.NewPriceSource(ctx, conf)
	if err == nil {
		var entries []pricing.Entry
		entries, err = prices.Fetch(ctx)
		pubs = append(pubs, pricefinder.Published{Provider: runtimeName, Region: region, Entries: entries, Err: err})
	} else {
		pubs = append(pubs, pricefinder.Published{Provider: runtimeName, Region: region, Err: err})
	}

	mantle, err := bedrock.NewMantle(ctx, conf)
	if err != nil {
		cats = append(cats, pricefinder.Catalogue{Provider: mantleName, Region: region, Err: err})
		pubs = append(pubs, pricefinder.Published{Provider: mantleName, Region: region, Err: err})
		return cats, pubs
	}
	mantleCatalogue := bedrock.NewMantleCatalogue(mantle, nil)
	entries, err := mantleCatalogue.Catalogue(ctx)
	cats = append(cats, pricefinder.Catalogue{Provider: mantleName, Region: region, Entries: entries, Err: err})
	mantlePrices, err := bedrock.NewMantlePriceSource(ctx, conf, mantleCatalogue)
	if err == nil {
		var priced []pricing.Entry
		priced, err = mantlePrices.Fetch(ctx)
		pubs = append(pubs, pricefinder.Published{Provider: mantleName, Region: region, Entries: priced, Err: err})
	} else {
		pubs = append(pubs, pricefinder.Published{Provider: mantleName, Region: region, Err: err})
	}
	return cats, pubs
}

// checkTable is what a source build runs on a table it downloaded before
// embedding it: the same parse and the same per-row validation the binary
// applies at startup, so a table that would not load fails the build that
// fetched it rather than the deployment that runs it.
func checkTable(log *slog.Logger, path string) int {
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the caller's
	if err != nil {
		log.Error("reading the table", "path", path, "error", err)
		return 1
	}
	table, err := pricing.ParseFoundTable(raw)
	if err != nil {
		log.Error("parsing the table", "path", path, "error", err)
		return 1
	}
	entries, err := table.PriceEntries()
	if err != nil {
		log.Error("the table does not load", "path", path, "error", err)
		return 1
	}
	sum := sha256.Sum256(raw)
	log.Info("found price table checks out", "path", path, "rows", len(entries),
		"digest", hex.EncodeToString(sum[:])[:12])
	return 0
}

func readPrevious(path string) (pricing.FoundTable, error) {
	if path == "" {
		return pricing.FoundTable{
			Note: "Rates the price finder added for models seed.json does not price, kept by the Prices " +
				"workflow and embedded by every release. Only ever added to; see cmd/pricefinder.",
		}, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path is the caller's
	if err != nil {
		return pricing.FoundTable{}, err
	}
	return pricing.ParseFoundTable(raw)
}

var client = &http.Client{Timeout: time.Minute}

// fetch reads a public documentation page.
func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "panorama-pricefinder (+https://github.com/dnxlabs/panorama)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, errors.New(url + ": empty page")
	}
	return body, nil
}

func split(list string) []string {
	var out []string
	for _, r := range strings.Split(list, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
