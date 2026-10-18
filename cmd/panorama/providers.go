// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/controlplane"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/providers/bedrock"
	"github.com/dnxlabs/panorama/internal/store"
)

// providersCmd configures the model providers from a terminal.
//
// The same functions the console calls - controlplane.SaveProvider and
// CheckProvider - rather than a second implementation of what a change means.
// A running gateway serves a change within policy.GenerationPollInterval.
func providersCmd(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("providers: want a subcommand (list, set, check or policy)")
	}

	// The policy needs no database, which is the point: it goes into a role
	// before there is a deployment to run anything against.
	if args[0] == "policy" {
		fmt.Println(bedrock.TaskRolePolicyJSON())
		return nil
	}

	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			log.Error("closing the database", "error", cerr)
		}
	}()

	registry := providers.NewRegistry(log, bedrock.Descriptors()...)
	if serr := seedProviders(ctx, cfg, db, log); serr != nil {
		return serr
	}
	deps := controlplane.ProviderDeps{DB: db, Registry: registry, Log: log}

	switch args[0] {
	case "list":
		return listProviders(ctx, db, registry)

	case "set":
		fs := flag.NewFlagSet("providers set", flag.ContinueOnError)
		region := fs.String("region", "", "the region this provider invokes, e.g. us-east-1")
		enabled := fs.String("enabled", "", "true or false; unset leaves it as it is (a new provider is enabled)")
		betas := fs.String("unsupported-betas", "", "comma-separated beta flags to drop; \"-\" forwards every flag")
		windows := fs.String("context-windows", "", "model=tokens pairs, comma-separated")
		kind, perr := providerKindArg(fs, args[1:])
		if perr != nil {
			return perr
		}

		change := controlplane.ProviderChange{}
		if *region != "" {
			change.Region = region
		}
		if *enabled != "" {
			on, perr := strconv.ParseBool(*enabled)
			if perr != nil {
				return fmt.Errorf("providers set: -enabled %q is not true or false", *enabled)
			}
			change.Enabled = &on
		}
		if *betas != "" || *windows != "" {
			existing, _, lerr := db.Provider(ctx, kind)
			if lerr != nil {
				return lerr
			}
			options := existing.Options
			if *betas == "-" {
				options.UnsupportedBetas = &[]string{}
			} else if *betas != "" {
				list := splitList(*betas)
				options.UnsupportedBetas = &list
			}
			if *windows != "" {
				parsed, perr := parseWindows(*windows)
				if perr != nil {
					return fmt.Errorf("providers set: %w", perr)
				}
				options.ContextWindows = parsed
			}
			change.Options = &options
		}

		saved, serr := controlplane.SaveProvider(ctx, deps, kind, change, "cli")
		if serr != nil {
			return fmt.Errorf("providers set: %w", serr)
		}
		state := "enabled"
		if !saved.Settings.Enabled {
			state = "disabled"
		}
		fmt.Printf("%s is %s in %s.\n", kind, state, saved.Settings.Region)
		if saved.RegionChanged {
			fmt.Println("The region changed, so the last check no longer applies; run `panorama providers check " +
				string(kind) + "`.")
		}
		switch {
		case saved.DiscoveryError != "":
			fmt.Printf("\nDiscovery failed: %s\n", saved.DiscoveryError)
		case saved.Settings.Enabled:
			fmt.Printf("Discovered %d targets.\n", saved.Discovered)
		}
		if len(saved.Stranded) > 0 {
			fmt.Printf("\nThese routes point at a model this region does not list: %s\n",
				strings.Join(saved.Stranded, ", "))
		}
		fmt.Printf("\nA running gateway serves this within %s.\n", policy.GenerationPollInterval)
		return nil

	case "check":
		fs := flag.NewFlagSet("providers check", flag.ContinueOnError)
		model := fs.String("model", "", "the model id to invoke; empty tries the routes' models, then the cheapest one this gateway can serve")
		kind, perr := providerKindArg(fs, args[1:])
		if perr != nil {
			return perr
		}

		svc := pricingService(cfg, db, registry, log)
		if lerr := svc.Load(ctx); lerr != nil {
			return lerr
		}
		result, cerr := controlplane.CheckProvider(ctx, deps, kind, *model, svc.Table())
		if cerr != nil {
			return fmt.Errorf("providers check: %w", cerr)
		}
		for _, at := range result.Refused {
			printRefusal(kind, at.Target, at.Route, result.Region, at.Error, at.Hint, at.Access)
		}
		if !result.OK {
			printRefusal(kind, result.Target, result.Route, result.Region, result.Error, result.Hint, result.Access)
			return errFindings
		}
		fmt.Printf("%s invoked %s%s in %s in %s, streamed. One token of output.\n",
			kind, result.Target, routeNote(result.Route), result.Region, result.Duration.Round(time.Millisecond))
		if len(result.Refused) > 0 {
			fmt.Println("\nThe deployment can invoke Bedrock; the models above were refused for reasons of their own.")
		}
		return nil

	default:
		return fmt.Errorf("providers: unknown subcommand %q (want list, set, check or policy)", args[0])
	}
}

