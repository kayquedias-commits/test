/**
 * Unified fetch wrapper - the SINGLE entry point to the HTTP API (mandatory
 * service layer). No component or store calls `fetch` directly; every
 * `endpoints.ts` function routes through `apiRequest` (Blueprint §5.1/§7.7).
 *
 * This module is the one trust boundary for request shaping and the one seam
 * that maps the RFC 7807 envelope to typed, user-oriented errors. It:
 *   - attaches `X-Correlation-Id` + `Authorization: Bearer` per request,
 *   - honors `AbortSignal` for stale-request cancellation on every method,
 *   - retries idempotent 5xx requests with exponential backoff + jitter,
 *   - maps 400/401/403/404/409/429/timeout to distinct typed errors.
 *
 * @security All server-supplied message/fieldErrors/correlationId values are
 * coerced to plain strings (never HTML), truncated, and never written to the
 * DOM by this module. No stack traces ever reach the UI. Credentials are never
 * logged. `credentials` is `same-origin` (never `include`), so no session
 * cookie is attached cross-site (CSRF-safe while we auth via bearer header).
 */

import type { ApiErrorCode, ApiErrorEnvelope, FieldError } from "./types";
import { isApiErrorEnvelope, isFieldError } from "./types";

const API_BASE_URL: string = import.meta.env.VITE_API_BASE_URL;

/** Compile-time login destination; never derived from server input (CWE-601). */
const LOGIN_PATH = "/login" as const;

/** Default AbortController budget - bounds any single request (CWE-400). */
export const API_TIMEOUT = 15_000;

/** Max 5xx retries for idempotent requests (3 retries, then a terminal error). */
export const RETRY_MAX = 3;
/** Base backoff (ms), doubled per retry, plus jitter. */
export const RETRY_BASE = 100;

/** Error-message length cap to prevent UI spoofing / memory exhaustion. */
const MAX_ERROR_MESSAGE = 500;
/** Max field errors surfaced per payload. */
const MAX_FIELD_ERRORS = 50;
/** Cap on the error-response body size we will buffer (CWE-400). */
const MAX_ERROR_BODY = 1024 * 1024;

// Validate the API origin: bearer tokens must never travel over cleartext.
if (
  !/^https:\/\//i.test(API_BASE_URL) &&
  !/^http:\/\/(localhost|127\.0\.0\.1)(:\d+)?$/i.test(API_BASE_URL)
) {
  throw new Error(
    "[api] VITE_API_BASE_URL must be https:// (or http://localhost): refusing to send auth tokens over cleartext"
  );
}

/** Options for the public entry point. */
export type ApiRequestOptions<TBody = unknown> = {
  method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  path: string;
  body?: TBody;
  params?: Record<string, string | number>;
  /** Caller-provided abort (stale-request supersession); honored on every method. */
  signal?: AbortSignal;
  /** Optional idempotency key; also the gate that permits auto-retry of writes. */
  idempotencyKey?: string;
  /** Override the default 15s budget. */
  timeoutMs?: number;
};

/** Injected by auth wiring to supply tokens and react to session expiry. */
export const sessionHandlers: {
  getAccessToken?: () => string | null;
  onSessionExpired?: (destination: string) => void;
} = {};

let sessionExpiryFlowActive = false;

/**
 * Base type for every surfaced API error. Fields are self-contained so any
 * screen can render a complete recovery state from a single object (no
 * cross-request state tracking in components). `message` is human and
 * solution-oriented; it never carries a stack trace or internal state.
 */
export class ApiError extends Error {
  readonly code: ApiErrorCode;
  readonly status: number;
  readonly fieldErrors: FieldError[];
  readonly correlationId: string;
  readonly retryAfterSeconds?: number;

  constructor(opts: {
    code: ApiErrorCode;
    status: number;
    message: string;
    fieldErrors?: FieldError[];
    correlationId?: string;
    retryAfterSeconds?: number;
  }) {
    super(opts.message);
    this.name = "ApiError";
    this.code = opts.code;
    this.status = opts.status;
    this.fieldErrors = opts.fieldErrors ?? [];
    this.correlationId = opts.correlationId ?? "";
    if (opts.retryAfterSeconds !== undefined) {
      this.retryAfterSeconds = opts.retryAfterSeconds;
    }
  }
}

