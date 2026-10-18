// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Command panorama is the single binary: gateway data plane, control plane API
// and jobs, and the embedded console.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dnxlabs/panorama/internal/attribution"
	"github.com/dnxlabs/panorama/internal/auth"
	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/controlplane"
	"github.com/dnxlabs/panorama/internal/dataplane"
	"github.com/dnxlabs/panorama/internal/integrations"
	"github.com/dnxlabs/panorama/internal/integrations/github"
	"github.com/dnxlabs/panorama/internal/integrations/mock"
	"github.com/dnxlabs/panorama/internal/jobs"
	"github.com/dnxlabs/panorama/internal/logging"
	"github.com/dnxlabs/panorama/internal/policy"
	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/providers/bedrock"
	"github.com/dnxlabs/panorama/internal/secrets"
	"github.com/dnxlabs/panorama/internal/store"
	"github.com/dnxlabs/panorama/internal/version"
	"github.com/dnxlabs/panorama/internal/websearch"
)

const usage = `panorama - an AI gateway that connects coding-AI spend to real work.

Usage:
  panorama [flags] <command>

Commands:
  serve             Run the gateway and control plane (default)
  migrate up        Apply pending database migrations
  migrate status    Show which migrations have been applied
  admin setup       Issue a one-time code to configure this deployment
  admin generate-key  Mint the key that protects stored credentials
  sso presets       Show what each identity provider needs
  sso probe         Validate an issuer and print the configuration to apply
  sso doctor        Check the configured connection against what it has seen
  sso claims        Show the groups sign-ins have actually carried
  login             Sign this machine in to a deployment, through a browser
  logout            Forget a deployment on this machine
  token             Print this machine's gateway token, refreshing it when due
  whoami            Show who this machine is signed in as
  configure <tool>  Write the client configuration for a coding tool
  token create      Mint a token for someone, on the machine holding the database
  team create       Create a team
  team list         List teams
  team add          Put a user on a team
  policy show       Show routes and budgets, with current spend
  policy check      Validate a policy file without applying it
  policy apply      Apply a policy file
  prices list       Show the model rate table
  prices refresh    Fetch current rates from the AWS Price List API
  providers list    Show each model provider, its region and its state
  providers set     Configure a model provider: providers set <kind> -region <region>
  providers check   Prove a provider can invoke a model, with one token
  providers policy  Print the IAM policy the deployment's role needs
  models discover   Ask each provider what this account can invoke
  models list       Show the discovered catalogue, with residency and price
  version           Print build information

Flags:
  -config path      Configuration file (default "config.yaml" if present)

Configuration comes from the file above and PANORAMA_-prefixed environment
variables; the environment wins. See config.example.yaml for every key.

login, logout, token, whoami and configure run on a developer's own machine and
need none of it: they keep a credential in ~/.panorama and talk to a deployment
over HTTP. Everything else runs where the database is.
`

