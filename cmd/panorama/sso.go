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
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/ssosetup"
	"github.com/dnxlabs/panorama/internal/store"
)

// errFindings reports that a command found something wrong. It carries no
// message because the findings were already printed in full; what it does is
// give the shell a non-zero exit status so `panorama sso doctor` is usable in a
// deployment check.
var errFindings = errors.New("")

// ssoCmd validates a single sign-on connection and renders the configuration
// that would establish it.
//
// It never writes configuration. In compose, Helm, and ECS the environment
// comes from a manifest that lives outside the container, so a command that
// "saved" the connection would either be lost on the next deploy or leave the
// running deployment disagreeing with the manifest that produced it. What it
// does instead is prove the connection works and print exactly what to paste,
// which is the part that is actually hard.
func ssoCmd(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("sso: want a subcommand (presets, probe, doctor or claims)")
	}

	switch args[0] {
	case "presets":
		return ssoPresets(cfg)
	case "probe":
		return ssoProbe(ctx, cfg, args[1:])
	case "doctor":
		return ssoDoctor(ctx, cfg, log, args[1:])
	case "claims":
		return ssoClaims(ctx, cfg, log)
	default:
		return fmt.Errorf("sso: unknown subcommand %q (want presets, probe, doctor or claims)", args[0])
	}
}

// redirectURI is the callback URL a provider must have registered for this
// deployment, derived from the same function the server uses.
func redirectURI(cfg config.Config) string {
	return ssosetup.RedirectURI(cfg.Server.BaseURL)
}

// ssoPresets lists what each provider needs.
func ssoPresets(cfg config.Config) error {
	fmt.Printf(`Whichever provider you use, this is the redirect URI to register at it:

  %s

It comes from server.base_url, so if that is not the URL a browser reaches this
console at, register the right one and correct base_url as well.

`, redirectURI(cfg))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tPROVIDER\tSCOPES\tGROUPS CLAIM\n")
	for _, p := range ssosetup.Presets() {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, p.Label, strings.Join(p.Scopes, ","), p.GroupsClaim)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// The one absence worth explaining, because a deployment proxying Bedrock is
	// an AWS deployment and this is the first place people look.
	fmt.Print(`
A preset supplies scopes and the claim name, and carries the provider-specific
things that have to be switched on before groups arrive at all. Pick one with:

  panorama sso probe -preset <id> -issuer <your issuer>

Assume group membership is off until a sign-in proves otherwise: almost no
provider emits it by default, and several emit identifiers rather than names.

AWS IAM Identity Center is not listed because it cannot be used: it is not an
OIDC provider for third-party applications, and it cannot put group membership
in an assertion at all. Point Panorama at the identity source behind it.
`)
	return nil
}

