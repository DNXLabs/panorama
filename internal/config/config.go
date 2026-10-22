// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package config loads and validates Panorama's configuration.
//
// Configuration comes from two places: an optional YAML file and the process
// environment, in that order - environment wins. Validation is strict and
// happens once, at startup. Panorama refuses to start on an invalid
// configuration rather than falling back to a default that would surprise the
// operator later, and unknown keys in the YAML file are an error rather than a
// silently ignored typo.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dnxlabs/panorama/internal/pricing"
	"github.com/dnxlabs/panorama/internal/providers"
	"github.com/dnxlabs/panorama/internal/secrets"
)

// Driver names for the supported databases. These are an allowlist: anything
// else is rejected at load time.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// DefaultSQLiteDSN is the zero-configuration database: a file beside the
// binary, in the working directory the container image sets to its volume.
//
// It is a named constant because the discrete Postgres form has to tell "the
// operator gave us a DSN" from "nobody said anything and this default is still
// here". A defaulted field is indistinguishable from an explicitly set one
// after decoding, so the sentinel is the comparison, and the consequence is
// stated rather than hidden: an operator who sets database.host *and* writes
// this exact string as database.dsn gets the assembled DSN rather than an
// error. Every other combination of the two is refused.
const DefaultSQLiteDSN = "file:panorama.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"

// sslModes is the allowlist of libpq sslmode values accepted in the discrete
// form. An unknown value is refused at load time rather than carried into a
// connection string for pgx to reject at the first query.
var sslModes = map[string]bool{
	"disable":     true,
	"allow":       true,
	"prefer":      true,
	"require":     true,
	"verify-ca":   true,
	"verify-full": true,
}

// DefaultPath is where Panorama looks for a configuration file when none is
// given explicitly. A missing file there is not an error; a missing file at an
// explicitly requested path is.
const DefaultPath = "config.yaml"

// Config is the fully resolved configuration for a running instance.
type Config struct {
	// EncryptionKey protects the credentials an administrator stores from the
	// console - today the integration connections.
	//
	// Either 32 base64 bytes, which `panorama admin generate-key` mints and
	// which are used exactly as given, or any passphrase of at least
	// secrets.MinPassphrase characters, which the key is derived from. See
	// secrets.DeriveKey for why the first case is not simply the second.
	//
	// It stays in the file while the credentials it protects live in the
	// database, and that split is the point: a dump, a replica or a backup
	// carries no working credential, because the one thing needed to read them
	// was never in any of them.
	//
	// Optional. A deployment without one runs normally and simply cannot store
	// a credential - the console says so at the moment somebody tries.
	EncryptionKey string `yaml:"encryption_key"`

	Server       Server       `yaml:"server"`
	Database     Database     `yaml:"database"`
	Log          Log          `yaml:"log"`
	Bedrock      Bedrock      `yaml:"bedrock"`
	Providers    Providers    `yaml:"providers"`
	Pricing      Pricing      `yaml:"pricing"`
	Policy       Policy       `yaml:"policy"`
	Auth         Auth         `yaml:"auth"`
	Attribution  Attribution  `yaml:"attribution"`
	Integrations Integrations `yaml:"integrations"`
	OIDC         OIDC         `yaml:"oidc"`
	WebSearch    WebSearch    `yaml:"web_search"`
	Dev          Dev          `yaml:"dev"`
}

// WebSearch is the search gateway a deployment created for itself.
//
// Web search is otherwise configured in the console (Settings, Web search) and
// stored in the database. A gateway the deployment's own stack created is
// different: the stack made it and will delete it, so a URL changed in the
// console would point somewhere nobody manages. When this is set the console
// shows the gateway and its price and does not offer to change either, search
// is on unless an administrator has switched it off, and that switch is the
// only thing the database keeps.
type WebSearch struct {
	// GatewayURL is the AgentCore gateway's MCP endpoint, the
	// WebSearchGatewayUrl output of the CloudFormation stack. Empty leaves the
	// whole of web search to the console.
	GatewayURL string `yaml:"gateway_url"`
}

// Integrations configures the systems of record that confirm inferred work and
// supply outcomes.
//
// Nothing here is required. A deployment with this block empty attributes spend
// exactly as well as one with GitHub and Jira connected; what it does not have
// is titles, merge status, and cost per merged pull request, and every screen
// that would show those says so rather than showing a zero.
type Integrations struct {
	// SyncInterval is how often the connected systems are asked about the work
	// items Panorama has seen. Zero uses the default.
	SyncInterval time.Duration `yaml:"sync_interval"`
	// StaleAfter is how long a confirmed answer is trusted before it is asked
	// again. Zero uses the default.
	StaleAfter time.Duration `yaml:"stale_after"`
}

// Providers an integration may name. Both are chosen in the console.
const (
	ProviderMock   = "mock"
	ProviderGitHub = "github"
)

func (i *Integrations) validate() []error {
	var errs []error

	if i.SyncInterval < 0 {
		errs = append(errs, errors.New("integrations.sync_interval: must not be negative"))
	}
	if i.StaleAfter < 0 {
		errs = append(errs, errors.New("integrations.stale_after: must not be negative"))
	}
	return errs
}

// Attribution configures how spend is tied to the work it paid for.
//
// There is very little here on purpose. Attribution works with no client
// configuration and, deliberately, with almost no server configuration either:
// the extraction rules are properties of the coding tools, not of the customer.
// The one exception is what a ticket key looks like, which is a fact about the
// customer's issue tracker that nothing in a request body can reveal.
type Attribution struct {
	// Enabled turns in-flight extraction on. It is on by default: a gateway
	// that does not attribute is missing the reason it was deployed.
	Enabled *bool `yaml:"enabled"`

	// TicketPattern is the regular expression that recognises a ticket key.
	// Empty uses the default, which matches the shape most trackers use.
	//
	// Naming your projects - (PAY|BILL)-\d+ - turns the lowest rung of the
	// provenance ladder from a heuristic into an allowlist, and is the
	// recommended setting for anyone who cares about precision. The default has
	// to guess, and guards itself against the identifiers that look like ticket
	// keys and are not: UTF-8, SHA-1, CVE-2024.
	TicketPattern string `yaml:"ticket_pattern"`

	// SessionCacheSize is how many live conversations are remembered in memory
	// so that a session is attributed once rather than once per turn. Zero uses
	// the default.
	SessionCacheSize int `yaml:"session_cache_size"`
}

// On reports whether attribution is enabled.
func (a *Attribution) On() bool { return a.Enabled == nil || *a.Enabled }

// Policy configures how routes and budgets are enforced.
//
// Neither is here. They live in the database, because the console edits them
// and because a budget has to be enforced against spend that only the database
// knows - see `panorama policy apply` for the file form.
type Policy struct {
	// ReloadInterval is how often routes, budgets, and spend are re-read.
	//
	// Between reloads a replica still counts its own traffic, so this does not
	// bound how fast a budget reacts to what this process is serving - only how
	// fast it learns what other replicas have served. A single-replica
	// deployment is unaffected by the value. Nor does it bound how fast a change
	// to a route, budget, team or provider arrives: that is served within a
	// second, whatever this is set to.
	ReloadInterval time.Duration `yaml:"reload_interval"`
}

