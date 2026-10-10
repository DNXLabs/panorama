#!/usr/bin/env node
// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

/**
 * Reference pages are generated from the repository's own annotated examples,
 * never hand-copied.
 *
 * THE FAILURE THIS EXISTS TO REMOVE. A configuration reference written by hand
 * is a second copy of config.example.yaml, and the day somebody adds a key to
 * one and not the other, the documentation is wrong in the specific way
 * documentation is worst: confidently, and about a detail nobody re-reads. The
 * example file is already the source of truth - every key in it is documented
 * in place, because that is the file an operator copies - so the reference is
 * that file, rendered.
 *
 * The generated pages are gitignored and rebuilt by `prebuild`, so there is no
 * committed copy to go stale and no check that could pass while it had.
 */
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, '..', '..');
const out = resolve(here, '..', 'src', 'content', 'docs', 'reference');

/** @type {{source: string, slug: string, title: string, description: string, lead: string, order: number}[]} */
const pages = [
  {
    source: 'config.example.yaml',
    slug: 'configuration',
    title: 'Configuration',
    description:
      'Every configuration key: where to set it in the file, the environment variable that sets it, and what it defaults to.',
    order: 1,
    // The table of every key goes above the example file. Both are derived:
    // the table from internal/config, the example from the file that ships.
    keys: true,
    lead: `Panorama reads **one YAML file and the environment**, and the environment
wins. Every key below can be set either way.

The file's location is \`-config <path>\`, or \`config.yaml\` in the working
directory when it is present. In the container images it is
\`/etc/panorama/config.yaml\`.

**The variable name is the key, mechanically**: \`PANORAMA_\` followed by the
path upper-cased with dots as underscores, so \`server.base_url\` is
\`PANORAMA_SERVER_BASE_URL\`. That holds for every key here without exception,
and a test asserts it, so the column below is a convenience rather than a list
of special cases to check.

**The file holds what a deployment cannot start without**: how to reach the
identity provider, the key that protects everything else, and a region to seed
the first model provider with. Anything an administrator turns on afterwards,
model providers and their regions included, belongs in the database behind a
screen.`,
  },
  {
    source: 'policy.example.yaml',
    slug: 'policy',
    title: 'Policy file',
    description:
      "Routes and budgets as a file, for GitOps. Generated from the repository's policy.example.yaml.",
    order: 3,
    lead: `Routes and budgets can be applied from a file with \`panorama policy apply\`.
It runs the same validation the console does, so a model that is not in the
catalogue is refused when you run the command, not when somebody makes a
request. [Routes as a file](/docs/configure/routes/#routes-as-a-file) shows how
to use it.

Two things to know before you write one:

- **A policy file is the whole policy.** \`apply\` removes any route or budget
  that is not in the file.
- **A model id is written \`provider/model\`**, such as
  \`bedrock/us.anthropic.claude-sonnet-5\`. A bare id is refused.`,
  },
];

const banner = (source) =>
  `<!--\n  GENERATED FILE. Do not edit.\n\n  Built from ${source} by docs/scripts/generate-reference.mjs,\n  which runs on every \`npm run build\`. Edit ${source} instead.\n-->`;

/** A cell's worth of Markdown: pipes would end the column, newlines the row. */
const cell = (s) => String(s ?? '').replace(/\|/g, '\\|').replace(/\s*\n\s*/g, ' ').trim();

/**
 * The first sentence of a field's doc comment.
 *
 * The comments in internal/config are long on purpose - several of them argue
 * a decision over a paragraph - and a table wants the claim, not the argument.
 * The rest is not lost: it is in the annotated example below the table, which
 * is the same file an operator copies.
 */
