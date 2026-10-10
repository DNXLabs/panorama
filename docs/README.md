# The documentation site

[panorama.dnx.ai/docs](https://panorama.dnx.ai/docs/). Astro + Starlight, built
here and published to the landing page's CloudFront distribution.

```sh
make docs-dev                  # http://localhost:4321/docs/, hot reload
make docs-dev DOCS_PORT=4400   # if 4321 is taken
make docs                      # build into docs/dist
```

`make docs HOST=1` uses your own Node instead of the container.

**The dev server is pinned and refuses to move.** `strictPort` is set, so if
the port is taken the server fails to start rather than quietly binding another
one and printing a URL nobody re-reads. It is also a published port rather than
`--network host`, which `ui-dev` uses: the console's dev server needs the
host's network because it proxies to a gateway running on it, and this site
talks to nothing, so host networking would only inherit the host's port
collisions, such as `make dev` holding 5173.

**The lockfile is generated in the container, not on a laptop.** A
macOS-generated one is missing the Linux platform binaries, so the container's
install rewrites it on every run, Vite sees "lockfile has changed" and restarts
mid-startup, and the restart races the listen. Regenerate it the same way if it
ever needs rebuilding:

```sh
docker run --rm -v "$PWD":/src -w /src/docs -u "$(id -u):$(id -g)" \
  -e HOME=/tmp -e npm_config_cache=/tmp/npm \
  node:24-trixie-slim npm install --no-audit --no-fund
```

Two warnings on every build are expected and explained where they originate:
the missing `i18n` collection (see `src/content.config.ts`) and the `/404`
route conflict, which is Starlight's own 404 route winning over the catch-all,
as it should.

## Why the docs live in this repository

The page describing a configuration key and the code reading it change in the
same pull request, or they drift. That is the whole argument, and it is why the
docs are not in the website repository even though they are served from the
website's domain.

The website stays private and owns `panorama.dnx.ai`. These docs are a second
origin on its distribution, under `/docs/*`. Nothing here runs CloudFormation.

## Versions

Three paths, and which one a build writes is decided by `DOCS_VERSION`.

| Path | `DOCS_VERSION` | What it is |
| --- | --- | --- |
| `/docs` | unset | The latest released series. The link that ends up in a README. |
| `/docs/vX.Y` | `v1.2` | That series, frozen at its release. Never rebuilt. |
| `/docs/next` | `next` | main, ahead of any release. |

A patch release does not get a path of its own: v1.2.0 and v1.2.3 are the same
documentation, so a patch overwrites its series.

**Which release moves `/docs`** is decided exactly as `release.yml` decides
which release moves the `stable` image tag, and for the same reason: a
backported v1.3.5 published after v1.4.0 must not drag the unversioned path
backwards. A prerelease never moves it.

**The version selector fetches its list at runtime** from
`/docs/versions.json`, rather than baking one in at build time. A frozen build
cannot know about versions published after it, and a list baked in would be
correct only on the newest version and quietly wrong on every older one, which
is exactly where somebody is looking for the way forward.

## What is generated, and what is written

Generated on every build by `scripts/generate-reference.mjs`, gitignored, and
never edited by hand:

- `reference/configuration.md`, from `../config.example.yaml`
- `reference/policy.md`, from `../policy.example.yaml`

There is no committed copy of either, so there is nothing that can go stale and
no check that could pass while it had. Adding a configuration key documents it
here by construction.

Everything else is written.

## The CSP, and the check that keeps it honest

The landing page sets `script-src 'self'` and carries no inline script.
Starlight cannot hold that line: it writes the reader's theme onto the document
before first paint, and the only way to do that without a flash of the wrong
theme is an inline script.

So `/docs/*` has its own response headers policy, which names the SHA-256
hashes of exactly what Starlight inlines. `npm run build` checks the built
output against `csp-hashes.json` as a postbuild step.

This matters because the failure is silent: an inline script missing from the
policy is blocked, the page still renders, and the theme toggle just stops
working for everybody. When the check fails after a Starlight upgrade:

```sh
node scripts/csp-hashes.mjs --write     # prints the replacement policy
```

then paste the printed policy into `DocsHeadersPolicy` in the website
repository's `template.yaml`, **and deploy that stack before deploying these
docs**.

## Deploying

CI does it. `.github/workflows/docs.yml` publishes on a push to main
(`/docs/next`) and on a published release (`/docs/vX.Y`, plus `/docs` when the
release is the highest stable one).

By hand, with credentials for the website's account:

```sh
DOCS_VERSION=next npm run build
scripts/deploy.sh next
```

`scripts/deploy.sh` reads the bucket and the distribution from the website
stack's outputs rather than holding a copy of either.

## Design

The tokens are the DNX Experience design system, vendored, as the landing page
vendors them: `src/styles/tokens.css` records the three divergences and why.
`src/styles/docs.css` maps them onto Starlight's own custom properties and does
nothing else that is not argued in place.

Nothing loads from a third-party origin. All four font families are
self-hosted, and search is Pagefind, which indexes at build time and needs no
service.

## Licence

The manual's prose and images (everything under `src/content`, the console
screenshots in `src/assets/console` and the diagrams) are licensed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/): quote and adapt them
with attribution to the Panorama project. Code samples inside the pages, and the
site's own code (components, scripts, configuration), are part of Panorama and
licensed under AGPL-3.0-only like the rest of the repository. The fonts in
`src/assets/fonts` are under the SIL Open Font License 1.1; see
`LICENSES/OFL-1.1.txt` and `REUSE.toml` at the repository root. Every page says
so in its footer.