func main() {
	if err := run(); err != nil {
		// A command that already printed its findings in full exits non-zero
		// without a second, emptier message. `sso doctor` is the case: it is
		// meant to be usable as a deployment check, which needs the status
		// code, and its output is the report rather than the error.
		if errors.Is(err, errFindings) {
			os.Exit(1)
		}
		// The logger may not exist yet - a configuration error happens before
		// it is built - so failures go to stderr directly.
		fmt.Fprintf(os.Stderr, "panorama: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("panorama", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	configPath := fs.String("config", "", "path to the configuration file")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	args := fs.Args()
	command := "serve"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}

	// version needs no configuration, so it works on a broken deployment -
	// which is exactly when someone asks what is running.
	if command == "version" {
		info := version.Get()
		fmt.Println(info)
		fmt.Println("source", info.Source)
		return nil
	}

	// The commands a developer runs on their own machine, dispatched before any
	// configuration is loaded. A laptop has no config.yaml and no database, and
	// requiring one would make signing in the hardest step of onboarding rather
	// than the first. They reach a deployment over HTTP with a credential in
	// the user's home directory and touch nothing else.
	if clientCommand(command, args) {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runClient(ctx, command, args)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log, err := logging.New(cfg.Log, os.Stdout)
	if err != nil {
		return err
	}

	// Cancelled on SIGINT or SIGTERM, which starts a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "serve":
		return serve(ctx, cfg, log)
	case "migrate":
		return migrate(ctx, cfg, log, args)
	case "admin":
		return admin(ctx, cfg, log, args)
	case "sso":
		return ssoCmd(ctx, cfg, log, args)
	case "token":
		return token(ctx, cfg, log, args)
	case "team":
		return team(ctx, cfg, log, args)
	case "policy":
		return policyCmd(ctx, cfg, log, args)
	case "prices":
		return prices(ctx, cfg, log, args)
	case "models":
		return models(ctx, cfg, log, args)
	case "providers":
		return providersCmd(ctx, cfg, log, args)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	info := version.Get()
	log.Info("starting panorama",
		"version", info.Version,
		"commit", info.Commit,
		"driver", cfg.Database.Driver,
		"sso", cfg.OIDC.Configured(),
	)

	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("closing the database", "error", err)
		}
	}()

	if cfg.Database.AutoMigrate {
		if err := db.Migrate(ctx); err != nil {
			return err
		}
		log.Info("schema is current")
	} else {
		pending, err := db.PendingMigrations(ctx)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			// Serving against a schema the code does not expect produces
			// confusing failures at request time. Refuse instead.
			return fmt.Errorf("%d migrations are pending and auto_migrate is off; run `panorama migrate up`", len(pending))
		}
	}

	// The providers this build can serve. Which of them are configured, and in
	// which region, is data - the providers table - and the registry is what
	// turns a row into a live provider when the policy snapshot loads it.
	registry := providers.NewRegistry(log, bedrock.Descriptors()...)
	if serr := seedProviders(ctx, cfg, db, log); serr != nil {
		return serr
	}

	prices := pricingService(cfg, db, registry, log)
	if lerr := prices.Load(ctx); lerr != nil {
		return lerr
	}

	// Policy is loaded before anything is served. A gateway that started
	// accepting traffic and then discovered its budgets would spend the first
	// seconds of every restart enforcing nothing, which is exactly the window a
	// runaway agent would find.
	//
	// Providers load with it, into the same snapshot, for the same reason:
	// a request admitted by a route is sent to that route's provider, and a
	// provider built on a separate schedule would spend part of every cycle
	// disagreeing with the routes about whether it exists.
	policyEngine := policy.New(policy.Options{
		Repository:     db,
		Providers:      registry,
		ReloadInterval: cfg.Policy.ReloadInterval,
		Logger:         log,
	})
	if lerr := policyEngine.Load(ctx); lerr != nil {
		return lerr
	}
	if len(registry.Instances()) == 0 {
		log.Warn("no model provider is serving; the gateway will refuse every request",
			"hint", "configure one in Settings, Model providers, or set bedrock.region before the first start")
	}

	// A deployment with no users cannot be signed in to, and SSO cannot be
	// configured without signing in. A one-time setup code breaks that circle,
	// and the same code repairs a deployment whose mapping later admits nobody.
	if berr := controlplane.PrepareSetup(ctx, db, log); berr != nil {
		return berr
	}

	secretBox, serr := secrets.NewBox(cfg.EncryptionKey)
	if serr != nil {
		return serr
	}
	// Said once at startup because the two ways of configuring a key are not
	// distinguishable afterwards, and one of them is reached by accident: a
	// minted key that lost a character is no longer 32 bytes, so it derives a
	// working key that is not the one the stored credentials were sealed with.
	// That is a line in the log here, or a mystery at the next confirmation
	// pass.
	if secretBox.Derived() {
		log.Info("encryption key derived from a passphrase",
			"note", "credentials sealed under a different key cannot be read; `panorama admin generate-key` mints one used as-is")
	}
	resolver := integrationResolver(db, secretBox)

	confirmer := jobs.NewConfirmer(jobs.ConfirmerOptions{
		DB:         db,
		Resolver:   resolver,
		Interval:   cfg.Integrations.SyncInterval,
		StaleAfter: cfg.Integrations.StaleAfter,
		Logger:     log,
	})
	// Registering is best-effort at startup. Connections are made in the
	// console, so a deployment with none is the ordinary first-run state
	// rather than a misconfiguration worth refusing to boot over.
	if rerr := confirmer.Register(ctx); rerr != nil {
		log.Warn("could not register integrations", "error", rerr)
	}

	// Discovery is what the console's model screen is built from, and it is the
	// answer to the question an operator would otherwise have to research: what
	// can this account invoke, where would the request be processed, and what
	// does that cost. It asks whichever providers are serving on each pass, so
	// a provider configured in the console is discovered without a restart.
	discoverer := jobs.NewDiscoverer(jobs.DiscovererOptions{
		DB: db, Instances: registry.Instances,
		Interval: cfg.Providers.DiscoveryInterval, Logger: log,
	})

	// A gateway named in the configuration is refused at startup when it is
	// not one, like any other malformed key: it is signed for with this
	// deployment's credentials, and nothing in the console can correct it.
	searchKind := bedrock.WebSearchDescriptor()
	searchDeployed, derr := deployedWebSearch(cfg.WebSearch.GatewayURL, searchKind)
	if derr != nil {
		return derr
	}

	// Web search is otherwise configured in the console and is off until it
	// is, so a backend that cannot be built is a warning and not a reason to
	// refuse to boot: the console that fixes it has to be reachable.
	search := webSearchService(db, log, cfg.Policy.ReloadInterval, searchDeployed)
	if werr := search.Reload(ctx); werr != nil {
		log.Warn("web search is configured but not serving", "error", werr,
			"hint", "check the gateway in Settings, Web search")
	}

	srv := controlplane.New(cfg, db, log)
	srv.MountConsole(controlplane.ConsoleOptions{
		Prices:            prices.Table(),
		Policy:            policyEngine,
		Integrations:      resolver,
		Secrets:           secretBox,
		Providers:         registry,
		PriceService:      prices,
		WebSearch:         search,
		WebSearchKind:     searchKind,
		WebSearchDeployed: searchDeployed,
	})

	// Mounted whether or not a provider is serving yet. A route whose provider
	// is missing is refused per request, naming the deployment as the fault,
	// and a provider configured in the console starts serving at the next
	// snapshot rather than at the next restart.
	if merr := mountDataPlane(cfg, db, log, srv, prices.Table(), policyEngine, search); merr != nil {
		return merr
	}

	go policyEngine.Run(ctx)

	// Picks up a change made on another replica; a save on this one reloads
	// straight away.
	go search.Run(ctx)

	// Refreshing rates is background work that must never hold up serving: a
	// deployment whose pricing API is unreachable still proxies traffic and
	// still prices it, from the rates it already has.
	go prices.Run(ctx)

	// Confirming work items is the same kind of background work, and fails the
	// same way: an unreachable GitHub costs outcome metrics, not traffic.
	go confirmer.Run(ctx)

	// Also background work that must not hold up serving: a deployment whose
	// catalogue is stale still proxies every model already permitted.
	go discoverer.Run(ctx)

	go jobs.NewSweeper(db, log).Run(ctx)

	if serr := srv.ListenAndServe(ctx); serr != nil {
		return serr
	}
	log.Info("stopped")
	return nil
}