// ssoProbe validates an issuer that is not configured yet.
//
// Needs no database and no running deployment - only a route to the identity
// provider - so it works before anything is set up, which is when it is useful.
func ssoProbe(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("sso probe", flag.ContinueOnError)
	issuer := fs.String("issuer", "", "OIDC issuer URL to validate (required)")
	clientID := fs.String("client-id", "", "client id registered at the provider")
	presetID := fs.String("preset", ssosetup.PresetGeneric,
		"provider preset ("+strings.Join(ssosetup.PresetIDs(), ", ")+")")
	format := fs.String("format", ssosetup.FormatEnv,
		"configuration to render ("+strings.Join(ssosetup.Formats(), ", ")+")")
	scopes := fs.String("scopes", "", "comma-separated scopes, overriding the preset's")
	groupsClaim := fs.String("groups-claim", "", "token claim carrying group membership, overriding the preset's")
	write := fs.String("write", "", "write the rendered configuration to this file (yaml format only; never writes the secret)")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	preset, ok := ssosetup.PresetByID(*presetID)
	if !ok {
		return fmt.Errorf("sso probe: unknown preset %q (want %s)", *presetID, strings.Join(ssosetup.PresetIDs(), ", "))
	}

	if *issuer == "" {
		return errors.New("sso probe: -issuer is required")
	}

	probe := ssosetup.Probe{
		Issuer:      *issuer,
		ClientID:    *clientID,
		Preset:      preset,
		GroupsClaim: *groupsClaim,
		RedirectURI: redirectURI(cfg),
	}
	if *scopes != "" {
		probe.Scopes = splitList(*scopes)
	}

	fmt.Printf("Probing %s as %s…\n\n", *issuer, preset.Label)
	result := ssosetup.Run(ctx, probe)
	printFindings(result.Findings)

	if len(result.Notes) > 0 {
		fmt.Printf("\nBefore this will work, at %s:\n", preset.Label)
		for _, n := range result.Notes {
			fmt.Printf("  - %s\n", n)
		}
		if result.DocsURL != "" {
			fmt.Printf("\n  %s\n", result.DocsURL)
		}
	}

	if !result.OK {
		fmt.Println("\nNo configuration rendered: the issuer has to resolve first.")
		return errFindings
	}

	// The redirect URI comes from this deployment's base_url, which is the
	// value a browser has to reach the console at. Rendered from whatever
	// configuration this command was run with, and called out, because running
	// the probe on a laptop against a deployment that lives elsewhere is a
	// normal thing to do and would otherwise silently print a localhost URI.
	conn := ssosetup.Connection{
		Issuer:      result.Issuer,
		ClientID:    *clientID,
		Scopes:      result.Scopes,
		GroupsClaim: result.GroupsClaim,
		RedirectURI: redirectURI(cfg),
	}

	body, err := ssosetup.Render(*format, conn)
	if err != nil {
		return err
	}

	// Stated here rather than left to the artifact. Most formats embed it in a
	// comment, but the ECS one is the secret's value and holds JSON only - and
	// this is the value that has to match what is registered at the provider
	// character for character, so it is printed whatever was rendered.
	fmt.Printf("\nRegister this redirect URI at your provider:\n  %s\n", conn.RedirectURI)

	fmt.Printf("\n%s\n", strings.Repeat("─", 72))
	fmt.Print(body)
	fmt.Printf("%s\n", strings.Repeat("─", 72))

	fmt.Printf(`
Panorama stores none of this. Put it where your deployment's environment comes
from, restart, then confirm with:

  panorama sso doctor
`)

	if *write != "" {
		if *format != ssosetup.FormatYAML {
			return fmt.Errorf("sso probe: -write renders a configuration file, so it needs -format %s", ssosetup.FormatYAML)
		}
		// 0o600: a configuration file naming an issuer and a client id is not a
		// secret, but it is deployment configuration and the directory it lands
		// in is the operator's.
		if werr := os.WriteFile(*write, []byte(body), 0o600); werr != nil {
			return fmt.Errorf("sso probe: writing %s: %w", *write, werr)
		}
		fmt.Printf("\nWrote %s. The client secret is not in it - set PANORAMA_OIDC_CLIENT_SECRET.\n", *write)
	}

	return nil
}

// ssoDoctor checks the running deployment's connection against what it has seen.
func ssoDoctor(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("sso doctor", flag.ContinueOnError)
	if err := parseFlags(fs, args); err != nil {
		return err
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

	facts, err := ssoFacts(ctx, db)
	if err != nil {
		return err
	}

	fmt.Printf("Checking single sign-on for %s…\n\n", cfg.Server.BaseURL)
	findings := ssosetup.Diagnose(ctx, ssosetup.Deployment{
		Issuer:      cfg.OIDC.Issuer,
		ClientID:    cfg.OIDC.ClientID,
		HasSecret:   cfg.OIDC.ClientSecret != "",
		Scopes:      cfg.OIDC.Scopes,
		GroupsClaim: cfg.OIDC.GroupsClaim,
		BaseURL:     cfg.Server.BaseURL,
		RedirectURI: redirectURI(cfg),
	}, facts)

	printFindings(findings)

	switch ssosetup.Worst(findings) {
	case ssosetup.LevelFail:
		return errFindings
	case ssosetup.LevelWarn:
		fmt.Println("\nNothing is broken, but the warnings above are the things that")
		fmt.Println("look like success and are not.")
	}
	return nil
}

// ssoClaims shows the group strings sign-ins have actually carried.
//
// The reason this exists: the role mapping asks for group names, and until a
// sign-in happens nothing knows what a given provider calls them. Guessing is
// not recoverable by trying again - a wrong name grants nothing and fails
// silently.
func ssoClaims(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			log.Error("closing the database", "error", cerr)
		}
	}()

	observed, err := db.ObservedGroups(ctx)
	if err != nil {
		return err
	}
	claims, err := db.ObservedClaims(ctx)
	if err != nil {
		return err
	}
	counters, err := db.SignInCounters(ctx)
	if err != nil {
		return err
	}
	mapping, err := db.GroupRoles(ctx)
	if err != nil {
		return err
	}

	total := counters[store.CounterSignIns]
	if total == 0 {
		fmt.Printf(`No sign-in has completed through single sign-on yet, so nothing here knows
what your provider calls its groups.

Sign in once at %s, then run this again.
`, cfg.Server.BaseURL)
		return nil
	}

	role := map[string]string{}
	for _, m := range mapping {
		role[strings.ToLower(strings.TrimSpace(m.Group))] = m.Role
	}

	fmt.Printf("%d sign-in(s) through %q.\n", total, cfg.OIDC.GroupsClaim)
	if n := counters[store.CounterSignInsWithoutGroups]; n > 0 {
		fmt.Printf("%d carried no groups at all.\n", n)
	}
	fmt.Println()

	if len(observed) == 0 {
		fmt.Printf("No sign-in has ever carried a group, so every one of them took the\ndefault role.\n")
		printClaimShape(cfg.OIDC.GroupsClaim, claims, total)
		return errFindings
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "GROUP\tSEEN\tLAST\tMAPPED TO\n")
	for _, g := range observed {
		mapped := role[strings.ToLower(strings.TrimSpace(g.Group))]
		if mapped == "" {
			mapped = "-"
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", g.Group, g.Count, g.LastSeen.Format("2006-01-02"), mapped)
	}
	if ferr := w.Flush(); ferr != nil {
		return ferr
	}

	// A mapping naming a group nobody has been seen in is the silent failure
	// worth printing unprompted: it grants nothing and looks configured.
	seen := map[string]bool{}
	for _, g := range observed {
		seen[strings.ToLower(strings.TrimSpace(g.Group))] = true
	}
	var unseen []string
	for _, m := range mapping {
		if !seen[strings.ToLower(strings.TrimSpace(m.Group))] {
			unseen = append(unseen, m.Group)
		}
	}
	if len(unseen) > 0 {
		sort.Strings(unseen)
		fmt.Printf("\nMapped, but never seen: %s\n", strings.Join(unseen, ", "))
		fmt.Println("These grant nothing. Usually a typo, or a name where the provider emits an id.")
	}

	return nil
}