function summarise(doc, name) {
  if (!doc) return '';
  let first = doc.split(/\n\s*\n/)[0].replace(/\s*\n\s*/g, ' ').trim();

  // Go doc convention opens every comment with the identifier - "Addr is the
  // listen address". Beside a column that already holds the key that reads as
  // a stutter, so the subject is dropped and the predicate promoted. "is" and
  // "are" are handled separately because dropping them too would leave "The
  // listen address" rather than "Is the listen address".
  if (name) {
    const lead = new RegExp(`^${name}\\s+(is|are)\\s+`);
    if (lead.test(first)) {
      first = first.replace(lead, '');
    } else if (first.startsWith(name + ' ')) {
      first = first.slice(name.length + 1);
    }
    first = first.charAt(0).toUpperCase() + first.slice(1);
  }

  // A sentence ends at a full stop followed by a space and a capital, which
  // leaves "e.g. 30s" and "secrets.MinPassphrase" alone.
  const match = first.match(/^(.*?[.!?])(\s+[A-Z(`])/);
  return cell(match ? match[1] : first);
}

/**
 * Every configuration key, grouped by the block it sits under.
 *
 * The data is generated from internal/config by
 * `go run ./internal/config/configdoc/generate`, committed, and asserted
 * current by TestTheConfigurationReferenceIsCurrent. It is read here rather
 * than derived again, because a second derivation is a second answer.
 */
function keyTables(dataPath) {
  let keys;
  try {
    keys = JSON.parse(readFileSync(dataPath, 'utf8'));
  } catch (err) {
    // Not a page without a table: a reference missing half its content is
    // worse than a build that stopped and said why.
    console.error(
      `[docs] cannot read the configuration key data at ${dataPath}.\n` +
        `       ${err.message}\n` +
        `       Regenerate it: go run ./internal/config/configdoc/generate -o ${dataPath}`,
    );
    process.exit(1);
  }

  const sections = [];
  for (const key of keys) {
    let section = sections.find((s) => s.name === key.section);
    if (!section) {
      section = { name: key.section, rows: [] };
      sections.push(section);
    }
    section.rows.push(key);
  }

  const out = [
    '## Every key',
    '',
    'Set any of these in the file or in the environment. **The environment wins.**',
    'A key whose default is *not set* has no value until you give it one; where that',
    'means "use a built-in default" rather than "off", the description says so.',
    '',
  ];

  for (const section of sections) {
    // A single top-level key - encryption_key - is not a block, and a heading
    // over one row that repeats the row would be a heading that says nothing.
    const heading = section.rows.length === 1 && section.rows[0].path === section.name
      ? null
      : `### \`${section.name}\``;
    if (heading) out.push(heading, '');

    // Wrapped, because this is the one table on the site that has to scroll:
    // its first two columns are identifiers that must not wrap, and together
    // they are wider than a phone. Every other table is a plain full-width
    // table. The blank lines are what make the rows Markdown again inside an
    // HTML block. See `.table-wrap` in docs.css.
    out.push(
      '<div class="table-wrap">',
      '',
      '| Key | Environment variable | Type | Default | What it does |',
      '| --- | --- | --- | --- | --- |',
    );
    for (const row of section.rows) {
      out.push(
        `| \`${cell(row.path)}\` | \`${cell(row.env)}\` | ${cell(row.type)} | ` +
          `${row.default === 'not set' ? '*not set*' : `\`${cell(row.default)}\``} | ${summarise(row.doc, row.name)} |`,
      );
    }
    out.push('', '</div>');
    out.push('');
  }

  return out.join('\n');
}

mkdirSync(out, { recursive: true });

for (const page of pages) {
  const path = join(repo, page.source);
  let body;
  try {
    body = readFileSync(path, 'utf8');
  } catch (err) {
    // Not a fallback to an empty page: a missing source file means the docs
    // build produces a reference nobody can trust, so it fails here instead.
    console.error(
      `[docs] cannot generate reference/${page.slug}: ${page.source} is unreadable.\n` +
        `       ${err.message}`,
    );
    process.exit(1);
  }

  const frontmatter = [
    '---',
    `title: ${page.title}`,
    `description: >-`,
    `  ${page.description}`,
    'sidebar:',
    `  order: ${page.order}`,
    'editUrl: false',
    '---',
  ].join('\n');

  const contents = [
    frontmatter,
    '',
    banner(page.source),
    '',
    page.lead,
    '',
    ...(page.keys ? [keyTables(resolve(here, '..', 'src', 'data', 'config-keys.json')), ''] : []),
    `## The annotated \`${page.source}\``,
    '',
    'The same keys in the file an operator copies, each documented where it sits.',
    '',
    '```yaml',
    body.trimEnd(),
    '```',
    '',
  ].join('\n');

  const target = join(out, `${page.slug}.md`);
  writeFileSync(target, contents);
  console.log(`[docs] generated reference/${page.slug}.md from ${page.source}`);
}
