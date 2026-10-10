#!/usr/bin/env node
// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

/**
 * The inline scripts and styles the built site contains, as CSP hashes.
 *
 * WHY THIS EXISTS. The docs are served from panorama.dnx.ai, whose landing
 * page sets `script-src 'self'` and carries no inline script at all. The docs
 * cannot hold that line: Starlight writes the reader's theme onto the document
 * before first paint, and the only way to do that without a flash of the wrong
 * theme is an inline script. So the /docs/* behaviour gets its own response
 * headers policy, and that policy names the hashes of exactly what Starlight
 * inlines.
 *
 * A hash list is only safe if it fails loudly. Upgrade Starlight, get a new
 * inline script, and the deployed CSP silently blocks it: the site still
 * renders, so nothing looks broken, and the theme toggle just stops working
 * for everybody. So this runs as `postbuild` in --check mode, and a build
 * whose inline content is not in csp-hashes.json fails here rather than in a
 * reader's browser.
 *
 *   node scripts/csp-hashes.mjs            print what the build contains
 *   node scripts/csp-hashes.mjs --check    fail if it differs from the file
 *   node scripts/csp-hashes.mjs --write    accept the current build
 *   node scripts/csp-hashes.mjs --header   print the CSP header value
 */
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const dist = resolve(here, '..', 'dist');
const manifestPath = resolve(here, '..', 'csp-hashes.json');

const mode = process.argv[2] ?? '--print';

/** Every .html file under dist/. */
function htmlFiles(dir) {
  /** @type {string[]} */
  const found = [];
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) found.push(...htmlFiles(path));
    else if (entry.endsWith('.html')) found.push(path);
  }
  return found;
}

/**
 * Inline <script> and <style> bodies.
 *
 * A tag carrying src= or href= is not inline and needs no hash. An empty body
 * is skipped because CSP does not require a hash for one, and hashing the
 * empty string would put a meaningless constant in the policy.
 */
function inlineBodies(html, tag) {
  const pattern = new RegExp(`<${tag}([^>]*)>([\\s\\S]*?)</${tag}>`, 'gi');
  /** @type {string[]} */
  const bodies = [];
  for (const [, attrs, body] of html.matchAll(pattern)) {
    if (/\s(src|href)\s*=/i.test(attrs)) continue;
    if (!body.trim()) continue;
    bodies.push(body);
  }
  return bodies;
}

const sha256 = (s) => `'sha256-${createHash('sha256').update(s, 'utf8').digest('base64')}'`;

let files;
try {
  files = htmlFiles(dist);
} catch (err) {
  console.error(`[docs] no build to scan at ${dist}: ${err.message}`);
  console.error('       run `npm run build` first.');
  process.exit(1);
}

const scripts = new Set();
const styles = new Set();

for (const file of files) {
  const html = readFileSync(file, 'utf8');
  for (const body of inlineBodies(html, 'script')) scripts.add(sha256(body));
  for (const body of inlineBodies(html, 'style')) styles.add(sha256(body));
}

const found = {
  script: [...scripts].sort(),
  style: [...styles].sort(),
};

/** The header value the CloudFront response headers policy carries. */
function header(h) {
  return [
    `default-src 'none'`,
    `script-src 'self' ${h.script.join(' ')}`.trimEnd(),
    `style-src 'self' ${h.style.join(' ')}`.trimEnd(),
    `img-src 'self' data:`,
    `font-src 'self'`,
    // The version selector fetches /docs/versions.json and HEADs a candidate
    // page in another version. Both are same-origin.
    `connect-src 'self'`,
    `base-uri 'none'`,
    `form-action 'none'`,
    `frame-ancestors 'none'`,
  ].join('; ');
}

if (mode === '--write') {
  writeFileSync(manifestPath, JSON.stringify(found, null, 2) + '\n');
  console.log(`[docs] wrote ${found.script.length} script and ${found.style.length} style hashes`);
  console.log(`[docs] update the DocsHeadersPolicy in the website repo's template.yaml:`);
  console.log();
  console.log(header(found));
  process.exit(0);
}

if (mode === '--header') {
  console.log(header(JSON.parse(readFileSync(manifestPath, 'utf8'))));
  process.exit(0);
}

if (mode === '--check') {
  let expected;
  try {
    expected = JSON.parse(readFileSync(manifestPath, 'utf8'));
  } catch {
    console.error('[docs] csp-hashes.json is missing. Run: node scripts/csp-hashes.mjs --write');
    process.exit(1);
  }

  const diff = (kind) => {
    const added = found[kind].filter((h) => !expected[kind].includes(h));
    const gone = expected[kind].filter((h) => !found[kind].includes(h));
    return { added, gone };
  };

  const s = diff('script');
  const t = diff('style');

  if (s.added.length || s.gone.length || t.added.length || t.gone.length) {
    console.error('[docs] the built site inlines different content than csp-hashes.json allows.');
    console.error();
    console.error('       This matters because the deployed Content-Security-Policy names these');
    console.error('       hashes. An inline script that is not in the policy is blocked silently:');
    console.error('       the page still renders, so nothing looks broken, and the theme toggle');
    console.error('       just stops working.');
    console.error();
    for (const [kind, d] of [
      ['script', s],
      ['style', t],
    ]) {
      for (const h of d.added) console.error(`       + ${kind} ${h}`);
      for (const h of d.gone) console.error(`       - ${kind} ${h}`);
    }
    console.error();
    console.error('       If this was a deliberate upgrade:');
    console.error('         1. node scripts/csp-hashes.mjs --write');
    console.error("         2. paste the printed policy into the website repo's template.yaml");
    console.error('         3. deploy the website stack before deploying these docs');
    process.exit(1);
  }

  console.log(
    `[docs] CSP hashes match (${found.script.length} script, ${found.style.length} style)`,
  );
  process.exit(0);
}

console.log(JSON.stringify(found, null, 2));
console.log();
console.log(header(found));