// integrationResolver assembles the connected systems of record from what an
// administrator set up in the console.
//
// Two halves, kept apart on purpose. Loading reads the stored rows and decrypts
// each credential - which is the only place in the process a credential exists
// in the clear, and it exists for the length of one build. Building turns a
// connection into a source and is the only place that names a concrete
// implementation.
//
// An empty result is a supported deployment rather than a degraded one:
// attribution works with nothing connected, and the console says plainly which
// numbers are unavailable because of it.
func integrationResolver(db *store.DB, box *secrets.Box) *integrations.Resolver {
	load := func(ctx context.Context) ([]integrations.Connection, error) {
		stored, err := db.IntegrationConfigs(ctx)
		if err != nil {
			return nil, err
		}

		out := make([]integrations.Connection, 0, len(stored))
		for _, c := range stored {
			if !c.Enabled {
				continue
			}

			conn := integrations.Connection{
				Kind:           integrations.Kind(c.Kind),
				Provider:       c.Provider,
				AppID:          c.AppID,
				InstallationID: c.InstallationID,
				APIBaseURL:     c.APIBaseURL,
				SecretKind:     c.SecretKind,
				Fixtures:       c.Fixtures,
			}

			if c.HasSecret() {
				ciphertext, serr := db.IntegrationSecret(ctx, c.Kind)
				if serr != nil {
					return nil, serr
				}
				if conn.Secret, err = box.Open(ciphertext); err != nil {
					// Reported rather than skipped. A credential that will not
					// decrypt means the key changed, and an administrator needs
					// to be told that rather than watching confirmation quietly
					// stop.
					return nil, fmt.Errorf("integrations.%s: %w", c.Kind, err)
				}
			}
			out = append(out, conn)
		}
		return out, nil
	}

	build := func(c integrations.Connection) (integrations.Source, error) {
		switch c.Provider {
		case config.ProviderMock:
			return mock.Open(c.Kind, c.Fixtures)
		case config.ProviderGitHub:
			opts := github.Config{
				APIBaseURL:     c.APIBaseURL,
				AppID:          c.AppID,
				InstallationID: c.InstallationID,
			}
			switch c.SecretKind {
			case store.SecretKindAppKey:
				opts.PrivateKey = []byte(c.Secret)
			case store.SecretKindToken:
				opts.Token = c.Secret
			}
			return github.Open(opts)
		default:
			return nil, fmt.Errorf("%q is not a provider Panorama can build", c.Provider)
		}
	}

	return integrations.NewResolver(load, build)
}

// deployedWebSearch reads the gateway the configuration names, normalised the
// way the console normalises one it is given.
func deployedWebSearch(raw string, d websearch.Descriptor) (websearch.Deployment, error) {
	if strings.TrimSpace(raw) == "" {
		return websearch.Deployment{}, nil
	}
	gw, err := d.Endpoint(raw)
	if err != nil {
		return websearch.Deployment{}, fmt.Errorf("web_search.gateway_url: %w", err)
	}
	return websearch.Deployment{Kind: d.Kind, GatewayURL: gw.URL}, nil
}

// webSearchService builds the service that holds the web search backend,
// read from the console's configuration and the deployment's own gateway.
func webSearchService(db *store.DB, log *slog.Logger, interval time.Duration, deployed websearch.Deployment) *websearch.Service {
	load := func(ctx context.Context) (websearch.Config, bool, error) {
		s, found, err := db.EffectiveWebSearch(ctx, deployed)
		if err != nil || !found {
			return websearch.Config{}, false, err
		}
		return s.Config(), true, nil
	}
	build := func(ctx context.Context, c websearch.Config) (websearch.Backend, error) {
		switch c.Kind {
		case websearch.KindAgentCore:
			return bedrock.NewWebSearch(ctx, c.GatewayURL, log)
		default:
			return nil, fmt.Errorf("%q is not a web search backend this build can construct", c.Kind)
		}
	}
	return websearch.NewService(load, build, interval, log)
}

