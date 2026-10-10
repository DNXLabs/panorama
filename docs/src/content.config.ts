// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { defineCollection } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

// Starlight warns on every build that the `i18n` collection is missing. It is
// missing on purpose: this site is English-only. Declaring it does not silence
// the warning, it adds a second one about the empty directory behind it, so
// the collection is left undeclared and the one warning is accepted.
export const collections = {
  docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
};
