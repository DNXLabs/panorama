// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default configuration must be valid, got: %v", err)
	}
}

func TestLoadMissingDefaultFileIsNotAnError(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with no config file: %v", err)
	}
	if cfg.Server.Addr != Default().Server.Addr {
		t.Errorf("Addr = %q, want the default %q", cfg.Server.Addr, Default().Server.Addr)
	}
}

func TestLoadMissingExplicitFileIsAnError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("naming a config file that does not exist must fail, got nil error")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("error should name the missing file, got: %v", err)
	}
}

func TestLoadFileOverlaysDefaults(t *testing.T) {
	path := writeConfig(t, `
server:
  addr: "127.0.0.1:9999"
  base_url: "https://panorama.example.com"
database:
  driver: postgres
  dsn: "postgres://panorama:hunter2@db:5432/panorama?sslmode=require"
log:
  level: debug
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Addr != "127.0.0.1:9999" {
		t.Errorf("Addr = %q, want 127.0.0.1:9999", cfg.Server.Addr)
	}
	if cfg.Database.Driver != DriverPostgres {
		t.Errorf("Driver = %q, want postgres", cfg.Database.Driver)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Level = %q, want debug", cfg.Log.Level)
	}
	// Untouched keys keep their defaults.
	if cfg.Log.Format != "json" {
		t.Errorf("Format = %q, want the default json", cfg.Log.Format)
	}
	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v, want the default 30s", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `
server:
  adrr: ":8088"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("a misspelled key must fail rather than be ignored, got nil error")
	}
	if !strings.Contains(err.Error(), "adrr") {
		t.Errorf("error should name the unknown key, got: %v", err)
	}
}

func TestLoadEmptyFileUsesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("an empty config file should mean 'use defaults', got: %v", err)
	}
	if cfg.Server.Addr != Default().Server.Addr {
		t.Errorf("Addr = %q, want the default", cfg.Server.Addr)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	cfg := Default()
	cfg.Server.Addr = ":1111"

	env := mapEnv{
		"PANORAMA_SERVER_ADDR":             ":2222",
		"PANORAMA_DATABASE_MAX_OPEN_CONNS": "5",
		"PANORAMA_DATABASE_AUTO_MIGRATE":   "false",
		"PANORAMA_SERVER_SHUTDOWN_TIMEOUT": "45s",
		"PANORAMA_OIDC_SCOPES":             "openid, email ,groups",
	}
	if err := applyEnv(&cfg, env.get); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}

	if cfg.Server.Addr != ":2222" {
		t.Errorf("Addr = %q, want :2222", cfg.Server.Addr)
	}
	if cfg.Database.MaxOpenConns != 5 {
		t.Errorf("MaxOpenConns = %d, want 5", cfg.Database.MaxOpenConns)
	}
	if cfg.Database.AutoMigrate {
		t.Error("AutoMigrate = true, want false")
	}
	if cfg.Server.ShutdownTimeout != 45*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 45s", cfg.Server.ShutdownTimeout)
	}
	if got, want := strings.Join(cfg.OIDC.Scopes, ","), "openid,email,groups"; got != want {
		t.Errorf("Scopes = %q, want %q (whitespace trimmed)", got, want)
	}
}

func TestEnvUnsetLeavesValueAlone(t *testing.T) {
	cfg := Default()
	cfg.Log.Level = "warn"

	if err := applyEnv(&cfg, mapEnv{}.get); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("Level = %q, want warn - an unset variable must not overwrite", cfg.Log.Level)
	}
}

func TestEnvMalformedValuesAreReported(t *testing.T) {
	cfg := Default()
	env := mapEnv{
		"PANORAMA_DATABASE_MAX_OPEN_CONNS": "lots",
		"PANORAMA_SERVER_SHUTDOWN_TIMEOUT": "soon",
		"PANORAMA_DATABASE_AUTO_MIGRATE":   "yes please",
	}

	err := applyEnv(&cfg, env.get)
	if err == nil {
		t.Fatal("malformed environment values must fail, got nil error")
	}
	for _, want := range []string{"MAX_OPEN_CONNS", "SHUTDOWN_TIMEOUT", "AUTO_MIGRATE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("all problems should be reported at once; %s missing from: %v", want, err)
		}
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg := Config{
		Server:   Server{Addr: "not-a-port", BaseURL: "panorama.example.com"},
		Database: Database{Driver: "mysql", DSN: ""},
		Log:      Log{Level: "chatty", Format: "xml"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	for _, want := range []string{
		"server.addr", "server.base_url", "server.read_header_timeout",
		"database.driver", "database.dsn", "log.level", "log.format",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s not reported in: %v", want, err)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{"https", "https://panorama.internal", false},
		{"http for local dev", "http://localhost:8088", false},
		{"no scheme", "panorama.internal", true},
		{"trailing slash", "https://panorama.internal/", true},
		{"empty", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Server.BaseURL = tt.baseURL

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("base_url %q should be rejected", tt.baseURL)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("base_url %q should be accepted, got: %v", tt.baseURL, err)
			}
		})
	}
}

