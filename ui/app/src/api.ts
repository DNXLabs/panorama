// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// The console's one door to the server.
//
// Every request goes through here, which is what makes three things true in one
// place rather than in fifty: the CSRF token is attached to every write, a 401
// puts the whole console back on the sign-in screen instead of leaving one card
// spinning, and an error carries the server's own sentence rather than a
// generic one this file invented.

/** ApiError carries what the server said, and what it answered with. */
export class ApiError extends Error {
  readonly status: number
  readonly requestId: string

  constructor(status: number, message: string, requestId = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.requestId = requestId
  }
}

/** The CSRF token for this session, handed over by GET /api/v1/auth/session. */
let csrfToken = ''

export function setCsrfToken(token: string) {
  csrfToken = token
}

/** Listeners notified when the server stops recognising this session. */
const signedOutListeners = new Set<() => void>()

export function onSignedOut(fn: () => void): () => void {
  signedOutListeners.add(fn)
  return () => signedOutListeners.delete(fn)
}

type Options = {
  method?: string
  body?: unknown
  /** Suppress the global sign-out broadcast - used by the probe that asks
   *  whether anyone is signed in at all, where a 401 is the answer rather than
   *  an event. */
  quiet?: boolean
}

export async function api<T>(path: string, options: Options = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const headers: Record<string, string> = { Accept: 'application/json' }

  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }
  if (method !== 'GET' && method !== 'HEAD' && csrfToken) {
    headers['X-Panorama-CSRF'] = csrfToken
  }

  const response = await fetch(path, {
    method,
    headers,
    // Same-origin only. The console never talks to anywhere else, and the
    // server sends no CORS headers, so this is a statement of intent as much
    // as a setting.
    credentials: 'same-origin',
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  })

  if (response.status === 401 && !options.quiet) {
    signedOutListeners.forEach((fn) => fn())
  }

  const text = await response.text()
  let payload: unknown = null
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = null
    }
  }

  if (!response.ok) {
    const record = (payload ?? {}) as { error?: string; request_id?: string }
    throw new ApiError(
      response.status,
      record.error || `Panorama answered ${response.status}.`,
      record.request_id || '',
    )
  }

  return payload as T
}

export const get = <T,>(path: string, quiet = false) => api<T>(path, { quiet })
export const post = <T,>(path: string, body?: unknown) => api<T>(path, { method: 'POST', body })
export const put = <T,>(path: string, body?: unknown) => api<T>(path, { method: 'PUT', body })
export const patch = <T,>(path: string, body?: unknown) => api<T>(path, { method: 'PATCH', body })
export const del = <T,>(path: string) => api<T>(path, { method: 'DELETE' })

/** A window, as the API's `since` parameter wants it. */
export type Range = '24h' | '7d' | '30d' | '90d'

export const RANGES: { id: Range; label: string }[] = [
  { id: '24h', label: '24h' },
  { id: '7d', label: '7d' },
  { id: '30d', label: '30d' },
  { id: '90d', label: '90d' },
]

/** query builds a query string, dropping anything empty. */
export function query(params: Record<string, string | number | undefined | null>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    search.set(key, String(value))
  }
  const encoded = search.toString()
  return encoded ? `?${encoded}` : ''
}
