# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

# Panorama build targets.
#
# Everything runs in a container, so Docker is the only prerequisite - no host
# Go toolchain required. If you have one and would rather use it, pass HOST=1:
#
#     make test HOST=1
#
# Caches live in ./.gocache so containers run as your user and nothing lands in
# the repository owned by root.

GO_VERSION   ?= 1.27.2
GO_IMAGE     ?= golang:$(GO_VERSION)-trixie
LINT_VERSION ?= v2.14.0
LINT_IMAGE   ?= golangci/golangci-lint:$(LINT_VERSION)
NODE_VERSION ?= 24
NODE_IMAGE   ?= node:$(NODE_VERSION)-trixie-slim

BIN     := bin/panorama
CLI_BIN := bin/panorama-cli
PKG     := github.com/dnxlabs/panorama
PORT    ?= 8088

# The host's own platform, for the client binary.
#
# `build` produces the deployable artifact, which is Linux because it is built
# in a container and lands in an image. The commands a developer runs on their
# own laptop - login, token, configure, whoami - have to run on that laptop, so
# they get their own target and their own GOOS. Go cross-compiles this for free:
# the production build is already CGO-free.
HOST_OS   := $(shell uname -s | tr '[:upper:]' '[:lower:]')
HOST_ARCH := $(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X '$(PKG)/internal/version.Version=$(VERSION)' \
	-X '$(PKG)/internal/version.Commit=$(COMMIT)' \
	-X '$(PKG)/internal/version.Date=$(DATE)'

CACHE_DIR   := $(CURDIR)/.gocache
DOCKER_USER ?= $(shell id -u):$(shell id -g)

DOCKER := docker run --rm \
	-v "$(CURDIR)":/src -w /src \
	-v "$(CACHE_DIR)":/gocache \
	-u $(DOCKER_USER) \
	-e HOME=/gocache \
	-e GOMODCACHE=/gocache/mod \
	-e GOCACHE=/gocache/build \
	-e GOLANGCI_LINT_CACHE=/gocache/lint

# The console is a React application built by Vite, embedded with go:embed. Its
# output is not committed, so every Go command depends on `ui` - including
# `test`, because the embed directive will not compile against an empty
# directory. `go build ./...` on its own therefore does not work from a fresh
# clone; that is the cost of not putting a minified bundle in every diff.
NODE := docker run --rm \
	-v "$(CURDIR)/ui":/ui -w /ui/app \
	-u $(DOCKER_USER) \
	-e HOME=/nodecache \
	-e npm_config_cache=/nodecache/npm \
	-v "$(CACHE_DIR)/node":/nodecache \
	$(NODE_IMAGE)

# The documentation site. Unlike the console, it is not embedded in the binary
# and nothing in Go depends on it, so it is absent from `check` and from every
# build target: a typo in a heading should not fail the gateway's CI.
#
# It mounts the repository root rather than docs/, because the reference pages
# are generated from config.example.yaml and policy.example.yaml, which sit
# above it. That is the whole point - a configuration key and the page
# documenting it cannot drift when one is generated from the other.
DOCS_NODE := docker run --rm \
	-v "$(CURDIR)":/src -w /src/docs \
	-u $(DOCKER_USER) \
	-e HOME=/nodecache \
	-e npm_config_cache=/nodecache/npm \
	-v "$(CACHE_DIR)/node":/nodecache \
	$(NODE_IMAGE)

# The development stack: the gateway and the console both rebuilt from the
# working tree as it changes. Nothing here builds or runs the production image.
#
# One compose file and no overlay, so `docker compose -f
# deploy/compose/docker-compose.dev.yml up` is the same stack this runs. An
# overlay the Makefile added and the compose file did not mention would work
# under `make dev` and be silently missing under the command everyone reaches
# for - which is exactly how ~/.aws went missing once already.
DEV := PANORAMA_DEV_USER=$(DOCKER_USER) docker compose -f deploy/compose/docker-compose.dev.yml

ifeq ($(HOST),1)
  GO      := go
  GO_TEST := go
  LINT    := golangci-lint
  NPM     := cd ui/app && npm
  DOCS_NPM := cd docs && npm
else
  # The production build is CGO-free so the binary stays a single static
  # artifact. The race detector needs cgo, so tests get their own invocation.
  GO      := $(DOCKER) -e CGO_ENABLED=0 $(GO_IMAGE) go
  GO_TEST := $(DOCKER) -e CGO_ENABLED=1 $(GO_IMAGE) go
  LINT    := $(DOCKER) $(LINT_IMAGE) golangci-lint
  NPM     := $(NODE) npm
  DOCS_NPM := $(DOCS_NODE) npm
endif

.DEFAULT_GOAL := help
.PHONY: help build cli test e2e e2e-live lint fmt tidy tidy-check license-check check run image migrate-up migrate-status shell clean cache ui ui-install ui-dev docs docs-install docs-dev docs-diagrams dev dev-down dev-logs dev-restart dev-shell dev-psql dev-seed fake-bedrock

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

cache:
	@mkdir -p "$(CACHE_DIR)" "$(CACHE_DIR)/node"

ui-install: cache ## Install the console's dependencies
	$(NPM) install --no-audit --no-fund

ui: ui-install ## Build the console into ui/dist
	$(NPM) run build

docs-install: cache
	$(DOCS_NPM) install --no-audit --no-fund

docs: docs-install ## Build the documentation site into docs/dist
	$(DOCS_NPM) run build

# The diagrams are drawn by mingrammer's `diagrams` from docs/diagrams/*.py, and
# the PNGs they write are committed - the same trade docs/csp-hashes.json makes,
# so that `make docs` needs neither Python nor Graphviz. Run this after changing
# a diagram's source, look at the result, and commit both.
#
# The image is built inline rather than kept in a Dockerfile of its own: it is
# three lines, nothing else uses it, and the layer cache makes the second run
# as fast as a file would. fonts-liberation is there because the diagrams ask
# for Helvetica and a slim Debian image has no sans-serif at all - without it
# Graphviz falls back to a bitmap face and every label is drawn in it.
DIAGRAMS_IMAGE := panorama-docs-diagrams

docs-diagrams: ## Redraw docs/src/assets/diagrams from docs/diagrams/*.py
ifeq ($(HOST),1)
	cd docs/diagrams && for f in request_flow.py aws_architecture.py; do \
		uv run --quiet --with diagrams --with pillow python3 $$f || exit 1; done
else
	printf 'FROM python:3.13-slim\nRUN apt-get update && apt-get install -y --no-install-recommends graphviz fonts-liberation && rm -rf /var/lib/apt/lists/*\nRUN pip install --no-cache-dir diagrams pillow\n' \
		| docker build -q -t $(DIAGRAMS_IMAGE) - >/dev/null
	docker run --rm -v "$(CURDIR)":/src -w /src/docs/diagrams -u $(DOCKER_USER) \
		-e HOME=/tmp $(DIAGRAMS_IMAGE) \
		sh -c 'for f in request_flow.py aws_architecture.py; do python3 $$f || exit 1; done'
endif
	@ls -1 docs/src/assets/diagrams/

# A published port rather than `--network host`, which is what `ui-dev` uses.
# The console's dev server needs the host's network because it proxies to a
# gateway running on it; this site talks to nothing, so it gains only the
# host's port collisions, such as `make dev` holding 5173, which is Vite's
# default and so where Astro lands if its port setting is lost. Mapping one port also
# makes DOCS_PORT mean the same thing inside the container and out.
DOCS_PORT ?= 4321

docs-dev: docs-install ## Run the documentation site with hot reload (DOCS_PORT=4321)
	@echo "docs on http://localhost:$(DOCS_PORT)/docs/"
	docker run --rm -it \
		-v "$(CURDIR)":/src -w /src/docs \
		-u $(DOCKER_USER) -e HOME=/nodecache -e npm_config_cache=/nodecache/npm \
		-v "$(CACHE_DIR)/node":/nodecache \
		-e DOCS_PORT=$(DOCS_PORT) \
		-p $(DOCS_PORT):$(DOCS_PORT) \
		$(NODE_IMAGE) npm run dev

ui-dev: ui-install ## Run the console's dev server against a local `make run`
	@echo "console on http://localhost:5173, proxying to http://localhost:8088"
	docker run --rm -it \
		-v "$(CURDIR)/ui":/ui -w /ui/app \
		-u $(DOCKER_USER) -e HOME=/nodecache -e npm_config_cache=/nodecache/npm \
		-v "$(CACHE_DIR)/node":/nodecache \
		--network host \
		$(NODE_IMAGE) npm run dev -- --host

build: cache ui ## Compile the binary into ./bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/panorama
	@echo "built $(BIN) - $(VERSION) ($(COMMIT)), for linux/$(HOST_ARCH)"
	@echo "this is the deployable artifact. For the commands you run on this machine: make cli"

cli: cache ui ## Compile the client binary for THIS machine into ./bin
	$(DOCKER) -e CGO_ENABLED=0 -e GOOS=$(HOST_OS) -e GOARCH=$(HOST_ARCH) $(GO_IMAGE) \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $(CLI_BIN) ./cmd/panorama
	@echo "built $(CLI_BIN) for $(HOST_OS)/$(HOST_ARCH)"
	@echo "put it on your PATH:  sudo install $(CLI_BIN) /usr/local/bin/panorama"

test: cache ui ## Run the test suite with the race detector
	$(GO_TEST) test -race -count=1 ./...

# The end-to-end suite: the built binary, a real database, and a fake Bedrock
# speaking the wire protocol, driven by whichever coding tools are installed.
#
# Not part of `check`. It builds the binary, binds ports and runs real coding
# agents, which is minutes rather than seconds - and the legs that need a tool
# skip cleanly when it is absent, so this is still useful on a machine with only
# a Go toolchain.
#
#     make e2e
#     make e2e ARGS='-run TestClaudeCode'
e2e: cache ui ## Run the end-to-end suite against a fake Bedrock
	$(GO_TEST) test -tags e2e -count=1 -timeout 20m $(ARGS) ./test/e2e/...

# The narrow set that talks to real AWS. It exists for the handful of claims a
# fake cannot settle - whether these credentials can invoke a model at all, what
# the catalogue and the price list actually say today, and which beta flags
# Bedrock still refuses. Every request it makes asks for one token.
e2e-live: cache ui ## Run the tests that need real AWS credentials
	@if [ -z "$(PANORAMA_TEST_AWS_REGION)" ]; then \
		echo "set PANORAMA_TEST_AWS_REGION to the region to test against"; exit 1; \
	fi
	$(GO_TEST) test -tags e2e,live -count=1 -timeout 20m $(ARGS) ./test/e2e/... ./internal/providers/bedrock/...

fake-bedrock: cache ## Serve Bedrock's wire protocol locally, for a stack with no AWS
	@echo "point a gateway at it with:"
	@echo "  AWS_ENDPOINT_URL_BEDROCK_RUNTIME=http://127.0.0.1:9099 AWS_ENDPOINT_URL_BEDROCK=http://127.0.0.1:9099"
	$(DOCKER) -p 9099:9099 $(GO_IMAGE) go run ./cmd/fakebedrock -addr 0.0.0.0:9099

lint: cache ui ## Run golangci-lint
	$(LINT) run

fmt: cache ## Format the source
	$(GO) fmt ./...

tidy: cache ## Update go.mod and go.sum
	$(GO) mod tidy

# Every tracked file carries its licence: an SPDX header in a source file, an
# entry in REUSE.toml for anything that cannot hold one. Always in a container,
# HOST=1 or not: the REUSE tool is Python and nothing else here needs it. The
# git directory is mounted at its own path so that reuse can ask git which files
# are tracked from a worktree too, whose .git is a file naming that path; without
# it reuse walks the disk and reports .gocache and node_modules.
GIT_COMMON_DIR = $(shell cd "$$(git rev-parse --git-common-dir)" && pwd)
license-check: ## Fail if a file has no SPDX licence or copyright (reuse lint)
	docker run --rm -v "$(CURDIR)":/data -v "$(GIT_COMMON_DIR)":"$(GIT_COMMON_DIR)" fsfe/reuse:5 lint

tidy-check: cache ## Fail if go.mod or go.sum is not tidy
	@cp go.mod .go.mod.bak && cp go.sum .go.sum.bak
	@$(GO) mod tidy
	@if ! diff -q go.mod .go.mod.bak >/dev/null || ! diff -q go.sum .go.sum.bak >/dev/null; then \
		mv .go.mod.bak go.mod; mv .go.sum.bak go.sum; \
		echo "go.mod or go.sum is not tidy - run 'make tidy'"; exit 1; \
	fi
	@rm -f .go.mod.bak .go.sum.bak

check: tidy-check license-check lint test build ## Everything CI runs

run: cache ui ## Run the server against SQLite on $(PORT)
	$(DOCKER) -p $(PORT):8088 \
		-e PANORAMA_SERVER_ADDR=:8088 \
		-e PANORAMA_SERVER_BASE_URL=http://localhost:$(PORT) \
		-e PANORAMA_LOG_FORMAT=text \
		-e CGO_ENABLED=0 \
		$(GO_IMAGE) go run ./cmd/panorama serve

dev: cache ## Start the live-reloading development stack
	@echo "console  http://localhost:5173   (hot reload)"
	@echo "gateway  http://localhost:8088   (rebuilt and restarted on a .go change)"
	@echo "postgres localhost:5432"
	$(DEV) up --remove-orphans

dev-down: ## Stop the development stack, keeping its database
	$(DEV) down

dev-logs: ## Follow the gateway's log
	$(DEV) logs -f panorama

dev-restart: ## Restart the gateway - for a go.mod or compose change
	$(DEV) restart panorama

dev-shell: ## Open a bash prompt inside the running gateway container
	$(DEV) exec panorama bash

dev-seed: ## Fill the development database with fake analytics traffic (ARGS=-reset to replace it)
	@# Runs inside the gateway container, which already has the toolchain, the
	@# module cache and a route to Postgres. `make dev` must be up.
	$(DEV) exec -T panorama go run ./scripts/seed $(ARGS)

dev-psql: ## Open psql against the development stack's database
	@# Credentials read from the container's own environment, so overriding
	@# POSTGRES_USER in .env does not leave this target pointing at a role that
	@# does not exist.
	$(DEV) exec postgres sh -c 'exec psql -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"'


# Which release's found price table a source build embeds: latest, a tag such as
# v1.4.0, or none. See the Dockerfile.
FOUND_PRICES ?= latest
image: ## Build the container image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		--build-arg FOUND_PRICES=$(FOUND_PRICES) \
		-t panorama:$(VERSION) -t panorama:latest .

migrate-up: cache ui ## Apply pending migrations
	$(GO) run ./cmd/panorama migrate up

migrate-status: cache ui ## Show migration state
	$(GO) run ./cmd/panorama migrate status

shell: cache ## Open a shell in the build container
	$(DOCKER) -it $(GO_IMAGE) bash

clean: ## Remove build output and caches
	rm -rf bin dist ui/dist ui/app/node_modules
	@# The module cache is written read-only by Go; make it removable first.
	-chmod -R u+w "$(CACHE_DIR)" 2>/dev/null || true
	rm -rf "$(CACHE_DIR)"