func TestOIDCOptionalButNotPartial(t *testing.T) {
	t.Run("absent is fine", func(t *testing.T) {
		cfg := Default()
		if err := cfg.Validate(); err != nil {
			t.Errorf("OIDC is optional, got: %v", err)
		}
	})

	t.Run("credentials without an issuer are rejected", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.ClientID = "panorama"

		if err := cfg.Validate(); err == nil {
			t.Error("a client id with no issuer looks like SSO is on when nobody can sign in")
		}
	})

	t.Run("issuer without credentials is rejected", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.Issuer = "https://idp.example.com"

		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{"oidc.client_id", "oidc.client_secret"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s not reported in: %v", want, err)
			}
		}
	})

	t.Run("plaintext issuer is refused", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.Issuer = "http://idp.example.com"
		cfg.OIDC.ClientID = "panorama"
		cfg.OIDC.ClientSecret = "s3cret"

		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("an http issuer sends tokens in the clear and must be refused, got: %v", err)
		}
	})

	t.Run("scopes must include openid", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.Issuer = "https://idp.example.com"
		cfg.OIDC.ClientID = "panorama"
		cfg.OIDC.ClientSecret = "s3cret"
		cfg.OIDC.Scopes = []string{"profile", "email"}

		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "openid") {
			t.Errorf("expected the missing openid scope to be reported, got: %v", err)
		}
	})

	t.Run("fully configured is accepted", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.Issuer = "https://idp.example.com"
		cfg.OIDC.ClientID = "panorama"
		cfg.OIDC.ClientSecret = "s3cret"

		if err := cfg.Validate(); err != nil {
			t.Errorf("a complete OIDC block should validate, got: %v", err)
		}
	})
}

// TestGoogleAsTheIssuerNeedsAnAllowedDomain.
//
// Every other issuer Panorama documents names a tenant, and the tenant is what
// keeps strangers out. accounts.google.com signs in every Google account there
// is, and the first account to sign in owns the deployment - so a Google
// deployment with no domain list is one whose administrator is whoever found
// its address first. Refused at startup rather than warned about, for the same
// reason the development sign-in is: nothing on a running system would ever
// complain.
func TestGoogleAsTheIssuerNeedsAnAllowedDomain(t *testing.T) {
	google := func() Config {
		cfg := Default()
		cfg.OIDC.Issuer = "https://accounts.google.com"
		cfg.OIDC.ClientID = "panorama"
		cfg.OIDC.ClientSecret = "s3cret"
		return cfg
	}

	cfg := google()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "oidc.allowed_domains") {
		t.Fatalf("Google with no allowed domain validated, or failed on something else: %v", err)
	}
	if !strings.Contains(err.Error(), "PANORAMA_OIDC_ALLOWED_DOMAINS") {
		t.Errorf("the refusal does not name the variable that fixes it: %v", err)
	}

	cfg = google()
	cfg.OIDC.AllowedDomains = []string{"example.com"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Google with an allowed domain should validate, got: %v", err)
	}

	// A tenant's own issuer needs no list, and gets no new demand on upgrade.
	cfg = google()
	cfg.OIDC.Issuer = "https://login.microsoftonline.com/0f6e2b7a/v2.0"
	if err := cfg.Validate(); err != nil {
		t.Errorf("an issuer that names a tenant should validate with no list, got: %v", err)
	}

	for issuer, want := range map[string]bool{
		"https://accounts.google.com":      true,
		"https://accounts.google.com/":     true,
		"https://ACCOUNTS.GOOGLE.COM":      true,
		"https://accounts.google.com.evil": false,
		"https://your-org.okta.com":        false,
		"":                                 false,
	} {
		if got := SharedIssuer(issuer); got != want {
			t.Errorf("SharedIssuer(%q) = %v, want %v", issuer, got, want)
		}
	}
}