/** 400 - field-level validation failed; forms map fieldErrors inline. */
export class ValidationError extends ApiError {
  constructor(message: string, fieldErrors: FieldError[], correlationId: string) {
    super({ code: "VALIDATION_FAILED", status: 400, message, fieldErrors, correlationId });
    this.name = "ValidationError";
  }
}

/** 401 - session expired; emits a signal before redirecting to login. */
export class SessionExpiredError extends ApiError {
  constructor(message: string, correlationId: string) {
    super({ code: "UNAUTHENTICATED", status: 401, message, correlationId });
    this.name = "SessionExpiredError";
  }
}

/** 403 - route renders the insufficient-permissions state, never a blank page. */
export class ForbiddenError extends ApiError {
  constructor(message: string, correlationId: string) {
    super({ code: "FORBIDDEN", status: 403, message, correlationId });
    this.name = "ForbiddenError";
  }
}

/** 404 - route renders the not-found state; never retried. */
export class NotFoundError extends ApiError {
  constructor(message: string, correlationId: string) {
    super({ code: "NOT_FOUND", status: 404, message, correlationId });
    this.name = "NotFoundError";
  }
}

/** 409 - record modified by another user. Never auto-overwritten; the user
 * decides via the ConflictRecoveryBanner Refresh action. */
export class ConflictError extends ApiError {
  constructor(message: string, correlationId: string) {
    super({ code: "CONFLICT_STATE", status: 409, message, correlationId });
    this.name = "ConflictError";
  }
}

/** 429 - rate limited. Exposes Retry-After guidance; never auto-retried. */
export class RateLimitedError extends ApiError {
  constructor(message: string, correlationId: string, retryAfterSeconds: number) {
    super({ code: "RATE_LIMITED", status: 429, message, correlationId, retryAfterSeconds });
    this.name = "RateLimitedError";
  }
}

/** 5xx - surfaced only after idempotent retries are exhausted. */
export class RetryableError extends ApiError {
  constructor(status: number, message: string, correlationId: string) {
    super({
      code: status === 503 ? "DEPENDENCY_UNAVAILABLE" : "INTERNAL_ERROR",
      status,
      message,
      correlationId,
    });
    this.name = "RetryableError";
  }
}

/** 15s budget exceeded - distinct from a caller-initiated abort. */
export class TimeoutError extends ApiError {
  constructor(correlationId: string) {
    super({
      code: "INTERNAL_ERROR",
      status: 0,
      message: "The request timed out - check your connection and try again.",
      correlationId,
    });
    this.name = "TimeoutError";
  }
}

/**
 * True when a request may safely auto-retry a 5xx: an idempotent method
 * (GET/HEAD/PUT/DELETE) or an Idempotency-Key is supplied. Non-idempotent
 * writes (POST/PATCH without a key) must NOT auto-retry - retrying a write that
 * reached the server would double-post a ledger entry (data-integrity gate).
 */
function shouldAutoRetry(method: string, idempotencyKey?: string): boolean {
  if (idempotencyKey !== undefined && idempotencyKey.length > 0) return true;
  return method === "GET" || method === "HEAD" || method === "PUT" || method === "DELETE";
}

function generateCorrelationId(): string {
  return crypto.randomUUID();
}

/** Coerce and neutralize a server-supplied correlation value for logs/UI. */
function sanitizeCorrelation(value: unknown): string {
  return String(value ?? "").replace(/[\r\n]/g, "").slice(0, 128);
}

/** Cap an untrusted message; never attaches HTML and never throws stack text. */
function safeMessage(value: unknown): string {
  return String(value ?? "").slice(0, MAX_ERROR_MESSAGE);
}

