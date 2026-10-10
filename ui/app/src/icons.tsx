// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactElement } from 'react'

// The console's glyphs, and there are deliberately few of them.
//
// The design system is close to iconless: structure is carried by rules,
// squares and mono labels, and the navigation is a column of tracked mono caps
// with nothing drawn beside it. What survives here is the handful of places a
// picture says something a label cannot - the magnifier in a search field, the
// mark on a callout that is warning rather than explaining, the state of a step
// in the wizard.
//
// They share one 16px box, one 1.5px stroke, round caps and joins, and no fill,
// so a set of them reads as one hand. Colour is inherited. There is no icon
// font and no icon library; every one of these is drawn here, which is what
// keeps the console's only third-party origin at none.

type IconProps = { className?: string }

const BOX = {
  viewBox: '0 0 16 16',
  width: 16,
  height: 16,
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.5,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
  'aria-hidden': true,
  focusable: 'false' as const,
}

/** Work - a branch leaving a line and being merged back, which is the unit of
 *  work every attribution resolves to. */
function Work({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <circle cx="4" cy="3.5" r="1.6" />
      <circle cx="4" cy="12.5" r="1.6" />
      <circle cx="12" cy="8" r="1.6" />
      <path d="M4 5.1 L4 10.9" />
      <path d="M10.4 8 L7.5 8 A3.5 3.5 0 0 1 4 4.5" />
    </svg>
  )
}

/** Set up Panorama - the panorama again, part-drawn: the deployment is not
 *  finished opening. */
function Welcome({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <path d="M8 2 A6 6 0 0 1 14 8 A6 6 0 0 1 8 14" />
      <path d="M5.6 13.5 A6 6 0 0 1 2.5 10.4" strokeDasharray="0.1 3" />
      <path d="M2.2 6.9 A6 6 0 0 1 4.7 3.2" strokeDasharray="0.1 3" />
      <circle cx="8" cy="8" r="2.2" />
    </svg>
  )
}

// The rest are for callouts and marks rather than navigation. Same box, same
// stroke, so a tile in a callout and an item in the sidebar are visibly the
// same hand.

/** Something worth knowing before you act. */
function Info({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <circle cx="8" cy="8" r="6" />
      <path d="M8 7.4 L8 11.2" />
      <circle cx="8" cy="5.1" r="0.85" fill="currentColor" stroke="none" />
    </svg>
  )
}

/** Something is wrong, or is not what it appears to be. */
function Alert({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <path d="M7 2.6 L1.7 12.2 A1.15 1.15 0 0 0 2.7 13.9 L13.3 13.9 A1.15 1.15 0 0 0 14.3 12.2 L9 2.6 A1.15 1.15 0 0 0 7 2.6 Z" />
      <path d="M8 6.4 L8 9.6" />
      <circle cx="8" cy="11.7" r="0.85" fill="currentColor" stroke="none" />
    </svg>
  )
}

/** A promise about what is not kept - the trust stances are drawn with this. */
function Shield({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <path d="M8 1.8 L13.3 4 L13.3 8 C13.3 11.2 11 13.4 8 14.2 C5 13.4 2.7 11.2 2.7 8 L2.7 4 Z" />
      <path d="M5.9 7.9 L7.4 9.4 L10.3 6.3" />
    </svg>
  )
}

/** A URI a provider must send a browser back to. */
function Redirect({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <path d="M13.5 3.5 L13.5 7 A2.5 2.5 0 0 1 11 9.5 L3 9.5" />
      <path d="M5.6 7 L3 9.5 L5.6 12" />
    </svg>
  )
}

/** Nothing has arrived yet, and the console is watching for it. */
function Waiting({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <circle cx="8" cy="8" r="6" />
      <path d="M8 4.6 L8 8 L10.4 9.6" />
    </svg>
  )
}

/** Who somebody is, according to a provider - never a face, for the same reason
 *  the People icon has none. */
function Identity({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <rect x="1.8" y="3.4" width="12.4" height="9.2" rx="2" />
      <circle cx="5.9" cy="7.4" r="1.6" />
      <path d="M3.5 11.1 A2.8 2.8 0 0 1 8.3 11.1" />
      <path d="M10.4 6.9 L12.4 6.9" />
      <path d="M10.4 9.4 L12.4 9.4" />
    </svg>
  )
}

/** A step that is done. The same shape the action buttons draw. */
function Check({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <path d="M3.5 8.5 L6.5 11.5 L12.5 4.5" />
    </svg>
  )
}

/** A step deliberately passed over - not failed, and not pending. */
function Skipped({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <path d="M4 8 L12 8" />
    </svg>
  )
}

/** Search - the magnifier a filter row is read as, at any size. */
function Search({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <circle cx="7" cy="7" r="4.5" />
      <path d="M10.4 10.4 L14 14" />
    </svg>
  )
}

/** Caret - the one mark that says a control opens something.
 *
 *  A native select's arrow is the one part of it nobody can draw: every
 *  operating system supplies its own, at its own size, in its own grey. This is
 *  that arrow,
 *  in the same box and the same hand as the magnifier - which is the argument
 *  for drawing it rather than setting a glyph, since the two of them are the
 *  only marks in the console that live inside a field. */
function Caret({ className }: IconProps) {
  return (
    <svg {...BOX} className={className}>
      <path d="M4.5 6.5 L8 10 L11.5 6.5" />
    </svg>
  )
}

/** Every glyph, by the name its call site uses. */
export const ICONS: Record<string, (props: IconProps) => ReactElement> = {
  work: Work,
  welcome: Welcome,
  info: Info,
  alert: Alert,
  shield: Shield,
  redirect: Redirect,
  waiting: Waiting,
  identity: Identity,
  check: Check,
  skipped: Skipped,
  search: Search,
  caret: Caret,
}

/** Icon renders one by name. An unknown name draws nothing rather than
 *  throwing: a missing glyph should cost a callout its mark, not the screen
 *  around it. It is logged, because it is still a mistake. */
export function Icon({ name, className }: { name: string; className?: string }) {
  const Glyph = ICONS[name]
  if (!Glyph) {
    console.error(`no icon named "${name}"`)
    return null
  }
  return <Glyph className={className} />
}