// TestAnAllowedDomainIsWrittenBare.
//
// An entry that can never match is a restriction that silently admits nobody,
// and "*" is one its author believes admits everybody. Each of these is
// somebody writing what they meant in a form the list does not match against.
func TestAnAllowedDomainIsWrittenBare(t *testing.T) {
	for _, bad := range []string{"@example.com", "*.example.com", "*", "https://example.com", "example", "me@example.com", "example.com/", ""} {
		cfg := Default()
		cfg.OIDC.Issuer = "https://idp.example.com"
		cfg.OIDC.ClientID = "panorama"
		cfg.OIDC.ClientSecret = "s3cret"
		cfg.OIDC.AllowedDomains = []string{bad}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "oidc.allowed_domains") {
			t.Errorf("allowed domain %q validated, or failed on something else: %v", bad, err)
		}
	}

	oidc := OIDC{AllowedDomains: []string{"Example.com ", "example.org"}}
	for domain, want := range map[string]bool{
		"example.com":      true,
		"EXAMPLE.COM":      true,
		"example.org":      true,
		"eu.example.com":   false, // a subdomain is a different domain
		"notexample.com":   false,
		"example.com.evil": false,
		"":                 false,
	} {
		if got := oidc.AllowsDomain(domain); got != want {
			t.Errorf("AllowsDomain(%q) = %v, want %v", domain, got, want)
		}
	}
	if !(&OIDC{}).AllowsDomain("anything.example") {
		t.Error("no list should allow every domain")
	}
}

func TestClientSecretFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "oidc-secret")
	if err := os.WriteFile(secretPath, []byte("  s3cret-from-mount\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("is read and trimmed", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.ClientSecretFile = secretPath

		if err := cfg.resolveSecrets(); err != nil {
			t.Fatalf("resolveSecrets: %v", err)
		}
		if cfg.OIDC.ClientSecret != "s3cret-from-mount" {
			t.Errorf("ClientSecret = %q, want the trimmed file contents", cfg.OIDC.ClientSecret)
		}
	})

	t.Run("conflicts with an inline secret", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.ClientSecretFile = secretPath
		cfg.OIDC.ClientSecret = "inline"

		if err := cfg.resolveSecrets(); err == nil {
			t.Error("setting both the secret and the secret file is ambiguous and must fail")
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		cfg := Default()
		cfg.OIDC.ClientSecretFile = filepath.Join(dir, "absent")

		if err := cfg.resolveSecrets(); err == nil {
			t.Error("a named secret file that does not exist must fail")
		}
	})

	t.Run("empty file is an error", func(t *testing.T) {
		empty := filepath.Join(dir, "empty")
		if err := os.WriteFile(empty, []byte("\n \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := Default()
		cfg.OIDC.ClientSecretFile = empty

		if err := cfg.resolveSecrets(); err == nil {
			t.Error("an empty secret file must fail rather than authenticate with nothing")
		}
	})
}

func TestRedacted(t *testing.T) {
	cfg := Default()
	cfg.OIDC.ClientSecret = "super-secret"
	cfg.Database.DSN = "postgres://panorama:hunter2@db:5432/panorama?sslmode=require"

	got := cfg.Redacted()

	if strings.Contains(got.OIDC.ClientSecret, "super-secret") {
		t.Error("the client secret survived redaction")
	}
	if got.OIDC.ClientSecret == "" {
		t.Error("redaction should still show that a secret is set")
	}
	if strings.Contains(got.Database.DSN, "hunter2") {
		t.Errorf("the DSN password survived redaction: %q", got.Database.DSN)
	}
	if !strings.Contains(got.Database.DSN, "db:5432") {
		t.Errorf("redaction should keep the host so the DSN stays identifiable, got %q", got.Database.DSN)
	}

	// The original must not be mutated.
	if cfg.OIDC.ClientSecret != "super-secret" {
		t.Error("Redacted mutated its receiver")
	}
}

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name      string
		dsn       string
		wantNot   string
		wantHas   string
		wantExact string
	}{
		{
			name:    "url with credentials",
			dsn:     "postgres://user:pw@host:5432/db?sslmode=require",
			wantNot: "pw",
			wantHas: "host:5432/db",
		},
		{
			name:    "url without credentials",
			dsn:     "postgres://host:5432/db",
			wantHas: "host:5432/db",
		},
		{
			name:    "key value dsn",
			dsn:     "host=db port=5432 user=panorama password=hunter2 dbname=panorama",
			wantNot: "hunter2",
			wantHas: "user=panorama",
		},
		{
			name:      "sqlite file with pragmas",
			dsn:       "file:panorama.db?_pragma=journal_mode(WAL)",
			wantExact: "file:panorama.db",
		},
		{
			name:      "empty",
			dsn:       "",
			wantExact: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactDSN(tt.dsn)

			if tt.wantExact != "" || tt.dsn == "" {
				if got != tt.wantExact {
					t.Errorf("redactDSN(%q) = %q, want %q", tt.dsn, got, tt.wantExact)
				}
				return
			}
			if tt.wantNot != "" && strings.Contains(got, tt.wantNot) {
				t.Errorf("redactDSN(%q) = %q, must not contain %q", tt.dsn, got, tt.wantNot)
			}
			if tt.wantHas != "" && !strings.Contains(got, tt.wantHas) {
				t.Errorf("redactDSN(%q) = %q, want it to contain %q", tt.dsn, got, tt.wantHas)
			}
		})
	}
}

