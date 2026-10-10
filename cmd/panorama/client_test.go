// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTokenSubcommandDispatch guards the collision between the two things
// `token` means: the client command Claude Code's apiKeyHelper runs, and the
// server-side command that mints a credential into a database.
func TestTokenSubcommandDispatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cmd    string
		args   []string
		client bool
	}{
		{"bare token is the client", "token", nil, true},
		{"token with flags is the client", "token", []string{"--quiet"}, true},
		{"token create writes to a database", "token", []string{"create", "-email", "a@b.c"}, false},
		{"login is the client", "login", []string{"-url", "https://x"}, true},
		{"configure is the client", "configure", []string{"claude-code"}, true},
		{"serve is not", "serve", nil, false},
		{"team add is not", "team", []string{"add"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientCommand(tc.cmd, tc.args); got != tc.client {
				t.Errorf("clientCommand(%q, %v) = %v, want %v", tc.cmd, tc.args, got, tc.client)
			}
		})
	}
}

func TestCredentialsRoundTripAndPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PANORAMA_HOME", home)

	// A machine that has never signed in reads as empty rather than failing.
	creds, err := loadCredentials()
	if err != nil {
		t.Fatalf("loadCredentials on a fresh machine: %v", err)
	}
	if _, _, rerr := creds.resolve(""); rerr == nil {
		t.Error("resolve succeeded with nothing signed in")
	}

	creds.set("https://panorama.example", &deployment{Grant: "png_x", Name: "laptop"})
	if serr := saveCredentials(creds); serr != nil {
		t.Fatalf("saveCredentials: %v", serr)
	}

	info, err := os.Stat(filepath.Join(home, credentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != credentialsMode {
		t.Errorf("credentials file is %#o, want %#o - it holds a live credential", perm, credentialsMode)
	}

	reloaded, err := loadCredentials()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	base, dep, err := reloaded.resolve("")
	if err != nil {
		t.Fatalf("resolve after reload: %v", err)
	}
	if base != "https://panorama.example" || dep.Grant != "png_x" {
		t.Errorf("round trip lost the deployment: %s %+v", base, dep)
	}

	// A second deployment does not displace the first: consultants and anyone
	// with a staging environment use more than one.
	reloaded.set("https://staging.example", &deployment{Grant: "png_y"})
	if serr := saveCredentials(reloaded); serr != nil {
		t.Fatal(serr)
	}
	again, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, rerr := again.resolve("https://panorama.example"); rerr != nil {
		t.Errorf("signing in elsewhere forgot the first deployment: %v", rerr)
	}
	if again.Default != "https://staging.example" {
		t.Errorf("default = %q, want the one just signed in to", again.Default)
	}
}

// TestSaveCredentialsDoesNotLeaveAHalfFile. An interrupted write that truncated
// the file would sign the machine out.
func TestSaveCredentialsIsAtomic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PANORAMA_HOME", home)

	creds := &credentials{Deployments: map[string]*deployment{}}
	creds.set("https://panorama.example", &deployment{Grant: "png_x"})
	if err := saveCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if err := saveCredentials(creds); err != nil {
		t.Fatalf("rewriting over an existing file: %v", err)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".credentials-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

// TestConfigureMergesRatherThanReplaces. Somebody's permissions, hooks and
// model choice live in the same file.
func TestConfigureMergesRatherThanReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	existing := map[string]any{
		"model": "opus",
		"permissions": map[string]any{
			"allow": []any{"Bash(git diff:*)"},
		},
		"env": map[string]any{"FOO": "bar"},
	}
	raw, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(path, raw, 0o600); werr != nil {
		t.Fatal(werr)
	}

	merged, err := mergeSettings(path, map[string]any{
		"apiKeyHelper": "panorama token --quiet",
		"env": map[string]any{
			"ANTHROPIC_BASE_URL": "https://panorama.example",
		},
	})
	if err != nil {
		t.Fatalf("mergeSettings: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatal(err)
	}

	if out["model"] != "opus" {
		t.Error("configure took someone's model choice with it")
	}
	if _, ok := out["permissions"]; !ok {
		t.Error("configure took someone's permissions with it")
	}
	env, _ := out["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Error("configure replaced the env block instead of merging into it")
	}
	if env["ANTHROPIC_BASE_URL"] != "https://panorama.example" {
		t.Error("configure did not set the base URL")
	}
	if out["apiKeyHelper"] != "panorama token --quiet" {
		t.Error("configure did not set the helper")
	}
}

// TestConfigureRefusesToOverwriteBrokenJSON. A file that does not parse is
// still somebody's configuration, and replacing it is not a repair.
func TestConfigureRefusesToOverwriteBrokenJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := mergeSettings(path, map[string]any{
		"apiKeyHelper": "x", "env": map[string]any{},
	}); err == nil {
		t.Error("mergeSettings overwrote a file it could not parse")
	}
}

// TestHelperTTLStaysUnderTheRefreshPoint is the arithmetic that keeps a tool
// from using an expired token.
//
// `panorama token` refreshes at half a token's life, so anything it hands back
// is good for at least half a lifetime. The helper has to ask again before that
// runs out, or Claude Code would keep using a token past its expiry for the
// remainder of its own interval.
func TestHelperTTLStaysUnderTheRefreshPoint(t *testing.T) {
	for _, ttl := range []time.Duration{5 * time.Minute, time.Hour, 8 * time.Hour} {
		helper := time.Duration(float64(ttl) * helperRefreshFraction)
		if helper >= ttl/2 {
			t.Errorf("token life %s: helper asks every %s, which is not sooner than the %s refresh point",
				ttl, helper, ttl/2)
		}
	}
}

// TestConfigureAcceptsFlagsAfterTheToolName covers a silent no-op.
//
// Go's flag package stops at the first argument that is not a flag, so parsing
// the whole list before reading the tool name out of fs.Args() ignored every
// flag that followed it. `panorama configure claude-code -write` - the form the
// setup screen tells people to run - therefore printed the configuration,
// wrote nothing, and exited 0.
func TestConfigureAcceptsFlagsAfterTheToolName(t *testing.T) {
	cases := []struct {
		name string
		args []string
		rest []string
	}{
		{
			name: "flags after the tool, as the console publishes it",
			args: []string{"claude-code", "-url", "https://panorama.example", "-write"},
			rest: []string{"-url", "https://panorama.example", "-write"},
		},
		{
			name: "flags before the tool",
			args: []string{"-url", "https://panorama.example", "-write", "claude-code"},
			rest: []string{"-url", "https://panorama.example", "-write"},
		},
		{
			name: "a value that looks like a tool name is not one",
			args: []string{"-path", "opencode.json", "opencode"},
			rest: []string{"-path", "opencode.json"},
		},
		{
			name: "an = form carries its own value",
			args: []string{"-url=https://panorama.example", "opencode", "-write"},
			rest: []string{"-url=https://panorama.example", "-write"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, rest := splitToolName(tc.args)
			if tool == "" {
				t.Fatalf("no tool name found in %v", tc.args)
			}
			if !slices.Equal(rest, tc.rest) {
				t.Errorf("rest = %v, want %v", rest, tc.rest)
			}

			fs := flag.NewFlagSet("configure", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			url := fs.String("url", "", "")
			write := fs.Bool("write", false, "")
			path := fs.String("path", "", "")
			if err := fs.Parse(rest); err != nil {
				t.Fatalf("parsing %v: %v", rest, err)
			}

			// Whichever order they were written in, the flags have to have
			// been read. A -write that parsed as a positional is the bug.
			if strings.Contains(strings.Join(tc.args, " "), "-write") && !*write {
				t.Error("-write was not parsed; the command would print and write nothing")
			}
			if strings.Contains(strings.Join(tc.args, " "), "-url") && *url == "" {
				t.Error("-url was not parsed; the command would use the default deployment")
			}
			if strings.Contains(strings.Join(tc.args, " "), "-path") && *path == "" {
				t.Error("-path was not parsed; the command would write to the default location")
			}
		})
	}
}

// TestOpenCodeMergeSelectsAModel covers the other half of a configuration that
// looked complete and did nothing.
//
// Merging only the provider block declared every route the reader may use and
// selected none of them, which leaves opencode on whatever its picker had last
// - on a first run, nothing at all.
func TestOpenCodeMergeSelectsAModel(t *testing.T) {
	generated := `{
	  "$schema": "https://opencode.ai/config.json",
	  "provider": {"panorama": {"name": "Panorama", "models": {"standard": {}}}},
	  "model": "panorama/standard",
	  "small_model": "panorama/fast"
	}`

	t.Run("a fresh file gets a selection", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opencode.json")

		merged, err := mergeOpenCodeConfig(path, generated)
		if err != nil {
			t.Fatalf("merging: %v", err)
		}

		var got map[string]any
		if uerr := json.Unmarshal(merged, &got); uerr != nil {
			t.Fatalf("the merged file is not JSON: %v", uerr)
		}
		if got["model"] != "panorama/standard" {
			t.Errorf("model = %v, want panorama/standard", got["model"])
		}
		if got["small_model"] != "panorama/fast" {
			t.Errorf("small_model = %v, want panorama/fast", got["small_model"])
		}
	})

	t.Run("another provider's selection is left alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opencode.json")
		existing := `{"model": "anthropic/claude-sonnet-4-5", "small_model": "anthropic/claude-haiku-4-5"}`
		if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}

		merged, err := mergeOpenCodeConfig(path, generated)
		if err != nil {
			t.Fatalf("merging: %v", err)
		}

		var got map[string]any
		if uerr := json.Unmarshal(merged, &got); uerr != nil {
			t.Fatalf("the merged file is not JSON: %v", uerr)
		}
		if got["model"] != "anthropic/claude-sonnet-4-5" {
			t.Errorf("model = %v; a deliberate choice of another provider was overwritten", got["model"])
		}
	})

	t.Run("a stale panorama selection is refreshed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "opencode.json")
		existing := `{"model": "panorama/a-route-that-was-deleted"}`
		if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}

		merged, err := mergeOpenCodeConfig(path, generated)
		if err != nil {
			t.Fatalf("merging: %v", err)
		}

		var got map[string]any
		if uerr := json.Unmarshal(merged, &got); uerr != nil {
			t.Fatalf("the merged file is not JSON: %v", uerr)
		}
		if got["model"] != "panorama/standard" {
			t.Errorf("model = %v; a selection this command wrote should follow the routes", got["model"])
		}
	})
}
