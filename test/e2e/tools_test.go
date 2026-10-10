// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// toolSetup is what GET /api/v1/setup/tools serves.
//
// The rows are decoded loosely because each tool carries a different shape -
// Claude Code gets environment rows, opencode gets a rendered provider block -
// and the assertions below are about what a specific row contains.
type toolSetup struct {
	Tools            []map[string]any `json:"tools"`
	Endpoint         string           `json:"endpoint"`
	TokenPlaceholder string           `json:"token_placeholder"`
}

func (s toolSetup) row(t *testing.T, id string) map[string]any {
	t.Helper()

	for _, row := range s.Tools {
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("no %q row in /api/v1/setup/tools", id)
	return nil
}

func (d *deployment) toolSetup() toolSetup {
	d.t.Helper()

	var out toolSetup
	d.getJSON("/api/v1/setup/tools", &out)
	return out
}

// sandbox is a private HOME for one tool run.
//
// Everything a coding tool reads is under a home directory, and this suite must
// never write to the one running it. On Windows the variable is USERPROFILE,
// and XDG_CONFIG_HOME is set as well because opencode looks there first.
type sandbox struct {
	home string
	repo string
	env  []string
}

func newSandbox(t *testing.T, d *deployment) *sandbox {
	t.Helper()

	home := t.TempDir()
	repo := gitFixture(t)
	s := &sandbox{
		home: home,
		repo: repo,
		env: append(baseEnv(),
			// Set alongside cmd.Dir, not instead of it: a tool may call getcwd
			// or may read PWD, and the two disagreeing is how this suite
			// briefly reported the wrong repository.
			"PWD="+repo,
			"HOME="+home,
			"USERPROFILE="+home,
			"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
			"PANORAMA_HOME="+filepath.Join(home, ".panorama"),
			// A first run that phoned home would be a network dependency this
			// suite does not want and an air-gapped deployment would not have.
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"DISABLE_TELEMETRY=1",
			"DISABLE_AUTOUPDATER=1",
			"CI=1",
		),
	}
	return s
}

// The branch the fixture repository is on.
//
// APER-1234 is what the default ticket pattern extracts from it, and APER is
// not one of the prefixes that pattern excludes - RFC, HTTP, SHA and the rest
// are, because they appear in ordinary prose.
const (
	fixtureBranch = "feature/APER-1234-ci-harness"
	fixtureTicket = "APER-1234"
	fixtureRepo   = "acme/panorama-e2e"
	// What the checkout is called on disk, and therefore what a session
	// records as its repository when nothing carried a remote.
	fixtureDirectory = "panorama-e2e"
)

// gitFixture builds the repository a tool is run inside.
//
// This is what makes attribution fire with no client configuration at all: a
// coding agent puts its branch and remote in the request body as ordinary
// context, and the extractors read them in flight. Without a real repository
// there is nothing to attribute and the test would prove only that a request
// was recorded.
func gitFixture(t *testing.T) string {
	t.Helper()

	// A directory named like a checkout, not like a temp directory.
	//
	// The working directory's last element is what the extractors keep - it is
	// the one value that would otherwise say something about a person rather
	// than about their work, so /Users/their-name never travels. In a one-shot
	// run with no tool calls it is also the only repository signal there is:
	// the remote appears in a request body only when `git remote` output comes
	// back inside a tool result, which a single -p turn never produces.
	dir := filepath.Join(t.TempDir(), fixtureDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the fixture repository: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// A runner has no committer identity, and `git commit` refuses without
		// one. Set locally rather than globally: this must not touch the
		// machine's own configuration.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Panorama CI", "GIT_AUTHOR_EMAIL=ci@example.com",
			"GIT_COMMITTER_NAME=Panorama CI", "GIT_COMMITTER_EMAIL=ci@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("remote", "add", "origin", "https://github.com/"+fixtureRepo+".git")

	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# panorama-e2e\n\nA fixture repository.\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture README: %v", err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "Add a README")
	run("checkout", "-q", "-b", fixtureBranch)

	// An uncommitted change, because that is what a repository somebody is
	// working in looks like. It is not decoration: a coding agent reports git
	// state as part of its own context, and a spotless tree is the one state
	// where some of them report nothing at all.
	notes := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(notes, []byte("# Notes\n\nWork in progress.\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture's uncommitted change: %v", err)
	}

	return dir
}

var userCodePattern = regexp.MustCompile(`Your code is\s+([A-Za-z0-9-]+)`)

// deviceLogin drives `panorama login` to completion.
//
// It is the recommended credential path and the one `panorama configure`
// requires, so testing it is not optional: a device flow that stopped working
// would leave the whole CLI story broken with every other test still green. The
// human half - reading a code off a terminal and approving it in the console -
// is replaced by parsing the code and calling the same endpoint the console
// calls.
func (s *sandbox) deviceLogin(t *testing.T, d *deployment) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.binary, "login", "-url", d.baseURL, "-no-browser", "-name", "e2e")
	cmd.Dir = s.repo
	cmd.Env = s.env

	// The code is printed to stderr, before the polling loop begins.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("piping login's stderr: %v", err)
	}
	var stdout strings.Builder
	cmd.Stdout = &stdout

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting login: %v", err)
	}

	codes := make(chan string, 1)
	var transcript strings.Builder
	go func() {
		defer close(codes)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			transcript.WriteString(line + "\n")
			if m := userCodePattern.FindStringSubmatch(line); m != nil {
				select {
				case codes <- m[1]:
				default:
				}
			}
		}
	}()

	var code string
	select {
	case c, ok := <-codes:
		if !ok || c == "" {
			_ = cmd.Process.Kill()
			t.Fatalf("login printed no user code:\n%s", transcript.String())
		}
		code = c
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("login printed no user code within 30s:\n%s", transcript.String())
	}

	// What the console does when somebody clicks Approve. A gateway token
	// carries no CSRF requirement, which is what lets a script stand in here.
	d.putJSON("POST", "/api/v1/cli/decide", map[string]any{
		"user_code": code,
		"approve":   true,
	})

	if err := cmd.Wait(); err != nil {
		t.Fatalf("login did not complete: %v\n%s\n%s", err, transcript.String(), stdout.String())
	}
}

