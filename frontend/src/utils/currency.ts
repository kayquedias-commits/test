/**
 * Currency formatting utilities.
 *
 * This module is the single formatting authority for every monetary value
 * rendered in the app (KPI cards, tables, reports, exports). Monetary values
 * are exact minor-unit (two-decimal) strings; all arithmetic uses BigInt so
 * binary floating-point drift is impossible. Storage stays exact decimal
 * strings; locale formatting happens only at presentation time.
 *
 * Mirrors the minor-unit, two-decimal invariant owned by
 * `backend/internal/domain/money.go`. No `Number`/`parseFloat` is used on
 * money anywhere in this module for arithmetic.
 *
 * Security note for callers: every function here returns plain text. Render
 * the result via `textContent` (or a framework text node / `escapeHtml`)
 * only — never via `innerHTML`/`dangerouslySetInnerHTML` (injection-prone,
 * CWE-79 class, see https://owasp.org/Top10/A03_2021-Injection/).
 */

/** Maximum number of decimal places accepted by the input paths. */
export const MAX_DECIMALS = 2;

const FALLBACK_LOCALE = "en-US";
const MINOR_FACTOR = 100n;

/** One optional leading sign, 1-15 integer digits, 0-2 decimal places. */
const AMOUNT_RE = /^[+-]?\d{1,15}(\.\d{1,2})?$/;
const WHOLE_RE = /^\d{1,15}$/;
const FRAC_RE = /^\d{0,2}$/;
const CURRENCY_RE = /^[A-Z]{3}$/;

/**
 * Error thrown by the input-path functions when a value is malformed or has
 * more than two decimal places. Callers (form validation) catch this and
 * translate it into a solution-oriented validation message.
 */
export class CurrencyParseError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CurrencyParseError";
  }
}

let defaultCurrency = "USD";
let defaultLocale = FALLBACK_LOCALE;

/**
 * Sets the session base currency used by the formatters when no currency is
 * passed. Call from a bootstrap step with the server-side base currency;
 * never read from `localStorage`/`document.cookie` inside this module.
 * Throws `CurrencyParseError` if the code is not an ISO-4217-shaped three
 * uppercase letters.
 */
export function setDefaultCurrency(currency: string): void {
  if (!CURRENCY_RE.test(currency)) {
    throw new CurrencyParseError(`Invalid currency code: ${currency}`);
  }
  defaultCurrency = currency;
}

/** Sets the session locale used when none is passed to the formatters. */
export function setDefaultLocale(locale: string): void {
  defaultLocale = locale;
}

/**
 * Parses a decimal string (at most two decimal places) into an exact number
 * of minor units. The optional leading sign is honored (e.g. `"12.34"` ->
 * `1234n`, `"-12.30"` -> `-1230n`). Rejects non-numeric, scientific, hex,
 * multi-sign and three-or-more-decimal input by throwing
 * `CurrencyParseError`.
 */
export function parseMinorUnits(input: string): bigint {
  const value = input.trim();
  if (!AMOUNT_RE.test(value)) {
    throw new CurrencyParseError(
      "Amount must be a number with at most two decimal places."
    );
  }

  const negative = value.startsWith("-");
  const unsigned = value.replace(/^[+-]/, "");
  const [whole, frac = ""] = unsigned.split(".");

  try {
    const wholeBig = BigInt(whole);
    // A one-digit fraction (e.g. "3") means 30 minor units.
    const fracBig = frac.length === 1 ? BigInt(`${frac}0`) : BigInt(frac || "0");
    const units = wholeBig * MINOR_FACTOR + fracBig;
    return negative ? -units : units;
  } catch {
    throw new CurrencyParseError(
      "Amount must be a number with at most two decimal places."
    );
  }
}

/**
 * Combines a whole part and an optional 0-2-digit fractional part into a
 * minor-unit decimal string (e.g. `toMinorUnits("12", "34")` -> `"1234"`,
 * `toMinorUnits("5")` -> `"500"`). Throws `CurrencyParseError` on malformed
 * input. Negative amounts are entered via `parseMinorUnits("-...")`.
 */