function isAbortError(err: unknown): boolean {
  return (
    (err instanceof DOMException && err.name === "AbortError") ||
    (err as { name?: unknown })?.name === "AbortError"
  );
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function buildUrl(path: string, params?: Record<string, string | number>): string {
  const url = new URL(API_BASE_URL.replace(/\/+$/, "") + path);
  if (params) {
    for (const [key, value] of Object.entries(params)) {
      url.searchParams.set(key, String(value));
    }
  }
  return url.toString();
}

function parseRetryAfter(header: string | null): number {
  if (!header) return 0;
  const seconds = Number(header);
  if (Number.isFinite(seconds) && seconds >= 0) return Math.ceil(seconds);
  const when = Date.parse(header);
  if (Number.isFinite(when)) return Math.max(0, Math.ceil((when - Date.now()) / 1000));
  return 0;
}

function fieldErrorsFromEnvelope(envelope: ApiErrorEnvelope): FieldError[] {
  if (!Array.isArray(envelope.error?.field_errors)) return [];
  const errors: FieldError[] = [];
  for (const fe of envelope.error.field_errors) {
    if (errors.length >= MAX_FIELD_ERRORS) break;
    if (isFieldError(fe)) {
      errors.push({ field: String(fe.field), message: safeMessage(fe.message) });
    }
  }
  return errors;
}

function correlationFrom(envelope: ApiErrorEnvelope | null, fallback: string): string {
  return sanitizeCorrelation(envelope?.error?.correlation_id ?? fallback);
}

function messageFrom(envelope: ApiErrorEnvelope | null, fallback: string): string {
  return envelope ? safeMessage(envelope.error.message) : fallback;
}

async function readErrorBody(response: Response): Promise<string | null> {
  const declared = response.headers.get("Content-Length");
  if (declared !== null && Number(declared) > MAX_ERROR_BODY) return null;
  const text = await response.text();
  if (text.length > MAX_ERROR_BODY) return null;
  return text;
}

function textToEnvelope(text: string): ApiErrorEnvelope | null {
  try {
    const parsed = JSON.parse(text) as unknown;
    return isApiErrorEnvelope(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

/** Trigger the one-time session-expiry flow: emit the typed error, call the
 * wiring handler, then redirect to the compile-time login path. A single in-
 * flight flag prevents parallel requests on one expired session from firing
 * multiple redirects. */
function handleSessionExpiry(correlationId: string): SessionExpiredError {
  const destination = `${window.location.pathname}${window.location.search}`;
  if (!sessionExpiryFlowActive) {
    sessionExpiryFlowActive = true;
    sessionHandlers.onSessionExpired?.(destination);
    window.location.assign(LOGIN_PATH);
  }
  return new SessionExpiredError(
    "Your session expired. Please sign in again to continue.",
    correlationId
  );
}

async function request<T>(
  method: string,
  path: string,
  opts: ApiRequestOptions,
  attempt: number,
  correlationId: string,
  callerSignal: AbortSignal | undefined
): Promise<T> {
  const controller = new AbortController();
  const timeoutMs = opts.timeoutMs ?? API_TIMEOUT;
  const timeoutHandle = setTimeout(() => controller.abort(), timeoutMs);
  const onCallerAbort = () => controller.abort();
  callerSignal?.addEventListener("abort", onCallerAbort, { once: true });

  const token = sessionHandlers.getAccessToken?.() ?? null;
  const headers: Record<string, string> = {
    "X-Correlation-Id": correlationId,
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
    ...(opts.idempotencyKey ? { Idempotency-Key: opts.idempotencyKey } : {}),
    ...(opts.body !== undefined ? { "Content-Type": "application/json" } : {}),
  };

  try {
    let response: Response;
    try {
      response = await fetch(buildUrl(path, opts.params), {
        method,
        headers,
        credentials: "same-origin", // never "include" - do not attach session cookies cross-site
        signal: controller.signal,
        ...(opts.body !== undefined ? { body: JSON.stringify(opts.body) } : {}),
      });
    } catch (err) {
      if (isAbortError(err)) {
        if (callerSignal?.aborted) throw err; // caller-initiated: swallowed upstream
        throw new TimeoutError(correlationId);
      }
      throw new RetryableError(0, "Could not reach the server. Check your connection and try again.", correlationId);
    }

    if (response.ok) {
      const text = await response.text();
      if (text.length === 0) return undefined as T;
      return JSON.parse(text) as T;
    }

    const rawText = await readErrorBody(response);
    const envelope = rawText === null ? null : textToEnvelope(rawText);
    const oversized = rawText === null;
    const fallback = oversized
      ? `The server returned a response that was too large. (reference ${correlationId})`
      : `Request failed (${response.status}). (reference ${correlationId})`;

    switch (response.status) {
      case 400: {
        throw new ValidationError(
          messageFrom(envelope, fallback),
          envelope ? fieldErrorsFromEnvelope(envelope) : [],
          correlationFrom(envelope, correlationId)
        );
      }
      case 401: {
        throw handleSessionExpiry(correlationId);
      }
      case 403: {
        throw new ForbiddenError(
          messageFrom(envelope, "You do not have permission to do this."),
          correlationFrom(envelope, correlationId)
        );
      }
      case 404: {
        throw new NotFoundError(
          messageFrom(envelope, "The requested resource was not found."),
          correlationFrom(envelope, correlationId)
        );
      }
      case 409: {
        throw new ConflictError(
          messageFrom(envelope, "This record was modified by another user. Refresh to see the latest version."),
          correlationFrom(envelope, correlationId)
        );
      }
      case 429: {
        const retryAfterSeconds = parseRetryAfter(response.headers.get("Retry-After"));
        throw new RateLimitedError(
          messageFrom(
            envelope,
            `Too many requests. Try again ${retryAfterSeconds > 0 ? `in about ${retryAfterSeconds}s` : "in a moment"}.`
          ),
          correlationFrom(envelope, correlationId),
          retryAfterSeconds
        );
      }
      default: {
        const is5xx = response.status >= 500;
        const msg = messageFrom(envelope, fallback);
        // Retry up to RETRY_MAX times (idempotent requests only), then surface
        // a single terminal RetryableError ("500 retries 3 then throws").
        if (is5xx && shouldAutoRetry(method, opts.idempotencyKey) && attempt <= RETRY_MAX) {
          const backoff = RETRY_BASE * 2 ** (attempt - 1) + Math.random() * RETRY_BASE;
          await delay(backoff);
          if (callerSignal?.aborted) {
            throw new DOMException("Aborted", "AbortError"); // swallowed upstream
          }
          return request<T>(method, path, opts, attempt + 1, correlationId, callerSignal);
        }
        if (is5xx) {
          const retryMsg =
            attempt > 1
              ? "The server is temporarily unavailable. Please try again in a moment."
              : msg;
          throw new RetryableError(
            response.status,
            retryMsg,
            correlationFrom(envelope, correlationId)
          );
        }
        throw new ApiError({
          code: "INTERNAL_ERROR",
          status: response.status,
          message: msg,
          correlationId: correlationFrom(envelope, correlationId),
        });
      }
    }
  } finally {
    clearTimeout(timeoutHandle);
    callerSignal?.removeEventListener("abort", onCallerAbort);
  }
}

/**
 * Public entry point. Every `endpoints.ts` function calls this; it is the only
 * place `fetch` is invoked in the app.
 *
 * @returns the parsed success payload, or throws a typed `ApiError` subclass.
 * A caller-initiated abort (stale request superseded) resolves silently with
 * no error surface; timeouts and retry exhaustion surface distinct typed
 * errors carrying a complete recovery payload.
 */
export async function apiRequest<T>(options: ApiRequestOptions): Promise<T> {
  const correlationId = generateCorrelationId();
  try {
    return await request<T>(options.method, options.path, options, 1, correlationId, options.signal);
  } catch (err) {
    if (isAbortError(err) && options.signal?.aborted) {
      // Caller superseded this request - invisible to the user.
      return undefined as T;
    }
    throw err;
  }
}

/** Register auth wiring: bearer-token supplier and session-expiry handler. */
export function setSessionHandlers(handlers: {
  getAccessToken: () => string | null;
  onSessionExpired: (destination: string) => void;
}): void {
  sessionHandlers.getAccessToken = handlers.getAccessToken;
  sessionHandlers.onSessionExpired = handlers.onSessionExpired;
}