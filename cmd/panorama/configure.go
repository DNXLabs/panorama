// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// configureCmd writes the client-side configuration that points a coding tool
// at this deployment.
//
// The interesting part is what it does not write: a token. Claude Code takes an
// `apiKeyHelper` - a command it runs to obtain an auth value, on an interval it
// controls - so the configuration names `panorama token` rather than embedding
// a credential. That is what makes rotation invisible and revocation effective:
// there is nothing on disk to go stale, and a signed-out machine stops working
// at the next refresh rather than whenever somebody notices.
func configureCmd(ctx context.Context, args []string) error {
	// The tool name is taken before the flags are parsed, and the flags may
	// come on either side of it.
	//
	// Go's flag package stops at the first argument that is not a flag, so
	// parsing first and reading the name out of fs.Args() afterwards silently
	// ignores everything after the name. That is how `panorama configure
	// claude-code -write` - the form this product's own setup screen tells
	// people to run - came to print the configuration to stdout, write no file,
	// and exit 0.
	tool, rest := splitToolName(args)
	if tool == "" {
		return errors.New("which tool? panorama configure opencode, or panorama configure claude-code")
	}

	fs := flag.NewFlagSet("panorama configure "+tool, flag.ContinueOnError)
	url := fs.String("url", "", "the deployment to configure for")
	write := fs.Bool("write", false, "write the configuration file instead of printing it")
	path := fs.String("path", "", "where to write it (default: the tool's usual location)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return fmt.Errorf("panorama configure takes one tool; it was also given %q", strings.Join(extra, " "))
	}

	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	base, dep, err := creds.resolve(*url)
	if err != nil {
		return err
	}

	switch tool {
	case "claude-code", "claude":
		return configureClaudeCode(ctx, creds, base, dep, *write, *path)
	case "opencode":
		return configureOpenCode(ctx, creds, base, dep, *write, *path)
	default:
		return fmt.Errorf("panorama does not generate configuration for %q yet; "+
			"point the tool at %s and use `panorama token` for its credential", tool, base)
	}
}

// splitToolName pulls the tool out of the argument list, wherever it sits.
//
// A name is anything that is not a flag and does not follow a flag that takes a
// value, so both `configure claude-code -write` and `configure -write
// claude-code` name the same tool. Accepting one and quietly misreading the
// other is worse than refusing both.
func splitToolName(args []string) (tool string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			// -url and -path take a value, which is not the tool name.
			// -write is boolean and takes none. An "=" form carries its own.
			if !strings.Contains(arg, "=") && takesValue(arg) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if tool == "" {
			tool = arg
			continue
		}
		rest = append(rest, arg)
	}
	return tool, rest
}

// takesValue reports whether a configure flag is followed by its value.
func takesValue(arg string) bool {
	switch strings.TrimLeft(arg, "-") {
	case "url", "path":
		return true
	default:
		return false
	}
}

// helperRefreshFraction is how much of a token's life passes before Claude Code
// asks for another.
//
// Two fifths rather than a half, because `panorama token` already refreshes at
// the halfway point: asking a little sooner than that means the helper is never
// the thing that notices a token has expired. The two intervals are set from
// one number here so they cannot drift into a window where a valid-looking
// token is used past its expiry.
const helperRefreshFraction = 0.4