// printRefusal reports one model a check could not invoke: the provider's own
// words, what to do about them, and - where the provider can say - what it
// reports about the model's availability, which is evidence and not a verdict.
func printRefusal(kind providers.Kind, target, route, region, message, hint string, access *providers.ModelAccess) {
	fmt.Printf("%s could not invoke %s%s in %s.\n\n  %s\n\n%s\n", kind, dash(target), routeNote(route), region,
		message, hint)
	if access != nil {
		fmt.Printf("\nBedrock reports for %s: agreement %s, authorization %s, entitlement %s, region %s.\n",
			access.Model, dash(access.Agreement), dash(access.Authorization), dash(access.Entitlement),
			dash(access.Region))
		if access.AgreementError != "" {
			fmt.Printf("  The agreement says: %s\n", access.AgreementError)
		}
	}
	fmt.Println()
}

// routeNote names the route a checked model was chosen from.
func routeNote(route string) string {
	if route == "" {
		return ""
	}
	return " (the route " + route + ")"
}

func listProviders(ctx context.Context, db *store.DB, registry *providers.Registry) error {
	configured, err := db.ListProviders(ctx)
	if err != nil {
		return err
	}
	byKind := map[providers.Kind]providers.Settings{}
	for _, s := range configured {
		byKind[s.Kind] = s
	}
	catalogue, err := db.ListCatalogue(ctx)
	if err != nil {
		return err
	}
	counts := map[providers.Kind]int{}
	for _, m := range catalogue {
		counts[m.Provider]++
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PROVIDER\tSTATE\tREGION\tVERIFIED\tMODELS\n")
	for _, d := range registry.Descriptors() {
		s, ok := byKind[d.Kind]
		switch {
		case !ok:
			fmt.Fprintf(w, "%s\tnot configured\t-\t-\t-\n", d.Kind)
		default:
			state := "enabled"
			if !s.Enabled {
				state = "disabled"
			}
			verified := "never checked"
			if s.VerifiedAt != nil {
				verified = s.VerifiedAt.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", d.Kind, state, s.Region, verified, counts[d.Kind])
		}
	}
	return w.Flush()
}

// providerKindArg parses a providers subcommand's flags and returns the one
// argument it takes, the provider kind, from wherever it was written: `providers
// set bedrock -region us-east-1` the way a person says it, and `providers check
// -model X bedrock` the way somebody adding a flag to a command they already
// typed ends up writing it.
func providerKindArg(fs *flag.FlagSet, args []string) (providers.Kind, error) {
	positional, err := parseArgs(fs, args)
	if err != nil {
		return "", err
	}
	switch len(positional) {
	case 0:
		return "", fmt.Errorf("%s: name a provider (%s)", fs.Name(), kindList())
	case 1:
		return providers.Kind(positional[0]), nil
	default:
		return "", fmt.Errorf("%s: name one provider (%s); it was given %q",
			fs.Name(), kindList(), strings.Join(positional, " "))
	}
}

func kindList() string {
	names := make([]string, 0, len(providers.Kinds()))
	for _, k := range providers.Kinds() {
		names = append(names, string(k))
	}
	return strings.Join(names, " or ")
}

func parseWindows(v string) (map[string]int, error) {
	out := map[string]int{}
	for _, pair := range splitList(v) {
		model, tokens, found := strings.Cut(pair, "=")
		if !found || strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("-context-windows: %q is not model=tokens", pair)
		}
		n, err := strconv.Atoi(strings.TrimSpace(tokens))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("-context-windows: %q is not a token count", tokens)
		}
		out[strings.TrimSpace(model)] = n
	}
	return out, nil
}
