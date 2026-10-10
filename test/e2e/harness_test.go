// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dnxlabs/panorama/internal/config"
	"github.com/dnxlabs/panorama/internal/fakebedrock"
	"github.com/dnxlabs/panorama/internal/store"
)

// The account the harness bootstraps and runs as.
const (
	adminEmail = "ci@example.com"
	// routeStandard and routeFast are the two names a tool is configured with.
	// They are route names rather than model ids because that is the whole of
	// the contract: an engineer sets ANTHROPIC_MODEL to a name somebody routed,
	// and a model id sent by a client is a name nobody routed.
	routeStandard = "standard"
	routeFast     = "fast"
	routeThrifty  = "thrifty"
	// The routes on bedrock-mantle: Claude over the Anthropic Messages API, an
	// open-weight model over Chat Completions and Responses at /v1, a GPT-5.x
	// model over the same two APIs at /openai/v1, and a model that speaks Chat
	// Completions alone.
	routeMantleSonnet = "mantle-sonnet"
	routeOSS          = "oss"
	routeGPT          = "gpt"
	routeQwen         = "qwen"
)

// deployment is one Panorama, its database, and the fake Bedrock behind it -
// both endpoints, bedrock-runtime and bedrock-mantle, served by the one fake.
type deployment struct {
	t *testing.T

	binary  string
	dir     string
	baseURL string
	token   string

	Fake *fakebedrock.Server

	env []string
	cmd *exec.Cmd

	// sent counts the data-plane requests this deployment has been given, so a
	// test reading the audit trail waits for its own row rather than for "at
	// least one". The metadata write happens off the request path, so without
	// this a second request reads the first one's record and passes for the
	// wrong reason.
	//
	// Atomic because a deployment is shared by the goroutines of a test that
	// streams concurrently, and a counter that lost increments would have those
	// tests waiting for fewer rows than they sent - which is a pass, on
	// somebody else's record.
	sent atomic.Int64
	// log accumulates the gateway's stderr, and is printed only when a test
	// fails. A passing run that dumped a server log would bury the one that did
	// not.
	log     *lockedBuffer
	stopped bool
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// binaryOnce builds cmd/panorama at most once per `go test` invocation.
//
// Every deployment needs it and building it per test would dominate the run.
var binaryOnce struct {
	sync.Once
	path string
	err  error
}

// panoramaBinary returns the gateway binary to drive.
//
// PANORAMA_BINARY names a prebuilt one, which is what CI passes so the artifact
// under test is the same one `make build` produced rather than a second build
// with different flags.
func panoramaBinary(t *testing.T) string {
	t.Helper()

	if p := os.Getenv("PANORAMA_BINARY"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("resolving PANORAMA_BINARY: %v", err)
		}
		return abs
	}

	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "panorama-e2e-bin")
		if err != nil {
			binaryOnce.err = err
			return
		}
		out := filepath.Join(dir, "panorama")
		if isWindows() {
			out += ".exe"
		}
		// The console is embedded, so this needs ui/dist to exist. `make ui`
		// is the caller's job; saying so beats a go:embed error.
		cmd := exec.Command("go", "build", "-o", out, "./cmd/panorama")
		cmd.Dir = repoRoot()
		if combined, berr := cmd.CombinedOutput(); berr != nil {
			binaryOnce.err = fmt.Errorf("building the binary: %w\n%s", berr, combined)
			return
		}
		binaryOnce.path = out
	})

	if binaryOnce.err != nil {
		t.Fatalf("%v\n\nIf this is a go:embed failure, run `make ui` first.", binaryOnce.err)
	}
	return binaryOnce.path
}

func repoRoot() string {
	// This file is test/e2e/harness_test.go, so the root is two levels up.
	// Deriving it beats an environment variable nobody remembers to set.
	wd, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("e2e: getwd: %v", err))
	}
	return filepath.Dir(filepath.Dir(wd))
}

func isWindows() bool { return os.PathSeparator == '\\' }

// options configure a deployment.
type options struct {
	policy     string
	usage      fakebedrock.Usage
	frameDelay time.Duration
}

type option func(*options)

// withFrameDelay spaces the events of a streamed response, so a test can watch
// one arrive rather than only see that it did.
func withFrameDelay(d time.Duration) option { return func(o *options) { o.frameDelay = d } }

// withUsage sets the accounting every invocation reports.
func withUsage(u fakebedrock.Usage) option { return func(o *options) { o.usage = u } }