func configureClaudeCode(ctx context.Context, creds *credentials, base string, dep *deployment, write bool, path string) error {
	ttl := time.Hour
	if !dep.TokenExpiresAt.IsZero() && !dep.RefreshAfter.IsZero() {
		// RefreshAfter is half the token's life, so the life is twice the gap.
		if life := dep.TokenExpiresAt.Sub(dep.RefreshAfter) * 2; life > 0 {
			ttl = life
		}
	}
	// Rounded to a whole second so the generated file reads as a decision
	// somebody made rather than as the remainder of a division.
	helperTTL := (time.Duration(float64(ttl) * helperRefreshFraction)).Round(time.Second)

	env := map[string]any{
		"ANTHROPIC_BASE_URL":                base,
		"CLAUDE_CODE_API_KEY_HELPER_TTL_MS": fmt.Sprintf("%d", helperTTL.Milliseconds()),
	}

	// Which route this machine asks for is the deployment's answer, not this
	// binary's guess. Claude Code sends a model name on every request and a name
	// that is not a route is refused - including the built-in defaults it falls
	// back to, which is what makes an unset variable look like a broken gateway
	// rather than a missing route.
	for name, value := range routeVariables(ctx, creds, base, dep) {
		env[name] = value
	}

	// Not a credential, despite the name: it is the command Claude Code runs to
	// obtain one. That indirection is the whole point of this file.
	settings := map[string]any{ // #nosec G101
		"apiKeyHelper": "panorama token --quiet",
		"env":          env,
	}

	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if !write {
		fmt.Println(string(encoded))
		fmt.Fprintf(os.Stderr, "\nMerge this into ~/.claude/settings.json, or re-run with -write.\n")
		printBaseURLCaveat()
		return nil
	}

	target := path
	if target == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return fmt.Errorf("finding your home directory: %w", herr)
		}
		target = filepath.Join(home, ".claude", "settings.json")
	}

	merged, err := mergeSettings(target, settings)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, append(merged, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}

	fmt.Fprintf(os.Stderr, "Wrote %s. Claude Code will get its credential from `panorama token`.\n", target)
	printBaseURLCaveat()
	return nil
}

// configureOpenCode writes the provider block opencode reads.
//
// The whole block is fetched from the deployment rather than assembled here,
// which is the same arrangement configureClaudeCode uses for its variables and
// for the same reason: the console's setup screen renders that string too, so
// the file this writes and the block somebody copies out of a browser cannot
// drift apart. What routes a person may ask for, and what context window each
// one carries, are both facts only the deployment holds.
//
// The credential is the one place opencode is weaker than Claude Code, and the
// generated file says so rather than papering over it. There is no
// credential-helper command in opencode - a key is a literal, an environment
// variable, or the contents of a file - so this writes {env:PANORAMA_TOKEN} and
// tells the reader to mint one per shell. Nothing long-lived lands on disk,
// which is the property worth keeping; what is lost is rotation underneath a
// running session.
func configureOpenCode(ctx context.Context, creds *credentials, base string, dep *deployment, write bool, path string) error {
	config, err := openCodeConfig(ctx, creds, base, dep)
	if err != nil {
		return err
	}

	if !write {
		fmt.Println(config)
		fmt.Fprintf(os.Stderr, "\nMerge this into %s, or re-run with -write.\n", openCodeConfigDisplayPath)
		printOpenCodeTokenNote()
		return nil
	}

	target := path
	if target == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return fmt.Errorf("finding your home directory: %w", herr)
		}
		target = filepath.Join(home, ".config", "opencode", "opencode.json")
	}

	merged, err := mergeOpenCodeConfig(target, config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, append(merged, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}

	fmt.Fprintf(os.Stderr, "Wrote %s.\n", target)
	printOpenCodeTokenNote()
	return nil
}

// openCodeConfigDisplayPath is where opencode reads its global configuration,
// as it is written in a sentence rather than resolved on this machine.
const openCodeConfigDisplayPath = "~/.config/opencode/opencode.json"

// openCodeConfig asks the deployment for the provider block.
//
// Unlike routeVariables, a failure here is fatal. There is nothing useful to
// write without it: the block is the routes, their context windows and the
// endpoint, so a version of it assembled locally would be a guess at all three.
func openCodeConfig(ctx context.Context, creds *credentials, base string, dep *deployment) (string, error) {
	if err := ensureToken(ctx, creds, base, dep); err != nil {
		return "", fmt.Errorf("asking %s which routes you can use: %w", base, err)
	}

	var setup struct {
		Tools []struct {
			ID  string `json:"id"`
			CLI struct {
				Config string `json:"config"`
			} `json:"cli"`
		} `json:"tools"`
	}
	if err := getJSON(ctx, base+"/api/v1/setup/tools", dep.Token, &setup); err != nil {
		return "", fmt.Errorf("asking %s which routes you can use: %w", base, err)
	}

	for _, tool := range setup.Tools {
		if tool.ID == "opencode" && tool.CLI.Config != "" {
			if strings.Contains(tool.CLI.Config, routePlaceholder) {
				fmt.Fprintf(os.Stderr,
					"panorama: %s has no route you can use yet, so the model name is a placeholder.\n"+
						"          An administrator creates routes on the console's Routing screen.\n",
					base)
			}
			return tool.CLI.Config, nil
		}
	}
	return "", fmt.Errorf("%s did not offer an opencode configuration; it may be an older deployment", base)
}

