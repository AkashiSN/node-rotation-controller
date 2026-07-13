// docs/.vitepress/theme/components/simulator/timeutil.ts
//
// Wall-clock arithmetic in a NAMED timezone, on top of Intl. PURE — no Vue, no DOM.
//
// Every calendar boundary the page draws (a tick at local midnight, a Monday that
// starts a calendar week, a 15-minute cell of the window grid) is a WALL-CLOCK
// boundary in the policy's timezone, not a fixed offset from an arbitrary instant. A
// "day" is 23h or 25h across a DST transition, and a "week" is 167h or 169h; a grid
// built from fixed 24h chunks would drift away from the weekday column it is the
// denominator for.
//
// The display timezone is NEVER the browser's: it is the policy's, and the page always
// labels it.

/** The wall-clock fields of an instant in `tz`. weekday is 0=Monday … 6=Sunday — the
 *  week the calendar grid uses, not JS's Sunday-first getDay(). */
export interface Parts {
  y: number
  mo: number // 1-12
  d: number
  h: number
  mi: number
  s: number
  weekday: number // 0=Mon … 6=Sun
}

const WEEKDAY_INDEX: Record<string, number> = {
  Mon: 0, Tue: 1, Wed: 2, Thu: 3, Fri: 4, Sat: 5, Sun: 6,
}

const formatterCache = new Map<string, Intl.DateTimeFormat>()

function partsFormatter(tz: string): Intl.DateTimeFormat {
  let f = formatterCache.get(tz)
  if (!f) {
    f = new Intl.DateTimeFormat('en-US', {
      timeZone: tz,
      hourCycle: 'h23',
      weekday: 'short',
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    })
    formatterCache.set(tz, f)
  }
  return f
}

/** True when `tz` is a timezone this runtime knows. A policy can name anything, and an
 *  unknown zone must degrade to a labelled fallback rather than throw inside a render. */
export function isValidTimezone(tz: string): boolean {
  if (!tz) return false
  try {
    partsFormatter(tz).format(0)
    return true
  } catch {
    formatterCache.delete(tz)
    return false
  }
}

/** The wall-clock fields of `ms` in `tz`. */
export function zonedParts(ms: number, tz: string): Parts {
  const parts = partsFormatter(tz).formatToParts(new Date(ms))
  const get = (type: string) => parts.find(p => p.type === type)?.value ?? ''
  return {
    y: Number(get('year')),
    mo: Number(get('month')),
    d: Number(get('day')),
    h: Number(get('hour')),
    mi: Number(get('minute')),
    s: Number(get('second')),
    weekday: WEEKDAY_INDEX[get('weekday')] ?? 0,
  }
}

/** The zone's UTC offset at `ms`, in milliseconds (positive east of Greenwich). */
export function zoneOffsetMs(ms: number, tz: string): number {
  const p = zonedParts(ms, tz)
  const asUTC = Date.UTC(p.y, p.mo - 1, p.d, p.h, p.mi, p.s)
  // The instant's own sub-second remainder is not carried by Parts; drop it from both
  // sides so the difference is exactly the offset.
  return asUTC - (ms - (((ms % 1000) + 1000) % 1000))
}

/** The instant at which the wall clock in `tz` reads the given local time.
 *
 *  Two passes, because the offset that converts local → UTC is itself a function of the
 *  instant we are solving for: guess with the offset at the naive UTC reading, then
 *  correct with the offset actually in force there. A local time that a DST spring-
 *  forward SKIPS has no instant; this returns the instant the clock jumps to, which is
 *  what makes a skipped 15-minute cell measure zero elapsed minutes. */
export function instantOfLocal(
  y: number, mo: number, d: number, h: number, mi: number, tz: string,
): number {
  const naive = Date.UTC(y, mo - 1, d, h, mi, 0)
  const guess = naive - zoneOffsetMs(naive, tz)
  return naive - zoneOffsetMs(guess, tz)
}