// pricingService builds the price table and its refresher.
//
// The sources are asked of the registry at every refresh, so a provider
// configured in the console is priced by the next refresh. With none serving,
// a deployment still prices requests from the seed compiled into the binary and
// from any rate the operator configured.
func pricingService(cfg config.Config, db *store.DB, registry *providers.Registry, log *slog.Logger) *pricing.Service {
	overrides := make(map[string]pricing.RateCard, len(cfg.Pricing.Models))
	for model, configured := range cfg.Pricing.Models {
		// Validated when the configuration loaded, so this cannot fail here.
		card, _ := configured.Card()
		overrides[model] = card
	}

	return pricing.NewService(pricing.Options{
		Repository:      db,
		Overrides:       overrides,
		Sources:         registry.PriceSources,
		RefreshInterval: cfg.Pricing.RefreshInterval,
		Logger:          log,
	})
}

// seedProviders creates the bedrock provider from the configuration file on a
// deployment's first start, and says so when a later start's file disagrees
// with what is stored.
//
// First boot only, and deliberately. The row is the administrator's once it
// exists: two owners for one setting is how every screen and every startup path
// grew a branch for which one was speaking. The file keeps working for the one
// thing it is good at - a compose file or a CloudFormation stack that arrives
// with a provider already configured.
func seedProviders(ctx context.Context, cfg config.Config, db *store.DB, log *slog.Logger) error {
	if !cfg.Bedrock.Enabled() {
		return nil
	}
	seed := cfg.Bedrock.SeedSettings()
	created, stored, err := db.SeedProvider(ctx, seed)
	if err != nil {
		return fmt.Errorf("seeding the bedrock provider from the configuration: %w", err)
	}
	if created {
		log.Info("bedrock provider configured from the configuration file on first start",
			"region", stored.Region,
			"note", "from now on it is configured in Settings, Model providers; bedrock.region is not read again")
		return nil
	}

	seedOptions, _ := seed.Options.Encode()
	storedOptions, _ := stored.Options.Encode()
	if stored.Region != seed.Region || (seedOptions != "{}" && seedOptions != storedOptions) {
		log.Warn("the configuration file disagrees with the bedrock provider configured in the console; the file is ignored",
			"file_region", seed.Region, "configured_region", stored.Region,
			"hint", "remove bedrock.region (PANORAMA_BEDROCK_REGION) and its options from the file; "+
				"a later release refuses to start while they are set")
	}
	return nil
}

// mountDataPlane wires the proxy onto the server.
//
// Mounted on every deployment. With no provider serving, every request is
// refused - naming the deployment as the fault - which is a legitimate state
// during setup, and refusing to boot would make it impossible to reach the
// console that fixes it.
func mountDataPlane(
	cfg config.Config,
	db *store.DB,
	log *slog.Logger,
	srv *controlplane.Server,
	prices *pricing.Table,
	rules *policy.Engine,
	search *websearch.Service,
) error {
	extractor, sessions, err := attributionFor(cfg, log)
	if err != nil {
		return err
	}

	dp := dataplane.New(dataplane.Options{
		Store:        db,
		Prices:       prices,
		Policy:       rules,
		Attribution:  extractor,
		Sessions:     sessions,
		WebSearch:    search,
		Logger:       log,
		MaxBodyBytes: cfg.Server.MaxRequestBytes,
	})

	for pattern, handler := range dp.Routes() {
		srv.Handle(pattern, handler)
	}

	log.Info("data plane mounted", "surfaces", "anthropic, openai-chat, openai-responses, mcp")
	return nil
}

// attributionFor builds the in-flight extractor and its session cache.
//
// Returning a nil extractor is a supported state, not a failure: a deployment
// that turns attribution off records every request honestly as unattributed,
// which is a worse product and a coherent one.
func attributionFor(cfg config.Config, log *slog.Logger) (*attribution.Extractor, *attribution.Sessions, error) {
	if !cfg.Attribution.On() {
		log.Warn("attribution is off; spend will be recorded but not tied to any work",
			"hint", "remove attribution.enabled to turn it back on")
		return nil, nil, nil
	}

	extractor, err := attribution.NewExtractor(cfg.Attribution.TicketPattern)
	if err != nil {
		return nil, nil, fmt.Errorf("attribution.ticket_pattern: %w", err)
	}

	pattern := cfg.Attribution.TicketPattern
	if pattern == "" {
		pattern = attribution.DefaultTicketPattern
	}
	log.Info("attribution enabled", "ticket_pattern", pattern)

	return extractor, attribution.NewSessions(cfg.Attribution.SessionCacheSize), nil
}