// Auth configures how long the credentials a developer holds stay valid.
//
// The two lifetimes do different jobs and are set independently. A grant is
// what `panorama login` leaves on a laptop and is measured in weeks, because
// signing in is a thing a person does. A token is what the grant exchanges for
// and is measured in hours, because it is what actually buys model calls - and
// its length is how long a revoked laptop keeps working.
//
// Shortening TokenTTL therefore tightens revocation and costs nothing but more
// exchanges; there is no reason to make it long.
type Auth struct {
	// GrantTTL is how long a signed-in CLI stays signed in. Zero means the
	// grant never expires on its own and is ended only by revocation.
	GrantTTL time.Duration `yaml:"grant_ttl"`
	// TokenTTL is the lifetime of each gateway token minted from a grant, and
	// so the upper bound on how long a revoked grant keeps buying model calls.
	TokenTTL time.Duration `yaml:"token_ttl"`
	// DeviceCodeTTL is how long a device-authorization code waits for a human.
	DeviceCodeTTL time.Duration `yaml:"device_code_ttl"`
	// DevicePollInterval is the cadence a CLI is told to poll at. Polling
	// faster is answered with slow_down rather than with a grant.
	DevicePollInterval time.Duration `yaml:"device_poll_interval"`
	// MaxTokenTTL shortens the thirty days a minted gateway token lives. It
	// binds downward only: zero, or anything above thirty days, leaves the
	// thirty days as they are.
	MaxTokenTTL time.Duration `yaml:"max_token_ttl"`
}

// Server configures the HTTP listener that serves the data plane, the control
// plane API, and the embedded UI.
type Server struct {
	// Addr is the listen address, as host:port. An empty host listens on all
	// interfaces.
	Addr string `yaml:"addr"`
	// BaseURL is the externally reachable origin of this deployment, e.g.
	// https://panorama.internal. It is used to build OIDC redirect URLs and the
	// endpoint shown in client setup instructions, so it must match what clients
	// actually dial.
	BaseURL string `yaml:"base_url"`
	// ReadHeaderTimeout bounds how long a client may take to send its request
	// headers. It deliberately does not bound the body or the response: streaming
	// responses are long-lived by design.
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	// ShutdownTimeout bounds how long a graceful shutdown waits for in-flight
	// requests - including active streams - before the process exits anyway.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// MaxRequestBytes caps a proxied request body. Coding-agent requests carry
	// whole conversations, so the ceiling is high - but unbounded is not a
	// ceiling.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// Database configures persistent storage. SQLite is the zero-dependency
// default; Postgres is the production target and is required for background
// jobs.
type Database struct {
	// Driver is either "sqlite" or "postgres".
	Driver string `yaml:"driver"`
	// DSN is the driver-specific data source name. It is one of two ways to
	// say where the database is; Host is the other, and setting both is an
	// error rather than a precedence rule.
	DSN string `yaml:"dsn"`

	// Host selects the discrete form: when it is set, the Postgres DSN is
	// assembled from the fields below instead of being given whole.
	//
	// This exists because of how a secret reaches a container in AWS. ECS
	// injects a secret one of two ways - a whole secret value into one
	// environment variable, or one JSON key of a secret into one environment
	// variable - and it cannot concatenate. RDS, and every managed rotation
	// AWS offers, writes a JSON document: username, password, host, port,
	// dbname. So there is no path from what RDS writes to a single DSN string
	// without something in between assembling it, and the two candidates for
	// that something are both worse than this. Resolving the password into the
	// task definition's environment renders it in plaintext to anyone holding
	// ecs:DescribeTaskDefinition; a custom resource that reads one secret and
	// writes another is a second moving part that rotation would then have to
	// re-trigger.
	//
	// Assembling here costs a URL build and lets the password arrive as a
	// secret reference that is resolved by the task, never by the template.
	Host string `yaml:"host"`
	// Port is the Postgres port. Used only in the discrete form.
	Port int `yaml:"port"`
	// Name is the database name. Used only in the discrete form.
	Name string `yaml:"name"`
	// User is the database user. Used only in the discrete form.
	User string `yaml:"user"`
	// Password is the database password. Prefer PasswordFile or the
	// PANORAMA_DATABASE_PASSWORD environment variable over writing it into a
	// configuration file. Used only in the discrete form.
	Password string `yaml:"password"`
	// PasswordFile reads the password from a file, which is how a Kubernetes
	// secret or a Docker secret mount arrives. ECS needs neither: it injects
	// the value as an environment variable directly from Secrets Manager.
	PasswordFile string `yaml:"password_file"`
	// SSLMode is the libpq sslmode for the assembled DSN. Defaults to
	// "require", which encrypts the connection without verifying the server
	// certificate - the ordinary posture for a task and an RDS instance in one
	// VPC. "verify-full" is the stronger setting and needs the Amazon RDS CA
	// bundle present in the image, which the distroless base does not carry.
	SSLMode string `yaml:"sslmode"`

	// MaxOpenConns caps the connection pool. Ignored for SQLite, which is held
	// to a single writer.
	MaxOpenConns int `yaml:"max_open_conns"`
	// AutoMigrate runs pending migrations at startup. Convenient for the
	// single-container quickstart; operators running multiple replicas should
	// disable it and migrate as a separate step.
	AutoMigrate bool `yaml:"auto_migrate"`
}

// Log configures structured logging.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `yaml:"level"`
	// Format is either "json" or "text".
	Format string `yaml:"format"`
}

// Bedrock seeds the bedrock provider the first time a deployment starts.
//
// Model providers are configured in the console (Settings, Model providers)
// and stored in the database - one row per provider, each with its own region - because a region
// is an operational decision made by an administrator long after whoever wrote
// this file has moved on. So these keys are read once: when a deployment starts
// with no row for the bedrock provider, a row is created from them. After that
// the row is the configuration, the file is ignored, and a file that disagrees
// with the row is reported at WARN on every start.
//
// Credentials are not configured here and never were: the AWS SDK's default
// credential chain applies, so an instance role, a task role, or the ambient
// environment supplies them.
type Bedrock struct {
	// Region seeds the bedrock provider's region, e.g. ap-southeast-2. Setting
	// it on a fresh deployment is how compose and CloudFormation arrive with a
	// provider already configured.
	Region string `yaml:"region"`
	// UnsupportedBetas seeds the bedrock provider's list of beta feature flags
	// to drop before a request reaches Bedrock. Unset uses the provider's
	// built-in list; an empty list forwards every flag.
	UnsupportedBetas []string `yaml:"unsupported_betas"`
	// ContextWindows seeds the bedrock provider's context-window overrides,
	// keyed by model family ("anthropic.claude-sonnet-5") or exact id. Setting
	// a family to 0 makes it unrecorded.
	ContextWindows map[string]int `yaml:"context_windows"`
}

// Providers configures what applies to every model provider.
type Providers struct {
	// DiscoveryInterval is how often each provider is asked what this account
	// and its region can invoke. Zero uses the default; negative disables the
	// job, which is what an air-gapped deployment configured entirely from its
	// file wants.
	//
	// The answer changes when AWS ships a model or somebody edits an IAM
	// policy, so this is hours rather than minutes. The console offers a
	// refresh for the case where somebody has just made such a change.
	DiscoveryInterval time.Duration `yaml:"discovery_interval"`
}

