// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Client-side commands: what a developer runs on their own machine.
//
// These are the only commands that do not touch a database or read a
// deployment's configuration, because the machine running them has neither.
// They speak to a deployment over HTTP with a credential kept in the user's
// home directory, and they are dispatched before config.Load for exactly that
// reason - a laptop has no config.yaml, and demanding one would make signing in
// the hardest step of onboarding rather than the first.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// credentialsFile is where a signed-in CLI keeps its grant.
//
// One file for every deployment a person uses, keyed by URL. Consultants and
// anyone with a staging environment have more than one, and a single-valued
// file would have them signing in again every time they switched.
const credentialsFile = "credentials.json"

// credentialsMode is 0600 because the file holds a live credential. It is
// checked on read as well as set on write: a grant in a world-readable file is
// a grant that has to be assumed leaked.
const credentialsMode = 0o600

// httpTimeout bounds every call these commands make. A CLI that hangs is worse
// than one that fails, because apiKeyHelper runs inside somebody's editor.
const httpTimeout = 30 * time.Second

// credentials is the whole of the on-disk state.
type credentials struct {
	// Default is the deployment used when -url is not given.
	Default string `json:"default"`
	// Deployments is keyed by the deployment's base URL.
	Deployments map[string]*deployment `json:"deployments"`
}

// deployment is one signed-in Panorama.
type deployment struct {
	Grant          string    `json:"grant"`
	GrantExpiresAt time.Time `json:"grant_expires_at,omitzero"`
	Name           string    `json:"name,omitempty"`

	// The cached gateway token, so that `panorama token` makes no network call
	// on the common path. It is refreshed well before it expires - see
	// RefreshAfter.
	Token          string    `json:"token,omitempty"`
	TokenExpiresAt time.Time `json:"token_expires_at,omitzero"`
	// RefreshAfter is when to exchange for a new token, set to half the
	// token's life.
	//
	// Not "when it expires". The tool holding this token asks for it on its own
	// schedule and then uses whatever it got until the next ask, so handing
	// back a token with two minutes left would have it make requests with an
	// expired credential for the rest of the interval. Refreshing at the
	// halfway point means anything returned is good for at least half a
	// lifetime, whatever the caller's cadence is.
	RefreshAfter time.Time `json:"refresh_after,omitzero"`
}

// clientCommand reports whether a command runs on a developer's machine rather
// than against a deployment's database.
//
// `token` is the awkward one, because it means two things and both are worth
// keeping. `panorama token` prints the credential this machine already holds -
// it is what Claude Code's apiKeyHelper runs, so it has to stay short. `panorama
// token create` mints one for somebody by writing to the database, which is how
// a deployment issues its first credential before anybody can sign in.
//
// They are told apart by the subcommand rather than renamed, because renaming
// either one costs more than the distinction does: shortening the first is
// impossible, and the second is what the README's first thirty minutes uses.
func clientCommand(name string, args []string) bool {
	switch name {
	case "login", "logout", "whoami", "configure":
		return true
	case "token":
		return len(args) == 0 || args[0] != "create"
	default:
		return false
	}
}

func runClient(ctx context.Context, command string, args []string) error {
	switch command {
	case "login":
		return loginCmd(ctx, args)
	case "logout":
		return logoutCmd(args)
	case "token":
		return tokenCmd(ctx, args)
	case "whoami":
		return whoamiCmd(ctx, args)
	case "configure":
		return configureCmd(ctx, args)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// loginCmd runs the device-authorization flow.
func loginCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("panorama login", flag.ContinueOnError)
	url := fs.String("url", "", "the Panorama deployment to sign in to, e.g. https://panorama.example.internal")
	name := fs.String("name", "", "what to call this machine in the console (default: its hostname)")
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds, err := loadCredentials()
	if err != nil {
		return err
	}

	base := strings.TrimRight(strings.TrimSpace(*url), "/")
	if base == "" {
		base = creds.Default
	}
	if base == "" {
		return errors.New("which deployment? pass -url https://panorama.your-company.internal")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}

	machine := strings.TrimSpace(*name)
	if machine == "" {
		machine, _ = os.Hostname()
	}
	if machine == "" {
		machine = "a command line"
	}

	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		// The address with the code already in it. Opened rather than printed:
		// what goes on screen is the plain address and the code separately, so
		// that somebody approving from their phone has something to read.
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := postJSON(ctx, base+"/api/v1/cli/device", "",
		map[string]any{"client_name": machine}, &start); err != nil {
		return fmt.Errorf("starting sign-in at %s: %w", base, err)
	}

	fmt.Fprintf(os.Stderr, "\n  Your code is  %s\n\n  Open %s and enter it.\n\n",
		start.UserCode, start.VerificationURI)

	if !*noBrowser {
		target := start.VerificationURIComplete
		if target == "" {
			target = start.VerificationURI
		}
		openBrowser(ctx, base, target)
	}

	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)

	for {
		if time.Now().After(deadline) {
			return errors.New("that sign-in request expired before it was approved; run panorama login again")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}

		var granted struct {
			Grant     string `json:"grant"`
			Name      string `json:"name"`
			ExpiresAt string `json:"expires_at"`
			Error     string `json:"error"`
			Detail    string `json:"error_description"`
		}
		err := postJSON(ctx, base+"/api/v1/cli/device/grant", "",
			map[string]any{"device_code": start.DeviceCode}, &granted)

		switch {
		case err != nil && granted.Error == "":
			return fmt.Errorf("waiting for approval: %w", err)
		case granted.Error == "authorization_pending":
			continue
		case granted.Error == "slow_down":
			// The server is telling this client its cadence is wrong, so the
			// cadence changes. Ignoring it and retrying at the same rate is how
			// a CLI gets itself rate limited.
			interval += 5 * time.Second
			continue
		case granted.Error != "":
			return errors.New(strings.ToLower(granted.Detail))
		}

		creds.set(base, &deployment{
			Grant:          granted.Grant,
			GrantExpiresAt: parseTime(granted.ExpiresAt),
			Name:           granted.Name,
		})
		if err := saveCredentials(creds); err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "Signed in to %s as %s.\n", base, granted.Name)
		fmt.Fprintf(os.Stderr, "Run `panorama configure claude-code` to point your tools at it.\n")
		return nil
	}
}