// admin issues the code that configures a deployment.
//
// One command, for two situations that are the same situation: a deployment
// nobody has signed in to yet, and a deployment nobody can sign in to any more
// because its group mapping admits nobody or its identity provider changed.
// Both are answered by proof of access to the deployment itself.
//
// Not refused when users exist. That is the point - the second situation only arises on a deployment full of people.
// What keeps it acceptable is what the code opens: no account, no password, an
// hour, and nothing reachable but this deployment's own configuration.
func admin(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("admin: want a subcommand (setup, generate-key)")
	}

	// generate-key touches no database, so it works before a deployment exists
	// - which is when it is needed, since the key belongs in the configuration
	// the deployment starts with.
	if args[0] == "generate-key" {
		key, err := secrets.NewKey()
		if err != nil {
			return err
		}
		fmt.Printf(`Encryption key:

  %s

Set it as PANORAMA_ENCRYPTION_KEY, or encryption_key in the configuration file.

It protects the credentials administrators store from the console - today the
GitHub connection. The key stays in your configuration while what it protects
lives in the database, so a dump or a backup carries no working credential.

Keep it: lose it and those credentials cannot be read back, and each has to be
entered again in the console.

You do not have to use this. Any passphrase of %d characters or more works as
the key, and one is derived from it. This one is simply already the full 32
bytes, so nothing is derived and nothing is weaker than what you typed.
`, key, secrets.MinPassphrase)
		return nil
	}

	if args[0] != "setup" {
		return errors.New("admin: want a subcommand (setup, generate-key)")
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

	code, err := controlplane.IssueSetupCode(ctx, db)
	if err != nil {
		return err
	}

	fmt.Printf(`Setup code:

  %s

Open %s and enter it. It works once, opens a session lasting an hour, and is
destroyed on use. Any code issued earlier has stopped working.

That session creates no account. It configures single sign-on, the model
providers, and the group-to-role mapping, and nothing else: no gateway token,
no spend, no people, no routes. Finish, sign in through your identity provider,
and it expires behind you.

Evaluating without single sign-on? Mint a gateway token instead; it authenticates
the console as well as a coding tool:

  panorama token create -email you@example.com -role admin

`, code, cfg.Server.BaseURL)
	return nil
}

// token handles credential management for coding tools.
func token(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("token: want a subcommand (create)")
	}

	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	email := fs.String("email", "", "email address of the token's owner (required)")
	name := fs.String("name", "", "label for the token, e.g. laptop")
	role := fs.String("role", store.RoleDeveloper, "role to give the user if they do not exist yet")

	switch args[0] {
	case "create":
		if perr := parseFlags(fs, args[1:]); perr != nil {
			return perr
		}
		if *email == "" {
			return errors.New("token create: -email is required")
		}
		if !store.ValidRole(*role) {
			return fmt.Errorf("token create: %q is not a valid role (admin, manager or developer)", *role)
		}
	default:
		return fmt.Errorf("token: unknown subcommand %q (want create)", args[0])
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

	user, err := db.EnsureUser(ctx, *email, "", *role)
	if err != nil {
		return err
	}

	minted, err := auth.Generate()
	if err != nil {
		return err
	}

	// The same lifetime the console mints, from the same function, so "a
	// gateway token lasts thirty days" is true of both doors.
	expiresAt := auth.GatewayTokenExpiry(time.Now(), cfg.Auth.MaxTokenTTL)
	if _, err := db.CreateGatewayToken(ctx, user.ID, *name, minted.Hash, minted.DisplayPrefix, &expiresAt); err != nil {
		return err
	}

	// The plaintext exists only here and is never stored. Printing it to
	// stdout, separated from the log stream, is what lets the operator pipe it
	// somewhere without capturing the surrounding noise.
	fmt.Printf(`Gateway token for %s (%s). Expires %s.

  %s

This is shown once and is not recoverable - Panorama stores only its hash.

Point a coding tool at this deployment:

  export ANTHROPIC_BASE_URL=%s
  export ANTHROPIC_AUTH_TOKEN=%s
`, user.Email, user.Role, expiresAt.Format("2 Jan 2006"),
		minted.Plaintext, cfg.Server.BaseURL, minted.Plaintext)

	return nil
}

