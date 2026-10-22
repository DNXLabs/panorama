# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

ARG GO_VERSION=1.27.3
ARG NODE_VERSION=24
# Declared up here because an ARG used in a FROM must be in the global scope;
# the reasoning for the value is at the runtime stage below.
ARG RUNTIME_BASE=gcr.io/distroless/static-debian13:debug-nonroot

# The console is built first, because the Go binary embeds it. Its output is not
# committed, so there is no version of this image that could be built without
# this stage - which is the point: a binary that shipped without a console is a
# blank page for whoever deployed it.
# Pinned to the builder's own platform. Its output is JavaScript, which is the
# same bytes whatever runs it, so building it once for amd64 and again emulated
# for arm64 would cost minutes to produce an identical ui/dist.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-trixie-slim AS console

WORKDIR /ui/app

# The manifest first, so a change to a screen does not re-resolve the dependency
# graph.
COPY ui/app/package.json ui/app/package-lock.json* ./
RUN npm install --no-audit --no-fund

COPY ui/design-tokens.css /ui/design-tokens.css
COPY ui/app/ ./
RUN npm run build


# Also the builder's own platform, which turns a multi-platform build into a
# cross-compile rather than an emulation. Go cross-compiles for free here -
# CGO is already off and the stage already reads TARGETOS and TARGETARCH below -
# so the arm64 image is built by an amd64 toolchain at native speed. Without
# this, buildx runs the whole Go compile under QEMU for the non-native
# platform, which takes minutes rather than seconds and is the usual reason a
# multi-arch build gets abandoned.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=console /ui/dist ./ui/dist

# The found price table: rates for models the AWS Price List API does not
# publish (OpenAI's GPT-5.x on Bedrock), which the Prices workflow finds and
# keeps outside the repository. The repository's copy is empty on purpose. A
# release build replaces it before this stage runs; a build from source
# fetches the one attached to a published release, so that it prices the same
# models a published image does.
#
#   FOUND_PRICES=latest    the latest release's table (the default)
#   FOUND_PRICES=v1.4.0    that release's table, for a reproducible build
#   FOUND_PRICES=none      no table: those models are unpriced, and counted
#                          as unpriced beside every total
#
# A fetch that fails stops the build rather than quietly embedding nothing,
# because an unpriced model looks like a cheaper deployment. The binary logs
# the digest of the table it embedded at startup, as found_table.
ARG FOUND_PRICES=latest
ARG FOUND_PRICES_REPOSITORY=dnxlabs/panorama
RUN set -eu; \
    if ! grep -Eq '"entries": *\[\]' internal/pricing/found_prices.json; then \
      echo "found prices: using the table already in the tree"; \
    else \
      case "$FOUND_PRICES" in \
        none) echo "found prices: none, so models the Price List API does not publish will be unpriced"; exit 0 ;; \
        latest) url="https://github.com/${FOUND_PRICES_REPOSITORY}/releases/latest/download/found_prices.json" ;; \
        v*) url="https://github.com/${FOUND_PRICES_REPOSITORY}/releases/download/${FOUND_PRICES}/found_prices.json" ;; \
        *) echo "FOUND_PRICES is latest, none or a release tag such as v1.4.0, not '${FOUND_PRICES}'" >&2; exit 1 ;; \
      esac; \
      if ! curl -fsSL --retry 3 -o /tmp/found_prices.json "$url"; then \
        echo "could not fetch the found price table from $url" >&2; \
        echo "build with --build-arg FOUND_PRICES=none to embed no table, or name a release with FOUND_PRICES=v<version>" >&2; \
        exit 1; \
      fi; \
      go run ./cmd/pricefinder -check /tmp/found_prices.json; \
      cp /tmp/found_prices.json internal/pricing/found_prices.json; \
    fi

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# Supplied by BuildKit when building for a specific platform.
ARG TARGETOS=linux
ARG TARGETARCH

# CGO off keeps the binary static - pure-Go SQLite is what makes that possible -
# so it runs on a distroless base with no libc at all.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X 'github.com/dnxlabs/panorama/internal/version.Version=${VERSION}' \
        -X 'github.com/dnxlabs/panorama/internal/version.Commit=${COMMIT}' \
        -X 'github.com/dnxlabs/panorama/internal/version.Date=${DATE}'" \
      -o /out/panorama ./cmd/panorama

# The runtime image has no shell, so the data directory has to be created here
# with the right ownership and copied across.
RUN mkdir -p /data


# The runtime base, as a build argument so it can be changed without editing
# this file.
#
# `debug-nonroot` is the same distroless image plus busybox: a shell at
# /busybox/sh and ~380 applets, same nonroot uid, same absence of a package
# manager. It is here because without a shell there is nothing to
# `aws ecs execute-command` into - the exec succeeds and then fails with
# "executable file not found", which reads like a broken agent rather than an
# image with no /bin/sh.
#
# What it costs is honest: a shell in the runtime image is a shell an attacker
# who reaches RCE also has. That is a real widening for a product whose pitch is
# governance, and the reason this is an ARG rather than a hardcoded base -
#
#     docker build --build-arg RUNTIME_BASE=gcr.io/distroless/static-debian13:nonroot
#
# builds the shell-less image, and that is what GA should ship once the
# deployment is not being operated by the people who wrote it.
ARG RUNTIME_BASE
FROM ${RUNTIME_BASE}

# PATH is left alone: the debug base already carries /busybox on it, and adding
# it again would leave a dangling entry on an image built without the shell.

# What the image is, under what licence, and where its source is.
# `licenses` and `source` together are the offer of source AGPL-3.0-only
# obliges, made to whoever holds the image, and `revision` names the commit,
# so the offer is of exactly what was built. A fork that publishes its own
# image changes `source` to its own repository.
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="Panorama" \
      org.opencontainers.image.description="An AI gateway that connects coding-AI spend to real work" \
      org.opencontainers.image.vendor="DNX Solutions" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/dnxlabs/panorama" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

COPY --from=build /out/panorama /usr/local/bin/panorama
COPY --from=build --chown=nonroot:nonroot /data /data

# The default SQLite DSN is relative, so it lands in the mounted volume.
WORKDIR /data
VOLUME ["/data"]

USER nonroot:nonroot
EXPOSE 8088

# No HEALTHCHECK: this image has no shell or HTTP client to run one with, by
# design. Probe GET /healthz and GET /readyz over HTTP from the orchestrator -
# a Kubernetes httpGet probe or a load balancer health check.
ENTRYPOINT ["/usr/local/bin/panorama"]
CMD ["serve"]