// Pricing configures how token counts become money.
type Pricing struct {
	// RefreshInterval is how often rates are re-fetched from the AWS Price List
	// API. Zero disables the refresh and leaves whatever is already in the
	// price table - the compiled-in seed, plus anything configured below.
	//
	// The fetch needs two IAM permissions, pricing:GetProducts and
	// bedrock:ListFoundationModels. The second is not incidental: the price
	// catalogue identifies a model only by a display name, and Bedrock's model
	// list is what maps that name to the id the gateway actually invokes.
	RefreshInterval time.Duration `yaml:"refresh_interval"`

	// Models overrides published rates per model, in dollars per million
	// tokens. It exists for two real cases: an enterprise agreement whose
	// prices are not the public ones, and a model too new to be in either the
	// seed or the catalogue.
	//
	// The key is the model a route points at, as provider/model -
	// bedrock/us.anthropic.claude-sonnet-5 - with the inference-profile prefix
	// exactly as Bedrock publishes it. The prefix is part of the key because it
	// selects which of a model's two published rate sets is being overridden:
	// `us.` rates and `global.` rates differ by about 10%.
	//
	// A model whose price changes above a prompt size - GPT-5.x above 272K
	// input tokens - states the larger price under tiers, which replaces every
	// rate for a request that large.
	//
	// A key must name its provider. A bare model id is refused with the
	// spelling to use, because the same id is valid on more than one provider
	// and the two differ in where a prompt is processed and what it costs.
	Models map[string]ModelRates `yaml:"models"`
}

// ModelRates is one model's rates, in dollars per million tokens.
//
// The values are decimal text rather than numbers so they survive as written.
// A rate parsed into a float64 and back is no longer the price the operator
// typed, and a cost table that disagrees with the invoice by a rounding error
// is a cost table that gets argued with instead of used.
type ModelRates struct {
	Input     Decimal `yaml:"input"`
	Output    Decimal `yaml:"output"`
	CacheRead Decimal `yaml:"cache_read"`
	// CacheWrite is the five-minute TTL rate, which is the default.
	CacheWrite Decimal `yaml:"cache_write"`
	// CacheWrite1h is the one-hour TTL rate, typically around 1.6x
	// CacheWrite. Leave it unset only if the model cannot do one-hour caching:
	// a request that uses it with no rate is recorded unpriced.
	CacheWrite1h Decimal `yaml:"cache_write_1h"`
	// Tiers are the rates that replace these for a request whose prompt is
	// larger than a threshold, in ascending order. A tier replaces every rate
	// for that request, not only the tokens past the threshold, which is how
	// Bedrock bills OpenAI's GPT-5.x above 272K input tokens. An override of
	// a model with a long-context price has to state it, or long requests are
	// costed at the short-context rate.
	Tiers []ModelTier `yaml:"tiers"`
}

// ModelTier is one long-context price in an override.
type ModelTier struct {
	// AboveInputTokens is the prompt size, cached tokens included, that the
	// tier applies above.
	AboveInputTokens int64   `yaml:"above_input_tokens"`
	Input            Decimal `yaml:"input"`
	Output           Decimal `yaml:"output"`
	CacheRead        Decimal `yaml:"cache_read"`
	CacheWrite       Decimal `yaml:"cache_write"`
	CacheWrite1h     Decimal `yaml:"cache_write_1h"`
}

// Decimal is an exact decimal number, held as the text it was written as.
type Decimal string

// UnmarshalYAML keeps the literal text of a scalar.
//
// Without this, YAML would helpfully parse 0.30 into a float64 and hand back a
// value that is not 0.30. Reading node.Value takes the characters the operator
// typed, which the pricing package then parses digit by digit.
func (d *Decimal) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a rate must be a single value like 3.00", node.Line)
	}
	*d = Decimal(node.Value)
	return nil
}

// Set reports whether a rate was given.
func (d Decimal) Set() bool { return strings.TrimSpace(string(d)) != "" }

// OIDC configures single sign-on for human users. It is optional: a
// deployment without it is reached with a gateway token. Once any field is
// set, the whole block must be coherent.
type OIDC struct {
	// Issuer is the OIDC issuer URL, used for discovery.
	Issuer string `yaml:"issuer"`
	// ClientID identifies Panorama to the identity provider.
	ClientID string `yaml:"client_id"`
	// ClientSecret is the client secret. Prefer ClientSecretFile or the
	// PANORAMA_OIDC_CLIENT_SECRET environment variable over writing it into a
	// configuration file.
	ClientSecret string `yaml:"client_secret"`
	// ClientSecretFile reads the client secret from a file, which is how
	// Kubernetes and ECS secret mounts present it.
	ClientSecretFile string `yaml:"client_secret_file"`
	// Scopes requested at authorisation. Defaults to openid, profile, email,
	// groups.
	Scopes []string `yaml:"scopes"`
	// GroupsClaim is the token claim carrying group membership, mapped to
	// Panorama roles.
	GroupsClaim string `yaml:"groups_claim"`
	// AllowedDomains limits sign-in to accounts in these domains, such as
	// example.com. Empty allows every account the issuer signs in, which is
	// right when the issuer is your own tenant. Required when the issuer is
	// Google, which signs in every Google account there is.
	AllowedDomains []string `yaml:"allowed_domains"`
}

// Dev is the local development escape hatch, and the only thing in Panorama
// that can open a console session without an identity provider.
//
// It exists because the console cannot be reviewed without being signed in to,
// and signing in means somebody's real credentials at somebody's real identity
// provider - which is not available to a test, to CI, or to a coding agent
// asked to check a change to a screen. Every other way of getting a session on
// a development stack was worse: minting a gateway token writes a credential
// nobody revokes, and a setup code opens the setup console rather than the ten
// screens the change is actually in.
//
// It is off unless SignInAs names an account, and it is refused at startup
// unless this deployment is reachable only from the machine it runs on. Both
// checks are in Validate, so a deployment that has it on and should not does
// not boot - rather than booting and serving an unauthenticated door that
// nothing on the running system would ever complain about.
//
// The account it opens is always an administrator. It is a development-only
// affordance whose whole purpose is seeing every screen, and a role knob would
// be a second thing to get wrong for no benefit - a reviewer who wants the
// developer view can change the role from People, which is a real console flow
// and better QA than a config line.
type Dev struct {
	// SignInAs is the email of the account POST /api/v1/auth/dev opens a
	// session for, creating it as an administrator if it does not exist. Empty
	// - the default, and the value of every deployment that is not somebody's
	// laptop - means the route is not registered at all.
	SignInAs string `yaml:"sign_in_as"`
}

// Enabled reports whether the development sign-in route should exist.
func (d Dev) Enabled() bool { return strings.TrimSpace(d.SignInAs) != "" }