// mergeOpenCodeConfig puts the Panorama provider into a file that may already
// hold others.
//
// Only the one provider key is replaced. opencode's configuration is where
// somebody keeps their local models, their theme and their keybindings, and a
// command that rewrote the file wholesale would be a command nobody runs twice.
func mergeOpenCodeConfig(path, generated string) ([]byte, error) {
	var ours map[string]any
	if err := json.Unmarshal([]byte(generated), &ours); err != nil {
		return nil, fmt.Errorf("the deployment sent a configuration that is not valid JSON: %w", err)
	}

	existing := map[string]any{}
	raw, err := os.ReadFile(path) // #nosec G304
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", path, err)
	default:
		if uerr := json.Unmarshal(raw, &existing); uerr != nil {
			// Refused rather than overwritten, exactly as for Claude Code's
			// settings: a file that does not parse is still somebody's
			// configuration, and replacing it is not a repair. opencode also
			// reads .jsonc, whose comments do not parse here - which is the
			// likeliest reason to land in this branch, and why the message
			// says the file was left alone rather than that it is broken.
			return nil, fmt.Errorf("%s is not valid JSON, so it has been left alone: %w", path, uerr)
		}
	}

	if schema, ok := ours["$schema"]; ok {
		if _, already := existing["$schema"]; !already {
			existing["$schema"] = schema
		}
	}

	providers, _ := existing["provider"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	mine, ok := ours["provider"].(map[string]any)
	if !ok {
		return nil, errors.New("panorama: the generated configuration has no provider block")
	}
	for k, v := range mine {
		providers[k] = v
	}
	existing["provider"] = providers

	// The two keys that choose a model, and they are not optional.
	//
	// Declaring every route somebody may use and selecting none of them leaves
	// opencode on whatever its picker had last, which on a first run is nothing
	// at all - a configured provider and a tool with no model. Merging only the
	// provider block did exactly that, so the file this wrote was inert until
	// the reader picked a model by hand.
	//
	// An existing selection is kept unless it names this provider: somebody who
	// pointed opencode at another provider chose that, while an "panorama/…"
	// value is this command's own from a previous run and is what goes stale
	// when their routes change.
	for _, key := range []string{"model", "small_model"} {
		chosen, ok := ours[key].(string)
		if !ok || chosen == "" {
			continue
		}
		if current, taken := existing[key].(string); taken && !isOpenCodePanorama(current) {
			continue
		}
		existing[key] = chosen
	}

	return json.MarshalIndent(existing, "", "  ")
}

// isOpenCodePanorama reports whether an opencode model selection names one of
// the provider blocks this command writes.
func isOpenCodePanorama(selection string) bool {
	for _, prefix := range openCodeProviderPrefixes {
		if strings.HasPrefix(selection, prefix) {
			return true
		}
	}
	return false
}

// openCodeProviderPrefixes are how a model of this deployment's providers is
// named in opencode's "provider/model" form: one block for routes on the
// Anthropic Messages API, one for routes on OpenAI Responses, and one for
// routes on models that speak only Chat Completions.
var openCodeProviderPrefixes = []string{"panorama/", "panorama-openai/", "panorama-chat/"}

// printOpenCodeTokenNote says how the credential gets there, and what it does
// not do.
func printOpenCodeTokenNote() {
	fmt.Fprint(os.Stderr, `
The configuration reads your token from the environment, so mint one per shell:

    export PANORAMA_TOKEN=$(panorama token --quiet)

`+"`panorama token`"+` refreshes it when it is due, so a new shell always has a
working one. opencode has no credential-helper command, so a session that runs
longer than a token's lifetime will need a new shell rather than rotating in
place.
`)
}

