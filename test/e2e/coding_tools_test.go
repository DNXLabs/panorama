// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// onPath puts the panorama binary somewhere a tool can run it as `panorama`.
//
// Claude Code's recommended credential mode is an apiKeyHelper, which is a
// command it shells out to. Without this the settings file this suite wrote
// would name a command that is not there, and the leg would fail for a reason
// that has nothing to do with the deployment.
func (s *sandbox) onPath(t *testing.T, d *deployment) []string {
	t.Helper()

	bin := filepath.Join(s.home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatalf("creating %s: %v", bin, err)
	}

	name := "panorama"
	if isWindows() {
		name += ".exe"
	}
	target := filepath.Join(bin, name)

	raw, err := os.ReadFile(d.binary)
	if err != nil {
		t.Fatalf("reading the panorama binary: %v", err)
	}
	if err := os.WriteFile(target, raw, 0o700); err != nil {
		t.Fatalf("installing panorama into the sandbox: %v", err)
	}

	separator := ":"
	if isWindows() {
		separator = ";"
	}
	return append(s.env, "PATH="+bin+separator+os.Getenv("PATH"))
}

// runTool executes a coding tool inside the fixture repository.
func runTool(t *testing.T, env []string, dir, bin string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(env), "PWD="+dir)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestClaudeCodeEndToEnd runs the real tool against the real binary.
//
// Both credential modes are covered, because they are different products from
// the deployment's point of view: a helper the tool shells out to and rotates,
// and a static token pasted into an environment. Only the first survives a
// token expiring mid-session, and only the second works where a shell command
// in a settings file does not.
func TestClaudeCodeEndToEnd(t *testing.T) {
	bin := toolPath(t, "claude")

	d := newDeployment(t)
	s := newSandbox(t, d)
	s.deviceLogin(t, d)
	s.configure(t, d, "claude-code", "-write")

	env := s.onPath(t, d)

	// Which route becomes ANTHROPIC_MODEL is the deployment's answer, not this
	// test's. The endpoint picks the first route the reader may use, so
	// hardcoding a name here would assert an ordering nobody promised - and
	// would fail the day somebody adds a route.
	wantRoute := consoleEnv(t, d.toolSetup().row(t, "claude-code"))["ANTHROPIC_MODEL"]
	if wantRoute == "" {
		t.Fatal("the deployment named no route for Claude Code")
	}

	t.Run("the settings file the CLI wrote", func(t *testing.T) {
		// Nothing is set in the environment here. Everything the tool needs -
		// where the gateway is, which route to ask for, how to get a
		// credential - has to come out of ~/.claude/settings.json.
		out := runTool(t, env, s.repo, bin, "-p", "Reply with the single word: ok")
		t.Logf("claude: %s", strings.TrimSpace(out))

		got := d.latest()
		if got.Tool != "claude-code" {
			t.Errorf("tool = %q, want claude-code", got.Tool)
		}
		if got.Decision != "allowed" {
			t.Errorf("decision = %q, want allowed", got.Decision)
		}
		if got.RequestedModel != wantRoute {
			t.Errorf("requested_model = %q, want the configured route %q", got.RequestedModel, wantRoute)
		}
		if got.ResolvedModel == got.RequestedModel {
			t.Error("the route name was passed through as the model; nothing resolved it")
		}
		if !got.Priced {
			t.Error("the request was not priced")
		}
	})

	t.Run("a token in the environment", func(t *testing.T) {
		// A sandbox of its own, with no settings file in it. Claude Code reads
		// the env block of ~/.claude/settings.json in preference to the process
		// environment, so reusing the configured one would test the file again
		// under a different name.
		plain := newSandbox(t, d)
		token := s.mintedToken(t, d)
		withToken := make([]string, 0, len(plain.env)+3)
		withToken = append(withToken, plain.env...)
		withToken = append(withToken,
			"ANTHROPIC_BASE_URL="+d.baseURL,
			"ANTHROPIC_AUTH_TOKEN="+token,
			"ANTHROPIC_MODEL="+routeFast,
		)

		out := runTool(t, withToken, plain.repo, bin, "-p", "Reply with the single word: ok")
		t.Logf("claude: %s", strings.TrimSpace(out))

		got := d.latest()
		if got.RequestedModel != routeFast {
			t.Errorf("requested_model = %q, want %q", got.RequestedModel, routeFast)
		}
		if got.Tool != "claude-code" {
			t.Errorf("tool = %q, want claude-code", got.Tool)
		}
	})
}

// TestClaudeCodeIsAttributedWithNoClientConfiguration is the headline.
//
// Nothing about the tool is told what repository or branch it is in. The
// environment block a coding agent writes into its own request carries both,
// and the extractors read them in flight and discard the body.
func TestClaudeCodeIsAttributedWithNoClientConfiguration(t *testing.T) {
	bin := toolPath(t, "claude")

	d := newDeployment(t)
	s := newSandbox(t, d)
	s.deviceLogin(t, d)
	s.configure(t, d, "claude-code", "-write")

	out := runTool(t, s.onPath(t, d), s.repo, bin, "-p", "Reply with the single word: ok")
	t.Logf("claude: %s", strings.TrimSpace(out))

	d.waitForRequests(1)
	got := d.onboarding()

	if !got.HasRequest {
		t.Fatal("the deployment saw no request")
	}
	t.Logf("detected: tool=%q route=%q repo=%q branch=%q attributed=%v describe=%q",
		got.Detected.Tool, got.Detected.Route, got.Detected.Repo,
		got.Detected.Branch, got.Detected.Attributed, got.Detected.Describe)

	if got.Detected.Branch != fixtureBranch {
		t.Errorf("branch = %q, want %q", got.Detected.Branch, fixtureBranch)
	}
	if got.Detected.Repo != fixtureDirectory {
		t.Errorf("repo = %q, want %q", got.Detected.Repo, fixtureDirectory)
	}
	if !got.Detected.Attributed {
		t.Fatal("the request was not attributed to any work item")
	}
	if got.Detected.WorkItem.Key != fixtureTicket {
		t.Errorf("work item = %q, want %q", got.Detected.WorkItem.Key, fixtureTicket)
	}
	// Structural beats content: the branch name is ground truth from the
	// environment block, not a ticket somebody happened to mention.
	if got.Detected.WorkItem.Rung != "structural" {
		t.Errorf("rung = %q, want structural", got.Detected.WorkItem.Rung)
	}
}