// Default returns the configuration Panorama starts from before a file or the
// environment is applied. It is a complete, valid, single-container SQLite
// deployment.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":8088",
			BaseURL:           "http://localhost:8088",
			ReadHeaderTimeout: 15 * time.Second,
			ShutdownTimeout:   30 * time.Second,
			MaxRequestBytes:   32 << 20, // 32 MiB
		},
		Database: Database{
			Driver: DriverSQLite,
			DSN:    DefaultSQLiteDSN,
			// Defaults for the discrete form. Host is deliberately empty: it
			// is what selects that form, so everything else here is inert
			// until an operator sets it.
			Port:         5432,
			Name:         "panorama",
			SSLMode:      "require",
			MaxOpenConns: 25,
			AutoMigrate:  true,
		},
		Log: Log{
			Level:  "info",
			Format: "json",
		},
		Providers: Providers{
			// The catalogue changes when AWS ships a model or somebody edits an
			// IAM policy, neither of which happens on a cadence worth polling.
			DiscoveryInterval: DefaultDiscoveryInterval,
		},
		Pricing: Pricing{
			// Published rates move rarely, so daily is frequent enough to catch
			// a change and infrequent enough to be invisible.
			RefreshInterval: 24 * time.Hour,
			Models:          map[string]ModelRates{},
		},
		Policy: Policy{
			ReloadInterval: 30 * time.Second,
		},
		Auth: Auth{
			// A month between sign-ins is short enough that a forgotten laptop
			// stops working on its own and long enough that nobody is asked to
			// re-authenticate during a piece of work.
			GrantTTL: 30 * 24 * time.Hour,
			// An hour bounds how long a revoked grant keeps buying model calls.
			// Claude Code refreshes on its own schedule, so this is invisible
			// to the developer as long as the helper's TTL sits below it.
			TokenTTL: time.Hour,
			// Long enough to switch to a browser, sign in, and read a code;
			// short enough that an abandoned attempt does not linger.
			DeviceCodeTTL:      10 * time.Minute,
			DevicePollInterval: 5 * time.Second,
		},
		OIDC: OIDC{
			Scopes:      []string{"openid", "profile", "email", "groups"},
			GroupsClaim: "groups",
		},
	}
}

// DefaultDiscoveryInterval is how often providers are asked what they can
// invoke when nothing says otherwise.
const DefaultDiscoveryInterval = 6 * time.Hour

// Load resolves configuration from the given file path and the environment.
//
// An empty path means "look at DefaultPath, and carry on if it is not there".
// A non-empty path that does not exist is an error: an operator who names a
// file expects it to be read.
func Load(path string) (Config, error) {
	cfg := Default()

	explicit := path != ""
	if !explicit {
		path = DefaultPath
	}

	if err := applyFile(&cfg, path, explicit); err != nil {
		return Config{}, err
	}
	if err := applyEnv(&cfg, os.Getenv); err != nil {
		return Config{}, err
	}
	if err := cfg.resolveSecrets(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyFile(cfg *Config, path string, explicit bool) error {
	f, err := os.Open(path) //nolint:gosec // the path is operator-supplied by design
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return nil
		}
		return fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	// Reject unknown keys. A typo in a configuration file should fail loudly at
	// startup, not silently leave a policy unapplied.
	dec.KnownFields(true)

	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file is a legitimate way to say "use defaults".
			return nil
		}
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// getenv is the environment lookup, injected so tests need not mutate the
// process environment.
type getenv func(string) string

// applyEnv overlays PANORAMA_-prefixed environment variables. Only variables
// that are actually set are applied, so the environment overrides the file
// key by key rather than wholesale.
func applyEnv(cfg *Config, env getenv) error {
	var errs []error

	str := func(key string, dst *string) {
		if v, ok := lookup(env, key); ok {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) {
		v, ok := lookup(env, key)
		if !ok {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a duration (want e.g. 30s, 5m): %w", key, v, err))
			return
		}
		*dst = d
	}
	num := func(key string, dst *int) {
		v, ok := lookup(env, key)
		if !ok {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not an integer: %w", key, v, err))
			return
		}
		*dst = n
	}
	num64 := func(key string, dst *int64) {
		v, ok := lookup(env, key)
		if !ok {
			return
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not an integer: %w", key, v, err))
			return
		}
		*dst = n
	}
	boolean := func(key string, dst *bool) {
		v, ok := lookup(env, key)
		if !ok {
			return
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a boolean (want true or false): %w", key, v, err))
			return
		}
		*dst = b
	}
	list := func(key string, dst *[]string) {
		v, ok := lookup(env, key)
		if !ok {
			return
		}
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		*dst = out
	}

	str("PANORAMA_SERVER_ADDR", &cfg.Server.Addr)
	str("PANORAMA_SERVER_BASE_URL", &cfg.Server.BaseURL)
	dur("PANORAMA_SERVER_READ_HEADER_TIMEOUT", &cfg.Server.ReadHeaderTimeout)
	dur("PANORAMA_SERVER_SHUTDOWN_TIMEOUT", &cfg.Server.ShutdownTimeout)
	num64("PANORAMA_SERVER_MAX_REQUEST_BYTES", &cfg.Server.MaxRequestBytes)

	str("PANORAMA_DATABASE_DRIVER", &cfg.Database.Driver)
	str("PANORAMA_DATABASE_DSN", &cfg.Database.DSN)
	str("PANORAMA_DATABASE_HOST", &cfg.Database.Host)
	num("PANORAMA_DATABASE_PORT", &cfg.Database.Port)
	str("PANORAMA_DATABASE_NAME", &cfg.Database.Name)
	str("PANORAMA_DATABASE_USER", &cfg.Database.User)
	str("PANORAMA_DATABASE_PASSWORD", &cfg.Database.Password)
	str("PANORAMA_DATABASE_PASSWORD_FILE", &cfg.Database.PasswordFile)
	str("PANORAMA_DATABASE_SSLMODE", &cfg.Database.SSLMode)
	num("PANORAMA_DATABASE_MAX_OPEN_CONNS", &cfg.Database.MaxOpenConns)
	boolean("PANORAMA_DATABASE_AUTO_MIGRATE", &cfg.Database.AutoMigrate)

	str("PANORAMA_LOG_LEVEL", &cfg.Log.Level)
	str("PANORAMA_LOG_FORMAT", &cfg.Log.Format)

	// The seed of the bedrock provider, read on a deployment's first start.
	// See Bedrock.
	str("PANORAMA_BEDROCK_REGION", &cfg.Bedrock.Region)
	// See WebSearch.
	str("PANORAMA_WEB_SEARCH_GATEWAY_URL", &cfg.WebSearch.GatewayURL)
	dur("PANORAMA_PROVIDERS_DISCOVERY_INTERVAL", &cfg.Providers.DiscoveryInterval)

	list("PANORAMA_BEDROCK_UNSUPPORTED_BETAS", &cfg.Bedrock.UnsupportedBetas)
	if v, ok := lookup(env, "PANORAMA_BEDROCK_CONTEXT_WINDOWS"); ok {
		windows, err := parseContextWindows(v)
		if err != nil {
			return err
		}
		cfg.Bedrock.ContextWindows = windows
	}

	dur("PANORAMA_PRICING_REFRESH_INTERVAL", &cfg.Pricing.RefreshInterval)
	if v, ok := lookup(env, "PANORAMA_PRICING_MODELS"); ok {
		rates, err := parseModelRates(v)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.Pricing.Models = rates
		}
	}

	dur("PANORAMA_POLICY_RELOAD_INTERVAL", &cfg.Policy.ReloadInterval)

	dur("PANORAMA_AUTH_GRANT_TTL", &cfg.Auth.GrantTTL)
	dur("PANORAMA_AUTH_TOKEN_TTL", &cfg.Auth.TokenTTL)
	dur("PANORAMA_AUTH_DEVICE_CODE_TTL", &cfg.Auth.DeviceCodeTTL)
	dur("PANORAMA_AUTH_DEVICE_POLL_INTERVAL", &cfg.Auth.DevicePollInterval)
	dur("PANORAMA_AUTH_MAX_TOKEN_TTL", &cfg.Auth.MaxTokenTTL)

	if v, ok := lookup(env, "PANORAMA_ATTRIBUTION_ENABLED"); ok {
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			errs = append(errs, fmt.Errorf("PANORAMA_ATTRIBUTION_ENABLED: %q is not a boolean (want true or false): %w", v, perr))
		} else {
			cfg.Attribution.Enabled = &b
		}
	}
	str("PANORAMA_ATTRIBUTION_TICKET_PATTERN", &cfg.Attribution.TicketPattern)
	num("PANORAMA_ATTRIBUTION_SESSION_CACHE_SIZE", &cfg.Attribution.SessionCacheSize)

	str("PANORAMA_ENCRYPTION_KEY", &cfg.EncryptionKey)

	str("PANORAMA_DEV_SIGN_IN_AS", &cfg.Dev.SignInAs)

	dur("PANORAMA_INTEGRATIONS_SYNC_INTERVAL", &cfg.Integrations.SyncInterval)
	dur("PANORAMA_INTEGRATIONS_STALE_AFTER", &cfg.Integrations.StaleAfter)

	str("PANORAMA_OIDC_ISSUER", &cfg.OIDC.Issuer)
	str("PANORAMA_OIDC_CLIENT_ID", &cfg.OIDC.ClientID)
	str("PANORAMA_OIDC_CLIENT_SECRET", &cfg.OIDC.ClientSecret)
	str("PANORAMA_OIDC_CLIENT_SECRET_FILE", &cfg.OIDC.ClientSecretFile)
	str("PANORAMA_OIDC_GROUPS_CLAIM", &cfg.OIDC.GroupsClaim)
	list("PANORAMA_OIDC_SCOPES", &cfg.OIDC.Scopes)
	list("PANORAMA_OIDC_ALLOWED_DOMAINS", &cfg.OIDC.AllowedDomains)

	return errors.Join(errs...)
}