// newDeployment brings up a whole Panorama: schema, admin, catalogue, policy,
// and the server, with a fake Bedrock behind it.
//
// The server runs as a subprocess of the built binary rather than in-process.
// That is the point of this suite: it proves the artifact loads its own
// configuration, serves its own embedded console, and reaches the provider
// through the code path a deployment actually takes.
func newDeployment(t *testing.T, opts ...option) *deployment {
	t.Helper()

	conf := options{policy: "policy.yaml", usage: fakebedrock.DefaultUsage}
	for _, opt := range opts {
		opt(&conf)
	}

	fake := fakebedrock.New(
		fakebedrock.WithUsage(conf.usage),
		fakebedrock.WithFrameDelay(conf.frameDelay),
	)
	fakeSrv := httptest.NewServer(fake)
	t.Cleanup(fakeSrv.Close)

	dir := t.TempDir()
	port := freePort(t)

	d := &deployment{
		t:       t,
		binary:  panoramaBinary(t),
		dir:     dir,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		Fake:    fake,
		log:     &lockedBuffer{},
	}

	// SQLite rather than Postgres: it needs no service, it is the same pure-Go
	// driver the shipped binary uses, and it works unchanged on a Windows
	// runner. ToSlash because a backslash in a file: URI is not a separator.
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "panorama.db")) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"

	d.env = append(baseEnv(),
		"PANORAMA_SERVER_ADDR="+fmt.Sprintf("127.0.0.1:%d", port),
		"PANORAMA_SERVER_BASE_URL="+d.baseURL,
		"PANORAMA_DATABASE_DRIVER=sqlite",
		"PANORAMA_DATABASE_DSN="+dsn,
		"PANORAMA_DATABASE_AUTO_MIGRATE=true",
		"PANORAMA_LOG_FORMAT=text",
		"PANORAMA_LOG_LEVEL=info",
		// Seeds the bedrock provider on the first command that opens the
		// database, which is how a compose deployment arrives configured.
		// bedrock-mantle is configured below through the CLI, which is the
		// same function the console's Settings screen calls.
		"PANORAMA_BEDROCK_REGION=us-east-1",
		// The shortest reload the configuration permits, so a budget written
		// mid-test binds on the next request rather than at the end of a
		// default cycle. Anything under a second is refused outright, which is
		// the right call for a deployment and the floor for this suite.
		"PANORAMA_POLICY_RELOAD_INTERVAL=1s",
		// The rate table is the compiled-in seed. Refreshing it would reach the
		// real AWS Price List API, which is the one thing this suite must not
		// do - and the seed is what makes an expected cost computable.
		"PANORAMA_PRICING_REFRESH_INTERVAL=0",
		// Discovery is run explicitly below; the periodic job would only repeat
		// it against the same fixture.
		"PANORAMA_PROVIDERS_DISCOVERY_INTERVAL=-1s",
		"AWS_ENDPOINT_URL_BEDROCK_RUNTIME="+fakeSrv.URL,
		"AWS_ENDPOINT_URL_BEDROCK="+fakeSrv.URL,
		// bedrock-mantle has no SDK client, so the provider reads the
		// conventional variable itself - and warns that it is set.
		"AWS_ENDPOINT_URL_BEDROCK_MANTLE="+fakeSrv.URL,
	)
	d.run("migrate", "up")
	d.enableCLISignIn(dsn)
	d.token = d.mintToken(adminEmail, "admin")
	d.run("providers", "set", "bedrock-mantle", "-region", "us-east-1")
	d.run("models", "discover")
	d.run("policy", "apply", "-f", filepath.Join(repoRoot(), "test", "e2e", "fixtures", conf.policy))

	d.start()
	return d
}

// baseEnv strips the machine's AWS configuration and puts dummy credentials in
// its place.
//
// The SDK still signs - that path is under test - but a developer's real
// profile must never decide what these tests talk to, and an SSO profile with
// an expired token would fail them for a reason that has nothing to do with the
// change being tested.
func baseEnv() []string {
	keep := make([]string, 0, 16)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "AWS_") || strings.HasPrefix(upper, "PANORAMA_") {
			continue
		}
		// PWD is dropped rather than inherited. Setting cmd.Dir changes a
		// child's working directory but leaves PWD naming the parent's, and a
		// tool that reads the variable instead of calling getcwd then reports
		// the wrong repository - which looked like a product bug until the
		// request body said "Working directory: <this test's own package>".
		if upper == "PWD" || upper == "OLDPWD" || upper == "INIT_CWD" {
			continue
		}
		keep = append(keep, kv)
	}
	return append(keep,
		"AWS_REGION=us-east-1",
		"AWS_ACCESS_KEY_ID=fake",
		"AWS_SECRET_ACCESS_KEY=fake",
		"AWS_EC2_METADATA_DISABLED=true",
	)
}

