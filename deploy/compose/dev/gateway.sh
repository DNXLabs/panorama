#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

#
# Entrypoint for the development stack's gateway container.
#
# Three jobs, in order: make sure the embed directive has something to compile
# against, make sure the file watcher is installed, then hand off to it.
#
# Run inside golang:*-trixie - see deploy/compose/docker-compose.dev.yml.

set -euo pipefail

# Pinned, because `go install` refuses a module whose go directive is newer
# than the image's toolchain. v1.67.4 requires go 1.26.0, which is the Go
# version this repository targets. When an air release raises its directive
# past GO_VERSION, stay on the last one that builds; raise it with GO_VERSION.
AIR_VERSION="${AIR_VERSION:-v1.67.4}"
AIR_BIN="${GOBIN:-/gocache/bin}/air"
DIST="/src/ui/dist"

# go:embed refuses to compile against an empty directory, so a source build
# needs ui/dist to hold something even though nothing in this stack serves it:
# the console you open is Vite's, on :5173, with hot reload. Running `make ui`
# here would put a 20-second Vite build in front of every `docker compose up`
# for a bundle no browser in this stack loads.
#
# So a placeholder, written only when there is no build there already - if you
# have run `make ui`, that output is left exactly as it is. The page says where
# the real console is rather than being blank, because port 8088 is the one
# somebody reaches for out of habit.
#
# No inline style or script: this is served under the console's CSP, which
# permits neither, and a placeholder that itself trips the policy would be a
# confusing first impression.
if [ ! -f "$DIST/index.html" ]; then
  echo "panorama-dev: ui/dist has no build; writing a placeholder for go:embed" >&2
  mkdir -p "$DIST"
  cat > "$DIST/index.html" <<'HTML'
<!doctype html>
<meta charset="utf-8">
<title>Panorama - development stack</title>
<h1>The console is not served from this port in development.</h1>
<p>Open <a href="http://localhost:5173/">http://localhost:5173/</a>. Vite serves
the console there with hot reload and proxies the API back to this port.</p>
<p>This placeholder exists because <code>go:embed</code> will not compile against
an empty <code>ui/dist</code>. <code>make ui</code> replaces it with the real
build.</p>
HTML
fi

# A named profile with no configuration behind it, caught here rather than
# fifteen seconds later as `failed to get shared config profile, <name>` - which
# names the profile and so reads as a broken profile, when the actual fault is
# that this container cannot see your AWS configuration at all. Fatal either
# way: the gateway refuses to start without the credentials it was told to use,
# so this only decides which sentence you get.
if [ -n "${AWS_PROFILE:-}" ] && [ ! -r "$HOME/.aws/config" ] && [ ! -r "$HOME/.aws/credentials" ]; then
  echo "panorama-dev: AWS_PROFILE is $AWS_PROFILE, but $HOME/.aws holds no config or" >&2
  echo "  credentials file. Your ~/.aws is mounted there by docker-compose.dev.yml -" >&2
  echo "  check that it is, and that the profile is in one of those two files." >&2
  echo "  Unset AWS_PROFILE in deploy/compose/.env to run without Bedrock." >&2
  exit 1
fi

# The watcher, installed once into the build cache the Makefile already mounts,
# so it survives `docker compose down` and is not re-downloaded on every start.
if [ ! -x "$AIR_BIN" ]; then
  echo "panorama-dev: installing air $AIR_VERSION" >&2
  if ! GOBIN="${GOBIN:-/gocache/bin}" go install "github.com/air-verse/air@$AIR_VERSION"; then
    echo "panorama-dev: could not install the file watcher. If it refused the Go" >&2
    echo "  version, AIR_VERSION needs to move with GO_VERSION - see the note above." >&2
    exit 1
  fi
fi

# Warm the module cache before handing over. Doing it here means a missing
# dependency is reported as itself, rather than as a build failure inside the
# watcher's first rebuild.
go mod download

echo "panorama-dev: watching cmd/ and internal/ - edit a .go file and this restarts" >&2
exec "$AIR_BIN" -c /src/deploy/compose/dev/air.toml