// rateFieldOrder is the order rates appear in the environment-variable form,
// and the order they are documented in.
var rateFieldOrder = []string{"input", "output", "cache_read", "cache_write", "cache_write_1h"}

// parseModelRates reads the environment form of a price override:
//
//	model=input/output/cache_read/cache_write/cache_write_1h,model=...
//
// A long-context tier follows the base rates after an @, with the prompt size
// it applies above:
//
//	model=0.22/1.32/0.022/0.275/@272000=0.44/1.98/0.044/0.55/
//
// The nested YAML form is easier to read and is what documentation shows; this
// exists so that every configuration key has an environment equivalent, which
// is what makes a container deployable without a config file.
// parseContextWindows reads model=tokens pairs, comma separated:
//
//	anthropic.claude-sonnet-5=1000000,anthropic.claude-haiku-4-5=200000
//
// A token count of zero is legal and means "unrecorded": it is how an operator
// says this build should stop claiming to know a window it has wrong, rather
// than having to invent a replacement number.
func parseContextWindows(v string) (map[string]int, error) {
	out := make(map[string]int)

	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		model, tokens, found := strings.Cut(entry, "=")
		model = strings.TrimSpace(model)
		if !found || model == "" {
			return nil, fmt.Errorf("PANORAMA_BEDROCK_CONTEXT_WINDOWS: %q is not model=tokens", entry)
		}

		n, err := strconv.Atoi(strings.TrimSpace(tokens))
		if err != nil {
			return nil, fmt.Errorf("PANORAMA_BEDROCK_CONTEXT_WINDOWS: %s has a token count that is not a number: %q",
				model, strings.TrimSpace(tokens))
		}
		if n < 0 {
			return nil, fmt.Errorf("PANORAMA_BEDROCK_CONTEXT_WINDOWS: %s has a negative token count", model)
		}

		out[strings.ToLower(model)] = n
	}

	return out, nil
}

func parseModelRates(v string) (map[string]ModelRates, error) {
	out := make(map[string]ModelRates)

	for _, entry := range strings.Split(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		model, rates, found := strings.Cut(entry, "=")
		model = strings.TrimSpace(model)
		if !found || model == "" {
			return nil, fmt.Errorf("PANORAMA_PRICING_MODELS: %q is not model=rates", entry)
		}

		segments := strings.Split(rates, "@")
		base, err := splitRates(model, segments[0])
		if err != nil {
			return nil, err
		}
		m := ModelRates{
			Input: base[0], Output: base[1], CacheRead: base[2], CacheWrite: base[3], CacheWrite1h: base[4],
		}
		for _, segment := range segments[1:] {
			above, tierRates, found := strings.Cut(segment, "=")
			if !found {
				return nil, fmt.Errorf("PANORAMA_PRICING_MODELS: %s has a tier %q that is not tokens=rates", model, segment)
			}
			tokens, err := strconv.ParseInt(strings.TrimSpace(above), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("PANORAMA_PRICING_MODELS: %s has a tier threshold that is not a number: %q",
					model, strings.TrimSpace(above))
			}
			r, err := splitRates(model, tierRates)
			if err != nil {
				return nil, err
			}
			m.Tiers = append(m.Tiers, ModelTier{
				AboveInputTokens: tokens,
				Input:            r[0], Output: r[1], CacheRead: r[2], CacheWrite: r[3], CacheWrite1h: r[4],
			})
		}
		out[model] = m
	}
	return out, nil
}

// splitRates reads one slash-separated set of rates in rateFieldOrder.
func splitRates(model, rates string) ([]Decimal, error) {
	fields := strings.Split(rates, "/")
	if len(fields) != len(rateFieldOrder) {
		return nil, fmt.Errorf("PANORAMA_PRICING_MODELS: %s has %d rates, want %d (%s)",
			model, len(fields), len(rateFieldOrder), strings.Join(rateFieldOrder, "/"))
	}
	out := make([]Decimal, len(fields))
	for i, f := range fields {
		out[i] = Decimal(strings.TrimSpace(f))
	}
	return out, nil
}

func lookup(env getenv, key string) (string, bool) {
	v := env(key)
	if v == "" {
		return "", false
	}
	return v, true
}

// resolveSecrets reads any secret indirections into the configuration. It runs
// before validation so that validation sees the resolved value.
func (c *Config) resolveSecrets() error {
	secret, err := readSecretFile("oidc", "client_secret", c.OIDC.ClientSecret, c.OIDC.ClientSecretFile)
	if err != nil {
		return err
	}
	c.OIDC.ClientSecret = secret

	return c.Database.resolve()
}

// readSecretFile resolves a value/file pair into one value. Setting both is an
// error rather than a precedence rule: a deployment carrying two answers to
// where a credential comes from has one that is being silently ignored, and
// which one is not something an operator should have to know.
func readSecretFile(section, field, value, path string) (string, error) {
	if path == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("%s: %s and %s_file are both set; use one", section, field, field)
	}

	b, err := os.ReadFile(path) //nolint:gosec // the path is operator-supplied by design
	if err != nil {
		return "", fmt.Errorf("%s: read %s_file %s: %w", section, field, path, err)
	}
	// Trimmed because a file written by `echo` carries a trailing newline, and
	// a credential with one appended fails at the far end with an error that
	// says nothing about whitespace.
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s: %s_file %s is empty", section, field, path)
	}
	return v, nil
}