// configure runs `panorama configure` for a tool and writes the file.
func (s *sandbox) configure(t *testing.T, d *deployment, tool string, extra ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	args := append([]string{"configure", tool, "-url", d.baseURL}, extra...)
	cmd := exec.CommandContext(ctx, d.binary, args...)
	cmd.Dir = s.repo
	cmd.Env = s.env

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("panorama configure %s: %v\n%s", tool, err, out)
	}
	return string(out)
}

// mintedToken returns a gateway token from the signed-in credential store.
//
// This is `panorama token`, the command Claude Code's apiKeyHelper calls, so it
// exercises the same exchange rather than reusing the admin token minted at
// bootstrap.
func (s *sandbox) mintedToken(t *testing.T, d *deployment) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.binary, "token", "-url", d.baseURL, "-quiet")
	cmd.Dir = s.repo
	cmd.Env = s.env

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("panorama token: %v", err)
	}
	token := strings.TrimSpace(string(out))
	if !strings.HasPrefix(token, "pnt_") {
		t.Fatalf("panorama token printed something that is not a gateway token: %q", token)
	}
	return token
}

// toolPath finds an installed coding tool, or skips.
//
// Skipping rather than failing is what makes `make e2e` useful on a machine
// with only a Go toolchain: the gateway-level tests still run, and CI installs
// the tools so the legs that need them do too.
func toolPath(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not installed; install it to run this leg", name)
	}
	return path
}

// readJSON reads a JSON file the tool or the CLI wrote.
func readJSON(t *testing.T, path string, out any) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", path, err, raw)
	}
}
