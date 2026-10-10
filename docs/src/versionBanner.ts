// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

/**
 * The banner a versioned build carries on every page: "you are reading main"
 * on /docs/next, "you are reading the v0.9 series" on a frozen one. The latest
 * release says nothing, because a banner on every page reading "you are
 * reading the current version" is a banner people stop seeing.
 *
 * WHY THIS IS ROUTE MIDDLEWARE. Not a `banner` key passed to `starlight()` in
 * astro.config.mjs, because Starlight has no such option: a banner is a field
 * of a page's frontmatter, and Starlight validates its config strictly, so
 * every build with DOCS_VERSION set would stop at "Unrecognized key: banner".
 * A local build leaves DOCS_VERSION unset and never takes that branch, so only
 * the docs workflow's `next` build would notice. Route middleware is
 * Starlight's own way to put something on every page, and it sets the same
 * field a page's frontmatter would.
 *
 * A page that declares a banner of its own keeps it.
 */
import { defineRouteMiddleware } from '@astrojs/starlight/route-data';

const version = (process.env.DOCS_VERSION || '').trim();

const content =
  version === 'next'
    ? 'This is the documentation for <strong>main</strong>, ahead of any release. ' +
      '<a href="/docs/">Go to the latest release</a>.'
    : `You are reading the <strong>${version}</strong> series. ` +
      '<a href="/docs/">Go to the latest release</a>.';

export const onRequest = defineRouteMiddleware((context) => {
  if (!version) return;
  const { data } = context.locals.starlightRoute.entry;
  if (!data.banner) data.banner = { content };
});