// resolve reads the password indirection and, in the discrete form, assembles
// the DSN. It runs before validation so that validation sees a resolved DSN and
// need not know which of the two forms produced it.
func (d *Database) resolve() error {
	password, err := readSecretFile("database", "password", d.Password, d.PasswordFile)
	if err != nil {
		return err
	}
	d.Password = password

	if d.Host == "" {
		return nil
	}

	if d.Driver != DriverPostgres {
		return fmt.Errorf("database: host is set but driver is %q; the discrete form is Postgres only (set driver: %s)", d.Driver, DriverPostgres)
	}
	if d.DSN != DefaultSQLiteDSN {
		return errors.New("database: dsn and host are both set; use one")
	}

	dsn, err := d.postgresDSN()
	if err != nil {
		return err
	}
	d.DSN = dsn
	return nil
}

// postgresDSN builds the connection URL from the discrete fields.
//
// net/url does the escaping rather than a format string, because a generated
// password is exactly where that matters: RDS mints punctuation, and a `/` or
// an `@` written raw into a URL silently reparses into a different host and
// database. url.UserPassword percent-encodes both halves of the userinfo.
func (d *Database) postgresDSN() (string, error) {
	var errs []error

	if d.User == "" {
		errs = append(errs, errors.New("database.user: required when database.host is set"))
	}
	if d.Password == "" {
		errs = append(errs, errors.New("database.password: required when database.host is set (set PANORAMA_DATABASE_PASSWORD or database.password_file)"))
	}
	if d.Name == "" {
		errs = append(errs, errors.New("database.name: required when database.host is set"))
	}
	if d.Port < 1 || d.Port > 65535 {
		errs = append(errs, fmt.Errorf("database.port: %d is not a port (want 1-65535)", d.Port))
	}
	if !sslModes[d.SSLMode] {
		errs = append(errs, fmt.Errorf("database.sslmode: %q is not supported (want %s)", d.SSLMode, strings.Join(sortedSSLModes(), ", ")))
	}
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(d.User, d.Password),
		Host:     net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path:     "/" + d.Name,
		RawQuery: url.Values{"sslmode": {d.SSLMode}}.Encode(),
	}
	return u.String(), nil
}

func sortedSSLModes() []string {
	modes := make([]string, 0, len(sslModes))
	for m := range sslModes {
		modes = append(modes, m)
	}
	sort.Strings(modes)
	return modes
}

// Validate reports every problem with the configuration at once, so an operator
// fixing a deployment sees the whole list rather than one error per restart.
func (c *Config) Validate() error {
	var errs []error

	errs = append(errs, c.Server.validate()...)
	errs = append(errs, c.Database.validate()...)
	errs = append(errs, c.Log.validate()...)
	errs = append(errs, c.Bedrock.validate()...)
	if c.Providers.DiscoveryInterval > 0 && c.Providers.DiscoveryInterval < time.Minute {
		errs = append(errs, fmt.Errorf("providers.discovery_interval: %s is too frequent; "+
			"a catalogue changes when AWS ships a model, so use an hour or more", c.Providers.DiscoveryInterval))
	}
	errs = append(errs, c.Pricing.validate()...)
	errs = append(errs, c.Policy.validate()...)
	errs = append(errs, c.Auth.validate()...)

	// Checked here so a malformed key is a startup error naming the key, rather
	// than a cipher error the first time an administrator saves a credential.
	if c.EncryptionKey != "" {
		if _, _, err := secrets.DeriveKey(c.EncryptionKey); err != nil {
			errs = append(errs, fmt.Errorf("encryption_key: %w", err))
		}
	}
	errs = append(errs, c.Attribution.validate()...)
	errs = append(errs, c.Integrations.validate()...)
	errs = append(errs, c.OIDC.validate()...)
	// Given the server, because what makes the development sign-in safe is not
	// a property of the field: it is whether anyone other than this machine can
	// reach the door it opens.
	errs = append(errs, c.Dev.validate(c.Server)...)

	return errors.Join(errs...)
}

// validate refuses a development sign-in that anyone but this machine could
// use.
//
// Loopback is the whole test, and it is on the base URL rather than the listen
// address on purpose: the address says what socket is open, and in a container
// that is always 0.0.0.0. The base URL is what this deployment tells the world
// it is, which is the thing an operator would have had to change in order to
// put it in front of anybody.
//
// A startup error rather than a runtime refusal, because the failure this
// guards against is silent: an environment variable copied from a development
// compose file into a production one produces a deployment that works perfectly
// and has an unauthenticated administrator door on it.
func (d *Dev) validate(server Server) []error {
	if !d.Enabled() {
		return nil
	}

	var errs []error

	email := strings.TrimSpace(d.SignInAs)
	if !strings.Contains(email, "@") || strings.ContainsAny(email, " \t") {
		errs = append(errs, fmt.Errorf("dev.sign_in_as: %q is not an email address", email))
	}

	host := server.BaseURL
	if u, err := url.Parse(server.BaseURL); err == nil {
		host = u.Hostname()
	}
	if !isLoopback(host) {
		errs = append(errs, fmt.Errorf(
			"dev.sign_in_as: refused - it opens an administrator session with no identity provider, "+
				"and server.base_url is %q, which is not this machine. "+
				"It is for a development stack on a laptop and nothing else; unset PANORAMA_DEV_SIGN_IN_AS",
			server.BaseURL))
	}

	return errs
}

// isLoopback reports whether a host names only the machine Panorama is running
// on. Names rather than a DNS lookup: resolving would let a hostname that
// happens to answer 127.0.0.1 today open the door, and what is being asked is
// whether the operator wrote something that could ever have meant anywhere else.
func isLoopback(host string) bool {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func (s *Server) validate() []error {
	var errs []error

	if s.Addr == "" {
		errs = append(errs, errors.New("server.addr: required (e.g. :8088)"))
	} else if _, _, err := net.SplitHostPort(s.Addr); err != nil {
		errs = append(errs, fmt.Errorf("server.addr: %q is not host:port: %w", s.Addr, err))
	}

	switch {
	case s.BaseURL == "":
		errs = append(errs, errors.New("server.base_url: required - clients and OIDC redirects are built from it"))
	case !strings.HasPrefix(s.BaseURL, "http://") && !strings.HasPrefix(s.BaseURL, "https://"):
		errs = append(errs, fmt.Errorf("server.base_url: %q must start with http:// or https://", s.BaseURL))
	case strings.HasSuffix(s.BaseURL, "/"):
		errs = append(errs, fmt.Errorf("server.base_url: %q must not end with a slash", s.BaseURL))
	}

	if s.ReadHeaderTimeout <= 0 {
		errs = append(errs, errors.New("server.read_header_timeout: must be positive"))
	}
	if s.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("server.shutdown_timeout: must be positive"))
	}
	if s.MaxRequestBytes <= 0 {
		errs = append(errs, errors.New("server.max_request_bytes: must be positive"))
	}

	return errs
}

// Enabled reports whether the configuration seeds a bedrock provider.
//
// A region, and nothing else. Which models this deployment permits is not
// configuration at all - it is a set of routes an administrator curates from
// the console into the database - so a region is the whole of what has to be
// true before the provider can be built, the catalogue discovered, and the
// setup step's live invocation attempted.
func (b *Bedrock) Enabled() bool { return b.Region != "" }

func (b *Bedrock) validate() []error {
	// Nothing left to validate that the type system does not already: a region
	// is a string this deployment either has or has not, and whether it names a
	// real Bedrock region is answered by the provider's live check rather than
	// by a list compiled in here that AWS would outgrow.
	return nil
}