// run executes an panorama subcommand against this deployment's database and
// fails the test if it does not succeed.
func (d *deployment) run(args ...string) string {
	d.t.Helper()
	out, err := d.tryRun(args...)
	if err != nil {
		d.t.Fatalf("panorama %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (d *deployment) tryRun(args ...string) (string, error) {
	d.t.Helper()

	ctx, cancel := context.WithTimeout(d.t.Context(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.binary, args...)
	cmd.Dir = d.dir
	cmd.Env = d.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// enableCLISignIn switches on the credential path this suite tests.
//
// It ships off: the release does not hand `panorama login` to engineers yet, so
// a deployment out of the box refuses the device flow and the console never
// mentions it. The flow itself is built and tested, and this is what tests it -
// so the suite turns the switch on for its own deployment, by the one route
// that can while the console refuses the write.
//
// A direct row rather than a CLI subcommand or an API call, deliberately. The
// point of the release constant is that an administrator has no way to say yes
// yet, and a command that said it for them would be that way. What is left is a
// database write by something that already owns the database, which is what
// this is - and it means the day the constant flips, nothing here changes.
//
// Written before the server starts, so no request ever sees it change.
func (d *deployment) enableCLISignIn(dsn string) {
	d.t.Helper()

	db, err := store.Open(d.t.Context(), config.Database{Driver: "sqlite", DSN: dsn})
	if err != nil {
		d.t.Fatalf("opening the deployment database to switch command-line sign-in on: %v", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			d.t.Errorf("closing the database: %v", cerr)
		}
	}()

	if serr := db.SetSetting(d.t.Context(), "cli.sign_in", "true"); serr != nil {
		d.t.Fatalf("switching command-line sign-in on: %v", serr)
	}
}

var tokenPattern = regexp.MustCompile(`pnt_[A-Za-z0-9_-]{20,}`)

// mintToken creates an account and its credential in one step.
//
// This is the whole bootstrap: no identity provider, no browser, no setup code.
// `panorama login` is a device flow that waits for a human to approve it in a
// console, which is correct for a laptop and unusable here.
func (d *deployment) mintToken(email, role string) string {
	d.t.Helper()

	out := d.run("token", "create", "-email", email, "-role", role)
	token := tokenPattern.FindString(out)
	if token == "" {
		d.t.Fatalf("no gateway token in the output of `token create`:\n%s", out)
	}
	return token
}

// start runs the gateway and waits for it to report ready.
func (d *deployment) start() {
	d.t.Helper()

	cmd := exec.Command(d.binary, "serve")
	cmd.Dir = d.dir
	cmd.Env = d.env
	cmd.Stdout = d.log
	cmd.Stderr = d.log

	if err := cmd.Start(); err != nil {
		d.t.Fatalf("starting the gateway: %v", err)
	}
	d.cmd = cmd

	d.t.Cleanup(d.stop)
	d.t.Cleanup(func() {
		if d.t.Failed() {
			// Which side lost the bytes, printed for every failing test rather
			// than only where an assertion thought to ask. A leg driving a real
			// coding tool fails inside the tool, long before it reaches
			// requireServed, and the gateway log alone cannot say whether a
			// short upstream stream is one the fake never finished writing.
			completed, failures := d.Fake.Streams()
			d.t.Logf("fake bedrock: %d stream(s) written in full, %d stopped early %v",
				completed, len(failures), failures)
			d.t.Logf("gateway log:\n%s", d.log.String())
		}
	})

	d.waitReady()
}

func (d *deployment) stop() {
	if d.cmd == nil || d.cmd.Process == nil || d.stopped {
		return
	}
	d.stopped = true
	_ = d.cmd.Process.Kill()
	_, _ = d.cmd.Process.Wait()
}

// waitReady polls /readyz, which reports false while migrations are pending.
func (d *deployment) waitReady() {
	d.t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if d.cmd.ProcessState != nil {
			d.t.Fatalf("the gateway exited before becoming ready:\n%s", d.log.String())
		}
		resp, err := http.Get(d.baseURL + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	d.t.Fatalf("the gateway was not ready within a minute:\n%s", d.log.String())
}

// databaseContains reports whether raw appears anywhere in the deployment's
// storage.
//
// It reads the SQLite file and its write-ahead log as bytes rather than
// querying, because the claim is about what is on disk. A column nobody thought
// to check, a log the store wrote, or a value inside a JSON blob would all be
// missed by a query and are all found here.
func (d *deployment) databaseContains(raw string) bool {
	d.t.Helper()

	matches, err := filepath.Glob(filepath.Join(d.dir, "panorama.db*"))
	if err != nil {
		d.t.Fatalf("looking for the database: %v", err)
	}
	if len(matches) == 0 {
		d.t.Fatal("no database file was found; the check would pass vacuously")
	}

	needle := []byte(raw)
	for _, name := range matches {
		content, rerr := os.ReadFile(name)
		if rerr != nil {
			d.t.Fatalf("reading %s: %v", name, rerr)
		}
		if bytes.Contains(content, needle) {
			return true
		}
	}
	return false
}

// freePort finds a port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("the probe listener is not TCP: %T", l.Addr())
	}
	port := addr.Port
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the probe listener: %v", err)
	}
	return port
}
