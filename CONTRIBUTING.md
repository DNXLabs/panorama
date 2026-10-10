# Contributing to Panorama

Thanks for considering it. Panorama is pre-1.0, so the
highest-value contributions are review, design feedback, and issues that catch a bad
decision before it sets.

Read [`ARCHITECTURE.md`](ARCHITECTURE.md) first. It covers how the pieces fit and the
decisions where the obvious alternative is wrong for a non-obvious reason.

## Before your first pull request

Read the [Contributor License Agreement](CLA.md). **Submitting a pull request means
you accept it**, for that contribution and every later one; there is nothing to sign.
It lets DNX distribute your contribution under the AGPL and under a commercial
licence, and you keep the copyright. If you are contributing for your employer, make
sure you are allowed to before you open the pull request.

## Development setup

You do not need Go installed. Every Go command runs inside a container, so Docker is
the only prerequisite.

```sh
git clone git@github.com:dnxlabs/panorama.git
cd panorama
make check     # tidy-check + lint + test + build — run this before pushing
make dev       # the live-reloading stack: console on :5173, gateway on :8088
```

`make dev` is where the day is spent. It runs the gateway from source against
Postgres and serves the console from Vite, and both pick your edits up on their
own: a change under `ui/app/` patches the page in the browser, a change under
`cmd/` or `internal/` rebuilds and restarts the gateway. Open
<http://localhost:5173> — that is the console; point a coding tool at `:8088`.

The containers are the Go and Node images rather than the distroless one the
project ships, so they have a shell: `make dev-shell` puts you at a bash prompt
inside the gateway with the source mounted at `/src`, and `make dev-psql` opens
the database.

Two things it deliberately does not notice, because both are a command rather
than an edit: a change to `go.mod` and a change to `ui/app/package.json`. Run
`make dev-restart` after either.

Your `~/.aws` is mounted read-only, so `AWS_PROFILE` in `deploy/compose/.env`
works the same inside the container as it does on your machine — SSO profiles
included, once you have run `aws sso login`. If you have no `~/.aws` at all the
container will not start and will say so; `mkdir -p ~/.aws` is the whole fix.
That is deliberate: Docker's default is to invent a missing bind source as an
empty directory, and an empty `~/.aws` fails later and much less clearly.

Useful targets:

| Target | What it does |
| ------ | ------------ |
| `make build` | Compile `./bin/panorama` for the host platform |
| `make test` | Run the test suite with the race detector |
| `make e2e` | Drive the built binary and real coding tools against a fake Bedrock |
| `make e2e-live` | The few checks that need real AWS credentials |
| `make fake-bedrock` | Serve Bedrock's wire protocol locally, for a stack with no AWS |
| `make lint` | Run `golangci-lint` |
| `make tidy` | Run `go mod tidy` |
| `make run` | Run the server against SQLite on `:8088` |
| `make dev` | The live-reloading development stack |
| `make dev-logs` | Follow the development gateway's log |
| `make dev-shell` | A bash prompt inside the development gateway |
| `make dev-psql` | `psql` against the development database |
| `make dev-down` | Stop the development stack |
| `make migrate-status` | Show migration state for the configured database |
| `make shell` | Drop into a shell in the Go build container |
| `make clean` | Remove build output and the module cache volume |

If you *do* have a Go toolchain and would rather use it directly, pass `HOST=1`:

```sh
make check HOST=1
```

That is the path CI takes, so it stays exercised.

## The conventions that matter

[`CLAUDE.md`](CLAUDE.md) is the full list, and [`ARCHITECTURE.md`](ARCHITECTURE.md)
explains the reasoning behind most of it. The ones that get pull requests sent back:

**No body storage, ever.** Prompt and response bodies are pattern-matched in flight and
discarded. Nothing that persists a request or response body will be merged into the
open-source edition. This is a product commitment, not a configuration option.

**No fallbacks that mask errors.** Log the error and return a structured failure. The
one sanctioned fallback is a genuine service interruption — Bedrock throttling into a
fallback chain. "Try the other thing so the test passes" is not a fallback, it is a
hidden bug.

**Streaming is sacred.** Nothing on the hot path may buffer a stream, block, or add
meaningful latency. Attribution extraction on the request path is inline regex and
structural matching only; anything more expensive runs asynchronously against
already-extracted metadata.

**A developer sees only their own numbers.** Named per-person spend is visible to
administrators and to managers of that person's teams, and to nobody else. No UI, API
or export ranks one named person against another. `filterFrom` in
`internal/controlplane/api.go` is the single chokepoint — do not add a second.

**Allowlists over denylists** for anything crossing a trust boundary.

**Every metric and log line carries** `userId`, `teamId`, `sessionId`, `model`,
`provider`, and `tool` where they are known.

## Pull requests

- A large change lands as a series of reviewed pull requests, not a single commit dump.
- Tests come with the change, not after it.
- `make e2e` is not part of `make check` and does not need to pass before every
  push — it builds the binary, binds ports and runs real coding agents. The legs
  that need Claude Code or opencode skip when the tool is absent, so it is still
  worth running locally on anything touching the proxy, the provider, the policy
  engine, or the configuration a tool is given.
- `make check` passes locally before you push. CI runs the same targets.
- Changes to the [Principles](README.md#principles) (trust, design, scope) need an
  explicit decision in an issue first, not a drive-by pull request.
- Never force-push a shared branch. If something wrong is committed, revert it.

## Reporting a security issue

Do not open a public issue. Report it privately through GitHub, as
[`SECURITY.md`](SECURITY.md) describes, and we will coordinate a fix and disclosure
with you.

## Licence

Contributions are accepted under AGPL-3.0-only with the relicensing grant described in
[`CLA.md`](CLA.md). Prose and images in the manual (`docs/src/content`, the
screenshots and diagrams) are CC BY 4.0.

Every new source file starts with an SPDX header, in the file's own comment
syntax:

```go
// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only
```

A file that cannot carry a comment (data, images, Markdown) is covered by a
path in [`REUSE.toml`](REUSE.toml). `make license-check` runs `reuse lint` and
fails on a file with neither; it is part of `make check`.