// SeedSettings is the bedrock provider row these keys create on a first start.
func (b *Bedrock) SeedSettings() providers.Settings {
	s := providers.Settings{
		Kind:         providers.KindBedrock,
		Enabled:      true,
		Region:       b.Region,
		ConfiguredBy: "configuration file",
	}
	if b.UnsupportedBetas != nil {
		betas := append([]string{}, b.UnsupportedBetas...)
		s.Options.UnsupportedBetas = &betas
	}
	if len(b.ContextWindows) > 0 {
		s.Options.ContextWindows = b.ContextWindows
	}
	return s
}

// Card converts one model's configured rates, tiers included, into the
// pricing package's form.
func (m ModelRates) Card() (pricing.RateCard, error) {
	base, err := decimalRates(m.Input, m.Output, m.CacheRead, m.CacheWrite, m.CacheWrite1h)
	if err != nil {
		return pricing.RateCard{}, err
	}
	card := pricing.RateCard{Rates: base}
	var errs []error
	for _, t := range m.Tiers {
		rates, terr := decimalRates(t.Input, t.Output, t.CacheRead, t.CacheWrite, t.CacheWrite1h)
		if terr != nil {
			errs = append(errs, fmt.Errorf("tier above %d tokens: %w", t.AboveInputTokens, terr))
			continue
		}
		card.Tiers = append(card.Tiers, pricing.Tier{AboveInputTokens: t.AboveInputTokens, Rates: rates})
	}
	return card, errors.Join(errs...)
}

// decimalRates parses one set of configured rates.
func decimalRates(input, output, cacheRead, cacheWrite, cacheWrite1h Decimal) (pricing.Rates, error) {
	var (
		out  pricing.Rates
		errs []error
	)

	for _, field := range []struct {
		name  string
		value Decimal
		dst   *pricing.Rate
	}{
		{"input", input, &out.Input},
		{"output", output, &out.Output},
		{"cache_read", cacheRead, &out.CacheRead},
		{"cache_write", cacheWrite, &out.CacheWrite},
		{"cache_write_1h", cacheWrite1h, &out.CacheWrite1h},
	} {
		if !field.value.Set() {
			continue
		}
		rate, err := pricing.ParseUSDPerMillion(string(field.value))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field.name, err))
			continue
		}
		if rate < 0 {
			errs = append(errs, fmt.Errorf("%s: %q is negative", field.name, field.value))
			continue
		}
		*field.dst = rate
	}

	return out, errors.Join(errs...)
}

func (p *Pricing) validate() []error {
	var errs []error

	if p.RefreshInterval < 0 {
		errs = append(errs, errors.New("pricing.refresh_interval: must not be negative (0 disables the refresh)"))
	} else if p.RefreshInterval > 0 && p.RefreshInterval < time.Minute {
		// A tight loop against the AWS Price List API achieves nothing -
		// published rates change on the order of months - and looks like a
		// runaway process to whoever is reading the account's API metrics.
		errs = append(errs, fmt.Errorf("pricing.refresh_interval: %s is too frequent; published rates change rarely, use an hour or more", p.RefreshInterval))
	}

	for model, rates := range p.Models {
		if model == "" {
			errs = append(errs, errors.New("pricing.models: a model name is empty"))
			continue
		}
		// A rate is keyed by a provider reference, and a key that names no
		// provider is refused rather than guessed onto bedrock.
		//
		// This has to be an error and cannot be left alone: the lookup is by
		// the full reference, so a bare id parses, validates, and then matches
		// nothing for the life of the deployment - the override silently does
		// not apply, and the only symptom is a bill computed from the
		// published rate somebody thought they had replaced.
		if kind, _, found := strings.Cut(model, "/"); !found || !providers.Kind(kind).Valid() {
			errs = append(errs, fmt.Errorf(
				"pricing.models[%q]: name the provider, as %q - a rate that does not say which provider it "+
					"prices matches no model", model, string(providers.KindBedrock)+"/"+model))
			continue
		}
		card, err := rates.Card()
		if err != nil {
			errs = append(errs, fmt.Errorf("pricing.models[%q]: %w", model, err))
			continue
		}
		// A configured rate replaces the published one outright, so a partial
		// override would leave the missing classes at zero and every request
		// using them unpriced. Better to say so now than to have someone find
		// it in a month of empty cost columns.
		if !rates.Input.Set() || !rates.Output.Set() {
			errs = append(errs, fmt.Errorf(
				"pricing.models[%q]: input and output are required - a configured rate replaces the published one entirely", model))
		}
		// A tier replaces every rate for the requests it covers, so it needs
		// the same two a base price does; and the thresholds have to say
		// unambiguously which tier a request falls in.
		for _, t := range rates.Tiers {
			if !t.Input.Set() || !t.Output.Set() {
				errs = append(errs, fmt.Errorf(
					"pricing.models[%q]: the tier above %d tokens needs input and output - a tier replaces every rate",
					model, t.AboveInputTokens))
			}
		}
		if err := pricing.ValidateCard(card); err != nil {
			errs = append(errs, fmt.Errorf("pricing.models[%q]: %w", model, err))
		}
	}

	return errs
}

func (p *Policy) validate() []error {
	if p.ReloadInterval < time.Second {
		return []error{fmt.Errorf(
			"policy.reload_interval: %s is too frequent; a second is already far shorter than any budget window",
			p.ReloadInterval)}
	}
	return nil
}

func (a *Auth) validate() []error {
	var errs []error

	// Zero is "never expires" for the two that may be unbounded, so only a
	// negative is wrong there.
	if a.GrantTTL < 0 {
		errs = append(errs, fmt.Errorf("auth.grant_ttl: %s is negative", a.GrantTTL))
	}
	if a.MaxTokenTTL < 0 {
		errs = append(errs, fmt.Errorf("auth.max_token_ttl: %s is negative", a.MaxTokenTTL))
	}

	// These two are not allowed to be zero. A token that never expires is not
	// a short-lived token, and the whole shape of the login flow depends on it
	// being one; a device code that never expires is a pending approval that
	// waits forever.
	if a.TokenTTL < time.Minute {
		errs = append(errs, fmt.Errorf(
			"auth.token_ttl: %s is shorter than a minute, which would expire a token mid-conversation",
			a.TokenTTL))
	}
	if a.DeviceCodeTTL < time.Minute {
		errs = append(errs, fmt.Errorf(
			"auth.device_code_ttl: %s is not long enough for somebody to reach a browser",
			a.DeviceCodeTTL))
	}
	if a.DevicePollInterval < time.Second {
		errs = append(errs, fmt.Errorf(
			"auth.device_poll_interval: %s would have a waiting CLI poll more than once a second",
			a.DevicePollInterval))
	}

	// A grant that expires sooner than the tokens it mints would hand out a
	// credential outliving the authority to have issued it.
	if a.GrantTTL > 0 && a.GrantTTL < a.TokenTTL {
		errs = append(errs, fmt.Errorf(
			"auth.grant_ttl (%s) is shorter than auth.token_ttl (%s): a token would outlive the grant that minted it",
			a.GrantTTL, a.TokenTTL))
	}

	return errs
}