// printClaimShape turns "no groups arrived" into a definite diagnosis.
//
// Without it the answer is a disjunction - the claim is called something else,
// or the provider is not emitting it - and the two have opposite fixes, so an
// administrator ends up alternating between them a sign-in at a time. The names
// of the claims that did arrive settle it, and they are the provider's own
// vocabulary rather than anybody's guess.
func printClaimShape(configured string, claims []store.ObservedClaim, signIns int64) {
	if len(claims) == 0 {
		fmt.Printf(`
Nothing is recorded about the shape of your tokens, which means every sign-in
here predates Panorama recording it. Sign in once more and run this again - the
next token's claim names will be listed, and they answer this directly.
`)
		return
	}

	names := make([]string, 0, len(claims))
	var (
		present bool
		empty   int64
		seen    int64
	)
	for _, c := range claims {
		names = append(names, c.Claim)
		if c.Claim == configured {
			present, empty, seen = true, c.Empty, c.Count
		}
	}

	// The counts here can be lower than the sign-in total, because sign-ins
	// that predate this being recorded are counted in one and absent from the
	// other. Said plainly rather than left as a discrepancy to puzzle over.
	var widest int64
	for _, c := range claims {
		if c.Count > widest {
			widest = c.Count
		}
	}
	if widest < signIns {
		fmt.Printf("\nOf %d sign-ins, the claim names of %d were recorded - the rest happened\nbefore Panorama recorded them.\n", signIns, widest)
	}

	fmt.Printf("\nYour provider did send these claims. This is its vocabulary, not a guess:\n\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "CLAIM\tSIGN-INS\tEMPTY\n")
	for _, c := range claims {
		fmt.Fprintf(w, "%s\t%d\t%d\n", c.Claim, c.Count, c.Empty)
	}
	if ferr := w.Flush(); ferr != nil {
		return
	}

	switch {
	case ssosetup.GroupsOverage(names):
		fmt.Printf(`
%q was withheld, not misconfigured. The %q claim in that list is how Microsoft
Entra ID says it dropped group membership because the user is in more than it
will put in a token - around 200 - and pointed at Microsoft Graph instead.

Panorama does not call Graph: doing so would mean holding a directory-read
credential to answer a question the token was supposed to answer. Narrow what
is emitted instead. In the app registration, under Token configuration, edit
the groups claim and choose "Groups assigned to the application", then assign
the groups that matter to the enterprise application.
`, configured, "_claim_names")

	case present && empty == seen:
		fmt.Printf(`
%q arrives on every sign-in and is empty every time. The claim name is right,
so this is not a naming problem: the provider has been asked for groups and has
none to send for the accounts signing in.

Usually one of three things. The account is genuinely in no group of the type
you selected - Entra's "Security groups" excludes Microsoft 365 groups. Or the
claim is configured to emit only groups assigned to the application, and none
have been assigned. Or the groups exist in a directory the provider federates
to rather than in its own.
`, configured)

	case present:
		fmt.Printf(`
%q arrives on some sign-ins and not others, and %d of %d carried nothing. That
is not a misconfiguration - a claim that is right for most people and empty for
one is telling you about that one account's group membership.
`, configured, empty, seen)

	default:
		fmt.Printf(`
%q has never arrived at all. Nothing is emitting it, so renaming it in
Panorama's configuration will not help unless one of the claims above is what
your provider calls groups.

Requesting a scope is not enough on its own: nearly every provider requires the
claim to be switched on per application.
`, configured)
	}

	if candidates := ssosetup.GroupClaimCandidates(configured, names); len(candidates) > 0 {
		fmt.Printf("\nClaims above that could be group membership: %s\n", strings.Join(candidates, ", "))
		fmt.Printf("Point oidc.groups_claim at one of them if it is.\n")
	}

	fmt.Print(`
For Microsoft Entra ID, the claim is called "groups" and it is configured under
app registration → Token configuration → Add groups claim. If you have done
that and it still never arrives, check these in order - the first one accounts
for most of it, and none of them are visible from here:

  1. Whether the account signing in is in any group of the type you selected.
     Entra omits the claim entirely when it has nothing to put in it: there is
     no empty array to see, and a directory with groups in it looks identical
     to one without if the person signing in is in none of them. Check the
     user's own Groups blade, not the app registration. A guest account is the
     usual case - invited, never added to anything.

  2. Which group types are ticked at the top of the "Edit groups claim" dialog,
     above the per-token-type settings. "Security groups" excludes a Microsoft
     365 group that is not security-enabled, which is what you get by creating
     a group in the Microsoft 365 admin center. "All groups" includes it.

  3. Whether groups are assigned to the application, if you chose "Groups
     assigned to the application". That option emits nothing until somebody
     assigns groups under Enterprise applications → your app → Users and
     groups - the app registration alone is not enough.

  4. Whether the ID box is ticked for the token type. Panorama reads the ID
     token, so a groups claim configured only for access tokens is invisible
     here.

  5. Whether "Emit groups as role claims" is on. That moves group membership
     into "roles" rather than "groups", and it is the one case where changing
     oidc.groups_claim is the fix.

Expect object ids rather than names, and map those. Names require the "Groups
assigned to the application" option with cloud_displayname.
`)
}

// ssoFacts reads what the database knows about sign-ins that have happened.
func ssoFacts(ctx context.Context, db *store.DB) (ssosetup.Facts, error) {
	observed, err := db.ObservedGroups(ctx)
	if err != nil {
		return ssosetup.Facts{}, err
	}
	counters, err := db.SignInCounters(ctx)
	if err != nil {
		return ssosetup.Facts{}, err
	}
	mapping, err := db.GroupRoles(ctx)
	if err != nil {
		return ssosetup.Facts{}, err
	}

	defaultRole := store.RoleDeveloper
	if v, serr := db.Setting(ctx, "oidc.default_role"); serr == nil {
		defaultRole = v
	}

	facts := ssosetup.Facts{
		SSOSignIns:           int(counters[store.CounterSignIns]),
		SignInsWithoutGroups: int(counters[store.CounterSignInsWithoutGroups]),
		DefaultRole:          defaultRole,
	}
	for _, g := range observed {
		facts.ObservedGroups = append(facts.ObservedGroups, g.Group)
	}
	for _, m := range mapping {
		facts.MappedGroups = append(facts.MappedGroups, m.Group)
	}
	return facts, nil
}

// printFindings renders a check result.
//
// The marker is a word rather than a colour: this output gets pasted into
// issues and piped into files, and an escape sequence survives neither.
func printFindings(findings []ssosetup.Finding) {
	for _, f := range findings {
		fmt.Printf("%s  %s\n", marker(f.Level), f.Title)
		if f.Detail != "" {
			fmt.Printf("      %s\n", f.Detail)
		}
		if f.Remedy != "" {
			fmt.Printf("      → %s\n", f.Remedy)
		}
		fmt.Println()
	}
}

func marker(level string) string {
	switch level {
	case ssosetup.LevelFail:
		return "FAIL"
	case ssosetup.LevelWarn:
		return "WARN"
	default:
		return "  ok"
	}
}

// splitList parses a comma-separated flag, dropping empties so a trailing comma
// does not become a scope named "".
func splitList(in string) []string {
	var out []string
	for _, part := range strings.Split(in, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
