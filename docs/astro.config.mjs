// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLinksValidator from 'starlight-links-validator';
import { readdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

// WHICH VERSION THIS BUILD IS.
//
// Three shapes, and the path is the whole difference:
//
//   DOCS_VERSION unset   -> /docs        the latest released series
//   DOCS_VERSION=v0.9    -> /docs/v0.9   that series, frozen
//   DOCS_VERSION=next    -> /docs/next   main, ahead of any release
//
// The unversioned path is the latest release rather than main, because /docs
// is the link that ends up in a README, an issue and a blog post, and the
// person following it is running a released binary. Main is reachable and
// labelled, not the default.
//
// A frozen series is never rebuilt. That is the whole point: /docs/v0.9 is
// what the documentation said when v0.9 shipped, not what today's prose would
// say about a release from six months ago. The cost is that a frozen build
// cannot know about versions published after it, which is why the version
// selector fetches its list at runtime rather than baking one in. See
// src/components/VersionSelect.astro.
const version = process.env.DOCS_VERSION?.trim() || '';

// `v0.9` is a minor series, not a patch: v0.9.0 and v0.9.3 are the same
// documentation, so a patch release overwrites its series rather than adding a
// path nobody asked for.
if (version && version !== 'next' && !/^v\d+\.\d+$/.test(version)) {
  throw new Error(
    `DOCS_VERSION must be "next" or a minor series like "v0.9", got "${version}". ` +
      `A patch release writes the series it belongs to.`,
  );
}

/**
 * A versioned build keeps its reader inside its version.
 *
 * Every hand-written link in these pages is `/docs/...`, because that is the
 * address a page has and the form the link validator can check. Astro compiles
 * the base into the URLs *it* generates - the sidebar, pagination, assets - and
 * leaves a link somebody typed exactly as typed. So on /docs/next, the first
 * link a reader followed out of the prose took them to the latest release
 * without saying so, and on a frozen /docs/v0.9 every cross-reference pointed
 * at documentation for a binary they are not running.
 *
 * It is done to the finished HTML rather than in a Markdown plugin because a
 * link reaches a page three ways - Markdown, a component's `href`, and the
 * hero actions in frontmatter - and the HTML is the one place all three are
 * the same thing.
 *
 * Two kinds of link are left alone, both deliberately: `/docs/` itself, which
 * is how the version banner and the 404 page say "the latest release", and a
 * link that already names a version.
 */
function keepLinksInVersion(version) {
  const namesAVersion = /^(?:next|v\d+\.\d+)(?:\/|$)/;
  return {
    name: 'panorama-docs:keep-links-in-version',
    hooks: {
      'astro:build:done': async ({ dir, logger }) => {
        if (!version) return;
        const root = fileURLToPath(dir);
        let pages = 0;
        let links = 0;
        for (const entry of await readdir(root, { recursive: true, withFileTypes: true })) {
          if (!entry.isFile() || !entry.name.endsWith('.html')) continue;
          const file = join(entry.parentPath ?? entry.path, entry.name);
          const html = await readFile(file, 'utf8');
          const out = html.replace(/(<a\b[^>]*?\shref=")\/docs\/([^"]*)"/g, (whole, head, rest) => {
            if (rest === '' || namesAVersion.test(rest)) return whole;
            links += 1;
            return `${head}/docs/${version}/${rest}"`;
          });
          if (out !== html) {
            await writeFile(file, out);
            pages += 1;
          }
        }
        logger.info(`kept ${links} links inside ${version}, across ${pages} pages`);
      },
    },
  };
}

const base = version ? `/docs/${version}` : '/docs';
const site = process.env.DOCS_SITE?.trim() || 'https://panorama.dnx.ai';