func (a *Attribution) validate() []error {
	var errs []error

	if a.TicketPattern != "" {
		// Compiled at startup rather than at the first request. A pattern that
		// does not compile would otherwise fail every extraction silently, and
		// the operator's first sign of it would be a dashboard of unattributed
		// spend.
		if _, err := regexp.Compile(a.TicketPattern); err != nil {
			errs = append(errs, fmt.Errorf("attribution.ticket_pattern: %q is not a valid regular expression: %w",
				a.TicketPattern, err))
		}
	}
	if a.SessionCacheSize < 0 {
		errs = append(errs, errors.New("attribution.session_cache_size: must not be negative (0 uses the default)"))
	}

	return errs
}

func (d *Database) validate() []error {
	var errs []error

	switch d.Driver {
	case DriverSQLite, DriverPostgres:
	case "":
		errs = append(errs, fmt.Errorf("database.driver: required (%s or %s)", DriverSQLite, DriverPostgres))
	default:
		errs = append(errs, fmt.Errorf("database.driver: %q is not supported (want %s or %s)", d.Driver, DriverSQLite, DriverPostgres))
	}

	if d.DSN == "" {
		errs = append(errs, errors.New("database.dsn: required"))
	}
	if d.MaxOpenConns < 1 {
		errs = append(errs, fmt.Errorf("database.max_open_conns: %d must be at least 1", d.MaxOpenConns))
	}

	return errs
}

func (l *Log) validate() []error {
	var errs []error

	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level: %q is not one of debug, info, warn, error", l.Level))
	}

	switch l.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log.format: %q is not one of json, text", l.Format))
	}

	return errs
}

// Configured reports whether single sign-on has been set up.
func (o *OIDC) Configured() bool {
	return o.Issuer != ""
}

// SharedIssuer reports whether an issuer signs in accounts from outside the
// organization deploying Panorama, so that a verified token proves somebody is
// who they say and nothing about whether they belong here.
//
// Every other provider Panorama documents has an issuer that names a tenant -
// an Entra tenant id, an Okta org, a Keycloak realm, a Cognito pool - and the
// tenant is what keeps strangers out. Google has one issuer for every Google
// account there is. It matters more here than in most products, because the
// first account to sign in owns the deployment.
//
// One function, asked by validation and by the configuration the console
// renders, because a rendered configuration that validation then refuses is a
// screen handing out something that does not boot.
func SharedIssuer(issuer string) bool {
	u, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "accounts.google.com")
}

// AllowsDomain reports whether an account in this domain may sign in. No list
// allows every domain; with one, the match is exact and ignores case, and a
// subdomain is a different domain.
func (o *OIDC) AllowsDomain(domain string) bool {
	if len(o.AllowedDomains) == 0 {
		return true
	}
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return false
	}
	return slices.ContainsFunc(o.AllowedDomains, func(allowed string) bool {
		return strings.EqualFold(strings.TrimSpace(allowed), domain)
	})
}

// validDomain is deliberately narrow: a bare host name with a dot in it. An
// address, a URL, a wildcard or a leading @ are each somebody writing what they
// meant in a form this list does not match against, and a list entry that can
// never match is a restriction that silently admits nobody - or, written as
// "*", one its author believes admits everybody.
func validDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" || !strings.Contains(d, ".") || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return false
	}
	for _, r := range d {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func (o *OIDC) validate() []error {
	// OIDC is optional. Partially configured, though, is worse than absent: it
	// looks like SSO is on when no one can actually sign in.
	if !o.Configured() {
		if o.ClientID != "" || o.ClientSecret != "" {
			return []error{errors.New("oidc: client credentials are set but oidc.issuer is not; set the issuer or remove them")}
		}
		return nil
	}

	var errs []error

	if !strings.HasPrefix(o.Issuer, "https://") {
		// http:// issuers would send tokens in the clear. Refuse rather than warn.
		errs = append(errs, fmt.Errorf("oidc.issuer: %q must use https", o.Issuer))
	}
	if o.ClientID == "" {
		errs = append(errs, errors.New("oidc.client_id: required when oidc.issuer is set"))
	}
	if o.ClientSecret == "" {
		errs = append(errs, errors.New("oidc.client_secret: required when oidc.issuer is set (set PANORAMA_OIDC_CLIENT_SECRET or oidc.client_secret_file)"))
	}
	if o.GroupsClaim == "" {
		errs = append(errs, errors.New("oidc.groups_claim: required when oidc.issuer is set - role mapping depends on it"))
	}
	for _, d := range o.AllowedDomains {
		if !validDomain(d) {
			errs = append(errs, fmt.Errorf("oidc.allowed_domains: %q is not a domain - write it bare, like example.com, "+
				"with no @, no scheme and no wildcard", d))
		}
	}
	if SharedIssuer(o.Issuer) && len(o.AllowedDomains) == 0 {
		// Refused rather than warned about, for the reason the development
		// sign-in is: the failure is an open door that nothing on a running
		// system would ever complain about. Google's consent screen set to
		// Internal closes it too, and Panorama cannot see that setting.
		errs = append(errs, errors.New("oidc.allowed_domains: required when the issuer is Google "+
			"(set PANORAMA_OIDC_ALLOWED_DOMAINS to your Workspace domain) - accounts.google.com signs in every "+
			"Google account there is, and the first account to sign in owns this deployment"))
	}
	if len(o.Scopes) == 0 {
		errs = append(errs, errors.New("oidc.scopes: required when oidc.issuer is set"))
	} else {
		var hasOpenID bool
		for _, s := range o.Scopes {
			if s == "openid" {
				hasOpenID = true
				break
			}
		}
		if !hasOpenID {
			errs = append(errs, errors.New("oidc.scopes: must include openid"))
		}
	}

	return errs
}

// Redacted returns a copy safe to log or serve from a diagnostics endpoint.
// Secrets are replaced rather than blanked, so the reader can tell "set" from
// "unset" without learning the value. Database DSNs commonly embed a password,
// so the DSN is reduced to its driver-visible shape.
func (c Config) Redacted() Config {
	out := c

	if out.OIDC.ClientSecret != "" {
		out.OIDC.ClientSecret = "[redacted]"
	}
	if out.Database.Password != "" {
		out.Database.Password = "[redacted]"
	}
	out.Database.DSN = redactDSN(out.Database.DSN)

	return out
}

// redactDSN strips credentials and query parameters from a data source name,
// keeping enough to identify which database is in use.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}

	// URL-shaped DSNs: postgres://user:password@host:5432/db?sslmode=require
	if i := strings.Index(dsn, "://"); i >= 0 {
		scheme, rest := dsn[:i+3], dsn[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = "[redacted]@" + rest[at+1:]
		}
		if q := strings.Index(rest, "?"); q >= 0 {
			rest = rest[:q]
		}
		return scheme + rest
	}

	// Key-value Postgres DSNs: host=... user=... password=...
	if strings.Contains(dsn, "=") && strings.Contains(dsn, " ") {
		fields := strings.Fields(dsn)
		for i, f := range fields {
			if k, _, ok := strings.Cut(f, "="); ok && strings.EqualFold(k, "password") {
				fields[i] = k + "=[redacted]"
			}
		}
		return strings.Join(fields, " ")
	}

	// SQLite file DSNs: keep the file name, drop the pragmas.
	base := dsn
	if q := strings.Index(base, "?"); q >= 0 {
		base = base[:q]
	}
	return filepath.Clean(base)
}
