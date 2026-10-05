/**
 * Locale-aware date utilities.
 *
 * All storage/API dates are single-system-timezone ISO 8601 UTC (YYYY-MM-DD);
 * "today" boundary follows the user's configured locale, and formatting is
 * applied only at presentation. Mirrors backend/domain/date_range.go.
 */

/**
 * Formats a Date as an ISO-8601 UTC calendar date (YYYY-MM-DD) for storage/API.
 * Returns an empty string if the input is not a valid Date.
 */
export function toIsoDate(d: Date): string {
  if (isNaN(d.getTime())) return "";
  const y = d.getUTCFullYear();
  const m = String(d.getUTCMonth() + 1).padStart(2, "0");
  const day = String(d.getUTCDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

// Strict `YYYY-MM-DD` allowlist with a day-count check against the 1st of the
// following month, so impossibilities like `2023-02-30` never reach arithmetic.
const ISO_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

/**
 * Parses a strict `YYYY-MM-DD` string into a UTC-normalized Date, or returns
 * `null` for malformed or impossible calendar dates (never an Invalid Date).
 */
export function parseIsoDate(s: string): Date | null {
  const match = ISO_RE.exec(s);
  if (!match) return null;
  const y = Number(match[1]);
  const m = Number(match[2]) - 1;
  const day = Number(match[3]);
  if (m < 0 || m > 11 || day < 1 || day > 31) return null;
  if (day > new Date(Date.UTC(y, m + 1, 0)).getUTCDate()) return null;
  return new Date(Date.UTC(y, m, day));
}

const FALLBACK_TIME_ZONE = "UTC";
const FALLBACK_LOCALE = "en-US";

/**
 * Returns the caller's configured local "today" as YYYY-MM-DD, derived from the
 * provided timeZone via Intl (en-CA yields the YYYY-MM-DD shape). Falls back to
 * UTC on a malformed locale/timeZone so a RangeError never breaks a render.
 */
export function todayInLocale(locale: string, timeZone?: string): string {
  const tz = timeZone || FALLBACK_TIME_ZONE;
  try {
    return new Intl.DateTimeFormat("en-CA", {
      timeZone: tz,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).format(new Date());
  } catch {
    return new Intl.DateTimeFormat("en-CA", {
      timeZone: FALLBACK_TIME_ZONE,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).format(new Date());
  }
}

/**
 * Formats an ISO date string for locale display (the single source of truth for
 * rendered dates). Never output raw ISO storage values to end users. Returns an
 * empty string for null/invalid input so callers never render "Invalid Date".
 */
export function formatDate(s: string, locale: string): string {
  const d = parseIsoDate(s);
  if (!d) return "";
  try {
    return new Intl.DateTimeFormat(locale, {
      year: "numeric",
      month: "short",
      day: "numeric",
    }).format(d);
  } catch {
    return new Intl.DateTimeFormat(FALLBACK_LOCALE, {
      year: "numeric",
      month: "short",
      day: "numeric",
    }).format(d);
  }
}

/**
 * Start of the month containing the given Date (UTC), as a new Date. Never
 * mutates the input.
 */
export function startOfMonth(d: Date): Date {
  return new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1));
}

/**
 * End of the month containing the given Date (UTC), as a new Date — last
 * instant of the month, mirroring date_range.go. Never mutates the input.
 */
export function endOfMonth(d: Date): Date {
  return new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1) - 1);
}

/**
 * Adds `n` months to a Date, preserving end-of-month integrity: the day clamps
 * to the last valid day of the target month (Jan 31 -> Feb 28/29 -> Mar 31).
 * All arithmetic is on UTC-normalized values so DST never shifts the result.
 */
export function addMonths(date: Date, n: number): Date {
  const y = date.getUTCFullYear();
  const m = date.getUTCMonth() + n;
  const day = date.getUTCDate();
  const targetYear = y + Math.floor(m / 12);
  const targetMonth = ((m % 12) + 12) % 12;
  const lastDay = new Date(Date.UTC(targetYear, targetMonth + 1, 0)).getUTCDate();
  return new Date(Date.UTC(targetYear, targetMonth, Math.min(day, lastDay)));
}

/**
 * Returns true only when both bounds parse and start <= end (equal bounds are
 * valid — a single-instant window). Mirrors DateRange.Valid() client-side.
 */
export function isRangeValid(start: string, end: string): boolean {
  const startDate = parseIsoDate(start);
  const endDate = parseIsoDate(end);
  if (!startDate || !endDate) return false;
  return startDate.getTime() <= endDate.getTime();
}

/**
 * Guard for the backdated-budget rule: true when `start` falls before the
 * user's local "today". This is a UI-only control — the authoritative
 * BackdatedBudgetsEnabled flag and the same rule must be enforced by the
 * server session and backend BudgetService.
 */
export function isPastPeriod(
  start: string,
  locale: string,
  timeZone?: string
): boolean {
  const startDate = parseIsoDate(start);
  if (!startDate) return false;
  const today = parseIsoDate(todayInLocale(locale, timeZone));
  if (!today) return false;
  return startDate.getTime() < today.getTime();
}