// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

import { rate } from './format'
import type { ModelAPI, ModelRow, PersonRow, RouteRow } from './types'

// What the routing screen works out about a route and the models behind it.
//
// Shared because the card, the editor and the catalogue each need the same
// answers - which model a target names, what it costs, who a condition admits -
// and three copies of "who can ask for this name" would eventually disagree
// with the gateway, which is the one place the question is really answered.

/** Names Claude Code resolves itself, so a route called one of them is never
 *  asked for.
 *
 *  `default` is the one everybody reaches for and the worst of them: to Claude
 *  Code it means "clear any override and use this account's runtime default",
 *  so it never reaches the gateway as a name. What arrives is whatever the
 *  client resolved to - a model id nobody routed - and the refusal names that
 *  model and lists `default` among the routes the engineer may use, which
 *  points them at everything except the cause.
 *
 *  The server refuses these in Route.Validate and is what actually enforces it.
 *  This copy exists so the answer arrives while the name is being typed rather
 *  than on save; if the two ever disagree, the server is right. */
const RESERVED_ROUTE_NAMES = new Set([
  'default', 'best', 'fable', 'sonnet', 'opus', 'haiku', 'opusplan',
  'sonnet[1m]', 'opus[1m]',
])

export function reservedRouteName(name: string): boolean {
  return RESERVED_ROUTE_NAMES.has(name.trim().toLowerCase())
}

/** The name Claude Code knows an Anthropic model by, or undefined for any other
 *  vendor's model.
 *
 *  Claude Code shapes a request for the model it believes it is talking to, and
 *  a name it does not recognise is treated as the newest model there is - so it
 *  sends adaptive thinking, effort and a mid-conversation system message, and
 *  an older model behind the route refuses the whole request. A route named with
 *  the model's own Anthropic ID is recognised, and gets a request the model
 *  accepts (Haiku 4.5 behind `fast` fails every subagent request, and behind
 *  `claude-haiku-4-5-20251001` serves them all).
 *
 *  Read from the foundation-model id, which carries no geography:
 *  `anthropic.claude-haiku-4-5-20251001-v1:0` is `claude-haiku-4-5-20251001`,
 *  and Mantle's `anthropic.claude-sonnet-5` is `claude-sonnet-5`. */
export function claudeCodeName(model: ModelRow): string | undefined {
  const id = model.model || model.id
  if (!id.startsWith('anthropic.claude-')) return undefined
  return id.slice('anthropic.'.length).replace(/-v\d+(:\d+)?$/, '')
}

/** Draft is a route being edited. Staged, and written only on save: a route is
 *  several decisions and half of them applied is a route nobody meant. */
export type Draft = {
  original: RouteRow | null
  name: string
  description: string
  /** The model, as `provider/id`. */
  target: string
  /** Whether this route keeps serving once a budget passes its soft limit.
   *  True is the ordinary route and the default; false is what makes an
   *  engineer switch. */
  servesOverSoft: boolean
  users: string[]
  teams: string[]
  tools: string[]
}

/** Overlay is whatever is open on top of the route editor. */
export type Overlay =
  /** `picking` is choosing the route's model; otherwise the catalogue is being
   *  read. */
  | { kind: 'catalogue'; picking: boolean }
  | { kind: 'people' }
  | { kind: 'teams' }
  | null

export function matchedPeople(
  route: { users?: string[]; teams?: string[] },
  people: PersonRow[],
): PersonRow[] {
  const users = route.users ?? []
  const teams = route.teams ?? []
  return people.filter((p) => {
    if (users.length > 0 && !users.includes(p.id)) return false
    if (teams.length > 0 && !p.teams.some((t) => teams.includes(t.id))) return false
    return true
  })
}

/** Where a target is processed, as the pill beside its name reads it.
 *
 *  bedrock-runtime reaches most models through an inference profile, so a row
 *  without one is the exception and says "no profile". A provider with no
 *  profiles at all invokes every model in the region it is configured for, and
 *  that region is the whole answer. */
export function profileLabel(model: ModelRow): string {
  if (model.geography !== '') return model.geography
  return model.provider === 'bedrock' ? 'no profile' : `in-region · ${model.region}`
}

/** The APIs a model or route is served on, in the words a client's own
 *  documentation uses. */
const API_LABELS: Record<ModelAPI, string> = {
  'anthropic-messages': 'Messages',
  'openai-chat': 'Chat Completions',
  'openai-responses': 'Responses',
}

/** The endpoint each API arrives at, which is what a refusal on the wrong one
 *  names. */
export const API_ENDPOINTS: Record<ModelAPI, string> = {
  'anthropic-messages': '/v1/messages',
  'openai-chat': '/v1/chat/completions',
  'openai-responses': '/v1/responses',
}

export function apiLabel(apis?: ModelAPI[] | null): string {
  const list = apis ?? []
  if (list.length === 0) return 'no API this gateway serves'
  return list.map((api) => API_LABELS[api] ?? api).join(' · ')
}

/** A model a route can point at: one the provider says can be invoked, that
 *  speaks an API this gateway serves, and that streams - a model that cannot
 *  stream cannot serve a coding agent. */
export function routable(model: ModelRow): boolean {
  return model.available && model.apis.length > 0 && model.streaming
}

/**
 * scopeOf says how narrow a route is, in the terms that narrowed it.
 *
 * A route restricted only by tool matches every person, so counting people
 * would report "8 people" for a route nobody can reach from a browser - true
 * of the person condition and misleading about the route.
 *
 * `matched` is how many of the roster the person and team conditions admit,
 * which the caller works out because only it has the roster.
 */
export function scopeOf(route: { users?: string[] | null; teams?: string[] | null; tools?: string[] | null }, matched = 0): string {
  const identity = (route.users ?? []).length > 0 || (route.teams ?? []).length > 0
  const tools = (route.tools ?? []).length
  if (identity) {
    return (
      `${matched} ${matched === 1 ? 'person' : 'people'}` +
      (tools > 0 ? `, ${tools === 1 ? '1 tool' : `${tools} tools`}` : '')
    )
  }
  if (tools > 0) return tools === 1 ? `${route.tools![0]} only` : `${tools} tools`
  return 'everyone'
}

/** modelMeta is the line under a model's name wherever one is being chosen:
 *  where it is processed, and what it costs. The two things the choice is
 *  actually made on, which is why they travel together. */
export function modelMeta(model?: ModelRow): string {
  if (!model) return ''
  return `${model.provider_label} · ${profileLabel(model)} · ${priceLine(model)}`
}

export function priceLine(model?: ModelRow): string {
  if (!model) return ''
  if (!model.rates) return 'unpriced - requests are recorded with no cost'
  return `${rate(model.rates['input'])} in · ${rate(model.rates['output'])} out per M`
}

/** The catalogue row a route's reference names, when discovery has seen it. */
export function modelFor(models: ModelRow[], ref: string): ModelRow | undefined {
  return ref ? models.find((m) => m.ref === ref) : undefined
}

export function nameOf(models: ModelRow[], ref: string): string {
  return modelFor(models, ref)?.name ?? ref
}

/** Profiles before the bare foundation-model id: almost every current model is
 *  invocable only through one, and the handful that still accept a bare id are
 *  the ones being retired. */
export function byNameThenProfile(a: ModelRow, b: ModelRow): number {
  return (
    a.name.localeCompare(b.name) ||
    Number(a.geography === '') - Number(b.geography === '') ||
    a.geography.localeCompare(b.geography)
  )
}