export default defineConfig({
  site,
  base,
  // THE DEV SERVER IS PINNED, AND REFUSES RATHER THAN WANDERING.
  //
  // `make docs-dev` prints a URL, so the server has to be on that port or the
  // message is a lie. The way that happens: a restart during startup drops
  // Astro's own port, Vite's default 5173 applies, that port is already held by
  // the console's dev server, and Astro moves to 5174 and announces it in a
  // line nobody reads twice. `strictPort` turns that
  // into a refusal to start, which is the one outcome that cannot be mistaken
  // for the site being broken.
  //
  // `host` because it runs in a container: without it the server binds
  // loopback inside the namespace and nothing outside reaches it.
  server: {
    port: Number(process.env.DOCS_PORT || 4321),
    host: true,
    strictPort: true,
  },
  // Every page is a directory with an index.html, which is what the CloudFront
  // rewrite function is written against. Changing this means changing that
  // function in the website repo's template.yaml.
  build: { format: 'directory' },
  trailingSlash: 'always',
  // The marketing page sets a strict `script-src 'self'`, and the docs behaviour
  // carries its own policy naming the hashes of what Starlight inlines. Astro
  // must therefore not inline anything it would otherwise be free to inline,
  // or the hash list changes on a whim. `npm run build` is checked against
  // csp-hashes.json by scripts/csp-hashes.mjs.
  vite: { build: { assetsInlineLimit: 0 } },
  integrations: [
    starlight({
      // The product's name alone, as the console's sidebar writes it. The
      // company is not in the wordmark here for the same reason it is not in
      // the console's: it is in the address these pages live at.
      title: 'Panorama',
      description:
        'An open-source AI gateway that connects coding-AI spend to real work. ' +
        'SSO, per-team budgets, model routing over Amazon Bedrock, and cost attributed ' +
        'to branches, tickets and pull requests with no client configuration.',
      // The banner a versioned build carries is set per page by route
      // middleware, because Starlight has no site-wide `banner` option and
      // refuses a config that names one. See src/versionBanner.ts.
      routeMiddleware: './src/versionBanner.ts',
      // NO LOGO, because the console carries no company mark either: beside a
      // column of tracked caps in the same face, a mark reads as decoration,
      // and it would be the one place a brand blue is spent outside the
      // accent.
      //
      // So the wordmark is the whole of the brand here too, set the way
      // `.brand-name` sets it: Questrial, uppercase, tracked, one weight. See
      // `.site-title` in docs.css.
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/dnxlabs/panorama',
        },
      ],
      editLink: {
        // Only the current version is editable. A frozen series is history.
        baseUrl: version
          ? undefined
          : 'https://github.com/dnxlabs/panorama/edit/main/docs/',
      },
      customCss: [
        './src/styles/fonts.css',
        // THE CONSOLE'S TOKEN FILE, IMPORTED RATHER THAN COPIED. The docs used
        // to vendor the landing page's copy of the design system, which made
        // two token sets in one repository and a third place for the system to
        // drift to. This is the same file `ui/app` imports, so a colour the
        // console changes is a colour these pages change with it - and the
        // divergences argued at the head of it apply here without being
        // re-argued.
        '../ui/design-tokens.css',
        './src/styles/docs.css',
      ],
      components: {
        // The version selector rides above the sidebar nav, which is where a
        // reader looking for "which version am I in" already is.
        Sidebar: './src/components/Sidebar.astro',
        // "On this page", with a button that folds it away. The reference
        // tables are wider than the column it leaves.
        PageSidebar: './src/components/PageSidebar.astro',
        // Starlight's footer, then the manual's licence on every page.
        Footer: './src/components/Footer.astro',
      },
      // GROUPED BY WHO IS READING, NOT BY WHAT THE THING IS. Somebody arrives as
      // one of four people - deciding whether to try it, standing it up, running
      // it, or pointing a coding tool at it - and each group is what that person
      // needs in the order they need it. The group is "Administer" while its
      // slugs are `configure/`, because a renamed path is a broken link in
      // somebody's README. Credentials is under Reference and not "For
      // engineers", because it is an operator's page of which an engineer
      // needs three lines.
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'What Panorama is', slug: 'start/what-it-is' },
            { label: 'Quickstart', slug: 'start/try-it' },
            { label: 'Concepts', slug: 'start/concepts' },
            { label: 'How attribution works', slug: 'start/attribution' },
          ],
        },
        {
          label: 'Deploy',
          items: [
            { label: 'Requirements', slug: 'deploy/requirements' },
            { label: 'On AWS', slug: 'deploy/aws' },
            { label: 'On a server', slug: 'deploy/server' },
            { label: 'On your laptop', slug: 'deploy/laptop' },
            { label: 'Choosing a database', slug: 'deploy/database' },
            { label: 'Upgrading', slug: 'deploy/upgrading' },
          ],
        },
        {
          label: 'Administer',
          items: [
            { label: 'Single sign-on', slug: 'configure/sso' },
            { label: 'Identity provider guides', slug: 'configure/sso-providers' },
            { label: 'SSO troubleshooting', slug: 'configure/sso-troubleshooting' },
            { label: 'Model providers', slug: 'configure/providers' },
            { label: 'Web search', slug: 'configure/web-search' },
            { label: 'Routes', slug: 'configure/routes' },
            { label: 'Budgets', slug: 'configure/budgets' },
            { label: 'Cost centers', slug: 'configure/cost-centers' },
            {
              label: 'GitHub integration',
              slug: 'configure/integrations',
              badge: { text: 'Experimental', variant: 'caution' },
            },
            { label: 'Troubleshooting', slug: 'troubleshooting' },
          ],
        },
        {
          label: 'For engineers',
          items: [
            { label: 'Set up your coding tool', slug: 'engineers/setup' },
            { label: 'Claude Code', slug: 'engineers/claude-code' },
            { label: 'opencode', slug: 'engineers/opencode' },
            { label: 'Codex CLI', slug: 'engineers/codex' },
            { label: 'When a request is refused', slug: 'engineers/refusals' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { label: 'Configuration file', slug: 'reference/configuration' },
            { label: 'CLI', slug: 'reference/cli' },
            { label: 'Policy file', slug: 'reference/policy' },
            { label: 'Credentials', slug: 'reference/credentials' },
          ],
        },
        {
          label: 'How it works',
          items: [{ autogenerate: { directory: 'internals' } }],
        },
      ],
      lastUpdated: true,
      pagination: true,
      // A link to a page that does not exist is the failure this site is
      // worst at showing: the distribution's 404 serves the landing page, so
      // a dead cross-reference looks like a marketing detour rather than a
      // broken link. It fails the build instead.
      //
      // THE UNVERSIONED BUILD IS WHERE LINKS ARE CHECKED. A versioned build
      // cannot check them: the validator only knows pages under this build's
      // base, so under /docs/next every `/docs/...` link is "invalid" before
      // keepLinksInVersion has made it right. They are the same links from the
      // same commit, so the workflow builds the unversioned path first, on
      // every run, and that build fails on a dead one. Skipping them here loses
      // nothing and is what lets a versioned build finish at all.
      plugins: [
        starlightLinksValidator({
          errorOnRelativeLinks: false,
          exclude: version ? ['/docs/**'] : [],
        }),
      ],
    }),
    keepLinksInVersion(version),
  ],
});