// team manages the middle scope: the grouping a budget can govern and the only
// level at which one person's spend is ever shown to another.
func team(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("team: want a subcommand (create, list or add)")
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

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("team create", flag.ContinueOnError)
		name := fs.String("name", "", "team name (required)")
		slug := fs.String("slug", "", "stable identifier used in policy files (defaults to a slug of the name)")
		desc := fs.String("description", "", "what this team does")
		if perr := parseFlags(fs, args[1:]); perr != nil {
			return perr
		}
		if *name == "" {
			return errors.New("team create: -name is required")
		}

		t, cerr := db.CreateTeam(ctx, *name, *slug, *desc)
		if cerr != nil {
			return cerr
		}
		fmt.Printf("Created team %s (%s).\n\nRefer to it in a policy file as:\n\n  scope: team\n  id: %s\n",
			t.Name, t.ID, t.Slug)
		return nil

	case "list":
		teams, lerr := db.ListTeams(ctx)
		if lerr != nil {
			return lerr
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "SLUG\tNAME\tMEMBERS\n")
		for _, t := range teams {
			n, cerr := db.TeamMemberCount(ctx, t.ID)
			if cerr != nil {
				return cerr
			}
			fmt.Fprintf(w, "%s\t%s\t%d\n", t.Slug, t.Name, n)
		}
		return w.Flush()

	case "add":
		fs := flag.NewFlagSet("team add", flag.ContinueOnError)
		slug := fs.String("team", "", "team slug (required)")
		email := fs.String("email", "", "email address of the user to add (required)")
		role := fs.String("role", store.TeamRoleMember, "lead or member")
		if perr := parseFlags(fs, args[1:]); perr != nil {
			return perr
		}
		if *slug == "" || *email == "" {
			return errors.New("team add: -team and -email are required")
		}

		t, terr := db.TeamBySlug(ctx, *slug)
		if terr != nil {
			return fmt.Errorf("team add: %w", terr)
		}
		u, uerr := db.UserByEmail(ctx, *email)
		if uerr != nil {
			return fmt.Errorf("team add: %s: %w", *email, uerr)
		}
		if aerr := db.AddTeamMember(ctx, t.ID, u.ID, *role); aerr != nil {
			return aerr
		}
		fmt.Printf("%s is now a %s of %s.\n", u.Email, *role, t.Name)
		return nil

	default:
		return fmt.Errorf("team: unknown subcommand %q (want create, list or add)", args[0])
	}
}

// policyCmd manages routes and budgets.
//
// The database is authoritative at runtime, and this applies a file into it
// rather than replacing it: routes, their rules, and budgets keep their
// identity across edits, so re-applying the same document on every deploy is a
// no-op and every request already recorded against a rule stays explicable.
func policyCmd(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("policy: want a subcommand (show, check or apply)")
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

	switch args[0] {
	case "show":
		return showPolicy(ctx, db, log)

	case "check", "apply":
		fs := flag.NewFlagSet("policy "+args[0], flag.ContinueOnError)
		file := fs.String("f", "", "policy file to read (required)")
		if perr := parseFlags(fs, args[1:]); perr != nil {
			return perr
		}
		if *file == "" {
			return fmt.Errorf("policy %s: -f is required", args[0])
		}

		set, perr := loadPolicyFile(ctx, db, *file)
		if perr != nil {
			return perr
		}

		if args[0] == "check" {
			fmt.Printf("%s is valid: %d routes, %d budgets.\n", *file, len(set.Routes), len(set.Budgets))
			return nil
		}

		changes, aerr := db.ApplyPolicy(ctx, set)
		if aerr != nil {
			return aerr
		}
		fmt.Printf("Applied %s: %d added, %d updated, %d removed, %d unchanged.\n",
			*file, changes.Added, changes.Updated, changes.Removed, changes.Unchanged)
		if changes.Removed > 0 {
			fmt.Println("\nRemoved entries were not in the file. A policy file is the whole policy.")
		}
		if changes.Changed() {
			fmt.Printf("\nA running gateway serves the change within %s.\n", policy.GenerationPollInterval)
		}
		return nil

	default:
		return fmt.Errorf("policy: unknown subcommand %q (want show, check or apply)", args[0])
	}
}

