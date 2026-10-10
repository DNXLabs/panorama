// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { Analytics } from './screens/Analytics'
import { Requests } from './screens/Requests'
import { People } from './screens/People'
import { Routing } from './screens/Routing'
import { Settings } from './screens/Settings'
import { MyUsage } from './screens/MyUsage'
import { MySetup } from './screens/MySetup'
import { Welcome } from './screens/Welcome'
import { ApproveCLI } from './screens/ApproveCLI'

// What the console can render, in one list.
//
// It lives here rather than in App because the sign-in gate needs it too: the
// gate renders at whatever URL a visitor arrived at, including one that names no
// screen, and it must not propose that URL as the place to land after signing
// in. Two lists would have drifted the first time a screen was added.

export type Screen = {
  path: string
  label: string
  title: string
  element: () => React.ReactElement
  /** admin and leaders gate what appears in the sidebar. The server gates the
   *  data regardless; this only decides what is worth offering. */
  leaders?: boolean
  admin?: boolean
}

// Analytics is the start page, and it answers three questions behind tabs: "is
// everything fine", "what are we spending" and "what did it buy". They share a
// date range and most of their numbers, so they are one screen, and `/` is it.
export const ORGANIZATION: Screen[] = [
  { path: '/', label: 'Analytics', title: 'Analytics', element: () => <Analytics /> },
  { path: '/requests', label: 'Audit trail', title: 'Audit trail', element: () => <Requests /> },
  { path: '/people', label: 'People & teams', title: 'People & teams', element: () => <People />, leaders: true },
  { path: '/routing', label: 'Routing', title: 'Routing', element: () => <Routing /> },
  // Integrations is a Settings tab, not an entry of its own: it is one-off
  // administrator configuration in exactly the sense single sign-on is.
  { path: '/settings', label: 'Settings', title: 'Settings', element: () => <Settings />, leaders: true },
]

export const PERSONAL: Screen[] = [
  { path: '/me', label: 'My usage', title: 'My usage', element: () => <MyUsage /> },
  { path: '/setup', label: 'My setup', title: 'My setup', element: () => <MySetup /> },
]

export const WELCOME: Screen = {
  path: '/welcome',
  label: 'Set up Panorama',
 
  title: 'Set up Panorama',
  element: () => <Welcome />,
  admin: true,
}

/**
 * Approving a terminal sign-in.
 *
 * Not in the sidebar, because nobody navigates here: they arrive from a command
 * line that printed the address. It is in ALL so that landingPath recognises it
 * - a developer who is not signed in yet lands on the sign-in gate first, and
 * has to come back to the code they were given rather than to the overview.
 *
 * Still registered while command-line sign-in is switched off, which is this
 * release. The screen says so itself rather than the path ceasing to exist: the
 * address is in documentation and in runbooks, and "there is no such screen"
 * would send somebody looking for a broken deployment instead of reading one
 * sentence about a switched-off feature.
 */
export const APPROVE_CLI: Screen = {
  path: '/cli',
  label: 'Approve a sign-in',
 
  title: 'Approve a sign-in',
  element: () => <ApproveCLI />,
}

const ALL = [...ORGANIZATION, ...PERSONAL, WELCOME, APPROVE_CLI]

/** screenAt finds the screen a path names, or null. */
export function screenAt(path: string): Screen | null {
  return ALL.find((screen) => screen.path === path) ?? null
}

/**
 * landingPath is where a visitor at `path` should be returned after signing in.
 *
 * Empty for anything the console cannot render, which is what stops a sign-in
 * from delivering somebody to a dead page. Every asset the console serves is
 * index.html - that is how client-side routing works - so a mistyped or guessed
 * URL looks exactly like a screen until React decides it is not one, and
 * "/login" is the URL people guess.
 */
export function landingPath(path: string, search = ''): string {
  if (path === '/') return ''
  if (!screenAt(path)) return ''
  // The query is kept only for a path that names a screen, which is what stops
  // this from being a way to reflect arbitrary parameters through a sign-in.
  // /cli is why it is kept at all: a developer who is not signed in arrives
  // with the code their terminal printed, and losing it would have them
  // transcribe eight characters the flow had already carried for them.
  return path + search
}