// mapEnv is a getenv backed by a map, so tests need not touch the process
// environment.
type mapEnv map[string]string

func (m mapEnv) get(key string) string { return m[key] }

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestExampleConfigLoads guards the file operators actually start from.
//
// Unknown keys are a startup error, so a key added to the struct and forgotten
// in the example is not a documentation gap - it is an example that refuses to
// boot, discovered by whoever copied it. Loading it here is the cheapest way to
// keep the two in step.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if verr := cfg.Validate(); verr != nil {
		t.Fatalf("config.example.yaml does not validate: %v", verr)
	}

	// Spot-check that the example carries the values it documents rather than
	// falling back to defaults because a block was misspelled.
	if cfg.Auth.TokenTTL != time.Hour {
		t.Errorf("auth.token_ttl = %s, want 1h from the example", cfg.Auth.TokenTTL)
	}
	if cfg.Auth.GrantTTL != 720*time.Hour {
		t.Errorf("auth.grant_ttl = %s, want 720h from the example", cfg.Auth.GrantTTL)
	}
}

// The development sign-in is the only thing in Panorama that opens a console
// session without an identity provider, so what these check is not that it
// works - it is that a deployment anyone else could reach refuses to start with
// it set. The failure it guards against is silent: an environment variable
// copied out of the development compose file into a production one produces a
// deployment that works perfectly and has an unauthenticated administrator door
// on it.
func TestDevSignInIsRefusedAnywhereButThisMachine(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		signInAs string
		wantErr  bool
	}{
		{"off by default", "https://panorama.internal", "", false},
		{"localhost", "http://localhost:5173", "dev@localhost", false},
		{"127.0.0.1", "http://127.0.0.1:8088", "dev@localhost", false},
		{"ipv6 loopback", "http://[::1]:8088", "dev@localhost", false},
		{"a real hostname", "https://panorama.internal", "dev@localhost", true},
		{"a hostname that resolves locally today", "http://dev.example.com:5173", "dev@localhost", true},
		{"an address on the LAN", "http://192.168.1.10:8088", "dev@localhost", true},
		{"not an email", "http://localhost:5173", "dev", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Server.BaseURL = tt.baseURL
			cfg.Dev.SignInAs = tt.signInAs

			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Errorf("dev.sign_in_as %q with base_url %q must be refused", tt.signInAs, tt.baseURL)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("dev.sign_in_as %q with base_url %q should be accepted, got: %v",
					tt.signInAs, tt.baseURL, err)
			}
		})
	}
}

func TestDevSignInIsOffUnlessAnAccountIsNamed(t *testing.T) {
	if Default().Dev.Enabled() {
		t.Error("the development sign-in must be off in the default configuration")
	}
	if (Dev{SignInAs: "   "}).Enabled() {
		t.Error("whitespace is not an account; the development sign-in must stay off")
	}
	if !(Dev{SignInAs: "dev@localhost"}).Enabled() {
		t.Error("a named account should enable the development sign-in")
	}
}