// loadPolicyFile reads, compiles, and sanity-checks a policy document.
func loadPolicyFile(ctx context.Context, db *store.DB, path string) (policy.Set, error) {

	f, err := os.Open(path) //nolint:gosec // the path is operator-supplied by design
	if err != nil {
		return policy.Set{}, fmt.Errorf("policy: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	doc, err := policy.ParseDocument(f)
	if err != nil {
		return policy.Set{}, err
	}

	set, err := doc.Compile(ctx, db)
	if err != nil {
		return policy.Set{}, fmt.Errorf("policy: %s is not valid:\n%w", path, err)
	}
	if err := validateTargets(ctx, db, set); err != nil {
		return policy.Set{}, fmt.Errorf("policy: %s is not valid:\n%w", path, err)
	}
	return set, nil
}

// validateTargets refuses a policy that names models this deployment cannot
// invoke.
//
// The moment to discover that a route points nowhere is here, at the desk of the
// person who wrote it - not when an engineer's request is refused. Checked
// through the same function the console calls, against the discovered catalogue
// and the configured providers, so a document and a console cannot disagree
// about which models exist.
func validateTargets(ctx context.Context, db *store.DB, set policy.Set) error {
	catalogue, err := db.ListCatalogue(ctx)
	if err != nil {
		return fmt.Errorf("reading the model catalogue: %w", err)
	}
	configured, err := db.ListProviders(ctx)
	if err != nil {
		return fmt.Errorf("reading the model providers: %w", err)
	}
	// Settings only: nothing here invokes anything, so there is no reason to
	// build a provider just to ask whether it is switched on.
	snapshot := policy.Set{Providers: map[providers.Kind]providers.State{}}
	for _, s := range configured {
		snapshot.Providers[s.Kind] = providers.State{Settings: s}
	}

	var errs []error
	for _, route := range set.Routes {
		if msg := controlplane.RouteTargetProblem(route, catalogue, snapshot); msg != "" {
			errs = append(errs, fmt.Errorf("route %q: %s", route.Name, msg))
		}
	}
	return errors.Join(errs...)
}

// showPolicy prints the routes and budgets in force, with what has been spent
// against each.
func showPolicy(ctx context.Context, db *store.DB, log *slog.Logger) error {
	engine := policy.New(policy.Options{
		Repository: db,
		// Negative disables the background loop: this is a one-shot command.
		ReloadInterval: -1,
		Logger:         log,
	})
	if err := engine.Load(ctx); err != nil {
		return err
	}

	set := engine.Snapshot()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ROUTE\tAVAILABLE TO\tPAST SOFT\tMODEL\n")
	for _, route := range set.Routes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			route.Name, describeConditions(route), pastSoft(route.ServesOverSoft),
			dash(route.Target.String()))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(set.Routes) == 0 {
		fmt.Println("No routes. Every request is refused: a route is the only name a client may ask for.")
	}

	fmt.Println()

	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "BUDGET\tSCOPE\tPERIOD\tSPENT\tSOFT\tHARD\tREQUESTS\tSTATE\n")
	for _, s := range engine.Status() {
		spent := s.Spend.Cost.Display()
		if s.Spend.Unpriced > 0 {
			spent += fmt.Sprintf(" (+%d unpriced)", s.Spend.Unpriced)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			s.Budget.Label(), s.Budget.Scope, s.Budget.Period,
			spent, limit(s.Budget.SoftLimit), limit(s.Budget.HardLimit),
			s.Spend.Requests, s.State)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(set.Budgets) == 0 {
		fmt.Println("No budgets. Nothing is capped.")
	}
	return nil
}

// describeConditions says who a route exists for.
func describeConditions(r policy.Route) string {
	var parts []string
	for _, c := range []struct {
		label  string
		values []string
	}{
		{"users", r.Users}, {"teams", r.Teams}, {"tools", r.Tools},
	} {
		if len(c.values) > 0 {
			parts = append(parts, fmt.Sprintf("%s=%s", c.label, strings.Join(c.values, "|")))
		}
	}
	if len(parts) == 0 {
		return "everyone"
	}
	return strings.Join(parts, ",")
}

func limit(m *pricing.Money) string {
	if m == nil {
		return "-"
	}
	return m.Display()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// pastSoft renders whether a route survives a soft limit, as an answer rather
// than a flag. "no" is the interesting one and it is the one an operator is
// scanning the column for, so it says what happens rather than repeating the
// field name.
func pastSoft(serves bool) string {
	if serves {
		return "serves"
	}
	return "stops"
}

// models discovers and shows what this deployment can actually invoke.
//
// `discover` is the same work the background job does, run on demand, because
// the questions it answers - why is this model missing, where would my prompts
// go, what does this geography cost - are asked while somebody is configuring a
// deployment rather than on an interval.
func models(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("models: want a subcommand (discover or list)")
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

	switch args[0] {
	case "discover":
		registry, rerr := cliRegistry(ctx, cfg, db, log)
		if rerr != nil {
			return rerr
		}
		instances := registry.Instances()
		if len(instances) == 0 {
			return errors.New("models discover: no model provider is serving - configure one with " +
				"`panorama providers set bedrock -region <region>`, or in Settings, Model providers")
		}
		var failures []error
		for _, inst := range instances {
			n, derr := jobs.Discover(ctx, db, inst)
			if derr != nil {
				failures = append(failures, derr)
				continue
			}
			fmt.Printf("discovered %d targets on %s in %s\n", n, inst.Settings.Kind, inst.Settings.Region)
		}
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
		fmt.Println()
		fallthrough

	case "list":
		entries, lerr := db.ListCatalogue(ctx)
		if lerr != nil {
			return lerr
		}
		if len(entries) == 0 {
			fmt.Println("No models discovered yet. Run: panorama models discover")
			return nil
		}

		svc := pricingService(cfg, db, providers.NewRegistry(log), log)
		if serr := svc.Load(ctx); serr != nil {
			return serr
		}
		table := svc.Table()

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "MODEL\tNAME\tGEO\tPROCESSED IN\tAPIS\tSTREAM\tLIFECYCLE\tINPUT $/M\n")
		for _, e := range entries {
			// Priced through the same key the request path uses, so what this
			// prints is what a request would actually be billed at rather than
			// a second derivation that can disagree.
			in := "unpriced"
			if r, ok := table.Lookup(string(e.Provider), e.ID); ok && r.Rates.Input != 0 {
				in = rate(r.Rates.Input)
			}
			apis := providers.JoinAPIs(e.APIs)
			if apis == "" {
				apis = "browse only"
			}
			lifecycle := dashIfEmpty(e.Lifecycle)
			if e.Status != "" {
				lifecycle = e.Status
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				e.Ref(), e.DisplayName, dashIfEmpty(e.Geography), dashIfEmpty(e.Residency()),
				apis, yesNo(e.Streaming), lifecycle, in)
		}
		if ferr := w.Flush(); ferr != nil {
			return ferr
		}

		fetched, aerr := db.CatalogueFetchedAt(ctx)
		if aerr != nil {
			return aerr
		}
		fmt.Printf("\n%d targets.", len(entries))
		for _, k := range providers.Kinds() {
			if at, ok := fetched[k]; ok {
				fmt.Printf(" %s discovered %s.", k, at.Format(time.RFC3339))
			}
		}
		fmt.Println()
		return nil

	default:
		return fmt.Errorf("models: unknown subcommand %q (want discover or list)", args[0])
	}
}

// cliRegistry builds the providers configured in the database, for a one-shot
// command. The same descriptors and the same first-start seed the server uses,
// so a command run before the first `serve` sees the provider that serve would.
func cliRegistry(ctx context.Context, cfg config.Config, db *store.DB, log *slog.Logger) (*providers.Registry, error) {
	registry := providers.NewRegistry(log, bedrock.Descriptors()...)
	if err := seedProviders(ctx, cfg, db, log); err != nil {
		return nil, err
	}
	configured, err := db.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for kind, state := range registry.Sync(ctx, configured) {
		if state.Err != nil {
			return nil, fmt.Errorf("the %s provider could not be built: %w", kind, state.Err)
		}
	}
	return registry, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func prices(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("prices: want a subcommand (list or refresh)")
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

	svc := pricingService(cfg, db, providers.NewRegistry(log), log)
	if err := svc.Load(ctx); err != nil {
		return err
	}

	switch args[0] {
	case "refresh":
		if cfg.Pricing.RefreshInterval <= 0 {
			return errors.New("prices refresh: pricing.refresh_interval is 0, which disables the price source")
		}
		registry, rerr := cliRegistry(ctx, cfg, db, log)
		if rerr != nil {
			return rerr
		}
		if len(registry.PriceSources()) == 0 {
			return errors.New("prices refresh: no model provider is serving - " +
				"the fetch reads prices for the region each provider invokes")
		}
		svc = pricingService(cfg, db, registry, log)
		if err := svc.Refresh(ctx); err != nil {
			return err
		}
		fallthrough

	case "list":
		entries := svc.Table().All()
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "MODEL\tSCOPE\tINPUT\tOUTPUT\tCACHE READ\tCACHE WRITE\tCACHE WRITE 1H\tSOURCE\n")
		tiered := 0
		for _, e := range entries {
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				e.Provider, e.Model, e.Scope,
				rate(e.Rates.Input), rate(e.Rates.Output), rate(e.Rates.CacheRead),
				rate(e.Rates.CacheWrite), rate(e.Rates.CacheWrite1h), e.Source)
			// A long-context price replaces the row above for a larger
			// prompt, so it is shown under it rather than as a model of its
			// own.
			for _, t := range e.Tiers {
				fmt.Fprintf(w, "  above %d input tokens\t%s\t%s\t%s\t%s\t%s\t%s\t\n",
					t.AboveInputTokens, e.Scope,
					rate(t.Rates.Input), rate(t.Rates.Output), rate(t.Rates.CacheRead),
					rate(t.Rates.CacheWrite), rate(t.Rates.CacheWrite1h))
			}
			if len(e.Tiers) > 0 {
				tiered++
			}
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Printf("\n%d rates, in US dollars per million tokens.\n", len(entries))
		if tiered > 0 {
			fmt.Printf("%d have a long-context price, which replaces every rate for a request whose prompt is larger.\n", tiered)
		}
		return nil

	default:
		return fmt.Errorf("prices: unknown subcommand %q (want list or refresh)", args[0])
	}
}

// rate renders a rate for the table, showing an unpublished one as a dash
// rather than as zero. Zero would read as free.
func rate(r pricing.Rate) string {
	if r == 0 {
		return "-"
	}

	// Trim the trailing zeros of the nine-decimal exact form, but keep at least
	// two so a column of prices lines up as money rather than as arbitrary
	// numbers.
	s := strings.TrimRight(pricing.Money(r).USD(), "0")
	dot := strings.IndexByte(s, '.')
	for dot >= 0 && len(s)-dot-1 < 2 {
		s += "0"
	}
	return s
}

func migrate(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("migrate: want a subcommand (up or status)")
	}

	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Error("closing the database", "error", err)
		}
	}()

	switch args[0] {
	case "up":
		pending, err := db.PendingMigrations(ctx)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			log.Info("schema is already current")
			return nil
		}
		if err := db.Migrate(ctx); err != nil {
			return err
		}
		log.Info("migrations applied", "count", len(pending), "driver", db.Driver())
		return nil

	case "status":
		states, err := db.MigrationStatus(ctx)
		if err != nil {
			return err
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "VERSION\tSTATE\tSOURCE\n")
		for _, m := range states {
			state := "pending"
			if m.Applied {
				state = "applied"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\n", m.Version, state, m.Source)
		}
		return w.Flush()

	default:
		return fmt.Errorf("migrate: unknown subcommand %q (want up or status)", args[0])
	}
}
