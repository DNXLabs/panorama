// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Turning server values into the strings on the screen.
//
// One place, because the alternative is four screens that round the same figure
// differently and a support conversation about which one is right.

/** Nanodollars in a dollar. */
const NANOS = 1_000_000_000

/**
 * money renders integer nanodollars.
 *
 * The arithmetic stays in integers all the way to the last step. A total of a
 * few cents matters here - coding-agent requests cost fractions of one - so a
 * figure is not rounded to two places until it is large enough that the cents
 * are what a reader is looking at.
 */
export function money(nanos: number, options: { compact?: boolean } = {}): string {
  const negative = nanos < 0
  const value = Math.abs(nanos)

  if (options.compact && value >= 1000 * NANOS) {
    const thousands = value / NANOS / 1000
    return `${negative ? '−' : ''}$${thousands.toFixed(thousands >= 100 ? 0 : 1)}k`
  }

  const dollars = value / NANOS
  let text: string
  if (value === 0) {
    text = '$0.00'
  } else if (dollars >= 1) {
    text = `$${dollars.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`
  } else if (dollars >= 0.01) {
    text = `$${dollars.toFixed(4)}`
  } else {
    // Below a cent, two places would render every request as $0.00 and make
    // the whole audit trail look free.
    text = `$${dollars.toFixed(6)}`
  }
  return negative ? `−${text}` : text
}

/**
 * rate renders a published price, which arrives as exact decimal text.
 *
 * It is not parsed into a number on the way through: the string is the price as
 * AWS published it, and the only thing worth doing to it is trimming the zeros
 * that make a table unreadable. $1.100000000 and $1.10 are the same rate; only
 * one of them can be scanned down a column.
 */
export function rate(value?: string | null): string {
  if (!value) return '-'
  // A zero rate is an absence, not a price. AWS does not publish a token class
  // that costs nothing, so zero here means this model has no rate for that
  // class - and rendering it as $0.00 says caching is free, which is the same
  // mistake as costing an unpriced request at zero, made in the display layer.
  if (Number(value) === 0) return '-'
  let text = value.trim()
  if (text.includes('.')) {
    text = text.replace(/0+$/, '')
    if (text.endsWith('.')) text += '00'
    // Keep at least two decimal places, because a column of prices reads as
    // money and money has cents.
    const decimals = text.length - text.indexOf('.') - 1
    if (decimals === 1) text += '0'
  }
  return `$${text}`
}

/** count renders a whole number with separators. */
export function count(n: number): string {
  return n.toLocaleString('en-US')
}

/** compactCount renders a large count as 1.2k / 3.4M. */
export function compactCount(n: number): string {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(n >= 10_000 ? 0 : 1)}k`
  return `${(n / 1_000_000).toFixed(2)}M`
}

/** percent renders a share, keeping one decimal below ten per cent. */
export function percent(part: number, whole: number): string {
  if (whole <= 0) return '-'
  const value = (part / whole) * 100
  return value >= 10 ? `${value.toFixed(0)}%` : `${value.toFixed(1)}%`
}

/** ratio returns a 0–100 number for a bar width. */
export function ratio(part: number, whole: number): number {
  if (whole <= 0) return 0
  return Math.max(0, Math.min(100, (part / whole) * 100))
}

const TIME = new Intl.DateTimeFormat('en-GB', {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

const DATE = new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short' })

const DATE_TIME = new Intl.DateTimeFormat('en-GB', {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
})

export function time(iso: string): string {
  const at = new Date(iso)
  return Number.isNaN(at.getTime()) ? '-' : TIME.format(at)
}

export function date(iso: string): string {
  const at = new Date(iso)
  return Number.isNaN(at.getTime()) ? '-' : DATE.format(at)
}

export function dateTime(iso?: string): string {
  if (!iso) return '-'
  const at = new Date(iso)
  return Number.isNaN(at.getTime()) ? '-' : DATE_TIME.format(at)
}

/** ago renders a rough elapsed time, which is what a "last seen" column wants. */
export function ago(iso?: string): string {
  if (!iso) return 'never'
  const at = new Date(iso).getTime()
  if (Number.isNaN(at)) return 'never'

  const seconds = Math.floor((Date.now() - at) / 1000)
  if (seconds < 45) return 'just now'
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`
  const days = Math.floor(seconds / 86_400)
  if (days < 30) return `${days}d ago`
  return dateTime(iso)
}

/** duration renders milliseconds the way a latency column wants. */
export function duration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const minutes = Math.floor(ms / 60_000)
  return `${minutes}m ${Math.round((ms % 60_000) / 1000)}s`
}

/**
 * windowLabel names a range the way a sentence does: "30 days", "24 hours".
 *
 * The picker says `30d` because four buttons in a bordered group have room for
 * two characters. A kicker over a figure is read as language and says the words
 * - and both come from here, so a range renamed in one place is renamed in the
 * other.
 */
export function windowLabel(range: string): string {
  switch (range) {
    case '24h':
      return '24 hours'
    case '7d':
      return '7 days'
    case '90d':
      return '90 days'
    default:
      return '30 days'
  }
}

/** windowAdjective is the same window used attributively: "30-day window".
 *
 *  Singular, because an adjective is: a 30-day window, not a 30-days one. */
export function windowAdjective(range: string): string {
  return windowLabel(range).replace(' ', '-').replace(/s$/, '')
}

/** tokens renders a token count, which is always large. */
export function tokens(n: number): string {
  return compactCount(n)
}

/**
 * unattributedLabel is the one place the remainder is named.
 *
 * The API sends a label with the row for exactly this reason: a bucket that
 * some screens call "unattributed", some call "other", and one forgets to
 * render at all is how the honest number stops being shown.
 */
export const unattributedLabel = 'Unattributed'

/**
 * amountForEdit renders an exact USD string for a text field.
 *
 * The API sends money to the nanodollar - "500.000000000" - because that is the
 * number enforcement uses, and rounding it on the way out would put a different
 * limit in front of the person raising it. Nine zeros in an input box is
 * nobody's idea of a limit, so the trailing ones are dropped here, and never
 * anything that changes the value: "0.000000001" keeps every digit it has.
 */
export function amountForEdit(usd?: string | null): string {
  if (!usd) return ''
  if (!usd.includes('.')) return usd

  const [whole, fraction] = usd.split('.')
  const trimmed = (fraction ?? '').replace(/0+$/, '')
  if (trimmed === '') return `${whole}.00`
  return `${whole}.${trimmed.padEnd(2, '0')}`
}

/**
 * parseUSD reads decimal dollars into integer nanodollars.
 *
 * `Number(text) * 1e9` is the obvious way and it is the wrong one: the multiply
 * goes through a float, so a limit somebody typed comes back a nanodollar or
 * two off the figure enforcement will use. The digits are concatenated into one
 * integer instead - "2500.5" becomes "2500" + "500000000" - which is exact for
 * every budget anybody sets, and refuses rather than rounds above that.
 *
 * Null is "not a number I can use", which is what a half-typed box holds, and
 * is deliberately different from zero.
 */
export function parseUSD(text: string): number | null {
  const trimmed = text.trim()
  // Seven whole digits is $9,999,999 - past 2^53 nanodollars the concatenation
  // stops being exact, and a budget that large is a typo either way.
  if (!/^\d{1,7}(\.\d{0,9})?$/.test(trimmed)) return null
  const [whole, fraction = ''] = trimmed.split('.')
  return Number(whole + fraction.padEnd(9, '0'))
}