// tokenCmd prints a gateway token, refreshing it when it is halfway through its
// life.
//
// This is what Claude Code's apiKeyHelper runs, so its output on success is the
// token and nothing else. Everything explanatory goes to stderr.
//
// It never prints a token it knows to be past its expiry, and it never falls
// back to a cached credential when a refresh fails. A revoked grant has to
// surface as "you were signed out" here, at the one place that can say so -
// falling back would turn a deliberate revocation into an unexplained 401 from
// the gateway an hour later.
func tokenCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("panorama token", flag.ContinueOnError)
	url := fs.String("url", "", "the deployment to get a token for (default: the one you signed in to)")
	quiet := fs.Bool("quiet", false, "print only the token")
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	base, dep, err := creds.resolve(*url)
	if err != nil {
		return err
	}

	now := time.Now()
	if !dep.GrantExpiresAt.IsZero() && !dep.GrantExpiresAt.After(now) {
		return fmt.Errorf("your sign-in to %s expired on %s; run panorama login",
			base, dep.GrantExpiresAt.Format(time.DateOnly))
	}

	if err := ensureToken(ctx, creds, base, dep); err != nil {
		return err
	}

	printToken(dep, *quiet)
	return nil
}

// ensureToken makes sure dep holds a gateway token that is good for a while,
// exchanging the grant for a new one if it is not.
//
// Shared by `token` and `configure`, because a second implementation of "is
// this token still worth using" would eventually answer differently - and the
// halfway-point refresh below is the reason anything this returns is good for
// at least half a token's life whatever the caller's cadence is.
func ensureToken(ctx context.Context, creds *credentials, base string, dep *deployment) error {
	now := time.Now()
	if dep.Token != "" && !dep.RefreshAfter.IsZero() && now.Before(dep.RefreshAfter) {
		return nil
	}

	var exchanged struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := postJSON(ctx, base+"/api/v1/cli/token", dep.Grant, nil, &exchanged); err != nil {
		return fmt.Errorf("refreshing your token from %s: %w", base, err)
	}

	dep.Token = exchanged.Token
	dep.TokenExpiresAt = parseTime(exchanged.ExpiresAt)
	dep.RefreshAfter = now.Add(time.Duration(exchanged.ExpiresIn) * time.Second / 2)
	if err := saveCredentials(creds); err != nil {
		// The token is good even if it could not be cached, so it is still
		// used - but the failure is reported, because a cache that silently
		// never writes means every request mints a token.
		fmt.Fprintf(os.Stderr, "panorama: could not cache the token: %v\n", err)
	}
	return nil
}

func printToken(dep *deployment, quiet bool) {
	fmt.Println(dep.Token)
	if !quiet && !dep.TokenExpiresAt.IsZero() {
		fmt.Fprintf(os.Stderr, "expires %s\n", dep.TokenExpiresAt.Local().Format(time.TimeOnly))
	}
}

// whoamiCmd says who this machine is signed in as.
func whoamiCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("panorama whoami", flag.ContinueOnError)
	url := fs.String("url", "", "the deployment to ask")
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	base, dep, err := creds.resolve(*url)
	if err != nil {
		return err
	}

	// Asked of the deployment rather than answered from the file, because the
	// question is whether this machine is still signed in - and the file cannot
	// know that it was signed out from the console an hour ago.
	var exchanged struct {
		Token string `json:"token"`
	}
	if err := postJSON(ctx, base+"/api/v1/cli/token", dep.Grant, nil, &exchanged); err != nil {
		return fmt.Errorf("checking your sign-in at %s: %w", base, err)
	}

	var me struct {
		User struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
		} `json:"user"`
	}
	if err := getJSON(ctx, base+"/api/v1/me", exchanged.Token, &me); err != nil {
		return err
	}

	fmt.Printf("%s (%s) on %s\n", me.User.Email, me.User.Role, base)
	if dep.Name != "" {
		fmt.Printf("this machine is registered as %q\n", dep.Name)
	}
	return nil
}