// The discrete form exists so that a password can reach the process as a
// secret reference rather than as part of a string somebody had to assemble.
// What these assert is the assembly, and the refusals around it.
func TestDiscreteDatabaseFields(t *testing.T) {
	postgres := func() Config {
		cfg := Default()
		cfg.Database.Driver = DriverPostgres
		cfg.Database.Host = "panorama.abc123.us-east-1.rds.amazonaws.com"
		cfg.Database.User = "panorama"
		cfg.Database.Password = "hunter2"
		return cfg
	}

	t.Run("assembles a DSN", func(t *testing.T) {
		cfg := postgres()

		if err := cfg.resolveSecrets(); err != nil {
			t.Fatalf("resolveSecrets: %v", err)
		}
		want := "postgres://panorama:hunter2@panorama.abc123.us-east-1.rds.amazonaws.com:5432/panorama?sslmode=require"
		if cfg.Database.DSN != want {
			t.Errorf("DSN = %q, want %q", cfg.Database.DSN, want)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("the assembled configuration must be valid, got: %v", err)
		}
	})

	// RDS mints punctuation, and a `/` or an `@` written raw into a URL
	// reparses into a different host and database without failing. This is the
	// case that made net/url the builder rather than a format string.
	t.Run("escapes a password with URL syntax in it", func(t *testing.T) {
		cfg := postgres()
		cfg.Database.Password = "p@ss/w:rd?#[]"

		if err := cfg.resolveSecrets(); err != nil {
			t.Fatalf("resolveSecrets: %v", err)
		}
		u, err := url.Parse(cfg.Database.DSN)
		if err != nil {
			t.Fatalf("the assembled DSN must parse as a URL, got %q: %v", cfg.Database.DSN, err)
		}
		if got := u.Hostname(); got != "panorama.abc123.us-east-1.rds.amazonaws.com" {
			t.Errorf("host = %q, want the configured host; the password has leaked into it", got)
		}
		if got := u.Path; got != "/panorama" {
			t.Errorf("path = %q, want /panorama", got)
		}
		pw, _ := u.User.Password()
		if pw != "p@ss/w:rd?#[]" {
			t.Errorf("password round-tripped as %q, want the original", pw)
		}
	})

	t.Run("reads the password from a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "db-password")
		if err := os.WriteFile(path, []byte("from-a-mount\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := postgres()
		cfg.Database.Password = ""
		cfg.Database.PasswordFile = path

		if err := cfg.resolveSecrets(); err != nil {
			t.Fatalf("resolveSecrets: %v", err)
		}
		if !strings.Contains(cfg.Database.DSN, "from-a-mount") {
			t.Errorf("DSN = %q, want the trimmed file contents in it", cfg.Database.DSN)
		}
	})

	t.Run("refuses a DSN and a host together", func(t *testing.T) {
		cfg := postgres()
		cfg.Database.DSN = "postgres://elsewhere:5432/panorama"

		err := cfg.resolveSecrets()
		if err == nil {
			t.Fatal("two answers to where the database is must fail rather than pick one")
		}
		if !strings.Contains(err.Error(), "both set") {
			t.Errorf("error should say both are set, got: %v", err)
		}
	})

	t.Run("refuses the discrete form on SQLite", func(t *testing.T) {
		cfg := postgres()
		cfg.Database.Driver = DriverSQLite

		if err := cfg.resolveSecrets(); err == nil {
			t.Error("a host with the SQLite driver is a contradiction and must fail")
		}
	})

	t.Run("reports every missing field at once", func(t *testing.T) {
		cfg := Default()
		cfg.Database.Driver = DriverPostgres
		cfg.Database.Host = "db"
		cfg.Database.Name = ""
		cfg.Database.Port = 0
		cfg.Database.SSLMode = "sort-of"

		err := cfg.resolveSecrets()
		if err == nil {
			t.Fatal("an incomplete discrete form must fail")
		}
		for _, want := range []string{"database.user", "database.password", "database.name", "database.port", "database.sslmode"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name %s, got: %v", want, err)
			}
		}
	})

	t.Run("leaves SQLite alone", func(t *testing.T) {
		cfg := Default()

		if err := cfg.resolveSecrets(); err != nil {
			t.Fatalf("resolveSecrets: %v", err)
		}
		if cfg.Database.DSN != DefaultSQLiteDSN {
			t.Errorf("DSN = %q, want the SQLite default untouched", cfg.Database.DSN)
		}
	})

	// The whole point of the form is that the password arrives from Secrets
	// Manager as an environment variable, so this is the path ECS actually
	// takes.
	t.Run("arrives from the environment", func(t *testing.T) {
		t.Chdir(t.TempDir())
		env := map[string]string{
			"PANORAMA_DATABASE_DRIVER":   "postgres",
			"PANORAMA_DATABASE_HOST":     "db.internal",
			"PANORAMA_DATABASE_PORT":     "5433",
			"PANORAMA_DATABASE_NAME":     "panorama",
			"PANORAMA_DATABASE_USER":     "panorama",
			"PANORAMA_DATABASE_PASSWORD": "from-secrets-manager",
		}
		for k, v := range env {
			t.Setenv(k, v)
		}

		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := "postgres://panorama:from-secrets-manager@db.internal:5433/panorama?sslmode=require"
		if cfg.Database.DSN != want {
			t.Errorf("DSN = %q, want %q", cfg.Database.DSN, want)
		}
	})

	// A password that reached the process as a field is a password that can be
	// logged with the configuration, which is what Redacted exists to stop.
	t.Run("the password is redacted", func(t *testing.T) {
		cfg := postgres()
		if err := cfg.resolveSecrets(); err != nil {
			t.Fatalf("resolveSecrets: %v", err)
		}

		got := cfg.Redacted()
		if strings.Contains(got.Database.Password, "hunter2") {
			t.Errorf("Password = %q, want it redacted", got.Database.Password)
		}
		if strings.Contains(got.Database.DSN, "hunter2") {
			t.Errorf("DSN = %q, want the credentials redacted", got.Database.DSN)
		}
	})
}