// routeVariables asks the deployment which routes this person may ask for, and
// returns the environment variables that point Claude Code at them.
//
// Asked rather than derived: the routes a person can use depend on conditions
// only the deployment knows, and the same endpoint fills in the console's setup
// screen - so the block somebody copies from a browser and the file this writes
// cannot disagree.
//
// A deployment that cannot be reached is not a reason to refuse: the rest of
// the file is still correct and still useful, so the failure is reported and
// the variables are left out, with the reader told what is missing.
func routeVariables(ctx context.Context, creds *credentials, base string, dep *deployment) map[string]string {
	if err := ensureToken(ctx, creds, base, dep); err != nil {
		fmt.Fprintf(os.Stderr, "panorama: could not ask %s which routes you can use: %v\n", base, err)
		return nil
	}

	var setup struct {
		Tools []struct {
			ID  string `json:"id"`
			Env []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		} `json:"tools"`
	}
	if err := getJSON(ctx, base+"/api/v1/setup/tools", dep.Token, &setup); err != nil {
		fmt.Fprintf(os.Stderr, "panorama: could not ask %s which routes you can use: %v\n", base, err)
		return nil
	}

	out := map[string]string{}
	for _, tool := range setup.Tools {
		if tool.ID != "claude-code" {
			continue
		}
		for _, kv := range tool.Env {
			if !settingsVariables[kv.Name] {
				continue
			}
			if strings.HasSuffix(kv.Name, "_MODEL") && kv.Value == routePlaceholder {
				fmt.Fprintf(os.Stderr,
					"panorama: %s has no route you can use yet, so %s is a placeholder.\n"+
						"          An administrator creates routes on the console's Routing screen.\n",
					base, kv.Name)
			}
			out[kv.Name] = kv.Value
		}
	}
	return out
}

// settingsVariables is what this command will write into a settings file.
//
// An allowlist of names rather than a suffix rule, and rather than "everything
// the deployment sent except the two the client owns". The base URL comes from
// the credential store and the token is the helper's job, so neither belongs
// here - but the reason to enumerate the rest is that this writes to a
// developer's machine from whatever a server answered, and a variable the
// deployment starts sending should need a line here rather than arriving on its
// own.
var settingsVariables = map[string]bool{
	"ANTHROPIC_MODEL":               true,
	"ANTHROPIC_DEFAULT_HAIKU_MODEL": true,
}

// routePlaceholder is what the deployment sends where a route name would go
// when there is no route the reader may use. It matches the constant the
// control plane serves; this binary only has to recognise it.
const routePlaceholder = "your-route-name"

// mergeSettings adds Panorama's keys to an existing settings file rather than
// replacing it.
//
// Overwriting would take somebody's permissions, hooks and model choice with
// it, which is not a thing a command called "configure" should do. Only the
// keys Panorama owns are touched, and env is merged rather than replaced.
func mergeSettings(path string, ours map[string]any) ([]byte, error) {
	existing := map[string]any{}

	// The path is the tool's own location, or one the user named with -path.
	raw, err := os.ReadFile(path) // #nosec G304
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", path, err)
	default:
		if uerr := json.Unmarshal(raw, &existing); uerr != nil {
			// Refused rather than overwritten. A file that does not parse is
			// still somebody's configuration, and replacing it is not a repair.
			return nil, fmt.Errorf("%s is not valid JSON, so it has been left alone: %w", path, uerr)
		}
	}

	existing["apiKeyHelper"] = ours["apiKeyHelper"]

	env, _ := existing["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	mine, ok := ours["env"].(map[string]any)
	if !ok {
		return nil, errors.New("panorama: the generated settings have no env block")
	}
	for k, v := range mine {
		env[k] = v
	}
	existing["env"] = env

	return json.MarshalIndent(existing, "", "  ")
}

// printBaseURLCaveat states a documented consequence rather than leaving it to
// be discovered.
//
// Pointing ANTHROPIC_BASE_URL at anything other than api.anthropic.com turns
// off Claude Code's MCP tool search by default, and disables Remote Control.
// Panorama relays request bodies verbatim, so tool search works once the flag
// is set - but nothing says so unless it is said here.
func printBaseURLCaveat() {
	_, _ = fmt.Fprint(os.Stderr, strings.Join([]string{
		"",
		"Two things Claude Code changes when its base URL is not api.anthropic.com:",
		"  - MCP tool search is off. Panorama forwards bodies unchanged, so set",
		"    ENABLE_TOOL_SEARCH=true to keep it.",
		"  - Remote Control is disabled, as it is on Bedrock directly.",
		"",
	}, "\n"))
}