// logoutCmd forgets a deployment locally and signs the grant out remotely.
func logoutCmd(args []string) error {
	fs := flag.NewFlagSet("panorama logout", flag.ContinueOnError)
	url := fs.String("url", "", "the deployment to sign out of")
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	base, _, err := creds.resolve(*url)
	if err != nil {
		return err
	}

	delete(creds.Deployments, base)
	if creds.Default == base {
		creds.Default = ""
		for other := range creds.Deployments {
			creds.Default = other
			break
		}
	}
	if err := saveCredentials(creds); err != nil {
		return err
	}

	// Said plainly rather than claimed. Removing the file locally does not
	// revoke anything, and a developer who thinks a lost machine has been
	// dealt with because they ran this would be wrong.
	fmt.Fprintf(os.Stderr, "Forgot %s on this machine.\n", base)
	fmt.Fprintf(os.Stderr, "The sign-in itself is still live: revoke it under My setup in the console.\n")
	return nil
}

// loadCredentials reads the file, or returns an empty set when there is none.
func loadCredentials() (*credentials, error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}

	// The path is this process's own, built from the user's home directory or
	// PANORAMA_HOME; nothing a server said reaches it.
	raw, err := os.ReadFile(path) // #nosec G304
	if errors.Is(err, os.ErrNotExist) {
		return &credentials{Deployments: map[string]*deployment{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// A credential in a file others can read is one to treat as leaked. Said
	// rather than silently corrected, because correcting it would not un-leak
	// it and the person needs to decide whether to sign in again.
	if info, serr := os.Stat(path); serr == nil && info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr,
			"panorama: %s is readable by other users (%#o). Revoke this sign-in in the console and run panorama login again.\n",
			path, info.Mode().Perm())
	}

	var creds credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if creds.Deployments == nil {
		creds.Deployments = map[string]*deployment{}
	}
	return &creds, nil
}

func saveCredentials(creds *credentials) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if mkerr := os.MkdirAll(filepath.Dir(path), 0o700); mkerr != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), mkerr)
	}

	encoded, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}

	// Written to a temporary file and renamed, so an interrupted write cannot
	// leave a half-file that signs the machine out.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(credentialsMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(append(encoded, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func credentialsPath() (string, error) {
	if override := os.Getenv("PANORAMA_HOME"); override != "" {
		return filepath.Join(override, credentialsFile), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding your home directory: %w", err)
	}
	return filepath.Join(home, ".panorama", credentialsFile), nil
}

func (c *credentials) set(base string, dep *deployment) {
	if c.Deployments == nil {
		c.Deployments = map[string]*deployment{}
	}
	c.Deployments[base] = dep
	c.Default = base
}

// resolve picks the deployment a command is about.
func (c *credentials) resolve(requested string) (string, *deployment, error) {
	base := strings.TrimRight(strings.TrimSpace(requested), "/")
	if base == "" {
		base = c.Default
	}
	if base == "" {
		return "", nil, errors.New("not signed in to any Panorama; run panorama login -url https://panorama.your-company.internal")
	}

	dep, ok := c.Deployments[base]
	if !ok || dep.Grant == "" {
		return "", nil, fmt.Errorf("not signed in to %s; run panorama login -url %s", base, base)
	}
	return base, dep, nil
}

func postJSON(ctx context.Context, url, bearer string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = strings.NewReader("{}")
	}
	return call(ctx, http.MethodPost, url, bearer, reader, out)
}

func getJSON(ctx context.Context, url, bearer string, out any) error {
	return call(ctx, http.MethodGet, url, bearer, nil, out)
}

func call(ctx context.Context, method, url, bearer string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	if out != nil && len(raw) > 0 {
		// Decoded even on failure, because the device-flow errors are carried
		// in the body and the caller reads them from the same struct.
		_ = json.Unmarshal(raw, out)
	}

	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error  string `json:"error"`
			Detail string `json:"error_description"`
			Msg    string `json:"message"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		switch {
		case apiErr.Error != "":
			// An RFC 8628 answer; the caller inspects it rather than failing.
			return fmt.Errorf("%s", apiErr.Error)
		case apiErr.Msg != "":
			return errors.New(apiErr.Msg)
		default:
			return fmt.Errorf("%s said %s", url, resp.Status)
		}
	}
	return nil
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return at
}

// openBrowser is best-effort. The URL is printed either way, so a machine with
// no browser - a container, a remote shell - loses nothing but a convenience.
//
// The address comes from the deployment's own reply, so it is checked before it
// is handed to a URL opener. `open` and `xdg-open` dispatch on scheme, and a
// deployment that returned file:// or something worse would otherwise have this
// command open it. An address that does not sit under the deployment the user
// typed is not opened at all - it is only ever printed.
func openBrowser(ctx context.Context, base, raw string) {
	target, err := neturl.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return
	}
	if !strings.HasPrefix(raw, base+"/") && raw != base {
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", raw) // #nosec G204 -- scheme and origin checked above
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", raw) // #nosec G204
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", raw) // #nosec G204
	}
	_ = cmd.Start()
}