// A configured rate is keyed by a provider reference, and a bare model id is
// refused rather than guessed onto bedrock.
//
// It has to be an error: the lookup is by the full reference, so a bare id
// would parse, validate, and then match nothing for the life of the
// deployment - the override silently not applying, with the only symptom a
// bill computed from the published rate somebody thought they had replaced.
func TestAPricingOverrideMustNameItsProvider(t *testing.T) {
	cfg := Default()
	cfg.Pricing.Models = map[string]ModelRates{
		"us.anthropic.claude-sonnet-5": {Input: "2.20", Output: "11.00"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want an error naming the reference to use")
	}
	if !strings.Contains(err.Error(), "bedrock/us.anthropic.claude-sonnet-5") {
		t.Errorf("error = %q, want it to show the prefixed form", err)
	}

	cfg.Pricing.Models = map[string]ModelRates{
		"bedrock/us.anthropic.claude-sonnet-5": {Input: "2.20", Output: "11.00"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate = %v, want a prefixed reference to be accepted", err)
	}
}

// The bedrock block seeds the provider row a first start creates, carrying its
// region, unsupported betas and context windows.
func TestBedrockSeedsTheProviderRow(t *testing.T) {
	cfg := Default()
	if err := applyEnv(&cfg, mapEnv{
		"PANORAMA_BEDROCK_REGION":            "ap-southeast-2",
		"PANORAMA_BEDROCK_UNSUPPORTED_BETAS": "a,b",
		"PANORAMA_BEDROCK_CONTEXT_WINDOWS":   "anthropic.claude-sonnet-5=1000000",
	}.get); err != nil {
		t.Fatal(err)
	}
	seed := cfg.Bedrock.SeedSettings()
	if seed.Kind != "bedrock" || seed.Region != "ap-southeast-2" || !seed.Enabled {
		t.Errorf("seed = %+v", seed)
	}
	if seed.Options.UnsupportedBetas == nil || len(*seed.Options.UnsupportedBetas) != 2 {
		t.Errorf("unsupported betas did not reach the seed: %+v", seed.Options)
	}
	if seed.Options.ContextWindows["anthropic.claude-sonnet-5"] != 1_000_000 {
		t.Errorf("context windows did not reach the seed: %+v", seed.Options)
	}

	// Unset betas stay unset, which is the provider's built-in list - not an
	// empty list, which would forward every flag.
	unset := Bedrock{Region: "us-east-1"}
	if unset.SeedSettings().Options.UnsupportedBetas != nil {
		t.Error("an unset beta list seeded as an empty one")
	}
}
