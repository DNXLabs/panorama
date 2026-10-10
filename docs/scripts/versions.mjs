#!/usr/bin/env node
// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

/**
 * Build /docs/versions.json from what is actually published.
 *
 * WHAT IT IS FOR. A frozen build cannot know about versions published after
 * it, so the version selector fetches this file at runtime instead of trusting
 * a list baked in at build time. Every deploy rewrites it, which is what keeps
 * the selector on a two-year-old page able to offer today's release.
 *
 * WHAT IT READS. The version list is derived from the bucket, not from a file
 * somebody maintains: the prefixes under docs/ are the versions, because they
 * are the only thing that actually decides what a reader can reach. A version
 * recorded in a manifest but absent from the bucket is a 404 the selector
 * offered; a version in the bucket but missing from the manifest is
 * unreachable.
 *
 * Usage:
 *   aws s3 ls s3://bucket/docs/ | node scripts/versions.mjs --latest v1.2
 *
 * --latest names the series that /docs itself currently serves, so the reader
 * can be told which one the unversioned path is. It is passed in rather than
 * inferred as "the highest", because the release workflow decides whether a
 * release moves the channel and a backported v1.3.5 published after v1.4.0
 * must not drag /docs backwards.
 */
import { readFileSync } from 'node:fs';

const args = process.argv.slice(2);
const latestFlag = args.indexOf('--latest');
const latest = latestFlag === -1 ? '' : (args[latestFlag + 1] ?? '').trim();

const input = readFileSync(0, 'utf8');

// `aws s3 ls` prints directories as "                           PRE v1.2/".
const prefixes = [...input.matchAll(/^\s*PRE\s+(.+?)\/\s*$/gm)].map((m) => m[1]);

const series = prefixes
  .filter((p) => /^v\d+\.\d+$/.test(p))
  .sort((a, b) => {
    const [aMaj, aMin] = a.slice(1).split('.').map(Number);
    const [bMaj, bMin] = b.slice(1).split('.').map(Number);
    return bMaj - aMaj || bMin - aMin;
  });

const hasNext = prefixes.includes('next');

if (latest && !series.includes(latest)) {
  // Loud rather than quietly emitting a list that says a version is the
  // latest while the bucket has no such prefix.
  console.error(
    `[docs] --latest ${latest} is not published: the bucket has ${series.join(', ') || 'no series'}.`,
  );
  process.exit(1);
}

/** @type {{id: string, label: string, path: string}[]} */
const versions = [];

// The unversioned path comes first, because it is the one a reader should be
// on unless they chose otherwise.
versions.push({
  id: 'latest',
  label: latest ? `${latest} (latest)` : 'latest release',
  path: '/docs/',
});

for (const v of series) {
  // The series that /docs already serves is not offered twice under its own
  // number: the same pages at two paths in one list reads as two versions.
  if (v === latest) continue;
  versions.push({ id: v, label: v, path: `/docs/${v}/` });
}

if (hasNext) {
  versions.push({ id: 'next', label: 'main (unreleased)', path: '/docs/next/' });
}

process.stdout.write(
  JSON.stringify(
    {
      generated: new Date().toISOString(),
      latest: latest || null,
      versions,
    },
    null,
    2,
  ) + '\n',
);