// TestOpenCodeEndToEnd runs opencode against the provider block the deployment
// rendered.
//
// The block is rendered whole on the server because the context window and
// output cap in it are RouteMinimum over a route's reachable models, which the
// browser has no way to compute.
func TestOpenCodeEndToEnd(t *testing.T) {
	bin := toolPath(t, "opencode")

	d := newDeployment(t)
	s := newSandbox(t, d)
	s.deviceLogin(t, d)
	s.configure(t, d, "opencode", "-write")

	// opencode has no credential-helper command, so the generated block reads
	// {env:PANORAMA_TOKEN} and a token is minted per shell. A session outliving
	// its token needs a new one; that is stated in the product rather than
	// papered over, and it is what this reproduces.
	env := append(s.onPath(t, d), "PANORAMA_TOKEN="+s.mintedToken(t, d))

	out := runTool(t, env, s.repo, bin, "run", "Reply with the single word: ok")
	t.Logf("opencode: %s", strings.TrimSpace(out))

	got := d.latest()

	// The assertion this leg exists for. opencode reaches Bedrock through the
	// Vercel AI SDK, which sends no distinctive user agent, so the only reason
	// its traffic is identifiable at all is the header the generated provider
	// block sets on every model. Drop that key and every request here becomes
	// "unknown" with nothing failing.
	if got.Tool != "opencode" {
		t.Errorf("tool = %q, want opencode - the generated provider block sets the user agent", got.Tool)
	}
	if got.Decision != "allowed" {
		t.Errorf("decision = %q, want allowed", got.Decision)
	}
	if !got.Priced {
		t.Error("the request was not priced")
	}

	if got.SessionID == "" {
		t.Error("no session was recorded; turns could not be grouped into a conversation")
	}

	// What opencode reports about the repository, pinned as it actually is.
	//
	// Checked against the request body rather than assumed: opencode's context
	// carries "Working directory: <path>" and, as of 1.18.29, no branch at all.
	// So the directory is attributed and a work item is not - the ladder has
	// nothing structural to climb, because a branch name is where the ticket
	// key lives.
	//
	// This is a difference between the two supported tools, not a defect in
	// either, and it is asserted rather than logged so that opencode gaining a
	// branch shows up here as a failing test with an obvious fix, instead of
	// silently improving in a number nobody was watching.
	o := d.onboarding()
	t.Logf("opencode attribution: repo=%q branch=%q attributed=%v describe=%q",
		o.Detected.Repo, o.Detected.Branch, o.Detected.Attributed, o.Detected.Describe)

	if o.Detected.Repo != fixtureDirectory {
		t.Errorf("repo = %q, want %q - opencode does carry its working directory",
			o.Detected.Repo, fixtureDirectory)
	}
	if o.Detected.Branch != "" {
		t.Errorf("branch = %q: opencode has started sending a branch. "+
			"Attribution can now reach a work item through it - tighten this test to assert %q and %q",
			o.Detected.Branch, fixtureBranch, fixtureTicket)
	}
}

// TestABudgetStopsARealTool is the limit as the engineer meets it.
//
// The refusal is rendered by the tool, not by a test client, which is the only
// way to know that an Anthropic-shaped error envelope carrying a paragraph of
// explanation actually reaches somebody.
func TestABudgetStopsARealTool(t *testing.T) {
	bin := toolPath(t, "claude")

	d := newDeployment(t)
	s := newSandbox(t, d)
	s.deviceLogin(t, d)

	// A static token rather than the credential helper, deliberately. Claude
	// Code reads a 403 as an authentication failure and re-runs apiKeyHelper,
	// backing off between attempts - three minutes of retries against a budget
	// that resets next month. The refusal reaching the user is what this test
	// is about, and it reaches them either way.
	token := s.mintedToken(t, d)
	env := append(slices.Clone(s.onPath(t, d)),
		"ANTHROPIC_BASE_URL="+d.baseURL,
		"ANTHROPIC_AUTH_TOKEN="+token,
		"ANTHROPIC_MODEL="+routeStandard,
	)

	runTool(t, env, s.repo, bin, "-p", "Reply with the single word: ok")
	d.waitForRequests(1)

	d.setBudget("month", "", "0.0001")

	// The tool is expected to fail here, so this does not go through runTool.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-p", "Reply with the single word: ok")
	cmd.Dir = s.repo
	cmd.Env = append(slices.Clone(env), "PWD="+s.repo)
	out, err := cmd.CombinedOutput()
	t.Logf("claude (blocked): err=%v\n%s", err, out)

	got := d.latest()
	if got.Decision != "blocked" {
		t.Errorf("decision = %q, want blocked", got.Decision)
	}
	if got.Priced {
		t.Error("a blocked request was priced")
	}
}