export function toMinorUnits(whole: string, frac?: string): string {
  const fraction = frac ?? "";
  if (!WHOLE_RE.test(whole) || !FRAC_RE.test(fraction)) {
    throw new CurrencyParseError(
      "Whole and fractional parts must be numeric with at most two decimal places."
    );
  }
  const padded = fraction.length < 2 ? fraction.padEnd(2, "0") : fraction;
  return `${whole}${padded}`;
}

// Cached formatters keyed by "locale:currency" so the expensive construction
// happens once and is reused across the 100k-row render path.
const formatterCache = new Map<string, Intl.NumberFormat>();

function buildFormatter(locale: string, currency: string): Intl.NumberFormat {
  return new Intl.NumberFormat(locale, {
    style: "currency",
    currency,
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
}

function formatAuthoritative(amount: string, locale: string, currency: string): string {
  const key = `${locale}:${currency}`;
  let formatter = formatterCache.get(key);
  if (!formatter) {
    // Wrap so an unknown locale or malformed currency can never break a
    // render; fall back to the safe defaults rather than throwing mid-list.
    try {
      formatter = buildFormatter(locale, currency);
    } catch {
      formatter = buildFormatter(FALLBACK_LOCALE, defaultCurrency);
    }
    formatterCache.set(key, formatter);
  }

  // Detect the sign from the raw string, then format the exact decimal so
  // there is no binary-float arithmetic. The integer part is < 15 digits
  // (< 2^53) and Intl is forced to exactly two decimals, so coercing the
  // exact string to a Number only at the Intl boundary is lossless and
  // re-rounds to the same two digits. All arithmetic above stays BigInt.
  const negative = amount.startsWith("-");
  const absoluteAmount = negative ? amount.slice(1) : amount;
  const units = BigInt(absoluteAmount);
  const wholes = units / MINOR_FACTOR;
  const fracs = units % MINOR_FACTOR;
  const wholeText = wholes.toString();
  const fracText = fracs.toString().padStart(2, "0");
  const exact = `${negative ? "-" : ""}${wholeText}.${fracText}`;

  return formatter.format(Number(exact));
}

/**
 * Formats a minor-unit decimal string into a locale/currency display string
 * with exactly two decimal places. Amounts are validated and allocated
 * atomically via integer arithmetic; unknown or malformed currency codes
 * fall back to the session base currency rather than throwing. Negative
 * amounts use the locale's currency sign glyph (e.g. `-$50.00` or
 * `($50.00)`) — never color.
 */
export function formatMinorUnits(amount: string, currency: string, locale?: string): string {
  const resolvedLocale = locale ?? defaultLocale;
  const resolvedCurrency = CURRENCY_RE.test(currency) ? currency : defaultCurrency;
  return formatAuthoritative(amount, resolvedLocale, resolvedCurrency);
}

/**
 * Adds two minor-unit decimal strings exactly (BigInt). Returns a minor-unit
 * string; the sign of the result is preserved. Currency matching and
 * overflow are owned by the backend domain, not this pure addition helper.
 */
export function addMoney(a: string, b: string): string {
  return (parseMinorUnits(a) + parseMinorUnits(b)).toString();
}

/**
 * Subtracts `b` from `a` in minor units exactly (BigInt). The result may be
 * negative (reason-coded adjustment/refund path); the sign survives all
 * arithmetic. Returns a minor-unit string.
 */
export function subtractMoney(a: string, b: string): string {
  return (parseMinorUnits(a) - parseMinorUnits(b)).toString();
}

/**
 * Returns whether the amount is zero or positive. Throws
 * `CurrencyParseError` on malformed input.
 */
export function isNonNegative(amount: string): boolean {
  return parseMinorUnits(amount) >= 0n;
}

/**
 * Returns whether the amount is exactly zero. Throws `CurrencyParseError` on
 * malformed input.
 */
export function isZero(amount: string): boolean {
  return parseMinorUnits(amount) === 0n;
}