/**
 * Client-side field validators mirroring the server's domain invariants
 * (backend/internal/domain/money.go, transaction.go, budget.go). Each validator
 * returns a localized, solution-oriented plain-text message or null.
 *
 * UX-ONLY: this module is not a trust boundary — the server remains
 * authoritative and validates independently. All returned strings are plain
 * text; render them only via text nodes / textContent, never innerHTML.
 */

import { parseIsoDate, isRangeValid, todayInLocale } from "./date";

/**
 * A field validator: returns a user-facing message when `value` is invalid, or
 * null when it passes. Empty-state handling is owned exclusively by `required`;
 * every other validator short-circuits on empty values so they never surface a
 * misleading format error for a missing value.
 */
export type Validator<T = string> = (value: T) => string | null;

/**
 * Returns a validator that rejects null, undefined and whitespace-only values.
 * The message names the field `label` and states the corrective action.
 */
export function required(label: string): Validator<string> {
  return (value) => {
    if (value == null || value.trim() === "") {
      return `${label} is required. Enter a value to continue.`;
    }
    return null;
  };
}

// Anchored, linear — no nested quantifiers or backreferences (ReDoS-safe).
// Mirrors money.go's minorStringRE (integer minor units or up to 2 decimals).
const AMOUNT_FORMAT_RE = /^\d+(\.\d{1,2})?$/;
// Mirrors money.go's maxParseLength cap on the integer part (19 digits).
const MAX_INTEGER_DIGITS = 19;
// Strict zero check (0, 00.00, 0.0, 0.00) without ever touching a float.
const ZERO_RE = /^0+(\.0{1,2})?$/;

/**
 * Returns a validator that rejects amounts with an invalid decimal format, more
 * than `MAX_INTEGER_DIGITS` integer digits, or a zero value. Converts the value
 * to a string before validating so callers may pass a number or a string.
 */
export function positiveAmount(): Validator<string> {
  return (value) => {
    if (value == null || value === "") return null;
    const raw = String(value).trim();
    if (!AMOUNT_FORMAT_RE.test(raw)) {
      return "Enter an amount with up to 2 decimal places, for example 12.50.";
    }
    if (raw.split(".")[0].length > MAX_INTEGER_DIGITS) {
      return "Amount exceeds the maximum allowed value.";
    }
    if (ZERO_RE.test(raw)) {
      return "Amount must be greater than zero.";
    }
    return null;
  };
}

/**
 * Returns a validator that rejects anything that is not a strict, real calendar
 * date (YYYY-MM-DD). Impossible dates such as 2023-02-30 are rejected by
 * parseIsoDate's day-count check, never rolled over by the Date constructor.
 */
export function validDate(): Validator<string> {
  return (value) => {
    if (value == null || value === "") return null;
    if (!parseIsoDate(value.trim())) {
      return "That is not a real calendar date. Enter a valid date, for example 2024-05-14.";
    }
    return null;
  };
}

/**
 * Returns a validator that rejects values longer than `max` characters. The
 * message interpolates the current count so the user sees how far they overshot.
 */
export function maxLength(max: number, label: string): Validator<string> {
  return (value) => {
    if (value == null || value.length <= max) return null;
    return `${label} must be ${max} characters or fewer. You used ${value.length} — shorten it to continue.`;
  };
}

// Structural ISO 4217 shape. Mirrors money.go, which validates the 3-letter
// A-Z form only (a full registry check is intentionally a server concern).
// Case is normalized before validation so "usd" and "USD" both pass.
const CURRENCY_RE = /^[A-Z]{3}$/;

/**
 * Returns a validator that accepts any 3-letter A–Z currency code,
 * case-insensitively, and rejects any other shape.
 */
export function currencyCode(): Validator<string> {
  return (value) => {
    if (value == null || value === "") return null;
    const normalized = value.trim().toUpperCase();
    if (!CURRENCY_RE.test(normalized)) {
      return "That is not a valid ISO 4217 currency code. Enter one, for example USD or EUR.";
    }
    return null;
  };
}

/**
 * Returns a validator that rejects an empty category selection (Module B
 * requires at least one category per transaction).
 */
export function categoryRequired(): Validator<string[]> {
  return (ids) => {
    if (ids == null || ids.length === 0) {
      return "Select at least one category. Choose a category to continue.";
    }
    return null;
  };
}

/**
 * Returns a validator that rejects a date in the future unless the entry is
 * explicitly flagged as scheduled (Module B boundary). Empty/invalid dates are
 * left to `required`/`validDate`; unparseable values short-circuit to null.
 */
export function futureDateAllowed(
  scheduled: boolean,
  opts?: { locale?: string; timeZone?: string }
): Validator<string> {
  return (value) => {
    if (value == null || value === "") return null;
    const date = parseIsoDate(value.trim());
    if (!date) return null;
    if (scheduled) return null;
    const today = parseIsoDate(
      todayInLocale(opts?.locale ?? "en-US", opts?.timeZone)
    );
    if (!today) return null;
    if (date.getTime() > today.getTime()) {
      return "This date is in the future. Enable the Scheduled option to save it.";
    }
    return null;
  };
}

/**
 * Cross-field check (not a Validator): rejects a date range whose start is after
 * its end, or whose bounds do not parse. Returns null when valid; the message is
 * intended to render adjacent to the end field.
 */
export function rangeValid(start: string, end: string): string | null {
  if (isRangeValid(start, end)) return null;
  return "The start date must be on or before the end date. Adjust either date to continue.";
}

/**
 * A single field error, matching the RFC 7807 `field_errors` array element shape
 * so client and server validation render through the same message pipeline.
 */
export interface FieldError {
  field: string;
  message: string;
}

/** Map of field name → raw form value, keyed identically to the schema. */
export type FieldValues = Record<string, unknown>;

/**
 * Aggregates validation across an entire form: runs each field's validators in
 * order and records the first failing message per field. Fields are visited in
 * schema insertion order, giving a deterministic order consumers can use to
 * focus the first invalid field. Returns RFC 7807 `field_errors`-shaped array.
 */
export function validateForm(
  values: FieldValues,
  schema: Record<string, Validator[]>
): FieldError[] {
  const fieldErrors: FieldError[] = [];
  for (const field of Object.keys(schema)) {
    const validators = schema[field];
    const value = values[field];
    for (const validate of validators) {
      const message = validate(value);
      if (message != null) {
        fieldErrors.push({ field, message });
        break;
      }
    }
  }
  return fieldErrors;
}