export const MINUTE_MS = 60_000
export const HOUR_MS = 3_600_000
export const DAY_MS = 86_400_000

/** The most recent local midnight at or before `ms`. */
export function startOfLocalDay(ms: number, tz: string): number {
  const p = zonedParts(ms, tz)
  return instantOfLocal(p.y, p.mo, p.d, 0, 0, tz)
}

/** Local midnight `n` calendar days after the local day containing `ms`. Calendar days,
 *  not 24h steps: across a DST transition the two differ by an hour. */
export function addLocalDays(ms: number, n: number, tz: string): number {
  const p = zonedParts(ms, tz)
  // Date.UTC normalises an out-of-range day (Jan 32 → Feb 1), so month/year rollover is
  // free; read the normalised fields back out before resolving in the zone.
  const rolled = new Date(Date.UTC(p.y, p.mo - 1, p.d + n))
  return instantOfLocal(
    rolled.getUTCFullYear(), rolled.getUTCMonth() + 1, rolled.getUTCDate(), 0, 0, tz,
  )
}

/** The most recent local Monday 00:00 at or before `ms` — the start of the calendar week
 *  the grid is keyed to. */
export function startOfLocalWeek(ms: number, tz: string): number {
  const day = startOfLocalDay(ms, tz)
  return addLocalDays(day, -zonedParts(day, tz).weekday, tz)
}

/** Minutes from local midnight to `ms`, by the wall clock. Across a DST transition this
 *  is NOT (ms − startOfLocalDay)/60000: that difference counts real elapsed minutes,
 *  and the wall clock skipped or repeated an hour of them. */
export function localMinuteOfDay(ms: number, tz: string): number {
  const p = zonedParts(ms, tz)
  return p.h * 60 + p.mi
}

const labelCache = new Map<string, Intl.DateTimeFormat>()

function labelFormatter(tz: string, opts: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
  const key = `${tz}|${JSON.stringify(opts)}`
  let f = labelCache.get(key)
  if (!f) {
    f = new Intl.DateTimeFormat('en-GB', { timeZone: tz, hourCycle: 'h23', ...opts })
    labelCache.set(key, f)
  }
  return f
}

/** A tick's time-of-day label ("02:15") in the display timezone — never the browser's. */
export function formatTimeOfDay(ms: number, tz: string): string {
  return labelFormatter(tz, { hour: '2-digit', minute: '2-digit' }).format(new Date(ms))
}

/** A tick's date label ("Sat 3 Jan") in the display timezone. */
export function formatDate(ms: number, tz: string): string {
  return labelFormatter(tz, { weekday: 'short', day: 'numeric', month: 'short' }).format(new Date(ms))
}

/** An instant, fully qualified, for a tooltip or an accessible name. */
export function formatInstant(ms: number, tz: string): string {
  return labelFormatter(tz, {
    year: 'numeric', month: 'short', day: '2-digit',
    hour: '2-digit', minute: '2-digit',
  }).format(new Date(ms))
}

/** A duration as a compact human string ("1h 15m", "45s"). Used for the ruler's bar
 *  labels and the TGP cap; NOT a Go duration (the wire's strings are rendered verbatim
 *  wherever the controller's own value is what matters). */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms)) return '—'
  const neg = ms < 0
  let rest = Math.round(Math.abs(ms) / 1000)
  const d = Math.floor(rest / 86_400); rest -= d * 86_400
  const h = Math.floor(rest / 3_600); rest -= h * 3_600
  const m = Math.floor(rest / 60); rest -= m * 60
  const parts: string[] = []
  if (d) parts.push(`${d}d`)
  if (h) parts.push(`${h}h`)
  if (m) parts.push(`${m}m`)
  if (rest || parts.length === 0) parts.push(`${rest}s`)
  return (neg ? '-' : '') + parts.slice(0, 2).join(' ')
}
